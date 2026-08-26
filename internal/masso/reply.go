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
	"fmt"
)

// Reply is implemented by every decoded controller-to-client reply:
// Identity, ConfigReply, Status, ToolRecord, StartAck, and ChunkAck. The
// set of implementations is closed to this package.
type Reply interface {
	isReply()
}

// Fixed reply lengths, from docs/protocol.md §3-5. Unlike requests,
// replies are strict about length: DecodeReply rejects anything that does
// not match exactly, since (length, type) together identify the shape.
const (
	identityLen    = 46
	configReplyLen = 10
	statusLen      = 270
	toolRecordLen  = 38
	startAckLen    = 10
	chunkAckLen    = 10
)

// versionMarker is the '@' byte that precedes the version string in an
// Identity reply; locating it this way is robust across firmware versions
// (docs/protocol.md §3.1).
const versionMarker = 0x40

// Identity is the 46-byte reply to a discovery request (TypeDiscovery).
type Identity struct {
	// Serial is the controller's serial number.
	Serial uint16
	// Version is the firmware version string (for example
	// "5-Axis v5.13").
	Version string
}

func (Identity) isReply() {}

// Encode returns the 46-byte wire form of r.
func (r Identity) Encode() []byte {
	pkt := make([]byte, identityLen)
	pkt[2], pkt[3] = magic[0], magic[1]
	pkt[4] = TypeDiscovery
	binary.LittleEndian.PutUint16(pkt[5:7], r.Serial)
	pkt[12] = versionMarker
	putCString(pkt[13:], r.Version)
	binary.LittleEndian.PutUint16(pkt[0:2], crc16XModem(pkt[2:]))
	return pkt
}

// ConfigReply is the 10-byte reply to a config (handshake) request
// (TypeConfig).
type ConfigReply struct {
	// Serial is the controller's serial number.
	Serial uint16
}

func (ConfigReply) isReply() {}

// Encode returns the 10-byte wire form of r.
func (r ConfigReply) Encode() []byte {
	pkt := make([]byte, configReplyLen)
	pkt[2], pkt[3] = magic[0], magic[1]
	pkt[4] = TypeConfig
	binary.LittleEndian.PutUint16(pkt[5:7], r.Serial)
	pkt[9] = 0x0A
	binary.LittleEndian.PutUint16(pkt[0:2], crc16XModem(pkt[2:]))
	return pkt
}

// runStateRunning and promptWaiting are the only documented values of the
// Status.State and Status.Prompt bytes (docs/protocol.md §4).
const (
	runStateRunning = 0x02
	promptWaiting   = 0x00
)

// Status is the 270-byte status reply (TypeStatus), sent once per
// keepalive request at roughly 1 Hz.
type Status struct {
	// Progress is the current job's completion percentage, 0-100.
	Progress uint8
	// State is the raw run-state byte (offset 6): 0x00 for
	// stopped/idle/feed-hold/e-stop, 0x02 for running. Other values are
	// undocumented; it is kept raw so callers can surface them.
	State byte
	// Running reports whether State == 0x02.
	Running bool
	// Jobs is the controller's lifetime job counter.
	Jobs uint32
	// Prompt is the raw user-prompt byte (offset 12): 0x01 for normal,
	// 0x00 for paused waiting for the operator. Other values are
	// undocumented; it is kept raw so callers can surface them.
	Prompt byte
	// WaitingForOperator reports whether Prompt == 0x00.
	WaitingForOperator bool
	// Line is the current program line number.
	Line uint32
	// File is the current file name, empty when idle.
	File string
}

func (Status) isReply() {}

// Encode returns the 270-byte wire form of r.
func (r Status) Encode() []byte {
	pkt := make([]byte, statusLen)
	pkt[2], pkt[3] = magic[0], magic[1]
	pkt[4] = TypeStatus
	pkt[5] = r.Progress
	pkt[6] = r.State
	pkt[7] = 0xFF
	binary.LittleEndian.PutUint32(pkt[8:12], r.Jobs)
	pkt[12] = r.Prompt
	binary.LittleEndian.PutUint32(pkt[13:17], r.Line)
	putCString(pkt[17:], r.File)
	binary.LittleEndian.PutUint16(pkt[0:2], crc16XModem(pkt[2:]))
	return pkt
}

// ToolRecord is the 38-byte reply to a tool-table query (TypeTool).
type ToolRecord struct {
	// Index is the 1-based tool index this record answers.
	Index uint8
	// Name is the tool's name, empty for an unnamed tool.
	Name string
}

func (ToolRecord) isReply() {}

// Encode returns the 38-byte wire form of r.
func (r ToolRecord) Encode() []byte {
	pkt := make([]byte, toolRecordLen)
	pkt[2], pkt[3] = magic[0], magic[1]
	pkt[4] = TypeTool
	pkt[5] = r.Index
	putCString(pkt[6:], r.Name)
	binary.LittleEndian.PutUint16(pkt[0:2], crc16XModem(pkt[2:]))
	return pkt
}

// Upload-start ACK result codes (docs/protocol.md §5.1).
const (
	// StartOK indicates the USB drive is present and writable.
	StartOK = 0x00
	// StartNoUSB indicates no USB flash drive is connected.
	StartNoUSB = 0xE9
)

// StartAck is the 10-byte reply to an upload-start request
// (TypeUploadStart).
type StartAck struct {
	// Result is the raw result byte; see StartOK and StartNoUSB. Any
	// other value is a generic transfer error.
	Result byte
}

func (StartAck) isReply() {}

// Encode returns the 10-byte wire form of a.
func (a StartAck) Encode() []byte {
	pkt := make([]byte, startAckLen)
	pkt[2], pkt[3] = magic[0], magic[1]
	pkt[4] = TypeUploadStart
	pkt[5] = a.Result
	binary.LittleEndian.PutUint16(pkt[0:2], crc16XModem(pkt[2:]))
	return pkt
}

// Err maps a.Result to a sentinel error, or nil for StartOK.
func (a StartAck) Err() error {
	switch a.Result {
	case StartOK:
		return nil
	case StartNoUSB:
		return ErrNoUSB
	default:
		return ErrTransfer
	}
}

// Upload-chunk ACK result codes (docs/protocol.md §5.2).
const (
	// ChunkOK indicates the chunk was written successfully.
	ChunkOK = 0x00
	// ChunkUSBWriteError indicates the controller could not write the
	// chunk to its USB drive.
	ChunkUSBWriteError = 0x01
	// ChunkCanceled indicates the operator canceled the transfer on the
	// Masso.
	ChunkCanceled = 0x02
)

// ChunkAck is the 10-byte reply to an upload data-chunk request
// (TypeUploadChunk).
type ChunkAck struct {
	// Result is the raw result byte; see ChunkOK, ChunkUSBWriteError, and
	// ChunkCanceled. Any other value is a generic transfer error.
	Result byte
	// Accepted is the number of chunks the controller has accepted so
	// far.
	Accepted uint32
}

func (ChunkAck) isReply() {}

// Encode returns the 10-byte wire form of a.
func (a ChunkAck) Encode() []byte {
	pkt := make([]byte, chunkAckLen)
	pkt[2], pkt[3] = magic[0], magic[1]
	pkt[4] = TypeUploadChunk
	pkt[5] = a.Result
	binary.LittleEndian.PutUint32(pkt[6:10], a.Accepted)
	binary.LittleEndian.PutUint16(pkt[0:2], crc16XModem(pkt[2:]))
	return pkt
}

// Err maps a.Result to a sentinel error, or nil for ChunkOK.
func (a ChunkAck) Err() error {
	switch a.Result {
	case ChunkOK:
		return nil
	case ChunkUSBWriteError:
		return ErrUSBWrite
	case ChunkCanceled:
		return ErrCanceled
	default:
		return ErrTransfer
	}
}

// putCString writes s into dst as a NUL-terminated string, truncating s if
// necessary to leave room for the terminator; any remaining bytes in dst
// are left zero. dst must have length >= 1.
func putCString(dst []byte, s string) {
	limit := len(dst) - 1
	if len(s) > limit {
		s = s[:limit]
	}
	copy(dst, s)
	dst[len(s)] = 0
}

// cString returns the NUL-terminated string at the start of b, or all of b
// if it contains no NUL.
func cString(b []byte) string {
	before, _, _ := bytes.Cut(b, []byte{0})
	return string(before)
}

// DecodeReply parses a controller-to-client datagram and returns its
// decoded form: Identity, ConfigReply, Status, ToolRecord, StartAck, or
// ChunkAck.
func DecodeReply(pkt []byte) (Reply, error) {
	typ, _, err := parse(pkt)
	if err != nil {
		return nil, err
	}
	switch typ {
	case TypeDiscovery:
		return decodeIdentity(pkt)
	case TypeConfig:
		if len(pkt) != configReplyLen {
			return nil, fmt.Errorf("%w: config reply", ErrBadLength)
		}
		return ConfigReply{Serial: binary.LittleEndian.Uint16(pkt[5:7])}, nil
	case TypeStatus:
		if len(pkt) != statusLen {
			return nil, fmt.Errorf("%w: status reply", ErrBadLength)
		}
		return decodeStatus(pkt), nil
	case TypeTool:
		if len(pkt) != toolRecordLen {
			return nil, fmt.Errorf("%w: tool record reply", ErrBadLength)
		}
		return decodeToolRecord(pkt), nil
	case TypeUploadStart:
		if len(pkt) != startAckLen {
			return nil, fmt.Errorf("%w: start ack reply", ErrBadLength)
		}
		return StartAck{Result: pkt[5]}, nil
	case TypeUploadChunk:
		if len(pkt) != chunkAckLen {
			return nil, fmt.Errorf("%w: chunk ack reply", ErrBadLength)
		}
		return ChunkAck{Result: pkt[5], Accepted: binary.LittleEndian.Uint32(pkt[6:10])}, nil
	default:
		return nil, fmt.Errorf("%w: 0x%02X", ErrUnknownType, typ)
	}
}

func decodeIdentity(pkt []byte) (Reply, error) {
	if len(pkt) != identityLen {
		return nil, fmt.Errorf("%w: identity reply", ErrBadLength)
	}
	serial := binary.LittleEndian.Uint16(pkt[5:7])
	idx := bytes.IndexByte(pkt[7:], versionMarker)
	if idx < 0 {
		return nil, fmt.Errorf("%w: identity reply missing version marker", ErrMalformed)
	}
	start := 7 + idx + 1
	return Identity{Serial: serial, Version: cString(pkt[start:])}, nil
}

func decodeStatus(pkt []byte) Reply {
	return Status{
		Progress:           pkt[5],
		State:              pkt[6],
		Running:            pkt[6] == runStateRunning,
		Jobs:               binary.LittleEndian.Uint32(pkt[8:12]),
		Prompt:             pkt[12],
		WaitingForOperator: pkt[12] == promptWaiting,
		Line:               binary.LittleEndian.Uint32(pkt[13:17]),
		File:               cString(pkt[17:]),
	}
}

func decodeToolRecord(pkt []byte) Reply {
	return ToolRecord{Index: pkt[5], Name: cString(pkt[6:])}
}
