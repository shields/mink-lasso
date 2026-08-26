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

package clock

import (
	"slices"
	"sync"
	"time"
)

// Fake is a Clock that only moves when Advance is called. Timers and tickers
// fire, in deadline order, as the clock passes their deadlines.
type Fake struct {
	mu      sync.Mutex
	cond    *sync.Cond
	now     time.Time
	pending []*fakeTimer
	waiters int
}

// NewFake returns a Fake clock reading start.
func NewFake(start time.Time) *Fake {
	f := &Fake{now: start}
	f.cond = sync.NewCond(&f.mu)
	return f
}

// Now implements Clock.
func (f *Fake) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

// Since implements Clock.
func (f *Fake) Since(t time.Time) time.Duration { return f.Now().Sub(t) }

// After implements Clock.
func (f *Fake) After(d time.Duration) <-chan time.Time { return f.NewTimer(d).C() }

// NewTimer implements Clock.
func (f *Fake) NewTimer(d time.Duration) Timer {
	f.mu.Lock()
	defer f.mu.Unlock()
	t := &fakeTimer{clock: f, c: make(chan time.Time, 1)}
	f.schedule(t, f.now.Add(d), 0)
	return t
}

// NewTicker implements Clock.
func (f *Fake) NewTicker(d time.Duration) Ticker {
	if d <= 0 {
		panic("clock: non-positive interval for NewTicker")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	t := &fakeTimer{clock: f, c: make(chan time.Time, 1)}
	f.schedule(t, f.now.Add(d), d)
	return fakeTicker{t}
}

// Advance moves the clock forward by d, firing every timer and ticker whose
// deadline is reached, in order.
func (f *Fake) Advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	target := f.now.Add(d)
	for {
		i := f.next(target)
		if i < 0 {
			break
		}
		t := f.pending[i]
		f.now = t.deadline
		select {
		case t.c <- f.now:
		default:
		}
		if t.period > 0 {
			t.deadline = t.deadline.Add(t.period)
		} else {
			// i already located t in f.pending (via next, above); delete it
			// directly instead of making remove redo the same scan.
			f.pending = slices.Delete(f.pending, i, i+1)
		}
	}
	f.now = target
}

// Pending reports how many timers and tickers are waiting to fire.
func (f *Fake) Pending() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.pending)
}

// BlockUntil returns once at least n timers and tickers are pending, which
// lets a test synchronize with a goroutine that is about to wait.
func (f *Fake) BlockUntil(n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for len(f.pending) < n {
		f.waiters++
		f.cond.Wait()
		f.waiters--
	}
}

// waiting reports how many BlockUntil callers are parked; tests use it to
// synchronize deterministically.
func (f *Fake) waiting() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.waiters
}

// next returns the index of the earliest pending timer with a deadline at or
// before target, or -1. Caller holds mu.
func (f *Fake) next(target time.Time) int {
	best := -1
	for i, t := range f.pending {
		if t.deadline.After(target) {
			continue
		}
		if best < 0 || t.deadline.Before(f.pending[best].deadline) {
			best = i
		}
	}
	return best
}

// Caller holds mu.
func (f *Fake) schedule(t *fakeTimer, deadline time.Time, period time.Duration) {
	t.deadline = deadline
	t.period = period
	f.pending = append(f.pending, t)
	f.cond.Broadcast()
}

// Caller holds mu. The return value reports whether t was pending (i.e.
// found in f.pending), which is the same fact a "was this timer active"
// field would hold, so remove is the single source of truth for it.
func (f *Fake) remove(t *fakeTimer) bool {
	i := slices.Index(f.pending, t)
	if i < 0 {
		return false
	}
	f.pending = slices.Delete(f.pending, i, i+1)
	return true
}

type fakeTimer struct {
	clock    *Fake
	c        chan time.Time
	deadline time.Time
	period   time.Duration
}

func (t *fakeTimer) C() <-chan time.Time { return t.c }

func (t *fakeTimer) Stop() bool {
	t.clock.mu.Lock()
	defer t.clock.mu.Unlock()
	return t.clock.remove(t)
}

func (t *fakeTimer) Reset(d time.Duration) bool {
	t.clock.mu.Lock()
	defer t.clock.mu.Unlock()
	wasActive := t.clock.remove(t)
	// Drain any value the timer already fired and left unread. Without
	// this, a Reset that lands after the timer fired but before its value
	// was read leaves the channel's buffer full: the newly scheduled fire
	// then silently loses its non-blocking send in Advance, and the timer
	// is never observed again. Draining here is what lets Timer mirror
	// *time.Timer's Go 1.23+ guarantee that a receive after Reset never
	// returns a stale value.
	select {
	case <-t.c:
	default:
	}
	t.clock.schedule(t, t.clock.now.Add(d), 0)
	return wasActive
}

type fakeTicker struct{ *fakeTimer }

func (t fakeTicker) Stop() { t.fakeTimer.Stop() }
