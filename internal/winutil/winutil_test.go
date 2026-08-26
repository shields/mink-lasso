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

package winutil

import (
	"runtime"
	"testing"
)

func TestSentinelErrors(t *testing.T) {
	t.Parallel()

	if got, want := ErrBusy.Error(), "file is in use by another program"; got != want {
		t.Errorf("ErrBusy = %q, want %q", got, want)
	}

	if got, want := ErrAlreadyRunning.Error(), "another instance is already running"; got != want {
		t.Errorf("ErrAlreadyRunning = %q, want %q", got, want)
	}
}

// TestStartCommandRunsAProcess exercises the real (non-overridden)
// startCommand implementation with a harmless, always-present command.
//
// It deliberately does not call t.Parallel(): the OpenFolder tests override
// the package-level startCommand var during their own parallel-resumed
// phase, and this test must finish running the real implementation during
// the serial phase that precedes that, or the two would race on the var.
//
//nolint:paralleltest // must run serially, before parallel tests override startCommand
func TestStartCommandRunsAProcess(t *testing.T) {
	name := "true"

	var args []string
	if runtime.GOOS == "windows" {
		name = "cmd.exe"
		args = []string{"/c", "exit"}
	}

	if err := startCommand(name, args...); err != nil {
		t.Fatalf("startCommand(%s, %v): %v", name, args, err)
	}
}
