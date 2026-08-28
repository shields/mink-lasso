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
	"net"
	"os"
	"testing"

	"msrl.dev/mink-lasso/internal/masso"
)

// testDiscover checks the identity from a unicast connect to addr — the
// address the suite's one broadcast discovery already found for
// MINK_LASSO_SERIAL — and empirically checks the G3-nnnnn <-> uint16 serial
// mapping documented on masso.SerialString against a live controller.
func testDiscover(t *testing.T, addr *net.UDPAddr, serial uint16) {
	t.Helper()

	conn := connect(t, addr)

	t.Logf(
		"found controller %s (serial %d) at %s, firmware %q",
		masso.SerialString(conn.identity.Serial), conn.identity.Serial, conn.addr, conn.identity.Version,
	)

	if conn.identity.Serial != conn.cfg.Serial {
		t.Errorf("discovery identity serial %d != config-reply serial %d from Connect", conn.identity.Serial, conn.cfg.Serial)
	}

	if conn.cfg.Serial != serial {
		t.Errorf(
			"serial mismatch: controller reports %s (%d), MINK_LASSO_SERIAL=%q parsed as %s (%d)",
			masso.SerialString(conn.cfg.Serial), conn.cfg.Serial,
			os.Getenv("MINK_LASSO_SERIAL"), masso.SerialString(serial), serial,
		)
	}
}
