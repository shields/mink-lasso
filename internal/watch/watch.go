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

// Package watch watches a folder, and its subfolders, for G-code files a CAM
// post-processor has finished writing.
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
// listing happen sooner, and only for the top folder: a change inside a
// subfolder waits for the next periodic listing.
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

// SentDir is the archive folder directly inside Options.Dir that a Watcher
// never descends into, matched case-insensitively since Windows and macOS
// filesystems are. The engine moves each sent file there.
const SentDir = "sent"

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
	// Name is the file's base name.
	Name string
	// Dir is the file's directory relative to Options.Dir, OS-native, ""
	// for a file directly in Options.Dir.
	Dir     string
	Path    string
	Size    int64
	ModTime time.Time
	// Changed reports that this file was emitted before, at a different
	// (size, mtime): it was overwritten after its earlier send.
	Changed bool
}

// Rejected is a settled file that Options.Validate rejected, for its name
// or its directory.
type Rejected struct {
	// Name is the file's base name.
	Name string
	// Dir is the file's directory relative to Options.Dir, as in File.
	Dir    string
	Path   string
	Reason string
}

// Options configures a Watcher. Zero-value durations and nil function fields
// take documented defaults; only Dir is required.
type Options struct {
	// Dir is the folder to watch. Its subfolders are watched too, except
	// SentDir at the top level and, at any depth, a folder whose name
	// starts with "." or "~$" or that Hidden reports hidden. Symbolic links
	// are not followed.
	Dir string

	// Interval is how often Dir and its subfolders are listed. Default 2s.
	Interval time.Duration
	// Settle is how long a file's (size, mtime) must hold steady before it
	// is considered done. Default 3s.
	Settle time.Duration
	// Extensions are the recognized file extensions, matched
	// case-insensitively and including the leading dot. Default:
	// masso.Extensions.
	Extensions []string

	// Validate, when set, rejects settled files whose relative directory
	// (as in File.Dir) or base name it does not accept, returning the
	// reason as an error. Nil accepts every file.
	Validate func(dir, name string) error
	// Hidden, when set, reports whether the subfolder at path is hidden by
	// something other than its name, such as a Windows file attribute.
	// Nil reports never hidden.
	Hidden func(path string) bool
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
	// ReadDir lists Dir and each subfolder. Default os.ReadDir.
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

	// unlistable holds the relative paths of the subfolders the last
	// complete scan could not list, so a folder that stays unlistable is
	// logged once rather than on every scan. Only Run's goroutine uses it.
	unlistable map[string]bool
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
		opts.Validate = func(string, string) error { return nil }
	}
	if opts.Hidden == nil {
		opts.Hidden = func(string) bool { return false }
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

// Ready delivers settled files in (mtime, relative path) order.
func (w *Watcher) Ready() <-chan File { return w.ready }

// Rejected delivers settled files that Options.Validate rejected, in
// relative path order.
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

// entry is a Watcher's per-file bookkeeping, keyed by its path relative to
// Options.Dir. Nothing here persists across a Run: the directory listing is
// the only source of truth, so a file that vanishes from it is simply
// forgotten.
type entry struct {
	size    int64
	modTime time.Time

	// stableSince and stableScans track how long and how many scans the
	// current (size, mtime) has held.
	stableSince time.Time
	stableScans int

	// emitted and rejected record the (size, mtime) at which this file was
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

type scanState struct {
	entries map[string]*entry
	now     time.Time
	seen    map[string]bool
	// unlisted holds the relative paths of subfolders that could not be
	// listed; entries under them are kept as they are.
	unlisted []string
	toEmit   []File
	toReject []Rejected
}

// scan lists Dir and its subfolders once, updates entries, and delivers any
// newly settled or newly rejected files. Once ctx is done it descends into
// no further subfolders and delivers nothing more, though a ReadDir,
// Hidden, or Probe call already under way runs to completion.
func (w *Watcher) scan(ctx context.Context, entries map[string]*entry) {
	if w.scanHook != nil {
		defer w.scanHook()
	}

	dirEntries, err := w.opts.ReadDir(w.opts.Dir)
	if err != nil {
		w.opts.Logger.Warn("watch: could not list directory", "dir", w.opts.Dir, "error", err)
		return
	}

	st := &scanState{entries: entries, now: w.opts.Clock.Now(), seen: make(map[string]bool)}
	w.scanDir(ctx, st, "", dirEntries)

	w.unlistable = make(map[string]bool, len(st.unlisted))
	for _, relDir := range st.unlisted {
		w.unlistable[relDir] = true
	}
	for key := range entries {
		if !st.seen[key] && !underAny(key, st.unlisted) {
			delete(entries, key)
		}
	}

	// Every Path is Dir joined with the relative path, so ordering by Path
	// orders by relative path.
	slices.SortFunc(st.toEmit, func(a, b File) int {
		if c := a.ModTime.Compare(b.ModTime); c != 0 {
			return c
		}
		return strings.Compare(a.Path, b.Path)
	})
	slices.SortFunc(st.toReject, func(a, b Rejected) int { return strings.Compare(a.Path, b.Path) })

	for _, f := range st.toEmit {
		select {
		case w.ready <- f:
		case <-ctx.Done():
			return
		}
	}
	for _, r := range st.toReject {
		select {
		case w.rejected <- r:
		case <-ctx.Done():
			return
		}
	}
}

func (w *Watcher) scanDir(ctx context.Context, st *scanState, relDir string, dirEntries []fs.DirEntry) {
	for _, de := range dirEntries {
		name := de.Name()
		if de.IsDir() {
			sub := filepath.Join(relDir, name)
			if ctx.Err() == nil && walkedDir(relDir, name) && !w.opts.Hidden(filepath.Join(w.opts.Dir, sub)) {
				w.scanSubdir(ctx, st, sub)
			}
			continue
		}
		if !w.isCandidateName(name) {
			continue
		}
		key := filepath.Join(relDir, name)

		info, err := de.Info()
		if err != nil {
			// The file may just have vanished between listing and stat;
			// leave its entry, if any, untouched and retry next scan.
			w.opts.Logger.Debug("watch: could not stat file", "name", key, "error", err)
			st.seen[key] = true
			continue
		}
		if !info.Mode().IsRegular() {
			continue
		}
		st.seen[key] = true

		e := updateEntry(st.entries, key, info, st.now)

		if e.emitted && e.emittedSize == e.size && e.emittedModTime.Equal(e.modTime) {
			continue
		}
		if e.rejected && e.rejectedSize == e.size && e.rejectedModTime.Equal(e.modTime) {
			continue
		}
		if e.stableScans < 2 || st.now.Sub(e.stableSince) < w.opts.Settle {
			continue
		}

		if file, rejected, ok := w.settle(relDir, name, e); ok {
			st.toEmit = append(st.toEmit, file)
		} else if rejected != nil {
			st.toReject = append(st.toReject, *rejected)
		}
	}
}

// scanSubdir lists the subfolder at relDir and scans it. A folder that
// cannot be listed is recorded in st.unlisted, so its existing entries are
// neither forgotten nor re-emitted, and logged only when it starts or stops
// failing: one that stays unlistable would otherwise log on every scan.
func (w *Watcher) scanSubdir(ctx context.Context, st *scanState, relDir string) {
	dir := filepath.Join(w.opts.Dir, relDir)
	dirEntries, err := w.opts.ReadDir(dir)
	if err != nil {
		if !w.unlistable[relDir] {
			w.opts.Logger.Warn("watch: could not list directory", "dir", dir, "error", err)
		}
		st.unlisted = append(st.unlisted, relDir)
		return
	}
	if w.unlistable[relDir] {
		w.opts.Logger.Info("watch: can list directory again", "dir", dir)
	}
	w.scanDir(ctx, st, relDir, dirEntries)
}

func walkedDir(relDir, name string) bool {
	if skippedName(name) {
		return false
	}
	return relDir != "" || !strings.EqualFold(name, SentDir)
}

// skippedName reports whether name marks a hidden entry or an Office-style
// "~$" lock file, which the Watcher ignores whether file or folder.
func skippedName(name string) bool {
	return strings.HasPrefix(name, ".") || strings.HasPrefix(name, "~$")
}

// RelDir reports the folder a Watcher rooted at dir would find path's file
// in — relative to dir, OS-native, "" for the top, exactly as File.Dir
// reports it — and whether that folder is one the Watcher actually walks:
// not SentDir at the top level, not a "." or "~$" folder at any depth, and
// not a folder hidden (nil taken as never hidden) reports hidden. It
// reports ok=false for a path outside dir entirely, for dir itself (a
// folder, not a file), and for a file under a folder the Watcher skips.
// Path components are compared with strings.EqualFold, since Windows and
// macOS filesystems are case-insensitive — matching SentDir's own match —
// and both dir and path are cleaned first so an equivalent but
// differently-formed path still resolves the same way. The engine uses it
// to route a manual send as the watcher would.
func RelDir(dir, path string, hidden func(string) bool) (relDir string, ok bool) {
	if hidden == nil {
		hidden = func(string) bool { return false }
	}

	sep := string(filepath.Separator)
	// A filesystem root such as E:\ or / keeps its separator when
	// cleaned, which would otherwise split into a trailing empty
	// component that no path matches — for both dir and path, or dir
	// itself passed as path would split one component longer than dir
	// and slip past the guard below.
	dirParts := strings.Split(strings.TrimSuffix(filepath.Clean(dir), sep), sep)
	pathParts := strings.Split(strings.TrimSuffix(filepath.Clean(path), sep), sep)
	if len(pathParts) <= len(dirParts) {
		return "", false
	}
	for i, p := range dirParts {
		if !strings.EqualFold(p, pathParts[i]) {
			return "", false
		}
	}

	current := filepath.Clean(dir)
	for _, name := range pathParts[len(dirParts) : len(pathParts)-1] {
		if !walkedDir(relDir, name) {
			return "", false
		}
		current = filepath.Join(current, name)
		if hidden(current) {
			return "", false
		}
		relDir = filepath.Join(relDir, name)
	}
	return relDir, true
}

func underAny(key string, dirs []string) bool {
	return slices.ContainsFunc(dirs, func(d string) bool {
		return strings.HasPrefix(key, d+string(filepath.Separator))
	})
}

// updateEntry refreshes and returns entries[key] from a freshly stat'd,
// already-filtered candidate, resetting its stability tracking whenever
// (size, mtime) changed.
func updateEntry(entries map[string]*entry, key string, info fs.FileInfo, now time.Time) *entry {
	e, ok := entries[key]
	if !ok {
		e = &entry{}
		entries[key] = e
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

// settle validates and, if valid, probes a candidate, name in the folder at
// relDir, whose (size, mtime) has already held long enough to be considered
// settled, and records the outcome. ok is true for a file ready to emit; a
// non-nil rejected reports a validation failure instead. Validation comes
// first so a file the probe cannot open, such as one past MAX_PATH, is
// still rejected for its name or folder.
func (w *Watcher) settle(relDir, name string, e *entry) (file File, rejected *Rejected, ok bool) {
	path := filepath.Join(w.opts.Dir, relDir, name)

	if verr := w.opts.Validate(relDir, name); verr != nil {
		e.rejected = true
		e.rejectedSize = e.size
		e.rejectedModTime = e.modTime
		return File{}, &Rejected{Name: name, Dir: relDir, Path: path, Reason: verr.Error()}, false
	}

	busy, err := w.opts.Probe(path)
	if err != nil {
		w.opts.Logger.Warn("watch: probe failed; treating as busy", "path", path, "error", err)
		busy = true
	}
	if busy {
		return File{}, nil, false
	}

	changed := e.emitted
	e.emitted = true
	e.emittedSize = e.size
	e.emittedModTime = e.modTime
	return File{Name: name, Dir: relDir, Path: path, Size: e.size, ModTime: e.modTime, Changed: changed}, nil, true
}

// isCandidateName reports whether a non-directory entry called name is a
// file this Watcher tracks at all, based on its name alone (no I/O): a
// recognized extension, not a partial or hidden file.
func (w *Watcher) isCandidateName(name string) bool {
	if skippedName(name) {
		return false
	}
	lower := strings.ToLower(name)
	if strings.HasSuffix(lower, ".tmp") {
		return false
	}
	return w.exts[filepath.Ext(lower)]
}
