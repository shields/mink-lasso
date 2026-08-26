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

// Package logfile implements a small, size-based rotating log file.
//
// gopkg.in/natefinch/lumberjack.v2, the usual choice for this, is
// unmaintained, so this package replaces it with the minimal behavior
// mink-lasso needs: a fixed byte limit, a fixed number of generations kept
// as Path.1 (newest) through Path.Keep (oldest), and nothing else — no
// compression, no age-based pruning. Writer implements io.WriteCloser, so it
// is a drop-in [log/slog] handler sink.
package logfile

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"sync"
)

// DefaultMaxBytes is the rotation threshold used when Options.MaxBytes is
// zero or negative.
const DefaultMaxBytes int64 = 5 << 20

// DefaultKeep is the number of rotated generations retained when
// Options.Keep is zero or negative.
const DefaultKeep = 5

// ErrClosed is returned by Write when the Writer has already been closed.
var ErrClosed = errors.New("logfile: writer is closed")

// Options configures Open.
type Options struct {
	// Path is the active log file. Rotated generations are written
	// alongside it as Path.1 (newest) through Path.Keep (oldest).
	Path string

	// MaxBytes is the size, in bytes, at which the active file is rotated.
	// Zero or negative means DefaultMaxBytes.
	MaxBytes int64

	// Keep is the number of rotated generations to retain. Zero or negative
	// means DefaultKeep.
	Keep int

	// OpenFile opens the active log file. Nil means os.OpenFile.
	OpenFile func(name string, flag int, perm os.FileMode) (*os.File, error)

	// Rename renames a rotated generation. Nil means os.Rename.
	Rename func(oldpath, newpath string) error

	// Remove deletes the oldest generation during rotation. Nil means
	// os.Remove.
	Remove func(name string) error

	// MkdirAll creates Path's directory before opening it. Nil means
	// os.MkdirAll.
	MkdirAll func(path string, perm os.FileMode) error
}

// Writer is a size-based rotating log file. It implements io.WriteCloser and
// is safe for concurrent use.
type Writer struct {
	mu     sync.Mutex
	opts   Options
	file   *os.File
	size   int64
	err    error // sticky error from a rotation that could not reopen the file
	closed bool
}

// Open creates Path's directory if needed, opens Path for appending
// (creating it if it does not exist), and returns a Writer positioned at the
// file's current size.
func Open(opts Options) (*Writer, error) {
	if opts.MaxBytes <= 0 {
		opts.MaxBytes = DefaultMaxBytes
	}
	if opts.Keep <= 0 {
		opts.Keep = DefaultKeep
	}
	if opts.OpenFile == nil {
		opts.OpenFile = os.OpenFile
	}
	if opts.Rename == nil {
		opts.Rename = os.Rename
	}
	if opts.Remove == nil {
		opts.Remove = os.Remove
	}
	if opts.MkdirAll == nil {
		opts.MkdirAll = os.MkdirAll
	}

	if err := opts.MkdirAll(filepath.Dir(opts.Path), 0o755); err != nil {
		return nil, fmt.Errorf("logfile: create directory: %w", err)
	}

	f, err := opts.OpenFile(opts.Path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, fmt.Errorf("logfile: open: %w", err)
	}

	fi, statErr := f.Stat()
	if statErr != nil {
		closeErr := f.Close()
		return nil, fmt.Errorf("logfile: stat: %w", errors.Join(statErr, closeErr))
	}

	return &Writer{opts: opts, file: f, size: fi.Size()}, nil
}

// Write appends p to the active file, rotating first if p would push the
// file past Options.MaxBytes. A single write larger than MaxBytes is still
// written whole; it triggers rotation on the next call.
//
// An error from rotation is returned without writing p. If rotation could
// reopen the active file despite a failed step (a missing intermediate
// generation is not an error), the Writer remains usable for the next call;
// if reopening itself fails, every subsequent call returns that error.
func (w *Writer) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.closed {
		return 0, ErrClosed
	}
	if w.err != nil {
		return 0, w.err
	}

	if w.size > 0 && w.size+int64(len(p)) > w.opts.MaxBytes {
		if err := w.rotate(); err != nil {
			return 0, err
		}
	}

	n, err := w.file.Write(p)
	w.size += int64(n)
	if err != nil {
		return n, fmt.Errorf("logfile: write: %w", err)
	}
	return n, nil
}

// rotate closes the active file, shifts Path.1..Path.Keep-1 up one
// generation (dropping Path.Keep), moves Path to Path.1, and reopens Path.
// Caller holds w.mu.
func (w *Writer) rotate() error {
	var firstErr error

	// The close error, if any, is always the first one recorded here.
	if err := w.file.Close(); err != nil {
		firstErr = fmt.Errorf("logfile: close: %w", err)
	}
	w.file = nil

	oldest := generation(w.opts.Path, w.opts.Keep)
	if err := w.opts.Remove(oldest); err != nil && !errors.Is(err, fs.ErrNotExist) && firstErr == nil {
		firstErr = fmt.Errorf("logfile: remove %s: %w", oldest, err)
	}

	for i := w.opts.Keep - 1; i >= 1; i-- {
		from, to := generation(w.opts.Path, i), generation(w.opts.Path, i+1)
		if err := w.opts.Rename(from, to); err != nil && !errors.Is(err, fs.ErrNotExist) && firstErr == nil {
			firstErr = fmt.Errorf("logfile: rename %s: %w", from, err)
		}
	}

	if err := w.opts.Rename(w.opts.Path, generation(w.opts.Path, 1)); err != nil && firstErr == nil {
		firstErr = fmt.Errorf("logfile: rename %s: %w", w.opts.Path, err)
	}

	f, err := w.opts.OpenFile(w.opts.Path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		// Without a file, every future write would fail the same way, so
		// make that failure sticky rather than repeating rotation forever.
		// Join in firstErr too, so a step that failed earlier in this same
		// rotation is not lost.
		w.err = fmt.Errorf("logfile: reopen: %w", errors.Join(firstErr, err))
		return w.err
	}

	// Stat rather than assume 0: when the Path -> Path.1 rename above
	// failed, Path was never displaced, so OpenFile's O_APPEND just
	// reopened the same non-empty file. Hardcoding size 0 here would
	// desync it from the real file, silently disabling MaxBytes
	// enforcement (w.size > 0 would stay false) until it happened to
	// drift back past MaxBytes on its own. Stat matches what Open does
	// for the same reason when it first opens Path.
	fi, statErr := f.Stat()
	if statErr != nil {
		closeErr := f.Close()
		w.err = fmt.Errorf("logfile: stat: %w", errors.Join(firstErr, statErr, closeErr))
		return w.err
	}
	w.file = f
	w.size = fi.Size()

	return firstErr
}

func generation(path string, n int) string {
	return path + "." + strconv.Itoa(n)
}

// Close closes the active file. It is idempotent: calling it again returns
// nil.
func (w *Writer) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.closed {
		return nil
	}
	w.closed = true

	if w.file == nil {
		return nil
	}
	err := w.file.Close()
	w.file = nil
	if err != nil {
		return fmt.Errorf("logfile: close: %w", err)
	}
	return nil
}
