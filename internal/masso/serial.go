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

import (
	"fmt"
	"strconv"
)

// SerialString formats a controller serial number the way it is written
// on the Masso's own screen and in Masso Link (for example, 12345 ->
// "G3-12345": decimal, no zero padding). The u16-to-"G3-nnnnn" mapping is
// a documented assumption, not yet confirmed against real hardware;
// internal/integration checks it against a live controller.
func SerialString(serial uint16) string {
	return "G3-" + strconv.Itoa(int(serial))
}

// ParseSerial parses a controller serial number written as "G3-12345",
// "g3-12345", or bare "12345". It returns ErrBadSerial for any other form,
// including a value that does not fit in uint16.
func ParseSerial(s string) (uint16, error) {
	digits := s
	if len(s) >= 3 && (s[:3] == "G3-" || s[:3] == "g3-") {
		digits = s[3:]
	}
	n, err := strconv.ParseUint(digits, 10, 16)
	if err != nil {
		return 0, fmt.Errorf("%w: %q", ErrBadSerial, s)
	}
	return uint16(n), nil
}
