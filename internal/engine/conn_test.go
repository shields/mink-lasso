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

// This file exercises the connection loop (conn.go) end-to-end against a
// real internal/masso/sim controller over loopback UDP, using a real
// *masso.Client wrapped through clientAdapter. Real broadcast discovery
// cannot reach a loopback simulator on every OS, and would re-target any
// real controller on the LAN, so every client here discovers only an
// unanswered loopback port (see unansweredDiscoveryTargets); the
// broadcast-path tests wrap the real client in discoverStubClient to supply
// a canned Discover result while every other method — Connect, Run, Status,
// Tools — still talks to the real sim over the real socket.

import (
	"context"
	"net"
	"slices"
	"sync"
	"testing"
	"time"

	"msrl.dev/mink-lasso/internal/config"
	"msrl.dev/mink-lasso/internal/masso"
	"msrl.dev/mink-lasso/internal/masso/sim"
)

// connTestTimeout bounds how long a test waits for something driven by real,
// asynchronous socket I/O before failing. It only catches a hung test and
// decides no outcome, so it is far longer than any wait a loaded machine
// needs, such as an in-flight upload from before a reconnect waiting out
// LostAfter and StallTimeout.
const connTestTimeout = 90 * time.Second

// Raw masso.Status.State/.Prompt wire values (docs/protocol.md §4) used to
// drive the simulator's status directly: masso.Status.Running and
// .WaitingForOperator are derived read-only fields (see reply.go's
// DecodeReply), not inputs to Status.Encode, so a test that wants the sim
// to actually report Running or WaitingForOperator must set these raw
// bytes rather than the derived bools.
const (
	simStateStopped = 0x00
	simStateRunning = 0x02
	simPromptNormal = 0x01
)

// newConnTestNewClient returns an Options.NewClient that binds a real
// *masso.Client on its own private port range (ignoring whatever range the
// engine's normalize computed, so tests are not limited to
// masso.ListenPortMin-ListenPortMax) while keeping every other field —
// Logger, Clock, and the short timeouts a test sets — as the engine
// prepared them. The range is a handful of ports wide, not a single pinned
// one, so masso.NewClient's own retry absorbs a port a moment too slow to
// release from a just-finished test rather than failing the next one.
func newConnTestNewClient(port int) func(masso.Options) (Client, error) {
	return func(o masso.Options) (Client, error) {
		o.PortMin, o.PortMax = port, port+9
		c, err := masso.NewClient(o)
		if err != nil {
			return nil, err
		}
		return clientAdapter{c}, nil
	}
}

// newConnTestSim starts a simulator with short-lived timeouts suitable for
// a real (not fake) clock, and returns it alongside matching Options.
func newConnTestSim(t *testing.T, serial uint32) *sim.Controller {
	t.Helper()
	s, err := sim.New(sim.Options{Serial: serial, Version: "5-Axis v5.13"})
	if err != nil {
		t.Fatalf("sim.New: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("sim Close: %v", err)
		}
	})
	return s
}

// connTestOptions returns Options with every timing field short enough for
// a real clock to exercise quickly, and the given serial configured. The
// caller still supplies NewClient and, typically, Config.Address.
//
// The retry and pacing intervals are short: missing one only costs another
// retry. LostAfter decides what the engine does when it expires, so it is
// long enough that load cannot trip it in a test that expects to stay
// connected; a test that wants a Lost shortens it itself.
func connTestOptions(serial uint32) Options {
	return Options{
		Config: config.Config{Serial: masso.SerialString(serial)},
		ClientOptions: masso.Options{
			ReplyTimeout:      20 * time.Millisecond,
			KeepaliveInterval: 20 * time.Millisecond,
			LostAfter:         2 * time.Second,
			DiscoveryTargets:  unansweredDiscoveryTargets(),
		},
		IdleHold:          time.Millisecond,
		UnicastFirst:      300 * time.Millisecond,
		BroadcastInterval: 40 * time.Millisecond,
		DiscoverTimeout:   30 * time.Millisecond,
	}
}

// waitForEvent drains e.Events() until pred returns true for some event,
// failing the test if connTestTimeout elapses first. Every intervening
// event is ignored, since these tests care about reaching a particular
// state, not every state passed through on the way there.
func waitForEvent(t *testing.T, events <-chan Event, pred func(Event) bool) Event {
	t.Helper()
	deadline := time.After(connTestTimeout)
	for {
		select {
		case ev, ok := <-events:
			if !ok {
				t.Fatal("events channel closed before the expected event arrived")
			}
			if pred(ev) {
				return ev
			}
		case <-deadline:
			t.Fatal("timed out waiting for expected event")
		}
	}
}

func isConnState(kind ConnKind) func(Event) bool {
	return func(ev Event) bool {
		cs, ok := ev.(ConnState)
		return ok && cs.Kind == kind
	}
}

func isToolsEvent(ev Event) bool { _, ok := ev.(ToolsEvent); return ok }

// asConnState asserts ev is a ConnState, failing the test (rather than
// panicking) if a predicate bug ever lets something else through.
func asConnState(t *testing.T, ev Event) ConnState {
	t.Helper()
	cs, ok := ev.(ConnState)
	if !ok {
		t.Fatalf("event = %#v, want ConnState", ev)
	}
	return cs
}

// runEngine starts e.Run in the background and returns a cancel func that
// stops it and waits for Run to return, for t.Cleanup.
func runEngine(t *testing.T, e *Engine) (events <-chan Event, stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := e.Run(ctx); err != nil {
			t.Errorf("Run: %v", err)
		}
	}()
	stop = func() {
		cancel()
		// Run waits for Events() to be fully drained before it returns
		// (see its doc comment), and the test body's own reads may have
		// already stopped by now (its last waitForEvent call done, or
		// never having read at all) — so keep draining here to actually
		// unblock Run's shutdown rather than deadlocking this cleanup.
		drained := make(chan struct{})
		go func() {
			defer close(drained)
			for ev := range e.Events() {
				_ = ev
			}
		}()
		select {
		case <-done:
		case <-time.After(connTestTimeout):
			t.Fatal("Run did not return after ctx cancel")
		}
		<-drained
	}
	t.Cleanup(stop)
	return e.Events(), stop
}

// TestConnUnicastFirstZeroBroadcasts confirms that with Config.Address set,
// the engine connects via unicast Connect alone and never calls Discover,
// because discoverAndConnect never reaches broadcastConnect when the
// unicast candidate succeeds.
func TestConnUnicastFirstZeroBroadcasts(t *testing.T) {
	t.Parallel()
	s := newConnTestSim(t, 111)
	port := freeAdapterPort()

	opts := connTestOptions(111)
	opts.Config.Address = s.Addr().String()
	var rc *recordingClient
	realNewClient := newConnTestNewClient(port)
	opts.NewClient = func(o masso.Options) (Client, error) {
		c, err := realNewClient(o)
		if err != nil {
			return nil, err
		}
		rc = &recordingClient{Client: c}
		return rc, nil
	}

	e, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	events, _ := runEngine(t, e)

	ev := waitForEvent(t, events, isConnState(Connected))
	cs := asConnState(t, ev)
	if cs.Identity.Serial != 111 {
		t.Errorf("Connected Identity.Serial = %d, want 111", cs.Identity.Serial)
	}
	if got := rc.discoverCalls(); got != 0 {
		t.Errorf("Discover called %d times, want 0 (unicast only, no broadcast)", got)
	}
	assertOnlyConnectedTo(t, rc, s.Addr())
}

// recordingClient wraps a real Client, records every Discover and Connect
// call, and optionally replaces Discover's result with a canned one so
// broadcast-path tests can drive the engine's serial matching and Connect
// logic without relying on OS broadcast delivery to a loopback simulator.
// Tests assert on these records rather than on sim.Discoveries(), which
// also counts the retransmits Connect sends when a reply is slow under
// load.
type recordingClient struct {
	Client

	found     []masso.Found
	stub      bool
	mu        sync.Mutex
	discovers int
	connects  []*net.UDPAddr
}

func (r *recordingClient) Discover(ctx context.Context, timeout time.Duration) ([]masso.Found, error) {
	r.mu.Lock()
	r.discovers++
	r.mu.Unlock()
	if r.stub {
		return r.found, nil
	}
	return r.Client.Discover(ctx, timeout)
}

func (r *recordingClient) Connect(ctx context.Context, addr *net.UDPAddr) (masso.Identity, masso.ConfigReply, error) {
	r.mu.Lock()
	r.connects = append(r.connects, addr)
	r.mu.Unlock()
	return r.Client.Connect(ctx, addr)
}

func (r *recordingClient) discoverCalls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.discovers
}

func (r *recordingClient) connectAddrs() []*net.UDPAddr {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.connects)
}

// assertOnlyConnectedTo fails unless every Connect call went to want and
// there was at least one.
func assertOnlyConnectedTo(t *testing.T, rc *recordingClient, want *net.UDPAddr) {
	t.Helper()
	addrs := rc.connectAddrs()
	if len(addrs) == 0 {
		t.Fatal("Connect was never called")
	}
	for _, a := range addrs {
		if a.String() != want.String() {
			t.Errorf("Connect called with %v, want only %v", a, want)
		}
	}
}

// TestConnBroadcastFindsBySerialIgnoringWrongSerial covers the broadcast
// path: no Config.Address is set, so the engine goes straight to
// broadcastConnect; a wrong-serial candidate in the canned Discover result
// is logged and skipped (never connected to), and the matching one is
// connected for real.
func TestConnBroadcastFindsBySerialIgnoringWrongSerial(t *testing.T) {
	t.Parallel()
	s := newConnTestSim(t, 222)
	port := freeAdapterPort()
	realNewClient := newConnTestNewClient(port)

	opts := connTestOptions(222)
	var rc *recordingClient
	opts.NewClient = func(o masso.Options) (Client, error) {
		c, err := realNewClient(o)
		if err != nil {
			return nil, err
		}
		rc = &recordingClient{
			Client: c,
			stub:   true,
			found: []masso.Found{
				{Addr: &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1}, Identity: masso.Identity{Serial: 999}},
				{Addr: s.Addr(), Identity: masso.Identity{Serial: 222}},
			},
		}
		return rc, nil
	}

	e, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	events, _ := runEngine(t, e)

	ev := waitForEvent(t, events, isConnState(Connected))
	cs := asConnState(t, ev)
	if cs.Identity.Serial != 222 {
		t.Errorf("Connected Identity.Serial = %d, want 222", cs.Identity.Serial)
	}
	assertOnlyConnectedTo(t, rc, s.Addr())
}

// TestConnToolsFetchedOnConnectAndRefresh confirms a ToolsEvent is emitted
// once on connect and again on RefreshTools.
func TestConnToolsFetchedOnConnectAndRefresh(t *testing.T) {
	t.Parallel()
	s := newConnTestSim(t, 333)
	port := freeAdapterPort()

	opts := connTestOptions(333)
	opts.Config.Address = s.Addr().String()
	opts.NewClient = newConnTestNewClient(port)

	e, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	events, _ := runEngine(t, e)

	waitForEvent(t, events, isToolsEvent)

	e.RefreshTools()
	waitForEvent(t, events, isToolsEvent)
}

// TestConnLostThenReconnect confirms a connection that goes silent is
// reported Lost and then reconnected once the simulator answers again,
// using the same last-known address.
func TestConnLostThenReconnect(t *testing.T) {
	t.Parallel()
	s := newConnTestSim(t, 444)
	port := freeAdapterPort()

	opts := connTestOptions(444)
	opts.Config.Address = s.Addr().String()
	opts.NewClient = newConnTestNewClient(port)
	opts.ClientOptions.LostAfter = 300 * time.Millisecond

	e, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	events, _ := runEngine(t, e)

	waitForEvent(t, events, isConnState(Connected))

	s.SetSilent(true)
	waitForEvent(t, events, isConnState(Lost))

	s.SetSilent(false)
	waitForEvent(t, events, isConnState(Connected))
}

// TestConnLostThenReconnectQueuedFileCompletes confirms a file dropped
// while the connection is Lost sits Waiting("Not connected") and is sent
// once the controller answers again — the gate/scheduler interaction with
// a genuine post-connection Lost->reconnect cycle (distinct from
// TestSchedulerWaitsForConnection's Unconfigured->SetSerial path).
func TestConnLostThenReconnectQueuedFileCompletes(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s := newConnTestSim(t, 446)
	opts := schedTestOptions(446, dir)
	opts.Config.Address = s.Addr().String()
	opts.NewClient = newConnTestNewClient(freeAdapterPort())
	opts.ClientOptions.LostAfter = 300 * time.Millisecond

	e, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	events, _ := runEngine(t, e)
	waitForEvent(t, events, isConnState(Connected))

	s.SetSilent(true)
	waitForEvent(t, events, isConnState(Lost))

	writeFile(t, dir, "E.NC", []byte("eeee"))
	ev := waitForEvent(t, events, isTransferEvent("E.NC", Waiting))
	if te := asTransferEvent(t, ev); te.Message != "Not connected" {
		t.Errorf("Waiting message = %q, want %q", te.Message, "Not connected")
	}

	s.SetSilent(false)
	waitForEvent(t, events, isConnState(Connected))
	waitForEvent(t, events, isTransferEvent("E.NC", Sent))
}

// TestGateEndToEndRunningThenIdleHoldThenSend confirms a real masso.Status
// delivered over the connected client's Status() channel actually reaches
// gate.updateStatus and drives the scheduler: a file dropped while the
// simulator reports Running Waits ("Machining"), and is sent only once the
// machine has been reported idle continuously for IdleHold.
func TestGateEndToEndRunningThenIdleHoldThenSend(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s := newConnTestSim(t, 1011)
	opts := schedTestOptions(1011, dir)
	opts.Config.Address = s.Addr().String()
	opts.NewClient = newConnTestNewClient(freeAdapterPort())
	opts.IdleHold = 80 * time.Millisecond
	// Set before the engine ever connects, so the very first status it
	// forwards to the gate already reports Running — avoiding a race with
	// the default idle status the sim would otherwise deliver first.
	s.SetStatus(masso.Status{State: simStateRunning, Prompt: simPromptNormal})

	e, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	events, _ := runEngine(t, e)
	waitForEvent(t, events, isConnState(Connected))

	writeFile(t, dir, "M.NC", []byte("x"))
	ev := waitForEvent(t, events, isTransferEvent("M.NC", Waiting))
	if te := asTransferEvent(t, ev); te.Message != "Machining" {
		t.Errorf("Waiting message = %q, want %q", te.Message, "Machining")
	}

	s.SetStatus(masso.Status{State: simStateStopped, Progress: 100, Prompt: simPromptNormal})
	waitForEvent(t, events, isTransferEvent("M.NC", Sent))
}

// TestGateEndToEndPauseGrace confirms the pause-grace case over a real
// status feed: the machine stopping mid-job (0 < Progress < 100) holds
// uploads for PauseGrace (reason "Job paused"), on top of IdleHold, before
// sending.
func TestGateEndToEndPauseGrace(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s := newConnTestSim(t, 1012)
	opts := schedTestOptions(1012, dir)
	opts.Config.Address = s.Addr().String()
	opts.NewClient = newConnTestNewClient(freeAdapterPort())
	opts.IdleHold = 30 * time.Millisecond
	opts.Config.PauseGrace = config.Duration(150 * time.Millisecond)
	// Set before the engine ever connects, so the very first status it
	// forwards to the gate already reports Running — avoiding a race with
	// the default idle status the sim would otherwise deliver first.
	s.SetStatus(masso.Status{State: simStateRunning, Progress: 50, Prompt: simPromptNormal})

	e, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	events, _ := runEngine(t, e)
	waitForEvent(t, events, isConnState(Connected))

	writeFile(t, dir, "M.NC", []byte("x"))
	waitForEvent(t, events, isTransferEvent("M.NC", Waiting))

	// Stopped mid-job (feed hold/e-stop look like "stopped" on the wire):
	// the pause-grace hold applies on top of IdleHold. The scheduler only
	// re-checks the gate's reason text when something else wakes it (a
	// gate-reason change alone does not, by design: signalReadyLocked only
	// wakes the scheduler once the gate actually opens), so this asserts
	// the timing guarantee that matters — the send is held close to
	// PauseGrace, not just IdleHold — rather than an intermediate "Job
	// paused" TransferEvent that may or may not be observed.
	stoppedAt := time.Now()
	s.SetStatus(masso.Status{State: simStateStopped, Progress: 50, Prompt: simPromptNormal})
	waitForEvent(t, events, isTransferEvent("M.NC", Sent))
	if elapsed := time.Since(stoppedAt); elapsed < 100*time.Millisecond {
		t.Errorf("sent %v after going idle mid-job, want held close to PauseGrace (%v)", elapsed, 150*time.Millisecond)
	}
}

// TestGateEndToEndUploadWhileMachiningSendsImmediately confirms
// UploadWhileMachining=true opens the gate as soon as connected, ignoring a
// Running status entirely.
func TestGateEndToEndUploadWhileMachiningSendsImmediately(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s := newConnTestSim(t, 1013)
	opts := schedTestOptions(1013, dir)
	opts.Config.Address = s.Addr().String()
	opts.Config.UploadWhileMachining = true
	opts.NewClient = newConnTestNewClient(freeAdapterPort())

	e, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	events, _ := runEngine(t, e)
	waitForEvent(t, events, isConnState(Connected))

	s.SetStatus(masso.Status{State: simStateRunning, Prompt: simPromptNormal})
	writeFile(t, dir, "M.NC", []byte("x"))
	waitForEvent(t, events, isTransferEvent("M.NC", Sent))
}

// TestConnSetSerialRestarts confirms SetSerial interrupts a stalled
// connection attempt (wrong serial configured) and reconnects against the
// newly configured one.
func TestConnSetSerialRestarts(t *testing.T) {
	t.Parallel()
	s := newConnTestSim(t, 555)
	port := freeAdapterPort()

	opts := connTestOptions(1) // wrong serial: the sim will never match
	opts.Config.Address = s.Addr().String()
	opts.NewClient = newConnTestNewClient(port)

	e, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	events, _ := runEngine(t, e)

	waitForEvent(t, events, isConnState(Discovering))

	e.SetSerial(555)
	ev := waitForEvent(t, events, isConnState(Connected))
	cs := asConnState(t, ev)
	if cs.Identity.Serial != 555 {
		t.Errorf("Connected Identity.Serial = %d, want 555", cs.Identity.Serial)
	}
}

// TestConnUnconfiguredStart confirms an engine with no serial configured
// reports Unconfigured and idles until SetSerial.
func TestConnUnconfiguredStart(t *testing.T) {
	t.Parallel()
	s := newConnTestSim(t, 666)
	port := freeAdapterPort()

	opts := connTestOptions(0)
	opts.Config.Serial = ""
	opts.Config.Address = s.Addr().String()
	opts.NewClient = newConnTestNewClient(port)

	e, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	events, _ := runEngine(t, e)

	waitForEvent(t, events, isConnState(Unconfigured))

	e.SetSerial(666)
	waitForEvent(t, events, isConnState(Connected))
}

// TestConnShutdownStopsCleanly confirms Run returns nil, and closes the
// client, on ctx cancellation.
func TestConnShutdownStopsCleanly(t *testing.T) {
	t.Parallel()
	s := newConnTestSim(t, 777)
	port := freeAdapterPort()

	opts := connTestOptions(777)
	opts.Config.Address = s.Addr().String()
	opts.NewClient = newConnTestNewClient(port)

	e, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	events, stop := runEngine(t, e)
	waitForEvent(t, events, isConnState(Connected))
	stop()
}
