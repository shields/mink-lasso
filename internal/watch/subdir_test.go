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
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// writeTree writes each file (a slash-separated path relative to root)
// with the same content and mtime, creating its folders.
func writeTree(t *testing.T, root string, when time.Time, files ...string) {
	t.Helper()
	for _, rel := range files {
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		writeFile(t, path, "G0")
		if err := os.Chtimes(path, when, when); err != nil {
			t.Fatalf("Chtimes(%s): %v", path, err)
		}
	}
}

type validateCalls struct {
	mu    sync.Mutex
	calls [][2]string
}

func (v *validateCalls) validate(dir, name string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.calls = append(v.calls, [2]string{dir, name})
	return nil
}

func (v *validateCalls) sorted() [][2]string {
	v.mu.Lock()
	defer v.mu.Unlock()
	out := slices.Clone(v.calls)
	slices.SortFunc(out, func(a, b [2]string) int {
		return slices.Compare(a[:], b[:])
	})
	return out
}

func TestSubfoldersWatched(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	when := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	writeTree(t, dir, when,
		"top.nc",
		"JOBS/SUB/part.nc",
		"JOBS/sent/nested.nc", // only the top-level sent folder is skipped
		"Sent/archived.nc",
		".git/a.nc",
		"JOBS/.cache/b.nc",
		"~$tmp/c.nc",
		"JOBS/~$x/d.nc",
		"notafile.nc/e.nc", // a folder with a candidate's name is still a folder
	)

	var v validateCalls
	h := newHarness(t, Options{Dir: dir, Interval: testInterval, Settle: testSettle, Validate: v.validate})

	settleFile(h)
	got := make([]File, 0, 4)
	for range 4 {
		got = append(got, h.recvFile())
	}
	h.waitScan()
	h.tickQuiet()

	want := []File{
		{Name: "part.nc", Dir: filepath.Join("JOBS", "SUB")},
		{Name: "nested.nc", Dir: filepath.Join("JOBS", "sent")},
		{Name: "e.nc", Dir: "notafile.nc"},
		{Name: "top.nc", Dir: ""},
	}
	for i, w := range want {
		g := got[i]
		if g.Name != w.Name || g.Dir != w.Dir {
			t.Errorf("Ready()[%d] = (%q, %q), want (%q, %q)", i, g.Dir, g.Name, w.Dir, w.Name)
			continue
		}
		if wantPath := filepath.Join(dir, w.Dir, w.Name); g.Path != wantPath {
			t.Errorf("Ready()[%d].Path = %q, want %q", i, g.Path, wantPath)
		}
	}

	wantCalls := [][2]string{
		{"", "top.nc"},
		{filepath.Join("JOBS", "SUB"), "part.nc"},
		{filepath.Join("JOBS", "sent"), "nested.nc"},
		{"notafile.nc", "e.nc"},
	}
	slices.SortFunc(wantCalls, func(a, b [2]string) int { return slices.Compare(a[:], b[:]) })
	if gotCalls := v.sorted(); !slices.Equal(gotCalls, wantCalls) {
		t.Errorf("Validate calls = %q, want %q", gotCalls, wantCalls)
	}
}

func TestRejectedInSubfolderReportsDir(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	when := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	writeTree(t, dir, when, "b.nc", "A/z.nc")

	validate := func(string, string) error { return errors.New("bad") }
	h := newHarness(t, Options{Dir: dir, Interval: testInterval, Settle: testSettle, Validate: validate})

	settleFile(h)
	first := h.recvRejected()
	second := h.recvRejected()
	h.waitScan()

	if first.Dir != "A" || first.Name != "z.nc" || first.Path != filepath.Join(dir, "A", "z.nc") {
		t.Errorf("first Rejected() = %+v, want A/z.nc", first)
	}
	if second.Dir != "" || second.Name != "b.nc" || second.Path != filepath.Join(dir, "b.nc") {
		t.Errorf("second Rejected() = %+v, want b.nc", second)
	}
}

func TestSubfolderReadDirErrorKeepsItsEntries(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	when := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	writeTree(t, dir, when, "top.nc", "JOBS/SUB/part.nc")
	failing := filepath.Join(dir, "JOBS", "SUB")

	var fail atomic.Bool
	readDir := func(d string) ([]fs.DirEntry, error) {
		if fail.Load() && d == failing {
			return nil, errors.New("permission denied")
		}
		return os.ReadDir(d)
	}

	var logged memHandler
	h := newHarness(t, Options{
		Dir: dir, Interval: testInterval, Settle: testSettle,
		ReadDir: readDir, Logger: slog.New(&logged),
	})

	settleFile(h)
	h.recvFile()
	h.recvFile()
	h.waitScan()

	before := logged.count()
	fail.Store(true)
	h.tickQuiet()
	if !slices.Contains(logged.messages()[before:], "watch: could not list directory") {
		t.Error("expected a log line for the subfolder ReadDir error")
	}
	failed := logged.count()

	// While JOBS/SUB is unlisted, the rest of the tree is still scanned: a
	// new sibling settles, and a file removed elsewhere is forgotten.
	writeTree(t, dir, when, "JOBS/late.nc")
	if err := os.Remove(filepath.Join(dir, "top.nc")); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	settleFile(h)
	if f := h.recvFile(); f.Name != "late.nc" || f.Dir != "JOBS" {
		t.Fatalf("Ready() = %+v, want JOBS/late.nc", f)
	}
	h.waitScan()

	if n := countMessage(logged.messages()[failed:], "watch: could not list directory"); n != 0 {
		t.Errorf("a folder that stayed unlistable was logged %d more times, want 0", n)
	}

	// Once JOBS/SUB lists again, part.nc's entry is still there, so it is
	// not re-emitted as if it were new.
	recovered := logged.count()
	fail.Store(false)
	for range int(testSettle/testInterval) + 2 {
		h.tickQuiet()
	}
	if n := countMessage(logged.messages()[recovered:], "watch: can list directory again"); n != 1 {
		t.Errorf("recovery was logged %d times, want 1", n)
	}

	// top.nc was forgotten: recreated with new content, it is not Changed.
	writeFile(t, filepath.Join(dir, "top.nc"), "G0 X1")
	settleFile(h)
	if f := h.recvFile(); f.Name != "top.nc" || f.Changed {
		t.Fatalf("Ready() = %+v, want top.nc not Changed", f)
	}
}

func countMessage(messages []string, want string) int {
	n := 0
	for _, m := range messages {
		if m == want {
			n++
		}
	}
	return n
}

func TestScanStopsWalkingOnceCanceled(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeTree(t, dir, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), "A/a.nc", "B/b.nc", "c.nc")
	ctx, cancel := context.WithCancel(t.Context())
	var listed []string
	w, err := New(Options{Dir: dir, ReadDir: func(d string) ([]fs.DirEntry, error) {
		listed = append(listed, d)
		if d == filepath.Join(dir, "A") {
			cancel()
		}
		return os.ReadDir(d)
	}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	w.scan(ctx, make(map[string]*entry))

	if want := []string{dir, filepath.Join(dir, "A")}; !slices.Equal(listed, want) {
		t.Errorf("listed %q, want %q", listed, want)
	}
}

func TestInvalidFileRejectedEvenIfProbeFails(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeTree(t, dir, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), "JOBS/part.nc")
	var probed atomic.Bool
	h := newHarness(t, Options{
		Dir: dir, Interval: testInterval, Settle: testSettle,
		Validate: func(string, string) error { return errors.New("folder too long") },
		Probe: func(string) (bool, error) {
			probed.Store(true)
			return false, errors.New("path too long")
		},
	})

	settleFile(h)
	if r := h.recvRejected(); r.Dir != "JOBS" || r.Name != "part.nc" || r.Reason != "folder too long" {
		t.Fatalf("Rejected() = %+v, want JOBS/part.nc for its folder", r)
	}
	if probed.Load() {
		t.Error("Probe ran for a file Validate rejects")
	}
}

func TestHiddenSubfoldersSkipped(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	when := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	writeTree(t, dir, when, "top.nc", "$RECYCLE.BIN/S-1/$R1.nc", "JOBS/part.nc")
	hiddenDir := filepath.Join(dir, "$RECYCLE.BIN")

	var listed, asked sync.Map
	readDir := func(d string) ([]fs.DirEntry, error) {
		listed.Store(d, true)
		return os.ReadDir(d)
	}
	hidden := func(path string) bool {
		asked.Store(path, true)
		return path == hiddenDir
	}
	h := newHarness(t, Options{
		Dir: dir, Interval: testInterval, Settle: testSettle, ReadDir: readDir, Hidden: hidden,
	})

	settleFile(h)
	first, second := h.recvFile(), h.recvFile()
	h.waitScan()
	h.tickQuiet()

	if first.Dir != "JOBS" || first.Name != "part.nc" || second.Dir != "" || second.Name != "top.nc" {
		t.Errorf("Ready() = %+v then %+v, want JOBS/part.nc then top.nc", first, second)
	}
	if _, ok := listed.Load(hiddenDir); ok {
		t.Error("hidden folder was listed")
	}
	if _, ok := asked.Load(filepath.Join(dir, "JOBS")); !ok {
		t.Error("Hidden was not asked about JOBS by its full path")
	}
}

// symlinkInfo describes a symbolic link itself, as os.ReadDir's entries do
// for one.
type symlinkInfo struct{ name string }

func (i symlinkInfo) Name() string     { return i.name }
func (symlinkInfo) Size() int64        { return 0 }
func (symlinkInfo) Mode() fs.FileMode  { return fs.ModeSymlink | 0o777 }
func (symlinkInfo) ModTime() time.Time { return time.Time{} }
func (symlinkInfo) IsDir() bool        { return false }
func (symlinkInfo) Sys() any           { return nil }

func TestSymlinksNotFollowed(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeTree(t, dir, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), "job.nc")

	var listed sync.Map
	readDir := func(d string) ([]fs.DirEntry, error) {
		listed.Store(d, true)
		entries, err := os.ReadDir(d)
		if d == dir {
			entries = append(entries,
				fs.FileInfoToDirEntry(symlinkInfo{"linked"}), fs.FileInfoToDirEntry(symlinkInfo{"linked.nc"}))
		}
		return entries, err
	}
	h := newHarness(t, Options{Dir: dir, Interval: testInterval, Settle: testSettle, ReadDir: readDir})

	settleFile(h)
	if f := h.recvFile(); f.Name != "job.nc" {
		t.Fatalf("Ready() = %+v, want job.nc", f)
	}
	h.waitScan()
	h.tickQuiet()

	if _, ok := listed.Load(filepath.Join(dir, "linked")); ok {
		t.Error("the watcher listed a symbolic link as a folder")
	}
}

func TestRealSymlinksNotFollowed(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	other := t.TempDir()
	when := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	writeTree(t, dir, when, "job.nc")
	writeTree(t, other, when, "outside.nc")
	if err := os.Symlink(other, filepath.Join(dir, "linked")); err != nil {
		if runtime.GOOS == "windows" {
			// Creating a symbolic link needs a privilege or Developer Mode;
			// TestSymlinksNotFollowed covers the logic without one.
			t.Skipf("Symlink: %v", err)
		}
		t.Fatalf("Symlink: %v", err)
	}
	if err := os.Symlink(filepath.Join(other, "outside.nc"), filepath.Join(dir, "linked.nc")); err != nil {
		t.Fatalf("Symlink: %v", err)
	}

	h := newHarness(t, Options{Dir: dir, Interval: testInterval, Settle: testSettle})

	settleFile(h)
	if f := h.recvFile(); f.Name != "job.nc" {
		t.Fatalf("Ready() = %+v, want job.nc", f)
	}
	h.waitScan()
	h.tickQuiet()
}

func TestWalkedDir(t *testing.T) {
	t.Parallel()
	tests := []struct {
		relDir, name string
		want         bool
	}{
		{"", "JOBS", true},
		{"", "sent", false},
		{"", "SENT", false},
		{"", "Sent", false},
		{"JOBS", "sent", true},
		{"", "sent2", true},
		{"", ".git", false},
		{"JOBS", ".cache", false},
		{"", "~$lock", false},
		{"JOBS", "~$lock", false},
		{"", "~x", true},
	}
	for _, tt := range tests {
		if got := walkedDir(tt.relDir, tt.name); got != tt.want {
			t.Errorf("walkedDir(%q, %q) = %v, want %v", tt.relDir, tt.name, got, tt.want)
		}
	}
}

func TestUnderAny(t *testing.T) {
	t.Parallel()
	sub := filepath.Join("JOBS", "SUB")
	tests := []struct {
		key  string
		want bool
	}{
		{filepath.Join("JOBS", "SUB", "a.nc"), true},
		{filepath.Join("JOBS", "SUB", "DEEP", "a.nc"), true},
		{filepath.Join("JOBS", "SUBX", "a.nc"), false},
		{filepath.Join("JOBS", "a.nc"), false},
		{"a.nc", false},
	}
	for _, tt := range tests {
		if got := underAny(tt.key, []string{"OTHER", sub}); got != tt.want {
			t.Errorf("underAny(%q) = %v, want %v", tt.key, got, tt.want)
		}
	}
	if underAny("a.nc", nil) {
		t.Error("underAny with no folders = true, want false")
	}
}
