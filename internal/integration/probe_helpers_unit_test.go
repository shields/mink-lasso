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
	"bytes"
	"testing"

	"msrl.dev/mink-lasso/internal/masso"
	"msrl.dev/mink-lasso/internal/masso/sim"
)

func TestStartAccepted(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		raw     []byte
		retries int
		want    bool
	}{
		{"OK first attempt", masso.StartAck{Result: masso.StartOK}.Encode(), 0, true},
		{"OK after retry", masso.StartAck{Result: masso.StartOK}.Encode(), 1, true},
		{"already started first attempt", masso.StartAck{Result: masso.StartAlreadyStarted}.Encode(), 0, false},
		{"already started after retry", masso.StartAck{Result: masso.StartAlreadyStarted}.Encode(), 1, true},
		{"already started after two retries", masso.StartAck{Result: masso.StartAlreadyStarted}.Encode(), 2, true},
		{"no USB after retry", masso.StartAck{Result: masso.StartNoUSB}.Encode(), 1, false},
		{"wrong reply type", masso.ChunkAck{}.Encode(), 1, false},
		{"undecodable", nil, 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := startAccepted(tc.raw, tc.retries); got != tc.want {
				t.Errorf("startAccepted = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestProbeHarnessStartRetryCleanup(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		optional bool
		dropped  int
		refused  bool
	}{
		{"required retry", false, 1, false},
		{"optional retries", true, 2, false},
		{"required first attempt refusal", false, 0, true},
		{"optional first attempt refusal", true, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			controller, err := sim.New(sim.Options{Serial: 1})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = controller.Close() })
			controller.SetDropStartAcks(tc.dropped)
			if tc.refused {
				controller.SetStartResult(masso.StartAlreadyStarted)
			}
			h := newProbeHarness(t, controller.Addr())
			data := []byte("G0 X1")
			const name = "MLTEST.NC"
			pkt, err := masso.UploadStart(uint32(len(data)), "", name)
			if err != nil {
				t.Fatal(err)
			}
			// Registered before the transfer's own cleanup so this checks
			// that cleanup actually sends the missing data after a retry.
			t.Cleanup(func() {
				got, exists := controller.File(name)
				if tc.refused {
					if exists || controller.ChunkRequests() != 0 {
						t.Error("first-attempt refusal sent chunks or created a file")
					}
				} else if !exists || !bytes.Equal(got, data) {
					t.Errorf("cleanup stored (%q, %v), want (%q, true)", got, exists, data)
				}
			})

			var tr *probeTransfer
			if tc.optional {
				var ok bool
				tr, _, ok = h.startTransferOptional(t, pkt, data)
				if !ok {
					t.Fatal("optional start received no reply")
				}
			} else {
				tr, _ = h.startTransfer(t, pkt, data)
			}
			if tr.open == tc.refused {
				t.Errorf("transfer open = %v, want %v", tr.open, !tc.refused)
			}
			if got := controller.StartRequests(); got != tc.dropped+1 {
				t.Errorf("start requests = %d, want %d", got, tc.dropped+1)
			}
		})
	}
}
