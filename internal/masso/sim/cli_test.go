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

package sim_test

import (
	"bytes"
	"context"
	"net"
	"runtime"
	"strings"
	"testing"
	"time"

	"msrl.dev/mink-lasso/internal/masso"
	"msrl.dev/mink-lasso/internal/masso/sim"
)

func TestMain_FlagError(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	if got := sim.Main(t.Context(), []string{"-bogus"}, &out); got != 2 {
		t.Fatalf("Main() = %d, want 2", got)
	}
}

func TestMain_SerialOutOfRange(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	if got := sim.Main(t.Context(), []string{"-serial", "4294967296"}, &out); got != 2 {
		t.Fatalf("Main() = %d, want 2", got)
	}
	if !strings.Contains(out.String(), "out of range") {
		t.Fatalf("output = %q, want it to explain the out-of-range serial", out.String())
	}
}

func TestMain_ListenError(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	if got := sim.Main(t.Context(), []string{"-addr", "bad"}, &out); got != 1 {
		t.Fatalf("Main() = %d, want 1", got)
	}
	if !strings.Contains(out.String(), "masso-sim:") {
		t.Fatalf("output = %q, want an error message", out.String())
	}
}

// TestMain_RunAndUpload starts Main against a real loopback socket, drives
// a full discovery/tool-query/upload exchange against it as a real client
// would, and checks it logs the request stream and the completed upload
// before exiting cleanly on context cancellation.
func TestMain_RunAndUpload(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	var out syncBuffer
	done := make(chan int, 1)
	go func() {
		done <- sim.Main(ctx, []string{"-addr", "127.0.0.1:0", "-serial", "5", "-tools", "Drill,Mill"}, &out)
	}()

	addr := waitForListenAddr(t, &out)

	client := newClient(t)
	discover(t, client, addr)

	mustWrite(t, client, addr, masso.ToolQuery(1))
	rec, ok := readReply(t, client).(masso.ToolRecord)
	if !ok || rec.Name != "Drill" {
		t.Fatalf("tool record = %+v (ok=%v), want Name \"Drill\"", rec, ok)
	}

	data := []byte("G0 X0\n")
	uploadFile(t, client, addr, "CLI.NC", data)

	waitUntil(t, func() bool { return strings.Contains(out.String(), "stored CLI.NC (6 bytes)") })

	cancel()
	if got := <-done; got != 0 {
		t.Fatalf("Main() = %d, want 0", got)
	}

	if !strings.Contains(out.String(), "listening on") {
		t.Fatalf("output = %q, want it to print the listening address", out.String())
	}
}

// waitForListenAddr polls out for the "listening on <addr>" line Main
// prints once its socket is bound, and parses that address.
func waitForListenAddr(t *testing.T, out *syncBuffer) *net.UDPAddr {
	t.Helper()
	const prefix = "listening on "
	waitUntil(t, func() bool { return strings.Contains(out.String(), prefix) })
	_, after, _ := strings.Cut(out.String(), prefix)
	line, _, _ := strings.Cut(after, "\n")
	addr, err := net.ResolveUDPAddr("udp", strings.TrimSpace(line))
	if err != nil {
		t.Fatalf("ResolveUDPAddr(%q): %v", line, err)
	}
	return addr
}

// waitUntil polls cond, failing the test rather than hanging if it never
// becomes true (for example when the socket cannot be bound at all).
func waitUntil(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for the simulator")
		}
		runtime.Gosched()
	}
}
