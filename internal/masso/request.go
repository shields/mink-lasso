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
	"strings"
	"time"
)

// Request is implemented by every decoded client-to-controller request:
// DiscoveryRequest, ConfigRequest, KeepaliveRequest, ToolQueryRequest,
// UploadStartRequest, UploadChunkRequest, and UploadAbortRequest. The set of
// implementations is closed to this package.
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
	// Path is the raw path field: `\` for the drive root, otherwise a
	// backslash-separated directory. JoinUploadPath combines it with Name.
	Path string
	// Name is the bare file name.
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

// UploadAbortRequest is a decoded upload-abort notification
// (TypeUploadAbort). It carries no fields.
type UploadAbortRequest struct{}

func (UploadAbortRequest) isRequest() {}

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

// ToolQuery encodes a tool-table query for the 1-based tool index, followed
// by four zero bytes (docs/protocol.md §3.4).
func ToolQuery(index uint8) []byte {
	return frame(TypeTool, []byte{index, 0, 0, 0, 0})
}

const rootUploadPath = `\`

// uploadStartReserved is the number of zero bytes that follow the name's
// NUL in an upload-start request (docs/protocol.md §5.1).
const uploadStartReserved = 3

// UploadStart encodes an upload-start request for a size-byte file named
// name in directory dir on the USB drive (docs/protocol.md §5.1). An empty
// dir means the drive root; otherwise dir is a backslash-separated relative
// directory such as `JOBS\SUB`. It returns ErrBadFileName if name fails
// ValidateFileName, or ErrBadUploadDir if dir fails ValidateUploadDir.
func UploadStart(size uint32, dir, name string) ([]byte, error) {
	if err := ValidateFileName(name); err != nil {
		return nil, err
	}
	if err := ValidateUploadDir(dir); err != nil {
		return nil, err
	}
	path := dir
	if path == "" {
		path = rootUploadPath
	}
	payload := make([]byte, 0, 4+2+1+len(path)+1+len(name)+1+uploadStartReserved)
	payload = binary.LittleEndian.AppendUint32(payload, size)
	payload = append(payload, 0, 0) // reserved
	// ValidateUploadDir bounds path to MaxUploadDir (255) bytes; the mask
	// proves that to the static analyzer.
	payload = append(payload, byte(len(path)&0xFF))
	payload = append(payload, path...)
	payload = append(payload, 0)
	payload = append(payload, name...)
	payload = append(payload, 0)
	payload = append(payload, make([]byte, uploadStartReserved)...)
	return frame(TypeUploadStart, payload), nil
}

// JoinUploadPath returns the drive-relative location an upload-start
// request's path and name fields name, without a leading separator: the
// bare name for a root upload, otherwise `DIR\NAME`.
func JoinUploadPath(path, name string) string {
	dir := strings.Trim(path, `\`)
	if dir == "" {
		return name
	}
	return dir + `\` + name
}

// UploadAbort encodes the payload-free notification sent after an
// acknowledged upload fails (docs/protocol.md §5.5).
func UploadAbort() []byte {
	return frame(TypeUploadAbort, make([]byte, 5))
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
// ToolQueryRequest, UploadStartRequest, UploadChunkRequest, or
// UploadAbortRequest. Decoders
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
	case TypeUploadAbort:
		return UploadAbortRequest{}, nil
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
// path(pathlen) | 0x00 | name | 0x00 | padding.
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
	path := string(body[minHeader : minHeader+pathLen])
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
	return UploadStartRequest{Size: size, Path: path, Name: string(name)}, nil
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
