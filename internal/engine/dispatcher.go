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
	"sync"
	"time"
)

// dispatcher decouples every event producer inside the engine from the
// consumer reading Engine.Events: producers call emit, which never blocks,
// appending to an unbounded internal queue; a single goroutine drains that
// queue into the buffered-by-nothing but always-ready-to-send out channel.
// This is what lets the connection loop, the scheduler, and the watcher all
// report state changes without ever waiting on a slow or absent consumer.
type dispatcher struct {
	out  chan Event
	done chan struct{} // closed once run has returned and out is closed

	mu      sync.Mutex
	cond    *sync.Cond
	queue   []Event
	closed  bool
	waiting bool // true while run is blocked in cond.Wait, for tests to poll
}

// newDispatcher returns a dispatcher whose run goroutine has not yet been
// started; call run once, from Engine.Run, in its own goroutine.
func newDispatcher() *dispatcher {
	d := &dispatcher{
		out:  make(chan Event),
		done: make(chan struct{}),
	}
	d.cond = sync.NewCond(&d.mu)
	return d
}

// emit appends ev to the queue and returns immediately; it never blocks on
// the consumer. Calling emit after close is a no-op, since Engine.Run has
// already stopped every producer by the time it closes the dispatcher.
func (d *dispatcher) emit(ev Event) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return
	}
	d.queue = append(d.queue, ev)
	d.cond.Signal()
}

// run drains the queue into out until close is called, then closes out. It
// must run in its own goroutine, started once.
func (d *dispatcher) run() {
	defer close(d.done)
	defer close(d.out)
	for {
		d.mu.Lock()
		for len(d.queue) == 0 && !d.closed {
			d.waiting = true
			d.cond.Wait()
			d.waiting = false
		}
		if len(d.queue) == 0 && d.closed {
			d.mu.Unlock()
			return
		}
		ev := d.queue[0]
		d.queue = d.queue[1:]
		d.mu.Unlock()

		d.out <- ev
	}
}

// close stops accepting further events and lets run drain whatever remains
// in the queue before it closes out. It returns immediately; call wait to
// block until run has actually finished doing so.
func (d *dispatcher) close() {
	d.mu.Lock()
	d.closed = true
	d.cond.Broadcast()
	d.mu.Unlock()
}

// wait blocks until run's goroutine has drained the queue and closed out,
// so a caller (Engine.Run) can join it instead of returning while it is
// still alive. Call only after close, with a consumer still draining
// Events(): a caller that stops reading Events() before Run returns makes
// Run hang here, which is what lets a real consumer receive every event
// queued before shutdown, right up to the channel actually closing.
func (d *dispatcher) wait() {
	<-d.done
}

// isWaiting reports whether run is currently blocked in cond.Wait, letting
// a test poll deterministically for that state instead of sleeping a fixed
// duration.
func (d *dispatcher) isWaiting() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.waiting
}

// progressCoalescer decides, for one file's in-flight upload, whether a new
// Sending event is worth emitting: at most once per interval unless the
// percentage crossed a multiple of 5 or the transfer finished. It holds no
// clock of its own — the caller supplies "now" — so it stays trivially
// testable without a real or fake clock.
type progressCoalescer struct {
	interval   time.Duration
	lastAt     time.Time
	lastPct    int
	hasEmitted bool
}

// newProgressCoalescer returns a coalescer that always allows the very
// first call.
func newProgressCoalescer(interval time.Duration) *progressCoalescer {
	return &progressCoalescer{interval: interval, lastPct: -1}
}

// allow reports whether a Sending event for (sent, total) at now should be
// emitted, and if so records it as the new baseline.
func (p *progressCoalescer) allow(now time.Time, sent, total int64) bool {
	pct := percent(sent, total)
	if !p.hasEmitted || pct/5 != p.lastPct/5 || now.Sub(p.lastAt) >= p.interval {
		p.record(now, pct)
		return true
	}
	return false
}

// finish records the transfer's final (sent, total) at now and always
// reports true: a transfer's last progress call — success or failure — is
// never suppressed, regardless of timing.
func (p *progressCoalescer) finish(now time.Time, sent, total int64) bool {
	p.record(now, percent(sent, total))
	return true
}

func (p *progressCoalescer) record(now time.Time, pct int) {
	p.hasEmitted = true
	p.lastAt = now
	p.lastPct = pct
}

// percent returns sent as a percentage of total, 0 when total is 0 (an
// empty file's start-ACK progress callback), clamped to [0, 100].
func percent(sent, total int64) int {
	if total <= 0 {
		return 0
	}
	pct := int(sent * 100 / total)
	if pct < 0 {
		return 0
	}
	if pct > 100 {
		return 100
	}
	return pct
}
