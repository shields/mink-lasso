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
	"sync"
	"time"

	"msrl.dev/mink-lasso/internal/clock"
	"msrl.dev/mink-lasso/internal/masso"
)

// gate tracks whether an upload may currently start, per the machine-gate
// rule in the package doc: while UploadWhileMachining is false, an upload
// needs the machine to have been continuously neither Running nor
// WaitingForOperator for IdleHold, and — if the job stopped mid-progress,
// since feed hold and e-stop look like "stopped" on the wire — a further
// PauseGrace from the moment it went idle. It holds no reference to the
// scheduler; readyChan is the hook the scheduler stage watches to
// re-evaluate promptly instead of on a fixed poll.
//
// A single background goroutine (started by run) owns the one clock.Timer
// created in newGate: every setter only Stops or Resets that same Timer
// under the lock, so run's select never needs to track more than one
// channel and nothing leaks a goroutine per reschedule.
type gate struct {
	clk        clock.Clock
	idleHold   time.Duration
	pauseGrace time.Duration

	mu                   sync.Mutex
	isConnected          bool
	uploadWhileMachining bool
	haveStatus           bool
	status               masso.Status
	busy                 bool
	idleSince            time.Time
	pausedAt             time.Time
	timer                clock.Timer

	ready        chan struct{}
	lastSignaled Gate // last verdict reported via signalReadyLocked
}

// newGate returns a gate that starts disconnected ("Not connected").
func newGate(clk clock.Clock, idleHold, pauseGrace time.Duration, uploadWhileMachining bool) *gate {
	t := clk.NewTimer(time.Hour)
	t.Stop()
	return &gate{
		clk:                  clk,
		idleHold:             idleHold,
		pauseGrace:           pauseGrace,
		uploadWhileMachining: uploadWhileMachining,
		timer:                t,
		ready:                make(chan struct{}, 1),
	}
}

// run watches the gate's expiry timer until ctx is done. Every setter
// (connected, disconnected, setUploadWhileMachining, updateStatus) already
// signals readyChan itself via rescheduleLocked when its call opens the
// gate; run's only job is to do the same when the armed timer, rather than
// a setter, is what causes the gate to open.
func (g *gate) run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			g.mu.Lock()
			g.timer.Stop()
			g.mu.Unlock()
			return
		case <-g.timer.C():
			g.mu.Lock()
			g.rescheduleLocked()
			g.mu.Unlock()
		}
	}
}

// readyChan is signaled, non-blocking, whenever the gate's timer expires and
// the gate may have just opened. The scheduler stage calls current to get
// the actual verdict; this channel only means "look again".
func (g *gate) readyChan() <-chan struct{} { return g.ready }

// current returns the gate's verdict as of now, without changing any state.
func (g *gate) current() Gate {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.evaluateLocked(g.clk.Now())
}

// connected marks the gate connected: it becomes evaluable per status
// updates rather than reporting "Not connected".
func (g *gate) connected() Gate {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.isConnected = true
	return g.rescheduleLocked()
}

// disconnected marks the gate disconnected ("Not connected") and clears the
// machine's remembered status: it is stale the next time a connection is
// made.
func (g *gate) disconnected() Gate {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.isConnected = false
	g.haveStatus = false
	g.busy = false
	g.idleSince = time.Time{}
	g.pausedAt = time.Time{}
	return g.rescheduleLocked()
}

// setUploadWhileMachining updates the operator's override.
func (g *gate) setUploadWhileMachining(v bool) Gate {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.uploadWhileMachining = v
	return g.rescheduleLocked()
}

// updateStatus folds in a new status reply and returns the gate's verdict.
func (g *gate) updateStatus(s masso.Status) Gate {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.clk.Now()

	switch newBusy := s.Running || s.WaitingForOperator; {
	case newBusy:
		g.busy = true
		g.idleSince = time.Time{}
		g.pausedAt = time.Time{}
	case g.busy, !g.haveStatus:
		// Just went idle, or this is the very first status the gate has
		// ever observed (right after connected(), or after a
		// disconnected()-reset reconnect) and it already reports idle:
		// treat both the same way, per finding 3 — start the IdleHold
		// clock from now, not from a year-1 zero-value idleSince, and the
		// PauseGrace clock too if the job stopped mid-progress rather
		// than finishing.
		g.busy = false
		g.idleSince = now
		if s.Progress > 0 && s.Progress < 100 {
			g.pausedAt = now
		} else {
			g.pausedAt = time.Time{}
		}
	default:
		// Already idle: a fresh 0 or 100 clears a pending pause hold.
		if s.Progress == 0 || s.Progress == 100 {
			g.pausedAt = time.Time{}
		}
	}

	g.status = s
	g.haveStatus = true
	return g.rescheduleLocked()
}

// evaluateLocked computes the current verdict. Callers must hold g.mu.
func (g *gate) evaluateLocked(now time.Time) Gate {
	if !g.isConnected {
		return Gate{Reason: "Not connected"}
	}
	if g.uploadWhileMachining {
		return Gate{Open: true}
	}
	if !g.haveStatus || g.busy {
		if g.haveStatus && g.status.WaitingForOperator {
			return Gate{Reason: "Waiting for operator"}
		}
		return Gate{Reason: "Machining"}
	}
	if now.Sub(g.idleSince) < g.idleHold {
		return Gate{Reason: "Machine just stopped"}
	}
	if !g.pausedAt.IsZero() && now.Sub(g.pausedAt) < g.pauseGrace {
		return Gate{Reason: "Job paused"}
	}
	return Gate{Open: true}
}

// rescheduleLocked recomputes the verdict and arms (or disarms) the shared
// expiry timer so a gate that is closed only because of IdleHold/PauseGrace
// timing wakes run the moment it would open. Callers must hold g.mu.
func (g *gate) rescheduleLocked() Gate {
	now := g.clk.Now()
	verdict := g.evaluateLocked(now)
	g.signalReadyLocked(verdict)

	g.timer.Stop()
	if verdict.Open || !g.isConnected || g.uploadWhileMachining || !g.haveStatus || g.busy {
		return verdict
	}

	next := g.idleSince.Add(g.idleHold)
	if !g.pausedAt.IsZero() {
		if pn := g.pausedAt.Add(g.pauseGrace); pn.After(next) {
			next = pn
		}
	}
	if d := next.Sub(now); d > 0 {
		g.timer.Reset(d)
	}
	return verdict
}

// signalReadyLocked wakes the scheduler, non-blocking, whenever verdict
// differs from the last one reported this way — not only when it opens. A
// Reason-only change (e.g. Machining -> Waiting for operator, gate staying
// closed throughout) still needs to reach the Transfers table's per-item
// Waiting message, which only refreshes when the scheduler wakes and calls
// next() again; waking solely on the Open transition left that message
// stale until an unrelated poke happened to fire. Callers must hold g.mu.
func (g *gate) signalReadyLocked(verdict Gate) {
	if verdict == g.lastSignaled {
		return
	}
	g.lastSignaled = verdict
	select {
	case g.ready <- struct{}{}:
	default:
	}
}
