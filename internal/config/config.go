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

// Package config reads, writes, and validates mink-lasso's JSON config file.
package config

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"msrl.dev/mink-lasso/internal/masso"
)

const defaultListenPort = masso.ListenPortMin

// logLevels maps the accepted logLevel strings to their slog.Level, shared
// by Validate and SlogLevel so the two can never disagree.
var logLevels = map[string]slog.Level{
	"debug": slog.LevelDebug,
	"info":  slog.LevelInfo,
	"warn":  slog.LevelWarn,
	"error": slog.LevelError,
}

// writeFile is a package variable so tests can force Save's write-failure
// path deterministically on every OS: unlike a read-only directory (which
// does not block writes the same way on Windows), an injected failure here
// does not depend on platform permission-bit semantics.
var writeFile = os.WriteFile

// Config holds mink-lasso's persisted settings.
type Config struct {
	// Serial is the controller's serial number ("G3-12345" or "12345"),
	// used to find it by broadcast discovery. Empty means unconfigured.
	Serial string `json:"serial"`
	// Address is an optional host:port to try before falling back to
	// broadcast discovery. Empty means broadcast only.
	Address string `json:"address"`
	// LastAddress is the last-known address for Serial, persisted so
	// reconnects can try it first.
	LastAddress string `json:"lastAddress"`
	// WatchDir is the folder watched for new G-code files.
	WatchDir string `json:"watchDir"`
	// ListenPort is the first UDP port to try (up to 11050).
	ListenPort int `json:"listenPort"`
	// ScanInterval is how often the watch folder is rescanned.
	ScanInterval Duration `json:"scanInterval"`
	// SettleDelay is how long a file's size and modification time must be
	// unchanged before it is considered done being written.
	SettleDelay Duration `json:"settleDelay"`
	// PauseGrace is how long an idle, partially run job is held before
	// being treated as stopped, since feed hold and e-stop look the same
	// as "stopped" on the wire.
	PauseGrace Duration `json:"pauseGrace"`
	// Extensions lists the file extensions (with leading dot) watched for
	// upload.
	Extensions []string `json:"extensions"`
	// UploadWhileMachining allows uploads while the machine is running or
	// waiting for the operator.
	UploadWhileMachining bool `json:"uploadWhileMachining"`
	// MinimizeToTray hides the window to the tray icon instead of closing
	// it.
	MinimizeToTray bool `json:"minimizeToTray"`
	// StartMinimized starts the app with the window hidden.
	StartMinimized bool `json:"startMinimized"`
	// LogLevel is one of debug, info, warn, or error.
	LogLevel string `json:"logLevel"`
}

// Default returns the built-in default configuration.
func Default() Config {
	return Config{
		ListenPort:     defaultListenPort,
		ScanInterval:   Duration(2 * time.Second),
		SettleDelay:    Duration(3 * time.Second),
		PauseGrace:     Duration(2 * time.Minute),
		Extensions:     slices.Clone(masso.Extensions),
		MinimizeToTray: true,
		LogLevel:       "info",
	}
}

// DefaultPath returns the default config file path:
// <UserConfigDir>/mink-lasso/config.json. Service accounts can have no
// profile, so a userConfigDir failure falls back to a config.json next to
// the executable; if both fail, their errors are combined and returned.
// Callers pass os.UserConfigDir and os.Executable.
func DefaultPath(userConfigDir, executable func() (string, error)) (string, error) {
	dir, err := userConfigDir()
	if err == nil {
		return filepath.Join(dir, "mink-lasso", "config.json"), nil
	}

	exe, exeErr := executable()
	if exeErr != nil {
		return "", fmt.Errorf("%w: %w; %w", ErrDefaultPath, err, exeErr)
	}
	return filepath.Join(filepath.Dir(exe), "config.json"), nil
}

// Load reads and validates the config file at path. A missing file is not
// an error: Load fills in Default() and writes it to path, so the file
// exists on disk after the first run. Unknown JSON fields are rejected so
// typos surface; fields absent from the file keep their default value.
func Load(path string) (Config, error) {
	data, err := os.ReadFile(path) //nolint:gosec // path is caller-supplied by design; that's Load's contract
	if errors.Is(err, os.ErrNotExist) {
		cfg := Default()
		if saveErr := cfg.Save(path); saveErr != nil {
			return Config{}, fmt.Errorf("config: write default %s: %w", path, saveErr)
		}
		return cfg, nil
	}
	if err != nil {
		return Config{}, fmt.Errorf("config: read %s: %w", path, err)
	}

	cfg := Default()
	if err := json.Unmarshal(data, &cfg, json.RejectUnknownMembers(true)); err != nil {
		return Config{}, fmt.Errorf("config: parse %s: %w", path, err)
	}
	cfg = cfg.Normalize()

	if err := cfg.Validate(); err != nil {
		return Config{}, fmt.Errorf("config: %s: %w", path, err)
	}
	return cfg, nil
}

// Save writes c to path as indented JSON with a trailing newline. It
// creates path's directory if needed and replaces any existing file
// atomically by writing to a temp file in the same directory and renaming
// it into place.
func (c Config) Save(path string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("config: create %s: %w", dir, err)
	}

	data, err := json.Marshal(c, jsontext.WithIndent("  "))
	if err != nil {
		return fmt.Errorf("config: marshal: %w", err)
	}
	data = append(data, '\n')

	tmpPath := path + ".tmp"
	if err := writeFile(tmpPath, data, 0o600); err != nil {
		return fmt.Errorf("config: write %s: %w", tmpPath, err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("config: rename %s to %s: %w", tmpPath, path, err)
	}
	return nil
}

// Normalize returns a copy of c with fields in canonical form: Extensions
// are lowercased, so extension matching elsewhere in the application can be
// a simple case-sensitive comparison. Load calls this automatically;
// callers that build a Config by hand (e.g., from flags) should call it
// before use.
func (c Config) Normalize() Config {
	if len(c.Extensions) == 0 {
		return c
	}
	normalized := make([]string, len(c.Extensions))
	for i, ext := range c.Extensions {
		normalized[i] = strings.ToLower(ext)
	}
	c.Extensions = normalized
	return c
}

// Validate reports whether c's fields are self-consistent: ListenPort is in
// range, the durations are positive, LogLevel is recognized, every
// extension starts with a dot, and Serial (if set) parses.
func (c Config) Validate() error {
	if c.ListenPort < masso.ListenPortMin || c.ListenPort > masso.ListenPortMax {
		return fmt.Errorf("%w: %d", ErrListenPort, c.ListenPort)
	}
	if time.Duration(c.ScanInterval) <= 0 {
		return fmt.Errorf("%w: scanInterval %s", ErrNonPositiveDuration, time.Duration(c.ScanInterval))
	}
	if time.Duration(c.SettleDelay) <= 0 {
		return fmt.Errorf("%w: settleDelay %s", ErrNonPositiveDuration, time.Duration(c.SettleDelay))
	}
	if time.Duration(c.PauseGrace) <= 0 {
		return fmt.Errorf("%w: pauseGrace %s", ErrNonPositiveDuration, time.Duration(c.PauseGrace))
	}
	if _, ok := logLevels[c.LogLevel]; !ok {
		return fmt.Errorf("%w: %q", ErrLogLevel, c.LogLevel)
	}
	for _, ext := range c.Extensions {
		if !strings.HasPrefix(ext, ".") {
			return fmt.Errorf("%w: %q", ErrExtension, ext)
		}
	}
	if c.Serial != "" {
		if _, err := masso.ParseSerial(c.Serial); err != nil {
			return fmt.Errorf("%w: %q", ErrSerial, c.Serial)
		}
	}
	return nil
}

// SlogLevel returns the slog.Level for c.LogLevel, defaulting to
// slog.LevelInfo for an unrecognized value.
func (c Config) SlogLevel() slog.Level {
	if level, ok := logLevels[c.LogLevel]; ok {
		return level
	}
	return slog.LevelInfo
}
