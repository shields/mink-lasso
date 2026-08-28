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

//go:build !windows

package engine

import (
	"bytes"
	"testing"
)

// changeDuringSending is the POSIX half of the changed-while-sending
// scenario: nothing stops a post-processor from replacing a file that is
// being read, so the content is swapped for 'b's mid-transfer, and the
// resend must deliver those.
func changeDuringSending(t *testing.T, path string, size int) []byte {
	t.Helper()
	data := bytes.Repeat([]byte{'b'}, size)
	overwriteFileAtomic(t, path, data)

	return data
}
