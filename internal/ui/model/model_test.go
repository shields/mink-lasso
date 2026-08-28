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
	"sync"

	"msrl.dev/mink-lasso/internal/config"
)

// fakeControl is a test double for Control, recording every call so tests
// can assert what the model asked the engine to do.
type fakeControl struct {
	mu sync.Mutex

	serials     []uint16
	watchDirs   []string
	watchDirErr error
	// watchDirErrs, if non-nil, supplies a distinct error per SetWatchDir
	// call by call index (a short slice covers only the first few calls;
	// any call past the end, or when this is nil, falls back to
	// watchDirErr) — lets a test make an initial call succeed and a later
	// one (e.g. SetWatchDir's revert-on-save-failure) fail, or vice versa.
	watchDirErrs []error
	uwm          []bool
	refreshed    int
	retried      []string
	sent         []string
	sendErr      error
}

func (f *fakeControl) SetSerial(serial uint16) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.serials = append(f.serials, serial)
}

func (f *fakeControl) SetWatchDir(dir string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	idx := len(f.watchDirs)
	f.watchDirs = append(f.watchDirs, dir)
	if idx < len(f.watchDirErrs) {
		return f.watchDirErrs[idx]
	}
	return f.watchDirErr
}

func (f *fakeControl) SetUploadWhileMachining(v bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.uwm = append(f.uwm, v)
}

func (f *fakeControl) RefreshTools() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.refreshed++
}

func (f *fakeControl) Retry(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.retried = append(f.retried, name)
}

func (f *fakeControl) SendFile(path string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, path)
	return f.sendErr
}

// fakeSaver is a test double for Options.Save.
type fakeSaver struct {
	mu   sync.Mutex
	cfgs []config.Config
	err  error
}

func (f *fakeSaver) save(cfg config.Config) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cfgs = append(f.cfgs, cfg)
	return f.err
}

var errSaveFailed = errors.New("simulated save failure")
