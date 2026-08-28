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
	"context"
	"fmt"
	"net"
	"testing"
	"time"
)

// mltest1Data is a short comment file that fits in a single upload chunk.
func mltest1Data() []byte {
	return []byte("(mink-lasso integration test 1)\nM30\n")
}

// mltest2Data is roughly 10 KiB of comment lines, so the upload spans
// several chunks and the last one is short.
func mltest2Data() []byte {
	var b bytes.Buffer

	b.WriteString("(mink-lasso integration test 2)\n")

	const want = 10 * 1024
	for b.Len() < want {
		fmt.Fprintf(&b, "(padding line to make this file span multiple upload chunks, currently %d bytes)\n", b.Len())
	}

	b.WriteString("M30\n")

	return b.Bytes()
}

// testUpload uploads MLTEST1.NC, then MLTEST2.NC, then MLTEST1.NC again to
// confirm overwriting, skipping unless the machine is idle or
// MINK_LASSO_ALLOW_RUNNING=1.
func testUpload(t *testing.T, addr *net.UDPAddr) {
	t.Helper()

	requireIdleOrAllowed(t, addr)

	conn := connect(t, addr)

	upload := func(name string, data []byte) {
		t.Helper()

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		var sent, total int64

		err := conn.client.Upload(ctx, name, bytes.NewReader(data), int64(len(data)), func(s, tot int64) {
			sent, total = s, tot
		})
		if err != nil {
			t.Fatalf("Upload(%s): %v (progress before failure: %d/%d bytes)", name, err, sent, total)
		}

		if total != int64(len(data)) {
			t.Errorf("Upload(%s): final progress total=%d, want %d", name, total, len(data))
		}

		if sent != total {
			t.Errorf("Upload(%s): final progress sent=%d, want %d (== total)", name, sent, total)
		}
	}

	upload("MLTEST1.NC", mltest1Data())
	upload("MLTEST2.NC", mltest2Data())
	upload("MLTEST1.NC", mltest1Data()) // overwrite
}
