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

package masso

import (
	"errors"
	"net"
	"runtime"
	"testing"
	"time"

	"msrl.dev/mink-lasso/internal/clock"
)

// badAddr is a net.Addr with no "host:port" form, standing in for a
// directed-broadcast destination directedBroadcastAddr cannot compute from.
type badAddr struct{}

func (badAddr) Network() string { return "fake" }
func (badAddr) String() string  { return "not-an-address" }

func TestDiscoverEmptyWhenNothingAnswers(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	fc := clock.NewFake(time.Unix(0, 0))
	c := newTestClient(t, fc)
	// No addDiscoveryTarget: the limited broadcast always sends
	// successfully but nothing on this test machine will answer it, so
	// this exercises "sent, nothing answered" rather than a send failure.
	resultCh := make(chan struct {
		found []Found
		err   error
	}, 1)
	go func() {
		found, err := c.Discover(ctx, time.Second)
		resultCh <- struct {
			found []Found
			err   error
		}{found, err}
	}()
	fc.BlockUntil(1)
	fc.Advance(time.Second)

	res := waitFor(t, resultCh)
	if res.err != nil {
		t.Fatalf("Discover error = %v, want nil", res.err)
	}
	if len(res.found) != 0 {
		t.Fatalf("found = %v, want empty", res.found)
	}
}

// fakeAddr is a net.Addr that is not a *net.UDPAddr, used to force
// Discover's source-address type assertion to fail — a real socket always
// reports *net.UDPAddr, so this can only happen with an injected
// Options.ListenPacket.
type fakeAddr struct{ s string }

func (fakeAddr) Network() string  { return "fake" }
func (a fakeAddr) String() string { return a.s }

func TestDiscoverIgnoresNonUDPSourceAddr(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	identityPkt := Identity{Serial: 1}.Encode()

	// Bound to 0.0.0.0, not 127.0.0.1: a loopback-bound socket cannot send
	// to a broadcast address at all (EADDRNOTAVAIL on macOS), which would
	// make sendDiscoveryBroadcast fail before Discover ever creates its
	// collection timer.
	realConn, err := net.ListenPacket("udp", "0.0.0.0:0")
	if err != nil {
		t.Fatalf("ListenPacket: %v", err)
	}
	// readFrom blocks on release before returning the bad-addr reply, so
	// the test can guarantee Discover has already registered its waiter
	// before the reply is delivered — otherwise it could instead be
	// dropped as "no waiter" before Discover ever starts collecting.
	release := make(chan struct{})
	var delivered bool
	fc := &fakeConn{PacketConn: realConn, readFrom: func(p []byte) (int, net.Addr, error) {
		if !delivered {
			<-release
			delivered = true
			n := copy(p, identityPkt)
			return n, fakeAddr{"not-udp"}, nil
		}
		return realConn.ReadFrom(p)
	}}
	port := freePort(t)
	c, err := NewClient(Options{
		Clock: clock.Real{}, PortMin: port, PortMax: port,
		ListenPacket: func(string, string) (net.PacketConn, error) { return fc, nil },
		Interfaces:   func() ([]net.Interface, error) { return nil, nil }, // avoid this machine's real interfaces
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer func() { _ = c.Close() }()

	resultCh := make(chan struct {
		found []Found
		err   error
	}, 1)
	go func() {
		found, err := c.Discover(ctx, 300*time.Millisecond)
		resultCh <- struct {
			found []Found
			err   error
		}{found, err}
	}()

	// Mirrors internal/clock's own tests, which wait on a goroutine
	// reaching a blocking point by polling its internal state rather than
	// sleeping.
	for {
		c.mu.Lock()
		n := len(c.waiters[TypeDiscovery])
		c.mu.Unlock()
		if n > 0 {
			break
		}
		runtime.Gosched()
	}
	close(release)

	res := waitFor(t, resultCh)
	if res.err != nil {
		t.Fatalf("Discover error = %v, want nil", res.err)
	}
	if len(res.found) != 0 {
		t.Fatalf("found = %v, want empty (non-UDP source addr must be ignored)", res.found)
	}
}

func TestSendDiscoveryBroadcastLogsInterfaceEnumFailure(t *testing.T) {
	t.Parallel()
	port := freePort(t)
	enumErr := errors.New("enum boom")
	c, err := NewClient(Options{
		PortMin: port, PortMax: port,
		Interfaces: func() ([]net.Interface, error) { return nil, enumErr },
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer func() { _ = c.Close() }()

	// The limited broadcast still sends successfully despite the
	// enumeration failure, so this must not return an error — it only
	// needs to exercise the log line taken along the way.
	if err := c.sendDiscoveryBroadcast(); err != nil {
		t.Fatalf("sendDiscoveryBroadcast: %v", err)
	}
}

func TestDiscoverSendFailure(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	writeErr := errors.New("write boom")
	realConn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("ListenPacket: %v", err)
	}
	fc := &fakeConn{PacketConn: realConn, writeTo: func([]byte, net.Addr) (int, error) { return 0, writeErr }}
	port := freePort(t)
	c, err := NewClient(Options{
		PortMin: port, PortMax: port,
		ListenPacket: func(string, string) (net.PacketConn, error) { return fc, nil },
		Interfaces:   func() ([]net.Interface, error) { return nil, nil },
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer func() { _ = c.Close() }()

	_, err = c.Discover(ctx, time.Second)
	if !errors.Is(err, ErrDiscoverySend) {
		t.Fatalf("Discover = %v, want ErrDiscoverySend", err)
	}
}

func TestDiscoveryDestinationsIncludesDirectedBroadcast(t *testing.T) {
	t.Parallel()
	ifaceUp := net.Interface{Name: "en-test", Flags: net.FlagUp}
	ifaceDown := net.Interface{Name: "down-test", Flags: 0}
	ifaceLoopback := net.Interface{Name: "lo-test", Flags: net.FlagUp | net.FlagLoopback}

	_, ipNet, err := net.ParseCIDR("192.168.50.1/24")
	if err != nil {
		t.Fatalf("ParseCIDR: %v", err)
	}
	ipNet.IP = net.ParseIP("192.168.50.1").To4()

	addrsFn := func(i *net.Interface) ([]net.Addr, error) {
		switch i.Name {
		case "en-test":
			return []net.Addr{ipNet}, nil
		case "down-test", "lo-test":
			t.Fatalf("InterfaceAddrs called for %s, which should have been skipped", i.Name)
		default:
		}
		return nil, nil
	}

	port := freePort(t)
	c, err := NewClient(Options{
		PortMin: port, PortMax: port,
		Interfaces:     func() ([]net.Interface, error) { return []net.Interface{ifaceDown, ifaceLoopback, ifaceUp}, nil },
		InterfaceAddrs: addrsFn,
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer func() { _ = c.Close() }()

	dests, err := c.discoveryDestinations()
	if err != nil {
		t.Fatalf("discoveryDestinations: %v", err)
	}
	want := &net.UDPAddr{IP: net.ParseIP("192.168.50.255").To4(), Port: ControllerPort}
	found := false
	for _, d := range dests {
		if d.IP.Equal(want.IP) && d.Port == want.Port {
			found = true
		}
	}
	if !found {
		t.Errorf("dests = %v, want to include %v", dests, want)
	}
	// The limited broadcast is always present too.
	if dests[0].IP.String() != net.IPv4bcast.String() {
		t.Errorf("dests[0] = %v, want the limited broadcast", dests[0])
	}
}

func TestDiscoveryDestinationsDeduplicatesExtraTargets(t *testing.T) {
	t.Parallel()
	port := freePort(t)
	c, err := NewClient(Options{
		PortMin: port, PortMax: port,
		Interfaces: func() ([]net.Interface, error) { return nil, nil },
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer func() { _ = c.Close() }()

	limited := &net.UDPAddr{IP: net.IPv4bcast, Port: ControllerPort}
	c.addDiscoveryTarget(limited) // duplicate of the always-present limited broadcast
	c.addDiscoveryTarget(&net.UDPAddr{IP: net.ParseIP("10.0.0.5"), Port: ControllerPort})
	c.addDiscoveryTarget(&net.UDPAddr{IP: net.ParseIP("10.0.0.5"), Port: ControllerPort}) // duplicate

	dests, err := c.discoveryDestinations()
	if err != nil {
		t.Fatalf("discoveryDestinations: %v", err)
	}
	if len(dests) != 2 {
		t.Fatalf("dests = %v, want 2 entries after deduplication", dests)
	}
}

func TestDiscoveryDestinationsInterfaceEnumFailure(t *testing.T) {
	t.Parallel()
	port := freePort(t)
	enumErr := errors.New("enum boom")
	c, err := NewClient(Options{
		PortMin: port, PortMax: port,
		Interfaces: func() ([]net.Interface, error) { return nil, enumErr },
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer func() { _ = c.Close() }()

	dests, err := c.discoveryDestinations()
	if !errors.Is(err, ErrInterfaceEnum) {
		t.Fatalf("err = %v, want ErrInterfaceEnum", err)
	}
	if len(dests) != 1 {
		t.Fatalf("dests = %v, want just the limited broadcast", dests)
	}
}

func TestDiscoveryDestinationsSkipsInterfaceAddrsError(t *testing.T) {
	t.Parallel()
	port := freePort(t)
	iface := net.Interface{Name: "bad-addrs", Flags: net.FlagUp}
	addrsErr := errors.New("addrs boom")
	c, err := NewClient(Options{
		PortMin: port, PortMax: port,
		Interfaces:     func() ([]net.Interface, error) { return []net.Interface{iface}, nil },
		InterfaceAddrs: func(*net.Interface) ([]net.Addr, error) { return nil, addrsErr },
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer func() { _ = c.Close() }()

	dests, err := c.discoveryDestinations()
	if err != nil {
		t.Fatalf("discoveryDestinations: %v", err)
	}
	if len(dests) != 1 {
		t.Fatalf("dests = %v, want just the limited broadcast", dests)
	}
}

func TestDirectedBroadcastAddr(t *testing.T) {
	t.Parallel()
	_, ipNet24, err := net.ParseCIDR("192.168.1.1/24")
	if err != nil {
		t.Fatalf("ParseCIDR: %v", err)
	}
	ipNet24.IP = net.ParseIP("192.168.1.50").To4()

	// A 16-byte mask whose real bits live in the last 4 bytes, as Go's net
	// package can report on some platforms for an IPv4 interface address;
	// directedBroadcastAddr must trim it rather than misreading it as a
	// 16-byte-wide (all but three bits host) mask.
	sixteenByteMask := net.IPNet{
		IP:   net.ParseIP("10.1.2.3").To4(),
		Mask: net.IPMask{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 255, 255, 255, 0},
	}

	badMask := net.IPNet{IP: net.ParseIP("10.1.2.3").To4(), Mask: net.IPMask{0xFF, 0xFF, 0xFF}}

	ipv6 := &net.IPNet{IP: net.ParseIP("fe80::1"), Mask: net.CIDRMask(64, 128)}

	cases := []struct {
		name string
		addr net.Addr
		want *net.UDPAddr
	}{
		{"normal /24", ipNet24, &net.UDPAddr{IP: net.ParseIP("192.168.1.255").To4(), Port: ControllerPort}},
		{"16-byte mask trimmed", &sixteenByteMask, &net.UDPAddr{IP: net.ParseIP("10.1.2.255").To4(), Port: ControllerPort}},
		{"unusual mask length", &badMask, nil},
		{"not an IPNet", badAddr{}, nil},
		{"IPv6 address", ipv6, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := directedBroadcastAddr(tc.addr)
			if tc.want == nil {
				if got != nil {
					t.Fatalf("directedBroadcastAddr(%v) = %v, want nil", tc.addr, got)
				}
				return
			}
			if got == nil || !got.IP.Equal(tc.want.IP) || got.Port != tc.want.Port {
				t.Fatalf("directedBroadcastAddr(%v) = %v, want %v", tc.addr, got, tc.want)
			}
		})
	}
}
