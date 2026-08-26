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
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"
)

// TestNewFSNotifierRealFilesystem is the one place a real filesystem's own
// timing is unavoidable: it watches an actual directory and waits, with a
// generous deadline rather than a sleep, for a real create to be reported.
func TestNewFSNotifierRealFilesystem(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	n, err := NewFSNotifier(dir)
	if err != nil {
		t.Fatalf("NewFSNotifier: %v", err)
	}
	t.Cleanup(func() { _ = n.Close() })

	path := filepath.Join(dir, "job.nc")
	if err := os.WriteFile(path, []byte("G0 X0\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	select {
	case <-n.Events():
	case err := <-n.Errors():
		t.Fatalf("unexpected notifier error: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for a filesystem notification")
	}
}

func TestNewFSNotifierMissingDir(t *testing.T) {
	t.Parallel()

	dir := filepath.Join(t.TempDir(), "does-not-exist")
	if _, err := NewFSNotifier(dir); err == nil {
		t.Fatal("NewFSNotifier: expected an error for a directory that does not exist")
	}
}

func TestNewFSNotifierBufferedWatcherError(t *testing.T) {
	t.Parallel()

	injected := errors.New("injected NewBufferedWatcher failure")
	_, err := newFSNotifierWith(t.TempDir(), func(uint) (*fsnotify.Watcher, error) {
		return nil, injected
	})
	if !errors.Is(err, injected) {
		t.Fatalf("newFSNotifierWith error = %v, want %v", err, injected)
	}
}

// TestFSNotifierCoalescesEvents drives fsNotifier.run directly against a
// hand-built *fsnotify.Watcher (its Events/Errors fields are exported and
// its zero-value backend is never touched here), so the coalescing and
// shutdown logic can be exercised deterministically instead of racing a real
// filesystem. Each additional send only completes once run has fully
// finished processing the previous one — that ordering, not timing, is what
// guarantees the second event lands on the buffered channel while it is
// still full from the first.
func TestFSNotifierCoalescesEvents(t *testing.T) {
	t.Parallel()

	events := make(chan fsnotify.Event)
	fw := &fsnotify.Watcher{Events: events, Errors: make(chan error)}
	n := &fsNotifier{w: fw, events: make(chan struct{}, 1), errors: make(chan error, 1)}

	exited := make(chan struct{})
	go func() {
		n.run()
		close(exited)
	}()

	events <- fsnotify.Event{}
	events <- fsnotify.Event{}
	events <- fsnotify.Event{} // sync pin: proves the second event's processing (a coalesced drop) is complete

	<-n.Events()

	close(events)
	select {
	case <-exited:
	case <-time.After(5 * time.Second):
		t.Fatal("run did not exit after Events closed")
	}
}

// TestFSNotifierCoalescesErrors is TestFSNotifierCoalescesEvents' mirror for
// the Errors side.
func TestFSNotifierCoalescesErrors(t *testing.T) {
	t.Parallel()

	errA := errors.New("a")
	errs := make(chan error)
	fw := &fsnotify.Watcher{Events: make(chan fsnotify.Event), Errors: errs}
	n := &fsNotifier{w: fw, events: make(chan struct{}, 1), errors: make(chan error, 1)}

	exited := make(chan struct{})
	go func() {
		n.run()
		close(exited)
	}()

	errs <- errA
	errs <- errors.New("b")
	errs <- errors.New("c") // sync pin, as above

	if got := <-n.Errors(); !errors.Is(got, errA) {
		t.Fatalf("Errors() = %v, want %v", got, errA)
	}

	close(errs)
	select {
	case <-exited:
	case <-time.After(5 * time.Second):
		t.Fatal("run did not exit after Errors closed")
	}
}
