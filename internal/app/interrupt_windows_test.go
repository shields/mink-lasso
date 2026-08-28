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

package app

import (
	"os/exec"
	"syscall"
	"testing"

	"golang.org/x/sys/windows"
)

// childProcAttr puts the child in a process group of its own, so that a
// console control event can be aimed at it alone.
func childProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
}

// ensureConsole gives a test binary that was started without a console (by
// a runner service, say) one to raise console control events on; a child
// started afterwards inherits it. Failure means a console already exists.
func ensureConsole() {
	_ = callBool(windows.NewLazySystemDLL("kernel32.dll").NewProc("AllocConsole"))
}

// interruptChild delivers the Windows equivalent of SIGINT to the child.
// Windows has no per-process signals — os.Process.Signal(os.Interrupt) is
// unsupported — and GenerateConsoleCtrlEvent reaches every process in the
// named process group that shares the caller's console, which is why the
// child has a group of its own: the same arrangement as the Ctrl+Break test
// in Go's own os/signal package. It raises Ctrl+Break rather than Ctrl+C
// because CREATE_NEW_PROCESS_GROUP disables Ctrl+C in the child; Go's
// runtime turns either into os.Interrupt.
func interruptChild(t *testing.T, cmd *exec.Cmd) {
	t.Helper()
	pid := cmd.Process.Pid
	if pid <= 0 {
		t.Fatalf("child pid = %d", pid)
	}
	if err := windows.GenerateConsoleCtrlEvent(windows.CTRL_BREAK_EVENT, uint32(pid)); err != nil {
		t.Fatalf("GenerateConsoleCtrlEvent: %v", err)
	}
}

// callBool invokes a kernel32 function whose BOOL result is zero on failure.
func callBool(p *windows.LazyProc) error {
	r, _, err := p.Call()
	if r == 0 {
		return err
	}

	return nil
}
