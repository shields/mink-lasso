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
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"msrl.dev/mink-lasso/internal/masso"
	"msrl.dev/mink-lasso/internal/watch"
)

// msgWaitingToSend and msgSending are the item.message text for Pending and
// Sending respectively; msgSending also doubles as TransferState.Sending's
// String() (see event.go), which happens to read identically.
const (
	msgWaitingToSend = "Waiting to send"
	msgSending       = "Sending"
)

// item is one file the scheduler knows about, keyed by name. The scheduler
// goroutine (run) is the only place a send ever happens, but the map is
// also written from the watcher-forwarding goroutine and from Retry/
// SendFile, so it lives behind scheduler.mu.
type item struct {
	// name is the map key and TransferEvent.Name: the file's path
	// relative to root, the watch folder it came from, or its base name
	// for a manual send (whose root is ""). dir and base are that path's
	// OS-native folder ("" for the root) and base name.
	name    string
	root    string
	dir     string
	base    string
	path    string
	size    int64
	modTime time.Time
	manual  bool

	state   TransferState
	message string

	// failCount selects Backoff[min(failCount-1, len-1)] after a real
	// upload failure; it is distinct from the fixed Backoff[0] retry used
	// for a pre-flight open failure (busy or vanished), which is not a
	// "failure" of the upload itself.
	failCount int
	// nextAttempt is when the scheduler may next try this item; the zero
	// value means "as soon as the gate and connection allow" (also true
	// of a Failed item after ErrCanceled, which gets no automatic retry
	// at all — ErrCanceled sets state to Failed and leaves nextAttempt
	// zero, but next() only considers Pending/Waiting items, so it simply
	// stays put until Retry or a watcher Changed re-arms it).
	nextAttempt time.Time

	// sending is true for the one item currently being uploaded.
	sending bool
	// resendAfter is set when a Changed arrives for an item that is
	// currently Sending: the stale transfer in flight is never aborted,
	// but the moment it finishes the new bytes are sent.
	resendAfter bool
	// orphaned marks a non-manual item that clearNonManual could not
	// delete outright because it was still Sending: finishSend removes it
	// from the map once the in-flight transfer (and any resend it queued)
	// has fully settled, so a folder switch never permanently loses an
	// item this way but also never leaves it in the queue forever.
	orphaned bool
}

// scheduler owns the upload queue and the single background goroutine
// (run) that sends one file at a time.
type scheduler struct {
	e *Engine

	mu    sync.Mutex
	items map[string]*item
	wake  chan struct{}
}

func newScheduler(e *Engine) *scheduler {
	return &scheduler{
		e:     e,
		items: make(map[string]*item),
		wake:  make(chan struct{}, 1),
	}
}

// poke wakes the scheduler loop, non-blocking; safe from any goroutine.
func (s *scheduler) poke() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// ready folds in a Ready file from the watcher on root: a brand new name
// enters as Pending; a resend of the name currently Sending is deferred
// until it finishes; anything else just has its size/mtime refreshed and
// its failure state cleared — the Changed re-emission the package doc
// describes.
func (s *scheduler) ready(root string, f watch.File) {
	s.mu.Lock()
	it := s.itemLocked(root, f.Dir, f.Name)
	it.path = f.Path
	it.size = f.Size
	it.modTime = f.ModTime

	if it.sending {
		it.resendAfter = true
	} else {
		it.state = Pending
		it.message = msgWaitingToSend
		it.failCount = 0
		it.nextAttempt = time.Time{}
		// A plain watcher Ready/Changed event is not a manual send: clear
		// any manual flag left over from a previous SendFile of this same
		// name, so Manual only ever describes the one SendFile call that
		// set it (see finding 2), not the filename from then on.
		it.manual = false
	}
	ev := s.event(it)
	s.mu.Unlock()

	s.e.dispatcher.emit(ev)
	s.poke()
}

// rejected records a Rejected file from the watcher on root as a terminal
// Rejected transfer; it is never retried automatically.
func (s *scheduler) rejected(root string, r watch.Rejected) {
	s.mu.Lock()
	it := s.itemLocked(root, r.Dir, r.Name)
	it.path = r.Path
	it.state = Rejected
	it.message = r.Reason
	it.sending = false
	ev := s.event(it)
	s.mu.Unlock()

	s.e.dispatcher.emit(ev)
}

// itemLocked returns the item for base in the folder dir under the watch
// folder root, creating it if need be, with its location set. An orphaned
// item is replaced rather than reused: it still belongs to the watch folder
// SetWatchDir moved away from, and will archive and drop itself once its
// send settles. Callers must hold s.mu.
func (s *scheduler) itemLocked(root, dir, base string) *item {
	name := filepath.Join(dir, base)
	it, ok := s.items[name]
	if !ok || it.orphaned {
		it = &item{name: name}
		s.items[name] = it
	}
	it.root, it.dir, it.base = root, dir, base
	return it
}

// clearNonManual drops every non-manual entry, used when SetWatchDir
// restarts the watcher against a new folder. An item currently Sending is
// left in place: sendOne/finishSend hold its *item directly, not by map
// lookup, and finishSend's resend branch (see resendAfter) re-arms it to
// Pending in place — deleting the map entry out from under that would
// silently lose the resend, since next()/nextDeadline() only ever range
// over s.items. Once finishSend observes it.sending go false without a
// resend pending, it removes the entry itself (see finishSend).
func (s *scheduler) clearNonManual() {
	s.mu.Lock()
	for name, it := range s.items {
		if !it.manual && !it.sending {
			delete(s.items, name)
		} else if !it.manual {
			it.orphaned = true
		}
	}
	s.mu.Unlock()
}

// retry re-queues a Failed, Rejected, or SentUnfiled file immediately.
// Unknown, or non-retryable, names are ignored with a log line.
func (s *scheduler) retry(name string) {
	s.mu.Lock()
	it, ok := s.items[name]
	if !ok || !it.state.Retryable() {
		s.mu.Unlock()
		s.e.opts.Logger.Info("engine: retry of unknown or non-retryable file ignored", "name", name)
		return
	}
	it.state = Pending
	it.message = msgWaitingToSend
	it.failCount = 0
	it.nextAttempt = time.Time{}
	ev := s.event(it)
	s.mu.Unlock()

	s.e.dispatcher.emit(ev)
	s.poke()
}

// sendFile queues path as a manual send to the drive root under its base
// name: never archived, always reported with Manual: true. If name is
// currently Sending, the map's existing *item (which sendOne/finishSend
// hold directly) is updated in place and marked for a resend once that
// transfer finishes — mirroring ready() — rather than replaced outright:
// replacing the map entry would orphan the in-flight item, whose
// completion would still archive the file out from under this manual
// request with no visible error.
func (s *scheduler) sendFile(name, path string, size int64, modTime time.Time) {
	s.mu.Lock()
	it := s.itemLocked("", "", name)
	it.path = path
	it.size = size
	it.modTime = modTime
	it.manual = true

	if it.sending {
		it.resendAfter = true
	} else {
		it.state = Pending
		it.message = msgWaitingToSend
		it.failCount = 0
		it.nextAttempt = time.Time{}
	}
	ev := s.event(it)
	s.mu.Unlock()

	s.e.dispatcher.emit(ev)
	s.poke()
}

// event snapshots it as a TransferEvent. Callers must hold s.mu.
func (s *scheduler) event(it *item) TransferEvent {
	return TransferEvent{
		Name: it.name, Path: it.path, Size: it.size,
		State: it.state, Message: it.message, Manual: it.manual,
		At: s.e.opts.Clock.Now(),
	}
}

// run is the scheduler's single background goroutine: it picks the best
// eligible item, sends it synchronously (one at a time), and repeats,
// waking on demand instead of polling. Because a send is synchronous on
// this goroutine, ctx cancellation is only ever observed between sends —
// which is exactly what lets Run wait for an in-flight, acknowledged
// transfer to finish rather than abort it.
func (s *scheduler) run(ctx context.Context) {
	for ctx.Err() == nil {
		it, ok := s.next()
		if !ok {
			s.waitForWork(ctx)
			continue
		}
		s.sendOne(ctx, it)
	}
}

// waitForWork blocks until there might be something new to do: an explicit
// poke (new/changed file, retry, gate open), the gate's own timer/state
// signal, or the earliest item-specific backoff deadline elapsing.
func (s *scheduler) waitForWork(ctx context.Context) {
	var timerC <-chan time.Time
	if d := s.nextDeadline(); d > 0 {
		t := s.e.opts.Clock.NewTimer(d)
		defer t.Stop()
		timerC = t.C()
	}
	select {
	case <-ctx.Done():
	case <-s.wake:
	case <-s.e.gate.readyChan():
	case <-timerC:
	}
}

// nextDeadline returns how long until the earliest pending item's
// nextAttempt, or 0 if none is scheduled (wait indefinitely for a poke).
func (s *scheduler) nextDeadline() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	var best time.Time
	for _, it := range s.items {
		// Only Pending/Waiting/Failed ever carry a live nextAttempt that
		// still matters; a terminal item (Sent, Rejected, SentUnfiled) can
		// be left holding a stale, long-elapsed one from an earlier
		// attempt, which next() rightly ignores by state — but this loop
		// must too, or a permanently-elapsed timestamp here would arm a
		// 1ms timer forever and spin the whole loop. (A Sending item is
		// excluded the same way: this only ever runs when run's single
		// goroutine has already confirmed nothing is currently sending.)
		switch it.state {
		case Pending, Waiting, Failed:
		default:
			continue
		}
		if it.nextAttempt.IsZero() {
			continue
		}
		if best.IsZero() || it.nextAttempt.Before(best) {
			best = it.nextAttempt
		}
	}
	if best.IsZero() {
		return 0
	}
	if d := best.Sub(s.e.opts.Clock.Now()); d > 0 {
		return d
	}
	return time.Millisecond
}

// next picks the best candidate to send now: manual items first, then by
// (ModTime, Name), skipping anything whose nextAttempt has not arrived yet.
// If the gate is closed, every other Pending/Waiting item is (re)reported
// Waiting with the gate's reason instead.
func (s *scheduler) next() (*item, bool) {
	gate := s.e.gate.current()

	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.e.opts.Clock.Now()
	var candidates []*item
	for _, it := range s.items {
		// An item currently Sending (with or without resendAfter set) is
		// never in state Pending/Waiting/Failed at the same time — run's
		// single goroutine only ever calls next() between sends, never
		// during one — so the switch below already excludes it without
		// needing to check sending/resendAfter directly here too.
		if it.state == Failed {
			// A Failed item is only eligible again once its backoff
			// elapses; ErrCanceled leaves nextAttempt zero, which
			// never elapses, giving it no automatic retry at all.
			//
			// This deliberately does not pass through Pending first: per
			// event.go's TransferState doc, Failed is terminal-but-
			// retryable, not a state a fresh attempt must be reported as
			// leaving before Sending. Emitting Pending here would be
			// synchronously overwritten by Sending (or Waiting) before any
			// consumer could observe it, on this same single goroutine.
			if it.nextAttempt.IsZero() {
				continue
			}
		} else if it.state != Pending && it.state != Waiting {
			continue
		}
		if !it.nextAttempt.IsZero() && it.nextAttempt.After(now) {
			continue
		}
		candidates = append(candidates, it)
	}
	slices.SortFunc(candidates, compareItems)

	if !gate.Open {
		for _, it := range candidates {
			if it.state != Waiting || it.message != gate.Reason {
				it.state = Waiting
				it.message = gate.Reason
				s.e.dispatcher.emit(s.event(it))
			}
		}
		return nil, false
	}

	if len(candidates) == 0 {
		return nil, false
	}
	chosen := candidates[0]
	chosen.sending = true
	return chosen, true
}

// compareItems orders manual sends first, then by (ModTime, Name). It is a
// named function rather than an inline closure so both of the manual/manual
// tie-break's branches are directly, deterministically testable — a
// slices.SortFunc call over candidates built from Go's randomized map
// iteration order can leave one of the two symmetric branches uncovered on
// any given run.
func compareItems(a, b *item) int {
	if a.manual != b.manual {
		if a.manual {
			return -1
		}
		return 1
	}
	if c := a.modTime.Compare(b.modTime); c != 0 {
		return c
	}
	return strings.Compare(a.name, b.name)
}

// sendOne runs one upload attempt for it to completion (success or
// failure) and folds the result back into the queue.
func (s *scheduler) sendOne(ctx context.Context, it *item) {
	f, src, err := s.openForSend(it)
	if err != nil {
		s.preflightFailed(it, err)
		return
	}

	s.mu.Lock()
	it.size = src.size
	it.state = Sending
	it.message = msgSending
	ev := s.event(it)
	name, path, manual := it.name, it.path, it.manual
	s.mu.Unlock()
	s.e.dispatcher.emit(ev)

	coalescer := newProgressCoalescer(s.e.opts.ProgressInterval)
	acked := false
	progress := func(sent, total int64) {
		acked = true
		now := s.e.opts.Clock.Now()
		finished := sent >= total
		emit := coalescer.allow(now, sent, total)
		if finished {
			emit = coalescer.finish(now, sent, total)
		}
		if !emit {
			return
		}
		s.e.dispatcher.emit(TransferEvent{
			Name: name, Path: path, Size: total, Sent: sent,
			State: Sending, Message: msgSending, Manual: manual, At: now,
		})
	}

	uploadErr := s.e.client.Upload(ctx, src.remoteDir, src.base, f, src.size, progress)

	// f must be closed before finishSend can archive it: on Windows,
	// opts.Open (winutil.OpenDenyWrite) holds a share-mode handle that
	// blocks a rename of the same path (ERROR_SHARING_VIOLATION) for as
	// long as it stays open, so archiveSent's Rename must never run while
	// this send's own read handle is still held.
	if closeErr := f.Close(); closeErr != nil {
		s.e.opts.Logger.Warn("engine: close file after send", "name", it.name, "error", closeErr)
	}

	var failMsg string
	if uploadErr != nil {
		wording := failureMessage(uploadErr)
		failMsg = wording.base
		if acked {
			failMsg = wording.acked
		}
	}
	s.finishSend(ctx, it, src, uploadErr, failMsg)
}

// sendSource is one send attempt's snapshot of where its item's file is —
// base in the OS-native folder dir under the watch folder root, at path —
// along with its size when opened and remoteDir, the controller folder it
// is uploaded into. Archiving uses this snapshot rather than the item,
// which a later watcher event may already have pointed somewhere else.
type sendSource struct {
	root, dir, base, path string
	size                  int64
	remoteDir             string
}

// openForSend re-validates the file's name and folder, opens it deny-write,
// and stats it for the authoritative size, all immediately before
// uploading.
func (s *scheduler) openForSend(it *item) (*os.File, sendSource, error) {
	s.mu.Lock()
	name := it.name
	src := sendSource{root: it.root, dir: it.dir, base: it.base, path: it.path}
	s.mu.Unlock()

	remoteDir, err := uploadTarget(src.dir, src.base)
	if err != nil {
		return nil, sendSource{}, err
	}
	src.remoteDir = remoteDir
	f, err := s.e.opts.Open(src.path)
	if err != nil {
		return nil, sendSource{}, err
	}
	info, err := f.Stat()
	if err != nil {
		if closeErr := f.Close(); closeErr != nil {
			s.e.opts.Logger.Warn("engine: close file after failed stat", "name", name, "error", closeErr)
		}
		return nil, sendSource{}, err
	}
	src.size = info.Size()
	return f, src, nil
}

// preflightFailed handles an error from openForSend: a vanished file is
// forgotten entirely; a name or folder the controller cannot take is
// Rejected again; anything else (most commonly ErrBusy) leaves the item
// Pending, retried on the next watcher emission or after Backoff[0].
func (s *scheduler) preflightFailed(it *item, err error) {
	s.mu.Lock()
	it.sending = false
	// A resendAfter set during the unlocked open above (a concurrent
	// ready()/sendFile() saw sending==true and deferred instead of
	// resetting the item directly) is moot: this send never got far
	// enough to need a resend, and the Pending/orphaned handling below
	// already re-arms or drops the item — leaving resendAfter set would
	// make some later finishSend skip archiving as though a real resend
	// were pending.
	it.resendAfter = false

	if it.orphaned {
		// clearNonManual left this item in place only to let the
		// in-flight open settle without aborting it; now that it has,
		// drop it exactly as finishSend does, rather than resurrecting
		// it as a live Pending candidate pointing at a path the watch
		// folder already moved away from.
		s.dropOrphanLocked(it)
		s.mu.Unlock()
		return
	}

	if errors.Is(err, masso.ErrBadFileName) || errors.Is(err, masso.ErrBadUploadDir) {
		it.state = Rejected
		it.message = err.Error()
		it.nextAttempt = time.Time{}
		ev := s.event(it)
		s.mu.Unlock()
		s.e.dispatcher.emit(ev)
		return
	}

	if errors.Is(err, fs.ErrNotExist) {
		delete(s.items, it.name)
		s.mu.Unlock()
		s.e.opts.Logger.Info("engine: file vanished before send; forgetting it", "name", it.name)
		return
	}

	it.state = Pending
	it.message = msgWaitingToSend
	it.nextAttempt = s.e.opts.Clock.Now().Add(s.e.opts.Backoff[0])
	ev := s.event(it)
	s.mu.Unlock()

	s.e.opts.Logger.Info("engine: could not open file for send; will retry", "name", it.name, "error", err)
	s.e.dispatcher.emit(ev)
}

// finishSend folds an upload attempt's outcome back into the queue: archive
// src on success, record the failure on failure (failMsg is the caller's
// already-resolved wording — see failureMessage — and is ignored when
// uploadErr is nil), then — if a Changed arrived mid-transfer — immediately
// re-queue for a resend.
func (s *scheduler) finishSend(ctx context.Context, it *item, src sendSource, uploadErr error, failMsg string) {
	s.mu.Lock()
	resend := it.resendAfter
	it.resendAfter = false
	s.mu.Unlock()

	// it.sending stays true through the switch below, on purpose: a
	// concurrent ready()/sendFile() for this same name arriving while
	// archiveSent is still mid-Rename must still see sending==true and
	// defer via resendAfter (as it would during the upload itself),
	// rather than resetting it.path/state directly underneath the
	// archive step and risking archiveSent filing away content that was
	// never actually sent. next()/nextDeadline() cannot run concurrently
	// with this — run's loop only ever calls them between sends, never
	// during one — so leaving sending true a little longer than the
	// upload itself is safe.
	switch {
	case uploadErr != nil:
		s.recordFailure(it, uploadErr, failMsg)
	case resend:
		// The file changed again before this send even finished, so
		// whatever now sits at it.path is the newer content the resend
		// is about to pick up — archiving would move that away under
		// the just-sent name and starve the resend of anything to read.
		// The archive step only ever runs for the final, unsuperseded
		// send of a given name.
		s.setTerminal(it, Sent, "File sent")
	default:
		s.archiveSent(ctx, it, src)
	}

	// Only a successful send earns an immediate re-arm to Pending: a
	// failed send has already been recorded above, with its own backoff
	// and failCount, by recordFailure — forcing it back to Pending here
	// too would silently discard that backoff, letting a file that keeps
	// getting rewritten while it keeps failing (no USB stick, say) hammer
	// at full speed forever instead of backing off. Once the backoff
	// elapses, next() picks the item up again with whatever
	// path/size/modTime ready() already updated in place, so nothing is
	// lost by not resetting it here.
	if resend && uploadErr == nil {
		s.mu.Lock()
		it.sending = false
		// clearNonManual left this item in place, orphaned, only to let
		// the in-flight transfer finish without aborting it: a queued
		// resend belongs to the watch folder SetWatchDir already moved
		// away from, so it must not be resurrected as a live Pending
		// candidate pointing at that stale path (see finding 4) — drop it
		// instead, exactly as the non-resend path already does below.
		if it.orphaned {
			s.dropOrphanLocked(it)
			s.mu.Unlock()
			return
		}
		it.state = Pending
		it.message = msgWaitingToSend
		it.failCount = 0
		it.nextAttempt = time.Time{}
		ev := s.event(it)
		s.mu.Unlock()
		s.e.dispatcher.emit(ev)
		s.poke()
		return
	}

	// clearNonManual left this item in place only because it was Sending;
	// now that it has fully settled with no further resend queued, drop
	// it — it belongs to a watch folder SetWatchDir already moved away
	// from.
	s.mu.Lock()
	it.sending = false
	if it.orphaned {
		s.dropOrphanLocked(it)
	}
	s.mu.Unlock()
}

// dropOrphanLocked removes an orphaned item from the queue, unless
// itemLocked has already replaced it there. Callers must hold s.mu.
func (s *scheduler) dropOrphanLocked(it *item) {
	if s.items[it.name] == it {
		delete(s.items, it.name)
	}
}

// recordFailure applies the backoff schedule (or disables auto-retry for
// ErrCanceled by simply never setting a nextAttempt) and emits Failed with
// msg, which the caller has already resolved via failureMessage.
func (s *scheduler) recordFailure(it *item, uploadErr error, msg string) {
	s.mu.Lock()
	it.state = Failed
	it.message = msg
	if errors.Is(uploadErr, masso.ErrCanceled) {
		it.nextAttempt = time.Time{}
	} else {
		it.failCount++
		d := s.e.opts.Backoff[min(it.failCount-1, len(s.e.opts.Backoff)-1)]
		it.nextAttempt = s.e.opts.Clock.Now().Add(d)
	}
	ev := s.event(it)
	s.mu.Unlock()

	s.e.opts.Logger.Info("engine: upload failed", "name", it.name, "error", uploadErr)
	s.e.dispatcher.emit(ev)
}

// failureWording is failureMessage's result: base is the message as reported
// when the transfer never got as far as an ACK'd start; acked is what to
// report instead once it did (the file may now be incomplete on the
// controller's USB drive). Returning both, rather than taking a bool
// parameter for which one to produce, keeps failureMessage itself free of a
// control-flag parameter.
type failureWording struct {
	base  string
	acked string
}

// failureMessage maps a masso upload error to Masso Link's exact wording.
// Every masso sentinel error's own Error() text already is that wording
// (see internal/masso/errors.go's doc comment); only ErrCanceled needs a
// different acked-suffix than the generic incompleteSuffix.
func failureMessage(err error) failureWording {
	msg := operatorText(err)
	switch {
	case errors.Is(err, masso.ErrCanceled):
		return failureWording{msg, msg + "; resend it before running it"}
	default:
		return failureWording{msg, msg + incompleteSuffix}
	}
}

// operatorText returns the wording Masso Link itself shows for the
// controller's failure results, so what the operator reads here matches the
// manufacturer's documentation and forum posts. The masso sentinels carry
// the same text in Go's lowercase error convention; the capitalization and
// the "ERROR: " prefix are deliberate, not drift.
func operatorText(err error) string {
	switch {
	case errors.Is(err, masso.ErrNoUSB):
		return "No USB flash drive connected to Masso"
	case errors.Is(err, masso.ErrNoResponse):
		return "ERROR: No response from Masso"
	case errors.Is(err, masso.ErrCanceled):
		return "File transfer canceled by user on Masso"
	case errors.Is(err, masso.ErrUSBWrite):
		return "Unable to write file to USB"
	case errors.Is(err, masso.ErrTransfer):
		return "Error occurred while transferring file"
	default:
		return err.Error()
	}
}

// incompleteSuffix warns that a failure which happened after the start ACK
// may have left a partial file on the controller's USB drive.
const incompleteSuffix = " — the file on the Masso may be incomplete; it will be resent"
