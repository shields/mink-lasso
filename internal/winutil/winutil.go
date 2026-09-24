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
// deny-write file open, network-drive and hidden-folder detection, a
// single-instance mutex, and opening a folder in the file manager — behind a
// portable API. Every
// exported function also has a working non-Windows implementation, so the
// rest of the application never imports golang.org/x/sys/windows directly
// and every other package builds and tests on macOS and Linux.
package winutil

import (
	"errors"
	"os/exec"
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
