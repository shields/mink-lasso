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
	"encoding/binary"
	"errors"
	"strings"
	"testing"
)

func TestIdentityEncodeLayout(t *testing.T) {
	t.Parallel()

	r := Identity{Serial: 12345, Version: "5-Axis v5.13"}
	pkt := r.Encode()

	if len(pkt) != identityLen {
		t.Fatalf("len(pkt) = %d, want %d", len(pkt), identityLen)
	}
	if pkt[2] != 0x03 || pkt[3] != 0x00 {
		t.Errorf("magic = %#02x %#02x, want 03 00", pkt[2], pkt[3])
	}
	if pkt[4] != TypeDiscovery {
		t.Errorf("type = %#02x, want %#02x", pkt[4], TypeDiscovery)
	}
	if serial := binary.LittleEndian.Uint32(pkt[5:9]); serial != 12345 {
		t.Errorf("serial = %d, want 12345", serial)
	}
	if pkt[12] != identityByte12 {
		t.Errorf("pkt[12] = %#02x, want %#02x", pkt[12], byte(identityByte12))
	}
	if !bytes.HasPrefix(pkt[13:], []byte("5-Axis v5.13\x00")) {
		t.Errorf("version field = %q, want %q", pkt[13:29], "5-Axis v5.13\x00")
	}
}

func TestIdentityRoundTrip(t *testing.T) {
	t.Parallel()

	want := Identity{Serial: 4000054321, Version: "5-Axis v5.13"}
	got, err := DecodeReply(want.Encode())
	if err != nil {
		t.Fatalf("DecodeReply: unexpected error: %v", err)
	}
	if got != want {
		t.Errorf("DecodeReply(Encode()) = %#v, want %#v", got, want)
	}
}

func TestIdentityEncodeTruncatesLongVersion(t *testing.T) {
	t.Parallel()

	r := Identity{Serial: 1, Version: "this version string is far too long to fit in the reply"}
	pkt := r.Encode()
	if len(pkt) != identityLen {
		t.Fatalf("len(pkt) = %d, want %d", len(pkt), identityLen)
	}
	got, err := DecodeReply(pkt)
	if err != nil {
		t.Fatalf("DecodeReply: unexpected error: %v", err)
	}
	want := r.Version[:versionEnd-versionStart]
	if id, ok := got.(Identity); !ok || id.Version != want {
		t.Errorf("DecodeReply = %#v, want Identity with Version %q", got, want)
	}
}

// With no NUL anywhere from byte 13 on, only the byte-41 bound can end the
// version string.
func TestDecodeIdentityStopsAtByte41(t *testing.T) {
	t.Parallel()

	pkt := make([]byte, identityLen)
	pkt[2], pkt[3] = magic[0], magic[1]
	pkt[4] = TypeDiscovery
	binary.LittleEndian.PutUint32(pkt[5:9], 7)
	for i := versionStart; i < identityLen; i++ {
		pkt[i] = 'V'
	}
	binary.LittleEndian.PutUint16(pkt[0:2], crc16XModem(pkt[2:]))

	got, err := DecodeReply(pkt)
	if err != nil {
		t.Fatalf("DecodeReply: unexpected error: %v", err)
	}
	want := Identity{Serial: 7, Version: strings.Repeat("V", 29)}
	if got != want {
		t.Errorf("DecodeReply = %#v, want %#v", got, want)
	}
}

func TestIdentityMaxTools(t *testing.T) {
	t.Parallel()

	tests := []struct {
		id   Identity
		want int
	}{
		{Identity{Serial: 0, Version: "5-Axis v5.13"}, 32},
		{Identity{Serial: 5000, Version: "Lathe v5.09"}, 32},
		{Identity{Serial: 5001, Version: "5-Axis v5.13"}, 118},
		{Identity{Serial: 31578, Version: "Lathe v5.09"}, 100},
		{Identity{Serial: 31578, Version: "2-Axis LATHE v5.09"}, 100},
		{Identity{Serial: 31578, Version: "lathe"}, 100},
		{Identity{Serial: 31578, Version: ""}, 118},
	}
	for _, tt := range tests {
		if got := tt.id.MaxTools(); got != tt.want {
			t.Errorf("%#v.MaxTools() = %d, want %d", tt.id, got, tt.want)
		}
	}
}

func TestConfigReplyRoundTrip(t *testing.T) {
	t.Parallel()

	want := ConfigReply{Serial: 12345}
	pkt := want.Encode()
	if len(pkt) != configReplyLen {
		t.Fatalf("len(pkt) = %d, want %d", len(pkt), configReplyLen)
	}
	if pkt[9] != 0x0A {
		t.Errorf("pkt[9] = %#02x, want 0x0A", pkt[9])
	}
	got, err := DecodeReply(pkt)
	if err != nil {
		t.Fatalf("DecodeReply: unexpected error: %v", err)
	}
	if got != want {
		t.Errorf("DecodeReply(Encode()) = %#v, want %#v", got, want)
	}
}

func TestStatusRoundTrip(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		want Status
	}{
		{
			"running", Status{
				Progress: 42, State: runStateRunning, Running: true,
				Jobs: 7, Prompt: 0x01, WaitingForOperator: false,
				Line: 1234, File: "PART1.NC",
			},
		},
		{
			"waiting for operator", Status{
				Progress: 0, State: 0x00, Running: false,
				Jobs: 3, Prompt: 0x00, WaitingForOperator: true,
				Line: 0, File: "",
			},
		},
		{
			"undocumented state byte", Status{
				Progress: 5, State: 0x07, Running: false,
				Jobs: 1, Prompt: 0x02, WaitingForOperator: false,
				Line: 10, File: "X",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			pkt := tt.want.Encode()
			if len(pkt) != statusLen {
				t.Fatalf("len(pkt) = %d, want %d", len(pkt), statusLen)
			}
			if pkt[7] != 0xFF {
				t.Errorf("pkt[7] = %#02x, want 0xFF", pkt[7])
			}
			got, err := DecodeReply(pkt)
			if err != nil {
				t.Fatalf("DecodeReply: unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("DecodeReply(Encode()) = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestStatusEncodeTruncatesLongFile(t *testing.T) {
	t.Parallel()

	r := Status{File: strings.Repeat("X", 300)}
	pkt := r.Encode()
	if len(pkt) != statusLen {
		t.Fatalf("len(pkt) = %d, want %d", len(pkt), statusLen)
	}
	got, err := DecodeReply(pkt)
	if err != nil {
		t.Fatalf("DecodeReply: unexpected error: %v", err)
	}
	if st, ok := got.(Status); !ok || st.File != strings.Repeat("X", MaxStatusFile) {
		t.Errorf("DecodeReply = %#v, want Status with %d X's as File", got, MaxStatusFile)
	}
}

func TestDecodeStatusStopsAtByte49(t *testing.T) {
	t.Parallel()

	pkt := Status{File: strings.Repeat("F", MaxStatusFile)}.Encode()
	for i := statusFileStart + MaxStatusFile; i < statusLen; i++ {
		pkt[i] = 'R'
	}
	binary.LittleEndian.PutUint16(pkt[0:2], crc16XModem(pkt[2:]))

	got, err := DecodeReply(pkt)
	if err != nil {
		t.Fatalf("DecodeReply: unexpected error: %v", err)
	}
	if st, ok := got.(Status); !ok || st.File != strings.Repeat("F", MaxStatusFile) {
		t.Errorf("DecodeReply = %#v, want Status with %d F's as File", got, MaxStatusFile)
	}
}

func TestToolRecordRoundTrip(t *testing.T) {
	t.Parallel()

	want := ToolRecord{Index: 3, Name: "1/4 endmill"}
	pkt := want.Encode()
	if len(pkt) != toolRecordLen {
		t.Fatalf("len(pkt) = %d, want %d", len(pkt), toolRecordLen)
	}
	got, err := DecodeReply(pkt)
	if err != nil {
		t.Fatalf("DecodeReply: unexpected error: %v", err)
	}
	if got != want {
		t.Errorf("DecodeReply(Encode()) = %#v, want %#v", got, want)
	}
}

func TestToolRecordEncodeEmptyName(t *testing.T) {
	t.Parallel()

	want := ToolRecord{Index: 9, Name: ""}
	got, err := DecodeReply(want.Encode())
	if err != nil {
		t.Fatalf("DecodeReply: unexpected error: %v", err)
	}
	if got != want {
		t.Errorf("DecodeReply(Encode()) = %#v, want %#v", got, want)
	}
}

func TestStartAckRoundTripAndErr(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		result  byte
		wantErr error
	}{
		{"ok", StartOK, nil},
		{"no usb", StartNoUSB, ErrNoUSB},
		{"already started", StartAlreadyStarted, ErrTransfer},
		{"other", 0x42, ErrTransfer},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			want := StartAck{Result: tt.result}
			pkt := want.Encode()
			if len(pkt) != startAckLen {
				t.Fatalf("len(pkt) = %d, want %d", len(pkt), startAckLen)
			}
			got, err := DecodeReply(pkt)
			if err != nil {
				t.Fatalf("DecodeReply: unexpected error: %v", err)
			}
			if got != Reply(want) {
				t.Errorf("DecodeReply(Encode()) = %#v, want %#v", got, want)
			}
			if err := want.Err(); !errors.Is(err, tt.wantErr) {
				t.Errorf("Err() = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

func TestChunkAckRoundTripAndErr(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		result  byte
		wantErr error
	}{
		{"ok", ChunkOK, nil},
		{"usb write error", ChunkUSBWriteError, ErrUSBWrite},
		{"canceled", ChunkCanceled, ErrCanceled},
		{"other", 0x7F, ErrTransfer},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			want := ChunkAck{Result: tt.result, Accepted: 17}
			pkt := want.Encode()
			if len(pkt) != chunkAckLen {
				t.Fatalf("len(pkt) = %d, want %d", len(pkt), chunkAckLen)
			}
			got, err := DecodeReply(pkt)
			if err != nil {
				t.Fatalf("DecodeReply: unexpected error: %v", err)
			}
			if got != Reply(want) {
				t.Errorf("DecodeReply(Encode()) = %#v, want %#v", got, want)
			}
			if err := want.Err(); !errors.Is(err, tt.wantErr) {
				t.Errorf("Err() = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

func TestDecodeReplyRejectsUnknownType(t *testing.T) {
	t.Parallel()

	pkt := frame(0x99, nil)
	if _, err := DecodeReply(pkt); !errors.Is(err, ErrUnknownType) {
		t.Errorf("DecodeReply: err = %v, want ErrUnknownType", err)
	}
}

func TestDecodeReplyRejectsWrongLength(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		pkt  []byte
	}{
		{"identity too short", Identity{}.Encode()[:identityLen-1]},
		{"identity too long", append(Identity{}.Encode(), 0)},
		{"config reply too short", ConfigReply{}.Encode()[:configReplyLen-1]},
		{"status too short", Status{}.Encode()[:statusLen-1]},
		{"tool record too short", ToolRecord{}.Encode()[:toolRecordLen-1]},
		{"start ack too short", StartAck{}.Encode()[:startAckLen-1]},
		{"chunk ack too short", ChunkAck{}.Encode()[:chunkAckLen-1]},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			// Re-truncating invalidates the CRC for all but the "too
			// long" case; re-sign so DecodeReply reaches the length
			// check rather than failing on CRC first.
			pkt := append([]byte(nil), tt.pkt...)
			if len(pkt) >= 4 {
				crc := crc16XModem(pkt[2:])
				pkt[0], pkt[1] = byte(crc), byte(crc>>8)
			}
			if _, err := DecodeReply(pkt); !errors.Is(err, ErrBadLength) {
				t.Errorf("DecodeReply(%s): err = %v, want ErrBadLength", tt.name, err)
			}
		})
	}
}

func TestDecodeReplyPropagatesFramingErrors(t *testing.T) {
	t.Parallel()

	if _, err := DecodeReply([]byte{1, 2}); !errors.Is(err, ErrShortPacket) {
		t.Errorf("DecodeReply: err = %v, want ErrShortPacket", err)
	}
}

func TestPutCStringTruncatesToFit(t *testing.T) {
	t.Parallel()

	dst := make([]byte, 4)
	putCString(dst, "hello")
	if want := []byte("hel\x00"); !bytes.Equal(dst, want) {
		t.Errorf("putCString truncated to %v, want %v", dst, want)
	}
}

func TestCStringWithoutTerminator(t *testing.T) {
	t.Parallel()

	if got := cString([]byte("no nul here")); got != "no nul here" {
		t.Errorf("cString = %q, want %q", got, "no nul here")
	}
}
