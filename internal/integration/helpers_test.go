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
	"context"
	"net"
	"os"
	"testing"
	"time"

	"msrl.dev/mink-lasso/internal/masso"
)

// discoverTimeout bounds every broadcast discovery this suite sends.
const discoverTimeout = 3 * time.Second

// requireSerial reads and parses MINK_LASSO_SERIAL, skipping the test with a
// clear message when it is unset.
func requireSerial(t *testing.T) uint32 {
	t.Helper()

	raw := os.Getenv("MINK_LASSO_SERIAL")
	if raw == "" {
		t.Skip("MINK_LASSO_SERIAL not set; skipping integration test (see README.md's \"Integration tests\" section)")
	}

	serial, err := masso.ParseSerial(raw)
	if err != nil {
		t.Fatalf("MINK_LASSO_SERIAL=%q does not parse as a controller serial: %v", raw, err)
	}

	return serial
}

// newClient constructs a masso.Client with default options and registers its
// Close for test cleanup.
func newClient(t *testing.T) *masso.Client {
	t.Helper()

	client, err := masso.NewClient(masso.Options{})
	if err != nil {
		t.Fatalf("masso.NewClient: %v", err)
	}

	t.Cleanup(func() { _ = client.Close() })

	return client
}

// connection is what connect establishes: a connected client plus the
// identity and config-reply data Connect returned.
type connection struct {
	client   *masso.Client
	identity masso.Identity
	cfg      masso.ConfigReply
	addr     *net.UDPAddr
}

// discoverController performs this suite's one broadcast discovery, and
// returns the address of the controller whose identity serial matches
// serial. It closes its client immediately, before returning, so the suite
// never holds two clients against the controller at once. Every later step
// reuses the returned address with connect (a unicast masso.Client.Connect)
// instead of broadcasting again: AGENTS.md asks to keep broadcasts to a
// minimum, since every one re-targets every controller that hears it.
func discoverController(t *testing.T, serial uint32) *net.UDPAddr {
	t.Helper()

	client, err := masso.NewClient(masso.Options{})
	if err != nil {
		t.Fatalf("masso.NewClient: %v", err)
	}

	defer func() {
		if closeErr := client.Close(); closeErr != nil {
			t.Errorf("closing discovery client: %v", closeErr)
		}
	}()

	found, err := client.Discover(context.Background(), discoverTimeout)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}

	for i := range found {
		if found[i].Identity.Serial == serial {
			return found[i].Addr
		}
	}

	t.Fatalf(
		"no controller with serial %s (%d) answered discovery within %s; %d controller(s) answered",
		masso.SerialString(serial), serial, discoverTimeout, len(found),
	)

	return nil
}

// connect connects to addr by unicast — masso.Client.Connect sends a
// unicast discovery request that only retargets addr's own replies, not a
// broadcast — and registers the resulting client for cleanup. It fails the
// test on any error.
func connect(t *testing.T, addr *net.UDPAddr) connection {
	t.Helper()

	client := newClient(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	identity, cfg, err := client.Connect(ctx, addr)
	if err != nil {
		t.Fatalf("Connect to %s: %v", addr, err)
	}

	return connection{client: client, identity: identity, cfg: cfg, addr: addr}
}

// machineIdle connects, reads one Status, and closes, reporting whether the
// controller is neither running a job nor waiting for the operator.
func machineIdle(t *testing.T, addr *net.UDPAddr) bool {
	t.Helper()

	conn := connect(t, addr)
	defer func() { _ = conn.client.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	go func() { _ = conn.client.Run(ctx) }()

	select {
	case st := <-conn.client.Status():
		return !st.Running && !st.WaitingForOperator
	case <-ctx.Done():
		t.Fatal("machineIdle: no status reply within 5s")

		return false
	}
}

// requireIdleOrAllowed skips the calling test unless the machine is idle or
// MINK_LASSO_ALLOW_RUNNING=1 permits running the upload tests anyway.
func requireIdleOrAllowed(t *testing.T, addr *net.UDPAddr) {
	t.Helper()

	if os.Getenv("MINK_LASSO_ALLOW_RUNNING") == "1" {
		return
	}

	if !machineIdle(t, addr) {
		t.Skip("machine is running or waiting for the operator; set MINK_LASSO_ALLOW_RUNNING=1 to run this test anyway")
	}
}

// isPrintableASCII reports whether every byte of s is printable ASCII
// (0x20-0x7E); an empty string is printable.
func isPrintableASCII(s string) bool {
	for i := range len(s) {
		if s[i] < 0x20 || s[i] > 0x7E {
			return false
		}
	}

	return true
}
