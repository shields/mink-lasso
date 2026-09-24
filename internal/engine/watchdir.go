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
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"msrl.dev/mink-lasso/internal/masso"
	"msrl.dev/mink-lasso/internal/watch"
	"msrl.dev/mink-lasso/internal/winutil"
)

// errNotADirectory is returned by SetWatchDir when opts.Stat succeeds but
// the path is not a directory.
var errNotADirectory = errors.New("engine: not a directory")

// watchLoop drives the watcher's lifecycle until ctx is done, mirroring
// connLoop: idle (emitting WatchState{Dir: ""}) while no folder is
// configured, otherwise build and run a Watcher against the current
// WatchDir, forwarding its Ready/Rejected events to the scheduler until
// SetWatchDir cancels the attempt to restart against a new folder.
func (e *Engine) watchLoop(ctx context.Context) {
	for ctx.Err() == nil {
		e.runWatchAttempt(ctx)
	}
}

func (e *Engine) runWatchAttempt(ctx context.Context) {
	attemptCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	// stopped is closed only after this attempt — including its forwarding
	// goroutine below — has fully exited, so SetWatchDir can join it before
	// clearing the queue: otherwise a Ready file the outgoing watcher was
	// already about to deliver could still land in the queue after the
	// clear, surviving under a folder that is no longer configured.
	stopped := make(chan struct{})
	defer close(stopped)

	e.mu.Lock()
	e.cancelWatch = cancel
	e.watchStopped = stopped
	dir := e.watchDir
	e.mu.Unlock()

	if dir == "" {
		e.dispatcher.emit(WatchState{Dir: ""})
		<-attemptCtx.Done()
		return
	}

	w, err := e.opts.NewWatcher(watch.Options{
		Dir:        dir,
		Interval:   time.Duration(e.opts.Config.ScanInterval),
		Settle:     time.Duration(e.opts.Config.SettleDelay),
		Extensions: e.opts.Config.Extensions,
		Validate:   validateUpload,
		Hidden:     winutil.IsHidden,
		Probe:      e.probeOpen,
		IsRemote:   e.opts.IsRemote,
		Clock:      e.opts.Clock,
		Logger:     e.opts.Logger,
	})
	if err != nil {
		e.dispatcher.emit(WatchState{Dir: dir, Err: err})
		<-attemptCtx.Done()
		return
	}

	// The watch package itself decides, internally, whether a directory
	// gets filesystem notifications or falls back to scanning on a timer
	// alone (a UNC path, IsRemote, or NewNotifier failing); it does not
	// expose which happened. IsRemote is the only one of those three the
	// engine can observe without duplicating that private logic, so
	// TimerOnly reports IsRemote's verdict — the case operators actually
	// need surfaced (a network share).
	e.dispatcher.emit(WatchState{Dir: dir, TimerOnly: e.opts.IsRemote(dir)})

	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-attemptCtx.Done():
				return
			case f := <-w.Ready():
				e.scheduler.ready(dir, f)
			case r := <-w.Rejected():
				e.scheduler.rejected(dir, r)
			}
		}
	}()

	if err := w.Run(attemptCtx); err != nil && ctx.Err() == nil {
		// attemptCtx, not ctx, was canceled: SetWatchDir is restarting the
		// watcher against a new folder, not shutting down — the error is
		// always just attemptCtx.Err() and is not worth surfacing louder
		// than this.
		e.opts.Logger.Debug("engine: watcher stopped", "dir", dir, "error", err)
	}
	<-done
}

func validateUpload(dir, name string) error {
	_, err := uploadTarget(dir, name)
	return err
}

// uploadTarget checks that the controller can take a file named name in
// the OS-native folder dir relative to the watch folder, and returns the
// controller folder it is uploaded into.
func uploadTarget(dir, name string) (string, error) {
	if err := masso.ValidateFileName(name); err != nil {
		return "", err
	}
	return uploadDir(dir)
}

// uploadDir converts a watcher's OS-native folder, relative to the watch
// folder, to the controller's backslash-separated form, "" for the drive
// root.
func uploadDir(dir string) (string, error) {
	return toUploadDir(dir, filepath.Separator)
}

// toUploadDir is uploadDir for a filesystem whose separator is sep. Where
// sep is not a backslash, a backslash can only be part of a folder's name,
// which the controller has no way to represent.
func toUploadDir(dir string, sep rune) (string, error) {
	if sep != '\\' && strings.ContainsRune(dir, '\\') {
		return "", fmt.Errorf("%w: %q has a folder name containing a backslash", masso.ErrBadUploadDir, dir)
	}
	remote := strings.ReplaceAll(dir, string(sep), `\`)
	if err := masso.ValidateUploadDir(remote); err != nil {
		return "", err
	}
	return remote, nil
}

// probeOpen reports a file busy for winutil.ErrBusy from opts.Open, closing
// the handle again immediately otherwise: the watcher only needs to know
// whether something else still has the file open for write.
func (e *Engine) probeOpen(path string) (bool, error) {
	f, err := e.opts.Open(path)
	if err != nil {
		if errors.Is(err, winutil.ErrBusy) {
			return true, nil
		}
		return false, err
	}
	return false, f.Close()
}

// SetWatchDir validates that dir exists and is a directory, then restarts
// the watcher against it, clearing every non-manual entry from the queue.
// An empty dir is always valid: it means "no watch folder configured".
func (e *Engine) SetWatchDir(dir string) error {
	if dir != "" {
		info, err := e.opts.Stat(dir)
		if err != nil {
			return fmt.Errorf("engine: watch dir: %w", err)
		}
		if !info.IsDir() {
			return fmt.Errorf("%w: %s", errNotADirectory, dir)
		}
	}

	e.mu.Lock()
	e.watchDir = dir
	cancel := e.cancelWatch
	stopped := e.watchStopped
	e.mu.Unlock()

	// Join the outgoing attempt before clearing the queue: it must have no
	// further chance to deliver a Ready/Rejected file from the old folder,
	// or that file would survive the clear under a folder that's no
	// longer configured.
	if cancel != nil {
		cancel()
	}
	if stopped != nil {
		<-stopped
	}
	e.scheduler.clearNonManual()
	return nil
}

// Retry re-queues a Failed, Rejected, or SentUnfiled file immediately;
// name is its TransferEvent.Name. Unknown names are ignored with a log
// line.
func (e *Engine) Retry(name string) {
	e.scheduler.retry(name)
}

// SendFile validates path's base name, opens path deny-write to get its
// authoritative size, and queues it at the front of the queue as a manual
// send to the controller's drive root, named by that base name wherever
// path is: subject to the machine gate like any other file, but never
// archived, and reported with Manual: true. It returns the validation or
// open error immediately so the caller can show it without waiting for the
// queue.
func (e *Engine) SendFile(path string) error {
	name := filepath.Base(path)
	if err := masso.ValidateFileName(name); err != nil {
		return err
	}
	f, err := e.opts.Open(path)
	if err != nil {
		return fmt.Errorf("engine: send file: %w", err)
	}
	defer func() {
		if closeErr := f.Close(); closeErr != nil {
			e.opts.Logger.Warn("engine: close file after manual send", "path", path, "error", closeErr)
		}
	}()

	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("engine: send file: %w", err)
	}

	e.scheduler.sendFile(name, path, info.Size(), info.ModTime())
	return nil
}
