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

package watch

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"msrl.dev/mink-lasso/internal/clock"
	"msrl.dev/mink-lasso/internal/masso"
)

const (
	testInterval = time.Second
	testSettle   = 3 * time.Second
	testDebounce = 250 * time.Millisecond
)

// noNotifier always fails to start, putting the Watcher in timer-only mode
// so tests that aren't about notifications don't depend on one.
func noNotifier(string) (Notifier, error) {
	return nil, errors.New("notifier disabled for this test")
}

// harness runs a Watcher against a fake clock and gives tests deterministic
// ways to wait for it: waitScan blocks until one scan has fully finished
// (including any emission it made), so a caller that doesn't expect an
// emission can safely check Ready/Rejected afterward without racing it.
type harness struct {
	t       *testing.T
	w       *Watcher
	clk     *clock.Fake
	scanned chan struct{}
	done    chan error
}

func newHarness(t *testing.T, opts Options) *harness {
	t.Helper()

	clk := clock.NewFake(time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC))
	opts.Clock = clk
	if opts.NewNotifier == nil {
		opts.NewNotifier = noNotifier
	}

	w, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	scanned := make(chan struct{}, 4096)
	w.scanHook = func() { scanned <- struct{}{} }

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()
	clk.BlockUntil(1) // the interval ticker is registered

	h := &harness{t: t, w: w, clk: clk, scanned: scanned, done: done}
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("Run did not return after context cancel")
		}
	})
	return h
}

func (h *harness) waitScan() {
	h.t.Helper()
	select {
	case <-h.scanned:
	case <-time.After(5 * time.Second):
		h.t.Fatal("timed out waiting for a scan")
	}
}

func (h *harness) assertNoFile() {
	h.t.Helper()
	select {
	case f := <-h.w.Ready():
		h.t.Fatalf("unexpected Ready(): %+v", f)
	default:
	}
}

func (h *harness) assertNoRejected() {
	h.t.Helper()
	select {
	case r := <-h.w.Rejected():
		h.t.Fatalf("unexpected Rejected(): %+v", r)
	default:
	}
}

// tickQuiet advances the clock by one scan interval, waits for the
// resulting scan to finish, and asserts it produced nothing.
func (h *harness) tickQuiet() {
	h.t.Helper()
	h.clk.Advance(testInterval)
	h.waitScan()
	h.assertNoFile()
	h.assertNoRejected()
}

func (h *harness) recvFile() File {
	h.t.Helper()
	select {
	case f := <-h.w.Ready():
		return f
	case <-time.After(5 * time.Second):
		h.t.Fatal("timed out waiting for Ready()")
		return File{}
	}
}

func (h *harness) recvRejected() Rejected {
	h.t.Helper()
	select {
	case r := <-h.w.Rejected():
		return r
	case <-time.After(5 * time.Second):
		h.t.Fatal("timed out waiting for Rejected()")
		return Rejected{}
	}
}

func writeFile(t *testing.T, path string, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile(%s): %v", path, err)
	}
}

func TestNewRequiresDir(t *testing.T) {
	t.Parallel()
	if _, err := New(Options{}); !errors.Is(err, ErrDirRequired) {
		t.Fatalf("New({}) error = %v, want %v", err, ErrDirRequired)
	}
}

func TestNewDefaults(t *testing.T) {
	t.Parallel()

	w, err := New(Options{Dir: "/tmp/example"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if w.opts.Interval != defaultInterval {
		t.Errorf("Interval = %v, want %v", w.opts.Interval, defaultInterval)
	}
	if w.opts.Settle != defaultSettle {
		t.Errorf("Settle = %v, want %v", w.opts.Settle, defaultSettle)
	}
	if w.opts.Debounce != defaultDebounce {
		t.Errorf("Debounce = %v, want %v", w.opts.Debounce, defaultDebounce)
	}
	if len(w.opts.Extensions) != len(masso.Extensions) {
		t.Errorf("Extensions = %v, want %v", w.opts.Extensions, masso.Extensions)
	}
	if err := w.opts.Validate("anything.nc"); err != nil {
		t.Errorf("default Validate rejected: %v", err)
	}
	if busy, err := w.opts.Probe("/some/path"); busy || err != nil {
		t.Errorf("default Probe = (%v, %v), want (false, nil)", busy, err)
	}
	if w.opts.IsRemote("/some/dir") {
		t.Error("default IsRemote = true, want false")
	}
	if w.opts.NewNotifier == nil {
		t.Error("default NewNotifier is nil")
	}
	if w.opts.ReadDir == nil {
		t.Error("default ReadDir is nil")
	}
	if w.opts.Clock == nil {
		t.Error("default Clock is nil")
	}
	if w.opts.Logger == nil {
		t.Error("default Logger is nil")
	}
}

func TestRunReturnsContextErrOnCancel(t *testing.T) {
	t.Parallel()

	clk := clock.NewFake(time.Now())
	w, err := New(Options{Dir: t.TempDir(), Clock: clk, NewNotifier: noNotifier})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()
	clk.BlockUntil(1)

	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run() error = %v, want %v", err, context.Canceled)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after context cancel")
	}
}

// runToBlockedScan drives w through however many quiet scans Settle/Interval
// need — synchronized deterministically via scanHook, exactly as
// settleFile does — and then fires the final, settling scan without anyone
// reading Ready or Rejected afterward. That scan is therefore left blocked
// inside scan's emit loop, on whichever channel it has something for,
// letting a test exercise ctx cancellation while a send is pending.
func runToBlockedScan(ctx context.Context, t *testing.T, w *Watcher, clk *clock.Fake) chan error {
	t.Helper()

	scanned := make(chan struct{}, 64)
	w.scanHook = func() { scanned <- struct{}{} }

	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()
	clk.BlockUntil(1)

	quiet := func() {
		clk.Advance(testInterval)
		select {
		case <-scanned:
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for a scan")
		}
	}
	for range int(testSettle / testInterval) {
		quiet()
	}
	clk.Advance(testInterval) // the settling scan: left blocked on its own send

	return done
}

func TestScanAbortsEmitOnContextCancel(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "job.nc"), "G0")

	clk := clock.NewFake(time.Now())
	w, err := New(Options{Dir: dir, Interval: testInterval, Settle: testSettle, Clock: clk, NewNotifier: noNotifier})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	done := runToBlockedScan(ctx, t, w, clk)

	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run() error = %v, want %v", err, context.Canceled)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancel while blocked emitting to Ready")
	}
}

func TestScanAbortsRejectOnContextCancel(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "job.nc"), "G0")

	validate := func(string) error { return errors.New("bad name") }
	clk := clock.NewFake(time.Now())
	w, err := New(Options{
		Dir: dir, Interval: testInterval, Settle: testSettle, Clock: clk,
		NewNotifier: noNotifier, Validate: validate,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	done := runToBlockedScan(ctx, t, w, clk)

	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run() error = %v, want %v", err, context.Canceled)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancel while blocked emitting to Rejected")
	}
}

func TestSlowWriterWaitsForSettle(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "job.nc")

	h := newHarness(t, Options{Dir: dir, Interval: testInterval, Settle: testSettle})

	writeFile(t, path, "G0 X0\n")
	h.tickQuiet() // scan 1: first sighting, stableScans=1

	writeFile(t, path, "G0 X0\nG1 X1\n") // still being written: (size, mtime) changes
	h.tickQuiet()                        // scan 2: change detected, stableScans resets to 1
	h.tickQuiet()                        // scan 3: stableScans=2, but only 1 interval elapsed (<3s Settle)
	h.tickQuiet()                        // scan 4: 2 intervals elapsed (<3s Settle)

	h.clk.Advance(testInterval) // scan 5: stableScans=4, 3s elapsed: settled
	f := h.recvFile()

	if f.Name != "job.nc" || f.Path != path {
		t.Fatalf("Ready() = %+v", f)
	}
	if f.Changed {
		t.Error("first emission should not be Changed")
	}
}

func TestBusyProbeBlocksThenReleases(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "job.nc")
	writeFile(t, path, "G0 X0\n")

	const (
		probeError = iota
		probeBusy
		probeFree
	)
	var probeState atomic.Int32
	probeState.Store(probeError)
	probeCalls := atomic.Int32{}

	probe := func(string) (bool, error) {
		probeCalls.Add(1)
		switch probeState.Load() {
		case probeError:
			return false, errors.New("probe failed")
		case probeBusy:
			return true, nil
		default:
			return false, nil
		}
	}

	h := newHarness(t, Options{Dir: dir, Interval: testInterval, Settle: testSettle, Probe: probe})

	// Reach the point where (size, mtime) has held for Settle across >= 2
	// scans, so every subsequent scan calls Probe.
	h.tickQuiet()
	h.tickQuiet()
	h.tickQuiet()
	if calls := probeCalls.Load(); calls != 0 {
		t.Fatalf("Probe called %d times before settle criteria were met", calls)
	}

	h.tickQuiet() // settle criteria now met; Probe errors -> treated as busy
	if calls := probeCalls.Load(); calls != 1 {
		t.Fatalf("Probe called %d times, want 1", calls)
	}

	probeState.Store(probeBusy)
	h.tickQuiet()
	if calls := probeCalls.Load(); calls != 2 {
		t.Fatalf("Probe called %d times, want 2", calls)
	}

	probeState.Store(probeFree)
	h.clk.Advance(testInterval)
	f := h.recvFile()
	if f.Name != "job.nc" {
		t.Fatalf("Ready() = %+v", f)
	}
}

func TestChangeAfterEmitReemitsWithChanged(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "job.nc")
	writeFile(t, path, "G0 X0\n")

	h := newHarness(t, Options{Dir: dir, Interval: testInterval, Settle: testSettle})

	settleFile(h)
	first := h.recvFile()
	h.waitScan() // drain the settling scan's own completion signal before ticking again

	if first.Changed {
		t.Error("first emission should not be Changed")
	}

	// The engine has not moved the file yet; the operator overwrites it.
	writeFile(t, path, "G0 X0\nG1 X1\nG1 Y1\n")

	settleFile(h)
	second := h.recvFile()
	if !second.Changed {
		t.Error("re-emission after a change should be Changed")
	}
	if second.Size == first.Size {
		t.Error("re-emission has the same size as the first emission")
	}
}

func TestVanishedFileForgotten(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "job.nc")
	writeFile(t, path, "G0 X0\n")

	h := newHarness(t, Options{Dir: dir, Interval: testInterval, Settle: testSettle})

	h.tickQuiet() // first sighting: stableScans=1, not yet settled

	if err := os.Remove(path); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	h.tickQuiet() // the file is gone: its entry is dropped, not just paused

	// Recreating it starts a fresh (size, mtime) history: had the old entry
	// survived, this could reach settled sooner or be flagged Changed.
	writeFile(t, path, "G0 X0\nG1 X1\n")
	settleFile(h)
	f := h.recvFile()
	if f.Changed {
		t.Error("a file recreated after vanishing should not be marked Changed")
	}
}

func TestRejectedSortOrder(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "zzz.nc"), "G0")
	writeFile(t, filepath.Join(dir, "aaa.nc"), "G0")

	validate := func(name string) error { return errors.New("bad: " + name) }
	h := newHarness(t, Options{Dir: dir, Interval: testInterval, Settle: testSettle, Validate: validate})

	settleFile(h)
	first := h.recvRejected()
	second := h.recvRejected()
	if first.Name != "aaa.nc" || second.Name != "zzz.nc" {
		t.Fatalf("Rejected order = %s, %s; want aaa.nc, zzz.nc", first.Name, second.Name)
	}
}

// settleFile advances a harness through however many quiet ticks the current
// test's Interval/Settle need for a stable (size, mtime) to become settled,
// asserting no emission along the way, then leaves the final settling tick
// for the caller to consume via recvFile/recvRejected. The settling scan's
// own scanHook signal fires only once its emission(s) have been read, so a
// caller that will tick the harness again afterward must drain it with one
// waitScan call once it has read everything that scan produced — otherwise
// that stale signal, not the next tickQuiet's own scan, is what a later
// waitScan drains.
func settleFile(h *harness) {
	h.t.Helper()
	// A (size, mtime) first seen at scan k=1 needs stableScans>=2 and
	// elapsed=(k-1)*Interval>=Settle; with Settle an exact multiple of
	// Interval that is first true at k = Settle/Interval + 1. So
	// Settle/Interval scans are quiet, and the next one settles.
	ticks := int(testSettle / testInterval)
	for range ticks {
		h.tickQuiet()
	}
	h.clk.Advance(testInterval)
}

func TestRejectedOnceThenAgainAfterChange(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "BADNAME.nc")
	writeFile(t, path, "G0 X0\n")

	validate := func(name string) error {
		return errors.New("name too long: " + name)
	}
	h := newHarness(t, Options{Dir: dir, Interval: testInterval, Settle: testSettle, Validate: validate})

	settleFile(h)
	r := h.recvRejected()
	h.waitScan()
	if r.Name != "BADNAME.nc" || r.Reason == "" {
		t.Fatalf("Rejected() = %+v", r)
	}

	// Same (size, mtime): must not be re-rejected.
	h.tickQuiet()
	h.tickQuiet()

	// A change makes it eligible again.
	writeFile(t, path, "G0 X0\nG1 X1\n")
	settleFile(h)
	r2 := h.recvRejected()
	if r2.Name != "BADNAME.nc" {
		t.Fatalf("Rejected() = %+v", r2)
	}
}

func TestExtensionFilterAndExclusions(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	writeFile(t, filepath.Join(dir, "good.nc"), "G0")
	writeFile(t, filepath.Join(dir, "upper.NC"), "G0")
	writeFile(t, filepath.Join(dir, "ignored.exe"), "G0")
	writeFile(t, filepath.Join(dir, "~$conflict.nc"), "G0")
	writeFile(t, filepath.Join(dir, ".hidden.nc"), "G0")
	writeFile(t, filepath.Join(dir, "partial.tmp"), "G0")
	writeFile(t, filepath.Join(dir, "partial.TMP"), "G0")
	if err := os.Mkdir(filepath.Join(dir, "sent"), 0o700); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	if err := os.Mkdir(filepath.Join(dir, "notafile.nc"), 0o700); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}

	h := newHarness(t, Options{Dir: dir, Interval: testInterval, Settle: testSettle})

	settleFile(h)
	got := map[string]bool{}
	for range 2 {
		f := h.recvFile()
		got[f.Name] = true
	}
	h.waitScan()
	h.tickQuiet()

	want := map[string]bool{"good.nc": true, "upper.NC": true}
	if len(got) != len(want) {
		t.Fatalf("emitted %v, want %v", got, want)
	}
	for name := range want {
		if !got[name] {
			t.Errorf("expected %s to be emitted", name)
		}
	}
}

func TestRescan(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	var calls atomic.Int32
	readDir := func(d string) ([]fs.DirEntry, error) {
		calls.Add(1)
		return os.ReadDir(d)
	}
	h := newHarness(t, Options{Dir: dir, Interval: testInterval, Settle: testSettle, ReadDir: readDir})

	h.w.Rescan()
	h.waitScan()
	if got := calls.Load(); got != 1 {
		t.Fatalf("ReadDir called %d times, want 1", got)
	}
}

// TestRescanCoalesces checks Rescan's own buffering, with the Watcher never
// run: nothing else can drain w.rescan, so back-to-back calls deterministically
// exercise the coalescing (already-pending) branch rather than racing Run.
func TestRescanCoalesces(t *testing.T) {
	t.Parallel()
	w, err := New(Options{Dir: t.TempDir()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	w.Rescan()
	w.Rescan()

	select {
	case <-w.rescan:
	default:
		t.Fatal("expected a pending rescan hint")
	}
	select {
	case <-w.rescan:
		t.Fatal("expected exactly one coalesced hint")
	default:
	}
}

func TestReadDirError(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "job.nc"), "G0")

	var fail atomic.Bool
	fail.Store(true)
	readDirErr := errors.New("permission denied")
	readDir := func(d string) ([]fs.DirEntry, error) {
		if fail.Load() {
			return nil, readDirErr
		}
		return os.ReadDir(d)
	}

	var logged memHandler
	h := newHarness(t, Options{
		Dir: dir, Interval: testInterval, Settle: testSettle,
		ReadDir: readDir, Logger: slog.New(&logged),
	})

	h.tickQuiet()
	if logged.count() == 0 {
		t.Error("expected a log line for the ReadDir error")
	}

	// The watcher keeps running: once ReadDir works, scanning resumes.
	fail.Store(false)
	settleFile(h)
	f := h.recvFile()
	if f.Name != "job.nc" {
		t.Fatalf("Ready() = %+v", f)
	}
}

// fakeDirEntry is an fs.DirEntry whose Info() fails, simulating a file that
// vanished between listing and stat.
type fakeDirEntry struct {
	name string
	err  error
}

func (f fakeDirEntry) Name() string               { return f.name }
func (fakeDirEntry) IsDir() bool                  { return false }
func (fakeDirEntry) Type() fs.FileMode            { return 0 }
func (f fakeDirEntry) Info() (fs.FileInfo, error) { return nil, f.err }

func TestDirEntryInfoError(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "job.nc"), "G0")

	statErr := errors.New("stat failed")
	var useFake atomic.Bool
	useFake.Store(true)
	readDir := func(d string) ([]fs.DirEntry, error) {
		if useFake.Load() {
			return []fs.DirEntry{fakeDirEntry{name: "broken.nc", err: statErr}}, nil
		}
		return os.ReadDir(d)
	}

	h := newHarness(t, Options{Dir: dir, Interval: testInterval, Settle: testSettle, ReadDir: readDir})

	h.tickQuiet()
	h.tickQuiet()
	h.tickQuiet()
	h.tickQuiet()

	// The watcher keeps running afterward.
	useFake.Store(false)
	settleFile(h)
	f := h.recvFile()
	if f.Name != "job.nc" {
		t.Fatalf("Ready() = %+v", f)
	}
}

func TestEmissionOrderByModTimeThenName(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	type spec struct {
		name string
		when time.Time
	}
	files := []spec{
		{"z.nc", base.Add(2 * time.Second)},
		{"a.nc", base.Add(2 * time.Second)}, // same mtime as z.nc: name breaks the tie
		{"newest.nc", base.Add(3 * time.Second)},
		{"oldest.nc", base},
	}
	for _, f := range files {
		path := filepath.Join(dir, f.name)
		writeFile(t, path, "G0")
		if err := os.Chtimes(path, f.when, f.when); err != nil {
			t.Fatalf("Chtimes(%s): %v", f.name, err)
		}
	}

	h := newHarness(t, Options{Dir: dir, Interval: testInterval, Settle: testSettle})

	settleFile(h)
	order := make([]string, 0, len(files))
	for range files {
		order = append(order, h.recvFile().Name)
	}
	h.waitScan()
	h.tickQuiet()

	want := []string{"oldest.nc", "a.nc", "z.nc", "newest.nc"}
	if len(order) != len(want) {
		t.Fatalf("order = %v, want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("order = %v, want %v", order, want)
		}
	}
}

// fakeNotifier is a Notifier a test controls directly.
type fakeNotifier struct {
	events   chan struct{}
	errors   chan error
	closed   atomic.Bool
	closeErr error
}

func newFakeNotifier() *fakeNotifier {
	return &fakeNotifier{events: make(chan struct{}, 1), errors: make(chan error, 1)}
}

func (n *fakeNotifier) Events() <-chan struct{} { return n.events }
func (n *fakeNotifier) Errors() <-chan error    { return n.errors }

func (n *fakeNotifier) Close() error {
	n.closed.Store(true)
	return n.closeErr
}

func TestNotifierCloseErrorIsLogged(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	fn := newFakeNotifier()
	fn.closeErr = errors.New("close failed")

	var logged memHandler
	clk := clock.NewFake(time.Now())
	w, err := New(Options{
		Dir: dir, Clock: clk, Logger: slog.New(&logged),
		NewNotifier: func(string) (Notifier, error) { return fn, nil },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()
	clk.BlockUntil(1)

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return")
	}

	if logged.count() == 0 {
		t.Error("expected a log line for the notifier close error")
	}
}

func TestNotifierHintDebouncesEarlyScan(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "job.nc"), "G0")

	fn := newFakeNotifier()
	var calls atomic.Int32
	readDir := func(d string) ([]fs.DirEntry, error) {
		calls.Add(1)
		return os.ReadDir(d)
	}

	h := newHarness(t, Options{
		Dir: dir, Interval: testInterval, Settle: testSettle, Debounce: testDebounce,
		ReadDir:     readDir,
		NewNotifier: func(string) (Notifier, error) { return fn, nil },
	})

	fn.events <- struct{}{}
	h.clk.BlockUntil(2) // ticker + debounce timer now registered

	// A second hint before the debounce fires resets it (rather than
	// scheduling a second one) and still coalesces to a single scan.
	// Nothing about sending to fn.events or calling Advance (a fake-clock
	// call the Watcher goroutine isn't blocked on) forces that goroutine
	// to run before this test proceeds, so without yielding here, Advance
	// below can reach and fire the still-unreset original deadline before
	// the Watcher goroutine ever receives this second hint; Reset would
	// then apply to an already-fired timer, rescheduling it a further
	// Debounce past the point this test actually advances to, and the
	// expected scan would never come. Yielding repeatedly gives the
	// runtime every opportunity to run that goroutine (through the
	// receive and the Reset call) before Advance runs.
	fn.events <- struct{}{}
	for range 1000 {
		runtime.Gosched()
	}

	h.clk.Advance(testDebounce)
	h.waitScan()

	if got := calls.Load(); got != 1 {
		t.Fatalf("ReadDir called %d times after the debounce fired, want 1", got)
	}
}

func TestNotifierErrorTriggersRescan(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	fn := newFakeNotifier()
	var calls atomic.Int32
	readDir := func(d string) ([]fs.DirEntry, error) {
		calls.Add(1)
		return os.ReadDir(d)
	}

	h := newHarness(t, Options{
		Dir: dir, Interval: testInterval, Settle: testSettle,
		ReadDir:     readDir,
		NewNotifier: func(string) (Notifier, error) { return fn, nil },
	})

	fn.errors <- errors.New("ErrEventOverflow-like failure")
	h.waitScan()

	if got := calls.Load(); got != 1 {
		t.Fatalf("ReadDir called %d times after a notifier error, want 1", got)
	}
}

func TestTimerOnlyModeOnNewNotifierFailure(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "job.nc"), "G0")

	var logged memHandler
	h := newHarness(t, Options{
		Dir: dir, Interval: testInterval, Settle: testSettle,
		Logger:      slog.New(&logged),
		NewNotifier: func(string) (Notifier, error) { return nil, errors.New("no notifications here") },
	})

	if got := logged.count(); got != 1 {
		t.Fatalf("log lines = %d, want 1", got)
	}

	// Timer-only mode still scans and settles files.
	settleFile(h)
	f := h.recvFile()
	if f.Name != "job.nc" {
		t.Fatalf("Ready() = %+v", f)
	}
}

func TestTimerOnlyModeOnIsRemote(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	var logged memHandler
	var newNotifierCalls atomic.Int32
	clk := clock.NewFake(time.Now())
	w, err := New(Options{
		Dir: dir, Clock: clk,
		Logger:   slog.New(&logged),
		IsRemote: func(string) bool { return true },
		NewNotifier: func(string) (Notifier, error) {
			newNotifierCalls.Add(1)
			return nil, errors.New("must not be called: IsRemote should short-circuit")
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()
	clk.BlockUntil(1)
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return")
	}

	if got := logged.count(); got != 1 {
		t.Fatalf("log lines = %d, want 1", got)
	}
	if got := newNotifierCalls.Load(); got != 0 {
		t.Fatalf("NewNotifier called %d times, want 0 (IsRemote should short-circuit it)", got)
	}
}

func TestIsUNC(t *testing.T) {
	t.Parallel()
	// A plain string-prefix check, so this holds on every platform, not
	// just when compiled for Windows.
	if got := isUNC(`\\server\share\dir`); !got {
		t.Errorf("isUNC(UNC path) = %v, want true", got)
	}
	if got := isUNC(`/tmp/example`); got {
		t.Errorf("isUNC(non-UNC path) = %v, want false", got)
	}
}

// memHandler is a minimal slog.Handler that counts records, for asserting
// "logged once" without depending on message text.
type memHandler struct {
	mu      sync.Mutex
	records []slog.Record
}

func (*memHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *memHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, r)
	return nil
}

func (h *memHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *memHandler) WithGroup(string) slog.Handler      { return h }

func (h *memHandler) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.records)
}
