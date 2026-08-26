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

import "encoding/binary"

// magic identifies a Masso Link protocol datagram; it appears at bytes 2-3
// of every packet, immediately after the CRC (docs/protocol.md §2).
var magic = [2]byte{0x03, 0x00}

// crc16XModem computes CRC-16/XMODEM (polynomial 0x1021, initial value
// 0x0000, no input or output reflection) over data, matching the reference
// implementation in docs/protocol.md §2.
func crc16XModem(data []byte) uint16 {
	var crc uint16
	for _, b := range data {
		crc ^= uint16(b) << 8
		for range 8 {
			if crc&0x8000 != 0 {
				crc = crc<<1 ^ 0x1021
			} else {
				crc <<= 1
			}
		}
	}
	return crc
}

// roundUp4 rounds n up to the next multiple of 4.
func roundUp4(n int) int {
	return (n + 3) &^ 3
}

// frame assembles a complete datagram — CRC (little-endian) | 03 00 | type
// | payload — zero-padding the body (everything after the CRC) so its
// length is a multiple of 4, per docs/protocol.md §2. The CRC is computed
// over the padded body, from the magic bytes through the end.
func frame(typ byte, payload []byte) []byte {
	body := make([]byte, roundUp4(3+len(payload)))
	body[0], body[1] = magic[0], magic[1]
	body[2] = typ
	copy(body[3:], payload)

	pkt := make([]byte, 2+len(body))
	binary.LittleEndian.PutUint16(pkt[0:2], crc16XModem(body))
	copy(pkt[2:], body)
	return pkt
}

// parse validates a received datagram's framing — minimum length, magic
// bytes, and CRC — and returns its type byte and body (everything after
// the type byte, including any trailing padding).
func parse(pkt []byte) (byte, []byte, error) {
	if len(pkt) < 5 {
		return 0, nil, ErrShortPacket
	}
	if pkt[2] != magic[0] || pkt[3] != magic[1] {
		return 0, nil, ErrBadMagic
	}
	want := binary.LittleEndian.Uint16(pkt[0:2])
	got := crc16XModem(pkt[2:])
	if want != got {
		return 0, nil, ErrBadCRC
	}
	return pkt[4], pkt[5:], nil
}
