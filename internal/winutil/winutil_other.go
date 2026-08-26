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

//go:build !windows

package winutil

import (
	"os"
	"runtime"
)

// goos selects the command OpenFolder runs. It defaults to runtime.GOOS but
// is a variable so tests can exercise both non-Windows branches without
// needing to run on both host platforms.
var goos = runtime.GOOS

// OpenDenyWrite opens path for reading. Non-Windows filesystems have no
// deny-write share mode, so — unlike the Windows implementation — this does
// not prevent another process from writing to the file while it stays open.
func OpenDenyWrite(path string) (*os.File, error) {
	return os.Open(path) //nolint:gosec // path is caller-supplied (the watch dir or a manual send), not untrusted input
}

// IsRemote reports whether path names a UNC share. Non-Windows platforms
// have no separate notion of a "mapped network drive," so UNC detection is
// all that applies here.
func IsRemote(path string) bool {
	return isUNC(path)
}

// SingleInstance is a no-op everywhere but Windows: mink-lasso's GUI is
// Windows-only, so a headless dev run under go run or go test never needs a
// single-instance guard.
func SingleInstance(_ string) (func(), error) {
	return func() {}, nil
}

// OpenFolder opens path, which must be a directory, in the platform's file
// manager. Passing a file's path instead is not portable: xdg-open and open
// launch a file in its default handler rather than revealing it in a
// folder view, unlike the Windows implementation (which selects a file
// passed to it inside Explorer).
func OpenFolder(path string) error {
	name := "xdg-open"
	if goos == "darwin" {
		name = "open"
	}

	return startCommand(name, path)
}
