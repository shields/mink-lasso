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

//go:build windows

package engine

import (
	"bytes"
	"errors"
	"os"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// changeDuringSending is the Windows half of the changed-while-sending
// scenario. Here the transfer's deny-write handle (winutil.OpenDenyWrite)
// stops anyone from replacing the content mid-transfer, which this first
// proves — the overwrite fails with a sharing violation — and then it
// changes what it still can, the modification time (an attribute-only open
// is not subject to sharing checks), so that the watcher reports the file
// as changed and the scheduler's resend-after-transfer path still runs. The
// resend therefore delivers the original 'a's, which are returned.
func changeDuringSending(t *testing.T, path string, size int) []byte {
	t.Helper()
	err := os.WriteFile(path, bytes.Repeat([]byte{'b'}, size), 0o644)
	if !errors.Is(err, windows.ERROR_SHARING_VIOLATION) {
		t.Fatalf("overwriting a file mid-transfer: err = %v, want ERROR_SHARING_VIOLATION", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	later := info.ModTime().Add(time.Hour)
	if err := os.Chtimes(path, later, later); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}

	return bytes.Repeat([]byte{'a'}, size)
}
