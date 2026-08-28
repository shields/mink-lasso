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

//go:build integration

package integration

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	"msrl.dev/mink-lasso/internal/masso"
)

var versionRE = regexp.MustCompile(`^(\d{8}\.\d+|dev)\r?\n$`)

// sentMessage is the text the engine actually writes to its slog log on a
// successful send (internal/engine/archive.go's setTerminal call), not the
// differently-capitalized "File sent" used for TransferEvent.Message/UI text.
const sentMessage = "engine: file sent"

// connectedMessage is the text internal/engine/conn.go's connLoop logs once
// the client has connected to the controller.
const connectedMessage = "engine: connected"

// testE2E starts the built exe headless against a temporary watch folder and
// confirms it discovers, connects, holds an open file, uploads it once
// closed, and moves it to sent/.
func testE2E(t *testing.T, addr *net.UDPAddr, serial uint16) {
	t.Helper()

	exe := os.Getenv("MINK_LASSO_EXE")
	if exe == "" {
		t.Skip("MINK_LASSO_EXE not set; skipping headless E2E test")
	}

	if _, err := os.Stat(exe); err != nil {
		t.Skipf("MINK_LASSO_EXE=%s: %v", exe, err)
	}

	requireIdleOrAllowed(t, addr)

	checkVersion(t, exe)

	conn := connect(t, addr)
	if err := conn.client.Close(); err != nil {
		t.Fatalf("closing discovery client before starting the exe: %v", err)
	}

	tmp := t.TempDir()
	watchDir := filepath.Join(tmp, "watch")
	logDir := filepath.Join(tmp, "logs")
	configPath := filepath.Join(tmp, "config.json")

	if err := os.MkdirAll(watchDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(%s): %v", watchDir, err)
	}

	cmd := exec.CommandContext(context.Background(), exe,
		"-headless",
		"-config", configPath,
		"-log-dir", logDir,
		"-watch", watchDir,
		"-serial", masso.SerialString(serial),
		"-address", addr.String(),
	)

	var out bytes.Buffer

	cmd.Stdout = &out
	cmd.Stderr = &out

	if err := cmd.Start(); err != nil {
		t.Fatalf("starting %s: %v", exe, err)
	}

	closed := false

	// A single cleanup, not two: cmd.Wait only returns once the goroutines
	// copying Stdout/Stderr into out have finished, so reading out.String()
	// is only safe after Wait — folding kill, wait, and the failure log
	// into one func guarantees that order regardless of t.Cleanup's LIFO
	// sequencing, rather than relying on registration order between two
	// separate cleanups.
	t.Cleanup(func() {
		if !closed {
			if cmd.Process != nil {
				_ = cmd.Process.Kill()
			}

			_ = cmd.Wait()
		}

		if t.Failed() {
			t.Logf("mink-lasso.exe output:\n%s", out.String())
		}
	})

	waitForConnected(t, logDir)
	writeAndHold(t, watchDir)
	waitForSent(t, watchDir)
	checkLogContainsSent(t, logDir)

	if err := cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		t.Fatalf("killing mink-lasso.exe: %v", err)
	}

	waitErr := cmd.Wait()
	closed = true

	if cmd.ProcessState == nil {
		t.Fatal("mink-lasso.exe did not exit after Kill")
	}

	t.Logf("mink-lasso.exe exited: %v (wait error: %v)", cmd.ProcessState, waitErr)
}

// checkVersion runs the exe with -version and checks its stdout against the
// documented format.
func checkVersion(t *testing.T, exe string) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	out, err := exec.CommandContext(ctx, exe, "-version").Output()
	if err != nil {
		if exitErr, ok := errors.AsType[*exec.ExitError](err); ok {
			t.Fatalf("%s -version: %v: %s", exe, err, exitErr.Stderr)
		}

		t.Fatalf("%s -version: %v", exe, err)
	}

	if !versionRE.Match(out) {
		t.Fatalf("%s -version output %q does not match %s", exe, out, versionRE)
	}
}

// pollUntil polls cond every 500ms until it returns true, failing the test
// via msg — called only once, at the moment of timeout, so it can report the
// most recently observed state — once timeout has elapsed.
func pollUntil(t *testing.T, timeout time.Duration, cond func() bool, msg func() string) {
	t.Helper()

	deadline := time.Now().Add(timeout)

	for {
		if cond() {
			return
		}

		if time.Now().After(deadline) {
			t.Fatal(msg())
		}

		time.Sleep(500 * time.Millisecond)
	}
}

// waitForConnected polls the exe's log file for up to 20s for
// connectedMessage, so writeAndHold's hold — and the arithmetic behind
// heldOpenHold, which is measured from the engine's connection — starts from
// a known point rather than an arbitrary moment in the exe's own startup.
func waitForConnected(t *testing.T, logDir string) {
	t.Helper()

	logPath := filepath.Join(logDir, "mink-lasso.log")

	var readErr error
	pollUntil(t, 20*time.Second, func() bool {
		var data []byte
		data, readErr = os.ReadFile(logPath)

		return readErr == nil && bytes.Contains(data, []byte(connectedMessage))
	}, func() string {
		return fmt.Sprintf("timed out after 20s waiting for %q in %s (log read error: %v)",
			connectedMessage, logPath, readErr)
	})
}

// heldOpenHold is how long writeAndHold keeps MLTEST3.NC open for writing
// without touching it. It is the sum, with margin, of every default delay
// that could otherwise let the file move before it is closed:
//   - internal/engine.Options.IdleHold (5s): how long status must show the
//     machine neither running nor waiting for the operator, after
//     connecting, before the upload gate opens.
//   - internal/watch.Options.Settle (3s) plus two of its default
//     Options.Interval (2s each = 4s): how long a stable (size, mtime) must
//     hold, across at least two scans, before the watcher calls a file
//     settled.
//
// 5s + 3s + 4s = 12s. If the engine's deny-write probe were not actually
// honoring this open handle, MLTEST3.NC would settle and move to sent/ well
// before that deadline.
const heldOpenHold = 12 * time.Second

// writeAndHold creates MLTEST3.NC in dir with all of its content written
// and synced in one call — unlike a real post-processor, which writes in
// place over time, this deliberately removes any change in size or mtime
// during the hold, so only the open handle itself, not a still-changing
// file, is what could be keeping it from settling. It then keeps the file
// open for writing, untouched, for heldOpenHold, polling every 500ms and
// failing immediately if the file moves to sent/ or disappears from dir —
// the actual proof that the engine's deny-write probe holds off on a file
// another process still has open for write. It closes the file before
// returning.
func writeAndHold(t *testing.T, dir string) {
	t.Helper()

	path := filepath.Join(dir, "MLTEST3.NC")
	sentPath := filepath.Join(dir, "sent", "MLTEST3.NC")

	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("creating %s: %v", path, err)
	}

	const content = "(mink-lasso integration test 3)\n" +
		"(held open for writing to exercise the deny-write probe)\n" +
		"M30\n"

	if _, err := f.WriteString(content); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}

	if err := f.Sync(); err != nil {
		t.Fatalf("syncing %s: %v", path, err)
	}

	deadline := time.Now().Add(heldOpenHold)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(sentPath); err == nil {
			_ = f.Close()
			t.Fatalf("MLTEST3.NC moved to sent/ while still open for writing, %s before the %s hold ended",
				time.Until(deadline), heldOpenHold)
		}

		if _, err := os.Stat(path); err != nil {
			_ = f.Close()
			t.Fatalf("MLTEST3.NC disappeared from watch/ while still open for writing: %v", err)
		}

		time.Sleep(500 * time.Millisecond)
	}

	if err := f.Close(); err != nil {
		t.Fatalf("closing %s: %v", path, err)
	}
}

// waitForSent polls up to 30s for watch/sent/MLTEST3.NC to exist and
// watch/MLTEST3.NC to be gone.
func waitForSent(t *testing.T, dir string) {
	t.Helper()

	path := filepath.Join(dir, "MLTEST3.NC")
	sentPath := filepath.Join(dir, "sent", "MLTEST3.NC")

	var sentErr, origErr error
	pollUntil(t, 30*time.Second, func() bool {
		_, sentErr = os.Stat(sentPath)
		_, origErr = os.Stat(path)

		return sentErr == nil && os.IsNotExist(origErr)
	}, func() string {
		return fmt.Sprintf("timed out waiting for MLTEST3.NC to move to sent/ (sent stat: %v, watch stat: %v)",
			sentErr, origErr)
	})
}

// checkLogContainsSent reads the exe's log file and checks for the success
// message.
func checkLogContainsSent(t *testing.T, logDir string) {
	t.Helper()

	logPath := filepath.Join(logDir, "mink-lasso.log")

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("reading %s: %v", logPath, err)
	}

	if !bytes.Contains(data, []byte(sentMessage)) {
		t.Errorf("%s does not contain %q:\n%s", logPath, sentMessage, data)
	}
}
