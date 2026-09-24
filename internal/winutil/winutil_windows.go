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

//go:build windows

package winutil

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"unicode/utf16"

	"golang.org/x/sys/windows"
)

// OpenDenyWrite opens path for reading, denying every other process write
// access to it — the same check Explorer uses to decide that a file is "in
// use." The handle is held for as long as the returned file stays open.
func OpenDenyWrite(path string) (*os.File, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, &fs.PathError{Op: "open", Path: path, Err: err}
	}

	h, err := windows.CreateFile(
		p,
		windows.GENERIC_READ,
		windows.FILE_SHARE_READ,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL,
		0,
	)
	if err != nil {
		if errors.Is(err, windows.ERROR_SHARING_VIOLATION) {
			return nil, fmt.Errorf("open %s: %w", path, ErrBusy)
		}

		return nil, &fs.PathError{Op: "open", Path: path, Err: err}
	}

	return os.NewFile(uintptr(h), path), nil
}

// IsRemote reports whether path resolves to a UNC share or a mapped network
// drive.
func IsRemote(path string) bool {
	if isUNC(path) {
		return true
	}

	root := filepath.VolumeName(path)
	if root == "" {
		return false
	}

	// utf16.Encode never errors (unlike windows.UTF16PtrFromString): root is
	// always a clean drive-letter prefix such as "C:", so there is nothing
	// here that could produce an embedded NUL for GetDriveType to choke on.
	rootUTF16 := utf16.Encode([]rune(root + `\`))
	rootUTF16 = append(rootUTF16, 0)

	return windows.GetDriveType(&rootUTF16[0]) == windows.DRIVE_REMOTE
}

// IsHidden reports whether path has the Hidden attribute, as $RECYCLE.BIN
// and System Volume Information at a drive's root do. A folder with only the
// System attribute (as "attrib +s" leaves one) is not hidden: Explorer still
// shows it. It reports false if the attributes cannot be read.
func IsHidden(path string) bool {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return false
	}

	attrs, err := windows.GetFileAttributes(p)
	if err != nil {
		return false
	}

	return attrs&windows.FILE_ATTRIBUTE_HIDDEN != 0
}

// SingleInstance claims a machine-local, named mutex so that only one
// instance of the application runs at a time. On success it returns a
// release function that must be called — typically via defer — to give up
// the mutex before the process exits.
func SingleInstance(name string) (func(), error) {
	p, err := windows.UTF16PtrFromString(`Local\` + name)
	if err != nil {
		return nil, err
	}

	// CreateMutex returns a valid handle *and* ERROR_ALREADY_EXISTS when the
	// mutex already exists, so both must be checked before the ordinary
	// error case.
	h, err := windows.CreateMutex(nil, false, p)
	if errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		if h != 0 {
			// Best-effort: release has no error channel, and neither does this
			// early-exit path, so there is nothing productive to do with a
			// failure here.
			//nolint:errcheck // see comment above
			windows.CloseHandle(h)
		}

		return nil, ErrAlreadyRunning
	}
	if err != nil {
		return nil, err
	}

	return func() {
		//nolint:errcheck // release() has no error channel; see SingleInstance's doc comment
		windows.CloseHandle(h)
	}, nil
}

// OpenFolder opens path in Explorer. Explorer forwards the request to an
// existing Explorer process and exits immediately, so its own exit code
// carries no information — the process is started but never waited on.
// Unlike the non-Windows implementation, path may name either a directory
// (opened directly) or a file (selected inside its parent folder); callers
// that need portable behavior should only ever pass a directory.
func OpenFolder(path string) error {
	return startCommand("explorer.exe", path)
}
