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

package engine

// This file exercises clientAdapter and watcherAdapter — the thin wrappers
// around a real *masso.Client and *watch.Watcher — over real loopback UDP
// and a real temp directory. It needs the sandbox's loopback-UDP
// restriction lifted to run (see AGENTS.md/CLAUDE.md); every other test in
// this package uses fakes instead.

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"msrl.dev/mink-lasso/internal/config"
	"msrl.dev/mink-lasso/internal/masso"
	"msrl.dev/mink-lasso/internal/watch"
)

// nextAdapterTestPort hands every test in this file (and conn_test.go,
// scheduler_test.go) its own private block of ten ports, well clear of the
// ranges internal/masso's own test files use, so parallel test binaries
// never collide on "address already in use". A single port would not be
// enough for newConnTestNewClient, which gives masso.NewClient a small
// range of its own so its retry absorbs a port a moment too slow to
// release from a just-finished test.
var nextAdapterTestPort = func() *atomic.Int32 {
	var p atomic.Int32
	p.Store(50000)
	return &p
}()

const adapterTestPortBlock = 10

func freeAdapterPort() int {
	return int(nextAdapterTestPort.Add(adapterTestPortBlock))
}

// unansweredDiscoveryTargets returns a masso.Options.DiscoveryTargets naming
// a private loopback port nothing listens on. Every real *masso.Client in
// this package's tests gets one, so its Discover sends a packet nothing
// answers rather than broadcasting on the real network, where every
// controller that hears the request would re-target its replies to the test.
func unansweredDiscoveryTargets() []*net.UDPAddr {
	return []*net.UDPAddr{{IP: net.IPv4(127, 0, 0, 1), Port: freeAdapterPort()}}
}

// newAdapterTestClient binds a real *masso.Client on its own private port
// with short reply timeouts, so an unanswered request fails fast.
func newAdapterTestClient(t *testing.T) *masso.Client {
	t.Helper()
	port := freeAdapterPort()
	c, err := masso.NewClient(masso.Options{
		// A single, non-retryable port (finding 5) intermittently fails
		// to bind when something else transiently holds it for a moment
		// (this range sits inside the OS ephemeral port range); the full
		// ten-port block reserved above gives masso.NewClient's own
		// retry loop room to move past that, mirroring newConnTestNewClient.
		PortMin:          port,
		PortMax:          port + adapterTestPortBlock - 1,
		ReplyTimeout:     time.Millisecond,
		DiscoveryTargets: unansweredDiscoveryTargets(),
	})
	if err != nil {
		t.Fatalf("masso.NewClient: %v", err)
	}
	t.Cleanup(func() {
		if err := c.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return c
}

// TestClientAdapterUnconnected drives every clientAdapter method against a
// real, never-connected *masso.Client: the ones that require a prior
// Connect fail fast with ErrNotConnected (no network wait), and Status,
// Remote, and Close behave as documented.
func TestClientAdapterUnconnected(t *testing.T) {
	t.Parallel()
	a := clientAdapter{newAdapterTestClient(t)}
	ctx := context.Background()

	if ch := a.Status(); ch == nil {
		t.Error("Status() returned a nil channel")
	}
	if remote := a.Remote(); remote != nil {
		t.Errorf("Remote() = %v, want nil before Connect", remote)
	}
	if _, err := a.Tools(ctx); !errors.Is(err, masso.ErrNotConnected) {
		t.Errorf("Tools() error = %v, want ErrNotConnected", err)
	}
	if err := a.Run(ctx); !errors.Is(err, masso.ErrNotConnected) {
		t.Errorf("Run() error = %v, want ErrNotConnected", err)
	}
	if err := a.Upload(ctx, "", "TEST.NC", strings.NewReader(""), 0, nil); !errors.Is(err, masso.ErrNotConnected) {
		t.Errorf("Upload() error = %v, want ErrNotConnected", err)
	}
}

// TestClientAdapterDiscovery drives the two network-sending adapter
// methods, Discover and Connect, against loopback addresses with no
// listener: both fail fast because of the short timeouts given.
func TestClientAdapterDiscovery(t *testing.T) {
	t.Parallel()
	a := clientAdapter{newAdapterTestClient(t)}
	ctx := context.Background()

	if _, err := a.Discover(ctx, time.Millisecond); err != nil {
		t.Errorf("Discover() error = %v, want nil (timeout is not an error)", err)
	}

	addr := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: freeAdapterPort()}
	if _, _, err := a.Connect(ctx, addr); !errors.Is(err, masso.ErrNoResponse) {
		t.Errorf("Connect() error = %v, want ErrNoResponse", err)
	}
}

// TestNewDefaultClient exercises New's own default for NewClient —
// defaultNewClient, over a real socket — rather than the fakes every other
// New test in engine_test.go injects.
func TestNewDefaultClient(t *testing.T) {
	t.Parallel()
	// ListenPort must fall in [masso.ListenPortMin, masso.ListenPortMax]:
	// normalize always sets ClientOptions.PortMax to ListenPortMax, so a
	// PortMin above it would fail to bind for a reason unrelated to what
	// this test checks.
	e, err := New(Options{Config: config.Config{ListenPort: masso.ListenPortMin + 33}})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	t.Cleanup(func() {
		if err := e.client.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
}

func TestClientAdapterClose(t *testing.T) {
	t.Parallel()
	a := clientAdapter{newAdapterTestClient(t)}
	if err := a.Close(); err != nil {
		t.Errorf("Close() error = %v", err)
	}
}

// TestWatcherAdapter drives every watcherAdapter method against a real
// *watch.Watcher on a temp directory.
func TestWatcherAdapter(t *testing.T) {
	t.Parallel()
	w, err := watch.New(watch.Options{Dir: t.TempDir()})
	if err != nil {
		t.Fatalf("watch.New: %v", err)
	}
	a := watcherAdapter{w}

	if a.Ready() == nil {
		t.Error("Ready() returned a nil channel")
	}
	if a.Rejected() == nil {
		t.Error("Rejected() returned a nil channel")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := a.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("Run() error = %v, want context.Canceled", err)
	}
}
