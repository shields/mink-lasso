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

package masso_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"msrl.dev/mink-lasso/internal/clock"
	"msrl.dev/mink-lasso/internal/masso"
	"msrl.dev/mink-lasso/internal/masso/sim"
)

func TestDiscoverFindsSimulatorViaExtraTarget(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s := newSim(t, sim.Options{Serial: 999, Version: "5-Axis v5.13"})

	// A loopback interface has no IFF_BROADCAST on every OS (confirmed on
	// macOS), so nothing sent to 255.255.255.255 or a directed loopback
	// broadcast is ever delivered to a socket bound there; and a real
	// broadcast would also reach, and re-target, any controller on the LAN.
	// So Discover is aimed at the simulator alone. This test uses a real
	// (not fake) clock: it must let a genuine asynchronous reply be
	// collected within Discover's real collection window, and advancing a
	// fake clock for that window would race the reply's delivery.
	c := newDiscoveryTestClient(t, clock.Real{}, s.Addr())

	found, err := c.Discover(ctx, 500*time.Millisecond)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(found) != 1 {
		t.Fatalf("len(found) = %d, want 1", len(found))
	}
	if found[0].Identity.Serial != 999 {
		t.Errorf("Identity.Serial = %d, want 999", found[0].Identity.Serial)
	}
	if !found[0].Addr.IP.Equal(s.Addr().IP) || found[0].Addr.Port != s.Addr().Port {
		t.Errorf("Addr = %v, want %v", found[0].Addr, s.Addr())
	}
}

func TestDiscoverDeduplicatesBySourceAddress(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s := newSim(t, sim.Options{Serial: 111})
	s.SetDuplicateReplies(true)
	c := newDiscoveryTestClient(t, clock.Real{}, s.Addr())

	found, err := c.Discover(ctx, 500*time.Millisecond)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(found) != 1 {
		t.Fatalf("len(found) = %d, want 1 (duplicates must be deduplicated)", len(found))
	}
}

func TestDiscoverCtxCancelReturnsError(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	s := newSim(t, sim.Options{Serial: 1})
	c := newDiscoveryTestClient(t, clock.Real{}, s.Addr())

	resultCh := make(chan struct {
		found []masso.Found
		err   error
	}, 1)
	go func() {
		found, err := c.Discover(ctx, safetyNet)
		resultCh <- struct {
			found []masso.Found
			err   error
		}{found, err}
	}()
	cancel()

	res := waitFor(t, resultCh)
	if !errors.Is(res.err, context.Canceled) {
		t.Fatalf("Discover error = %v, want context.Canceled", res.err)
	}
}
