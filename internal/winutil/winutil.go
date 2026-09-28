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

// Package winutil isolates the Windows system calls mink-lasso needs — a
// deny-write file open, network-drive detection, a single-instance mutex,
// and opening a folder or showing a file in the file manager — behind a
// portable API. Every exported function also has a working non-Windows
// implementation, so the rest of the application never imports
// golang.org/x/sys/windows directly and every other package builds and
// tests on macOS and Linux.
package winutil

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// ErrBusy is returned by OpenDenyWrite when the file is already open for
// writing by another process.
var ErrBusy = errors.New("file is in use by another program")

// ErrAlreadyRunning is returned by SingleInstance when another instance of
// the application already holds the named mutex.
var ErrAlreadyRunning = errors.New("another instance is already running")

// isUNC reports whether path names a UNC share (\\server\share\...). It is
// a plain string check — rather than relying on path/filepath, whose
// VolumeName only recognizes UNC prefixes when compiled for GOOS=windows —
// so that the Windows and non-Windows implementations of IsRemote, and
// their tests, agree on what counts as a UNC path regardless of which OS
// compiled them.
func isUNC(path string) bool {
	return strings.HasPrefix(path, `\\`)
}

// startCommand runs name with args and returns once the process has
// started; it does not wait for the process to exit. It is a variable so
// tests can replace it instead of launching a real program.
var startCommand = func(name string, args ...string) error {
	//nolint:gosec // name is one of a fixed set of OS commands, not attacker input
	return exec.Command(name, args...).Start()
}

// ShowInFolder opens the folder containing path, a file, in the platform's
// file manager, selecting the file where the platform supports it. If the
// file no longer exists — a sent file someone has since deleted, say — its
// folder is opened instead, and if that is gone too the error says so:
// asked for a missing path, the file manager would open some unrelated
// default location. These checks can block for as long as a network share
// takes to time out, so call this off the UI thread.
func ShowInFolder(path string) error {
	path = absPath(path)

	if _, err := os.Stat(path); err == nil {
		return selectInFolder(path)
	}

	dir := filepath.Dir(path)
	if _, err := os.Stat(dir); err != nil {
		return err
	}

	return OpenFolder(dir)
}

// absPath resolves a relative path (from a relative -watch flag, say)
// against the working directory, leaving it unchanged in the unlikely case
// that the working directory is unknown. The file manager may be an
// already-running process with a different working directory, so it must
// never be handed a relative path.
func absPath(path string) string {
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}

	return path
}
