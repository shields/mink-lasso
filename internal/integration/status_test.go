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
	"errors"
	"net"
	"testing"
	"time"
)

// testStatus connects, runs the keepalive/status loop, and expects at least
// three status replies within 5s, sanity-checking their fields.
func testStatus(t *testing.T, addr *net.UDPAddr) {
	t.Helper()

	conn := connect(t, addr)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	runErr := make(chan error, 1)
	go func() { runErr <- conn.client.Run(ctx) }()

	const wantUpdates = 3

	deadline := time.After(5 * time.Second)

	var got int
	for got < wantUpdates {
		select {
		case st := <-conn.client.Status():
			got++

			t.Logf(
				"status %d: progress=%d running=%v jobs=%d prompt=0x%02x waitingForOperator=%v line=%d file=%q",
				got, st.Progress, st.Running, st.Jobs, st.Prompt, st.WaitingForOperator, st.Line, st.File,
			)

			if st.Progress > 100 {
				t.Errorf("status %d: progress %d out of range 0-100", got, st.Progress)
			}

			const implausible = 100_000_000
			if st.Line > implausible {
				t.Errorf("status %d: line number %d implausibly large", got, st.Line)
			}

			if st.Jobs > implausible {
				t.Errorf("status %d: job count %d implausibly large", got, st.Jobs)
			}

			if !isPrintableASCII(st.File) {
				t.Errorf("status %d: file name %q is not printable ASCII", got, st.File)
			}
		case <-deadline:
			t.Fatalf("received only %d status update(s) within 5s, want at least %d", got, wantUpdates)
		}
	}

	cancel()

	if err := <-runErr; err != nil && !errors.Is(err, context.Canceled) {
		t.Errorf("Client.Run: %v", err)
	}
}
