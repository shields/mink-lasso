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
	"runtime"
	"testing"
	"time"
)

func TestReal(t *testing.T) {
	t.Parallel()
	var c Clock = Real{}
	start := c.Now()
	if c.Since(start) < 0 {
		t.Fatal("Since went backwards")
	}
	<-c.After(time.Millisecond)
	tm := c.NewTimer(time.Millisecond)
	<-tm.C()
	if tm.Stop() {
		t.Fatal("Stop on a fired timer should report false")
	}
	if tm.Reset(time.Hour) {
		t.Fatal("Reset on a stopped timer should report false")
	}
	tm.Stop()
	tk := c.NewTicker(time.Millisecond)
	<-tk.C()
	tk.Stop()
}

func TestFakeTimersFireInOrder(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	f := NewFake(start)
	late := f.NewTimer(3 * time.Second)
	early := f.NewTimer(time.Second)
	after := f.After(2 * time.Second)
	if f.Pending() != 3 {
		t.Fatalf("Pending = %d, want 3", f.Pending())
	}
	f.Advance(2500 * time.Millisecond)
	if got := <-early.C(); !got.Equal(start.Add(time.Second)) {
		t.Fatalf("early fired at %v", got)
	}
	if got := <-after; !got.Equal(start.Add(2 * time.Second)) {
		t.Fatalf("after fired at %v", got)
	}
	select {
	case <-late.C():
		t.Fatal("late timer fired early")
	default:
	}
	if !f.Now().Equal(start.Add(2500 * time.Millisecond)) {
		t.Fatalf("Now = %v", f.Now())
	}
	if f.Since(start) != 2500*time.Millisecond {
		t.Fatalf("Since = %v", f.Since(start))
	}
	if f.Pending() != 1 {
		t.Fatalf("Pending = %d, want 1", f.Pending())
	}
	f.Advance(time.Second)
	if got := <-late.C(); !got.Equal(start.Add(3 * time.Second)) {
		t.Fatalf("late fired at %v", got)
	}
	if f.Pending() != 0 {
		t.Fatalf("Pending = %d, want 0", f.Pending())
	}
}

func TestFakeStopAndReset(t *testing.T) {
	t.Parallel()
	f := NewFake(time.Unix(0, 0))
	tm := f.NewTimer(time.Second)
	if !tm.Stop() {
		t.Fatal("Stop on an active timer should report true")
	}
	if tm.Stop() {
		t.Fatal("second Stop should report false")
	}
	f.Advance(time.Second)
	select {
	case <-tm.C():
		t.Fatal("stopped timer fired")
	default:
	}
	if tm.Reset(time.Second) {
		t.Fatal("Reset on a stopped timer should report false")
	}
	if !tm.Reset(2 * time.Second) {
		t.Fatal("Reset on an active timer should report true")
	}
	f.Advance(time.Second)
	select {
	case <-tm.C():
		t.Fatal("reset timer fired early")
	default:
	}
	f.Advance(time.Second)
	if got := <-tm.C(); !got.Equal(time.Unix(3, 0)) {
		t.Fatalf("fired at %v", got)
	}
}

// TestFakeResetDrainsUnreadFire checks that Reset, called after a timer has
// already fired but before its value was read, leaves the channel ready to
// deliver only the new deadline's value — mirroring the guarantee Go 1.23+
// documents for a real *time.Timer: no receive after Reset returns a stale
// value, and the newly scheduled fire is not lost.
func TestFakeResetDrainsUnreadFire(t *testing.T) {
	t.Parallel()
	f := NewFake(time.Unix(0, 0))
	tm := f.NewTimer(time.Second)

	f.Advance(time.Second) // fires; tm.C() now holds Unix(1,0), unread
	if f.Pending() != 0 {
		t.Fatalf("Pending = %d, want 0 (timer already fired)", f.Pending())
	}

	if tm.Reset(time.Second) {
		t.Fatal("Reset on an already-fired, undrained timer should report false")
	}

	// Without draining, the stale Unix(1,0) value would still be sitting in
	// the channel here, and the Advance below would silently lose its
	// send (buffer already full) instead of ever making tm.C() ready
	// again.
	select {
	case v := <-tm.C():
		t.Fatalf("Reset left a stale value ready: %v", v)
	default:
	}

	f.Advance(time.Second)
	if got := <-tm.C(); !got.Equal(time.Unix(2, 0)) {
		t.Fatalf("fired at %v, want the fresh Unix(2,0) deadline", got)
	}
}

func TestFakeTicker(t *testing.T) {
	t.Parallel()
	f := NewFake(time.Unix(0, 0))
	tk := f.NewTicker(time.Second)
	f.Advance(time.Second)
	if got := <-tk.C(); !got.Equal(time.Unix(1, 0)) {
		t.Fatalf("first tick at %v", got)
	}
	// Ticks that nobody drains are coalesced, like time.Ticker.
	f.Advance(5 * time.Second)
	if got := <-tk.C(); !got.Equal(time.Unix(2, 0)) {
		t.Fatalf("second tick at %v", got)
	}
	select {
	case <-tk.C():
		t.Fatal("ticker channel should hold at most one tick")
	default:
	}
	tk.Stop()
	f.Advance(time.Second)
	select {
	case <-tk.C():
		t.Fatal("stopped ticker fired")
	default:
	}
	defer func() {
		if recover() == nil {
			t.Fatal("NewTicker(0) should panic")
		}
	}()
	f.NewTicker(0)
}

func TestFakeBlockUntil(t *testing.T) {
	t.Parallel()
	f := NewFake(time.Unix(0, 0))
	done := make(chan struct{})
	go func() {
		f.BlockUntil(1)
		close(done)
	}()
	for f.waiting() == 0 {
		runtime.Gosched()
	}
	f.NewTimer(time.Second)
	<-done
	if f.waiting() != 0 {
		t.Fatal("waiter did not leave")
	}
	f.BlockUntil(0)
}
