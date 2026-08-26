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
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestOpenDenyWriteWindows(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "busy.txt")
	if err := os.WriteFile(path, []byte("hello"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// os.OpenFile shares read+write on Windows, but our deny-write open must
	// still fail because this handle itself has write access.
	w, err := os.OpenFile(path, os.O_RDWR, 0o600)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}

	if _, err = OpenDenyWrite(path); !errors.Is(err, ErrBusy) {
		t.Fatalf("OpenDenyWrite while open for write: err = %v, want ErrBusy", err)
	}

	if err = w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	f, err := OpenDenyWrite(path)
	if err != nil {
		t.Fatalf("OpenDenyWrite after close: %v", err)
	}
	defer f.Close()

	got, err := io.ReadAll(f)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}

	if string(got) != "hello" {
		t.Errorf("content = %q, want %q", got, "hello")
	}

	if _, err = OpenDenyWrite(filepath.Join(dir, "missing.txt")); err == nil {
		t.Error("missing file: want error, got nil")
	} else if errors.Is(err, ErrBusy) {
		t.Errorf("missing file: err = %v, want not ErrBusy", err)
	}

	if _, err = OpenDenyWrite("bad\x00path"); err == nil {
		t.Error("path with embedded NUL: want error, got nil")
	}
}

func TestIsRemoteWindows(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		path string
		want bool
	}{
		{"local drive", `C:\`, false},
		{"UNC share", `\\server\share\file.txt`, true},
		{"no volume", `relative\path.txt`, false},
	}

	for _, tt := range tests {
		if got := IsRemote(tt.path); got != tt.want {
			t.Errorf("%s: IsRemote(%q) = %v, want %v", tt.name, tt.path, got, tt.want)
		}
	}
}

func TestSingleInstanceWindows(t *testing.T) {
	t.Parallel()

	name := "mink-lasso-test-single-instance"

	release, err := SingleInstance(name)
	if err != nil {
		t.Fatalf("first SingleInstance: %v", err)
	}

	if _, err = SingleInstance(name); !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("second SingleInstance: err = %v, want ErrAlreadyRunning", err)
	}

	release()

	release2, err := SingleInstance(name)
	if err != nil {
		t.Fatalf("SingleInstance after release: %v", err)
	}
	release2()
}

func TestSingleInstanceInvalidNameWindows(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		arg  string
	}{
		// UTF16PtrFromString rejects an embedded NUL.
		{"embedded NUL", "bad\x00name"},
		// CreateMutex rejects a backslash anywhere but the Local\/Global\ prefix.
		{"embedded backslash", `bad\name`},
	}

	for _, tt := range tests {
		release, err := SingleInstance(tt.arg)
		if err == nil {
			t.Errorf("%s: want error, got nil", tt.name)

			release()

			continue
		}

		if errors.Is(err, ErrAlreadyRunning) {
			t.Errorf("%s: err = %v, want not ErrAlreadyRunning", tt.name, err)
		}
	}
}

func TestOpenFolderWindows(t *testing.T) {
	t.Parallel()

	orig := startCommand
	t.Cleanup(func() { startCommand = orig })

	var gotName, gotArg string
	startCommand = func(name string, args ...string) error {
		gotName = name
		if len(args) > 0 {
			gotArg = args[0]
		}

		return nil
	}

	const path = `C:\Users\me\parts`
	if err := OpenFolder(path); err != nil {
		t.Fatalf("OpenFolder: %v", err)
	}

	if gotName != "explorer.exe" {
		t.Errorf("command = %q, want explorer.exe", gotName)
	}

	if gotArg != path {
		t.Errorf("arg = %q, want %q", gotArg, path)
	}

	wantErr := errors.New("boom")
	startCommand = func(string, ...string) error { return wantErr }

	if err := OpenFolder(path); !errors.Is(err, wantErr) {
		t.Errorf("OpenFolder error = %v, want %v", err, wantErr)
	}
}
