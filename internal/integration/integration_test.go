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

import "testing"

// TestIntegration runs the whole suite as sequential subtests: a controller
// talks to one client at a time, so nothing here may run with t.Parallel().
//
//nolint:paralleltest // sequential by design; see the comment above.
func TestIntegration(t *testing.T) {
	serial := requireSerial(t)
	addr := resolveController(t, serial)

	//nolint:paralleltest // sequential by design; see TestIntegration's doc comment.
	t.Run("Discover", func(t *testing.T) {
		testDiscover(t, addr, serial)
	})
	//nolint:paralleltest // sequential by design; see TestIntegration's doc comment.
	t.Run("Status", func(t *testing.T) {
		testStatus(t, addr)
	})
	//nolint:paralleltest // sequential by design; see TestIntegration's doc comment.
	t.Run("Tools", func(t *testing.T) {
		testTools(t, addr)
	})
	//nolint:paralleltest // sequential by design; see TestIntegration's doc comment.
	t.Run("Upload", func(t *testing.T) {
		testUpload(t, addr)
	})
	//nolint:paralleltest // sequential by design; see TestIntegration's doc comment.
	t.Run("UploadSubfolder", func(t *testing.T) {
		testUploadSubfolder(t, addr)
	})
	//nolint:paralleltest // sequential by design; see TestIntegration's doc comment.
	t.Run("E2E", func(t *testing.T) {
		testE2E(t, addr, serial)
	})
}
