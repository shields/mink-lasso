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

package engine

// This file covers branches of conn.go and Run that don't need a real
// socket — bad candidate addresses, ctx cancellation mid-discovery, the
// broadcast loop's error and rationing paths, and Run's client.Close error
// — by driving the unexported methods directly against a stubbed Client.

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"msrl.dev/mink-lasso/internal/clock"
	"msrl.dev/mink-lasso/internal/config"
	"msrl.dev/mink-lasso/internal/masso"
)

// stubClient overrides Discover and Connect on top of fakeClient's no-op
// defaults, so a test can script exactly the discovery/connect behavior a
// particular branch needs.
type stubClient struct {
	fakeClient

	discover func(ctx context.Context, timeout time.Duration) ([]masso.Found, error)
	connect  func(ctx context.Context, addr *net.UDPAddr) (masso.Identity, masso.ConfigReply, error)
}

func (s stubClient) Discover(ctx context.Context, timeout time.Duration) ([]masso.Found, error) {
	return s.discover(ctx, timeout)
}

func (s stubClient) Connect(ctx context.Context, addr *net.UDPAddr) (masso.Identity, masso.ConfigReply, error) {
	return s.connect(ctx, addr)
}

// newStubEngine builds an *Engine around client, with clk supplying all
// timing, without starting Run.
func newStubEngine(t *testing.T, cfg config.Config, clk clock.Clock, client Client) *Engine {
	t.Helper()
	e, err := New(Options{
		Config: cfg,
		Clock:  clk,
		NewClient: func(masso.Options) (Client, error) {
			return client, nil
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return e
}

func TestSetUploadWhileMachining(t *testing.T) {
	t.Parallel()
	e := newStubEngine(t, config.Config{}, clock.NewFake(time.Now()), fakeClient{})
	e.SetUploadWhileMachining(true)
	e.gate.connected()
	if got := e.gate.current(); !got.Open {
		t.Errorf("gate after SetUploadWhileMachining(true) = %+v, want open", got)
	}
	e.SetUploadWhileMachining(false)
	if got := e.gate.current(); got.Open {
		t.Errorf("gate after SetUploadWhileMachining(false) = %+v, want closed", got)
	}
}

// TestCandidateAddrsBadAddressesSkipped confirms an unparsable
// Config.Address and Config.LastAddress are both logged and skipped rather
// than propagated as an error.
func TestCandidateAddrsBadAddressesSkipped(t *testing.T) {
	t.Parallel()
	e := newStubEngine(t, config.Config{
		Address:     "not a host:port",
		LastAddress: "also not one",
	}, clock.NewFake(time.Now()), fakeClient{})
	if got := e.candidateAddrs(); len(got) != 0 {
		t.Errorf("candidateAddrs() = %v, want empty (both unparsable)", got)
	}
}

// TestCandidateAddrsPrefersInMemoryLastAddr confirms a remembered lastAddr
// from a previous connect is used instead of re-parsing Config.LastAddress.
func TestCandidateAddrsPrefersInMemoryLastAddr(t *testing.T) {
	t.Parallel()
	e := newStubEngine(t, config.Config{LastAddress: "203.0.113.1:1"}, clock.NewFake(time.Now()), fakeClient{})
	remembered := &net.UDPAddr{IP: net.IPv4(203, 0, 113, 9), Port: 9}
	e.mu.Lock()
	e.lastAddr = remembered
	e.mu.Unlock()

	got := e.candidateAddrs()
	if len(got) != 1 || got[0] != remembered {
		t.Errorf("candidateAddrs() = %v, want [%v]", got, remembered)
	}
}

// TestDiscoverAndConnectCtxDoneDuringUnicast confirms an already-canceled
// ctx is returned promptly from inside the unicast candidate loop, never
// calling Connect.
func TestDiscoverAndConnectCtxDoneDuringUnicast(t *testing.T) {
	t.Parallel()
	client := stubClient{
		connect: func(context.Context, *net.UDPAddr) (masso.Identity, masso.ConfigReply, error) {
			t.Fatal("Connect must not be called once ctx is already done")
			return masso.Identity{}, masso.ConfigReply{}, nil
		},
	}
	e := newStubEngine(t, config.Config{Address: "127.0.0.1:12345"}, clock.NewFake(time.Now()), client)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err := e.discoverAndConnect(ctx, 42)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("discoverAndConnect error = %v, want context.Canceled", err)
	}
}

// TestDiscoverAndConnectUnicastWrongSerial confirms a unicast candidate
// that answers Connect successfully but with the wrong serial is logged and
// treated as a miss rather than accepted.
func TestDiscoverAndConnectUnicastWrongSerial(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	client := stubClient{
		connect: func(context.Context, *net.UDPAddr) (masso.Identity, masso.ConfigReply, error) {
			// Cancel from inside Connect itself: the next loop iteration's
			// ctx.Err() check ends the test deterministically, with no
			// concurrency needed, right after this wrong-serial branch runs.
			cancel()
			return masso.Identity{Serial: 999}, masso.ConfigReply{}, nil
		},
	}
	e := newStubEngine(t, config.Config{Address: "127.0.0.1:12345"}, clock.NewFake(time.Now()), client)
	e.opts.UnicastFirst = time.Hour

	_, _, err := e.discoverAndConnect(ctx, 42)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("discoverAndConnect error = %v, want context.Canceled", err)
	}
}

// TestDiscoverAndConnectUnicastMiss confirms a unicast candidate that
// returns a real error from Connect (e.g. a timeout) is logged and treated
// as a miss, not returned as discoverAndConnect's own error. This exercises
// the branch end-to-end tests only hit by real-clock luck (a reconnect
// attempt racing a still-silent sim), which made it a source of flaky
// coverage under -race.
func TestDiscoverAndConnectUnicastMiss(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	client := stubClient{
		connect: func(context.Context, *net.UDPAddr) (masso.Identity, masso.ConfigReply, error) {
			// Cancel from inside Connect itself: the next loop iteration's
			// ctx.Err() check ends the test deterministically, with no
			// concurrency needed, right after this miss branch runs.
			cancel()
			return masso.Identity{}, masso.ConfigReply{}, errStubConnect
		},
	}
	e := newStubEngine(t, config.Config{Address: "127.0.0.1:12345"}, clock.NewFake(time.Now()), client)
	e.opts.UnicastFirst = time.Hour

	_, _, err := e.discoverAndConnect(ctx, 42)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("discoverAndConnect error = %v, want context.Canceled", err)
	}
}

var errStubDiscover = errors.New("stub: discover failed")

// TestBroadcastConnectDiscoverErrorLogsAndPaces confirms a synchronous
// Discover failure (e.g. masso.ErrDiscoverySend, which the real masso
// client can return without ever blocking) is logged at Info and paced on
// the same BroadcastInterval ticker as an unmatched discovery, rather than
// spinning the loop with zero delay and no operator-visible sign of
// trouble (finding 3).
func TestBroadcastConnectDiscoverErrorLogsAndPaces(t *testing.T) {
	t.Parallel()

	var logMu sync.Mutex
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&syncWriter{mu: &logMu, w: &logBuf}, nil))

	var count atomic.Int32
	called := make(chan struct{}, 100)
	client := stubClient{
		discover: func(context.Context, time.Duration) ([]masso.Found, error) {
			count.Add(1)
			called <- struct{}{}
			return nil, errStubDiscover
		},
	}
	e := newStubEngine(t, config.Config{}, clock.NewFake(time.Now()), client)
	e.opts.Logger = logger
	e.opts.BroadcastInterval = time.Second
	clk, ok := e.opts.Clock.(*clock.Fake)
	if !ok {
		t.Fatalf("e.opts.Clock = %T, want *clock.Fake", e.opts.Clock)
	}

	ctx, cancel := context.WithCancel(context.Background())
	resultCh := make(chan error, 1)
	go func() {
		_, _, err := e.broadcastConnect(ctx, 42)
		resultCh <- err
	}()

	waitForChan(t, called) // first Discover call has landed
	if got := count.Load(); got != 1 {
		t.Fatalf("Discover called %d times, want exactly 1 before the clock advances", got)
	}

	// Without advancing the fake clock, the loop's only ways forward are
	// the BroadcastInterval ticker (which cannot fire) or ctx.Done (not
	// yet canceled): a real busy loop would run this many times over in a
	// moment, so a short real-time pause here reliably catches a
	// regression back to the unpaced loop without depending on exact
	// timing for a *passing* run.
	time.Sleep(20 * time.Millisecond)
	if got := count.Load(); got != 1 {
		t.Fatalf("Discover called %d times with the clock never advanced, want exactly 1 (busy loop?)", got)
	}

	clk.Advance(time.Second)
	waitForChan(t, called)
	if got := count.Load(); got != 2 {
		t.Fatalf("Discover called %d times after one BroadcastInterval, want exactly 2", got)
	}

	cancel()
	select {
	case err := <-resultCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("broadcastConnect error = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("broadcastConnect did not return after ctx cancel")
	}

	logMu.Lock()
	logged := logBuf.String()
	logMu.Unlock()
	if !strings.Contains(logged, "discovery failed") {
		t.Errorf("log output = %q, want it to mention the discovery failure", logged)
	}
}

// waitForChan waits up to 5s for a value on ch, failing the test otherwise.
func waitForChan(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for expected call")
	}
}

// syncWriter guards w with mu so a test goroutine can safely read what the
// logger goroutine wrote.
type syncWriter struct {
	mu *sync.Mutex
	w  *bytes.Buffer
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}

// TestRunConnAttemptLogsConnected confirms a successful connect is logged
// at Info (finding 5): the package's own doc comment on Options.Logger
// promises an Info line for "connected", alongside "file sent" and
// failures, but only failures were actually logged.
func TestRunConnAttemptLogsConnected(t *testing.T) {
	t.Parallel()
	var logMu sync.Mutex
	var logBuf bytes.Buffer
	client := stubClient{
		connect: func(context.Context, *net.UDPAddr) (masso.Identity, masso.ConfigReply, error) {
			return masso.Identity{Serial: 42}, masso.ConfigReply{}, nil
		},
	}
	e := newStubEngine(t, config.Config{Address: "127.0.0.1:12345"}, clock.NewFake(time.Now()), client)
	e.opts.Logger = slog.New(slog.NewTextHandler(&syncWriter{mu: &logMu, w: &logBuf}, nil))
	e.serial = 42

	// fakeClient.Run returns nil immediately, so runConnAttempt completes
	// this whole pass (connect, then runConnected) on its own.
	e.runConnAttempt(context.Background())

	logMu.Lock()
	logged := logBuf.String()
	logMu.Unlock()
	if !strings.Contains(logged, "engine: connected") {
		t.Errorf("log output = %q, want it to mention the successful connect", logged)
	}
}

// TestBroadcastConnectCtxDoneWhileWaiting confirms ctx cancellation while
// waiting out BroadcastInterval between empty rounds is returned promptly.
func TestBroadcastConnectCtxDoneWhileWaiting(t *testing.T) {
	t.Parallel()
	client := stubClient{
		discover: func(context.Context, time.Duration) ([]masso.Found, error) {
			return nil, nil // never finds anything: every round waits out the ticker
		},
	}
	e := newStubEngine(t, config.Config{}, clock.NewFake(time.Now()), client)
	ctx, cancel := context.WithCancel(context.Background())

	resultCh := make(chan error, 1)
	go func() {
		_, _, err := e.broadcastConnect(ctx, 42)
		resultCh <- err
	}()
	cancel()

	select {
	case err := <-resultCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("broadcastConnect error = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("broadcastConnect did not return after ctx cancel")
	}
}

// TestBroadcastConnectRationsAcrossRounds confirms a round with no matching
// serial waits out a full BroadcastInterval (via the fake clock) before
// trying again, and then connects to the match found on the second round.
func TestBroadcastConnectRationsAcrossRounds(t *testing.T) {
	t.Parallel()
	clk := clock.NewFake(time.Now())
	matchAddr := &net.UDPAddr{IP: net.IPv4(198, 51, 100, 1), Port: 1}
	round := 0
	client := stubClient{
		discover: func(context.Context, time.Duration) ([]masso.Found, error) {
			round++
			if round == 1 {
				return []masso.Found{{Addr: matchAddr, Identity: masso.Identity{Serial: 1}}}, nil // wrong serial
			}
			return []masso.Found{{Addr: matchAddr, Identity: masso.Identity{Serial: 42}}}, nil
		},
		connect: func(context.Context, *net.UDPAddr) (masso.Identity, masso.ConfigReply, error) {
			return masso.Identity{Serial: 42}, masso.ConfigReply{}, nil
		},
	}
	e := newStubEngine(t, config.Config{}, clk, client)
	e.opts.BroadcastInterval = time.Second

	resultCh := make(chan *net.UDPAddr, 1)
	go func() {
		addr, _, err := e.broadcastConnect(context.Background(), 42)
		if err != nil {
			t.Errorf("broadcastConnect error = %v", err)
			return
		}
		resultCh <- addr
	}()

	clk.BlockUntil(1)
	clk.Advance(time.Second)

	select {
	case addr := <-resultCh:
		if addr != matchAddr {
			t.Errorf("broadcastConnect addr = %v, want %v", addr, matchAddr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("broadcastConnect did not return after the second round")
	}
	if round != 2 {
		t.Errorf("Discover called %d times, want 2", round)
	}
}

var errStubConnect = errors.New("stub: connect failed")

// TestBroadcastConnectMatchButConnectFails confirms a Found entry whose
// serial matches, but whose subsequent Connect call fails, is logged and
// the round moves on rather than being accepted.
func TestBroadcastConnectMatchButConnectFails(t *testing.T) {
	t.Parallel()
	matchAddr := &net.UDPAddr{IP: net.IPv4(198, 51, 100, 2), Port: 2}
	ctx, cancel := context.WithCancel(context.Background())
	client := stubClient{
		discover: func(context.Context, time.Duration) ([]masso.Found, error) {
			return []masso.Found{{Addr: matchAddr, Identity: masso.Identity{Serial: 42}}}, nil
		},
		connect: func(context.Context, *net.UDPAddr) (masso.Identity, masso.ConfigReply, error) {
			// Cancel from inside Connect: broadcastConnect's next select
			// (ctx.Done vs the BroadcastInterval ticker) then returns
			// immediately, right after this failure branch runs.
			cancel()
			return masso.Identity{}, masso.ConfigReply{}, errStubConnect
		},
	}
	e := newStubEngine(t, config.Config{}, clock.NewFake(time.Now()), client)

	_, _, err := e.broadcastConnect(ctx, 42)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("broadcastConnect error = %v, want context.Canceled", err)
	}
}

var errStubClose = errors.New("stub: close failed")

type closeErrClient struct{ fakeClient }

func (closeErrClient) Close() error { return errStubClose }

// TestRunClientCloseError confirms a failing client.Close is wrapped and
// returned by Run.
func TestRunClientCloseError(t *testing.T) {
	t.Parallel()
	e := newStubEngine(t, config.Config{}, clock.NewFake(time.Now()), closeErrClient{})

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already done: connLoop returns immediately
	if err := e.Run(ctx); !errors.Is(err, errStubClose) {
		t.Fatalf("Run error = %v, want wrapping errStubClose", err)
	}
}
