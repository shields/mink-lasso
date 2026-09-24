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

// This file unit-tests archive.go's helpers directly against fake
// filesystem functions, for error paths a real filesystem cannot provoke
// deterministically (a MkdirAll or Rename failure, an already-elapsed
// shutdown mid-retry, a double name collision).

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"msrl.dev/mink-lasso/internal/clock"
	"msrl.dev/mink-lasso/internal/config"
	"msrl.dev/mink-lasso/internal/masso"
)

// archiveTestEngine builds an Engine (fakeClient, never Run) with the given
// filesystem overrides for archive.go unit tests.
func archiveTestEngine(t *testing.T, opts Options) *Engine {
	t.Helper()
	opts.Config = config.Config{}
	if opts.Clock == nil {
		opts.Clock = clock.NewFake(time.Now())
	}
	opts.NewClient = func(masso.Options) (Client, error) { return fakeClient{}, nil }
	e, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return e
}

// existingFileInfo returns a real os.FileInfo (of a throwaway temp file),
// for a fake Stat to return when simulating "this path already exists" —
// a fake Stat returning (nil, nil) would be indistinguishable from a bug.
func existingFileInfo(t *testing.T) os.FileInfo {
	t.Helper()
	path := filepath.Join(t.TempDir(), "exists")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	return info
}

// fakeFileInfo is an os.FileInfo with exactly the size and modTime a fake
// Stat needs to report, for archiveSent's own pre-rename verification —
// unlike existingFileInfo, whose caller only cares about presence.
type fakeFileInfo struct {
	size    int64
	modTime time.Time
}

func (fakeFileInfo) Name() string         { return "" }
func (f fakeFileInfo) Size() int64        { return f.size }
func (fakeFileInfo) Mode() os.FileMode    { return 0 }
func (f fakeFileInfo) ModTime() time.Time { return f.modTime }
func (fakeFileInfo) IsDir() bool          { return false }
func (fakeFileInfo) Sys() any             { return nil }

func TestMoveIntoSentMkdirAllError(t *testing.T) {
	t.Parallel()
	wantErr := errors.New("simulated mkdir failure")
	e := archiveTestEngine(t, Options{
		MkdirAll: func(string, os.FileMode) error { return wantErr },
	})
	err := e.scheduler.moveIntoSent("/sent", "/sent/A.NC", "/watch/A.NC")
	if !errors.Is(err, wantErr) {
		t.Errorf("moveIntoSent error = %v, want wrapping %v", err, wantErr)
	}
}

func TestMoveIntoSentRenameExistingError(t *testing.T) {
	t.Parallel()
	wantErr := errors.New("simulated rename failure")
	exists := existingFileInfo(t)
	e := archiveTestEngine(t, Options{
		MkdirAll: func(string, os.FileMode) error { return nil },
		Stat: func(path string) (os.FileInfo, error) {
			if path == "/sent/A.NC" {
				return exists, nil // dest already exists
			}
			return nil, os.ErrNotExist // the timestamped backup name is free
		},
		Rename: func(oldpath, newpath string) error {
			if oldpath == "/sent/A.NC" {
				return wantErr
			}
			t.Fatalf("unexpected Rename(%q, %q)", oldpath, newpath)
			return nil
		},
	})
	err := e.scheduler.moveIntoSent("/sent", "/sent/A.NC", "/watch/A.NC")
	if !errors.Is(err, wantErr) {
		t.Errorf("moveIntoSent error = %v, want wrapping %v", err, wantErr)
	}
}

func TestUniqueBackupNameDoubleCollision(t *testing.T) {
	t.Parallel()
	fake := clock.NewFake(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))
	stamp := fake.Now().Format(archiveTimestampLayout)
	noSuffix := "/sent/A." + stamp + ".NC"
	suffix1 := "/sent/A." + stamp + "-1.NC"

	exists := existingFileInfo(t)
	e := archiveTestEngine(t, Options{
		Clock: fake,
		Stat: func(path string) (os.FileInfo, error) {
			if path == noSuffix || path == suffix1 {
				return exists, nil // both already taken
			}
			return nil, os.ErrNotExist
		},
	})
	got := e.scheduler.uniqueBackupName("/sent/A.NC")
	want := "/sent/A." + stamp + "-2.NC"
	if got != want {
		t.Errorf("uniqueBackupName() = %q, want %q", got, want)
	}
}

func TestWaitCtxDone(t *testing.T) {
	t.Parallel()
	e := archiveTestEngine(t, Options{Clock: clock.Real{}})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if e.scheduler.wait(ctx, time.Hour) {
		t.Error("wait() = true with an already-canceled ctx, want false")
	}
}

func TestArchiveSentCtxCanceledDuringRetryReportsSentUnfiled(t *testing.T) {
	t.Parallel()
	renameErr := errors.New("simulated rename failure")
	e := archiveTestEngine(t, Options{
		Clock:             clock.Real{},
		MoveRetries:       3,
		MoveRetryInterval: time.Hour, // never actually waited: ctx is canceled first
		MkdirAll:          func(string, os.FileMode) error { return nil },
		Stat: func(path string) (os.FileInfo, error) {
			if path == "/watch/A.NC" {
				return fakeFileInfo{}, nil // unchanged since it was sent
			}
			return nil, os.ErrNotExist // the sent/ destination doesn't exist yet
		},
		Rename: func(string, string) error { return renameErr },
	})
	it := &item{name: "A.NC", path: "/watch/A.NC", state: Sending}
	e.scheduler.items["A.NC"] = it

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	e.scheduler.archiveSent(ctx, it, sendSource{root: "/watch", base: "A.NC", path: "/watch/A.NC"})

	e.scheduler.mu.Lock()
	defer e.scheduler.mu.Unlock()
	if it.state != SentUnfiled {
		t.Errorf("state = %v, want SentUnfiled after a canceled retry wait", it.state)
	}
	if !strings.Contains(it.message, renameErr.Error()) {
		t.Errorf("message = %q, want it to report the rename failure", it.message)
	}
}

func TestArchiveSentFileChangedBeforeFirstAttemptLeftInPlace(t *testing.T) {
	t.Parallel()
	sentAt := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	e := archiveTestEngine(t, Options{
		MoveRetries: 2,
		MkdirAll: func(string, os.FileMode) error {
			t.Fatal("MkdirAll called despite a source file changed since it was sent")
			return nil
		},
		Stat: func(string) (os.FileInfo, error) {
			return fakeFileInfo{size: 999, modTime: sentAt}, nil // size no longer matches
		},
		Rename: func(string, string) error {
			t.Fatal("Rename called despite a source file changed since it was sent")
			return nil
		},
	})
	it := &item{name: "A.NC", path: "/watch/A.NC", state: Sending}
	e.scheduler.items["A.NC"] = it

	e.scheduler.archiveSent(context.Background(), it,
		sendSource{root: "/watch", base: "A.NC", path: "/watch/A.NC", size: 4, modTime: sentAt})

	e.scheduler.mu.Lock()
	defer e.scheduler.mu.Unlock()
	if it.state != Sent {
		t.Errorf("state = %v, want Sent (left in place, not archived)", it.state)
	}
	if !strings.Contains(it.message, "changed") {
		t.Errorf("message = %q, want it to mention the file changing after it was sent", it.message)
	}
}

func TestArchiveSentRenameFailsThenFileChangesBeforeRetry(t *testing.T) {
	t.Parallel()
	renameErr := errors.New("simulated rename failure")
	sentAt := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	statCalls := 0
	e := archiveTestEngine(t, Options{
		Clock:             clock.Real{},
		MoveRetries:       2,
		MoveRetryInterval: 5 * time.Millisecond,
		MkdirAll:          func(string, os.FileMode) error { return nil },
		Stat: func(path string) (os.FileInfo, error) {
			if path != "/watch/A.NC" {
				return nil, os.ErrNotExist // the sent/ destination doesn't exist yet
			}
			statCalls++
			if statCalls == 1 {
				return fakeFileInfo{size: 4, modTime: sentAt}, nil
			}
			return fakeFileInfo{size: 5, modTime: sentAt}, nil // changed before the retry
		},
		Rename: func(oldpath, newpath string) error {
			if oldpath != "/watch/A.NC" {
				t.Fatalf("unexpected Rename(%q, %q)", oldpath, newpath)
			}
			return renameErr
		},
	})
	it := &item{name: "A.NC", path: "/watch/A.NC", state: Sending}
	e.scheduler.items["A.NC"] = it

	e.scheduler.archiveSent(context.Background(), it,
		sendSource{root: "/watch", base: "A.NC", path: "/watch/A.NC", size: 4, modTime: sentAt})

	e.scheduler.mu.Lock()
	defer e.scheduler.mu.Unlock()
	if it.state != Sent {
		t.Errorf("state = %v, want Sent (left in place once the retry sees the file changed)", it.state)
	}
	if !strings.Contains(it.message, "changed") {
		t.Errorf("message = %q, want it to mention the file changing after it was sent", it.message)
	}
	if statCalls != 2 {
		t.Errorf("verify Stat called %d times, want 2 (one before each attempt up to the mismatch)", statCalls)
	}
}

// A Stat error is a failed attempt, not a mismatch: treating it as one would
// leave the file unarchived with no failure reported.
func TestArchiveSentVerifyStatErrorRetriesThenSentUnfiled(t *testing.T) {
	t.Parallel()
	statErr := errors.New("simulated stat failure")
	e := archiveTestEngine(t, Options{
		Clock:             clock.Real{},
		MoveRetries:       2,
		MoveRetryInterval: 5 * time.Millisecond,
		MkdirAll: func(string, os.FileMode) error {
			t.Fatal("MkdirAll called despite a failed verify Stat")
			return nil
		},
		Stat: func(string) (os.FileInfo, error) { return nil, statErr },
		Rename: func(string, string) error {
			t.Fatal("Rename called despite a failed verify Stat")
			return nil
		},
	})
	it := &item{name: "A.NC", path: "/watch/A.NC", state: Sending}
	e.scheduler.items["A.NC"] = it

	e.scheduler.archiveSent(context.Background(), it, sendSource{root: "/watch", base: "A.NC", path: "/watch/A.NC"})

	e.scheduler.mu.Lock()
	defer e.scheduler.mu.Unlock()
	if it.state != SentUnfiled {
		t.Errorf("state = %v, want SentUnfiled once every verify Stat fails", it.state)
	}
	if !strings.Contains(it.message, statErr.Error()) {
		t.Errorf("message = %q, want it to report the stat failure", it.message)
	}
}

func TestArchiveSentManualDuringRetryStopsArchiving(t *testing.T) {
	t.Parallel()
	renameErr := errors.New("simulated rename failure")
	var it *item
	renameCalls := 0
	e := archiveTestEngine(t, Options{
		Clock:             clock.Real{},
		MoveRetries:       2,
		MoveRetryInterval: 5 * time.Millisecond,
		MkdirAll:          func(string, os.FileMode) error { return nil },
		Stat: func(path string) (os.FileInfo, error) {
			if path == "/watch/A.NC" {
				return fakeFileInfo{}, nil // unchanged since it was sent
			}
			return nil, os.ErrNotExist // the sent/ destination doesn't exist yet
		},
		Rename: func(oldpath, newpath string) error {
			if oldpath != "/watch/A.NC" {
				t.Fatalf("unexpected Rename(%q, %q)", oldpath, newpath)
			}
			renameCalls++
			it.manual = true // a SendFile call arriving mid-retry
			return renameErr
		},
	})
	it = &item{name: "A.NC", path: "/watch/A.NC", state: Sending}
	e.scheduler.items["A.NC"] = it

	e.scheduler.archiveSent(context.Background(), it, sendSource{root: "/watch", base: "A.NC", path: "/watch/A.NC"})

	e.scheduler.mu.Lock()
	defer e.scheduler.mu.Unlock()
	if it.state != Sent {
		t.Errorf("state = %v, want Sent (manual, so left in place)", it.state)
	}
	if renameCalls != 1 {
		t.Errorf("Rename called %d times, want exactly 1 (the retry must stop once the item turns manual)", renameCalls)
	}
}

// TestSetTerminalLogsFileSent confirms a successful send is logged at Info
// (finding 5): the package's own doc comment on Options.Logger promises an
// Info line for "file sent", alongside "connected" and failures, but only
// failures were actually logged.
func TestSetTerminalLogsFileSent(t *testing.T) {
	t.Parallel()
	var logMu sync.Mutex
	var logBuf bytes.Buffer
	e := archiveTestEngine(t, Options{
		Logger: slog.New(slog.NewTextHandler(&syncWriter{mu: &logMu, w: &logBuf}, nil)),
	})
	it := &item{name: "A.NC", path: "/watch/A.NC", state: Sending}
	e.scheduler.items["A.NC"] = it

	e.scheduler.setTerminal(it, Sent, "File sent")

	logMu.Lock()
	logged := logBuf.String()
	logMu.Unlock()
	if !strings.Contains(logged, "file sent") {
		t.Errorf("log output = %q, want it to mention the file being sent", logged)
	}
}

// TestSetTerminalSupersededOrphanStaysUnreported confirms setTerminal does
// not report an outcome for an item a folder switch orphaned and a new
// watcher has already replaced under the same name: emitting one would
// overwrite the replacement's live row with this stale item's state (see
// supersededLocked).
func TestSetTerminalSupersededOrphanStaysUnreported(t *testing.T) {
	t.Parallel()
	e := archiveTestEngine(t, Options{})
	it := &item{name: "A.NC", path: "/watch/A.NC", state: Sending, orphaned: true}
	e.scheduler.items["A.NC"] = &item{name: "A.NC", state: Pending} // the replacement now owns this name

	e.scheduler.setTerminal(it, Sent, "File sent")

	if evs := takeQueued(t, e); len(evs) != 0 {
		t.Errorf("events = %+v, want none (superseded item's outcome must not overwrite the replacement's row)", evs)
	}
}
