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
	"testing"
	"time"
)

// waitForWaiting polls until d.run has parked in cond.Wait (queue empty,
// not closed), so a test can be sure the empty-queue wait path is actually
// exercised before it emits — rather than sleeping a fixed duration and
// hoping the goroutine got there first.
func waitForWaiting(t *testing.T, d *dispatcher) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !d.isWaiting() {
		if time.Now().After(deadline) {
			t.Fatal("dispatcher never reached its empty-queue wait")
		}
		time.Sleep(time.Millisecond)
	}
}

// TestDispatcherOrdering checks that events come out in the order they went
// in, across a consumer that starts draining only after everything has
// already been queued.
func TestDispatcherOrdering(t *testing.T) {
	t.Parallel()
	d := newDispatcher()
	go d.run()
	waitForWaiting(t, d)

	want := []Event{
		WatchState{Dir: "a"},
		WatchState{Dir: "b"},
		WatchState{Dir: "c"},
	}
	for _, ev := range want {
		d.emit(ev)
	}
	d.close()

	var got []Event
	for ev := range d.out {
		got = append(got, ev)
	}
	if len(got) != len(want) {
		t.Fatalf("got %d events, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("event %d = %#v, want %#v", i, got[i], want[i])
		}
	}
}

// TestDispatcherNeverBlocksProducer emits far more events than any channel
// buffer could hold, with nothing draining Events() at all, and requires
// every emit call to return promptly — proving the queue, not a bounded
// channel, is what backs the producer side.
func TestDispatcherNeverBlocksProducer(t *testing.T) {
	t.Parallel()
	d := newDispatcher()
	go d.run()

	done := make(chan struct{})
	go func() {
		for range 10000 {
			d.emit(WatchState{})
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("emit blocked with no consumer draining Events()")
	}

	d.close()
	// Drain whatever was queued so run's goroutine exits cleanly.
	for ev := range d.out {
		_ = ev
	}
}

// TestDispatcherEmitAfterClose checks that a stray emit after close is a
// harmless no-op rather than a panic or a leaked goroutine.
func TestDispatcherEmitAfterClose(t *testing.T) {
	t.Parallel()
	d := newDispatcher()
	go d.run()
	d.close()
	d.emit(WatchState{}) // must not panic or block

	for range d.out {
		t.Fatal("expected no events after close")
	}
}

// TestDispatcherWaitJoinsRunGoroutine reproduces finding 1: close alone
// does not prove run's goroutine has actually finished draining the queue
// and closed out, only that it has been told to. wait must actually block
// until that has happened, with a consumer draining Events() throughout —
// the supported way to shut down — so a caller (Engine.Run) can join the
// goroutine instead of returning while it is still alive.
func TestDispatcherWaitJoinsRunGoroutine(t *testing.T) {
	t.Parallel()
	d := newDispatcher()
	go d.run()
	waitForWaiting(t, d)

	const n = 10000
	for range n {
		d.emit(WatchState{})
	}

	drained := make(chan int)
	go func() {
		count := 0
		for range d.out {
			count++
		}
		drained <- count
	}()

	joined := make(chan struct{})
	go func() {
		d.close()
		d.wait()
		close(joined)
	}()

	select {
	case <-joined:
	case <-time.After(5 * time.Second):
		t.Fatal("close+wait did not join run's goroutine with a consumer draining Events()")
	}

	if got := <-drained; got != n {
		t.Errorf("drained %d events, want %d", got, n)
	}

	// run's goroutine has now returned (wait proves it), so a further
	// receive must see out already closed, not block.
	select {
	case _, ok := <-d.out:
		if ok {
			t.Fatal("out produced a value after wait returned, want it closed")
		}
	default:
		t.Fatal("out not yet closed immediately after wait returned")
	}
}

func TestProgressCoalescer(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	interval := 100 * time.Millisecond
	p := newProgressCoalescer(interval)

	// First call always allowed.
	if !p.allow(base, 0, 100) {
		t.Fatal("first call should be allowed")
	}
	// Same percentage bucket, well within the interval: suppressed.
	if p.allow(base.Add(10*time.Millisecond), 1, 100) {
		t.Fatal("call within interval and percent bucket should be suppressed")
	}
	// Crossed a multiple-of-5 boundary: allowed even though the interval
	// has not elapsed.
	if !p.allow(base.Add(20*time.Millisecond), 5, 100) {
		t.Fatal("crossing a 5%% boundary should be allowed")
	}
	// Interval elapsed, same bucket: allowed.
	if !p.allow(base.Add(200*time.Millisecond), 6, 100) {
		t.Fatal("call after the interval elapses should be allowed")
	}
	// Neither boundary crossed nor interval elapsed, but finish is always
	// allowed.
	if !p.finish(base.Add(205*time.Millisecond), 6, 100) {
		t.Fatal("a finishing call should always be allowed")
	}

	// A different total confirms the percentage, not a hardcoded 100, is
	// what buckets are compared against.
	p2 := newProgressCoalescer(interval)
	if !p2.allow(base, 0, 4000) {
		t.Fatal("first call on a different total should be allowed")
	}
	if p2.allow(base.Add(10*time.Millisecond), 100, 4000) {
		t.Fatal("2%% of a 4000-byte file should stay in the same 5%% bucket")
	}
}

func TestPercent(t *testing.T) {
	t.Parallel()
	cases := []struct {
		sent, total int64
		want        int
	}{
		{0, 0, 0},
		{0, 100, 0},
		{50, 100, 50},
		{100, 100, 100},
		{150, 100, 100},
		{-1, 100, 0},
	}
	for _, c := range cases {
		if got := percent(c.sent, c.total); got != c.want {
			t.Errorf("percent(%d, %d) = %d, want %d", c.sent, c.total, got, c.want)
		}
	}
}
