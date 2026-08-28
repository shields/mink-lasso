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

	"msrl.dev/mink-lasso/internal/masso"
)

// ErrNoEngine is returned by an action that has no local-only meaning
// (SendFile) when Options.Engine was nil.
var ErrNoEngine = errors.New("model: no engine configured")

// ApplySerial parses and normalizes text as a controller serial, persists
// it, and reconfigures the engine's connection target. Nothing changes if
// text does not parse or persistence fails.
func (m *Model) ApplySerial(text string) error {
	serial, err := masso.ParseSerial(text)
	if err != nil {
		return err
	}
	normalized := masso.SerialString(serial)

	m.mu.Lock()
	old := m.cfg.Serial
	m.cfg.Serial = normalized
	save := m.save
	cfg := m.cfg
	eng := m.engine
	m.mu.Unlock()

	if save != nil {
		if err := save(cfg); err != nil {
			m.mu.Lock()
			m.cfg.Serial = old
			m.mu.Unlock()
			return err
		}
	}
	if eng != nil {
		eng.SetSerial(serial)
	}
	return nil
}

// SetWatchDir reconfigures the watch folder: the engine validates and
// restarts the watcher first, so a bad path is rejected before anything is
// persisted; nothing changes on either error.
func (m *Model) SetWatchDir(dir string) error {
	m.mu.Lock()
	eng := m.engine
	save := m.save
	old := m.cfg.WatchDir
	m.mu.Unlock()

	if eng != nil {
		if err := eng.SetWatchDir(dir); err != nil {
			return err
		}
	}

	m.mu.Lock()
	m.cfg.WatchDir = dir
	m.watchDir = dir
	cfg := m.cfg
	m.mu.Unlock()

	if save != nil {
		if err := save(cfg); err != nil {
			m.mu.Lock()
			m.cfg.WatchDir = old
			m.watchDir = old
			m.mu.Unlock()
			// The engine already committed to watching dir (and cleared
			// its queue for it) above; revert that too, or the running
			// watcher and the just-rolled-back config disagree about
			// which folder is active until the app restarts — contrary
			// to this method's doc, which promises nothing changes on
			// either error.
			if eng != nil {
				if revertErr := eng.SetWatchDir(old); revertErr != nil {
					return errors.Join(err, revertErr)
				}
			}
			return err
		}
	}
	return nil
}

// SetUploadWhileMachining updates the machine-gate override, persists it,
// and applies it to the engine. Nothing changes if persistence fails.
func (m *Model) SetUploadWhileMachining(v bool) error {
	m.mu.Lock()
	old := m.cfg.UploadWhileMachining
	m.cfg.UploadWhileMachining = v
	save := m.save
	cfg := m.cfg
	eng := m.engine
	m.mu.Unlock()

	if save != nil {
		if err := save(cfg); err != nil {
			m.mu.Lock()
			m.cfg.UploadWhileMachining = old
			m.mu.Unlock()
			return err
		}
	}
	if eng != nil {
		eng.SetUploadWhileMachining(v)
	}
	return nil
}

// Retry re-queues a Failed, Rejected, or SentUnfiled file immediately.
func (m *Model) Retry(name string) {
	m.mu.Lock()
	eng := m.engine
	m.mu.Unlock()
	if eng != nil {
		eng.Retry(name)
	}
}

// SendFile queues path as a manual send. It requires an engine: there is
// no local-only meaning for "send this file".
func (m *Model) SendFile(path string) error {
	m.mu.Lock()
	eng := m.engine
	m.mu.Unlock()
	if eng == nil {
		return ErrNoEngine
	}
	return eng.SendFile(path)
}

// RefreshTools re-fetches the tool table from the connected controller.
func (m *Model) RefreshTools() {
	m.mu.Lock()
	eng := m.engine
	m.mu.Unlock()
	if eng != nil {
		eng.RefreshTools()
	}
}

// FolderPath returns the configured watch folder path, or "" if
// unconfigured, for the binding to hand to winutil.OpenFolder.
func (m *Model) FolderPath() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.watchDir
}

// SentFolderPath returns the watch folder's sent/ subfolder path, or "" if
// no watch folder is configured.
func (m *Model) SentFolderPath() string {
	m.mu.Lock()
	dir := m.watchDir
	m.mu.Unlock()
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, "sent")
}

// RetryEnabled reports whether Retry(name) currently makes sense: name has
// a row in a retryable terminal state. There is only ever one row per
// name (see the transfers field's doc comment), so this reflects the
// engine's single, current item for name rather than any stale history.
func (m *Model) RetryEnabled(name string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()

	entry, ok := m.transfers[name]
	if !ok {
		return false
	}
	return entry.row.State.Retryable()
}
