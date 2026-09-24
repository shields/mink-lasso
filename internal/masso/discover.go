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
	"context"
	"fmt"
	"net"
	"slices"
	"time"
)

// Found is one controller's answer to a broadcast discovery request.
type Found struct {
	// Addr is the address the reply came from.
	Addr *net.UDPAddr
	// Identity is the controller's decoded identity reply.
	Identity Identity
}

// Discover broadcasts a discovery request — to the limited broadcast address
// 255.255.255.255:65535 and to the directed broadcast of every up,
// non-loopback IPv4 interface address, or only to Options.DiscoveryTargets
// if any were given — and collects Identity replies,
// deduplicated by source address, until timeout elapses. A nil error with a
// possibly empty slice means the request was sent but timeout ran out; the
// only error this returns for the send itself is ErrDiscoverySend, when no
// destination accepted the packet at all. If ctx is canceled while
// collecting replies, Discover returns what it has found so far along with
// ctx.Err().
func (c *Client) Discover(ctx context.Context, timeout time.Duration) ([]Found, error) {
	ch, cancel := c.expect(TypeDiscovery, nil, anyReply)
	defer cancel()

	if err := c.sendDiscoveryBroadcast(); err != nil {
		return nil, err
	}

	timer := c.clock.NewTimer(timeout)
	defer timer.Stop()

	var found []Found
	seen := make(map[string]bool)
	for {
		select {
		case <-ctx.Done():
			return found, ctx.Err()
		case <-timer.C():
			return found, nil
		case in := <-ch:
			// expect(TypeDiscovery, ...) only ever delivers Identity
			// values here; the assertion cannot fail.
			id := mustType[Identity](in.reply)
			addr, ok := in.addr.(*net.UDPAddr)
			if !ok {
				// Only reachable with an injected Options.ListenPacket
				// whose ReadFrom reports a non-UDP source address; a real
				// socket always reports *net.UDPAddr.
				continue
			}
			key := addr.String()
			if seen[key] {
				continue
			}
			seen[key] = true
			found = append(found, Found{Addr: addr, Identity: id})
		}
	}
}

// DiscoverAt sends a unicast discovery request to addr — which retargets
// that controller's replies to this client — and waits up to timeout for
// its identity reply.
func (c *Client) DiscoverAt(ctx context.Context, addr *net.UDPAddr, timeout time.Duration) (Identity, error) {
	reply, err := c.request(ctx, TypeDiscovery, nil, anyReply, Discovery(c.LocalPort()), addr, timeout, 1)
	if err != nil {
		return Identity{}, err
	}
	// request only ever delivers a reply that passed anyReply while
	// registered for TypeDiscovery, which the reader only ever routes
	// Identity values to; the assertion cannot fail.
	return mustType[Identity](reply), nil
}

// sendDiscoveryBroadcast sends a discovery request to every destination
// discoveryDestinations computes — which is never empty, so there is always
// at least one attempt to report on. It returns ErrDiscoverySend only if not one of them accepted
// the packet; a partial failure (some destinations sent, others errored or
// were never enumerated) is not an error.
func (c *Client) sendDiscoveryBroadcast() error {
	dests, enumErr := c.discoveryDestinations()
	if enumErr != nil {
		c.logger.Debug("masso: discovery: interface enumeration failed", "error", enumErr)
	}

	pkt := Discovery(c.LocalPort())
	var sent int
	var sendErr error
	for _, dest := range dests {
		if _, err := c.conn.WriteTo(pkt, dest); err != nil {
			sendErr = err
			c.logger.Debug("masso: discovery send failed", "dest", dest, "error", err)
			continue
		}
		sent++
	}
	if sent > 0 {
		return nil
	}
	return fmt.Errorf("%w: %w", ErrDiscoverySend, sendErr)
}

// discoveryDestinations returns Options.DiscoveryTargets if any were given.
// Otherwise it returns the limited broadcast address and the directed
// broadcast of every up, non-loopback interface address Options.Interfaces
// and Options.InterfaceAddrs report, deduplicated. A non-nil error means
// Options.Interfaces itself failed; discoveryDestinations still returns the
// limited broadcast in that case, since the caller only treats the overall
// send as failed if literally nothing got through.
func (c *Client) discoveryDestinations() ([]*net.UDPAddr, error) {
	if len(c.discoveryTargets) > 0 {
		return slices.Clone(c.discoveryTargets), nil
	}

	limited := &net.UDPAddr{IP: net.IPv4bcast, Port: ControllerPort}
	dests := []*net.UDPAddr{limited}
	seen := map[string]bool{limited.String(): true}

	addDest := func(d *net.UDPAddr) {
		if key := d.String(); !seen[key] {
			seen[key] = true
			dests = append(dests, d)
		}
	}

	ifaces, err := c.interfacesFn()
	if err != nil {
		return dests, fmt.Errorf("%w: %w", ErrInterfaceEnum, err)
	}

	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := c.interfaceAddrsFn(&iface)
		if err != nil {
			c.logger.Debug("masso: interface addrs failed", "interface", iface.Name, "error", err)
			continue
		}
		for _, a := range addrs {
			if dest := directedBroadcastAddr(a); dest != nil {
				addDest(dest)
			}
		}
	}
	return dests, nil
}

// directedBroadcastAddr returns the directed-broadcast UDP address for an
// IPv4 interface address (for example 192.168.1.50/24 ->
// 192.168.1.255:65535), or nil if a is not an IPv4 *net.IPNet.
func directedBroadcastAddr(a net.Addr) *net.UDPAddr {
	ipNet, ok := a.(*net.IPNet)
	if !ok {
		return nil
	}
	ip4 := ipNet.IP.To4()
	if ip4 == nil {
		return nil
	}
	mask := ipNet.Mask
	if len(mask) == net.IPv6len {
		mask = mask[net.IPv6len-net.IPv4len:]
	}
	if len(mask) != net.IPv4len {
		return nil
	}

	bcast := make(net.IP, net.IPv4len)
	for i := range ip4 {
		bcast[i] = ip4[i] | ^mask[i]
	}
	return &net.UDPAddr{IP: bcast, Port: ControllerPort}
}
