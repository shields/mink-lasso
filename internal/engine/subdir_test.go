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

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"msrl.dev/mink-lasso/internal/masso"
)

func mkdirAll(t *testing.T, path string) string {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	return path
}

func TestSchedulerSendsSubfolderFileAndArchivesNested(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	archived := writeFile(t, mkdirAll(t, filepath.Join(dir, "sent", "OLD")), "X.NC", []byte("archived"))

	s := newConnTestSim(t, 2101)
	opts := schedTestOptions(2101, dir)
	opts.Config.Address = s.Addr().String()
	opts.NewClient = newConnTestNewClient(freeAdapterPort())

	e, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	events, _ := runEngine(t, e)
	waitForEvent(t, events, isConnState(Connected))

	want := []byte("G0 X0 Y0\n")
	writeFile(t, mkdirAll(t, filepath.Join(dir, "JOBS", "SUB")), "PART.NC", want)
	name := filepath.Join("JOBS", "SUB", "PART.NC")

	var sentNames []string
	waitForEvent(t, events, func(ev Event) bool {
		te, ok := ev.(TransferEvent)
		if ok && strings.HasPrefix(te.Name, "sent") {
			sentNames = append(sentNames, te.Name)
		}
		return ok && te.Name == name && te.State == Sent
	})
	if len(sentNames) > 0 {
		t.Errorf("transfer events for files in sent/: %q", sentNames)
	}

	remote := masso.JoinUploadPath(`JOBS\SUB`, "PART.NC")
	if got, ok := s.File(remote); !ok || string(got) != string(want) {
		t.Errorf("sim file %q = (%q, %v), want %q", remote, got, ok, want)
	}
	if files := s.Files(); len(files) != 1 {
		t.Errorf("sim received %d files, want only %q", len(files), remote)
	}
	if _, err := os.Stat(filepath.Join(dir, name)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("%s still in the watch folder (Stat error %v), want moved into sent/", name, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "sent", name)); err != nil {
		t.Errorf("sent/%s: %v", name, err)
	}
	if _, err := os.Stat(archived); err != nil {
		t.Errorf("sent/OLD/X.NC: %v", err)
	}
}

func TestSchedulerRejectsSubfolderControllerCannotTake(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s := newConnTestSim(t, 2102)
	opts := schedTestOptions(2102, dir)
	opts.Config.Address = s.Addr().String()
	opts.NewClient = newConnTestNewClient(freeAdapterPort())

	e, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	events, _ := runEngine(t, e)
	waitForEvent(t, events, isConnState(Connected))

	bad := []string{"30°"}
	if runtime.GOOS != "windows" {
		// Neither can be relied on to exist on Windows: a colon is
		// reserved there, and a folder path past masso.MaxUploadDir
		// exceeds MAX_PATH.
		bad = append(bad, "A:B", filepath.Join(strings.Repeat("D", 128), strings.Repeat("E", 128)))
	}
	preds := make([]func(Event) bool, 0, len(bad))
	for _, sub := range bad {
		writeFile(t, mkdirAll(t, filepath.Join(dir, sub)), "A.NC", []byte("x"))
		name := filepath.Join(sub, "A.NC")
		preds = append(preds, func(ev Event) bool {
			te, ok := ev.(TransferEvent)
			if !ok || te.Name != name {
				return false
			}
			if te.State != Rejected {
				t.Errorf("%s reported %v, want Rejected", name, te.State)
				return false
			}
			if !strings.Contains(te.Message, masso.ErrBadUploadDir.Error()) {
				t.Errorf("%s Rejected with %q, want the invalid-folder reason", name, te.Message)
			}
			return true
		})
	}
	waitForEvents(t, events, preds...)

	if files := s.Files(); len(files) != 0 {
		t.Errorf("sim received %d files, want none", len(files))
	}
}

func TestSchedulerManualSendFileFromSubfolderGoesToRoot(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s := newConnTestSim(t, 2103)
	opts := schedTestOptions(2103, "")
	opts.Config.Address = s.Addr().String()
	opts.NewClient = newConnTestNewClient(freeAdapterPort())

	e, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	events, _ := runEngine(t, e)
	waitForEvent(t, events, isConnState(Connected))

	path := writeFile(t, mkdirAll(t, filepath.Join(dir, "JOBS")), "M.NC", []byte("manual"))
	if err := e.SendFile(path); err != nil {
		t.Fatalf("SendFile: %v", err)
	}

	ev := waitForEvent(t, events, isTransferEvent("M.NC", Sent))
	if !asTransferEvent(t, ev).Manual {
		t.Error("Sent event Manual = false, want true")
	}
	if got, ok := s.File("M.NC"); !ok || string(got) != "manual" {
		t.Errorf("sim root M.NC = (%q, %v), want %q", got, ok, "manual")
	}
	if _, ok := s.File(masso.JoinUploadPath("JOBS", "M.NC")); ok {
		t.Error("manual send went into JOBS on the controller, want the drive root")
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("manual send moved the file: %v", err)
	}
}

func TestSchedulerManualSendFileInWatchedSubfolderSharesRow(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s := newConnTestSim(t, 2104)
	opts := schedTestOptions(2104, dir)
	opts.Config.Address = s.Addr().String()
	opts.NewClient = newConnTestNewClient(freeAdapterPort())

	e, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	events, _ := runEngine(t, e)
	waitForEvent(t, events, isConnState(Connected))

	const size = 4 << 20 // enough transfer time to call SendFile mid-flight
	content := bytes.Repeat([]byte{'a'}, size)
	path := writeFile(t, mkdirAll(t, filepath.Join(dir, "JOBS")), "PART.NC", content)
	name := filepath.Join("JOBS", "PART.NC")
	waitForEvent(t, events, isTransferEvent(name, Sending))

	if err := e.SendFile(path); err != nil {
		t.Fatalf("SendFile: %v", err)
	}

	for {
		ev := waitForEvent(t, events, isTransferEvent(name, Sent))
		if !asTransferEvent(t, ev).Manual {
			t.Error("Sent event Manual = false, want true (SendFile arrived mid-transfer)")
		}
		if settledState(t, e, name) == Sent {
			break
		}
	}

	e.scheduler.mu.Lock()
	_, rootLevelEntry := e.scheduler.items["PART.NC"]
	_, sharedEntry := e.scheduler.items[name]
	numItems := len(e.scheduler.items)
	e.scheduler.mu.Unlock()
	if rootLevelEntry {
		t.Error("manual send created a second, root-level entry for PART.NC")
	}
	if !sharedEntry {
		t.Errorf("no item under the watcher's own key %q", name)
	}
	if numItems != 1 {
		t.Errorf("scheduler has %d items, want 1 (manual and automatic share one row)", numItems)
	}

	if _, ok := s.File("PART.NC"); ok {
		t.Error("manual send went to the drive root, want the matching JOBS folder")
	}
	remote := masso.JoinUploadPath("JOBS", "PART.NC")
	if got, ok := s.File(remote); !ok || !bytes.Equal(got, content) {
		t.Errorf("sim %q = (%d bytes, %v), want the full upload", remote, len(got), ok)
	}
	if files := s.Files(); len(files) != 1 {
		t.Errorf("sim received %d files, want exactly 1 (no duplicate upload)", len(files))
	}

	if _, err := os.Stat(path); err != nil {
		t.Errorf("%s should remain in the watch dir (manual sends are not archived): %v", name, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "sent", name)); !errors.Is(err, os.ErrNotExist) {
		t.Error("PART.NC must not be archived into sent/ after a manual SendFile merged with the watcher's queue")
	}
}
