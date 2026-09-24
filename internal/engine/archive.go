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
	"io/fs"
	"path/filepath"
	"strings"
	"time"

	"msrl.dev/mink-lasso/internal/watch"
)

// archiveTimestampLayout names a superseded backup in sent/ uniquely down
// to the second; a further collision (two sends within the same second)
// gets a "-N" suffix from uniqueBackupName.
const archiveTimestampLayout = "20060102-150405"

// archiveSent is called after a successful upload of src for a non-manual
// item: it moves the file into src.root's sent folder (watch.SentDir),
// under the same subfolder it was found in, preserving any existing file
// under that name by renaming it aside first with a timestamp, and retries
// the whole move on failure before giving up and reporting SentUnfiled. A
// manual send (SendFile) is never archived.
func (s *scheduler) archiveSent(ctx context.Context, it *item, src sendSource) {
	s.mu.Lock()
	manual, name := it.manual, it.name
	s.mu.Unlock()

	if manual {
		s.setTerminal(it, Sent, "File sent")
		return
	}

	sentDir := filepath.Join(src.root, watch.SentDir, src.dir)
	dest := filepath.Join(sentDir, src.base)
	path := src.path

	var lastErr error
	for attempt := 0; attempt <= s.e.opts.MoveRetries; attempt++ {
		if attempt > 0 {
			if !s.wait(ctx, s.e.opts.MoveRetryInterval) {
				break
			}
		}
		if err := s.moveIntoSent(sentDir, dest, path); err != nil {
			lastErr = err
			continue
		}
		s.setTerminal(it, Sent, "File sent")
		return
	}

	s.e.opts.Logger.Info("engine: could not archive sent file", "name", name, "error", lastErr)
	s.setTerminal(it, SentUnfiled, fmt.Sprintf("File sent, but could not move it to sent/: %v", lastErr))
}

// moveIntoSent performs one attempt of the archive move: create sentDir and
// any missing folders above it, rename any existing dest aside under a
// timestamped name, then rename path into dest.
func (s *scheduler) moveIntoSent(sentDir, dest, path string) error {
	if err := s.e.opts.MkdirAll(sentDir, 0o755); err != nil {
		return fmt.Errorf("engine: create sent dir: %w", err)
	}
	// Only a confirmed absence of dest skips the backup-aside step: any
	// other Stat error (permission, a transient network-share hiccup) is
	// treated as "dest might exist," not as "it doesn't," so a previously
	// archived file is never silently clobbered by the Rename below on
	// the strength of an inconclusive Stat.
	if _, err := s.e.opts.Stat(dest); err == nil || !errors.Is(err, fs.ErrNotExist) {
		backup := s.uniqueBackupName(dest)
		if err := s.e.opts.Rename(dest, backup); err != nil {
			return fmt.Errorf("engine: archive existing file: %w", err)
		}
	}
	if err := s.e.opts.Rename(path, dest); err != nil {
		return fmt.Errorf("engine: move into sent: %w", err)
	}
	return nil
}

// uniqueBackupName returns dest.<timestamp><ext>, appending "-N" if that
// name is already taken, so a superseded sent/ file is never overwritten.
func (s *scheduler) uniqueBackupName(dest string) string {
	ext := filepath.Ext(dest)
	base := strings.TrimSuffix(dest, ext)
	stamp := s.e.opts.Clock.Now().Format(archiveTimestampLayout)

	candidate := base + "." + stamp + ext
	for n := 1; ; n++ {
		// As above: only a confirmed absence of candidate makes it usable.
		// Any other Stat error must not be read as "free," or the Rename
		// this name feeds into could silently overwrite a file that is
		// actually still there.
		if _, err := s.e.opts.Stat(candidate); errors.Is(err, fs.ErrNotExist) {
			return candidate
		}
		candidate = fmt.Sprintf("%s.%s-%d%s", base, stamp, n, ext)
	}
}

// wait blocks for d or until ctx is done, reporting whether it waited the
// full duration (false means shutdown interrupted the retry loop).
func (s *scheduler) wait(ctx context.Context, d time.Duration) bool {
	t := s.e.opts.Clock.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C():
		return true
	case <-ctx.Done():
		return false
	}
}

// setTerminal records it's final state for this send attempt and emits it.
func (s *scheduler) setTerminal(it *item, state TransferState, msg string) {
	s.mu.Lock()
	it.state = state
	it.message = msg
	name := it.name
	ev := s.event(it)
	s.mu.Unlock()
	if state == Sent {
		s.e.opts.Logger.Info("engine: file sent", "name", name)
	}
	s.e.dispatcher.emit(ev)
}
