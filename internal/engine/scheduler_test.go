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

// This file exercises the scheduler end-to-end: a real watcher on a
// t.TempDir(), a real *masso.Client, and a real internal/masso/sim
// controller over loopback UDP, all on a real (but short-interval) clock —
// matching conn_test.go's style, since the scheduler's own timing
// (backoff, archive retry) needs to interleave with genuine socket and
// filesystem latency.

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"msrl.dev/mink-lasso/internal/config"
	"msrl.dev/mink-lasso/internal/masso"
)

// schedTestOptions returns Options wired for a fast, real-time scheduler
// test: short watcher, backoff, and archive-retry intervals alongside
// conn_test.go's short connection timeouts.
func schedTestOptions(serial uint32, watchDir string) Options {
	opts := connTestOptions(serial)
	opts.Config.WatchDir = watchDir
	// StallTimeout and StartTimeout decide Sent versus Failed, so like
	// LostAfter they are long enough that load cannot trip them in a test
	// that expects a transfer to succeed.
	opts.ClientOptions.StallTimeout = 5 * time.Second
	opts.ClientOptions.StartRetransmit = 15 * time.Millisecond
	opts.ClientOptions.StartTimeout = 5 * time.Second
	opts.Config.ScanInterval = config.Duration(20 * time.Millisecond)
	opts.Config.SettleDelay = config.Duration(30 * time.Millisecond)
	opts.Backoff = []time.Duration{60 * time.Millisecond, 100 * time.Millisecond}
	opts.MoveRetries = 2
	opts.MoveRetryInterval = 20 * time.Millisecond
	opts.ProgressInterval = 10 * time.Millisecond
	return opts
}

func isTransferEvent(name string, state TransferState) func(Event) bool {
	return func(ev Event) bool {
		te, ok := ev.(TransferEvent)
		return ok && te.Name == name && te.State == state
	}
}

// waitForEvents waits for every predicate in preds to match some event, in
// any order, failing the test if connTestTimeout elapses first. Unlike
// waitForEvent, it never discards an event that matches a predicate other
// than the one currently "in front" — needed whenever two outcomes (here,
// two different files' sends) can legitimately complete in either order.
func waitForEvents(t *testing.T, events <-chan Event, preds ...func(Event) bool) {
	t.Helper()
	remaining := append([]func(Event) bool{}, preds...)
	deadline := time.After(connTestTimeout)
	for len(remaining) > 0 {
		select {
		case ev, ok := <-events:
			if !ok {
				t.Fatal("events channel closed before every expected event arrived")
			}
			for i, pred := range remaining {
				if pred(ev) {
					remaining = append(remaining[:i], remaining[i+1:]...)
					break
				}
			}
		case <-deadline:
			t.Fatalf("timed out waiting for %d expected event(s)", len(remaining))
		}
	}
}

func asTransferEvent(t *testing.T, ev Event) TransferEvent {
	t.Helper()
	te, ok := ev.(TransferEvent)
	if !ok {
		t.Fatalf("event = %#v, want TransferEvent", ev)
	}
	return te
}

func writeFile(t *testing.T, dir, name string, data []byte) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return path
}

// overwriteFileAtomic replaces path's content via write-then-rename, like a
// well-behaved post-processor: an already-open reader of the old content
// (mid-upload, say) keeps reading the old inode undisturbed, rather than
// seeing a plain os.WriteFile's transient truncation of the very file it is
// reading.
func overwriteFileAtomic(t *testing.T, path string, data []byte) {
	t.Helper()
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatalf("Rename: %v", err)
	}
}

// TestSchedulerSendsAndArchives covers the basic happy path: a dropped
// file settles, the simulator receives the exact bytes, and the file ends
// up in sent/.
func TestSchedulerSendsAndArchives(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s := newConnTestSim(t, 1001)
	opts := schedTestOptions(1001, dir)
	opts.Config.Address = s.Addr().String()
	opts.NewClient = newConnTestNewClient(freeAdapterPort())

	e, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	events, _ := runEngine(t, e)
	waitForEvent(t, events, isConnState(Connected))

	want := []byte("G0 X0 Y0\n")
	writeFile(t, dir, "PART.NC", want)

	waitForEvent(t, events, isTransferEvent("PART.NC", Sent))

	got, ok := s.File("PART.NC")
	if !ok {
		t.Fatal("sim did not receive PART.NC")
	}
	if string(got) != string(want) {
		t.Errorf("sim received %q, want %q", got, want)
	}
	if _, err := os.Stat(filepath.Join(dir, "PART.NC")); !errors.Is(err, os.ErrNotExist) {
		t.Error("PART.NC still in watch dir, want moved into sent/")
	}
	if _, err := os.Stat(filepath.Join(dir, "sent", "PART.NC")); err != nil {
		t.Errorf("sent/PART.NC: %v", err)
	}
}

// TestSchedulerArchiveCollisionGetsTimestamped confirms an existing
// sent/NAME is archived aside with a timestamp rather than overwritten, and
// that a second collision within the same run gets a "-N" suffix.
func TestSchedulerArchiveCollisionGetsTimestamped(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	sentDir := filepath.Join(dir, "sent")
	if err := os.MkdirAll(sentDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	writeFile(t, sentDir, "PART.NC", []byte("old"))

	s := newConnTestSim(t, 1002)
	opts := schedTestOptions(1002, dir)
	opts.Config.Address = s.Addr().String()
	opts.NewClient = newConnTestNewClient(freeAdapterPort())

	e, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	events, _ := runEngine(t, e)
	waitForEvent(t, events, isConnState(Connected))

	writeFile(t, dir, "PART.NC", []byte("new"))
	waitForEvent(t, events, isTransferEvent("PART.NC", Sent))

	entries, err := os.ReadDir(sentDir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("sent/ has %d entries, want 2 (the archived original plus the new send)", len(entries))
	}
	foundBackup := false
	for _, de := range entries {
		if de.Name() != "PART.NC" {
			foundBackup = true
			data, err := os.ReadFile(filepath.Join(sentDir, de.Name()))
			if err != nil {
				t.Fatalf("ReadFile backup: %v", err)
			}
			if string(data) != "old" {
				t.Errorf("backup %s contains %q, want %q", de.Name(), data, "old")
			}
		}
	}
	if !foundBackup {
		t.Error("no timestamped backup found in sent/")
	}
}

// TestSchedulerStartAckNoUSBThenRetrySucceeds covers a start-ACK failure
// (No USB): Failed with the exact wording, then a backoff and a
// successful retry — while a second file queued behind it still goes
// through in the meantime.
func TestSchedulerStartAckNoUSBThenRetrySucceeds(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s := newConnTestSim(t, 1003)
	opts := schedTestOptions(1003, dir)
	opts.Config.Address = s.Addr().String()
	opts.NewClient = newConnTestNewClient(freeAdapterPort())

	e, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	events, _ := runEngine(t, e)
	waitForEvent(t, events, isConnState(Connected))

	s.SetStartResult(masso.StartNoUSB)
	writeFile(t, dir, "A.NC", []byte("aaaa"))

	ev := waitForEvent(t, events, isTransferEvent("A.NC", Failed))
	te := asTransferEvent(t, ev)
	if te.Message != "No USB flash drive connected to Masso" {
		t.Errorf("Failed message = %q, want the No-USB wording", te.Message)
	}

	// Reset the sim before B arrives so B goes through immediately, while
	// A.NC is still sitting in its backoff window; A's own automatic
	// retry then picks it up once that window elapses. The two can finish
	// in either order — A's backoff is short — so wait for both without
	// assuming which comes first.
	s.SetStartResult(masso.StartOK)
	writeFile(t, dir, "B.NC", []byte("bbbb"))
	waitForEvents(t, events, isTransferEvent("B.NC", Sent), isTransferEvent("A.NC", Sent))
}

// TestSchedulerChunkCanceledNoAutoRetry covers a chunk ACK "canceled by
// operator" result: Failed with the canceled wording, no automatic retry,
// and Retry(name) resending it.
func TestSchedulerChunkCanceledNoAutoRetry(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s := newConnTestSim(t, 1004)
	opts := schedTestOptions(1004, dir)
	opts.Config.Address = s.Addr().String()
	opts.NewClient = newConnTestNewClient(freeAdapterPort())

	e, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	events, _ := runEngine(t, e)
	waitForEvent(t, events, isConnState(Connected))

	s.SetChunkResult(masso.ChunkCanceled)
	writeFile(t, dir, "C.NC", []byte("cccc"))

	ev := waitForEvent(t, events, isTransferEvent("C.NC", Failed))
	te := asTransferEvent(t, ev)
	if want := "File transfer canceled by user on Masso; resend it before running it"; te.Message != want {
		t.Errorf("Failed message = %q, want %q", te.Message, want)
	}

	// No automatic retry: nothing more should arrive for C.NC for well
	// longer than the shortest backoff interval.
	select {
	case ev := <-events:
		if te, ok := ev.(TransferEvent); ok && te.Name == "C.NC" {
			t.Fatalf("unexpected auto-retry event for C.NC: %#v", te)
		}
	case <-time.After(150 * time.Millisecond):
	}

	s.SetChunkResult(masso.ChunkOK)
	e.Retry("C.NC")
	waitForEvent(t, events, isTransferEvent("C.NC", Sent))
}

// TestSchedulerSilentAfterChunkIncompleteSuffixThenRetry covers the
// controller going silent mid-transfer: ErrNoResponse wording with the
// incomplete-file suffix, then a successful retry after backoff.
func TestSchedulerSilentAfterChunkIncompleteSuffixThenRetry(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s := newConnTestSim(t, 1005)
	opts := schedTestOptions(1005, dir)
	opts.Config.Address = s.Addr().String()
	opts.NewClient = newConnTestNewClient(freeAdapterPort())
	opts.ClientOptions.ReplyTimeout = 15 * time.Millisecond
	opts.ClientOptions.StallTimeout = 150 * time.Millisecond

	e, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	events, _ := runEngine(t, e)
	waitForEvent(t, events, isConnState(Connected))

	s.SetSilentAfterChunk(0)
	writeFile(t, dir, "D.NC", make([]byte, 4096))

	ev := waitForEvent(t, events, isTransferEvent("D.NC", Failed))
	te := asTransferEvent(t, ev)
	want := "ERROR: No response from Masso — the file on the Masso may be incomplete; it will be resent"
	if te.Message != want {
		t.Errorf("Failed message = %q, want %q", te.Message, want)
	}

	s.SetSilentAfterChunk(-1)
	waitForEvent(t, events, isTransferEvent("D.NC", Sent))
}

// TestSchedulerWaitsForConnection confirms files Wait ("Not connected")
// until the controller appears, then send — and that if the file is
// overwritten while it sits Waiting, the new bytes are what the controller
// receives (ready()'s refresh-in-place branch for a Pending/Waiting item,
// as opposed to the resendAfter branch for one that is Sending).
func TestSchedulerWaitsForConnection(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	// No serial configured yet: the engine idles Unconfigured, so nothing
	// is ever connected and the gate reports "Not connected".
	opts := schedTestOptions(0, dir)
	opts.Config.Serial = ""
	opts.NewClient = newConnTestNewClient(freeAdapterPort())

	e, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	events, _ := runEngine(t, e)
	waitForEvent(t, events, isConnState(Unconfigured))

	path := writeFile(t, dir, "E.NC", []byte("eeee"))
	ev := waitForEvent(t, events, isTransferEvent("E.NC", Waiting))
	if te := asTransferEvent(t, ev); te.Message != "Not connected" {
		t.Errorf("Waiting message = %q, want %q", te.Message, "Not connected")
	}

	overwriteFileAtomic(t, path, []byte("new bytes while waiting"))
	waitForEvent(t, events, isTransferEvent("E.NC", Waiting))

	s := newConnTestSim(t, 1006)
	e.opts.Config.Address = s.Addr().String() // harmless: candidateAddrs reads Config directly
	e.SetSerial(1006)

	waitForEvent(t, events, isConnState(Connected))
	waitForEvent(t, events, isTransferEvent("E.NC", Sent))

	got, ok := s.File("E.NC")
	if !ok {
		t.Fatal("sim did not receive E.NC")
	}
	if string(got) != "new bytes while waiting" {
		t.Errorf("sim received %q, want the overwritten content", got)
	}
}

// TestSchedulerManualSendFile covers SendFile's happy path (not moved into
// sent/), a missing file, and a bad name. SendFile's ErrBusy case is
// covered separately by TestSendFileBusyFileError (watchdir_unit_test.go):
// it needs an injected Open override, since winutil.OpenDenyWrite cannot be
// made to report ErrBusy deterministically off Windows.
func TestSchedulerManualSendFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s := newConnTestSim(t, 1007)
	opts := schedTestOptions(1007, dir)
	opts.Config.Address = s.Addr().String()
	opts.Config.WatchDir = "" // manual send does not require a watch folder
	opts.NewClient = newConnTestNewClient(freeAdapterPort())

	e, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	events, _ := runEngine(t, e)
	waitForEvent(t, events, isConnState(Connected))

	path := writeFile(t, dir, "MANUAL.NC", []byte("manual"))
	if err := e.SendFile(path); err != nil {
		t.Fatalf("SendFile: %v", err)
	}

	ev := waitForEvent(t, events, isTransferEvent("MANUAL.NC", Sent))
	if !asTransferEvent(t, ev).Manual {
		t.Error("Sent event Manual = false, want true")
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("manual send moved the file: %v", err)
	}

	if err := e.SendFile(filepath.Join(dir, "nope.nc")); err == nil {
		t.Error("SendFile on a missing file: error = nil, want non-nil")
	}
	if err := e.SendFile(filepath.Join(dir, "this-name-is-way-too-long-for-masso.nc")); err == nil {
		t.Error("SendFile with a bad name: error = nil, want non-nil")
	}
}

// TestSchedulerSetWatchDirClearsQueue confirms SetWatchDir rejects a bad
// directory, and that switching to a new one clears the old, non-manual
// queue and watches the new folder.
func TestSchedulerSetWatchDirClearsQueue(t *testing.T) {
	t.Parallel()
	dir1 := t.TempDir()
	dir2 := t.TempDir()

	// Left unconfigured (no serial), the connection loop never connects,
	// so the gate reliably reports "Not connected" and OLD.NC sits
	// Waiting instead of racing an actual send.
	opts := schedTestOptions(0, dir1)
	opts.Config.Serial = ""
	opts.NewClient = newConnTestNewClient(freeAdapterPort())

	e, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	events, _ := runEngine(t, e)
	waitForEvent(t, events, isConnState(Unconfigured))

	writeFile(t, dir1, "OLD.NC", []byte("old"))
	waitForEvent(t, events, isTransferEvent("OLD.NC", Waiting))

	if err := e.SetWatchDir(filepath.Join(dir2, "does-not-exist")); err == nil {
		t.Error("SetWatchDir on a missing dir: error = nil, want non-nil")
	}
	if err := e.SetWatchDir(dir2); err != nil {
		t.Fatalf("SetWatchDir: %v", err)
	}

	waitForEvent(t, events, func(ev Event) bool {
		ws, ok := ev.(WatchState)
		return ok && ws.Dir == dir2
	})

	e.scheduler.mu.Lock()
	_, stillThere := e.scheduler.items["OLD.NC"]
	e.scheduler.mu.Unlock()
	if stillThere {
		t.Error("OLD.NC still in queue after SetWatchDir, want cleared")
	}
}

// DONE.NC is planted directly because nothing here can complete a send with
// no controller configured.
func TestSchedulerSetWatchDirEmitsDroppedForNonFinalItems(t *testing.T) {
	t.Parallel()
	dir1 := t.TempDir()
	dir2 := t.TempDir()

	opts := schedTestOptions(0, dir1)
	opts.Config.Serial = ""
	opts.NewClient = newConnTestNewClient(freeAdapterPort())

	e, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	events, _ := runEngine(t, e)
	waitForEvent(t, events, isConnState(Unconfigured))

	writeFile(t, dir1, "OLD.NC", []byte("old"))
	waitForEvent(t, events, isTransferEvent("OLD.NC", Waiting))

	e.scheduler.mu.Lock()
	e.scheduler.items["DONE.NC"] = &item{name: "DONE.NC", state: Sent, message: "File sent"}
	e.scheduler.mu.Unlock()

	if err := e.SetWatchDir(dir2); err != nil {
		t.Fatalf("SetWatchDir: %v", err)
	}

	// clearNonManual's own Dropped emissions race the new watch attempt's
	// WatchState (two different goroutines), so this drains only until it
	// finds OLD.NC's Dropped event, checking every event seen along the way
	// for one that should never exist: anything at all naming the
	// already-terminal DONE.NC.
	deadline := time.After(connTestTimeout)
collect:
	for {
		select {
		case ev, ok := <-events:
			if !ok {
				t.Fatal("events channel closed before OLD.NC's Dropped event arrived")
			}
			if te, isTE := ev.(TransferEvent); isTE && te.Name == "DONE.NC" {
				t.Errorf("unexpected event for already-terminal DONE.NC: %+v", te)
			}
			if te, isTE := ev.(TransferEvent); isTE && te.Name == "OLD.NC" && te.State == Dropped {
				if te.Message != DroppedWatchFolderChanged {
					t.Errorf("OLD.NC Dropped message = %q, want %q", te.Message, DroppedWatchFolderChanged)
				}
				break collect
			}
		case <-deadline:
			t.Fatal("timed out waiting for OLD.NC's Dropped event")
		}
	}

	e.scheduler.mu.Lock()
	_, oldStill := e.scheduler.items["OLD.NC"]
	_, doneStill := e.scheduler.items["DONE.NC"]
	e.scheduler.mu.Unlock()
	if oldStill || doneStill {
		t.Error("items still in queue after SetWatchDir, want both cleared")
	}
}

// TestSchedulerRejectedPassthrough confirms a watcher Rejected file is
// reported as a Rejected transfer with the reason as its message.
func TestSchedulerRejectedPassthrough(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s := newConnTestSim(t, 1009)
	opts := schedTestOptions(1009, dir)
	opts.Config.Address = s.Addr().String()
	opts.NewClient = newConnTestNewClient(freeAdapterPort())

	e, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	events, _ := runEngine(t, e)
	waitForEvent(t, events, isConnState(Connected))

	// A name over masso.MaxFileName (15 bytes) fails ValidateFileName.
	writeFile(t, dir, "THIS-NAME-IS-TOO-LONG.NC", []byte("x"))

	ev := waitForEvent(t, events, func(ev Event) bool {
		te, ok := ev.(TransferEvent)
		return ok && te.Name == "THIS-NAME-IS-TOO-LONG.NC" && te.State == Rejected
	})
	if asTransferEvent(t, ev).Message == "" {
		t.Error("Rejected event has empty Message, want the validation reason")
	}
}

// TestSchedulerArchiveFailureThenSentUnfiled confirms a Rename that always
// fails leads to SentUnfiled after MoveRetries, that the file is not
// re-uploaded, and that Retry sends it again.
func TestSchedulerArchiveFailureThenSentUnfiled(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s := newConnTestSim(t, 1010)
	opts := schedTestOptions(1010, dir)
	opts.Config.Address = s.Addr().String()
	opts.NewClient = newConnTestNewClient(freeAdapterPort())

	renameErr := errors.New("simulated rename failure")
	failRename := true
	opts.Rename = func(oldpath, newpath string) error {
		if failRename {
			return renameErr
		}
		return os.Rename(oldpath, newpath)
	}

	e, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	events, _ := runEngine(t, e)
	waitForEvent(t, events, isConnState(Connected))

	writeFile(t, dir, "F.NC", []byte("ffff"))
	ev := waitForEvent(t, events, isTransferEvent("F.NC", SentUnfiled))
	if msg := asTransferEvent(t, ev).Message; msg == "" {
		t.Error("SentUnfiled event has empty Message")
	}

	// It must not be re-uploaded on its own.
	select {
	case ev := <-events:
		if te, ok := ev.(TransferEvent); ok && te.Name == "F.NC" && te.State == Sending {
			t.Fatalf("F.NC re-sent on its own after SentUnfiled: %#v", te)
		}
	case <-time.After(150 * time.Millisecond):
	}

	failRename = false
	e.Retry("F.NC")
	waitForEvent(t, events, isTransferEvent("F.NC", Sent))
}

// settledState waits until name's item is no longer being sent and returns
// its state. A final Sent event can precede the moment the scheduler lets
// go of the item, and one that is re-armed for a resend reports Pending.
func settledState(t *testing.T, e *Engine, name string) TransferState {
	t.Helper()
	deadline := time.Now().Add(connTestTimeout)
	for {
		e.scheduler.mu.Lock()
		it := e.scheduler.items[name]
		sending := it != nil && it.sending
		var state TransferState
		if it != nil {
			state = it.state
		}
		e.scheduler.mu.Unlock()
		if !sending {
			return state
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s still sending after %v", name, connTestTimeout)
		}
		time.Sleep(time.Millisecond)
	}
}

// A file changed while its stale content is mid-transfer ends up on the
// controller as changed: either the stale transfer completes and the change
// is resent, or it fails and its retry sends the change.
func TestSchedulerChangedDuringSendingSendsTwice(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s := newConnTestSim(t, 1011)
	opts := schedTestOptions(1011, dir)
	opts.Config.Address = s.Addr().String()
	opts.NewClient = newConnTestNewClient(freeAdapterPort())
	// A big enough file that the loopback transfer takes long enough for
	// the watcher's own scan/settle cycle to notice a mid-transfer
	// overwrite; both need to be fast relative to that.
	opts.Config.ScanInterval = config.Duration(5 * time.Millisecond)
	opts.Config.SettleDelay = config.Duration(5 * time.Millisecond)

	e, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	events, _ := runEngine(t, e)
	waitForEvent(t, events, isConnState(Connected))

	const size = 4 << 20 // 4MiB: enough transfer time for the overwrite below
	path := writeFile(t, dir, "G.NC", bytes.Repeat([]byte{'a'}, size))
	waitForEvent(t, events, isTransferEvent("G.NC", Sending))

	// Change the file while the first transfer is still going (how, and
	// what the resend can then deliver, depends on the platform; see
	// changeDuringSending); the watcher needs another full settle window
	// to notice.
	want := changeDuringSending(t, path, size)

	for {
		waitForEvent(t, events, isTransferEvent("G.NC", Sent))
		if settledState(t, e, "G.NC") == Sent {
			break
		}
	}

	got, ok := s.File("G.NC")
	if !ok {
		t.Fatal("sim did not receive G.NC")
	}
	if !bytes.Equal(got, want) {
		t.Error("sim's final G.NC content is not what the resend should have delivered")
	}
}

// A manual SendFile for a name that is already auto-uploading updates the
// in-flight item in place: the in-progress transfer is not aborted, the
// manual request is sent after it (or by its retry, if it fails), and every
// send is reported Manual: true and never archived into sent/.
func TestSchedulerSendFileDuringInFlightAutoSendResends(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s := newConnTestSim(t, 1012)
	opts := schedTestOptions(1012, dir)
	opts.Config.Address = s.Addr().String()
	opts.NewClient = newConnTestNewClient(freeAdapterPort())

	e, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	events, _ := runEngine(t, e)
	waitForEvent(t, events, isConnState(Connected))

	const size = 4 << 20 // 4MiB: enough transfer time to call SendFile mid-flight
	path := writeFile(t, dir, "H.NC", bytes.Repeat([]byte{'a'}, size))
	waitForEvent(t, events, isTransferEvent("H.NC", Sending))

	if err := e.SendFile(path); err != nil {
		t.Fatalf("SendFile: %v", err)
	}

	for {
		ev := waitForEvent(t, events, isTransferEvent("H.NC", Sent))
		if !asTransferEvent(t, ev).Manual {
			t.Error("Sent event Manual = false, want true (SendFile arrived mid-transfer)")
		}
		if settledState(t, e, "H.NC") == Sent {
			break
		}
	}

	// A manual send is never archived, unlike the non-manual resend in
	// TestSchedulerChangedDuringSendingSendsTwice.
	if _, err := os.Stat(path); err != nil {
		t.Errorf("H.NC should remain in the watch dir (manual sends are not archived): %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "sent", "H.NC")); !errors.Is(err, os.ErrNotExist) {
		t.Error("H.NC must not be archived into sent/ after a manual SendFile arrived mid-transfer")
	}
}

// TestRunWaitsForInFlightTransferBeforeReturning confirms canceling Run's
// ctx while a transfer is genuinely in flight — past the start ACK, per
// masso.Client.Upload's own "acknowledged as started" guarantee — does not
// abort it: Run must block until that send finishes and the file lands
// intact, rather than returning early and leaving a partial upload on the
// controller's USB drive (finding 6). Canceling right as the very first
// Sending event fires (before Upload has even been called) would legally
// abort before anything reaches the wire, so this waits for a Sending
// event with Sent > 0 to make sure the transfer is truly underway.
func TestRunWaitsForInFlightTransferBeforeReturning(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s := newConnTestSim(t, 1013)
	opts := schedTestOptions(1013, dir)
	opts.Config.Address = s.Addr().String()
	opts.NewClient = newConnTestNewClient(freeAdapterPort())

	e, err := New(opts)
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
	t.Cleanup(func() {
		cancel()
		select {
		case <-runDone:
		case <-time.After(connTestTimeout):
			t.Fatal("Run did not return during cleanup")
		}
	})

	events := e.Events()
	waitForEvent(t, events, isConnState(Connected))

	const size = 4 << 20 // 4MiB: enough transfer time to cancel mid-flight
	want := bytes.Repeat([]byte{'c'}, size)
	writeFile(t, dir, "K.NC", want)

	deadline := time.After(connTestTimeout)
waitForAck:
	for {
		select {
		case ev, ok := <-events:
			if !ok {
				t.Fatal("events channel closed before the transfer was acknowledged as started")
			}
			if te, ok := ev.(TransferEvent); ok && te.Name == "K.NC" && te.State == Sending && te.Sent > 0 {
				break waitForAck
			}
		case <-deadline:
			t.Fatal("timed out waiting for K.NC to be acknowledged as started")
		}
	}

	cancel()

	select {
	case <-runDone:
		t.Fatal("Run returned immediately after ctx cancel, want it to wait for the in-flight transfer")
	case <-time.After(200 * time.Millisecond):
	}

	waitForEvent(t, events, isTransferEvent("K.NC", Sent))

	select {
	case <-runDone:
	case <-time.After(connTestTimeout):
		t.Fatal("Run did not return after the in-flight transfer finished")
	}

	got, ok := s.File("K.NC")
	if !ok {
		t.Fatal("sim did not receive K.NC")
	}
	if !bytes.Equal(got, want) {
		t.Error("sim's final K.NC content does not match what was written")
	}
	if _, err := os.Stat(filepath.Join(dir, "sent", "K.NC")); err != nil {
		t.Errorf("sent/K.NC: %v", err)
	}
}
