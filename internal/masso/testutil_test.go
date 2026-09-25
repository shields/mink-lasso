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

// This file's helpers back the internal (package masso) test files only.
// The sim-dependent tests live in an external package masso_test, because
// internal/masso/sim imports internal/masso: a same-package test file
// cannot also import sim without creating an import cycle. Some of its
// helpers are duplicated in sim_testutil_test.go for that package, and
// export_test.go makes the others available to it.

package masso

import (
	"context"
	"log/slog"
	"net"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"msrl.dev/mink-lasso/internal/clock"
)

// nextTestPort hands out a distinct port on every call, to both this
// package's tests and masso_test's (through FreePortForTest), which share
// one test binary. Don't replace it with an OS-assigned free port: released
// for the caller to rebind, one can be handed to two parallel tests before
// either rebinds it. The series starts well clear of the package's default
// 11000-11051 range and below Linux's ephemeral range.
var nextTestPort = func() *atomic.Int32 {
	var p atomic.Int32
	p.Store(30000)
	return &p
}()

// freePort returns a port number private to this call, for a test to give
// NewClient as a single-port [PortMin, PortMax] range. It skips any port
// that something outside this test binary already holds.
func freePort(t *testing.T) int {
	t.Helper()
	for range maxPortProbes {
		if port := int(nextTestPort.Add(1)); canBind(t, port) {
			return port
		}
	}
	t.Fatalf("no bindable port in %d tries", maxPortProbes)
	return 0
}

// unansweredTargets returns an Options.DiscoveryTargets naming a private
// loopback port nothing listens on, so a Discover call sends a real packet
// that nothing answers instead of broadcasting on the real network.
func unansweredTargets(t *testing.T) []*net.UDPAddr {
	t.Helper()
	return []*net.UDPAddr{{IP: net.IPv4(127, 0, 0, 1), Port: freePort(t)}}
}

func canBind(t *testing.T, port int) bool {
	t.Helper()
	pc, err := net.ListenPacket("udp4", net.JoinHostPort("0.0.0.0", strconv.Itoa(port)))
	if err != nil {
		return false
	}
	if err := pc.Close(); err != nil {
		t.Fatalf("closing probe socket: %v", err)
	}
	return true
}

const maxPortProbes = 100

// newTestClient builds a Client on its own private port (via freePort) using
// cl for all timing, and registers its Close for test cleanup. Its Discover
// sends only to unansweredTargets.
func newTestClient(t *testing.T, cl clock.Clock) *Client {
	t.Helper()
	port := freePort(t)
	c, err := NewClient(Options{Clock: cl, PortMin: port, PortMax: port, DiscoveryTargets: unansweredTargets(t)})
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

// expectSilence asserts that no value arrives on ch within d — a real,
// short, bounded wait used only to confirm a negative for reader drop-path
// tests, mirroring internal/masso/sim's own test helper of the same
// purpose.
func expectSilence[T any](t *testing.T, ch <-chan T, d time.Duration) {
	t.Helper()
	select {
	case v := <-ch:
		t.Fatalf("expected silence, got %v", v)
	case <-time.After(d):
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

// clientAddr returns addr, dialable as the Client's own UDP address, given
// its LocalPort.
func clientAddr(c *Client) *net.UDPAddr {
	return &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: int(c.LocalPort())}
}

// mustSend writes pkt to addr from conn, failing the test on error.
func mustSend(t *testing.T, conn *net.UDPConn, addr *net.UDPAddr, pkt []byte) {
	t.Helper()
	if _, err := conn.WriteToUDP(pkt, addr); err != nil {
		t.Fatalf("WriteToUDP: %v", err)
	}
}

// signalHandler is an slog.Handler that signals ch, non-blockingly, on
// every log record.
type signalHandler struct{ ch chan struct{} }

func (signalHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h signalHandler) Handle(context.Context, slog.Record) error {
	select {
	case h.ch <- struct{}{}:
	default:
	}
	return nil
}

func (h signalHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h signalHandler) WithGroup(string) slog.Handler      { return h }

// captureLogger returns a Logger whose every log call also signals the
// returned channel. It lets a test deterministically wait for the reader
// goroutine to have finished processing a packet even when that processing
// leaves no other observable trace — such as a dropped packet with no
// registered waiter.
func captureLogger() (*slog.Logger, <-chan struct{}) {
	ch := make(chan struct{}, 8)
	return slog.New(signalHandler{ch}), ch
}

// messageSignalHandler is an slog.Handler that signals ch, non-blockingly,
// only for a log record whose message equals want. It lets a test wait
// deterministically for one specific event among many log calls.
type messageSignalHandler struct {
	ch   chan struct{}
	want string
}

func (messageSignalHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h messageSignalHandler) Handle(_ context.Context, r slog.Record) error {
	if r.Message == h.want {
		select {
		case h.ch <- struct{}{}:
		default:
		}
	}
	return nil
}

func (h messageSignalHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h messageSignalHandler) WithGroup(string) slog.Handler      { return h }
