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

package app

import (
	"os"
	"os/exec"
	"syscall"
	"testing"
)

// childProcAttr is nil here: interruptChild signals the child by pid, so it
// needs no process group of its own.
func childProcAttr() *syscall.SysProcAttr { return nil }

// ensureConsole is a no-op: signals need no console.
func ensureConsole() {}

// interruptChild delivers a real SIGINT to the child, exactly what Ctrl+C
// or kill -INT does.
func interruptChild(t *testing.T, cmd *exec.Cmd) {
	t.Helper()
	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatalf("Signal: %v", err)
	}
}
