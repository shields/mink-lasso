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

// This file unit-tests watchdir.go branches a full end-to-end scenario
// cannot reach deterministically: NewWatcher itself failing, probeOpen's
// two error shapes, SetWatchDir's not-a-directory case, and SendFile's
// post-open Stat failure.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"msrl.dev/mink-lasso/internal/config"
	"msrl.dev/mink-lasso/internal/masso"
	"msrl.dev/mink-lasso/internal/watch"
	"msrl.dev/mink-lasso/internal/winutil"
)

func TestRunWatchAttemptNewWatcherError(t *testing.T) {
	t.Parallel()
	wantErr := errors.New("simulated watcher construction failure")
	e, err := New(Options{
		Config:    config.Config{WatchDir: "/some/dir"},
		NewClient: func(masso.Options) (Client, error) { return fakeClient{}, nil },
		NewWatcher: func(watch.Options) (Watcher, error) {
			return nil, wantErr
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	go e.dispatcher.run()
	t.Cleanup(e.dispatcher.close)
	go e.watchLoop(ctx)
	t.Cleanup(cancel)

	deadline := time.After(2 * time.Second)
	for {
		select {
		case ev := <-e.Events():
			if ws, ok := ev.(WatchState); ok && ws.Err != nil {
				if !errors.Is(ws.Err, wantErr) {
					t.Errorf("WatchState.Err = %v, want wrapping %v", ws.Err, wantErr)
				}
				return
			}
		case <-deadline:
			t.Fatal("timed out waiting for WatchState with an error")
		}
	}
}

func TestProbeOpenBusy(t *testing.T) {
	t.Parallel()
	e := newUnitTestEngine(t, func(string) (*os.File, error) { return nil, winutil.ErrBusy })
	busy, err := e.probeOpen("/some/path")
	if err != nil || !busy {
		t.Errorf("probeOpen() = (%v, %v), want (true, nil)", busy, err)
	}
}

func TestProbeOpenOtherError(t *testing.T) {
	t.Parallel()
	wantErr := errors.New("simulated open failure")
	e := newUnitTestEngine(t, func(string) (*os.File, error) { return nil, wantErr })
	busy, err := e.probeOpen("/some/path")
	if busy || !errors.Is(err, wantErr) {
		t.Errorf("probeOpen() = (%v, %v), want (false, %v)", busy, err, wantErr)
	}
}

func TestSetWatchDirNotADirectory(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "not-a-dir.txt")
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	e := newUnitTestEngine(t, nil)
	if err := e.SetWatchDir(path); !errors.Is(err, errNotADirectory) {
		t.Errorf("SetWatchDir(%q) error = %v, want errNotADirectory", path, err)
	}
}

// raceWatcher is a Watcher whose Ready channel the test controls directly,
// used to place a file on it right at the moment SetWatchDir cancels the
// attempt — the exact window in which a stale Ready delivery could
// otherwise survive clearNonManual.
type raceWatcher struct {
	ready    chan watch.File
	rejected chan watch.Rejected
}

func newRaceWatcher() *raceWatcher {
	return &raceWatcher{
		ready:    make(chan watch.File, 1),
		rejected: make(chan watch.Rejected, 1),
	}
}

func (*raceWatcher) Run(ctx context.Context) error {
	<-ctx.Done()
	return ctx.Err()
}

func (w *raceWatcher) Ready() <-chan watch.File        { return w.ready }
func (w *raceWatcher) Rejected() <-chan watch.Rejected { return w.rejected }

// TestSetWatchDirJoinsOutgoingAttemptBeforeClearing confirms SetWatchDir
// never leaves a stale Ready delivery from the outgoing watch folder in the
// queue: a file queued on the old watcher's Ready channel right at the
// moment of the switch must not survive in e.scheduler.items once
// SetWatchDir has returned and switched to the new folder, regardless of
// whether the old forwarding goroutine happened to consume it before
// exiting.
func TestSetWatchDirJoinsOutgoingAttemptBeforeClearing(t *testing.T) {
	t.Parallel()
	dir1, dir2 := t.TempDir(), t.TempDir()
	watchers := make(chan *raceWatcher, 2)

	e, err := New(Options{
		Config:    config.Config{WatchDir: dir1},
		NewClient: func(masso.Options) (Client, error) { return fakeClient{}, nil },
		NewWatcher: func(watch.Options) (Watcher, error) {
			w := newRaceWatcher()
			watchers <- w
			return w, nil
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go e.dispatcher.run()
	t.Cleanup(e.dispatcher.close)
	go e.watchLoop(ctx)

	select {
	case w1 := <-watchers:
		// Queue the stale delivery right as we switch folders: it races
		// runWatchAttempt's cancellation directly.
		w1.ready <- watch.File{Name: "OLD.NC", Path: filepath.Join(dir1, "OLD.NC")}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the first watcher to be constructed")
	}

	if err := e.SetWatchDir(dir2); err != nil {
		t.Fatalf("SetWatchDir: %v", err)
	}

	select {
	case <-watchers: // the new watcher for dir2
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the second watcher to be constructed")
	}

	e.scheduler.mu.Lock()
	_, stillThere := e.scheduler.items["OLD.NC"]
	e.scheduler.mu.Unlock()
	if stillThere {
		t.Error("OLD.NC from the abandoned folder survived SetWatchDir's clear")
	}
}

// TestSendFileBusyFileError confirms SendFile propagates winutil.ErrBusy
// (from opts.Open) as a matchable error, distinctly from a missing file —
// the scenario TestSchedulerManualSendFile's now-corrected doc comment used
// to (mis)claim it covered via a missing-file path instead.
func TestSendFileBusyFileError(t *testing.T) {
	t.Parallel()
	e := newUnitTestEngine(t, func(string) (*os.File, error) { return nil, winutil.ErrBusy })
	if err := e.SendFile("/some/path/A.NC"); !errors.Is(err, winutil.ErrBusy) {
		t.Errorf("SendFile() error = %v, want wrapping winutil.ErrBusy", err)
	}
}

func TestSendFileStatError(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "A.NC")
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	e := newUnitTestEngine(t, func(string) (*os.File, error) { return f, nil })
	if err := e.SendFile(path); err == nil {
		t.Error("SendFile() error = nil, want a Stat error on an already-closed file")
	}
}
