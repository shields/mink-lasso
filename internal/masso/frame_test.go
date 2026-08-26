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
	"bytes"
	"errors"
	"testing"
)

func TestCRC16XModem(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		data []byte
		want uint16
	}{
		{"reference vector", []byte("123456789"), 0x31C3},
		{"empty", nil, 0x0000},
		{"single zero byte", []byte{0x00}, 0x0000},
		{"single 0xFF byte", []byte{0xFF}, 0x1EF0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := crc16XModem(tt.data); got != tt.want {
				t.Errorf("crc16XModem(%v) = %#04x, want %#04x", tt.data, got, tt.want)
			}
		})
	}
}

func TestRoundUp4(t *testing.T) {
	t.Parallel()

	tests := []struct {
		n, want int
	}{
		{0, 0}, {1, 4}, {2, 4}, {3, 4}, {4, 4},
		{5, 8}, {6, 8}, {7, 8}, {8, 8}, {9, 12},
	}
	for _, tt := range tests {
		if got := roundUp4(tt.n); got != tt.want {
			t.Errorf("roundUp4(%d) = %d, want %d", tt.n, got, tt.want)
		}
	}
}

// TestFramePadding covers payload lengths 0..8, the boundary around the
// 4-byte alignment rule.
func TestFramePadding(t *testing.T) {
	t.Parallel()

	for n := range 9 {
		payload := bytes.Repeat([]byte{0xAA}, n)
		pkt := frame(0x7F, payload)

		wantBodyLen := roundUp4(3 + n)
		if len(pkt) != 2+wantBodyLen {
			t.Fatalf("frame with %d-byte payload: len(pkt) = %d, want %d", n, len(pkt), 2+wantBodyLen)
		}

		typ, body, err := parse(pkt)
		if err != nil {
			t.Fatalf("frame with %d-byte payload: parse failed: %v", n, err)
		}
		if typ != 0x7F {
			t.Errorf("frame with %d-byte payload: typ = %#02x, want 0x7f", n, typ)
		}
		if len(body) != wantBodyLen-3 {
			t.Fatalf("frame with %d-byte payload: len(body) = %d, want %d", n, len(body), wantBodyLen-3)
		}
		if !bytes.Equal(body[:n], payload) {
			t.Errorf("frame with %d-byte payload: body[:%d] = %v, want %v", n, n, body[:n], payload)
		}
		for i := n; i < len(body); i++ {
			if body[i] != 0 {
				t.Errorf("frame with %d-byte payload: padding byte %d = %#02x, want 0", n, i, body[i])
			}
		}
	}
}

func TestFrameLayout(t *testing.T) {
	t.Parallel()

	pkt := frame(TypeStatus, nil)
	if pkt[2] != 0x03 || pkt[3] != 0x00 {
		t.Errorf("magic = %#02x %#02x, want 03 00", pkt[2], pkt[3])
	}
	if pkt[4] != TypeStatus {
		t.Errorf("type = %#02x, want %#02x", pkt[4], TypeStatus)
	}
}

func TestParseRejectsShortPacket(t *testing.T) {
	t.Parallel()

	for n := range 5 {
		pkt := make([]byte, n)
		if _, _, err := parse(pkt); !errors.Is(err, ErrShortPacket) {
			t.Errorf("parse(%d-byte packet): err = %v, want ErrShortPacket", n, err)
		}
	}
}

func TestParseRejectsBadMagic(t *testing.T) {
	t.Parallel()

	body := []byte{0x99, 0x00, 0x01, 0x00}
	pkt := make([]byte, 2+len(body))
	// Deliberately do not use frame(); CRC is correct but magic is wrong.
	crc := crc16XModem(body)
	pkt[0] = byte(crc)
	pkt[1] = byte(crc >> 8)
	copy(pkt[2:], body)

	if _, _, err := parse(pkt); !errors.Is(err, ErrBadMagic) {
		t.Errorf("parse: err = %v, want ErrBadMagic", err)
	}
}

func TestParseRejectsBadCRC(t *testing.T) {
	t.Parallel()

	pkt := frame(TypeStatus, []byte{1, 2, 3})
	pkt[0] ^= 0xFF // corrupt the CRC

	if _, _, err := parse(pkt); !errors.Is(err, ErrBadCRC) {
		t.Errorf("parse: err = %v, want ErrBadCRC", err)
	}
}

func TestParseAcceptsValidFrame(t *testing.T) {
	t.Parallel()

	payload := []byte{1, 2, 3, 4, 5}
	pkt := frame(TypeTool, payload)

	typ, body, err := parse(pkt)
	if err != nil {
		t.Fatalf("parse: unexpected error: %v", err)
	}
	if typ != TypeTool {
		t.Errorf("typ = %#02x, want %#02x", typ, TypeTool)
	}
	if !bytes.Equal(body[:len(payload)], payload) {
		t.Errorf("body[:5] = %v, want %v", body[:len(payload)], payload)
	}
}
