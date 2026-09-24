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
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net"
	"runtime"
	"testing"
	"time"

	"msrl.dev/mink-lasso/internal/clock"
)

// fakeConn wraps a real net.PacketConn so a test can override WriteTo or
// ReadFrom while delegating everything else — including real timing and a
// real Close — to the underlying connection.
type fakeConn struct {
	net.PacketConn

	writeTo  func(p []byte, addr net.Addr) (int, error)
	readFrom func(p []byte) (int, net.Addr, error)
}

func (f *fakeConn) WriteTo(p []byte, addr net.Addr) (int, error) {
	if f.writeTo != nil {
		return f.writeTo(p, addr)
	}
	return f.PacketConn.WriteTo(p, addr)
}

func (f *fakeConn) ReadFrom(p []byte) (int, net.Addr, error) {
	if f.readFrom != nil {
		return f.readFrom(p)
	}
	return f.PacketConn.ReadFrom(p)
}

// fakeReply is a Reply implementation outside this package's own closed
// set, used only to exercise replyTypeOf's default case.
type fakeReply struct{}

func (fakeReply) isReply() {}

func TestResolveOptionsDefaults(t *testing.T) {
	t.Parallel()
	got := resolveOptions(Options{})
	if got.Logger == nil {
		t.Error("Logger default is nil")
	}
	if got.Clock == nil {
		t.Error("Clock default is nil")
	}
	if got.PortMin != ListenPortMin {
		t.Errorf("PortMin = %d, want %d", got.PortMin, ListenPortMin)
	}
	if got.PortMax != ListenPortMax {
		t.Errorf("PortMax = %d, want %d", got.PortMax, ListenPortMax)
	}
	if got.ListenPacket == nil {
		t.Error("ListenPacket default is nil")
	}
	if got.Interfaces == nil {
		t.Error("Interfaces default is nil")
	}
	if got.InterfaceAddrs == nil {
		t.Error("InterfaceAddrs default is nil")
	}
	if got.KeepaliveInterval != time.Second {
		t.Errorf("KeepaliveInterval = %v, want 1s", got.KeepaliveInterval)
	}
	if got.LostAfter != 5*time.Second {
		t.Errorf("LostAfter = %v, want 5s", got.LostAfter)
	}
	if got.ReplyTimeout != time.Second {
		t.Errorf("ReplyTimeout = %v, want 1s", got.ReplyTimeout)
	}
	if got.StartRetransmit != time.Second {
		t.Errorf("StartRetransmit = %v, want 1s", got.StartRetransmit)
	}
	if got.StartTimeout != 5*time.Second {
		t.Errorf("StartTimeout = %v, want 5s", got.StartTimeout)
	}
	if got.StallTimeout != 15*time.Second {
		t.Errorf("StallTimeout = %v, want 15s", got.StallTimeout)
	}
	if got.AbortInterval != 20*time.Millisecond {
		t.Errorf("AbortInterval = %v, want 20ms", got.AbortInterval)
	}
}

func TestResolveOptionsPreservesOverrides(t *testing.T) {
	t.Parallel()
	logger := slog.New(slog.DiscardHandler)
	cl := clock.NewFake(time.Unix(0, 0))
	sentinel := errors.New("sentinel")
	lp := func(string, string) (net.PacketConn, error) { return nil, sentinel }
	ifs := func() ([]net.Interface, error) { return nil, sentinel }
	addrs := func(*net.Interface) ([]net.Addr, error) { return nil, sentinel }

	got := resolveOptions(Options{
		Logger: logger, Clock: cl, PortMin: 1, PortMax: 2,
		ListenPacket: lp, Interfaces: ifs, InterfaceAddrs: addrs,
		KeepaliveInterval: 2 * time.Second, LostAfter: 3 * time.Second,
		ReplyTimeout: 4 * time.Second, StartRetransmit: 5 * time.Second, StartTimeout: 7 * time.Second,
		StallTimeout: 6 * time.Second, AbortInterval: 8 * time.Second,
	})

	if got.Logger != logger {
		t.Error("Logger override not preserved")
	}
	if got.Clock != cl {
		t.Error("Clock override not preserved")
	}
	if got.PortMin != 1 || got.PortMax != 2 {
		t.Errorf("PortMin/PortMax = %d/%d, want 1/2", got.PortMin, got.PortMax)
	}
	if _, err := got.ListenPacket("", ""); !errors.Is(err, sentinel) {
		t.Error("ListenPacket override not preserved")
	}
	if _, err := got.Interfaces(); !errors.Is(err, sentinel) {
		t.Error("Interfaces override not preserved")
	}
	if _, err := got.InterfaceAddrs(&net.Interface{}); !errors.Is(err, sentinel) {
		t.Error("InterfaceAddrs override not preserved")
	}
	if got.KeepaliveInterval != 2*time.Second {
		t.Errorf("KeepaliveInterval = %v, want 2s", got.KeepaliveInterval)
	}
	if got.LostAfter != 3*time.Second {
		t.Errorf("LostAfter = %v, want 3s", got.LostAfter)
	}
	if got.ReplyTimeout != 4*time.Second {
		t.Errorf("ReplyTimeout = %v, want 4s", got.ReplyTimeout)
	}
	if got.StartRetransmit != 5*time.Second {
		t.Errorf("StartRetransmit = %v, want 5s", got.StartRetransmit)
	}
	if got.StartTimeout != 7*time.Second {
		t.Errorf("StartTimeout = %v, want 7s", got.StartTimeout)
	}
	if got.StallTimeout != 6*time.Second {
		t.Errorf("StallTimeout = %v, want 6s", got.StallTimeout)
	}
	if got.AbortInterval != 8*time.Second {
		t.Errorf("AbortInterval = %v, want 8s", got.AbortInterval)
	}
}

func TestNewClientPortFallback(t *testing.T) {
	t.Parallel()
	const failN = 3
	var attempts []string
	listen := func(_, address string) (net.PacketConn, error) {
		attempts = append(attempts, address)
		if len(attempts) <= failN {
			return nil, errors.New("bind refused")
		}
		return net.ListenPacket("udp", "127.0.0.1:0")
	}
	base := freePort(t)
	c, err := NewClient(Options{PortMin: base, PortMax: base + 10, ListenPacket: listen})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer func() {
		if err := c.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	}()
	if len(attempts) != failN+1 {
		t.Fatalf("attempts = %d, want %d", len(attempts), failN+1)
	}
	for i, want := range []string{
		fmt.Sprintf("0.0.0.0:%d", base),
		fmt.Sprintf("0.0.0.0:%d", base+1),
		fmt.Sprintf("0.0.0.0:%d", base+2),
		fmt.Sprintf("0.0.0.0:%d", base+3),
	} {
		if attempts[i] != want {
			t.Errorf("attempts[%d] = %q, want %q", i, attempts[i], want)
		}
	}
}

func TestNewClientAllPortsFail(t *testing.T) {
	t.Parallel()
	listen := func(string, string) (net.PacketConn, error) { return nil, errors.New("bind refused") }
	base := freePort(t)
	_, err := NewClient(Options{PortMin: base, PortMax: base + 2, ListenPacket: listen})
	if !errors.Is(err, ErrNoPort) {
		t.Fatalf("err = %v, want ErrNoPort", err)
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	t.Parallel()
	c := newTestClient(t, clock.Real{})
	if err := c.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func TestLocalPortMatchesBoundPort(t *testing.T) {
	t.Parallel()
	port := freePort(t)
	c, err := NewClient(Options{PortMin: port, PortMax: port})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer func() { _ = c.Close() }()
	if int(c.LocalPort()) != port {
		t.Errorf("LocalPort() = %d, want %d", c.LocalPort(), port)
	}
}

func TestRemoteNilBeforeConnect(t *testing.T) {
	t.Parallel()
	c := newTestClient(t, clock.Real{})
	if got := c.Remote(); got != nil {
		t.Errorf("Remote() = %v, want nil", got)
	}
}

func TestReaderStopsOnUnexpectedReadError(t *testing.T) {
	t.Parallel()
	realConn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("ListenPacket: %v", err)
	}
	readErr := errors.New("boom")
	fc := &fakeConn{PacketConn: realConn, readFrom: func([]byte) (int, net.Addr, error) { return 0, nil, readErr }}
	logger, logged := captureLogger()
	port := freePort(t)
	c, err := NewClient(Options{
		PortMin: port, PortMax: port, Logger: logger,
		ListenPacket: func(string, string) (net.PacketConn, error) { return fc, nil },
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	// The reader goroutine fails every read from the start, so it logs and
	// exits on its own; wait for that log before calling Close, so the
	// read-failure branch is exercised deterministically instead of racing
	// Close's own close(c.done) (which would otherwise sometimes make the
	// reader take the "socket closed" branch instead).
	waitFor(t, logged)
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestSetLatestStatusReplacesUndrainedValue(t *testing.T) {
	t.Parallel()
	ch := make(chan Status, 1)
	setLatestStatus(ch, Status{Progress: 1})
	setLatestStatus(ch, Status{Progress: 2})
	got := <-ch
	if got.Progress != 2 {
		t.Fatalf("Progress = %d, want 2 (the newer value should win)", got.Progress)
	}
	select {
	case <-ch:
		t.Fatal("channel held a second value after a single drain")
	default:
	}
}

func TestReplyTypeOfKnownTypes(t *testing.T) {
	t.Parallel()
	// handlePacket special-cases Status before ever calling replyTypeOf, so
	// this direct call is the only way to cover that branch.
	if got := replyTypeOf(Status{}); got != TypeStatus {
		t.Fatalf("replyTypeOf(Status{}) = %d, want TypeStatus", got)
	}
}

func TestSendKeepaliveLogsSendFailure(t *testing.T) {
	t.Parallel()
	writeErr := errors.New("boom")
	realConn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("ListenPacket: %v", err)
	}
	fc := &fakeConn{PacketConn: realConn, writeTo: func([]byte, net.Addr) (int, error) { return 0, writeErr }}
	port := freePort(t)
	c, err := NewClient(Options{
		PortMin: port, PortMax: port,
		ListenPacket: func(string, string) (net.PacketConn, error) { return fc, nil },
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer func() { _ = c.Close() }()

	// sendKeepalive only logs a send failure; it does not panic or return
	// anything for the caller to check.
	c.sendKeepalive(&net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 1})
}

func TestConnectConfigFailure(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	// A minimal fake controller that answers discovery but silently drops
	// config requests, so Connect's discovery phase succeeds while its
	// config phase exhausts its attempts. internal/masso/sim has no knob
	// for this split, so this uses a raw responder instead.
	fake := rawConn(t)
	go func() {
		buf := make([]byte, MaxPacket)
		for {
			n, addr, err := fake.ReadFromUDP(buf)
			if err != nil {
				return
			}
			req, err := DecodeRequest(buf[:n])
			if err != nil {
				continue
			}
			dr, ok := req.(DiscoveryRequest)
			if !ok {
				continue
			}
			reply := &net.UDPAddr{IP: addr.IP, Port: int(dr.ReplyPort)}
			_, _ = fake.WriteToUDP(Identity{Serial: 1}.Encode(), reply)
		}
	}()
	fakeAddr, ok := fake.LocalAddr().(*net.UDPAddr)
	if !ok {
		t.Fatalf("LocalAddr() = %T, want *net.UDPAddr", fake.LocalAddr())
	}

	const replyTimeout = 30 * time.Millisecond
	fcClock := clock.NewFake(time.Unix(0, 0))
	port := freePort(t)
	c, err := NewClient(Options{Clock: fcClock, PortMin: port, PortMax: port, ReplyTimeout: replyTimeout})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer func() { _ = c.Close() }()

	errCh := make(chan error, 1)
	go func() {
		_, _, err := c.Connect(ctx, fakeAddr)
		errCh <- err
	}()

	// Let discovery resolve via the real reply above without ever touching
	// the fake clock — advancing it before the reply arrives would race
	// request()'s own timer against that reply. Wait until Connect has
	// moved on to config, which only registers its own waiter once
	// discovery has already succeeded (mirrors internal/clock's own tests,
	// which wait on a goroutine reaching a blocking point this way).
	for {
		c.mu.Lock()
		n := len(c.waiters[TypeConfig])
		c.mu.Unlock()
		if n > 0 {
			break
		}
		runtime.Gosched()
	}

	// Config gets three silent attempts.
	for range defaultAttempts {
		fcClock.BlockUntil(1)
		fcClock.Advance(replyTimeout)
	}

	if err := waitFor(t, errCh); !errors.Is(err, ErrNoResponse) {
		t.Fatalf("Connect = %v, want ErrNoResponse", err)
	}
}

func TestToolsRequestErrorPropagates(t *testing.T) {
	t.Parallel()
	writeErr := errors.New("boom")
	realConn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("ListenPacket: %v", err)
	}
	fc := &fakeConn{PacketConn: realConn, writeTo: func([]byte, net.Addr) (int, error) { return 0, writeErr }}
	port := freePort(t)
	c, err := NewClient(Options{
		Clock: clock.Real{}, PortMin: port, PortMax: port,
		ListenPacket: func(string, string) (net.PacketConn, error) { return fc, nil },
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer func() { _ = c.Close() }()
	c.setConnection(&net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 1}, Identity{})

	_, err = c.Tools(t.Context())
	if !errors.Is(err, ErrSend) {
		t.Fatalf("Tools = %v, want ErrSend", err)
	}
}

func TestMustPanicsOnError(t *testing.T) {
	t.Parallel()
	defer func() {
		if recover() == nil {
			t.Fatal("must did not panic on a non-nil error")
		}
	}()
	must(0, errors.New("boom"))
}

func TestMustTypePanicsOnMismatch(t *testing.T) {
	t.Parallel()
	defer func() {
		if recover() == nil {
			t.Fatal("mustType did not panic on a type mismatch")
		}
	}()
	mustType[ConfigReply](StartAck{})
}

func TestReplyTypeOfUnknown(t *testing.T) {
	t.Parallel()
	if got := replyTypeOf(fakeReply{}); got != 0 {
		t.Fatalf("replyTypeOf(fakeReply{}) = %d, want 0", got)
	}
}

func TestReaderDropsWithNoWaiter(t *testing.T) {
	t.Parallel()
	logger, logged := captureLogger()
	port := freePort(t)
	c, err := NewClient(Options{PortMin: port, PortMax: port, Logger: logger})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	t.Cleanup(func() {
		if err := c.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	fake := rawConn(t)
	caddr := clientAddr(c)

	// No waiter registered at all: this must be dropped harmlessly. Wait
	// for the drop's debug log before registering a waiter, so a slow
	// reader goroutine cannot instead deliver this packet to the waiter
	// registered below, which would otherwise be a real race.
	mustSend(t, fake, caddr, ToolRecord{Index: 1, Name: "drill"}.Encode())
	waitFor(t, logged)

	// A subsequent, properly waited-for exchange must still work, proving
	// the unwaited packet above did not corrupt any state.
	ch, cancel := c.expect(TypeTool, nil, anyReply)
	defer cancel()
	mustSend(t, fake, caddr, ToolRecord{Index: 2, Name: "endmill"}.Encode())
	in := waitFor(t, ch)
	tr, ok := in.reply.(ToolRecord)
	if !ok || tr.Index != 2 {
		t.Fatalf("got %+v, want ToolRecord{Index:2}", in.reply)
	}
}

func TestReaderDropsWhenMatcherRejects(t *testing.T) {
	t.Parallel()
	c := newTestClient(t, clock.Real{})
	fake := rawConn(t)
	caddr := clientAddr(c)

	match := func(r Reply) bool {
		tr, ok := r.(ToolRecord)
		return ok && tr.Index == 1
	}
	ch, cancel := c.expect(TypeTool, nil, match)
	defer cancel()

	mustSend(t, fake, caddr, ToolRecord{Index: 2, Name: "rejected"}.Encode())
	mustSend(t, fake, caddr, ToolRecord{Index: 1, Name: "accepted"}.Encode())

	in := waitFor(t, ch)
	tr, ok := in.reply.(ToolRecord)
	if !ok || tr.Index != 1 || tr.Name != "accepted" {
		t.Fatalf("got %+v, want the matching ToolRecord", in.reply)
	}
	expectSilence(t, ch, 100*time.Millisecond)
}

func TestReaderDropsWhenWaiterBufferFull(t *testing.T) {
	t.Parallel()
	full := make(chan struct{}, 1)
	logger := slog.New(messageSignalHandler{ch: full, want: "masso: dropping reply, waiter buffer full"})
	port := freePort(t)
	c, err := NewClient(Options{PortMin: port, PortMax: port, Logger: logger})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	t.Cleanup(func() {
		if err := c.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	fake := rawConn(t)
	caddr := clientAddr(c)

	ch, cancel := c.expect(TypeTool, nil, anyReply)
	defer cancel()

	// Flood without draining until the buffer actually overflows — sending
	// a fixed count and hoping it exceeds waiterBuffer would race the
	// reader goroutine's own progress, since nothing here drains ch to
	// force a pileup. A background goroutine keeps sending until the
	// buffer-full log confirms an overflow actually happened.
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			pkt := ToolRecord{Index: uint8(i % 118), Name: "x"}.Encode()
			_, _ = fake.WriteToUDP(pkt, caddr)
		}
	}()
	waitFor(t, full)

	// Confirm the reader kept delivering up to capacity instead of wedging.
	for range waiterBuffer {
		waitFor(t, ch)
	}
}

func TestRunErrNotConnected(t *testing.T) {
	t.Parallel()
	c := newTestClient(t, clock.Real{})
	if err := c.Run(t.Context()); !errors.Is(err, ErrNotConnected) {
		t.Fatalf("Run = %v, want ErrNotConnected", err)
	}
}

func TestToolsErrNotConnected(t *testing.T) {
	t.Parallel()
	c := newTestClient(t, clock.Real{})
	if _, err := c.Tools(t.Context()); !errors.Is(err, ErrNotConnected) {
		t.Fatalf("Tools = %v, want ErrNotConnected", err)
	}
}

func TestUploadErrNotConnected(t *testing.T) {
	t.Parallel()
	c := newTestClient(t, clock.Real{})
	err := c.Upload(t.Context(), "", "A.NC", bytes.NewReader(nil), 0, nil)
	if !errors.Is(err, ErrNotConnected) {
		t.Fatalf("Upload = %v, want ErrNotConnected", err)
	}
}

func TestUploadOversize(t *testing.T) {
	t.Parallel()
	c := newTestClient(t, clock.Real{})
	err := c.Upload(t.Context(), "", "BIG.NC", bytes.NewReader(nil), int64(math.MaxUint32)+1, nil)
	if !errors.Is(err, ErrFileTooLarge) {
		t.Fatalf("Upload = %v, want ErrFileTooLarge", err)
	}
}

func TestUploadNegativeSize(t *testing.T) {
	t.Parallel()
	c := newTestClient(t, clock.Real{})
	err := c.Upload(t.Context(), "", "NEG.NC", bytes.NewReader(nil), -1, nil)
	if !errors.Is(err, ErrFileTooLarge) {
		t.Fatalf("Upload = %v, want ErrFileTooLarge", err)
	}
}

func TestRequestCtxAlreadyCanceled(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	c := newTestClient(t, clock.Real{})
	dst := &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 1}
	_, err := c.request(ctx, TypeConfig, nil, anyReply, []byte("pkt"), dst, 5*time.Second, 1)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("request = %v, want context.Canceled", err)
	}
}

func TestRequestSendFailure(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	writeErr := errors.New("write boom")
	realConn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("ListenPacket: %v", err)
	}
	fc := &fakeConn{PacketConn: realConn, writeTo: func([]byte, net.Addr) (int, error) { return 0, writeErr }}
	port := freePort(t)
	c, err := NewClient(Options{
		PortMin: port, PortMax: port,
		ListenPacket: func(string, string) (net.PacketConn, error) { return fc, nil },
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer func() { _ = c.Close() }()

	dst := &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 12345}
	_, err = c.DiscoverAt(ctx, dst, time.Second)
	if !errors.Is(err, ErrSend) {
		t.Fatalf("DiscoverAt = %v, want ErrSend", err)
	}
}
