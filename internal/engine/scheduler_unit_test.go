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

// This file unit-tests scheduler helpers directly (no real socket, no real
// watcher) for branches that a full end-to-end scenario cannot reach
// deterministically: openForSend's own error paths, preflightFailed, and
// retry/nextDeadline's edge cases.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"msrl.dev/mink-lasso/internal/clock"
	"msrl.dev/mink-lasso/internal/config"
	"msrl.dev/mink-lasso/internal/masso"
	"msrl.dev/mink-lasso/internal/watch"
)

// newUnitTestEngine returns an Engine with a fakeClient (no real socket)
// and the given Open override, ready for scheduler unit tests that never
// call Run.
func newUnitTestEngine(t *testing.T, open func(string) (*os.File, error)) *Engine {
	t.Helper()
	e, err := New(Options{
		Config:    config.Config{},
		Clock:     clock.NewFake(time.Now()),
		NewClient: func(masso.Options) (Client, error) { return fakeClient{}, nil },
		Open:      open,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return e
}

// TestReadyClearsManualFromPreviousSendFile reproduces finding 2: Manual is
// documented (event.go) to describe only the one SendFile call that set
// it, not the filename from then on. A completed manual send left
// it.manual true; a later, completely ordinary watcher Ready/Changed event
// for that same name — not a SendFile call — must return the item to
// non-manual, or every future automatic upload of that name would be
// silently skipped by archiveSent's manual check and misreported as
// Manual: true.
func TestReadyClearsManualFromPreviousSendFile(t *testing.T) {
	t.Parallel()
	e := newUnitTestEngine(t, nil)
	e.scheduler.items["A.NC"] = &item{name: "A.NC", state: Sent, manual: true}

	e.scheduler.ready("/watch", watch.File{Name: "A.NC", Path: "/watch/A.NC", Size: 1})

	e.scheduler.mu.Lock()
	it := e.scheduler.items["A.NC"]
	state, manual := it.state, it.manual
	e.scheduler.mu.Unlock()
	if state != Pending {
		t.Errorf("state after plain ready() = %v, want Pending", state)
	}
	if manual {
		t.Error("manual still true after an ordinary watcher Ready event, want it cleared")
	}
}

// TestReadyDuringSendPreservesManualForResend confirms the fix for finding
// 2 does not disturb the in-flight resend chain: a Changed arriving while
// a manual SendFile's own upload is still Sending must keep manual true
// through to that resend's own completion (the sending branch of ready,
// which the fix does not touch), not clear it prematurely mid-transfer.
func TestReadyDuringSendPreservesManualForResend(t *testing.T) {
	t.Parallel()
	e := newUnitTestEngine(t, nil)
	e.scheduler.items["A.NC"] = &item{name: "A.NC", state: Sending, sending: true, manual: true}

	e.scheduler.ready("/watch", watch.File{Name: "A.NC", Path: "/watch/A.NC", Size: 2})

	e.scheduler.mu.Lock()
	it := e.scheduler.items["A.NC"]
	resendAfter, manual := it.resendAfter, it.manual
	e.scheduler.mu.Unlock()
	if !resendAfter {
		t.Error("resendAfter not set by ready() on a Sending item")
	}
	if !manual {
		t.Error("manual cleared mid-transfer, want it preserved until the in-flight send settles")
	}
}

func TestReadyLeavesManualItemAloneWhenUnchanged(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		state   TransferState
		sending bool
	}{
		{"sent", Sent, false},
		{"queued", Pending, false},
		{"waiting on the gate", Waiting, false},
		{"in flight", Sending, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := newUnitTestEngine(t, nil)
			now := e.opts.Clock.Now()
			it := &item{
				name: "A.NC", state: tc.state, sending: tc.sending, manual: true,
				path: "/watch/A.NC", size: 5, modTime: now,
			}
			e.scheduler.items["A.NC"] = it

			e.scheduler.ready("/watch", watch.File{Name: "A.NC", Path: "/watch/A.NC", Size: 5, ModTime: now})

			e.scheduler.mu.Lock()
			defer e.scheduler.mu.Unlock()
			if it.state != tc.state || !it.manual || it.resendAfter {
				t.Errorf("item = (state %v, manual %v, resendAfter %v), want (%v, true, false)",
					it.state, it.manual, it.resendAfter, tc.state)
			}
			if evs := takeQueued(t, e); len(evs) != 0 {
				t.Errorf("events = %+v, want none", evs)
			}
		})
	}
}

func TestReadyRearmsSentManualItemWhenChanged(t *testing.T) {
	t.Parallel()
	e := newUnitTestEngine(t, nil)
	now := e.opts.Clock.Now()
	it := &item{name: "A.NC", state: Sent, manual: true, path: "/watch/A.NC", size: 5, modTime: now}
	e.scheduler.items["A.NC"] = it

	e.scheduler.ready("/watch", watch.File{Name: "A.NC", Path: "/watch/A.NC", Size: 6, ModTime: now})

	e.scheduler.mu.Lock()
	defer e.scheduler.mu.Unlock()
	if it.state != Pending {
		t.Errorf("state = %v, want Pending (content actually changed)", it.state)
	}
	if it.manual {
		t.Error("manual still set after an ordinary Changed re-emission, want it cleared")
	}
}

func TestSendFileDuringSendSameContentSkipsResend(t *testing.T) {
	t.Parallel()
	e := newUnitTestEngine(t, nil)
	now := e.opts.Clock.Now()
	it := &item{name: "A.NC", state: Sending, sending: true, path: "/watch/A.NC", size: 5, modTime: now}
	e.scheduler.items["A.NC"] = it

	e.scheduler.sendFile("", "", "A.NC", "/watch/A.NC", 5, now)

	e.scheduler.mu.Lock()
	defer e.scheduler.mu.Unlock()
	if it.resendAfter {
		t.Error("resendAfter set for a manual send matching the in-flight item's own content")
	}
	if !it.manual {
		t.Error("manual not set by sendFile")
	}
}

func TestSendFileDuringSendChangedContentQueuesResend(t *testing.T) {
	t.Parallel()
	e := newUnitTestEngine(t, nil)
	now := e.opts.Clock.Now()
	it := &item{name: "A.NC", state: Sending, sending: true, path: "/watch/A.NC", size: 5, modTime: now}
	e.scheduler.items["A.NC"] = it

	e.scheduler.sendFile("", "", "A.NC", "/watch/A.NC", 6, now)

	e.scheduler.mu.Lock()
	defer e.scheduler.mu.Unlock()
	if !it.resendAfter {
		t.Error("resendAfter not set for a manual send with changed content")
	}
	if !it.manual {
		t.Error("manual not set by sendFile")
	}
}

func TestOpenForSendBadName(t *testing.T) {
	t.Parallel()
	e := newUnitTestEngine(t, nil)
	const name = "this-name-is-way-too-long-for-masso.nc"
	it := &item{name: name, base: name, path: "/dev/null"}
	if _, _, err := e.scheduler.openForSend(it); !errors.Is(err, masso.ErrBadFileName) {
		t.Errorf("openForSend error = %v, want ErrBadFileName", err)
	}
}

func TestOpenForSendOpenError(t *testing.T) {
	t.Parallel()
	wantErr := errors.New("simulated open failure")
	e := newUnitTestEngine(t, func(string) (*os.File, error) { return nil, wantErr })
	it := &item{name: "A.NC", base: "A.NC", path: "/does/not/matter"}
	if _, _, err := e.scheduler.openForSend(it); !errors.Is(err, wantErr) {
		t.Errorf("openForSend error = %v, want %v", err, wantErr)
	}
}

func TestOpenForSendStatError(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "A.NC")
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	// A file already closed by the time openForSend calls Stat on it
	// fails exactly like an unreadable one would.
	if err := f.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	e := newUnitTestEngine(t, func(string) (*os.File, error) { return f, nil })
	it := &item{name: "A.NC", base: "A.NC", path: path}
	if _, _, err := e.scheduler.openForSend(it); err == nil {
		t.Error("openForSend error = nil, want a Stat error on an already-closed file")
	}
}

func TestPreflightFailedVanishedForgetsItem(t *testing.T) {
	t.Parallel()
	e := newUnitTestEngine(t, nil)
	it := &item{name: "A.NC", path: "/does/not/exist/A.NC", state: Pending}
	e.scheduler.items["A.NC"] = it
	e.scheduler.preflightFailed(it, fs.ErrNotExist)

	e.scheduler.mu.Lock()
	_, ok := e.scheduler.items["A.NC"]
	e.scheduler.mu.Unlock()
	if ok {
		t.Error("item still in queue after a vanished-file preflight failure, want forgotten")
	}
}

func TestPreflightFailedBusyStaysPendingWithBackoff(t *testing.T) {
	t.Parallel()
	e := newUnitTestEngine(t, nil)
	it := &item{name: "A.NC", path: "/tmp/A.NC", state: Pending, resendAfter: true}
	e.scheduler.items["A.NC"] = it
	e.scheduler.preflightFailed(it, errors.New("busy"))

	e.scheduler.mu.Lock()
	defer e.scheduler.mu.Unlock()
	if it.state != Pending {
		t.Errorf("state = %v, want Pending", it.state)
	}
	if it.nextAttempt.IsZero() {
		t.Error("nextAttempt not set, want Backoff[0] from now")
	}
	if it.resendAfter {
		t.Error("resendAfter still set after preflight failure, want cleared")
	}
}

// TestPreflightFailedOrphanedDropsItem confirms an orphaned item (one
// clearNonManual could not delete outright because it was mid-open) is
// dropped on any preflight failure, not just fs.ErrNotExist, and not
// resurrected as a live Pending candidate pointing at a path the watch
// folder already moved away from.
func TestPreflightFailedOrphanedDropsItem(t *testing.T) {
	t.Parallel()
	e := newUnitTestEngine(t, nil)
	it := &item{name: "A.NC", path: "/tmp/A.NC", state: Pending, orphaned: true}
	e.scheduler.items["A.NC"] = it
	e.scheduler.preflightFailed(it, errors.New("busy"))

	e.scheduler.mu.Lock()
	_, ok := e.scheduler.items["A.NC"]
	e.scheduler.mu.Unlock()
	if ok {
		t.Error("orphaned item still in queue after preflight failure, want dropped")
	}
}

func TestRetryUnknownNameIgnored(t *testing.T) {
	t.Parallel()
	e := newUnitTestEngine(t, nil)
	e.scheduler.retry("nope") // must not panic or emit
}

// TestRetryRejectedItem confirms retry() treats a Rejected item as
// retryable, just like Failed and SentUnfiled — the three-way OR guard in
// retry's condition makes it easy for a future edit to drop one arm
// (Rejected in particular had no dedicated test) without any test in the
// package noticing, since the following statements are identical for all
// three states.
func TestRetryRejectedItem(t *testing.T) {
	t.Parallel()
	e := newUnitTestEngine(t, nil)
	it := &item{name: "A.NC", state: Rejected, message: "bad name"}
	e.scheduler.items["A.NC"] = it
	e.scheduler.retry("A.NC")

	e.scheduler.mu.Lock()
	defer e.scheduler.mu.Unlock()
	if it.state != Pending {
		t.Errorf("state = %v, want Pending", it.state)
	}
	if it.message != msgWaitingToSend {
		t.Errorf("message = %q, want %q", it.message, msgWaitingToSend)
	}
}

func TestRetryNonRetryableStateIgnored(t *testing.T) {
	t.Parallel()
	e := newUnitTestEngine(t, nil)
	it := &item{name: "A.NC", state: Pending}
	e.scheduler.items["A.NC"] = it
	e.scheduler.retry("A.NC")

	e.scheduler.mu.Lock()
	defer e.scheduler.mu.Unlock()
	if it.state != Pending {
		t.Errorf("state = %v, want unchanged Pending (retry is a no-op on a non-retryable state)", it.state)
	}
}

// TestNextOrdersManualFirstThenModTimeThenName exercises next()'s sort
// comparator directly: a manual item always wins regardless of modTime,
// and among non-manual items the older modTime (then the lexically first
// name, on a tie) wins.
func TestNextOrdersManualFirstThenModTimeThenName(t *testing.T) {
	t.Parallel()
	e := newUnitTestEngine(t, nil)
	e.gate.connected()
	e.gate.setUploadWhileMachining(true)

	now := e.opts.Clock.Now()
	e.scheduler.items["OLD.NC"] = &item{name: "OLD.NC", state: Pending, modTime: now}
	e.scheduler.items["NEW.NC"] = &item{name: "NEW.NC", state: Pending, modTime: now.Add(time.Second)}
	e.scheduler.items["MANUAL.NC"] = &item{name: "MANUAL.NC", state: Pending, manual: true, modTime: now.Add(time.Hour)}
	// A same-modTime tie with OLD.NC, to exercise the name tie-break too.
	e.scheduler.items["AAA.NC"] = &item{name: "AAA.NC", state: Pending, modTime: now}

	it, ok := e.scheduler.next()
	if !ok || it.name != "MANUAL.NC" {
		t.Fatalf("next() = %v, %v; want MANUAL.NC (manual always first)", it, ok)
	}
	// Simulate what sendOne would do next — move it out of Pending — so
	// the next next() call has to choose among what remains.
	it.state = Sending

	it, ok = e.scheduler.next()
	if !ok || it.name != "AAA.NC" {
		t.Fatalf("next() = %v, %v; want AAA.NC (tied modTime, lexically first name)", it, ok)
	}
	it.state = Sending

	it, ok = e.scheduler.next()
	if !ok || it.name != "OLD.NC" {
		t.Fatalf("next() = %v, %v; want OLD.NC (earlier modTime than NEW.NC)", it, ok)
	}
}

// TestNextSkipsCanceledFailureWithNoAutoRetry confirms a Failed item left
// with a zero nextAttempt (ErrCanceled's no-auto-retry marker) is never
// picked up by next() on its own — only Retry or a Changed re-emission
// makes it eligible again. End-to-end scenarios only reach this branch by
// real-clock luck (whether next() happens to run again while such an item
// still sits in the queue), which made it a source of flaky coverage under
// -race.
func TestNextSkipsCanceledFailureWithNoAutoRetry(t *testing.T) {
	t.Parallel()
	e := newUnitTestEngine(t, nil)
	e.gate.connected()
	e.gate.setUploadWhileMachining(true)

	e.scheduler.items["CANCELED.NC"] = &item{name: "CANCELED.NC", state: Failed}

	if it, ok := e.scheduler.next(); ok {
		t.Fatalf("next() = %v, %v; want none (Failed with zero nextAttempt has no auto-retry)", it, ok)
	}
}

// TestNextGateClosedMovesPendingItemToWaiting confirms next() reports a
// Pending item as Waiting with the gate's reason the first time it sees the
// gate closed. End-to-end scenarios only reach this transition by
// real-clock luck (whether next() happens to run while the item still sits
// Pending), which made it a source of flaky coverage under -race.
func TestNextGateClosedMovesPendingItemToWaiting(t *testing.T) {
	t.Parallel()
	e := newUnitTestEngine(t, nil)
	e.gate.disconnected()

	it := &item{name: "A.NC", state: Pending}
	e.scheduler.items["A.NC"] = it

	if _, ok := e.scheduler.next(); ok {
		t.Fatal("next() returned an item while the gate is closed")
	}
	if it.state != Waiting || it.message != "Not connected" {
		t.Fatalf("item = %v %q, want Waiting %q", it.state, it.message, "Not connected")
	}
}

// TestNextGateClosedSkipsReemitForAlreadyWaitingItem confirms next()
// leaves an item alone, rather than re-emitting a TransferEvent, when it is
// already Waiting with the current gate reason. End-to-end scenarios only
// reach this branch by real-clock luck (whether next() happens to run
// again before the gate reason next changes), which made it a source of
// flaky coverage under -race.
func TestNextGateClosedSkipsReemitForAlreadyWaitingItem(t *testing.T) {
	t.Parallel()
	e := newUnitTestEngine(t, nil)
	e.gate.disconnected()

	it := &item{name: "A.NC", state: Waiting, message: "Not connected"}
	e.scheduler.items["A.NC"] = it

	if _, ok := e.scheduler.next(); ok {
		t.Fatal("next() returned an item while the gate is closed")
	}
	if it.state != Waiting || it.message != "Not connected" {
		t.Fatalf("item mutated unexpectedly: state=%v message=%q", it.state, it.message)
	}
}

// TestCompareItemsManualTieBreak confirms compareItems orders a manual item
// before a non-manual one regardless of which side of the call it's on.
// next()'s own sort exercises this indirectly through slices.SortFunc over
// candidates built from randomized map iteration order, which does not
// reliably call the comparator in both argument orders on every run —
// hence testing the named comparator directly here.
func TestCompareItemsManualTieBreak(t *testing.T) {
	t.Parallel()
	manual := &item{name: "M.NC", manual: true}
	other := &item{name: "O.NC"}

	if got := compareItems(manual, other); got >= 0 {
		t.Errorf("compareItems(manual, other) = %d, want < 0", got)
	}
	if got := compareItems(other, manual); got <= 0 {
		t.Errorf("compareItems(other, manual) = %d, want > 0", got)
	}
}

// closeDuringUploadClient is a Client whose Upload closes the file out
// from under sendOne before returning success, so sendOne's own deferred
// Close call — which real filesystem behavior can't otherwise be made to
// fail deterministically — sees an already-closed file and logs it.
type closeDuringUploadClient struct{ fakeClient }

func (closeDuringUploadClient) Upload(
	_ context.Context, _, _ string, r io.ReaderAt, _ int64, _ func(int64, int64),
) error {
	if f, ok := r.(*os.File); ok {
		_ = f.Close()
	}
	return nil
}

func TestSendOneCloseAfterSendLogsError(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "A.NC")
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	e := newUnitTestEngine(t, os.Open)
	e.client = closeDuringUploadClient{}
	it := &item{name: "A.NC", root: dir, base: "A.NC", path: path, state: Pending}
	e.scheduler.items["A.NC"] = it

	// sendOne must not panic despite Upload closing the file first; the
	// only observable effect is the Warn log from the deferred Close.
	e.scheduler.sendOne(context.Background(), it)
}

func TestSendOnePreflightFailure(t *testing.T) {
	t.Parallel()
	e := newUnitTestEngine(t, func(string) (*os.File, error) { return nil, fs.ErrNotExist })
	it := &item{name: "A.NC", base: "A.NC", path: "/does/not/exist", state: Pending}
	e.scheduler.items["A.NC"] = it

	e.scheduler.sendOne(context.Background(), it)

	e.scheduler.mu.Lock()
	defer e.scheduler.mu.Unlock()
	if _, ok := e.scheduler.items["A.NC"]; ok {
		t.Error("item still queued after sendOne's preflight vanished-file failure, want forgotten")
	}
}

// TestSendOneClosesFileBeforeArchiving confirms sendOne closes its read
// handle on the source file before finishSend/archiveSent can rename it
// (finding 1): on Windows, opts.Open's real implementation
// (winutil.OpenDenyWrite) holds a share-mode handle that blocks a rename of
// the same path for as long as it stays open, so the archive step must
// never observe the handle as still open.
func TestSendOneClosesFileBeforeArchiving(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "A.NC")
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	var opened *os.File
	e := newUnitTestEngine(t, func(p string) (*os.File, error) {
		f, err := os.Open(p)
		opened = f
		return f, err
	})
	renamed := false
	e.opts.Rename = func(oldpath, newpath string) error {
		if opened == nil {
			t.Fatal("Rename called before Open")
		}
		if _, err := opened.Stat(); err == nil {
			t.Error("Rename observed the source file still open")
		}
		renamed = true
		return os.Rename(oldpath, newpath)
	}

	it := &item{name: "A.NC", root: dir, base: "A.NC", path: path, state: Pending}
	e.scheduler.items["A.NC"] = it
	e.scheduler.sendOne(context.Background(), it)

	if !renamed {
		t.Fatal("Rename was never called; test did not exercise the archive step")
	}
	e.scheduler.mu.Lock()
	defer e.scheduler.mu.Unlock()
	if it.state != Sent {
		t.Errorf("state = %v, want Sent", it.state)
	}
}

func TestFailureMessage(t *testing.T) {
	t.Parallel()
	other := errors.New("some other transfer error")
	cases := []struct {
		name  string
		err   error
		acked bool
		want  string
	}{
		{"no usb", masso.ErrNoUSB, false, "No USB flash drive connected to Masso"},
		{
			"no usb acked", masso.ErrNoUSB, true,
			"No USB flash drive connected to Masso — the file on the Masso may be incomplete; it will be resent",
		},
		{"no response", masso.ErrNoResponse, false, "ERROR: No response from Masso"},
		{"canceled", masso.ErrCanceled, false, "File transfer canceled by user on Masso"},
		{
			"canceled acked", masso.ErrCanceled, true,
			"File transfer canceled by user on Masso; resend it before running it",
		},
		{"usb write", masso.ErrUSBWrite, false, "Unable to write file to USB"},
		{
			"usb write acked", masso.ErrUSBWrite, true,
			"Unable to write file to USB — the file on the Masso may be incomplete; it will be resent",
		},
		{"transfer", masso.ErrTransfer, false, "Error occurred while transferring file"},
		{
			"wrapped", fmt.Errorf("upload: %w", masso.ErrNoUSB), false,
			"No USB flash drive connected to Masso",
		},
		{"other", other, false, other.Error()},
		{"other acked", other, true, other.Error() + " — the file on the Masso may be incomplete; it will be resent"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			wording := failureMessage(c.err)
			got := wording.base
			if c.acked {
				got = wording.acked
			}
			if got != c.want {
				t.Errorf("failureMessage(%v) [acked=%v] = %q, want %q", c.err, c.acked, got, c.want)
			}
		})
	}
}

// TestClearNonManualPreservesSendingItemThenDropsQueuedResend confirms
// clearNonManual does not delete an in-flight Sending item out from under
// the running send — the item stays in the queue, marked orphaned — but
// once that transfer finishes, a resend queued for it is never resurrected
// as a live Pending candidate pointing at the stale watch dir SetWatchDir
// already moved away from (finding 4): finishSend drops the orphaned item
// outright instead, exactly as it already does for the non-resend case.
func TestClearNonManualPreservesSendingItemThenDropsQueuedResend(t *testing.T) {
	t.Parallel()
	e := newUnitTestEngine(t, nil)

	it := &item{name: "F.NC", path: "/does/not/matter", state: Sending, sending: true}
	e.scheduler.items["F.NC"] = it

	// Simulate the watcher's Changed re-emission arriving while the send
	// is in flight: ready() sets resendAfter because it.sending is true.
	e.scheduler.ready("/watch", watch.File{Name: "F.NC", Path: "/does/not/matter", Size: 2})
	if !it.resendAfter {
		t.Fatal("resendAfter not set by ready() on a Sending item")
	}

	e.scheduler.clearNonManual()

	e.scheduler.mu.Lock()
	_, stillThere := e.scheduler.items["F.NC"]
	orphaned := it.orphaned
	e.scheduler.mu.Unlock()
	if !stillThere {
		t.Fatal("clearNonManual deleted a Sending item, losing its queued resend")
	}
	if !orphaned {
		t.Error("item not marked orphaned by clearNonManual")
	}

	// The in-flight send completes: finishSend must drop the orphaned item
	// rather than re-arm it to Pending for the stale resend, since the
	// folder it points at is no longer the configured watch dir.
	s := e.scheduler
	s.finishSend(context.Background(), it, sendSource{}, nil, "")

	s.mu.Lock()
	_, stillThere = s.items["F.NC"]
	s.mu.Unlock()
	if stillThere {
		t.Error("orphaned item with a queued resend was resurrected as Pending instead of dropped")
	}
}

// TestClearNonManualPreservesSendingItemThenDropsWithNoResend confirms the
// non-resend twin of the case above: an orphaned Sending item with no
// Changed arriving mid-transfer is dropped once finishSend observes it has
// fully settled, exactly as it already was before finding 4's fix — this
// covers finishSend's other, pre-existing orphaned-cleanup branch.
func TestClearNonManualPreservesSendingItemThenDropsWithNoResend(t *testing.T) {
	t.Parallel()
	e := newUnitTestEngine(t, nil)

	it := &item{name: "G.NC", path: "/does/not/matter", state: Sending, sending: true}
	e.scheduler.items["G.NC"] = it

	e.scheduler.clearNonManual()

	e.scheduler.mu.Lock()
	_, stillThere := e.scheduler.items["G.NC"]
	orphaned := it.orphaned
	e.scheduler.mu.Unlock()
	if !stillThere {
		t.Fatal("clearNonManual deleted a Sending item, losing the in-flight send")
	}
	if !orphaned {
		t.Error("item not marked orphaned by clearNonManual")
	}

	s := e.scheduler
	s.finishSend(context.Background(), it, sendSource{}, errors.New("simulated failure"), "simulated failure")

	s.mu.Lock()
	_, stillThere = s.items["G.NC"]
	s.mu.Unlock()
	if stillThere {
		t.Error("orphaned item with no queued resend was not dropped once its send settled")
	}
}

func TestNextDeadlineElapsedReturnsMinimalPoll(t *testing.T) {
	t.Parallel()
	e := newUnitTestEngine(t, nil)
	fake, ok := e.opts.Clock.(*clock.Fake)
	if !ok {
		t.Fatalf("Clock = %T, want *clock.Fake (newUnitTestEngine's default)", e.opts.Clock)
	}
	it := &item{name: "A.NC", state: Failed, nextAttempt: fake.Now().Add(-time.Second)}
	e.scheduler.items["A.NC"] = it

	if d := e.scheduler.nextDeadline(); d != time.Millisecond {
		t.Errorf("nextDeadline() = %v, want time.Millisecond for an already-elapsed item", d)
	}
}
