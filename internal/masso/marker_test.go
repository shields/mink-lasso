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

import "testing"

// TestMarkerMethods exercises the unexported isRequest/isReply methods
// that seal the Request and Reply interfaces. Nothing else calls them —
// their only job is to make the implementation set closed to this
// package — so this test exists purely to reach 100% coverage.
func TestMarkerMethods(t *testing.T) {
	t.Parallel()

	DiscoveryRequest{}.isRequest()
	ConfigRequest{}.isRequest()
	KeepaliveRequest{}.isRequest()
	ToolQueryRequest{}.isRequest()
	UploadStartRequest{}.isRequest()
	UploadChunkRequest{}.isRequest()

	Identity{}.isReply()
	ConfigReply{}.isReply()
	Status{}.isReply()
	ToolRecord{}.isReply()
	StartAck{}.isReply()
	ChunkAck{}.isReply()
}
