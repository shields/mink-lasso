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

// Package watch watches a folder for G-code files a CAM post-processor has
// finished writing.
//
// A post-processor writes its output in place — there is no temp-file-plus-
// rename — so a create notification can arrive while the file is still being
// written, filesystem notifications can be dropped (SMB shares, a
// ReadDirectoryChangesW overflow) or unavailable (a UNC path), and mtimes on
// FAT32/exFAT only have two-second resolution. So the periodic directory
// listing, not any single notification, is authoritative: a file is only
// reported once its (size, mtime) has held steady across a scan interval and
// a minimum number of scans, and a probe confirms nothing still has it open
// for write. Filesystem notifications, when available, only make that
// listing happen sooner.
package watch

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"msrl.dev/mink-lasso/internal/clock"
	"msrl.dev/mink-lasso/internal/masso"
)

// ErrDirRequired is returned by New when Options.Dir is empty.
var ErrDirRequired = errors.New("watch: dir is required")

const (
	defaultInterval = 2 * time.Second
	defaultSettle   = 3 * time.Second
	defaultDebounce = 250 * time.Millisecond
)

// File is a candidate G-code file whose (size, mtime) has settled: it has
// stopped changing for long enough, and across enough scans, that the
// post-processor is presumed done writing it. It is a trigger, not a
// guarantee — a caller that acts on it (uploading, say) should re-verify the
// file immediately before use.
type File struct {
	Name    string
	Path    string
	Size    int64
	ModTime time.Time
	// Changed reports that this name was emitted before, at a different
	// (size, mtime): the file was overwritten after its earlier send.
	Changed bool
}

// Rejected is a settled file whose name failed Options.Validate.
type Rejected struct {
	Name   string
	Path   string
	Reason string
}

// Options configures a Watcher. Zero-value durations and nil function fields
// take documented defaults; only Dir is required.
type Options struct {
	// Dir is the folder to watch, non-recursively.
	Dir string

	// Interval is how often Dir is listed. Default 2s.
	Interval time.Duration
	// Settle is how long a file's (size, mtime) must hold steady before it
	// is considered done. Default 3s.
	Settle time.Duration
	// Extensions are the recognized file extensions, matched
	// case-insensitively and including the leading dot. Default:
	// masso.Extensions.
	Extensions []string

	// Validate, when set, rejects settled file names it does not accept
	// (returning the reason as an error). Nil accepts every name.
	Validate func(name string) error
	// Probe, when set, reports whether a settled file is still open
	// elsewhere (for example, for write). Nil reports never busy.
	Probe func(path string) (busy bool, err error)
	// IsRemote, when set, reports whether Dir is on a network share, where
	// filesystem notifications are unreliable. Nil reports false. A UNC
	// path is always treated as remote regardless of what IsRemote returns.
	IsRemote func(dir string) bool
	// NewNotifier constructs the filesystem-notification source. Default
	// NewFSNotifier. When it returns an error, or when the directory is
	// remote, the Watcher scans on Interval alone.
	NewNotifier func(dir string) (Notifier, error)
	// ReadDir lists Dir. Default os.ReadDir.
	ReadDir func(dir string) ([]fs.DirEntry, error)

	// Clock supplies all timing. Default clock.Real{}.
	Clock clock.Clock
	// Logger receives diagnostic output. Default a discarding logger.
	Logger *slog.Logger

	// Debounce is how long the Watcher waits after a filesystem
	// notification, to let a burst of events settle, before scanning early.
	// Default 250ms.
	Debounce time.Duration
}

// Watcher watches Options.Dir for settled G-code files. A Watcher is only
// useful after Run has been started.
type Watcher struct {
	opts Options
	exts map[string]bool

	ready    chan File
	rejected chan Rejected
	rescan   chan struct{}

	// scanHook, when set by a test, runs synchronously after every scan
	// (including one that finds nothing to do) so a test can wait for a
	// scan to finish without sleeping or guessing at timing.
	scanHook func()
}

// New validates opts and returns a Watcher. It does not start scanning; call
// Run for that.
func New(opts Options) (*Watcher, error) {
	if opts.Dir == "" {
		return nil, ErrDirRequired
	}
	if opts.Interval <= 0 {
		opts.Interval = defaultInterval
	}
	if opts.Settle <= 0 {
		opts.Settle = defaultSettle
	}
	if opts.Debounce <= 0 {
		opts.Debounce = defaultDebounce
	}
	if len(opts.Extensions) == 0 {
		opts.Extensions = masso.Extensions
	}
	if opts.Validate == nil {
		opts.Validate = func(string) error { return nil }
	}
	if opts.Probe == nil {
		opts.Probe = func(string) (bool, error) { return false, nil }
	}
	if opts.IsRemote == nil {
		opts.IsRemote = func(string) bool { return false }
	}
	if opts.NewNotifier == nil {
		opts.NewNotifier = NewFSNotifier
	}
	if opts.ReadDir == nil {
		opts.ReadDir = os.ReadDir
	}
	if opts.Clock == nil {
		opts.Clock = clock.Real{}
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}

	exts := make(map[string]bool, len(opts.Extensions))
	for _, e := range opts.Extensions {
		exts[strings.ToLower(e)] = true
	}

	return &Watcher{
		opts:     opts,
		exts:     exts,
		ready:    make(chan File),
		rejected: make(chan Rejected),
		rescan:   make(chan struct{}, 1),
	}, nil
}

// Ready delivers settled files in (mtime, name) order.
func (w *Watcher) Ready() <-chan File { return w.ready }

// Rejected delivers settled files whose name Options.Validate rejected.
func (w *Watcher) Rejected() <-chan Rejected { return w.rejected }

// Rescan asks the Watcher to list Dir soon, coalescing with any rescan
// already pending. Callers use it after an out-of-band change, such as the
// engine moving a sent file out of Dir.
func (w *Watcher) Rescan() {
	select {
	case w.rescan <- struct{}{}:
	default:
	}
}

// Run scans Dir until ctx is canceled, delivering settled files on Ready and
// rejected names on Rejected. It returns ctx.Err() and closes the
// notification source, if one was started.
func (w *Watcher) Run(ctx context.Context) error {
	notifier := w.startNotifier()
	if notifier != nil {
		defer func() {
			if err := notifier.Close(); err != nil {
				w.opts.Logger.Warn("watch: could not close notifier", "dir", w.opts.Dir, "error", err)
			}
		}()
	}

	entries := make(map[string]*entry)

	ticker := w.opts.Clock.NewTicker(w.opts.Interval)
	defer ticker.Stop()

	var debounce clock.Timer
	var debounceC <-chan time.Time
	defer func() {
		if debounce != nil {
			debounce.Stop()
		}
	}()

	for {
		var notifyEvents <-chan struct{}
		var notifyErrors <-chan error
		if notifier != nil {
			notifyEvents = notifier.Events()
			notifyErrors = notifier.Errors()
		}

		select {
		case <-ctx.Done():
			return ctx.Err()

		case <-ticker.C():
			w.scan(ctx, entries)

		case <-notifyEvents:
			if debounce == nil {
				debounce = w.opts.Clock.NewTimer(w.opts.Debounce)
			} else {
				debounce.Reset(w.opts.Debounce)
			}
			debounceC = debounce.C()

		case err := <-notifyErrors:
			w.opts.Logger.Debug("watch: notifier error; rescanning", "dir", w.opts.Dir, "error", err)
			w.scan(ctx, entries)

		case <-debounceC:
			debounceC = nil
			w.scan(ctx, entries)

		case <-w.rescan:
			w.scan(ctx, entries)
		}
	}
}

// startNotifier decides whether Dir can be watched for filesystem
// notifications and, if so, starts doing it. It falls back to nil (timer-only
// scanning) for a remote directory or when NewNotifier fails, logging once
// either way.
func (w *Watcher) startNotifier() Notifier {
	dir := w.opts.Dir
	if isUNC(dir) || w.opts.IsRemote(dir) {
		w.opts.Logger.Info("watch: directory is remote; scanning on a timer only", "dir", dir)
		return nil
	}

	n, err := w.opts.NewNotifier(dir)
	if err != nil {
		w.opts.Logger.Warn("watch: could not start filesystem notifications; scanning on a timer only",
			"dir", dir, "error", err)
		return nil
	}
	return n
}

// isUNC reports whether dir is a Windows UNC path (\\server\share\...),
// which fsnotify cannot watch reliably. This is a plain string check —
// rather than filepath.VolumeName, which only recognizes UNC syntax when
// compiled for GOOS=windows — so Options.IsRemote's "a UNC path is always
// treated as remote" guarantee actually holds on every platform, not only
// when built for Windows (matching the same portable check
// internal/winutil's isUNC uses).
func isUNC(dir string) bool {
	return strings.HasPrefix(dir, `\\`)
}

// entry is a Watcher's per-file bookkeeping, keyed by name. Nothing here
// persists across a Run: the directory listing is the only source of truth,
// so a file that vanishes from it is simply forgotten.
type entry struct {
	size    int64
	modTime time.Time

	// stableSince and stableScans track how long and how many scans the
	// current (size, mtime) has held.
	stableSince time.Time
	stableScans int

	// emitted and rejected record the (size, mtime) at which this name was
	// last sent on Ready or Rejected, so neither fires twice for the same
	// content and a later change is detected as a change rather than a
	// first-time event.
	emitted        bool
	emittedSize    int64
	emittedModTime time.Time

	rejected        bool
	rejectedSize    int64
	rejectedModTime time.Time
}

// scan lists Dir once, updates entries, and delivers any newly settled or
// newly rejected files. It never blocks longer than ctx allows.
func (w *Watcher) scan(ctx context.Context, entries map[string]*entry) {
	if w.scanHook != nil {
		defer w.scanHook()
	}

	dirEntries, err := w.opts.ReadDir(w.opts.Dir)
	if err != nil {
		w.opts.Logger.Warn("watch: could not list directory", "dir", w.opts.Dir, "error", err)
		return
	}

	now := w.opts.Clock.Now()
	seen := make(map[string]bool, len(dirEntries))
	var toEmit []File
	var toReject []Rejected

	for _, de := range dirEntries {
		name := de.Name()
		if !w.isCandidateName(name) {
			continue
		}

		info, err := de.Info()
		if err != nil {
			// The file may just have vanished between listing and stat;
			// leave its entry, if any, untouched and retry next scan.
			w.opts.Logger.Debug("watch: could not stat file", "name", name, "error", err)
			seen[name] = true
			continue
		}
		if !info.Mode().IsRegular() {
			continue
		}
		seen[name] = true

		e := updateEntry(entries, name, info, now)

		if e.emitted && e.emittedSize == e.size && e.emittedModTime.Equal(e.modTime) {
			continue
		}
		if e.rejected && e.rejectedSize == e.size && e.rejectedModTime.Equal(e.modTime) {
			continue
		}
		if e.stableScans < 2 || now.Sub(e.stableSince) < w.opts.Settle {
			continue
		}

		if file, rejected, ok := w.settle(w.opts.Dir, name, e); ok {
			toEmit = append(toEmit, file)
		} else if rejected != nil {
			toReject = append(toReject, *rejected)
		}
	}

	for name := range entries {
		if !seen[name] {
			delete(entries, name)
		}
	}

	slices.SortFunc(toEmit, func(a, b File) int {
		if c := a.ModTime.Compare(b.ModTime); c != 0 {
			return c
		}
		return strings.Compare(a.Name, b.Name)
	})
	slices.SortFunc(toReject, func(a, b Rejected) int { return strings.Compare(a.Name, b.Name) })

	for _, f := range toEmit {
		select {
		case w.ready <- f:
		case <-ctx.Done():
			return
		}
	}
	for _, r := range toReject {
		select {
		case w.rejected <- r:
		case <-ctx.Done():
			return
		}
	}
}

// updateEntry refreshes and returns entries[name] from a freshly stat'd,
// already-filtered candidate, resetting its stability tracking whenever
// (size, mtime) changed.
func updateEntry(entries map[string]*entry, name string, info fs.FileInfo, now time.Time) *entry {
	e, ok := entries[name]
	if !ok {
		e = &entry{}
		entries[name] = e
	}

	size, modTime := info.Size(), info.ModTime()
	if e.size != size || !e.modTime.Equal(modTime) {
		e.size = size
		e.modTime = modTime
		e.stableSince = now
		e.stableScans = 1
	} else {
		e.stableScans++
	}
	return e
}

// settle probes and, if free, validates a candidate whose (size, mtime) has
// already held long enough to be considered settled, and records the
// outcome. ok is true for a file ready to emit; a non-nil rejected reports a
// validation failure instead.
func (w *Watcher) settle(dir, name string, e *entry) (file File, rejected *Rejected, ok bool) {
	path := filepath.Join(dir, name)

	busy, err := w.opts.Probe(path)
	if err != nil {
		w.opts.Logger.Warn("watch: probe failed; treating as busy", "name", name, "error", err)
		busy = true
	}
	if busy {
		return File{}, nil, false
	}

	if verr := w.opts.Validate(name); verr != nil {
		e.rejected = true
		e.rejectedSize = e.size
		e.rejectedModTime = e.modTime
		return File{}, &Rejected{Name: name, Path: path, Reason: verr.Error()}, false
	}

	changed := e.emitted
	e.emitted = true
	e.emittedSize = e.size
	e.emittedModTime = e.modTime
	return File{Name: name, Path: path, Size: e.size, ModTime: e.modTime, Changed: changed}, nil, true
}

// isCandidateName reports whether name is a file this Watcher tracks at all,
// based on its name alone (no I/O): a recognized extension, not a partial or
// hidden file. This also keeps the sent/ archive directory out of
// consideration, since directory names typically have no matching extension
// — and even one that did would still be excluded once stat'd, for not being
// a regular file.
func (w *Watcher) isCandidateName(name string) bool {
	if strings.HasPrefix(name, "~$") || strings.HasPrefix(name, ".") {
		return false
	}
	lower := strings.ToLower(name)
	if strings.HasSuffix(lower, ".tmp") {
		return false
	}
	return w.exts[filepath.Ext(lower)]
}
