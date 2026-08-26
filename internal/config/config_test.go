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

package config

import (
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestDefault(t *testing.T) {
	t.Parallel()

	got := Default()
	want := Config{
		ListenPort:   11000,
		ScanInterval: Duration(2 * time.Second),
		SettleDelay:  Duration(3 * time.Second),
		PauseGrace:   Duration(2 * time.Minute),
		Extensions: []string{
			".nc", ".txt", ".cnc", ".tap", ".eia", ".htg", ".wiz", ".gcode", ".ngc",
		},
		MinimizeToTray: true,
		LogLevel:       "info",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Default() = %+v, want %+v", got, want)
	}
	if err := got.Validate(); err != nil {
		t.Errorf("Default().Validate() = %v, want nil", err)
	}
}

func TestConfig_Validate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr error
	}{
		{name: "default is valid", mutate: func(*Config) {}},
		{name: "min listen port", mutate: func(c *Config) { c.ListenPort = 11000 }},
		{name: "max listen port", mutate: func(c *Config) { c.ListenPort = 11050 }},
		{
			name:    "listen port too low",
			mutate:  func(c *Config) { c.ListenPort = 10999 },
			wantErr: ErrListenPort,
		},
		{
			name:    "listen port too high",
			mutate:  func(c *Config) { c.ListenPort = 11051 },
			wantErr: ErrListenPort,
		},
		{
			name:    "zero scan interval",
			mutate:  func(c *Config) { c.ScanInterval = 0 },
			wantErr: ErrNonPositiveDuration,
		},
		{
			name:    "negative scan interval",
			mutate:  func(c *Config) { c.ScanInterval = Duration(-time.Second) },
			wantErr: ErrNonPositiveDuration,
		},
		{
			name:    "zero settle delay",
			mutate:  func(c *Config) { c.SettleDelay = 0 },
			wantErr: ErrNonPositiveDuration,
		},
		{
			name:    "zero pause grace",
			mutate:  func(c *Config) { c.PauseGrace = 0 },
			wantErr: ErrNonPositiveDuration,
		},
		{
			name:    "bad log level",
			mutate:  func(c *Config) { c.LogLevel = "verbose" },
			wantErr: ErrLogLevel,
		},
		{
			name:    "empty log level",
			mutate:  func(c *Config) { c.LogLevel = "" },
			wantErr: ErrLogLevel,
		},
		{name: "debug log level", mutate: func(c *Config) { c.LogLevel = "debug" }},
		{name: "warn log level", mutate: func(c *Config) { c.LogLevel = "warn" }},
		{name: "error log level", mutate: func(c *Config) { c.LogLevel = "error" }},
		{
			name:    "extension without dot",
			mutate:  func(c *Config) { c.Extensions = []string{"nc"} },
			wantErr: ErrExtension,
		},
		{name: "no extensions", mutate: func(c *Config) { c.Extensions = nil }},
		{name: "serial unset", mutate: func(c *Config) { c.Serial = "" }},
		{name: "serial valid", mutate: func(c *Config) { c.Serial = "G3-12345" }},
		{
			name:    "serial invalid",
			mutate:  func(c *Config) { c.Serial = "not-a-serial" },
			wantErr: ErrSerial,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			c := Default()
			tt.mutate(&c)
			err := c.Validate()
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("Validate() error = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Validate() = %v, want nil", err)
			}
		})
	}
}

func TestConfig_Normalize(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   []string
		want []string
	}{
		{name: "lowercases", in: []string{".NC", ".Tap"}, want: []string{".nc", ".tap"}},
		{name: "already lower", in: []string{".nc"}, want: []string{".nc"}},
		{name: "nil", in: nil, want: nil},
		{name: "empty", in: []string{}, want: []string{}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			c := Config{Extensions: tt.in}
			got := c.Normalize().Extensions
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("Normalize().Extensions = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestConfig_SlogLevel(t *testing.T) {
	t.Parallel()

	tests := []struct {
		level string
		want  slog.Level
	}{
		{level: "debug", want: slog.LevelDebug},
		{level: "info", want: slog.LevelInfo},
		{level: "warn", want: slog.LevelWarn},
		{level: "error", want: slog.LevelError},
		{level: "bogus", want: slog.LevelInfo},
		{level: "", want: slog.LevelInfo},
	}

	for _, tt := range tests {
		t.Run(tt.level, func(t *testing.T) {
			t.Parallel()

			c := Config{LogLevel: tt.level}
			if got := c.SlogLevel(); got != tt.want {
				t.Errorf("SlogLevel() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestLoad_missingFile(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "does-not-exist.json")
	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if want := Default(); !reflect.DeepEqual(got, want) {
		t.Errorf("Load(missing) = %+v, want %+v", got, want)
	}

	// Load must write the defaults out so the file exists after the first
	// run, per the plan's stated contract.
	if _, statErr := os.Stat(path); statErr != nil {
		t.Fatalf("Load(missing) did not write the file: %v", statErr)
	}
	reloaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load (second time): %v", err)
	}
	if !reflect.DeepEqual(reloaded, got) {
		t.Errorf("Load() after first-run write = %+v, want %+v", reloaded, got)
	}
}

//nolint:paralleltest // mutates the package-level writeFile hook; must not run concurrently with other Save tests
func TestLoad_missingFile_writeError(t *testing.T) {
	// Injecting a failing writeFile, rather than a read-only directory,
	// forces Save's write-failure path deterministically on every OS: a
	// read-only directory does not block writes the same way on Windows.
	writeErr := errors.New("boom")
	original := writeFile
	writeFile = func(string, []byte, os.FileMode) error { return writeErr }
	t.Cleanup(func() { writeFile = original })

	// The directory exists (so os.ReadFile(path) fails with ErrNotExist,
	// taking the first-run branch), but the Save that should persist the
	// defaults fails. Load must surface that error rather than silently
	// returning Default() without writing it.
	path := filepath.Join(t.TempDir(), "config.json")
	if _, err := Load(path); !errors.Is(err, writeErr) {
		t.Fatalf("Load() error = %v, want it to wrap %v", err, writeErr)
	}
}

func TestLoad_readError(t *testing.T) {
	t.Parallel()

	// A directory in place of a file makes os.ReadFile fail with something
	// other than os.ErrNotExist.
	dir := t.TempDir()
	if _, err := Load(dir); err == nil {
		t.Fatal("Load(directory) = nil error, want an error")
	}
}

func TestLoad_malformedJSON(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("Load(malformed) = nil error, want an error")
	}
}

func TestLoad_unknownField(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"serial":"G3-1","bogusField":true}`), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("Load(unknown field) = nil error, want an error")
	}
}

func TestLoad_invalidAfterValidate(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"listenPort":1}`), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	_, err := Load(path)
	if !errors.Is(err, ErrListenPort) {
		t.Fatalf("Load(bad listenPort) error = %v, want ErrListenPort", err)
	}
}

func TestLoad_partialKeepsDefaults(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"serial":"G3-42"}`), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := Default()
	want.Serial = "G3-42"
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Load(partial) = %+v, want %+v", got, want)
	}
}

func TestLoad_normalizesExtensions(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"extensions":[".NC",".Tap"]}`), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := []string{".nc", ".tap"}
	if !reflect.DeepEqual(got.Extensions, want) {
		t.Errorf("Load(mixed-case extensions) = %#v, want %#v", got.Extensions, want)
	}
}

func TestSave_roundTrip(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "sub", "config.json")
	c := Default()
	c.Serial = "G3-9"
	c.WatchDir = "/tmp/watch"

	if err := c.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if len(data) == 0 || data[len(data)-1] != '\n' {
		t.Errorf("Save() did not end with a trailing newline: %q", data)
	}
	var indentCheck map[string]any
	if unmarshalErr := json.Unmarshal(data, &indentCheck); unmarshalErr != nil {
		t.Fatalf("saved file is not valid JSON: %v", unmarshalErr)
	}

	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !reflect.DeepEqual(got, c) {
		t.Errorf("round trip = %+v, want %+v", got, c)
	}
}

func TestSave_mkdirAllError(t *testing.T) {
	t.Parallel()

	// A regular file where a directory component needs to be makes
	// MkdirAll fail.
	base := t.TempDir()
	blocker := filepath.Join(base, "blocker")
	if err := os.WriteFile(blocker, []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	path := filepath.Join(blocker, "sub", "config.json")

	if err := Default().Save(path); err == nil {
		t.Fatal("Save() = nil error, want an error")
	}
}

//nolint:paralleltest // mutates the package-level marshalIndent hook; must not run concurrently with other Save tests
func TestSave_marshalError(t *testing.T) {
	errMarshal := errors.New("boom")
	original := marshalIndent
	marshalIndent = func(any, string, string) ([]byte, error) { return nil, errMarshal }
	t.Cleanup(func() { marshalIndent = original })

	path := filepath.Join(t.TempDir(), "config.json")
	err := Default().Save(path)
	if !errors.Is(err, errMarshal) {
		t.Fatalf("Save() error = %v, want it to wrap %v", err, errMarshal)
	}
}

//nolint:paralleltest // mutates the package-level writeFile hook; must not run concurrently with other Save tests
func TestSave_writeError(t *testing.T) {
	// See TestLoad_missingFile_writeError: injecting writeFile keeps this
	// deterministic on every OS instead of relying on permission bits.
	writeErr := errors.New("boom")
	original := writeFile
	writeFile = func(string, []byte, os.FileMode) error { return writeErr }
	t.Cleanup(func() { writeFile = original })

	path := filepath.Join(t.TempDir(), "config.json")
	if err := Default().Save(path); !errors.Is(err, writeErr) {
		t.Fatalf("Save() error = %v, want it to wrap %v", err, writeErr)
	}
}

func TestSave_renameError(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	// An existing directory at path makes the final rename fail: you
	// cannot rename a regular file onto a directory.
	if err := os.Mkdir(path, 0o750); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}

	if err := Default().Save(path); err == nil {
		t.Fatal("Save() = nil error, want an error")
	}
}

func TestDefaultPath_userConfigDir(t *testing.T) {
	t.Parallel()

	got, err := DefaultPath(
		func() (string, error) { return "/config", nil },
		func() (string, error) { t.Fatal("executable should not be called"); return "", nil },
	)
	if err != nil {
		t.Fatalf("DefaultPath: %v", err)
	}
	if want := filepath.Join("/config", "mink-lasso", "config.json"); got != want {
		t.Errorf("DefaultPath() = %s, want %s", got, want)
	}
}

func TestDefaultPath_fallsBackToExecutable(t *testing.T) {
	t.Parallel()

	errNoProfile := errors.New("no profile")
	got, err := DefaultPath(
		func() (string, error) { return "", errNoProfile },
		func() (string, error) { return "/opt/mink-lasso/mink-lasso.exe", nil },
	)
	if err != nil {
		t.Fatalf("DefaultPath: %v", err)
	}
	if want := filepath.Join("/opt/mink-lasso", "config.json"); got != want {
		t.Errorf("DefaultPath() = %s, want %s", got, want)
	}
}

func TestDefaultPath_bothFail(t *testing.T) {
	t.Parallel()

	errNoProfile := errors.New("no profile")
	errNoExe := errors.New("no exe")
	_, err := DefaultPath(
		func() (string, error) { return "", errNoProfile },
		func() (string, error) { return "", errNoExe },
	)
	if !errors.Is(err, ErrDefaultPath) {
		t.Fatalf("DefaultPath() error = %v, want it to wrap ErrDefaultPath", err)
	}
	if !errors.Is(err, errNoProfile) {
		t.Errorf("DefaultPath() error = %v, want it to wrap %v", err, errNoProfile)
	}
	if !errors.Is(err, errNoExe) {
		t.Errorf("DefaultPath() error = %v, want it to wrap %v", err, errNoExe)
	}
}
