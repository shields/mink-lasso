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

import "net"

// AddDiscoveryTargetForTest exposes the unexported addDiscoveryTarget hook
// to the external masso_test test package, which cannot reach unexported
// identifiers directly: it needs the hook (Discover's real broadcast
// mechanism is not reliably deliverable to a loopback-bound simulator, see
// addDiscoveryTarget's doc comment) but must live outside this package
// because internal/masso/sim imports internal/masso, and a same-package
// test file cannot also import sim without an import cycle.
func (c *Client) AddDiscoveryTargetForTest(addr *net.UDPAddr) {
	c.addDiscoveryTarget(addr)
}
