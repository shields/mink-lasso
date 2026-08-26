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
	"time"
)

// Request is implemented by every decoded client-to-controller request:
// DiscoveryRequest, ConfigRequest, KeepaliveRequest, ToolQueryRequest,
// UploadStartRequest, and UploadChunkRequest. The set of implementations is
// closed to this package.
type Request interface {
	isRequest()
}

// DiscoveryRequest is a decoded discovery request (TypeDiscovery): it asks
// every controller that hears it to send its identity, and all future
// replies, to ReplyPort.
type DiscoveryRequest struct {
	// ReplyPort is the UDP port the controller should target for all
	// future replies to this client.
	ReplyPort uint16
}

func (DiscoveryRequest) isRequest() {}

// ConfigRequest is a decoded config (handshake) request (TypeConfig). The
// controller ignores these fields; Masso Link sends the wall clock. Year is
// two digits (26 for 2026).
type ConfigRequest struct {
	Hour, Minute, Second byte
	Day, Month, Year     byte
}

func (ConfigRequest) isRequest() {}

// KeepaliveRequest is a decoded keepalive/status request (TypeStatus). The
// controller ignores these fields and replies with a Status.
type KeepaliveRequest struct {
	Hour, Minute, Second byte
	Day, Month           byte
}

func (KeepaliveRequest) isRequest() {}

// ToolQueryRequest is a decoded tool-table query (TypeTool).
type ToolQueryRequest struct {
	// Index is the 1-based tool index being queried.
	Index uint8
}

func (ToolQueryRequest) isRequest() {}

// UploadStartRequest is a decoded upload-start request (TypeUploadStart).
type UploadStartRequest struct {
	// Size is the total file size in bytes.
	Size uint32
	// Name is the file name, rooted at the USB drive.
	Name string
}

func (UploadStartRequest) isRequest() {}

// UploadChunkRequest is a decoded upload data-chunk request
// (TypeUploadChunk).
type UploadChunkRequest struct {
	// Index is the 0-based chunk index.
	Index uint32
	// Data is this chunk's file bytes (at most MaxChunkData).
	Data []byte
}

func (UploadChunkRequest) isRequest() {}

// Discovery encodes a discovery request that asks every controller that
// hears it to send replies to replyPort (docs/protocol.md §3.1).
func Discovery(replyPort uint16) []byte {
	payload := make([]byte, 5)
	binary.LittleEndian.PutUint16(payload[0:2], replyPort)
	return frame(TypeDiscovery, payload)
}

// clockByte truncates n to a byte. It is used only for wall-clock fields
// (hour, minute, second, day, month, two-digit year) that package time
// guarantees are always small, so the truncation never actually loses
// information; the mask just proves that to the static analyzer.
func clockByte(n int) byte {
	return byte(n & 0xFF)
}

// Config encodes a config (handshake) request carrying t as the wall
// clock; the controller ignores these bytes (docs/protocol.md §3.2).
func Config(t time.Time) []byte {
	payload := []byte{
		clockByte(t.Hour()), clockByte(t.Minute()), clockByte(t.Second()),
		clockByte(t.Day()), clockByte(int(t.Month())), clockByte(t.Year() % 100),
		0, 0, 0,
	}
	return frame(TypeConfig, payload)
}

// Keepalive encodes a keepalive/status request carrying t as the wall
// clock; the controller ignores these bytes and replies with a status
// packet (docs/protocol.md §3.3).
func Keepalive(t time.Time) []byte {
	payload := []byte{
		clockByte(t.Hour()), clockByte(t.Minute()), clockByte(t.Second()),
		clockByte(t.Day()), clockByte(int(t.Month())),
	}
	return frame(TypeStatus, payload)
}

// toolQueryTrailer is the constant trailer Masso Link appends to every
// tool query; only the index byte varies (docs/protocol.md §3.4).
var toolQueryTrailer = [4]byte{0x22, 0x2C, 0x1C, 0x0B}

// ToolQuery encodes a tool-table query for the 1-based tool index.
func ToolQuery(index uint8) []byte {
	payload := append([]byte{index}, toolQueryTrailer[:]...)
	return frame(TypeTool, payload)
}

// uploadPath is the fixed root path ("\") every upload targets. The
// protocol supports subdirectory paths, but this package only ever
// uploads to the drive root.
const uploadPath = `\`

// uploadNameField is the fixed size of the name field in an upload-start
// request: MaxFileName characters plus a NUL. Masso Link always sends the
// field at this size (a captured start packet for "CLTEST.NC" is 30 bytes,
// six zero bytes after the name's NUL), and the controller has only ever
// been seen accepting that layout, so it is reproduced exactly rather than
// padded to the 4-byte rule that every other request follows.
const uploadNameField = MaxFileName + 1

// UploadStart encodes an upload-start request for a size-byte file named
// name, rooted at the USB drive (docs/protocol.md §5.1). It returns
// ErrBadFileName if name fails ValidateFileName.
func UploadStart(size uint32, name string) ([]byte, error) {
	if err := ValidateFileName(name); err != nil {
		return nil, err
	}
	payload := make([]byte, 0, 4+2+1+len(uploadPath)+1+uploadNameField)
	sizeBuf := make([]byte, 4)
	binary.LittleEndian.PutUint32(sizeBuf, size)
	payload = append(payload, sizeBuf...)
	payload = append(payload, 0, 0) // reserved
	payload = append(payload, byte(len(uploadPath)))
	payload = append(payload, uploadPath...)
	payload = append(payload, 0) // path terminator
	payload = append(payload, name...)
	payload = append(payload, make([]byte, uploadNameField-len(name))...) // NUL and fill
	return frame(TypeUploadStart, payload), nil
}

// UploadChunk encodes a data chunk for a 0-based chunk index
// (docs/protocol.md §5.2). It returns ErrChunkTooLarge if len(data)
// exceeds MaxChunkData.
func UploadChunk(index uint32, data []byte) ([]byte, error) {
	if len(data) > MaxChunkData {
		return nil, fmt.Errorf("%w: %d bytes", ErrChunkTooLarge, len(data))
	}
	payload := make([]byte, 8+len(data))
	binary.LittleEndian.PutUint32(payload[0:4], index)
	// len(data) is already bounded by the MaxChunkData check above; the
	// mask just proves that to the static analyzer.
	binary.LittleEndian.PutUint32(payload[4:8], uint32(len(data)&0xFFFFFFFF))
	copy(payload[8:], data)
	return frame(TypeUploadChunk, payload), nil
}

// DecodeRequest parses a client-to-controller datagram and returns its
// decoded form: DiscoveryRequest, ConfigRequest, KeepaliveRequest,
// ToolQueryRequest, UploadStartRequest, or UploadChunkRequest. Decoders
// require only the bytes needed for their meaningful fields, tolerating
// whatever trailing padding a real sender includes.
func DecodeRequest(pkt []byte) (Request, error) {
	typ, body, err := parse(pkt)
	if err != nil {
		return nil, err
	}
	switch typ {
	case TypeDiscovery:
		return decodeDiscoveryRequest(body)
	case TypeConfig:
		return decodeConfigRequest(body)
	case TypeStatus:
		return decodeKeepaliveRequest(body)
	case TypeTool:
		return decodeToolQueryRequest(body)
	case TypeUploadStart:
		return decodeUploadStartRequest(body)
	case TypeUploadChunk:
		return decodeUploadChunkRequest(body)
	default:
		return nil, fmt.Errorf("%w: 0x%02X", ErrUnknownType, typ)
	}
}

func decodeDiscoveryRequest(body []byte) (Request, error) {
	if len(body) < 2 {
		return nil, fmt.Errorf("%w: discovery request", ErrBadLength)
	}
	return DiscoveryRequest{ReplyPort: binary.LittleEndian.Uint16(body[0:2])}, nil
}

func decodeConfigRequest(body []byte) (Request, error) {
	if len(body) < 6 {
		return nil, fmt.Errorf("%w: config request", ErrBadLength)
	}
	return ConfigRequest{
		Hour: body[0], Minute: body[1], Second: body[2],
		Day: body[3], Month: body[4], Year: body[5],
	}, nil
}

func decodeKeepaliveRequest(body []byte) (Request, error) {
	if len(body) < 5 {
		return nil, fmt.Errorf("%w: keepalive request", ErrBadLength)
	}
	return KeepaliveRequest{
		Hour: body[0], Minute: body[1], Second: body[2],
		Day: body[3], Month: body[4],
	}, nil
}

func decodeToolQueryRequest(body []byte) (Request, error) {
	if len(body) < 1 {
		return nil, fmt.Errorf("%w: tool query request", ErrBadLength)
	}
	return ToolQueryRequest{Index: body[0]}, nil
}

// decodeUploadStartRequest parses size(4) | reserved(2) | pathlen(1) |
// path(pathlen) | 0x00 | name | 0x00 | padding. pathLen is read from the
// packet rather than assumed to be 1, so this decodes whatever a real
// client sends even though UploadStart always encodes a bare "\" path.
func decodeUploadStartRequest(body []byte) (Request, error) {
	const minHeader = 4 + 2 + 1 // size + reserved + pathlen
	if len(body) < minHeader {
		return nil, fmt.Errorf("%w: upload-start request", ErrBadLength)
	}
	size := binary.LittleEndian.Uint32(body[0:4])
	pathLen := int(body[6])
	nameStart := minHeader + pathLen + 1 // + path's NUL terminator
	if len(body) < nameStart {
		return nil, fmt.Errorf("%w: upload-start request path", ErrMalformed)
	}
	name := body[nameStart:]
	if i := bytes.IndexByte(name, 0); i >= 0 {
		name = name[:i]
	}
	// UploadStart's encoder never produces a name ValidateFileName would
	// reject; re-checking it here means a decoder used to drive
	// internal/masso/sim (or any other server-side consumer) rejects a
	// name a real controller never would have accepted either, instead of
	// silently storing it under an unvalidated key.
	if err := ValidateFileName(string(name)); err != nil {
		return nil, err
	}
	return UploadStartRequest{Size: size, Name: string(name)}, nil
}

func decodeUploadChunkRequest(body []byte) (Request, error) {
	const header = 8
	if len(body) < header {
		return nil, fmt.Errorf("%w: upload-chunk request", ErrBadLength)
	}
	index := binary.LittleEndian.Uint32(body[0:4])
	length := binary.LittleEndian.Uint32(body[4:8])
	// UploadChunk's encoder never produces a chunk larger than
	// MaxChunkData; re-checking it here means a decoder used to drive
	// internal/masso/sim (or any other server-side consumer) rejects an
	// oversized chunk outright instead of silently accepting data past the
	// protocol's chunk cap and ACKing as if nothing were wrong.
	if length > MaxChunkData {
		return nil, fmt.Errorf("%w: %d bytes", ErrChunkTooLarge, length)
	}
	if uint64(header)+uint64(length) > uint64(len(body)) {
		return nil, fmt.Errorf("%w: upload-chunk request length", ErrMalformed)
	}
	data := make([]byte, length)
	copy(data, body[header:header+int(length)])
	return UploadChunkRequest{Index: index, Data: data}, nil
}
