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

package model

import (
	"errors"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"msrl.dev/mink-lasso/internal/clock"
	"msrl.dev/mink-lasso/internal/config"
	"msrl.dev/mink-lasso/internal/engine"
	"msrl.dev/mink-lasso/internal/masso"
)

func TestApplySerialSuccess(t *testing.T) {
	t.Parallel()
	eng := &fakeControl{}
	saver := &fakeSaver{}
	m := New(Options{Engine: eng, Save: saver.save})

	if err := m.ApplySerial("g3-12345"); err != nil {
		t.Fatalf("ApplySerial() error = %v", err)
	}
	if got := m.SerialText(); got != "G3-12345" {
		t.Errorf("SerialText() = %q, want G3-12345 (normalized)", got)
	}
	if len(eng.serials) != 1 || eng.serials[0] != 12345 {
		t.Errorf("engine.serials = %v, want [12345]", eng.serials)
	}
	if len(saver.cfgs) != 1 || saver.cfgs[0].Serial != "G3-12345" {
		t.Errorf("saver.cfgs = %v, want one Config with Serial G3-12345", saver.cfgs)
	}
}

func TestApplySerialBadInput(t *testing.T) {
	t.Parallel()
	eng := &fakeControl{}
	m := New(Options{Engine: eng, Config: config.Config{Serial: "G3-1"}})

	err := m.ApplySerial("not a serial")
	if !errors.Is(err, masso.ErrBadSerial) {
		t.Errorf("ApplySerial() error = %v, want ErrBadSerial", err)
	}
	if got := m.SerialText(); got != "G3-1" {
		t.Errorf("SerialText() = %q, want unchanged G3-1", got)
	}
	if len(eng.serials) != 0 {
		t.Errorf("engine.serials = %v, want none called", eng.serials)
	}
}

func TestApplySerialSaveError(t *testing.T) {
	t.Parallel()
	eng := &fakeControl{}
	saver := &fakeSaver{err: errSaveFailed}
	m := New(Options{Engine: eng, Config: config.Config{Serial: "G3-1"}, Save: saver.save})

	err := m.ApplySerial("G3-9")
	if !errors.Is(err, errSaveFailed) {
		t.Errorf("ApplySerial() error = %v, want errSaveFailed", err)
	}
	if got := m.SerialText(); got != "G3-1" {
		t.Errorf("SerialText() = %q, want reverted to G3-1", got)
	}
	if len(eng.serials) != 0 {
		t.Errorf("engine.serials = %v, want no engine call after a save failure", eng.serials)
	}
}

func TestApplySerialNilEngineAndSave(t *testing.T) {
	t.Parallel()
	m := New(Options{})
	if err := m.ApplySerial("G3-42"); err != nil {
		t.Fatalf("ApplySerial() error = %v", err)
	}
	if got := m.SerialText(); got != "G3-42" {
		t.Errorf("SerialText() = %q, want G3-42", got)
	}
}

func TestSetWatchDirSuccess(t *testing.T) {
	t.Parallel()
	eng := &fakeControl{}
	saver := &fakeSaver{}
	m := New(Options{Engine: eng, Save: saver.save})

	if err := m.SetWatchDir("/watch"); err != nil {
		t.Fatalf("SetWatchDir() error = %v", err)
	}
	if got := m.FolderPath(); got != "/watch" {
		t.Errorf("FolderPath() = %q, want /watch", got)
	}
	// SentFolderPath is built with filepath.Join, which writes the OS
	// separator (e.g. "\watch\sent" on Windows); compare against the
	// same construction rather than a hardcoded forward-slash literal.
	if want := filepath.Join("/watch", "sent"); m.SentFolderPath() != want {
		t.Errorf("SentFolderPath() = %q, want %q", m.SentFolderPath(), want)
	}
	if len(eng.watchDirs) != 1 || eng.watchDirs[0] != "/watch" {
		t.Errorf("engine.watchDirs = %v", eng.watchDirs)
	}
	if len(saver.cfgs) != 1 || saver.cfgs[0].WatchDir != "/watch" {
		t.Errorf("saver.cfgs = %v", saver.cfgs)
	}
}

func TestSetWatchDirEngineError(t *testing.T) {
	t.Parallel()
	wantErr := errors.New("bad dir")
	eng := &fakeControl{watchDirErr: wantErr}
	saver := &fakeSaver{}
	m := New(Options{Engine: eng, Save: saver.save, Config: config.Config{WatchDir: "/old"}})

	err := m.SetWatchDir("/new")
	if !errors.Is(err, wantErr) {
		t.Errorf("SetWatchDir() error = %v, want wantErr", err)
	}
	if got := m.FolderPath(); got != "/old" {
		t.Errorf("FolderPath() = %q, want unchanged /old", got)
	}
	if len(saver.cfgs) != 0 {
		t.Errorf("saver.cfgs = %v, want no save after an engine error", saver.cfgs)
	}
}

func TestSetWatchDirSaveError(t *testing.T) {
	t.Parallel()
	eng := &fakeControl{}
	saver := &fakeSaver{err: errSaveFailed}
	m := New(Options{Engine: eng, Save: saver.save, Config: config.Config{WatchDir: "/old"}})

	err := m.SetWatchDir("/new")
	if !errors.Is(err, errSaveFailed) {
		t.Errorf("SetWatchDir() error = %v, want errSaveFailed", err)
	}
	if got := m.FolderPath(); got != "/old" {
		t.Errorf("FolderPath() = %q, want reverted to /old", got)
	}
	// The engine already committed to watching "/new" before the save was
	// attempted; a save failure must revert it too, or the running
	// watcher and the just-rolled-back config disagree about which
	// folder is active until restart.
	if want := []string{"/new", "/old"}; !slices.Equal(eng.watchDirs, want) {
		t.Errorf("engine.watchDirs = %v, want %v (initial call, then revert)", eng.watchDirs, want)
	}
}

// TestSetWatchDirSaveErrorAndRevertError confirms that when the engine
// revert itself also fails, neither error is silently dropped.
func TestSetWatchDirSaveErrorAndRevertError(t *testing.T) {
	t.Parallel()
	revertErr := errors.New("revert also failed")
	eng := &fakeControl{watchDirErrs: []error{nil, revertErr}}
	saver := &fakeSaver{err: errSaveFailed}
	m := New(Options{Engine: eng, Save: saver.save, Config: config.Config{WatchDir: "/old"}})

	err := m.SetWatchDir("/new")
	if !errors.Is(err, errSaveFailed) {
		t.Errorf("SetWatchDir() error = %v, want it to wrap errSaveFailed", err)
	}
	if !errors.Is(err, revertErr) {
		t.Errorf("SetWatchDir() error = %v, want it to also wrap revertErr", err)
	}
}

func TestSetWatchDirEmptyMeansUnconfigured(t *testing.T) {
	t.Parallel()
	m := New(Options{Config: config.Config{WatchDir: "/old"}})
	if err := m.SetWatchDir(""); err != nil {
		t.Fatalf("SetWatchDir(\"\") error = %v", err)
	}
	if got := m.SentFolderPath(); got != "" {
		t.Errorf("SentFolderPath() = %q, want empty when unconfigured", got)
	}
}

func TestSetUploadWhileMachining(t *testing.T) {
	t.Parallel()
	eng := &fakeControl{}
	saver := &fakeSaver{}
	m := New(Options{Engine: eng, Save: saver.save})

	if err := m.SetUploadWhileMachining(true); err != nil {
		t.Fatalf("SetUploadWhileMachining() error = %v", err)
	}
	if !m.Watch().UploadWhileMachining {
		t.Error("Watch().UploadWhileMachining = false, want true")
	}
	if len(eng.uwm) != 1 || !eng.uwm[0] {
		t.Errorf("engine.uwm = %v, want [true]", eng.uwm)
	}
}

func TestSetUploadWhileMachiningSaveError(t *testing.T) {
	t.Parallel()
	eng := &fakeControl{}
	saver := &fakeSaver{err: errSaveFailed}
	m := New(Options{Engine: eng, Save: saver.save})

	err := m.SetUploadWhileMachining(true)
	if !errors.Is(err, errSaveFailed) {
		t.Errorf("error = %v, want errSaveFailed", err)
	}
	if m.Watch().UploadWhileMachining {
		t.Error("UploadWhileMachining = true, want reverted to false")
	}
	if len(eng.uwm) != 0 {
		t.Errorf("engine.uwm = %v, want no engine call after a save failure", eng.uwm)
	}

	// Nil engine and save: no-op, no panic.
	if err := New(Options{}).SetUploadWhileMachining(true); err != nil {
		t.Errorf("nil Engine/Save SetUploadWhileMachining() error = %v", err)
	}
}

func TestRetry(t *testing.T) {
	t.Parallel()
	eng := &fakeControl{}
	m := New(Options{Engine: eng})
	m.Retry("A.NC")
	if len(eng.retried) != 1 || eng.retried[0] != "A.NC" {
		t.Errorf("engine.retried = %v", eng.retried)
	}

	// Nil engine: no-op, no panic.
	New(Options{}).Retry("A.NC")
}

func TestSendFile(t *testing.T) {
	t.Parallel()
	eng := &fakeControl{}
	m := New(Options{Engine: eng})
	if err := m.SendFile("/watch/A.NC"); err != nil {
		t.Fatalf("SendFile() error = %v", err)
	}
	if len(eng.sent) != 1 || eng.sent[0] != "/watch/A.NC" {
		t.Errorf("engine.sent = %v", eng.sent)
	}

	wantErr := errors.New("busy")
	eng.sendErr = wantErr
	if err := m.SendFile("/watch/B.NC"); !errors.Is(err, wantErr) {
		t.Errorf("SendFile() error = %v, want wantErr", err)
	}
}

func TestSendFileNilEngine(t *testing.T) {
	t.Parallel()
	m := New(Options{})
	if err := m.SendFile("/watch/A.NC"); !errors.Is(err, ErrNoEngine) {
		t.Errorf("SendFile() error = %v, want ErrNoEngine", err)
	}
}

func TestRefreshTools(t *testing.T) {
	t.Parallel()
	eng := &fakeControl{}
	m := New(Options{Engine: eng})
	m.RefreshTools()
	if eng.refreshed != 1 {
		t.Errorf("engine.refreshed = %d, want 1", eng.refreshed)
	}

	// Nil engine: no-op, no panic.
	New(Options{}).RefreshTools()
}

func TestRetryEnabled(t *testing.T) {
	t.Parallel()
	m := New(Options{})
	now := time.Now()

	if m.RetryEnabled("A.NC") {
		t.Error("RetryEnabled(unknown) = true, want false")
	}

	m.Apply(engine.TransferEvent{Name: "A.NC", State: engine.Sending, At: now})
	if m.RetryEnabled("A.NC") {
		t.Error("RetryEnabled(Sending) = true, want false")
	}

	m.Apply(engine.TransferEvent{Name: "A.NC", State: engine.Failed, At: now})
	if !m.RetryEnabled("A.NC") {
		t.Error("RetryEnabled(Failed) = false, want true")
	}

	// A row for a different, terminal-state name is skipped, not matched.
	m2 := New(Options{})
	m2.Apply(engine.TransferEvent{Name: "B.NC", State: engine.Sent, At: now})
	if m2.RetryEnabled("A.NC") {
		t.Error("RetryEnabled(A.NC) = true with only an unrelated B.NC row, want false")
	}
}

func TestFolderPathUnconfigured(t *testing.T) {
	t.Parallel()
	m := New(Options{})
	if got := m.FolderPath(); got != "" {
		t.Errorf("FolderPath() = %q, want empty", got)
	}
	if got := m.SentFolderPath(); got != "" {
		t.Errorf("SentFolderPath() = %q, want empty", got)
	}
}

func TestNewDefaults(t *testing.T) {
	t.Parallel()
	m := New(Options{})
	if m.maxTransfers != defaultMaxTransfers {
		t.Errorf("maxTransfers = %d, want %d", m.maxTransfers, defaultMaxTransfers)
	}
	if m.maxLog != defaultMaxLog {
		t.Errorf("maxLog = %d, want %d", m.maxLog, defaultMaxLog)
	}
	if _, ok := m.clk.(clock.Real); !ok {
		t.Errorf("clk = %#v, want clock.Real{}", m.clk)
	}
}
