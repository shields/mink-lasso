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

package app

import (
	"sync"

	"msrl.dev/mink-lasso/internal/config"
)

// configStore is the single writer of the config file, so that the model's
// own saves (triggered by user actions) and the app's LastAddress updates
// (triggered by a successful connection) never race or clobber each other.
// Every save merges onto the last-written Config: a save from the model
// carries forward whatever LastAddress the app most recently persisted, and
// a LastAddress update carries forward whatever the model most recently
// saved. LastAddress stays in the one config file, rather than in a file of
// its own, because config.json is documented as the single, hand-editable
// place every setting lives; this merge is the price of that.
type configStore struct {
	mu      sync.Mutex
	path    string
	current config.Config
}

func newConfigStore(path string, cfg config.Config) *configStore {
	return &configStore{path: path, current: cfg}
}

// save persists c, overriding c.LastAddress with the store's own so a save
// from the model (whose copy of Config only ever changes the field the
// action touched) cannot revert a LastAddress the app already persisted.
func (s *configStore) save(c config.Config) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	c.LastAddress = s.current.LastAddress

	return s.commit(c)
}

// persistLastAddress records addr as the current config's LastAddress and
// saves it, leaving every other field as last written. It is a no-op if
// addr is already current.
func (s *configStore) persistLastAddress(addr string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.current.LastAddress == addr {
		return nil
	}
	c := s.current
	c.LastAddress = addr

	return s.commit(c)
}

// commit saves c and, on success, remembers it as the current config. The
// caller must already hold s.mu.
func (s *configStore) commit(c config.Config) error {
	if err := c.Save(s.path); err != nil {
		return err
	}
	s.current = c

	return nil
}
