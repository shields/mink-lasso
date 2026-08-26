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

// Package masso_test holds the Client tests that need internal/masso/sim.
// internal/masso/sim imports internal/masso, so a same-package (package
// masso) test file cannot also import sim without an import cycle; these
// tests live in this separate, external test package instead, using only
// masso's exported API (plus the one test-only hook exported for this
// purpose in export_test.go). The remaining Client tests, which need
// unexported access but not sim, stay in package masso.
package masso_test

import (
	"net"
	"sync/atomic"
	"testing"
	"time"

	"msrl.dev/mink-lasso/internal/clock"
	"msrl.dev/mink-lasso/internal/masso"
	"msrl.dev/mink-lasso/internal/masso/sim"
)

// nextTestPort hands out a distinct port on every call. Earlier this asked
// the OS for a free port and released it for the caller to rebind, but
// under this package's heavy test parallelism two calls could both be
// handed the same just-released port before either rebound it, so a test's
// NewClient would intermittently fail with "address already in use". A
// monotonic counter gives every test a port none of the others will ever
// try; it starts well clear of both package masso's own default
// 11000-11050 range and testutil_test.go's counter (package masso compiles
// into this same test binary, seeded from 30000), so the two series cannot
// collide with each other either.
var nextTestPort = func() *atomic.Int32 {
	var p atomic.Int32
	p.Store(40000)
	return &p
}()

// freePort returns a port number private to this call, so a test can give
// NewClient a single-port [PortMin, PortMax] range.
func freePort(*testing.T) int {
	return int(nextTestPort.Add(1))
}

// newTestClient builds a Client on its own private port using cl for all
// timing, and registers its Close for test cleanup.
func newTestClient(t *testing.T, cl clock.Clock) *masso.Client {
	t.Helper()
	port := freePort(t)
	c, err := masso.NewClient(masso.Options{Clock: cl, PortMin: port, PortMax: port})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	t.Cleanup(func() {
		if err := c.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return c
}

// newSim starts a simulated controller on a random loopback port and
// registers its Close for test cleanup.
func newSim(t *testing.T, opts sim.Options) *sim.Controller {
	t.Helper()
	ctrl, err := sim.New(opts)
	if err != nil {
		t.Fatalf("sim.New: %v", err)
	}
	t.Cleanup(func() {
		if err := ctrl.Close(); err != nil {
			t.Errorf("sim Close: %v", err)
		}
	})
	return ctrl
}

// safetyNet bounds how long a test waits for something driven by real,
// asynchronous socket I/O (not by a fake clock) before failing — loopback
// UDP is effectively instant, so this only matters when something is
// actually broken.
const safetyNet = 5 * time.Second

// waitFor reads one value from ch, failing the test if none arrives within
// safetyNet. It is a bounded safety net for real, asynchronous I/O, not a
// timing-dependent synchronization mechanism: every test that uses it also
// arranges, via real socket delivery order or explicit fake-clock control,
// for the value to already be available almost immediately.
func waitFor[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(safetyNet):
		t.Fatal("timed out waiting for a result")
		panic("unreachable")
	}
}

// rawConn opens a plain UDP socket on loopback, standing in for a fake
// controller (or any raw sender) that writes bytes directly at a Client's
// port. It is closed on test cleanup.
func rawConn(t *testing.T) *net.UDPConn {
	t.Helper()
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatalf("ListenUDP: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// clientAddr returns addr, dialable as c's own UDP address.
func clientAddr(c *masso.Client) *net.UDPAddr {
	return &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: int(c.LocalPort())}
}

// mustSend writes pkt to addr from conn, failing the test on error.
func mustSend(t *testing.T, conn *net.UDPConn, addr *net.UDPAddr, pkt []byte) {
	t.Helper()
	if _, err := conn.WriteToUDP(pkt, addr); err != nil {
		t.Fatalf("WriteToUDP: %v", err)
	}
}

// testData returns n deterministic, non-repeating bytes for upload
// byte-exactness checks.
func testData(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i % 251) // 251 is prime, close to 256, avoids a short repeating period
	}
	return b
}
