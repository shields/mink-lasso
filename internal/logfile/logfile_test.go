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

package logfile

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// captureOpenFile wraps os.OpenFile and records the last *os.File it
// returned, so a test can reach in and close it out from under the Writer to
// force an otherwise uninjectable os/File error (Stat, Close, or Write on an
// already-closed file).
func captureOpenFile(captured **os.File) func(name string, flag int, perm os.FileMode) (*os.File, error) {
	return func(name string, flag int, perm os.FileMode) (*os.File, error) {
		f, err := os.OpenFile(name, flag, perm)
		if err == nil {
			*captured = f
		}
		return f, err
	}
}

func mustOpen(t *testing.T, opts Options) *Writer {
	t.Helper()
	w, err := Open(opts)
	if err != nil {
		t.Fatalf("Open() = %v", err)
	}
	return w
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%s) = %v", path, err)
	}
	return string(data)
}

func requireNotExist(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Stat(%s) = %v, want fs.ErrNotExist", path, err)
	}
}

func TestOpenDefaults(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "app.log")
	w := mustOpen(t, Options{Path: path})
	defer func() { _ = w.Close() }()

	if w.opts.MaxBytes != DefaultMaxBytes {
		t.Errorf("MaxBytes = %d, want %d", w.opts.MaxBytes, DefaultMaxBytes)
	}
	if w.opts.Keep != DefaultKeep {
		t.Errorf("Keep = %d, want %d", w.opts.Keep, DefaultKeep)
	}
}

func TestOpenExistingSizeCounted(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")
	if err := os.WriteFile(path, []byte("HELLO"), 0o644); err != nil {
		t.Fatal(err)
	}

	w := mustOpen(t, Options{Path: path, MaxBytes: 5})
	defer func() { _ = w.Close() }()

	if w.size != 5 {
		t.Fatalf("size = %d, want 5", w.size)
	}
	if _, err := w.Write([]byte("X")); err != nil {
		t.Fatalf("Write() = %v", err)
	}
	if got := readFile(t, path+".1"); got != "HELLO" {
		t.Errorf("path.1 = %q, want %q", got, "HELLO")
	}
	if got := readFile(t, path); got != "X" {
		t.Errorf("path = %q, want %q", got, "X")
	}
}

func TestOpenCreatesDirectory(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "logs", "sub", "app.log")
	w := mustOpen(t, Options{Path: path})
	defer func() { _ = w.Close() }()

	if _, err := os.Stat(path); err != nil {
		t.Fatalf("Stat(%s) = %v", path, err)
	}
}

func TestOpenMkdirAllError(t *testing.T) {
	t.Parallel()
	mkdirErr := errors.New("mkdir boom")
	_, err := Open(Options{
		Path:     filepath.Join(t.TempDir(), "sub", "app.log"),
		MkdirAll: func(string, os.FileMode) error { return mkdirErr },
	})
	if !errors.Is(err, mkdirErr) {
		t.Fatalf("Open() = %v, want wrapping %v", err, mkdirErr)
	}
}

func TestOpenOpenFileError(t *testing.T) {
	t.Parallel()
	openErr := errors.New("open boom")
	_, err := Open(Options{
		Path:     filepath.Join(t.TempDir(), "app.log"),
		OpenFile: func(string, int, os.FileMode) (*os.File, error) { return nil, openErr },
	})
	if !errors.Is(err, openErr) {
		t.Fatalf("Open() = %v, want wrapping %v", err, openErr)
	}
}

func TestOpenStatError(t *testing.T) {
	t.Parallel()
	// Returning an already-closed *os.File makes the subsequent Stat call
	// fail without needing an injectable Stat hook.
	_, err := Open(Options{
		Path: filepath.Join(t.TempDir(), "app.log"),
		OpenFile: func(name string, flag int, perm os.FileMode) (*os.File, error) {
			f, err := os.OpenFile(name, flag, perm)
			if err != nil {
				return nil, err
			}
			if err := f.Close(); err != nil {
				return nil, err
			}
			return f, nil
		},
	})
	if err == nil {
		t.Fatal("Open() = nil, want an error")
	}
}

func TestWriteExactFillThenRotate(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")
	w := mustOpen(t, Options{Path: path, MaxBytes: 4})
	defer func() { _ = w.Close() }()

	if _, err := w.Write([]byte("AAAA")); err != nil {
		t.Fatalf("Write() = %v", err)
	}
	requireNotExist(t, path+".1")

	if _, err := w.Write([]byte("B")); err != nil {
		t.Fatalf("Write() = %v", err)
	}
	if got := readFile(t, path+".1"); got != "AAAA" {
		t.Errorf("path.1 = %q, want %q", got, "AAAA")
	}
	if got := readFile(t, path); got != "B" {
		t.Errorf("path = %q, want %q", got, "B")
	}
}

func TestWriteLargerThanMaxIsWrittenWhole(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")
	w := mustOpen(t, Options{Path: path, MaxBytes: 4})
	defer func() { _ = w.Close() }()

	big := []byte("HELLOWORLD")
	n, err := w.Write(big)
	if err != nil || n != len(big) {
		t.Fatalf("Write() = (%d, %v), want (%d, nil)", n, err, len(big))
	}
	requireNotExist(t, path+".1")

	if _, err := w.Write([]byte("X")); err != nil {
		t.Fatalf("Write() = %v", err)
	}
	if got := readFile(t, path+".1"); got != string(big) {
		t.Errorf("path.1 = %q, want %q", got, big)
	}
}

func TestWriteRotationChain(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")
	w := mustOpen(t, Options{Path: path, MaxBytes: 1, Keep: 3})
	defer func() { _ = w.Close() }()

	for _, b := range []byte("12345") {
		if _, err := w.Write([]byte{b}); err != nil {
			t.Fatalf("Write(%q) = %v", b, err)
		}
	}

	want := map[string]string{
		path:        "5",
		path + ".1": "4",
		path + ".2": "3",
		path + ".3": "2",
	}
	for name, content := range want {
		if got := readFile(t, name); got != content {
			t.Errorf("%s = %q, want %q", name, got, content)
		}
	}
	requireNotExist(t, path+".4")
}

func TestWriteRotationKeepOne(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")
	w := mustOpen(t, Options{Path: path, MaxBytes: 1, Keep: 1})
	defer func() { _ = w.Close() }()

	for _, b := range []byte("123") {
		if _, err := w.Write([]byte{b}); err != nil {
			t.Fatalf("Write(%q) = %v", b, err)
		}
	}

	if got := readFile(t, path); got != "3" {
		t.Errorf("path = %q, want %q", got, "3")
	}
	if got := readFile(t, path+".1"); got != "2" {
		t.Errorf("path.1 = %q, want %q", got, "2")
	}
	requireNotExist(t, path+".2")
}

func TestWriteRemoveErrorSurfacesButWriterStaysUsable(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")
	removeErr := errors.New("remove boom")
	w := mustOpen(t, Options{
		Path:     path,
		MaxBytes: 1,
		Remove:   func(string) error { return removeErr },
	})
	defer func() { _ = w.Close() }()

	if _, err := w.Write([]byte("A")); err != nil {
		t.Fatalf("Write() = %v", err)
	}
	if _, err := w.Write([]byte("B")); !errors.Is(err, removeErr) {
		t.Fatalf("Write() = %v, want wrapping %v", err, removeErr)
	}
	if _, err := w.Write([]byte("C")); err != nil {
		t.Fatalf("Write() after rotation error = %v, want nil (writer should stay usable)", err)
	}
}

func TestWriteFinalRenameErrorSurfacesButWriterStaysUsable(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")
	renameErr := errors.New("rename boom")
	var failed bool
	w := mustOpen(t, Options{
		Path:     path,
		MaxBytes: 1,
		Rename: func(oldpath, newpath string) error {
			if oldpath == path {
				if !failed {
					failed = true
					return renameErr
				}
				return os.Rename(oldpath, newpath) // the transient failure has passed
			}
			return fs.ErrNotExist
		},
	})
	defer func() { _ = w.Close() }()

	if _, err := w.Write([]byte("A")); err != nil {
		t.Fatalf("Write() = %v", err)
	}
	if _, err := w.Write([]byte("B")); !errors.Is(err, renameErr) {
		t.Fatalf("Write() = %v, want wrapping %v", err, renameErr)
	}
	// The failed rename left "A" in place at path instead of moving it to
	// path.1, so rotate must have recorded the reopened file's real,
	// non-empty size — not reset it to 0 — or the next write below would
	// wrongly skip rotation and let the file grow past MaxBytes.
	if got := readFile(t, path); got != "A" {
		t.Fatalf("path content = %q, want %q (a failed rename must not lose data)", got, "A")
	}

	// The next write crosses MaxBytes again, retrying rotation; this time
	// the mock's rename succeeds for real, so the writer recovers.
	if _, err := w.Write([]byte("C")); err != nil {
		t.Fatalf("Write() after rotation error = %v, want nil (writer should stay usable)", err)
	}
	if got := readFile(t, path); got != "C" {
		t.Fatalf("path content = %q, want %q", got, "C")
	}
	if got := readFile(t, path+".1"); got != "A" {
		t.Fatalf("path.1 content = %q, want %q (the recovered rotation should have archived it)", got, "A")
	}
}

func TestWriteIntermediateRenameErrorSurfacesButWriterStaysUsable(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")
	renameErr := errors.New("intermediate rename boom")
	failNext := false
	w := mustOpen(t, Options{
		Path:     path,
		MaxBytes: 1,
		Keep:     2,
		Rename: func(oldpath, newpath string) error {
			if failNext && oldpath == path+".1" && newpath == path+".2" {
				return renameErr
			}
			return os.Rename(oldpath, newpath)
		},
	})
	defer func() { _ = w.Close() }()

	if _, err := w.Write([]byte("1")); err != nil {
		t.Fatalf("Write() = %v", err)
	}
	if _, err := w.Write([]byte("2")); err != nil { // first rotation: creates path.1, no intermediate shift yet
		t.Fatalf("Write() = %v", err)
	}

	failNext = true
	if _, err := w.Write([]byte("3")); !errors.Is(err, renameErr) {
		t.Fatalf("Write() = %v, want wrapping %v", err, renameErr)
	}
	if _, err := w.Write([]byte("4")); err != nil {
		t.Fatalf("Write() after rotation error = %v, want nil (writer should stay usable)", err)
	}
}

func TestWriteReopenErrorIsSticky(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")
	reopenErr := errors.New("reopen boom")
	calls := 0
	w := mustOpen(t, Options{
		Path:     path,
		MaxBytes: 1,
		OpenFile: func(name string, flag int, perm os.FileMode) (*os.File, error) {
			calls++
			if calls == 1 {
				return os.OpenFile(name, flag, perm)
			}
			return nil, reopenErr
		},
	})

	if _, err := w.Write([]byte("A")); err != nil {
		t.Fatalf("Write() = %v", err)
	}
	if _, err := w.Write([]byte("B")); !errors.Is(err, reopenErr) {
		t.Fatalf("Write() = %v, want wrapping %v", err, reopenErr)
	}
	if _, err := w.Write([]byte("C")); !errors.Is(err, reopenErr) {
		t.Fatalf("second Write() after sticky reopen error = %v, want wrapping %v", err, reopenErr)
	}
	// Close on a writer with no open file (reopen failed) is a no-op.
	if err := w.Close(); err != nil {
		t.Fatalf("Close() = %v, want nil", err)
	}
}

// TestWriteRotateStatErrorIsSticky checks that rotate treats a failed Stat
// on the freshly reopened file the same way it treats a failed reopen
// itself: as a sticky error, since without a reliable size there is no safe
// way to keep tracking MaxBytes for that file.
func TestWriteRotateStatErrorIsSticky(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")
	calls := 0
	w := mustOpen(t, Options{
		Path:     path,
		MaxBytes: 1,
		OpenFile: func(name string, flag int, perm os.FileMode) (*os.File, error) {
			calls++
			f, err := os.OpenFile(name, flag, perm)
			if err != nil {
				return nil, err
			}
			if calls == 2 {
				// Sabotage rotate's reopen (the first call is the Writer's
				// initial Open): Stat on an already-closed file fails.
				_ = f.Close()
			}
			return f, nil
		},
	})

	if _, err := w.Write([]byte("A")); err != nil {
		t.Fatalf("Write() = %v", err)
	}
	if _, err := w.Write([]byte("B")); err == nil {
		t.Fatal("Write() = nil, want an error from the failed Stat during rotation")
	}
	if _, err := w.Write([]byte("C")); err == nil {
		t.Fatal("second Write() after sticky stat error = nil, want the same sticky error")
	}
	// Close on a writer with no open file (stat failed after reopen) is a no-op.
	if err := w.Close(); err != nil {
		t.Fatalf("Close() = %v, want nil", err)
	}
}

func TestWriteReopenErrorJoinsEarlierRotationError(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")
	removeErr := errors.New("remove boom")
	reopenErr := errors.New("reopen boom")
	calls := 0
	w := mustOpen(t, Options{
		Path:     path,
		MaxBytes: 1,
		Remove:   func(string) error { return removeErr },
		OpenFile: func(name string, flag int, perm os.FileMode) (*os.File, error) {
			calls++
			if calls == 1 {
				return os.OpenFile(name, flag, perm)
			}
			return nil, reopenErr
		},
	})

	if _, err := w.Write([]byte("A")); err != nil {
		t.Fatalf("Write() = %v", err)
	}
	// Rotation both fails to remove the oldest generation and fails to
	// reopen the active file; the returned error should report both, not
	// just the reopen failure that happened to be recorded last.
	_, err := w.Write([]byte("B"))
	if !errors.Is(err, removeErr) {
		t.Errorf("Write() = %v, want wrapping %v", err, removeErr)
	}
	if !errors.Is(err, reopenErr) {
		t.Errorf("Write() = %v, want wrapping %v", err, reopenErr)
	}
}

func TestWriteRotateCloseErrorSurfacesButWriterStaysUsable(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")
	var captured *os.File
	w := mustOpen(t, Options{
		Path:     path,
		MaxBytes: 1,
		OpenFile: captureOpenFile(&captured),
	})
	defer func() { _ = w.Close() }()

	if _, err := w.Write([]byte("A")); err != nil {
		t.Fatalf("Write() = %v", err)
	}
	// Close the fd out from under the writer so rotate's own Close call fails.
	if err := captured.Close(); err != nil {
		t.Fatal(err)
	}

	if _, err := w.Write([]byte("B")); err == nil {
		t.Fatal("Write() = nil, want an error from the failed close during rotation")
	}
	if _, err := w.Write([]byte("C")); err != nil {
		t.Fatalf("Write() after rotation error = %v, want nil (writer should stay usable)", err)
	}
}

func TestWriteUnderlyingWriteError(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")
	var captured *os.File
	w := mustOpen(t, Options{
		Path:     path,
		OpenFile: captureOpenFile(&captured),
	})
	defer func() { _ = w.Close() }()

	if err := captured.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("x")); err == nil {
		t.Fatal("Write() = nil, want an error writing to a closed file")
	}
}

func TestWriteAfterClose(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "app.log")
	w := mustOpen(t, Options{Path: path})

	if err := w.Close(); err != nil {
		t.Fatalf("Close() = %v", err)
	}
	if _, err := w.Write([]byte("x")); !errors.Is(err, ErrClosed) {
		t.Fatalf("Write() after Close() = %v, want ErrClosed", err)
	}
}

func TestCloseIdempotent(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "app.log")
	w := mustOpen(t, Options{Path: path})

	if err := w.Close(); err != nil {
		t.Fatalf("first Close() = %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("second Close() = %v, want nil", err)
	}
}

func TestCloseError(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "app.log")
	var captured *os.File
	w := mustOpen(t, Options{
		Path:     path,
		OpenFile: captureOpenFile(&captured),
	})

	if err := captured.Close(); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err == nil {
		t.Fatal("Close() = nil, want an error from the already-closed file")
	}
}

func TestWriteConcurrent(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")
	w := mustOpen(t, Options{Path: path, MaxBytes: 64, Keep: 2})
	defer func() { _ = w.Close() }()

	const goroutines = 8
	const perGoroutine = 50
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for range goroutines {
		go func() {
			defer wg.Done()
			for range perGoroutine {
				if _, err := w.Write([]byte("x")); err != nil {
					t.Error(err)
				}
			}
		}()
	}
	wg.Wait()
}
