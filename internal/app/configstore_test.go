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
	"os"
	"path/filepath"
	"testing"

	"msrl.dev/mink-lasso/internal/config"
)

func readConfig(t *testing.T, path string) config.Config {
	t.Helper()
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}

	return cfg
}

func TestConfigStoreSavePreservesLastAddress(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "config.json")

	s := newConfigStore(path, config.Config{LastAddress: "1.2.3.4:5"})

	c := config.Default()
	c.UploadWhileMachining = true
	if err := s.save(c); err != nil {
		t.Fatalf("save: %v", err)
	}

	got := readConfig(t, path)
	if got.LastAddress != "1.2.3.4:5" {
		t.Errorf("LastAddress = %q, want %q", got.LastAddress, "1.2.3.4:5")
	}
	if !got.UploadWhileMachining {
		t.Error("UploadWhileMachining not persisted")
	}
}

func TestConfigStorePersistLastAddressThenSaveKeepsIt(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "config.json")

	s := newConfigStore(path, config.Default())

	if err := s.persistLastAddress("10.0.0.1:11000"); err != nil {
		t.Fatalf("persistLastAddress: %v", err)
	}

	// A later save from the model, whose own copy of Config never learned
	// about the address, must not clobber it.
	c := config.Default()
	c.UploadWhileMachining = true
	if err := s.save(c); err != nil {
		t.Fatalf("save: %v", err)
	}

	got := readConfig(t, path)
	if got.LastAddress != "10.0.0.1:11000" {
		t.Errorf("LastAddress = %q, want %q", got.LastAddress, "10.0.0.1:11000")
	}
	if !got.UploadWhileMachining {
		t.Error("UploadWhileMachining not persisted")
	}
}

func TestConfigStorePersistLastAddressNoopWhenUnchanged(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "config.json")
	s := newConfigStore(path, config.Config{LastAddress: "1.2.3.4:5"})

	if err := s.persistLastAddress("1.2.3.4:5"); err != nil {
		t.Fatalf("persistLastAddress: %v", err)
	}

	if _, err := os.Stat(path); err == nil {
		t.Error("expected no file to have been written for a no-op persist")
	}
}

// unsaveablePath returns a path whose directory cannot be created: it names
// a location under a plain file, so config.Config.Save's os.MkdirAll fails.
func unsaveablePath(t *testing.T) string {
	t.Helper()
	blocker := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	return filepath.Join(blocker, "config.json")
}

func TestConfigStoreSaveError(t *testing.T) {
	t.Parallel()
	s := newConfigStore(unsaveablePath(t), config.Default())
	if err := s.save(config.Default()); err == nil {
		t.Error("save: want an error, got nil")
	}
}

func TestConfigStorePersistLastAddressError(t *testing.T) {
	t.Parallel()
	s := newConfigStore(unsaveablePath(t), config.Default())
	if err := s.persistLastAddress("1.2.3.4:5"); err == nil {
		t.Error("persistLastAddress: want an error, got nil")
	}
}
