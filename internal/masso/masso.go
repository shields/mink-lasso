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

// Package masso implements the wire codec for the UDP protocol spoken
// between a client application and a Masso G3 Touch CNC controller, as
// documented in docs/protocol.md. It covers framing plus request and reply
// encoding and decoding, and the socket-owning Client built on top; it does
// not simulate a controller (see internal/masso/sim).
package masso

// Network constants from docs/protocol.md §1.
const (
	// ControllerPort is the UDP port every controller listens on for
	// client requests.
	ControllerPort = 65535

	// ListenPortMin is the first port a client tries to bind for replies.
	ListenPortMin = 11000

	// ListenPortMax is the last port a client tries to bind for replies.
	ListenPortMax = 11051

	// MaxChunkData is the largest number of file bytes a single upload
	// chunk (TypeUploadChunk) may carry.
	MaxChunkData = 1422

	// MaxFileName is the longest file name, in ASCII bytes, the controller
	// accepts for an upload.
	MaxFileName = 15

	// MaxUploadDir is the longest upload directory, in bytes, that fits the
	// upload-start request's one-byte path length.
	MaxUploadDir = 255

	// MaxStatusFile is the longest current-file name, in ASCII bytes, a
	// status reply carries.
	MaxStatusFile = 33

	// MaxPacket is a receive buffer size large enough for any reply this
	// package decodes; the largest is the 270-byte status packet.
	MaxPacket = 2048
)

// Packet type bytes (offset 4 of every datagram), from docs/protocol.md §3
// and §5.5.
// The controller reuses the same type byte for a request and its reply.
const (
	// TypeStatus marks a keepalive/status request or a status reply.
	TypeStatus byte = 0x01

	// TypeDiscovery marks a discovery request or an identity reply.
	TypeDiscovery byte = 0x02

	// TypeConfig marks a config (handshake) request or its serial-number
	// reply.
	TypeConfig byte = 0x03

	// TypeTool marks a tool-table query or a tool-record reply.
	TypeTool byte = 0x08

	// TypeUploadStart marks an upload-start request or its ACK.
	TypeUploadStart byte = 0x0A

	// TypeUploadChunk marks an upload data-chunk request or its ACK.
	TypeUploadChunk byte = 0x0B

	// TypeUploadAbort marks the notification a client sends after an
	// acknowledged upload fails. It has no reply.
	TypeUploadAbort byte = 0x0C
)
