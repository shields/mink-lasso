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
	"time"
)

// configTime is the wall-clock example from docs/protocol.md §3.2: 14:07:30
// on the 24th of August (year 26).
var configTime = time.Date(2026, time.August, 24, 14, 7, 30, 0, time.UTC)

// The expected byte sequences below (CRC-16/XMODEM computed independently
// in Python from the field layout in docs/protocol.md, not by calling this
// package) are golden-byte tests for every request encoder.

func TestDiscoveryGolden(t *testing.T) {
	t.Parallel()

	want := []byte{
		0xAB, 0x21, // CRC
		0x03, 0x00, 0x02, // magic, type
		0xF8, 0x2A, // reply port 11000 LE
		0x00, 0x00, 0x00,
	}
	if got := Discovery(11000); !bytes.Equal(got, want) {
		t.Errorf("Discovery(11000) = % X, want % X", got, want)
	}
}

func TestConfigGolden(t *testing.T) {
	t.Parallel()

	want := []byte{
		0x2B, 0x63, // CRC
		0x03, 0x00, 0x03, // magic, type
		14, 7, 30, 24, 8, 26, 0, 0, 0,
	}
	if got := Config(configTime); !bytes.Equal(got, want) {
		t.Errorf("Config(...) = % X, want % X", got, want)
	}
}

func TestKeepaliveGolden(t *testing.T) {
	t.Parallel()

	want := []byte{
		0xE0, 0x40, // CRC
		0x03, 0x00, 0x01, // magic, type
		14, 7, 30, 24, 8,
	}
	if got := Keepalive(configTime); !bytes.Equal(got, want) {
		t.Errorf("Keepalive(...) = % X, want % X", got, want)
	}
}

func TestToolQueryGolden(t *testing.T) {
	t.Parallel()

	want := []byte{
		0x92, 0xB1, // CRC
		0x03, 0x00, 0x08, // magic, type
		0x01, 0x22, 0x2C, 0x1C, 0x0B,
	}
	if got := ToolQuery(1); !bytes.Equal(got, want) {
		t.Errorf("ToolQuery(1) = % X, want % X", got, want)
	}
}

// capturedUploadStart is the upload-start packet for an 87-byte CLTEST.NC
// exactly as captured from Masso Link v2.12 against firmware v5.13 (30 bytes:
// the name field is a fixed 16 bytes, see uploadNameField).
var capturedUploadStart = []byte{
	0x06, 0x39, // CRC
	0x03, 0x00, 0x0A, // magic, type
	0x57, 0x00, 0x00, 0x00, // size = 87
	0x00, 0x00, // reserved
	0x01, 0x5C, 0x00, // pathlen=1, "\", NUL
	'C', 'L', 'T', 'E', 'S', 'T', '.', 'N', 'C', 0x00,
	0x00, 0x00, 0x00, 0x00, 0x00, 0x00, // rest of the 16-byte name field
}

// TestUploadStartGolden checks UploadStart against the captured packet byte
// for byte, including the CRC.
func TestUploadStartGolden(t *testing.T) {
	t.Parallel()

	got, err := UploadStart(87, "CLTEST.NC")
	if err != nil {
		t.Fatalf("UploadStart: unexpected error: %v", err)
	}
	if !bytes.Equal(got, capturedUploadStart) {
		t.Errorf("UploadStart(87, \"CLTEST.NC\") = % X, want % X", got, capturedUploadStart)
	}
	// A maximal name fills the field exactly; the packet length never changes.
	longest, err := UploadStart(1, "ABCDEFGHIJK.TAP")
	if err != nil {
		t.Fatalf("UploadStart: unexpected error: %v", err)
	}
	if len(longest) != len(capturedUploadStart) {
		t.Errorf("UploadStart with a 15-character name is %d bytes, want %d", len(longest), len(capturedUploadStart))
	}
}

func TestUploadStartRejectsBadFileName(t *testing.T) {
	t.Parallel()

	if _, err := UploadStart(1, "bad/name"); !errors.Is(err, ErrBadFileName) {
		t.Errorf("UploadStart with bad name: err = %v, want ErrBadFileName", err)
	}
}

func TestUploadChunkGolden(t *testing.T) {
	t.Parallel()

	want := []byte{
		0x2F, 0x70, // CRC
		0x03, 0x00, 0x0B, // magic, type
		0x00, 0x00, 0x00, 0x00, // index = 0
		0x02, 0x00, 0x00, 0x00, // length = 2
		'A', 'B',
		0x00, 0x00, 0x00, // 4-byte alignment padding
	}
	got, err := UploadChunk(0, []byte("AB"))
	if err != nil {
		t.Fatalf("UploadChunk: unexpected error: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("UploadChunk(0, \"AB\") = % X, want % X", got, want)
	}
}

func TestUploadChunkRejectsOversizedData(t *testing.T) {
	t.Parallel()

	data := bytes.Repeat([]byte{0x01}, MaxChunkData+1)
	if _, err := UploadChunk(0, data); !errors.Is(err, ErrChunkTooLarge) {
		t.Errorf("UploadChunk with %d bytes: err = %v, want ErrChunkTooLarge", len(data), err)
	}
}

func TestUploadChunkAcceptsMaxSize(t *testing.T) {
	t.Parallel()

	data := bytes.Repeat([]byte{0x01}, MaxChunkData)
	if _, err := UploadChunk(0, data); err != nil {
		t.Errorf("UploadChunk with %d bytes: unexpected error: %v", len(data), err)
	}
}

func TestDecodeRequestRoundTrip(t *testing.T) {
	t.Parallel()

	uploadStart, err := UploadStart(87, "CLTEST.NC")
	if err != nil {
		t.Fatalf("UploadStart: %v", err)
	}
	uploadChunk, err := UploadChunk(3, []byte("hello"))
	if err != nil {
		t.Fatalf("UploadChunk: %v", err)
	}

	tests := []struct {
		name string
		pkt  []byte
		want Request
	}{
		{"discovery", Discovery(11000), DiscoveryRequest{ReplyPort: 11000}},
		{
			"config", Config(configTime),
			ConfigRequest{Hour: 14, Minute: 7, Second: 30, Day: 24, Month: 8, Year: 26},
		},
		{
			"keepalive", Keepalive(configTime),
			KeepaliveRequest{Hour: 14, Minute: 7, Second: 30, Day: 24, Month: 8},
		},
		{"tool query", ToolQuery(5), ToolQueryRequest{Index: 5}},
		{"upload start", uploadStart, UploadStartRequest{Size: 87, Name: "CLTEST.NC"}},
		{"upload chunk", uploadChunk, UploadChunkRequest{Index: 3, Data: []byte("hello")}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := DecodeRequest(tt.pkt)
			if err != nil {
				t.Fatalf("DecodeRequest: unexpected error: %v", err)
			}
			gotChunk, gotIsChunk := got.(UploadChunkRequest)
			wantChunk, wantIsChunk := tt.want.(UploadChunkRequest)
			if gotIsChunk != wantIsChunk {
				t.Fatalf("DecodeRequest = %#v, want %#v", got, tt.want)
			}
			if gotIsChunk {
				if gotChunk.Index != wantChunk.Index || !bytes.Equal(gotChunk.Data, wantChunk.Data) {
					t.Errorf("DecodeRequest = %#v, want %#v", got, tt.want)
				}
				return
			}
			if got != tt.want {
				t.Errorf("DecodeRequest = %#v, want %#v", got, tt.want)
			}
		})
	}
}

// TestDecodeUploadStartCaptured decodes the packet captured from Masso Link.
func TestDecodeUploadStartCaptured(t *testing.T) {
	t.Parallel()

	got, err := DecodeRequest(capturedUploadStart)
	if err != nil {
		t.Fatalf("DecodeRequest: unexpected error: %v", err)
	}
	want := UploadStartRequest{Size: 87, Name: "CLTEST.NC"}
	if got != want {
		t.Errorf("DecodeRequest = %#v, want %#v", got, want)
	}
}

func TestDecodeRequestRejectsUnknownType(t *testing.T) {
	t.Parallel()

	pkt := frame(0x99, nil)
	if _, err := DecodeRequest(pkt); !errors.Is(err, ErrUnknownType) {
		t.Errorf("DecodeRequest: err = %v, want ErrUnknownType", err)
	}
}

func TestDecodeRequestRejectsShortBodies(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		typ  byte
	}{
		{"discovery", TypeDiscovery},
		{"config", TypeConfig},
		{"keepalive", TypeStatus},
		{"tool query", TypeTool},
		{"upload start", TypeUploadStart},
		{"upload chunk", TypeUploadChunk},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			// A truly empty body (no bytes past the type byte), built
			// directly rather than via frame(), which always leaves at
			// least one padding byte.
			body := []byte{magic[0], magic[1], tt.typ}
			crc := crc16XModem(body)
			pkt := append([]byte{byte(crc), byte(crc >> 8)}, body...)

			if _, err := DecodeRequest(pkt); !errors.Is(err, ErrBadLength) {
				t.Errorf("DecodeRequest(empty %s body): err = %v, want ErrBadLength", tt.name, err)
			}
		})
	}
}

func TestDecodeUploadStartRejectsTruncatedPath(t *testing.T) {
	t.Parallel()

	// size(4) + reserved(2) + pathlen=200, but no path bytes follow.
	payload := []byte{0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 200}
	pkt := frame(TypeUploadStart, payload)

	if _, err := DecodeRequest(pkt); !errors.Is(err, ErrMalformed) {
		t.Errorf("DecodeRequest: err = %v, want ErrMalformed", err)
	}
}

func TestDecodeUploadChunkRejectsOverrunLength(t *testing.T) {
	t.Parallel()

	// index(4) + declared length(4)=100, but no data follows.
	payload := []byte{0, 0, 0, 0, 100, 0, 0, 0}
	pkt := frame(TypeUploadChunk, payload)

	if _, err := DecodeRequest(pkt); !errors.Is(err, ErrMalformed) {
		t.Errorf("DecodeRequest: err = %v, want ErrMalformed", err)
	}
}

func TestDecodeUploadStartRejectsBadFileName(t *testing.T) {
	t.Parallel()

	// size(4) + reserved(2) + pathlen=1 + "\" + NUL + an empty name (just
	// its own NUL): framing is well-formed, but ValidateFileName rejects
	// an empty name, the same as UploadStart's encoder would.
	payload := []byte{0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 1, '\\', 0x00, 0x00}
	pkt := frame(TypeUploadStart, payload)

	if _, err := DecodeRequest(pkt); !errors.Is(err, ErrBadFileName) {
		t.Errorf("DecodeRequest: err = %v, want ErrBadFileName", err)
	}
}

func TestDecodeUploadChunkRejectsOversizedLength(t *testing.T) {
	t.Parallel()

	// index(4) + declared length(4) = MaxChunkData+1 = 1423 (0x058F
	// little-endian). No data needs to actually follow: the size check
	// must fire before the overrun-length check gets a chance to.
	payload := []byte{0, 0, 0, 0, 0x8F, 0x05, 0, 0}
	pkt := frame(TypeUploadChunk, payload)

	if _, err := DecodeRequest(pkt); !errors.Is(err, ErrChunkTooLarge) {
		t.Errorf("DecodeRequest: err = %v, want ErrChunkTooLarge", err)
	}
}

func TestDecodeRequestPropagatesFramingErrors(t *testing.T) {
	t.Parallel()

	if _, err := DecodeRequest([]byte{1, 2}); !errors.Is(err, ErrShortPacket) {
		t.Errorf("DecodeRequest: err = %v, want ErrShortPacket", err)
	}
}
