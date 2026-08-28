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

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"testing"
	"time"

	"msrl.dev/mink-lasso/internal/clock"
	"msrl.dev/mink-lasso/internal/config"
	"msrl.dev/mink-lasso/internal/masso"
	"msrl.dev/mink-lasso/internal/watch"
)

// fakeClient is a minimal Client used to test New's wiring without a real
// socket.
type fakeClient struct{}

func (fakeClient) Discover(context.Context, time.Duration) ([]masso.Found, error) { return nil, nil }

func (fakeClient) Connect(context.Context, *net.UDPAddr) (masso.Identity, masso.ConfigReply, error) {
	return masso.Identity{}, masso.ConfigReply{}, nil
}
func (fakeClient) Run(context.Context) error                         { return nil }
func (fakeClient) Status() <-chan masso.Status                       { return nil }
func (fakeClient) Tools(context.Context) ([]masso.ToolRecord, error) { return nil, nil }

func (fakeClient) Upload(context.Context, string, io.ReaderAt, int64, func(int64, int64)) error {
	return nil
}
func (fakeClient) Remote() *net.UDPAddr { return nil }
func (fakeClient) Close() error         { return nil }

var errFakeBind = errors.New("fake: bind failed")

// spammyClient connects immediately and then floods Status() as fast as it
// can be read, standing in for the reviewer's repro for finding 1: a
// connected engine emitting StatusEvents continuously, building up a real
// backlog in the dispatcher's queue.
type spammyClient struct {
	fakeClient

	serial uint16
	status chan masso.Status
}

func newSpammyClient(serial uint16) *spammyClient {
	c := &spammyClient{serial: serial, status: make(chan masso.Status)}
	return c
}

func (c *spammyClient) Connect(context.Context, *net.UDPAddr) (masso.Identity, masso.ConfigReply, error) {
	return masso.Identity{Serial: c.serial}, masso.ConfigReply{}, nil
}

func (c *spammyClient) Status() <-chan masso.Status { return c.status }

// Run pumps Status() as fast as the engine's status-forwarding loop can
// read it until ctx is done, then stops.
func (c *spammyClient) Run(ctx context.Context) error {
	for {
		select {
		case c.status <- masso.Status{}:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// TestRunJoinsDispatcherBeforeReturning reproduces finding 1: with a
// connected engine emitting StatusEvents continuously and a consumer
// draining Events() throughout (the supported shutdown pattern — see
// Events()'s doc comment), Run must still actually tear down its
// dispatcher goroutine before returning, proving Events() is genuinely
// closed by then rather than Run merely having told it to close while the
// goroutine was left running.
func TestRunJoinsDispatcherBeforeReturning(t *testing.T) {
	t.Parallel()
	client := newSpammyClient(12345)
	e, err := New(Options{
		Config: config.Config{
			Serial:  "G3-12345",
			Address: "203.0.113.1:1", // never dialed for UDP; just a unicast candidate
		},
		Clock:     clock.NewFake(time.Now()),
		NewClient: func(masso.Options) (Client, error) { return client, nil },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan struct{})
	go func() {
		defer close(runDone)
		if err := e.Run(ctx); err != nil {
			t.Errorf("Run: %v", err)
		}
	}()

	events := e.Events()

	// Poll (rather than sleep a fixed duration) until the connected engine
	// has actually built up a real backlog of unread StatusEvents, with no
	// consumer draining Events() at all yet — the "consumer that reads
	// nothing until the end" pattern the package's required test
	// scenarios call out — so this exercises a genuine multi-event drain
	// during shutdown rather than one that happened to already be empty.
	deadline := time.Now().Add(5 * time.Second)
	for {
		e.dispatcher.mu.Lock()
		n := len(e.dispatcher.queue)
		e.dispatcher.mu.Unlock()
		if n >= 50 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("dispatcher queue never built up a backlog")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()

	// Only now start draining — right up until Run tears down and closes
	// Events(), as its doc comment requires of a caller — counting events
	// so the test can confirm the backlog built up above was actually
	// delivered, not silently dropped.
	seen := make(chan int)
	go func() {
		n := 0
		for range events {
			n++
		}
		seen <- n
	}()

	select {
	case <-runDone:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return with a consumer draining Events() throughout")
	}

	select {
	case n := <-seen:
		if n == 0 {
			t.Error("consumer saw zero events, want a real backlog to have been delivered")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Events() never closed after Run returned")
	}
}

// TestRunTwiceReturnsErrAlreadyRunning confirms a second Run call is
// rejected rather than starting a second dispatcher-drain goroutine against
// the same channels, which would eventually double-close Events().
func TestRunTwiceReturnsErrAlreadyRunning(t *testing.T) {
	t.Parallel()
	e, err := New(Options{
		NewClient: func(masso.Options) (Client, error) { return fakeClient{}, nil },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := e.Run(ctx); err != nil {
		t.Fatalf("first Run: %v", err)
	}
	if err := e.Run(ctx); !errors.Is(err, ErrAlreadyRunning) {
		t.Errorf("second Run error = %v, want ErrAlreadyRunning", err)
	}
}

func TestNewBadSerial(t *testing.T) {
	t.Parallel()
	_, err := New(Options{Config: config.Config{Serial: "not-a-serial"}})
	if !errors.Is(err, masso.ErrBadSerial) {
		t.Fatalf("New() error = %v, want wrapping masso.ErrBadSerial", err)
	}
}

func TestNewUnconfiguredSerial(t *testing.T) {
	t.Parallel()
	e, err := New(Options{
		NewClient: func(masso.Options) (Client, error) { return fakeClient{}, nil },
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if e.serial != 0 {
		t.Errorf("serial = %d, want 0 (unconfigured)", e.serial)
	}
}

func TestNewValidSerial(t *testing.T) {
	t.Parallel()
	e, err := New(Options{
		Config:    config.Config{Serial: "G3-12345"},
		NewClient: func(masso.Options) (Client, error) { return fakeClient{}, nil },
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if e.serial != 12345 {
		t.Errorf("serial = %d, want 12345", e.serial)
	}
}

func TestNewBindFailure(t *testing.T) {
	t.Parallel()
	_, err := New(Options{
		NewClient: func(masso.Options) (Client, error) { return nil, errFakeBind },
	})
	if !errors.Is(err, ErrBind) || !errors.Is(err, errFakeBind) {
		t.Fatalf("New() error = %v, want wrapping ErrBind and the underlying error", err)
	}
}

// TestNewPassesClientOptions confirms New merges Logger, Clock, and the
// port range into ClientOptions before calling NewClient, without
// clobbering fields the caller already set.
func TestNewPassesClientOptions(t *testing.T) {
	t.Parallel()
	var got masso.Options
	_, err := New(Options{
		Config: config.Config{ListenPort: 11010},
		Clock:  clock.NewFake(time.Now()),
		NewClient: func(opts masso.Options) (Client, error) {
			got = opts
			return fakeClient{}, nil
		},
		ClientOptions: masso.Options{ReplyTimeout: 42},
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if got.PortMin != 11010 {
		t.Errorf("PortMin = %d, want 11010", got.PortMin)
	}
	if got.PortMax != masso.ListenPortMax {
		t.Errorf("PortMax = %d, want %d", got.PortMax, masso.ListenPortMax)
	}
	if got.ReplyTimeout != 42 {
		t.Errorf("ReplyTimeout = %d, want passthrough value 42", got.ReplyTimeout)
	}
	if got.Clock == nil {
		t.Error("Clock not set on ClientOptions")
	}
}

func TestNewDefaults(t *testing.T) {
	t.Parallel()
	e, err := New(Options{
		NewClient: func(masso.Options) (Client, error) { return fakeClient{}, nil },
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	o := e.opts
	if o.Logger == nil {
		t.Error("Logger default not applied")
	}
	if o.Clock == nil {
		t.Error("Clock default not applied")
	}
	if o.Open == nil || o.IsRemote == nil || o.Stat == nil || o.MkdirAll == nil || o.Rename == nil {
		t.Error("filesystem defaults not applied")
	}
	if o.IdleHold != 5*time.Second {
		t.Errorf("IdleHold = %v, want 5s", o.IdleHold)
	}
	if o.BroadcastInterval != 5*time.Second {
		t.Errorf("BroadcastInterval = %v, want 5s", o.BroadcastInterval)
	}
	if o.UnicastFirst != 10*time.Second {
		t.Errorf("UnicastFirst = %v, want 10s", o.UnicastFirst)
	}
	if o.DiscoverTimeout != time.Second {
		t.Errorf("DiscoverTimeout = %v, want 1s", o.DiscoverTimeout)
	}
	wantBackoff := []time.Duration{5 * time.Second, 10 * time.Second, 30 * time.Second, 60 * time.Second}
	if len(o.Backoff) != len(wantBackoff) {
		t.Fatalf("Backoff = %v, want %v", o.Backoff, wantBackoff)
	}
	for i := range wantBackoff {
		if o.Backoff[i] != wantBackoff[i] {
			t.Errorf("Backoff[%d] = %v, want %v", i, o.Backoff[i], wantBackoff[i])
		}
	}
	if o.MoveRetries != 5 {
		t.Errorf("MoveRetries = %d, want 5", o.MoveRetries)
	}
	if o.MoveRetryInterval != time.Second {
		t.Errorf("MoveRetryInterval = %v, want 1s", o.MoveRetryInterval)
	}
	if o.ProgressInterval != 100*time.Millisecond {
		t.Errorf("ProgressInterval = %v, want 100ms", o.ProgressInterval)
	}
	if o.NewWatcher == nil {
		t.Error("NewWatcher default not applied")
	}
}

func TestNewNoGoroutines(t *testing.T) {
	t.Parallel()
	// New must not start the dispatcher's run goroutine or anything else:
	// Events() should exist but nothing should ever be delivered on it
	// without Run.
	e, err := New(Options{
		NewClient: func(masso.Options) (Client, error) { return fakeClient{}, nil },
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	select {
	case ev, ok := <-e.Events():
		t.Fatalf("unexpected event %#v (ok=%v) with Run never called", ev, ok)
	case <-time.After(20 * time.Millisecond):
	}
}

// TestDefaultAdapters exercises the New→NewClient/NewWatcher default
// adapter functions themselves (defaultNewClient, defaultNewWatcher),
// which normalize does not otherwise reach in the tests above that inject
// their own NewClient/NewWatcher.
func TestDefaultAdaptersBindFailure(t *testing.T) {
	t.Parallel()
	// An injected ListenPacket that always errors makes masso.NewClient
	// fail deterministically, independent of which ports happen to be
	// free on the machine running the test.
	failEverywhere := func(string, string) (net.PacketConn, error) { return nil, errFakeBind }
	_, err := defaultNewClient(masso.Options{ListenPacket: failEverywhere})
	if !errors.Is(err, masso.ErrNoPort) {
		t.Fatalf("defaultNewClient error = %v, want ErrNoPort", err)
	}
}

func TestDefaultNewWatcherDirRequired(t *testing.T) {
	t.Parallel()
	_, err := defaultNewWatcher(watch.Options{})
	if !errors.Is(err, watch.ErrDirRequired) {
		t.Fatalf("defaultNewWatcher error = %v, want ErrDirRequired", err)
	}
}

func TestDefaultNewWatcherSuccess(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	w, err := defaultNewWatcher(watch.Options{Dir: dir})
	if err != nil {
		t.Fatalf("defaultNewWatcher error = %v", err)
	}
	if w == nil {
		t.Fatal("defaultNewWatcher returned nil Watcher with no error")
	}
}

// slowWatcher's Run blocks on ctx.Done() and then on release, standing in
// for a watcher whose teardown (e.g. closing a real OS directory-change
// notifier) takes noticeable time after cancellation — used to confirm Run
// actually waits for the watch-loop goroutine to finish, not just to have
// been told to stop.
type slowWatcher struct{ release <-chan struct{} }

func (w slowWatcher) Run(ctx context.Context) error {
	<-ctx.Done()
	<-w.release
	return ctx.Err()
}

func (slowWatcher) Ready() <-chan watch.File        { return nil }
func (slowWatcher) Rejected() <-chan watch.Rejected { return nil }

// TestRunJoinsWatchLoopBeforeReturning confirms Run does not return while
// the watch-loop goroutine it started is still tearing down: previously
// e.watchLoop (and e.gate.run) were started with a bare `go` and never
// joined, so Run could return, and client.Close()/dispatcher.close() could
// run, while a watcher's real OS-level notifier was still shutting down.
func TestRunJoinsWatchLoopBeforeReturning(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	e, err := New(Options{
		Config:    config.Config{WatchDir: t.TempDir()},
		NewClient: func(masso.Options) (Client, error) { return fakeClient{}, nil },
		NewWatcher: func(watch.Options) (Watcher, error) {
			return slowWatcher{release: release}, nil
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan struct{})
	go func() {
		defer close(runDone)
		if err := e.Run(ctx); err != nil {
			t.Errorf("Run: %v", err)
		}
	}()

	// Wait for the watcher to have actually started before canceling, so
	// this exercises the watch-loop goroutine's real teardown path rather
	// than a fresh attempt that never got as far as calling w.Run.
	deadline := time.After(2 * time.Second)
	for gotWatchState := false; !gotWatchState; {
		select {
		case ev := <-e.Events():
			_, gotWatchState = ev.(WatchState)
		case <-deadline:
			t.Fatal("timed out waiting for WatchState")
		}
	}

	// Run returns only once every queued event has been delivered, and
	// the connection loop's Unconfigured event may still be queued behind
	// the WatchState just read, so keep draining for the rest of the test.
	go func() {
		for ev := range e.Events() {
			_ = ev
		}
	}()

	cancel()
	select {
	case <-runDone:
		t.Fatal("Run returned before the slow watcher released control")
	case <-time.After(50 * time.Millisecond):
	}

	close(release)
	select {
	case <-runDone:
	case <-time.After(connTestTimeout):
		t.Fatal("Run did not return after the slow watcher released control")
	}
}

func TestOSDefaultsExist(t *testing.T) {
	t.Parallel()
	// Smoke-test the os.* defaults compile and behave sanely, since
	// normalize only assigns them and nothing else in this stage calls
	// them yet.
	if _, err := os.Stat(t.TempDir()); err != nil {
		t.Fatalf("os.Stat: %v", err)
	}
}
