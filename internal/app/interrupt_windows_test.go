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
	"testing"

	"golang.org/x/sys/windows"
)

// interruptSelf delivers the Windows equivalent of SIGINT to this process.
// Windows has no per-process signals — os.Process.Signal(os.Interrupt) is
// unsupported — and GenerateConsoleCtrlEvent reaches every process attached
// to the calling process's console, go test, make, and the CI shell
// included. So the test binary detaches from the inherited console and
// allocates a private one, which nothing else is attached to, before
// raising the event there. It raises Ctrl+Break rather than Ctrl+C because
// a parent that ignores Ctrl+C (SetConsoleCtrlHandler(NULL, TRUE)) passes
// that on to its children, whereas Ctrl+Break cannot be ignored; Go's
// runtime turns either into os.Interrupt.
func interruptSelf(t *testing.T) {
	t.Helper()

	kernel32 := windows.NewLazySystemDLL("kernel32.dll")
	freeConsole := kernel32.NewProc("FreeConsole")
	// Failing here only means there was no console to detach from.
	_ = callBool(freeConsole)
	if err := callBool(kernel32.NewProc("AllocConsole")); err != nil {
		t.Fatalf("AllocConsole: %v", err)
	}
	t.Cleanup(func() { _ = callBool(freeConsole) })

	if err := windows.GenerateConsoleCtrlEvent(windows.CTRL_BREAK_EVENT, 0); err != nil {
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
