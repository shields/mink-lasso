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

// Package sim implements an in-process fake Masso G3 controller: it speaks
// the wire protocol from docs/protocol.md well enough for unit tests, E2E
// tests, and local development on a machine with no real controller
// attached (see cmd/masso-sim). It builds on the codec in internal/masso
// but does not depend on internal/masso's Client.
package sim

import (
	"fmt"
	"log/slog"
	"net"
	"sync"

	"msrl.dev/mink-lasso/internal/masso"
)

// defaultAddr is used when Options.Addr is empty: loopback only, a random
// free port.
const defaultAddr = "127.0.0.1:0"

// defaultPrompt is the Status.Prompt value a fresh Controller starts with:
// 0x01 ("normal"), so an untouched simulator reads as idle and ready rather
// than "waiting for the operator" (docs/protocol.md §4). SetStatus
// overrides it.
const defaultPrompt = 0x01

// Options configures New.
type Options struct {
	// Addr is the UDP address to listen on. Empty means "127.0.0.1:0" (a
	// random loopback port).
	Addr string

	// Serial is the controller serial number returned in Identity and
	// ConfigReply.
	Serial uint16

	// Version is the firmware version string returned in Identity (for
	// example "5-Axis v5.13").
	Version string

	// Tools names the simulated tool table, indexed 1..len(Tools). A
	// query outside that range gets an empty name, like a real controller
	// with no tool in that slot.
	Tools []string

	// Logger receives a debug-level entry for every dropped or malformed
	// packet and a failed reply write, and an info-level entry for every
	// decoded request and every completed upload. Nil means
	// slog.New(slog.DiscardHandler).
	Logger *slog.Logger

	// ListenPacket opens the listening socket. Nil means net.ListenPacket.
	// Overriding it lets tests force a bind failure or substitute a
	// net.PacketConn whose ReadFrom, WriteTo, or LocalAddr behavior is
	// otherwise impossible to arrange with a real socket.
	ListenPacket func(network, address string) (net.PacketConn, error)
}

// Controller is a fake Masso controller bound to a UDP socket. Once
// created by New it serves requests on its own goroutine until Close.
// A zero Controller is not usable; construct one with New.
type Controller struct {
	conn    net.PacketConn
	logger  *slog.Logger
	serial  uint16
	version string
	tools   []string

	mu          sync.Mutex
	status      masso.Status
	replyTarget *net.UDPAddr
	discoveries int
	keepalives  int

	uploadName    string
	uploadSize    uint32
	uploadData    []byte
	acceptedCount uint32
	seenChunk     map[uint32]bool

	files map[string][]byte

	startResult      byte
	chunkResult      byte
	dropAck          func(chunkIndex uint32) bool
	duplicateReplies bool
	silentAfterChunk int
	silent           bool

	done      chan struct{}
	wg        sync.WaitGroup
	closeOnce sync.Once
	closeErr  error
}

// New binds a UDP socket per opts and starts a goroutine that serves
// requests on it until Close.
func New(opts Options) (*Controller, error) {
	if opts.Addr == "" {
		opts.Addr = defaultAddr
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	listenPacket := opts.ListenPacket
	if listenPacket == nil {
		listenPacket = net.ListenPacket
	}

	conn, err := listenPacket("udp", opts.Addr)
	if err != nil {
		return nil, fmt.Errorf("%w on %s: %w", ErrListen, opts.Addr, err)
	}

	c := &Controller{
		conn:             conn,
		logger:           opts.Logger,
		serial:           opts.Serial,
		version:          opts.Version,
		tools:            append([]string(nil), opts.Tools...),
		status:           masso.Status{Prompt: defaultPrompt},
		files:            make(map[string][]byte),
		seenChunk:        make(map[uint32]bool),
		silentAfterChunk: -1,
		done:             make(chan struct{}),
	}
	c.wg.Add(1)
	go c.serve()
	return c, nil
}

// Addr returns the socket's local address.
func (c *Controller) Addr() *net.UDPAddr {
	if a, ok := c.conn.LocalAddr().(*net.UDPAddr); ok {
		return a
	}
	return nil
}

// Close stops the serving goroutine and closes the socket. It is safe to
// call more than once; only the first call's result is returned.
func (c *Controller) Close() error {
	c.closeOnce.Do(func() {
		close(c.done)
		c.closeErr = c.conn.Close()
		c.wg.Wait()
	})
	return c.closeErr
}

// serve reads and handles datagrams until the socket errors, which happens
// once Close closes it.
func (c *Controller) serve() {
	defer c.wg.Done()
	buf := make([]byte, masso.MaxPacket)
	for {
		n, addr, err := c.conn.ReadFrom(buf)
		if err != nil {
			select {
			case <-c.done:
				c.logger.Debug("sim: socket closed")
			default:
				c.logger.Debug("sim: read failed, stopping", "error", err)
			}
			return
		}
		pkt := make([]byte, n)
		copy(pkt, buf[:n])
		c.handlePacket(pkt, addr)
	}
}

// handlePacket decodes and dispatches one datagram. A malformed packet, or
// any packet received while silenced, is dropped without a reply.
func (c *Controller) handlePacket(pkt []byte, src net.Addr) {
	if c.shouldStaySilent() {
		return
	}

	req, err := masso.DecodeRequest(pkt)
	if err != nil {
		c.logger.Debug("sim: dropping malformed packet", "error", err, "from", src)
		return
	}
	c.logger.Info("sim: request", "type", fmt.Sprintf("%T", req), "from", src)

	switch r := req.(type) {
	case masso.DiscoveryRequest:
		c.handleDiscovery(r, src)
	case masso.ConfigRequest:
		c.handleConfig(src)
	case masso.KeepaliveRequest:
		c.handleKeepalive(src)
	case masso.ToolQueryRequest:
		c.handleToolQuery(r, src)
	case masso.UploadStartRequest:
		c.handleUploadStart(r, src)
	case masso.UploadChunkRequest:
		c.handleUploadChunk(r, src)
	default:
		// masso.Request's implementation set is closed to the masso
		// package; DecodeRequest never actually returns anything else.
	}
}

// shouldStaySilent reports whether SetSilent or SetSilentAfterChunk
// currently suppress every reply.
func (c *Controller) shouldStaySilent() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.silent {
		return true
	}
	return c.silentAfterChunk >= 0 && int(c.acceptedCount) > c.silentAfterChunk
}

// targetFor returns where a reply to a request from src should go: the
// stored reply target once a discovery request has set one, or src itself
// beforehand (docs/protocol.md §1).
func (c *Controller) targetFor(src net.Addr) net.Addr {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.replyTarget != nil {
		return c.replyTarget
	}
	return src
}

func (c *Controller) handleDiscovery(r masso.DiscoveryRequest, src net.Addr) {
	target, err := replyAddrFor(src, r.ReplyPort)
	if err != nil {
		c.logger.Debug("sim: discovery: cannot determine reply target", "error", err, "from", src)
		return
	}

	c.mu.Lock()
	c.replyTarget = target
	c.discoveries++
	serial, version := c.serial, c.version
	c.mu.Unlock()

	c.send(masso.Identity{Serial: serial, Version: version}.Encode(), target)
}

func (c *Controller) handleConfig(src net.Addr) {
	c.mu.Lock()
	serial := c.serial
	c.mu.Unlock()
	c.send(masso.ConfigReply{Serial: serial}.Encode(), c.targetFor(src))
}

func (c *Controller) handleKeepalive(src net.Addr) {
	c.mu.Lock()
	c.keepalives++
	status := c.status
	c.mu.Unlock()
	c.send(status.Encode(), c.targetFor(src))
}

func (c *Controller) handleToolQuery(r masso.ToolQueryRequest, src net.Addr) {
	c.mu.Lock()
	var name string
	if idx := int(r.Index); idx >= 1 && idx <= len(c.tools) {
		name = c.tools[idx-1]
	}
	c.mu.Unlock()
	c.send(masso.ToolRecord{Index: r.Index, Name: name}.Encode(), c.targetFor(src))
}

func (c *Controller) handleUploadStart(r masso.UploadStartRequest, src net.Addr) {
	c.mu.Lock()
	c.uploadName = r.Name
	c.uploadSize = r.Size
	// The final size is already known, so preallocate it rather than
	// growing uploadData one append at a time as chunks arrive, which
	// would otherwise reallocate and copy the whole buffer several times
	// over the course of a large upload.
	c.uploadData = make([]byte, 0, r.Size)
	c.acceptedCount = 0
	c.seenChunk = make(map[uint32]bool)
	result := c.startResult
	c.mu.Unlock()

	c.send(masso.StartAck{Result: result}.Encode(), c.targetFor(src))
}

// handleUploadChunk implements the chunk state machine described in the
// package's caller-facing API: a chunk is accepted, advancing Accepted,
// only when its index matches the number of chunks accepted so far;
// anything else (a retransmit of an already-accepted chunk, or a chunk that
// arrived out of order) is re-acknowledged with the current Accepted count
// and does not change stored state. SetDropAck's callback runs at most once
// per chunk index — the first time that index is seen — so a client's
// retransmit of a dropped chunk always gets through.
func (c *Controller) handleUploadChunk(r masso.UploadChunkRequest, src net.Addr) {
	c.mu.Lock()
	first := !c.seenChunk[r.Index]
	c.seenChunk[r.Index] = true
	dropFn := c.dropAck
	c.mu.Unlock()

	drop := first && dropFn != nil && dropFn(r.Index)

	var storedName string
	var storedSize int
	justCompleted := false

	c.mu.Lock()
	if !drop && r.Index == c.acceptedCount {
		c.uploadData = append(c.uploadData, r.Data...)
		c.acceptedCount++
		if len(c.uploadData) == int(c.uploadSize) {
			stored := make([]byte, len(c.uploadData))
			copy(stored, c.uploadData)
			c.files[c.uploadName] = stored
			storedName, storedSize, justCompleted = c.uploadName, len(stored), true
		}
	}
	accepted := c.acceptedCount
	result := c.chunkResult
	c.mu.Unlock()

	if justCompleted {
		c.logger.Info(fmt.Sprintf("stored %s (%d bytes)", storedName, storedSize))
	}
	if drop {
		return
	}
	c.send(masso.ChunkAck{Result: result, Accepted: accepted}.Encode(), c.targetFor(src))
}

// send writes data to target, and a second time if SetDuplicateReplies(true)
// is in effect. A write failure is logged and otherwise ignored: the real
// controller has no way to know a reply was lost either.
func (c *Controller) send(data []byte, target net.Addr) {
	c.write(data, target)
	c.mu.Lock()
	dup := c.duplicateReplies
	c.mu.Unlock()
	if dup {
		c.write(data, target)
	}
}

func (c *Controller) write(data []byte, target net.Addr) {
	if _, err := c.conn.WriteTo(data, target); err != nil {
		c.logger.Debug("sim: write reply failed", "error", err, "target", target)
	}
}

// replyAddrFor derives the UDP reply target a discovery request asks for:
// the request's source IP, paired with the port it advertised
// (docs/protocol.md §1). It works from src.String() rather than a type
// assertion on src so it accepts any net.Addr whose string form is
// "host:port", not just *net.UDPAddr.
func replyAddrFor(src net.Addr, port uint16) (*net.UDPAddr, error) {
	host, _, err := net.SplitHostPort(src.String())
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrBadSourceAddr, err)
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return nil, fmt.Errorf("%w: %q is not an IP address", ErrBadSourceAddr, host)
	}
	return &net.UDPAddr{IP: ip, Port: int(port)}, nil
}

// SetStatus replaces the status reported to every future keepalive request.
// A fresh Controller reports idle, zero progress.
func (c *Controller) SetStatus(s masso.Status) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.status = s
}

// SetStartResult replaces the result byte sent in every future upload-start
// ACK. The default is masso.StartOK.
func (c *Controller) SetStartResult(result byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.startResult = result
}

// SetChunkResult replaces the result byte sent in every future chunk ACK.
// The default is masso.ChunkOK.
func (c *Controller) SetChunkResult(result byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.chunkResult = result
}

// SetDropAck installs a callback consulted the first time each chunk index
// is seen in the current upload: when it returns true for that index, the
// Controller silently drops the ACK for that one request instead of
// sending it. Because the callback only runs once per index, a client's
// subsequent retransmit of the same chunk is always acknowledged normally
// — this knob simulates a single lost ACK, not a permanently unreachable
// chunk. Pass nil to stop dropping any ACK.
func (c *Controller) SetDropAck(f func(chunkIndex uint32) bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.dropAck = f
}

// SetDuplicateReplies makes every future reply, of any kind, get sent
// twice.
func (c *Controller) SetDuplicateReplies(enabled bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.duplicateReplies = enabled
}

// SetSilentAfterChunk stops the Controller from answering anything —
// including discovery, config, keepalive, and tool queries, not only
// upload chunks — once the chunk at index n has been accepted. Pass -1 (the
// default) to disable.
func (c *Controller) SetSilentAfterChunk(n int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.silentAfterChunk = n
}

// SetSilent makes the Controller ignore every request, as though the
// controller were powered off.
func (c *Controller) SetSilent(silent bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.silent = silent
}

// Discoveries reports how many discovery requests have been answered.
func (c *Controller) Discoveries() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.discoveries
}

// Keepalives reports how many keepalive requests have been answered.
func (c *Controller) Keepalives() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.keepalives
}

// LastReplyTarget returns the address every reply is currently sent to, or
// nil if no discovery request has arrived yet.
func (c *Controller) LastReplyTarget() *net.UDPAddr {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.replyTarget
}

// Files returns a snapshot of every stored upload, keyed by name. Each
// byte slice is a copy, safe for the caller to keep or mutate.
func (c *Controller) Files() map[string][]byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string][]byte, len(c.files))
	for name, data := range c.files {
		cp := make([]byte, len(data))
		copy(cp, data)
		out[name] = cp
	}
	return out
}

// File returns a copy of one stored upload's bytes, and whether it exists.
func (c *Controller) File(name string) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	data, ok := c.files[name]
	if !ok {
		return nil, false
	}
	cp := make([]byte, len(data))
	copy(cp, data)
	return cp, true
}
