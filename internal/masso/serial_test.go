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
	"errors"
	"testing"
)

func TestSerialString(t *testing.T) {
	t.Parallel()

	tests := []struct {
		serial uint32
		want   string
	}{
		{12345, "G3-12345"},
		{0, "G3-0"},
		{65535, "G3-65535"},
		{65536, "G3-65536"},
		{4294967295, "G3-4294967295"},
		{1, "G3-1"},
	}
	for _, tt := range tests {
		if got := SerialString(tt.serial); got != tt.want {
			t.Errorf("SerialString(%d) = %q, want %q", tt.serial, got, tt.want)
		}
	}
}

func TestParseSerialValid(t *testing.T) {
	t.Parallel()

	tests := []struct {
		s    string
		want uint32
	}{
		{"G3-12345", 12345},
		{"g3-12345", 12345},
		{"12345", 12345},
		{"G3-0", 0},
		{"G3-65535", 65535},
		{"G3-70000", 70000},
		{"4294967295", 4294967295},
	}
	for _, tt := range tests {
		got, err := ParseSerial(tt.s)
		if err != nil {
			t.Errorf("ParseSerial(%q): unexpected error: %v", tt.s, err)
			continue
		}
		if got != tt.want {
			t.Errorf("ParseSerial(%q) = %d, want %d", tt.s, got, tt.want)
		}
	}
}

func TestParseSerialInvalid(t *testing.T) {
	t.Parallel()

	tests := []string{
		"",
		"G3-",
		"g3-",
		"G3-abc",
		"abc",
		"G3-4294967296", // out of uint32 range
		"4294967296",    // out of uint32 range
		"G3--12345",     // malformed
		"G3",            // too short for the prefix check
	}
	for _, s := range tests {
		if _, err := ParseSerial(s); !errors.Is(err, ErrBadSerial) {
			t.Errorf("ParseSerial(%q): err = %v, want ErrBadSerial", s, err)
		}
	}
}

func TestSerialRoundTrip(t *testing.T) {
	t.Parallel()

	for _, serial := range []uint32{0, 1, 12345, 65535, 65536, 4294967295} {
		got, err := ParseSerial(SerialString(serial))
		if err != nil {
			t.Fatalf("ParseSerial(SerialString(%d)): unexpected error: %v", serial, err)
		}
		if got != serial {
			t.Errorf("ParseSerial(SerialString(%d)) = %d, want %d", serial, got, serial)
		}
	}
}
