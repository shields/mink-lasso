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
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestOpenDenyWriteOther(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "file.txt")
	if err := os.WriteFile(path, []byte("hello"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	f, err := OpenDenyWrite(path)
	if err != nil {
		t.Fatalf("OpenDenyWrite: %v", err)
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
	}
}

func TestIsRemoteOther(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		path string
		want bool
	}{
		{"UNC share", `\\server\share\file.txt`, true},
		{"local path", "/tmp", false},
		{"empty", "", false},
	}

	for _, tt := range tests {
		if got := IsRemote(tt.path); got != tt.want {
			t.Errorf("%s: IsRemote(%q) = %v, want %v", tt.name, tt.path, got, tt.want)
		}
	}
}

func TestSingleInstanceOther(t *testing.T) {
	t.Parallel()

	release, err := SingleInstance("mink-lasso-test")
	if err != nil {
		t.Fatalf("SingleInstance: %v", err)
	}
	release()

	// A second call must also succeed: SingleInstance is a no-op off Windows.
	release2, err := SingleInstance("mink-lasso-test")
	if err != nil {
		t.Fatalf("second SingleInstance: %v", err)
	}
	release2()
}

func TestOpenFolderOther(t *testing.T) {
	t.Parallel()

	origGoos, origCmd := goos, startCommand
	t.Cleanup(func() {
		goos = origGoos
		startCommand = origCmd
	})

	tests := []struct {
		goos     string
		wantName string
	}{
		{"darwin", "open"},
		{"linux", "xdg-open"},
	}

	for _, tt := range tests {
		goos = tt.goos

		var gotName string
		startCommand = func(name string, _ ...string) error {
			gotName = name
			return nil
		}

		if err := OpenFolder("/tmp/parts"); err != nil {
			t.Fatalf("OpenFolder(goos=%s): %v", tt.goos, err)
		}

		if gotName != tt.wantName {
			t.Errorf("goos=%s: command = %q, want %q", tt.goos, gotName, tt.wantName)
		}
	}

	wantErr := errors.New("boom")
	startCommand = func(string, ...string) error { return wantErr }

	if err := OpenFolder("/tmp/x"); !errors.Is(err, wantErr) {
		t.Errorf("OpenFolder error = %v, want %v", err, wantErr)
	}
}
