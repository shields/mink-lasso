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
	"testing"
	"time"
)

// testTools queries the tool table and logs how many records came back.
// Client.Tools stops at the first empty tool name or the first index that
// never answers (logged at debug level inside the client itself), so the
// count alone is enough evidence the query ran to a sensible stop.
func testTools(t *testing.T, addr *net.UDPAddr) {
	t.Helper()

	conn := connect(t, addr)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	tools, err := conn.client.Tools(ctx)
	if err != nil {
		t.Logf("tools: %d record(s) returned before failure", len(tools))
		t.Fatalf("Tools: %v", err)
	}

	t.Logf("tools: %d record(s) returned", len(tools))

	for _, tool := range tools {
		t.Logf("  tool %d: %q", tool.Index, tool.Name)
	}
}
