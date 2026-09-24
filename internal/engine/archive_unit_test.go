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
		Stat:              func(string) (os.FileInfo, error) { return nil, os.ErrNotExist },
		Rename:            func(string, string) error { return renameErr },
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
