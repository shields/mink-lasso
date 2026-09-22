// Copyright © 2026 Michael Shields
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package masso

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"slices"
	"sync"
	"time"

	"msrl.dev/mink-lasso/internal/clock"
)

// Client-level errors. ErrNoResponse and ErrLost are declared in errors.go
// alongside the codec's other sentinels; the ones below are specific to the
// socket-owning Client.
var (
	// ErrNoPort indicates NewClient could not bind any UDP port in
	// [Options.PortMin, Options.PortMax].
	ErrNoPort = errors.New("masso: could not bind a UDP port")

	// ErrNotConnected indicates Run, Tools, or Upload was called before a
	// successful Connect.
	ErrNotConnected = errors.New("masso: not connected")

	// ErrBusy indicates Upload was called while another upload was
	// already running on this Client.
	ErrBusy = errors.New("masso: upload already in progress")

	// ErrSend indicates a UDP write to the controller failed.
	ErrSend = errors.New("masso: send failed")

	// ErrDiscoverySend indicates Discover could not send a discovery
	// request to any destination at all.
	ErrDiscoverySend = errors.New("masso: could not send discovery request to any destination")

	// ErrInterfaceEnum indicates Options.Interfaces failed while Discover
	// was building its list of directed-broadcast destinations.
	ErrInterfaceEnum = errors.New("masso: could not enumerate network interfaces")

	// ErrRead indicates a read of upload data via io.ReaderAt failed.
	ErrRead = errors.New("masso: reading upload data failed")

	// ErrFileTooLarge indicates an Upload size that does not fit the
	// protocol's 32-bit size field.
	ErrFileTooLarge = errors.New("masso: file size exceeds the protocol's 32-bit limit")
)

// defaultAttempts is how many times Connect and Tools each try a request
// that waits up to ReplyTimeout for its reply.
const defaultAttempts = 3

// maxToolIndex is the highest tool index Tools queries (docs/protocol.md
// §3.4: the app iterates 1..118 on mill/router firmware).
const maxToolIndex = 118

// waiterBuffer bounds how many not-yet-collected replies a single expect
// waiter holds before newer ones are dropped. It matters only for Discover,
// whose waiter stays registered for its whole collection window while
// several controllers may reply.
const waiterBuffer = 32

// Options configures NewClient. The zero value is valid: every field takes
// the documented default.
type Options struct {
	// Logger receives a debug-level entry for every dropped or
	// undecodable packet. Nil means slog.New(slog.DiscardHandler).
	Logger *slog.Logger

	// Clock provides every timeout, retransmit interval, and keepalive
	// tick, so tests can run without waiting on the wall clock. Nil means
	// clock.Real{}.
	Clock clock.Clock

	// PortMin and PortMax bound the UDP port range NewClient tries, in
	// order, like Masso Link. Zero means ListenPortMin and ListenPortMax.
	PortMin, PortMax int

	// ListenPacket opens the client's socket. Nil means net.ListenPacket.
	// Overriding it lets tests force bind failures.
	ListenPacket func(network, address string) (net.PacketConn, error)

	// Interfaces enumerates the local network interfaces Discover uses to
	// compute directed-broadcast destinations. Nil means net.Interfaces.
	Interfaces func() ([]net.Interface, error)

	// InterfaceAddrs returns one interface's addresses. Nil means
	// (*net.Interface).Addrs.
	InterfaceAddrs func(*net.Interface) ([]net.Addr, error)

	// DiscoveryTargets, if non-empty, are the only destinations Discover
	// sends to, replacing the limited and directed broadcasts entirely. Nil
	// means broadcast. Tests set it so that Discover can reach a loopback
	// simulator (a loopback interface does not support IP broadcast on
	// every OS) without also broadcasting on the real network, where every
	// controller that hears the request re-targets its replies to the test.
	DiscoveryTargets []*net.UDPAddr

	// KeepaliveInterval is how often Run sends a keepalive to the
	// connected controller. Zero means one second.
	KeepaliveInterval time.Duration

	// LostAfter is how long Run waits without a status reply before
	// returning ErrLost. Zero means five seconds.
	LostAfter time.Duration

	// ReplyTimeout bounds each attempt of a request that is retried a
	// fixed number of times (discovery and config during Connect, each
	// tool query). Zero means one second.
	ReplyTimeout time.Duration

	// Retransmit is how often Upload resends an unacknowledged start or
	// chunk packet. Zero means 100 milliseconds.
	Retransmit time.Duration

	// StallTimeout is how long Upload waits, retransmitting every
	// Retransmit interval, before giving up on a start or chunk ACK with
	// ErrNoResponse. Zero means fifteen seconds.
	StallTimeout time.Duration
}

// resolveOptions returns a copy of opts with every zero field replaced by
// its documented default.
func resolveOptions(opts Options) Options {
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	if opts.Clock == nil {
		opts.Clock = clock.Real{}
	}
	if opts.PortMin == 0 {
		opts.PortMin = ListenPortMin
	}
	if opts.PortMax == 0 {
		opts.PortMax = ListenPortMax
	}
	if opts.ListenPacket == nil {
		opts.ListenPacket = net.ListenPacket
	}
	if opts.Interfaces == nil {
		opts.Interfaces = net.Interfaces
	}
	if opts.InterfaceAddrs == nil {
		opts.InterfaceAddrs = (*net.Interface).Addrs
	}
	if opts.KeepaliveInterval == 0 {
		opts.KeepaliveInterval = time.Second
	}
	if opts.LostAfter == 0 {
		opts.LostAfter = 5 * time.Second
	}
	if opts.ReplyTimeout == 0 {
		opts.ReplyTimeout = time.Second
	}
	if opts.Retransmit == 0 {
		opts.Retransmit = 100 * time.Millisecond
	}
	if opts.StallTimeout == 0 {
		opts.StallTimeout = 15 * time.Second
	}
	return opts
}

// incoming pairs a decoded reply with the address it arrived from; Discover
// needs the address, everything else ignores it.
type incoming struct {
	addr  net.Addr
	reply Reply
}

// waiter is one registration made through Client.expect.
type waiter struct {
	match func(Reply) bool
	ch    chan incoming
}

// Client is a UDP endpoint that speaks the Masso Link protocol to one
// controller at a time: one socket for both send and receive, a reader
// goroutine that never blocks on a slow or absent consumer, and the
// request/reply exchanges built on top (discovery, connect, keepalive and
// status, tool table, file upload). A zero Client is not usable; construct
// one with NewClient.
type Client struct {
	conn      net.PacketConn
	logger    *slog.Logger
	clock     clock.Clock
	localPort uint16

	interfacesFn     func() ([]net.Interface, error)
	interfaceAddrsFn func(*net.Interface) ([]net.Addr, error)
	discoveryTargets []*net.UDPAddr

	keepaliveInterval time.Duration
	lostAfter         time.Duration
	replyTimeout      time.Duration
	retransmit        time.Duration
	stallTimeout      time.Duration

	mu      sync.Mutex
	waiters map[byte][]*waiter
	remote  *net.UDPAddr

	statusIn  chan Status // written by the reader goroutine only
	statusOut chan Status // written by Run only

	uploadMu sync.Mutex

	done      chan struct{}
	wg        sync.WaitGroup
	closeOnce sync.Once
	closeErr  error
}

// NewClient binds a single UDP socket on 0.0.0.0, trying ports
// Options.PortMin through Options.PortMax in order (like Masso Link), and
// starts the reader goroutine. Go's net package already sets SO_BROADCAST on
// every datagram socket, on every OS NewClient supports, so it does not set
// it again. It returns ErrNoPort if every port in range is unavailable.
func NewClient(opts Options) (*Client, error) {
	opts = resolveOptions(opts)

	var conn net.PacketConn
	var boundPort int
	var lastErr error
	for port := opts.PortMin; port <= opts.PortMax; port++ {
		pc, err := opts.ListenPacket("udp", fmt.Sprintf("0.0.0.0:%d", port))
		if err != nil {
			lastErr = err
			continue
		}
		conn = pc
		boundPort = port
		break
	}
	if conn == nil {
		return nil, fmt.Errorf("%w (tried %d-%d): %w", ErrNoPort, opts.PortMin, opts.PortMax, lastErr)
	}

	c := &Client{
		conn:              conn,
		logger:            opts.Logger,
		clock:             opts.Clock,
		localPort:         uint16(boundPort & 0xFFFF), // boundPort is one of the ports we asked to bind, always in range
		interfacesFn:      opts.Interfaces,
		interfaceAddrsFn:  opts.InterfaceAddrs,
		discoveryTargets:  slices.Clone(opts.DiscoveryTargets),
		keepaliveInterval: opts.KeepaliveInterval,
		lostAfter:         opts.LostAfter,
		replyTimeout:      opts.ReplyTimeout,
		retransmit:        opts.Retransmit,
		stallTimeout:      opts.StallTimeout,
		waiters:           make(map[byte][]*waiter),
		statusIn:          make(chan Status, 1),
		statusOut:         make(chan Status, 1),
		done:              make(chan struct{}),
	}
	c.wg.Add(1)
	go c.readLoop()
	return c, nil
}

// LocalPort returns the UDP port the client's socket is bound to.
func (c *Client) LocalPort() uint16 {
	return c.localPort
}

// Remote returns the controller address set by the most recent successful
// Connect, or nil if Connect has never succeeded.
func (c *Client) Remote() *net.UDPAddr {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.remote
}

func (c *Client) setRemote(addr *net.UDPAddr) {
	c.mu.Lock()
	c.remote = addr
	c.mu.Unlock()
}

// Status returns the channel Run publishes the latest Status to. It always
// holds at most one value: a new status replaces one nobody has read yet.
func (c *Client) Status() <-chan Status {
	return c.statusOut
}

// Close stops the reader goroutine and closes the socket. It is safe to
// call more than once; only the first call's result is returned.
func (c *Client) Close() error {
	c.closeOnce.Do(func() {
		close(c.done)
		c.closeErr = c.conn.Close()
		c.wg.Wait()
	})
	return c.closeErr
}

// readLoop reads and dispatches datagrams until the socket errors, which
// happens once Close closes it.
func (c *Client) readLoop() {
	defer c.wg.Done()
	buf := make([]byte, MaxPacket)
	for {
		n, addr, err := c.conn.ReadFrom(buf)
		if err != nil {
			select {
			case <-c.done:
				c.logger.Debug("masso: reader stopped: socket closed")
			default:
				c.logger.Debug("masso: reader stopped: read failed", "error", err)
			}
			return
		}
		pkt := make([]byte, n)
		copy(pkt, buf[:n])
		c.handlePacket(pkt, addr)
	}
}

// handlePacket decodes one datagram and, non-blockingly, offers it to every
// registered waiter whose type matches and whose match function accepts it.
// A status reply instead goes to the one-slot latest-value channel Run
// drains. Anything undecodable, unmatched, or unwanted (no waiter, buffer
// full, matcher rejects) is dropped with a debug log; the reader never
// blocks on a slow or absent consumer, so duplicate and stray replies are
// harmless.
func (c *Client) handlePacket(pkt []byte, addr net.Addr) {
	reply, err := DecodeReply(pkt)
	if err != nil {
		c.logger.Debug("masso: dropping undecodable packet", "error", err, "from", addr)
		return
	}
	if st, ok := reply.(Status); ok {
		setLatestStatus(c.statusIn, st)
		return
	}

	typ := replyTypeOf(reply)
	c.mu.Lock()
	list := slices.Clone(c.waiters[typ])
	c.mu.Unlock()

	delivered := false
	for _, w := range list {
		if !w.match(reply) {
			continue
		}
		select {
		case w.ch <- incoming{addr: addr, reply: reply}:
			delivered = true
		default:
			c.logger.Debug("masso: dropping reply, waiter buffer full", "type", fmt.Sprintf("%T", reply))
		}
	}
	if !delivered {
		c.logger.Debug("masso: dropping reply with no waiter or no match", "type", fmt.Sprintf("%T", reply))
	}
}

// replyTypeOf returns the packet type byte a decoded Reply came from. Reply's
// implementation set is closed to this package, so the switch is exhaustive.
func replyTypeOf(r Reply) byte {
	switch r.(type) {
	case Identity:
		return TypeDiscovery
	case ConfigReply:
		return TypeConfig
	case Status:
		return TypeStatus
	case ToolRecord:
		return TypeTool
	case StartAck:
		return TypeUploadStart
	case ChunkAck:
		return TypeUploadChunk
	default:
		return 0
	}
}

// setLatestStatus makes v the sole value held in ch, dropping whatever was
// there before. Callers must not share a channel between multiple writers.
func setLatestStatus(ch chan Status, v Status) {
	select {
	case ch <- v:
		return
	default:
	}
	select {
	case <-ch:
	default:
	}
	select {
	case ch <- v:
	default:
	}
}

// expect registers a waiter for replies of type typ that satisfy match. The
// returned channel receives every matching reply, non-blockingly, until
// cancel is called; cancel is safe to call more than once and should always
// run, typically via defer.
func (c *Client) expect(typ byte, match func(Reply) bool) (<-chan incoming, func()) {
	w := &waiter{match: match, ch: make(chan incoming, waiterBuffer)}
	c.mu.Lock()
	c.waiters[typ] = append(c.waiters[typ], w)
	c.mu.Unlock()

	var once sync.Once
	cancel := func() {
		once.Do(func() {
			c.mu.Lock()
			defer c.mu.Unlock()
			if i := slices.Index(c.waiters[typ], w); i >= 0 {
				c.waiters[typ] = slices.Delete(c.waiters[typ], i, i+1)
			}
		})
	}
	return w.ch, cancel
}

// anyReply matches any reply of the type expect was called with; it exists
// for waiters that only need to distinguish by packet type, not content.
func anyReply(Reply) bool { return true }

// must panics if err is non-nil. It exists only for codec calls in this
// file whose error is unreachable by construction — this package always
// calls them with already-validated, size-bounded arguments — so a failure
// here would mean a bug in this package, not a caller mistake worth
// returning gracefully.
func must[T any](v T, err error) T {
	if err != nil {
		panic(fmt.Sprintf("masso: unreachable: %v", err))
	}
	return v
}

// mustType type-asserts v to T, panicking if it fails. It exists for reply
// values the reader's own typ-keyed routing already guarantees are of type
// T (a waiter registered for a given packet type is only ever offered
// replies decoded from that same packet type), so a failure here would
// mean a bug in this package. Wrapping the assertion in a function, rather
// than writing v.(T) at each call site, is also what keeps errcheck (which
// treats a bare type assertion as an unchecked error) satisfied without
// scattering unreachable "not ok" branches through the caller code.
func mustType[T any](v any) T {
	t, ok := v.(T)
	if !ok {
		panic(fmt.Sprintf("masso: unreachable: got %T, want %T", v, t))
	}
	return t
}

// send writes pkt to addr, wrapping any error as ErrSend. request and
// sendUntilStall share it for every send they make; sendKeepalive does not,
// since a missed keepalive is logged and tolerated rather than treated as a
// failure worth returning.
func (c *Client) send(pkt []byte, addr net.Addr) error {
	if _, err := c.conn.WriteTo(pkt, addr); err != nil {
		return fmt.Errorf("%w: %w", ErrSend, err)
	}
	return nil
}

// request sends pkt to addr up to attempts times, waiting up to perAttempt
// each time for a reply of typ satisfying match. The waiter is registered
// before the first send, so a reply that arrives unusually fast is never
// missed. It returns ErrNoResponse if every attempt times out.
func (c *Client) request(
	ctx context.Context, typ byte, match func(Reply) bool,
	pkt []byte, addr net.Addr, perAttempt time.Duration, attempts int,
) (Reply, error) {
	ch, cancel := c.expect(typ, match)
	defer cancel()

	for range attempts {
		if err := c.send(pkt, addr); err != nil {
			return nil, err
		}
		timer := c.clock.NewTimer(perAttempt)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C():
		case in := <-ch:
			timer.Stop()
			return in.reply, nil
		}
	}
	return nil, ErrNoResponse
}

// sendUntilStall sends pkt to addr, then retransmits it every c.retransmit
// interval until a reply of typ satisfying match arrives, ctx is canceled,
// or c.stallTimeout elapses since the first send with nothing matching
// received — whichever comes first. The waiter is registered before the
// first send for the same reason as request.
func (c *Client) sendUntilStall(
	ctx context.Context, typ byte, match func(Reply) bool, pkt []byte, addr net.Addr,
) (Reply, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	ch, cancel := c.expect(typ, match)
	defer cancel()

	if err := c.send(pkt, addr); err != nil {
		return nil, err
	}

	retransmit := c.clock.NewTimer(c.retransmit)
	// retransmit is reassigned below each time it fires, so the cleanup
	// must read it through a closure rather than bind to today's Timer —
	// a bare "defer retransmit.Stop()" would only ever stop the first
	// Timer created here, leaking whichever one is current when this
	// function returns.
	defer func() { retransmit.Stop() }()
	stall := c.clock.NewTimer(c.stallTimeout)
	defer stall.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-stall.C():
			return nil, ErrNoResponse
		case <-retransmit.C():
			if err := c.send(pkt, addr); err != nil {
				return nil, err
			}
			retransmit = c.clock.NewTimer(c.retransmit)
		case in := <-ch:
			return in.reply, nil
		}
	}
}

// Connect unicasts a discovery request to addr — which retargets that
// controller's replies to this client — then requests its config, each with
// up to three attempts of Options.ReplyTimeout. On success it records addr
// as Remote.
func (c *Client) Connect(ctx context.Context, addr *net.UDPAddr) (Identity, ConfigReply, error) {
	var id Identity
	var err error
	for range defaultAttempts {
		id, err = c.DiscoverAt(ctx, addr, c.replyTimeout)
		if err == nil || ctx.Err() != nil {
			break
		}
	}
	if err != nil {
		return Identity{}, ConfigReply{}, fmt.Errorf("connect: discovery: %w", err)
	}

	cfg, err := c.configRequest(ctx, addr)
	if err != nil {
		return Identity{}, ConfigReply{}, fmt.Errorf("connect: config: %w", err)
	}

	c.setRemote(addr)
	return id, cfg, nil
}

func (c *Client) configRequest(ctx context.Context, addr net.Addr) (ConfigReply, error) {
	reply, err := c.request(ctx, TypeConfig, anyReply, Config(c.clock.Now()), addr, c.replyTimeout, defaultAttempts)
	if err != nil {
		return ConfigReply{}, err
	}
	// request only ever delivers a reply that passed anyReply while
	// registered for TypeConfig, which the reader only ever routes
	// ConfigReply values to; the assertion cannot fail.
	return mustType[ConfigReply](reply), nil
}

// Run sends a keepalive to Remote at Options.KeepaliveInterval and publishes
// every status reply to Status(). It returns ErrLost if no status arrives
// for Options.LostAfter, ErrNotConnected if called before a successful
// Connect, or ctx.Err() when ctx is canceled.
func (c *Client) Run(ctx context.Context) error {
	remote := c.Remote()
	if remote == nil {
		return ErrNotConnected
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	c.sendKeepalive(remote)

	ticker := c.clock.NewTicker(c.keepaliveInterval)
	defer ticker.Stop()

	lost := c.clock.NewTimer(c.lostAfter)
	// lost is reassigned below on every status, so the cleanup must read
	// it through a closure rather than bind to today's Timer — a bare
	// "defer lost.Stop()" would only ever stop the very first Timer
	// created here, leaking whichever one is current when Run returns.
	defer func() { lost.Stop() }()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C():
			c.sendKeepalive(remote)
		case <-lost.C():
			return ErrLost
		case st := <-c.statusIn:
			setLatestStatus(c.statusOut, st)
			lost.Stop()
			lost = c.clock.NewTimer(c.lostAfter)
		}
	}
}

func (c *Client) sendKeepalive(remote net.Addr) {
	if _, err := c.conn.WriteTo(Keepalive(c.clock.Now()), remote); err != nil {
		c.logger.Debug("masso: keepalive send failed", "error", err)
	}
}

// Tools queries tool indices 1..118 in order, three attempts of
// Options.ReplyTimeout each. It stops, successfully, at the first empty
// tool name or at the first index that never answers (logged at debug
// level); either way the records collected so far are returned with a nil
// error. It returns ErrNotConnected if called before a successful Connect.
func (c *Client) Tools(ctx context.Context) ([]ToolRecord, error) {
	remote := c.Remote()
	if remote == nil {
		return nil, ErrNotConnected
	}

	var out []ToolRecord
	for i := 1; i <= maxToolIndex; i++ {
		idx := uint8(i & 0xFF) // i is bounded by maxToolIndex (118); the mask proves that to the static analyzer
		match := func(r Reply) bool {
			tr, ok := r.(ToolRecord)
			return ok && tr.Index == idx
		}
		reply, err := c.request(ctx, TypeTool, match, ToolQuery(idx), remote, c.replyTimeout, defaultAttempts)
		if err != nil {
			if errors.Is(err, ErrNoResponse) {
				c.logger.Debug("masso: tools: index did not answer, stopping", "index", idx)
				return out, nil
			}
			return out, err
		}
		// match already asserted this is a ToolRecord for idx before
		// delivering it; the assertion cannot fail.
		tr := mustType[ToolRecord](reply)
		if tr.Name == "" {
			return out, nil
		}
		out = append(out, tr)
	}
	return out, nil
}

// Upload sends a size-byte file named name, read from r, to the
// controller's USB drive, one chunk of MaxChunkData bytes at a time. name
// must satisfy ValidateFileName. progress, if non-nil, is called once with
// (0, size) right after the start ACK and again after each chunk is
// accepted. Only one upload runs at a time per Client; a concurrent call
// returns ErrBusy. A zero-byte file sends only the start packet — the real
// controller's behavior for a zero-byte upload is unconfirmed. ctx
// cancellation returns ctx.Err(); it never aborts a transfer already
// acknowledged as started, since that risks leaving a partial file on the
// controller's USB drive.
func (c *Client) Upload(
	ctx context.Context, name string, r io.ReaderAt, size int64, progress func(sent, total int64),
) error {
	if size < 0 || size > math.MaxUint32 {
		return fmt.Errorf("%w: %d bytes", ErrFileTooLarge, size)
	}
	remote := c.Remote()
	if remote == nil {
		return ErrNotConnected
	}

	if !c.uploadMu.TryLock() {
		return ErrBusy
	}
	defer c.uploadMu.Unlock()

	sizeU32 := uint32(size & math.MaxUint32) // size is already checked to fit uint32 above
	startPkt, err := UploadStart(sizeU32, name)
	if err != nil {
		return err
	}
	reply, err := c.sendUntilStall(ctx, TypeUploadStart, anyReply, startPkt, remote)
	if err != nil {
		return err
	}
	// sendUntilStall only ever delivers a reply that passed anyReply while
	// registered for TypeUploadStart, which the reader only ever routes
	// StartAck values to; the assertion cannot fail.
	ack := mustType[StartAck](reply)
	if err := ack.Err(); err != nil {
		return err
	}

	if progress != nil {
		progress(0, size)
	}
	if size == 0 {
		return nil
	}

	// The start ACK succeeded, so the transfer is now "acknowledged as
	// started" per this method's doc comment: ctx cancellation must no
	// longer abort it, to avoid leaving a partial file on the controller's
	// USB drive. context.WithoutCancel keeps the deadline-free context
	// alive for the rest of the transfer; sendUntilStall's own stall timer
	// still bounds how long uploadChunks can wait for an unresponsive
	// controller.
	return c.uploadChunks(context.WithoutCancel(ctx), remote, r, size, progress)
}

func (c *Client) uploadChunks(
	ctx context.Context, remote net.Addr, r io.ReaderAt, size int64, progress func(sent, total int64),
) error {
	buf := make([]byte, MaxChunkData)
	var sent int64
	for idx := uint32(0); sent < size; idx++ {
		end := min(sent+int64(MaxChunkData), size)
		want := int(end - sent)
		n, err := r.ReadAt(buf[:want], sent)
		if err != nil && (!errors.Is(err, io.EOF) || n != want) {
			return fmt.Errorf("%w: %w", ErrRead, err)
		}

		// want, and therefore n, never exceeds MaxChunkData (see end's
		// computation above), so UploadChunk can never reject this for
		// size; must documents and enforces that invariant.
		chunkPkt := must(UploadChunk(idx, buf[:n]))

		accepted := idx + 1
		match := func(r Reply) bool {
			ca, ok := r.(ChunkAck)
			return ok && ca.Accepted == accepted
		}
		reply, err := c.sendUntilStall(ctx, TypeUploadChunk, match, chunkPkt, remote)
		if err != nil {
			return err
		}
		// match already asserted this is a ChunkAck for this chunk before
		// delivering it; the assertion cannot fail.
		ca := mustType[ChunkAck](reply)
		if err := ca.Err(); err != nil {
			return err
		}

		sent = end
		if progress != nil {
			progress(sent, size)
		}
	}
	return nil
}
