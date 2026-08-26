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

	"github.com/fsnotify/fsnotify"
)

// Notifier is a source of filesystem-change hints for a single directory. It
// exists so Watcher can fall back to timer-only scanning when none is
// available, and so tests can inject one without touching a real filesystem.
type Notifier interface {
	// Events delivers a hint whenever the watched directory may have
	// changed. The hint carries no information — Watcher treats every one
	// alike and rescans — so implementations may coalesce them.
	Events() <-chan struct{}
	// Errors delivers any error from the underlying watch, including a
	// dropped-events overflow.
	Errors() <-chan error
	// Close releases the underlying watch. Events and Errors are not
	// guaranteed to be readable after Close returns.
	Close() error
}

const (
	// fsEventBuffer sizes fsnotify's own Events channel. The default is
	// unbuffered, which under a burst of writes can make fsnotify block and
	// then drop events rather than report ErrEventOverflow.
	fsEventBuffer = 256
	// fsWindowsBufferBytes sizes the ReadDirectoryChangesW kernel buffer.
	// Windows-only; harmless elsewhere. The fsnotify default (64KiB) can
	// overflow when a post-processor writes many files in a burst.
	fsWindowsBufferBytes = 256 << 10
)

// NewFSNotifier watches dir, non-recursively, using fsnotify.
func NewFSNotifier(dir string) (Notifier, error) {
	return newFSNotifierWith(dir, fsnotify.NewBufferedWatcher)
}

// newFSNotifierWith is NewFSNotifier's implementation, taking the buffered-
// watcher constructor as a parameter so a test can make it fail without
// needing to exhaust real OS watch resources.
func newFSNotifierWith(dir string, newBuffered func(uint) (*fsnotify.Watcher, error)) (Notifier, error) {
	fw, err := newBuffered(fsEventBuffer)
	if err != nil {
		return nil, err
	}
	if err := fw.AddWith(dir, fsnotify.WithBufferSize(fsWindowsBufferBytes)); err != nil {
		return nil, errors.Join(err, fw.Close())
	}

	n := &fsNotifier{
		w:      fw,
		events: make(chan struct{}, 1),
		errors: make(chan error, 1),
	}
	go n.run()
	return n, nil
}

// fsNotifier adapts an *fsnotify.Watcher to Notifier: every event, of any
// op, becomes a hint, and every error is forwarded as-is.
type fsNotifier struct {
	w      *fsnotify.Watcher
	events chan struct{}
	errors chan error
}

func (n *fsNotifier) run() {
	for {
		select {
		case _, ok := <-n.w.Events:
			if !ok {
				return
			}
			select {
			case n.events <- struct{}{}:
			default:
			}

		case err, ok := <-n.w.Errors:
			if !ok {
				return
			}
			select {
			case n.errors <- err:
			default:
			}
		}
	}
}

func (n *fsNotifier) Events() <-chan struct{} { return n.events }

func (n *fsNotifier) Errors() <-chan error { return n.errors }

func (n *fsNotifier) Close() error { return n.w.Close() }
