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
	"os/exec"
	"path/filepath"
	"syscall"
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

// OpenFolder opens path, which must be a directory, in Explorer. Explorer
// forwards the request to an existing Explorer process and exits
// immediately, so its own exit code carries no information — the process is
// started but never waited on. (Given a file instead, Explorer would open it
// in its default program; ShowInFolder is the way to reveal a file.)
func OpenFolder(path string) error {
	return startCommand("explorer.exe", absPath(path))
}

// selectInFolder opens path's folder in Explorer with path selected.
// Explorer parses its own command line and wants the quotes around the path
// only, as in /select,"C:\my parts\F.NC"; exec.Command would quote the
// whole argument instead whenever the path contains a space, and Explorer
// then ignores it and opens a default folder. So the command line is built
// here by hand. A Windows path cannot contain a double quote, so wrapping
// it in quotes needs no escaping.
func selectInFolder(path string) error {
	return startCommandLine("explorer.exe", `explorer.exe /select,"`+path+`"`)
}

// startCommandLine is startCommand for a program that parses its command
// line itself: cmdLine, which must begin with the program name, is passed
// to CreateProcess exactly as given. It is a variable so tests can replace
// it instead of launching a real program.
var startCommandLine = func(name, cmdLine string) error {
	cmd := exec.Command(name)
	cmd.SysProcAttr = &syscall.SysProcAttr{CmdLine: cmdLine}

	return cmd.Start()
}
