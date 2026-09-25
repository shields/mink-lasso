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

import "errors"

// Framing errors returned by parse for a malformed datagram.
var (
	// ErrShortPacket indicates a datagram shorter than the 5-byte header
	// (2-byte CRC, 2-byte magic, 1-byte type).
	ErrShortPacket = errors.New("masso: packet shorter than the 5-byte header")

	// ErrBadMagic indicates the magic bytes at offset 2-3 were not 03 00.
	ErrBadMagic = errors.New("masso: bad magic bytes")

	// ErrBadCRC indicates the CRC-16/XMODEM checksum did not match.
	ErrBadCRC = errors.New("masso: CRC mismatch")
)

// Errors for higher-level structural problems found while decoding a
// request or reply whose framing already parsed successfully.
var (
	// ErrUnknownType indicates a packet type this package does not
	// recognize.
	ErrUnknownType = errors.New("masso: unknown packet type")

	// ErrBadLength indicates a packet whose length does not hold the
	// fields its type requires.
	ErrBadLength = errors.New("masso: unexpected packet length")

	// ErrMalformed indicates a packet with a recognized type and a
	// plausible length but an internally inconsistent body (for example,
	// a length field that overruns the packet).
	ErrMalformed = errors.New("masso: malformed packet body")

	// ErrChunkTooLarge indicates an upload chunk larger than
	// MaxChunkData.
	ErrChunkTooLarge = errors.New("masso: chunk data exceeds MaxChunkData")

	// ErrBadFileName indicates a file name that fails ValidateFileName.
	ErrBadFileName = errors.New("masso: invalid file name")

	// ErrBadUploadDir indicates an upload directory that fails
	// ValidateUploadDir.
	ErrBadUploadDir = errors.New("masso: invalid upload directory")

	// ErrBadSerial indicates a string ParseSerial could not parse as a
	// controller serial number.
	ErrBadSerial = errors.New("masso: invalid serial number")
)

// Errors mapped from controller result bytes by StartAck.Err and
// ChunkAck.Err, and shared with Client. The message text matches what
// Masso Link itself displays, so the UI can reuse it directly.
var (
	// ErrNoUSB indicates the controller has no USB flash drive to write
	// to.
	ErrNoUSB = errors.New("no USB flash drive connected to Masso")

	// ErrTransfer indicates a generic, otherwise-unclassified transfer
	// error reported by the controller.
	ErrTransfer = errors.New("error occurred while transferring file")

	// ErrUSBWrite indicates the controller could not write a chunk to its
	// USB flash drive.
	ErrUSBWrite = errors.New("unable to write file to USB")

	// ErrCanceled indicates the operator canceled the transfer on the
	// Masso's own screen.
	ErrCanceled = errors.New("file transfer canceled by user on Masso")

	// ErrNoResponse indicates no reply arrived from the controller within
	// the expected timeout.
	ErrNoResponse = errors.New("no response from Masso")

	// ErrLost indicates a previously connected controller stopped sending
	// any datagram this Client counts toward connection liveness
	// (docs/protocol.md §8).
	ErrLost = errors.New("lost connection to Masso")
)
