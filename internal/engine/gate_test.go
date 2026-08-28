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
	"testing"
	"time"

	"msrl.dev/mink-lasso/internal/clock"
	"msrl.dev/mink-lasso/internal/masso"
)

const (
	testIdleHold   = 5 * time.Second
	testPauseGrace = 30 * time.Second
)

func newTestGate(clk clock.Clock) *gate {
	return newGate(clk, testIdleHold, testPauseGrace, false)
}

func TestGateDisconnected(t *testing.T) {
	t.Parallel()
	g := newTestGate(clock.NewFake(time.Now()))
	if got := g.current(); got.Open || got.Reason != "Not connected" {
		t.Errorf("current() = %+v, want closed Not connected", got)
	}
}

func TestGateRunningIsClosed(t *testing.T) {
	t.Parallel()
	g := newTestGate(clock.NewFake(time.Now()))
	g.connected()
	got := g.updateStatus(masso.Status{Running: true})
	if got.Open || got.Reason != "Machining" {
		t.Errorf("updateStatus(Running) = %+v, want closed Machining", got)
	}
}

func TestGateWaitingForOperatorIsClosed(t *testing.T) {
	t.Parallel()
	g := newTestGate(clock.NewFake(time.Now()))
	g.connected()
	got := g.updateStatus(masso.Status{WaitingForOperator: true})
	if got.Open || got.Reason != "Waiting for operator" {
		t.Errorf("updateStatus(WaitingForOperator) = %+v, want closed Waiting for operator", got)
	}
}

// TestGateIdleHold confirms the gate stays closed until IdleHold elapses
// after the machine goes idle, then opens without further input.
func TestGateIdleHold(t *testing.T) {
	t.Parallel()
	clk := clock.NewFake(time.Now())
	g := newTestGate(clk)
	g.connected()
	g.updateStatus(masso.Status{Running: true})

	got := g.updateStatus(masso.Status{Progress: 100})
	if got.Open || got.Reason != "Machine just stopped" {
		t.Fatalf("just went idle: %+v, want closed Machine just stopped", got)
	}

	clk.BlockUntil(1)
	clk.Advance(testIdleHold)
	if got := g.current(); !got.Open {
		t.Errorf("current() after IdleHold = %+v, want open", got)
	}
}

// TestGatePauseGrace covers the mid-progress idle case: the gate stays
// closed for PauseGrace even after IdleHold has passed, and a status
// showing Progress 100 releases it immediately (IdleHold has already run).
// TestGateFirstStatusMidProgressHeldForPauseGrace reproduces finding 3: the
// very first status the gate ever observes after connected() (or, per the
// same code path, after a Lost/reconnect cycle that reset haveStatus) may
// already report the machine idle. Per docs/protocol.md §4, feed hold and
// e-stop look identical to "stopped" on the wire, so a status that arrives
// already idle with 0 < Progress < 100 must still be held for PauseGrace
// measured from now — not opened immediately against a zero-value
// idleSince/pausedAt that was never armed because no busy→idle transition
// was ever observed in-process.
func TestGateFirstStatusMidProgressHeldForPauseGrace(t *testing.T) {
	t.Parallel()
	clk := clock.NewFake(time.Now())
	g := newTestGate(clk)
	g.connected()

	got := g.updateStatus(masso.Status{Progress: 50})
	if got.Open || got.Reason != "Machine just stopped" {
		t.Fatalf("first-ever status, mid-progress, before IdleHold: %+v, want closed Machine just stopped", got)
	}

	clk.BlockUntil(1)
	clk.Advance(testIdleHold)
	if got := g.current(); got.Open || got.Reason != "Job paused" {
		// Not opened immediately from a zero-value idleSince/pausedAt.
		t.Fatalf("after IdleHold, mid-progress: %+v, want closed Job paused", got)
	}

	clk.BlockUntil(1)
	clk.Advance(testPauseGrace - testIdleHold)
	if got := g.current(); !got.Open {
		t.Errorf("current() after PauseGrace elapsed = %+v, want open", got)
	}
}

// TestGateFirstStatusFinishedProgressStillHonorsIdleHold confirms the
// first-ever-status fix doesn't over-correct: a first status reporting
// Progress 100 (job actually finished, not paused) must still wait out
// IdleHold like any other idle transition, rather than opening
// immediately against a zero-value idleSince.
func TestGateFirstStatusFinishedProgressStillHonorsIdleHold(t *testing.T) {
	t.Parallel()
	clk := clock.NewFake(time.Now())
	g := newTestGate(clk)
	g.connected()

	got := g.updateStatus(masso.Status{Progress: 100})
	if got.Open || got.Reason != "Machine just stopped" {
		t.Fatalf("first-ever status, Progress 100: %+v, want closed Machine just stopped", got)
	}

	clk.BlockUntil(1)
	clk.Advance(testIdleHold)
	if got := g.current(); !got.Open {
		t.Errorf("current() after IdleHold elapsed = %+v, want open", got)
	}
}

func TestGatePauseGrace(t *testing.T) {
	t.Parallel()
	clk := clock.NewFake(time.Now())
	g := newTestGate(clk)
	g.connected()
	g.updateStatus(masso.Status{Running: true})
	g.updateStatus(masso.Status{Progress: 50})

	clk.BlockUntil(1)
	clk.Advance(testIdleHold)
	if got := g.current(); got.Open || got.Reason != "Job paused" {
		t.Fatalf("after IdleHold, mid-progress: %+v, want closed Job paused", got)
	}

	got := g.updateStatus(masso.Status{Progress: 100})
	if !got.Open {
		t.Errorf("updateStatus(Progress: 100) = %+v, want open (IdleHold already elapsed)", got)
	}
}

// TestGatePauseGraceExpires confirms PauseGrace on its own eventually opens
// the gate even without a fresh 0/100 status.
func TestGatePauseGraceExpires(t *testing.T) {
	t.Parallel()
	clk := clock.NewFake(time.Now())
	g := newTestGate(clk)
	g.connected()
	g.updateStatus(masso.Status{Running: true})
	g.updateStatus(masso.Status{Progress: 50})

	clk.BlockUntil(1)
	clk.Advance(testIdleHold)
	if got := g.current(); got.Open {
		t.Fatalf("current() after IdleHold only = %+v, want still closed", got)
	}

	clk.BlockUntil(1)
	clk.Advance(testPauseGrace - testIdleHold)
	if got := g.current(); !got.Open {
		t.Errorf("current() after PauseGrace = %+v, want open", got)
	}
}

// TestGateRunningAgainResetsHold confirms a fresh Running status, seen
// while still inside IdleHold, resets the clock rather than opening early.
func TestGateRunningAgainResetsHold(t *testing.T) {
	t.Parallel()
	clk := clock.NewFake(time.Now())
	g := newTestGate(clk)
	g.connected()
	g.updateStatus(masso.Status{Running: true})
	g.updateStatus(masso.Status{Progress: 100})

	clk.Advance(testIdleHold / 2)
	got := g.updateStatus(masso.Status{Running: true})
	if got.Open || got.Reason != "Machining" {
		t.Fatalf("updateStatus(Running) mid-hold = %+v, want closed Machining", got)
	}

	got = g.updateStatus(masso.Status{Progress: 100})
	if got.Open {
		t.Fatalf("updateStatus just after re-Running = %+v, want still closed", got)
	}
	clk.BlockUntil(1)
	clk.Advance(testIdleHold)
	if got := g.current(); !got.Open {
		t.Errorf("current() after fresh IdleHold = %+v, want open", got)
	}
}

func TestGateUploadWhileMachining(t *testing.T) {
	t.Parallel()
	clk := clock.NewFake(time.Now())
	g := newGate(clk, testIdleHold, testPauseGrace, true)
	g.connected()
	got := g.updateStatus(masso.Status{Running: true})
	if !got.Open {
		t.Errorf("updateStatus(Running) with UploadWhileMachining = %+v, want open", got)
	}
}

func TestGateSetUploadWhileMachiningLive(t *testing.T) {
	t.Parallel()
	g := newTestGate(clock.NewFake(time.Now()))
	g.connected()
	g.updateStatus(masso.Status{Running: true})
	if got := g.setUploadWhileMachining(true); !got.Open {
		t.Errorf("setUploadWhileMachining(true) = %+v, want open", got)
	}
	if got := g.setUploadWhileMachining(false); got.Open {
		t.Errorf("setUploadWhileMachining(false) = %+v, want closed again", got)
	}
}

func TestGateDisconnectClosesAndClearsStatus(t *testing.T) {
	t.Parallel()
	clk := clock.NewFake(time.Now())
	g := newTestGate(clk)
	g.connected()
	g.updateStatus(masso.Status{Progress: 100})
	clk.Advance(testIdleHold)
	if got := g.current(); !got.Open {
		t.Fatalf("current() before disconnect = %+v, want open", got)
	}

	got := g.disconnected()
	if got.Open || got.Reason != "Not connected" {
		t.Fatalf("disconnected() = %+v, want closed Not connected", got)
	}

	// Reconnecting with no status yet must not still read as open from
	// stale idle/pause bookkeeping.
	got = g.connected()
	if got.Open {
		t.Errorf("connected() with no status yet = %+v, want closed", got)
	}
}

// TestGateRunSignalsReady drives the gate's background timer goroutine
// end-to-end: it must signal readyChan once the timer it armed fires.
func TestGateRunSignalsReady(t *testing.T) {
	t.Parallel()
	clk := clock.NewFake(time.Now())
	g := newTestGate(clk)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go g.run(ctx)

	g.connected()
	g.updateStatus(masso.Status{Running: true})
	g.updateStatus(masso.Status{Progress: 100})

	clk.BlockUntil(1)
	clk.Advance(testIdleHold)

	select {
	case <-g.readyChan():
	case <-time.After(5 * time.Second):
		t.Fatal("readyChan was not signaled after IdleHold elapsed")
	}
	if got := g.current(); !got.Open {
		t.Errorf("current() after ready signal = %+v, want open", got)
	}
}
