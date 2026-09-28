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

//go:build integration

package integration

import (
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"testing"
	"time"

	"msrl.dev/mink-lasso/internal/masso"
)

// probeAttempts and probeReplyWait bound every probe exchange that is not
// itself the thing under test: enough retries to ride out an occasional
// dropped UDP datagram on the LAN, without masking a controller that never
// answers at all.
const (
	probeAttempts  = 3
	probeReplyWait = 2 * time.Second

	// probeWriteDeadline bounds a single send on the probe socket.
	probeWriteDeadline = 2 * time.Second

	// probeDrainWait is how long drain waits for one more stray datagram
	// before deciding the socket is quiet.
	probeDrainWait = 50 * time.Millisecond

	// probeSignalReplyWait is how long sendAbortSignal listens for replies
	// to the post-transfer signal, which docs/protocol.md §5.5 says draws
	// none.
	probeSignalReplyWait = 500 * time.Millisecond
)

// probeHarness owns a UDP socket dedicated to the probe subtests: unlike
// masso.Client, it lets a probe control individual packets — resend a
// start, send chunks out of order, send the post-transfer signal
// mid-transfer, or inspect a reply's raw bytes — none of which
// masso.Client.Upload exposes.
type probeHarness struct {
	conn   *net.UDPConn
	remote *net.UDPAddr
}

// newProbeHarness binds the harness's socket the way masso.NewClient binds
// a Client's: the first free UDP port in masso.ListenPortMin..ListenPortMax
// on 0.0.0.0 (udp4), so a bind failure here has the same likely cause as it
// would for the real client — Masso Link or mink-lasso already holding
// every port in that range. It then retargets the controller's replies to
// it with a unicast discovery request (§1, §7) before returning, so every
// later exchange on remote's controller reaches this socket instead of
// whatever last connected to it.
func newProbeHarness(t *testing.T, remote *net.UDPAddr) *probeHarness {
	t.Helper()

	var conn *net.UDPConn
	var lastErr error
	for port := masso.ListenPortMin; port <= masso.ListenPortMax; port++ {
		c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: port})
		if err != nil {
			lastErr = err
			continue
		}
		conn = c
		break
	}
	if conn == nil {
		t.Fatalf("probe harness: no free UDP port in %d-%d: %v "+
			"(is Masso Link or mink-lasso still running?)", masso.ListenPortMin, masso.ListenPortMax, lastErr)
	}
	t.Cleanup(func() { _ = conn.Close() })

	h := &probeHarness{conn: conn, remote: remote}
	h.retarget(t)

	return h
}

// localPort returns the port the harness's socket is bound to.
func (h *probeHarness) localPort(t *testing.T) uint16 {
	t.Helper()

	addr, ok := h.conn.LocalAddr().(*net.UDPAddr)
	if !ok {
		t.Fatalf("probe harness: local address %v is not a *net.UDPAddr", h.conn.LocalAddr())
	}

	return uint16(addr.Port & 0xFFFF) // net always binds within the uint16 port range
}

// retarget sends a unicast discovery request naming the harness's own port
// as the reply target, which the controller must honor for every later
// reply (docs/protocol.md §1), and fails the test unless an identity reply
// confirms it.
func (h *probeHarness) retarget(t *testing.T) {
	t.Helper()

	raw := h.roundTrip(t, masso.Discovery(h.localPort(t)))

	reply, err := masso.DecodeReply(raw)
	if err != nil {
		t.Fatalf("probe harness: retarget: decoding identity reply: %v", err)
	}
	if _, ok := reply.(masso.Identity); !ok {
		t.Fatalf("probe harness: retarget: got %T, want masso.Identity", reply)
	}
}

// send writes pkt to h.remote, failing the test on any error.
func (h *probeHarness) send(t *testing.T, pkt []byte) {
	t.Helper()

	if err := h.conn.SetWriteDeadline(time.Now().Add(probeWriteDeadline)); err != nil {
		t.Fatalf("probe harness: SetWriteDeadline: %v", err)
	}
	if _, err := h.conn.WriteToUDP(pkt, h.remote); err != nil {
		t.Fatalf("probe harness: send to %s: %v", h.remote, err)
	}
}

// recv waits up to probeReplyWait for the next datagram whose source IP
// matches h.remote — the same source-IP filter docs/protocol.md §1
// describes for every reply type — and returns its raw bytes, or nil,
// false on timeout. A datagram from any other source is logged and
// skipped rather than treated as a reply.
func (h *probeHarness) recv(t *testing.T) ([]byte, bool) {
	t.Helper()

	return h.recvWithin(t, probeReplyWait)
}

// recvWithin behaves like recv but waits up to wait instead of
// probeReplyWait.
func (h *probeHarness) recvWithin(t *testing.T, wait time.Duration) ([]byte, bool) {
	t.Helper()

	deadline := time.Now().Add(wait)
	buf := make([]byte, masso.MaxPacket)

	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil, false
		}

		if err := h.conn.SetReadDeadline(time.Now().Add(remaining)); err != nil {
			t.Fatalf("probe harness: SetReadDeadline: %v", err)
		}

		n, addr, err := h.conn.ReadFromUDP(buf)
		if err != nil {
			if errors.Is(err, os.ErrDeadlineExceeded) {
				return nil, false
			}

			t.Fatalf("probe harness: reading: %v", err)
		}

		if !addr.IP.Equal(h.remote.IP) {
			t.Logf("probe harness: dropping a datagram from unexpected source %s (want %s)", addr, h.remote.IP)
			continue
		}

		pkt := make([]byte, n)
		copy(pkt, buf[:n])

		return pkt, true
	}
}

// tryRoundTrip sends pkt up to probeAttempts times, waiting up to
// probeReplyWait each time for the next reply, and returns the first
// reply's raw bytes and the number of retries sent, or ok == false if none
// arrives; roundTrip builds the harness's Fatal-on-no-reply behavior on top
// of this.
func (h *probeHarness) tryRoundTrip(t *testing.T, pkt []byte) (raw []byte, retries int, ok bool) {
	t.Helper()

	h.drain(t)

	for retries := range probeAttempts {
		h.send(t, pkt)
		if raw, ok := h.recv(t); ok {
			return raw, retries, true
		}
	}

	return nil, probeAttempts - 1, false
}

// roundTrip behaves like tryRoundTrip but fails the test if no reply
// arrives, for an exchange where silence can only mean a harness or network
// fault; an exchange whose reply docs/protocol.md leaves unverified uses
// tryRoundTrip or observeChunk instead, and one a probe deliberately sends
// only once (a resend) uses send and recv, since a retry would change the
// sequence being probed.
func (h *probeHarness) roundTrip(t *testing.T, pkt []byte) []byte {
	t.Helper()

	if raw, _, ok := h.tryRoundTrip(t, pkt); ok {
		return raw
	}

	t.Fatalf("probe harness: no reply from %s after %d attempt(s)", h.remote, probeAttempts)

	return nil
}

// drain reads and logs every datagram already waiting on the socket, or
// arriving within probeDrainWait of the last one, so that a late or
// unsolicited reply is recorded as such instead of being taken for the
// reply to the next request. Left unread, one stray reply shifts every later
// exchange by one and makes each probe after it misread its results.
func (h *probeHarness) drain(t *testing.T) {
	t.Helper()

	for {
		raw, ok := h.recvWithin(t, probeDrainWait)
		if !ok {
			return
		}

		logPacket(t, "PROBE", "unsolicited reply, not matched to any request", raw)
	}
}

// sendAbortSignal sends the post-transfer notification three times, 20ms
// apart, matching v2.15's own cadence (docs/protocol.md §5.5), then logs
// every reply that arrives within probeSignalReplyWait. §5.5 says the
// signal draws none, so any reply is an observation; collecting them here
// also keeps them from being read as the reply to the next request.
func (h *probeHarness) sendAbortSignal(t *testing.T) {
	t.Helper()

	const (
		times   = 3
		spacing = 20 * time.Millisecond
	)

	h.drain(t)

	for i := range times {
		if i > 0 {
			time.Sleep(spacing)
		}

		h.send(t, masso.UploadAbort())
	}

	replies := 0
	for {
		raw, ok := h.recvWithin(t, probeSignalReplyWait)
		if !ok {
			break
		}

		replies++
		logPacket(t, "PROBE", fmt.Sprintf("reply %d to the post-transfer signal (sent %d times)", replies, times), raw)
	}

	if replies == 0 {
		t.Logf("PROBE: no reply to the post-transfer signal within %s", probeSignalReplyWait)
	}
}

// requireStatusReply sends one status request and fails the test unless a
// status reply comes back, returning it. TestProbe calls it between probes:
// a controller left mid-transfer answers with something else, and every
// probe after that point would misread its results, so stopping is the only
// safe course.
func (h *probeHarness) requireStatusReply(t *testing.T, after string) masso.Status {
	t.Helper()

	raw := h.roundTrip(t, masso.Keepalive(time.Now()))
	logPacket(t, "PROBE", "status after "+after, raw)

	reply, err := masso.DecodeReply(raw)
	if err != nil {
		t.Fatalf("PROBE: decoding the status reply after %s: %v", after, err)
	}

	st, ok := reply.(masso.Status)
	if !ok {
		t.Fatalf("PROBE: after %s, a status request drew %s instead of a status reply; "+
			"the controller may be stuck mid-transfer. Check its screen for a transfer "+
			"in progress, cancel it, and rerun; stopping so later probes do not misread their replies",
			after, describeReply(reply))
	}

	return st
}

// probeTransfer tracks one upload a probe has started, so that it can be
// finished rather than left open. The post-transfer signal does not end a
// transfer on the controller (docs/protocol.md §5.5), and neither does
// anything else a client can send: an unfinished one stays open, showing
// "Receiving" on the controller's screen, until canceled there by hand. So
// a probe closes every transfer it opens by sending the chunks the
// controller still lacks, in order (finish), and a t.Cleanup registered with
// the transfer does the same if the probe fails or is skipped first. It is
// created only by probeHarness.startTransfer or startTransferOptional.
type probeTransfer struct {
	h     *probeHarness
	data  []byte
	total uint32
	// accepted is the controller's accepted-chunk count from its last
	// successful chunk ACK.
	accepted uint32
	// open is true while the controller has this transfer open: from a
	// StartOK or post-retry StartAlreadyStarted reply until every chunk is
	// accepted or a chunk ACK reports an error.
	open bool
}

// registerTransfer creates a probeTransfer for data and registers its
// safety-net cleanup, which finishes the transfer if it is still open when
// the test ends.
func (h *probeHarness) registerTransfer(t *testing.T, data []byte) *probeTransfer {
	t.Helper()

	tr := &probeTransfer{h: h, data: data, total: chunkCount(len(data))}
	t.Cleanup(func() {
		if tr.open {
			tr.finish(t, "PROBE cleanup")
		}
	})

	return tr
}

// startTransfer sends pkt, an upload-start request for data, and returns a
// probeTransfer covering it, already registered per registerTransfer, along
// with the raw reply. It fails the test if no reply arrives at all, since
// every caller needs the reply's content to continue. The transfer counts
// as open only if the reply is a start ACK carrying StartOK, or
// StartAlreadyStarted after a retry (docs/protocol.md §5.1).
func (h *probeHarness) startTransfer(t *testing.T, pkt, data []byte) (*probeTransfer, []byte) {
	t.Helper()

	tr, raw, ok := h.startTransferOptional(t, pkt, data)
	if !ok {
		t.Fatalf("probe harness: no reply from %s after %d attempt(s)", h.remote, probeAttempts)
	}

	return tr, raw
}

// startTransferOptional behaves like startTransfer, but a start request
// that draws no reply at all is returned as ok == false instead of failing
// the test, for a start docs/protocol.md gives no assurance the controller
// answers, where that silence is itself an observation.
func (h *probeHarness) startTransferOptional(t *testing.T, pkt, data []byte) (tr *probeTransfer, raw []byte, ok bool) {
	t.Helper()

	tr = h.registerTransfer(t, data)
	raw, retries, ok := h.tryRoundTrip(t, pkt)
	tr.open = ok && startAccepted(raw, retries)

	return tr, raw, ok
}

// startAccepted recognizes StartOK, and StartAlreadyStarted only after a
// retry; a first-attempt StartAlreadyStarted is a refusal (§5.1).
func startAccepted(raw []byte, retries int) bool {
	reply, err := masso.DecodeReply(raw)
	if err != nil {
		return false
	}

	ack, ok := reply.(masso.StartAck)

	return ok && (ack.Result == masso.StartOK || (ack.Result == masso.StartAlreadyStarted && retries > 0))
}

// chunk sends chunk index of tr's data and returns the controller's chunk
// ACK, as observeChunk does, and folds it into tr: a successful ACK updates
// the accepted count, closing the transfer once every chunk is in, and an
// error ACK closes it.
func (tr *probeTransfer) chunk(t *testing.T, tag, label string, index uint32) (masso.ChunkAck, bool) {
	t.Helper()

	ack, ok := tr.h.observeChunk(t, tag, label, tr.data, index)
	if !ok {
		return ack, false
	}

	switch {
	case ack.Result != masso.ChunkOK:
		tr.open = false
	case ack.Accepted <= tr.total:
		tr.accepted = max(tr.accepted, ack.Accepted)
		if tr.accepted == tr.total {
			tr.open = false
		}
	default:
		// An accepted count above the file's chunk count is not a count
		// at all (a canceled ACK's USER bytes, say); leave tr as it was.
	}

	return ack, true
}

// finish sends, in order, every chunk the controller has not yet accepted,
// and reports whether the transfer completed. It stops early, logging that
// the transfer may still be open on the controller, if a chunk draws no
// usable reply or does not advance the accepted count; a chunk ACK with an
// error result ends it too, since that closes the transfer. It does nothing
// for a transfer that is not open.
func (tr *probeTransfer) finish(t *testing.T, tag string) bool {
	t.Helper()

	for tr.open {
		idx := tr.accepted
		ack, ok := tr.chunk(t, tag, fmt.Sprintf("chunk %d ACK, finishing the transfer", idx), idx)
		if !ok || (ack.Result == masso.ChunkOK && tr.open && tr.accepted == idx) {
			t.Logf("%s: could not finish the transfer (%d/%d chunks accepted); it may still be open "+
				"on the controller: check its screen and cancel it before rerunning", tag, tr.accepted, tr.total)
			tr.open = false // handed to the operator; the cleanup would only fail the same way again

			return false
		}
	}

	if tr.accepted < tr.total {
		t.Logf("%s: transfer ended incomplete (%d/%d chunks accepted)", tag, tr.accepted, tr.total)

		return false
	}

	t.Logf("%s: transfer complete (%d/%d chunks accepted)", tag, tr.accepted, tr.total)

	return true
}

// abort sends the post-transfer signal for tr (docs/protocol.md §5.5). It
// does not close the transfer, so tr stays open and finish still applies.
func (tr *probeTransfer) abort(t *testing.T) {
	t.Helper()

	tr.h.sendAbortSignal(t)
}

// logPacket logs a reply both decoded and as hex, prefixed with tag and
// label. A datagram that fails to decode is still logged in hex, since a
// probe testing an edge case may legitimately draw one.
func logPacket(t *testing.T, tag, label string, raw []byte) {
	t.Helper()

	reply, err := masso.DecodeReply(raw)
	if err != nil {
		t.Logf("%s: %s: %d bytes, decode error: %v, hex=%s", tag, label, len(raw), err, hex.EncodeToString(raw))
		return
	}

	t.Logf("%s: %s: %s hex=%s", tag, label, describeReply(reply), hex.EncodeToString(raw))
}

// describeReply formats a decoded reply's fields relevant to the probes:
// the ACK result bytes and accepted counts, or the identifying fields of a
// discovery/config/status reply.
func describeReply(r masso.Reply) string {
	switch v := r.(type) {
	case masso.StartAck:
		return fmt.Sprintf("StartAck{Result=0x%02X}", v.Result)
	case masso.ChunkAck:
		return fmt.Sprintf("ChunkAck{Result=0x%02X, Accepted=%d}", v.Result, v.Accepted)
	case masso.Identity:
		return fmt.Sprintf("Identity{Serial=%d, Version=%q}", v.Serial, v.Version)
	case masso.ConfigReply:
		return fmt.Sprintf("ConfigReply{Serial=%d}", v.Serial)
	case masso.Status:
		return fmt.Sprintf(
			"Status{Progress=%d, Running=%v, WaitingForOperator=%v, Jobs=%d, Line=%d, File=%q}",
			v.Progress, v.Running, v.WaitingForOperator, v.Jobs, v.Line, v.File,
		)
	case masso.ToolRecord:
		return fmt.Sprintf("ToolRecord{Index=%d, Name=%q}", v.Index, v.Name)
	default:
		return fmt.Sprintf("%T{%+v}", r, r)
	}
}

// observeChunk sends chunk index of data and returns the controller's chunk
// ACK, or ok == false when none came or the reply did not decode as one,
// either of which it logs: in a probe of behavior docs/protocol.md leaves
// unverified, silence or an odd-shaped reply is an observation, not a
// harness error.
func (h *probeHarness) observeChunk(t *testing.T, tag, label string, data []byte, index uint32) (masso.ChunkAck, bool) {
	t.Helper()

	raw, _, ok := h.tryRoundTrip(t, chunkPkt(t, data, index))
	if !ok {
		t.Logf("%s: %s: no reply after %d attempt(s)", tag, label, probeAttempts)

		return masso.ChunkAck{}, false
	}

	logPacket(t, tag, label, raw)

	ack, ok := decodeChunkAckOptional(raw)
	if !ok {
		t.Logf("%s: %s: the reply did not decode as a chunk ACK", tag, label)
	}

	return ack, ok
}

// decodeChunkAckOptional decodes raw as a masso.ChunkAck, reporting ok ==
// false instead of failing the test when it decodes to anything else.
func decodeChunkAckOptional(raw []byte) (masso.ChunkAck, bool) {
	reply, err := masso.DecodeReply(raw)
	if err != nil {
		return masso.ChunkAck{}, false
	}

	ack, ok := reply.(masso.ChunkAck)

	return ack, ok
}

// chunkCount returns the number of MaxChunkData-sized chunks a size-byte
// file splits into (docs/protocol.md §5.2's chunking, matching
// internal/masso's own ceiling division).
func chunkCount(size int) uint32 {
	return uint32((size + masso.MaxChunkData - 1) / masso.MaxChunkData)
}

// chunkAt returns the index-th MaxChunkData-sized slice of data (the last
// necessarily shorter), matching docs/protocol.md §5.2.
func chunkAt(data []byte, index uint32) []byte {
	off := int(index) * masso.MaxChunkData
	end := min(off+masso.MaxChunkData, len(data))

	return data[off:end]
}

// chunkPkt builds the encoded chunk request for the index-th slice of data.
func chunkPkt(t *testing.T, data []byte, index uint32) []byte {
	t.Helper()

	pkt, err := masso.UploadChunk(index, chunkAt(data, index))
	if err != nil {
		t.Fatalf("building chunk %d: %v", index, err)
	}

	return pkt
}

// probeMagic is docs/protocol.md §2's constant magic bytes.
// internal/masso's frame and CRC helpers are unexported, and probeQ12 needs
// to build a start request that masso.UploadStart itself refuses (a name
// over masso.MaxFileName), so buildUploadStartRequest and probeFrame
// reimplement §2's framing and CRC directly from the spec.
var probeMagic = [2]byte{0x03, 0x00}

// probeCRC16XModem computes CRC-16/XMODEM (polynomial 0x1021, initial value
// 0x0000, no input or output reflection) over data, matching the reference
// implementation in docs/protocol.md §2.
func probeCRC16XModem(data []byte) uint16 {
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

// probeRoundUp4 rounds n up to the next multiple of 4, docs/protocol.md
// §2's body-padding rule.
func probeRoundUp4(n int) int {
	return (n + 3) &^ 3
}

// probeFrame assembles a complete datagram exactly as docs/protocol.md §2
// describes: CRC (little-endian) | 03 00 | type | payload, with the body
// (everything after the CRC) zero-padded to a 4-byte multiple before the
// CRC is computed over it.
func probeFrame(typ byte, payload []byte) []byte {
	body := make([]byte, probeRoundUp4(3+len(payload)))
	body[0], body[1] = probeMagic[0], probeMagic[1]
	body[2] = typ
	copy(body[3:], payload)

	pkt := make([]byte, 2+len(body))
	binary.LittleEndian.PutUint16(pkt[0:2], probeCRC16XModem(body))
	copy(pkt[2:], body)

	return pkt
}

// buildUploadStartRequest hand-builds an upload-start request for a
// size-byte file named name in directory dir, following docs/protocol.md
// §5.1's layout exactly: size(4) | reserved(2) | pathlen(1) | path | 0x00 |
// name | 0x00 | 3 reserved zero bytes. masso.UploadStart encodes the same
// layout but refuses any name over masso.MaxFileName
// (masso.ErrBadFileName), which is exactly what probeQ12 needs to send;
// name is therefore not validated here at all. dir is still checked with
// masso.ValidateUploadDir, since probeQ12 never has a reason to send a bad
// one.
func buildUploadStartRequest(size uint32, dir, name string) ([]byte, error) {
	if err := masso.ValidateUploadDir(dir); err != nil {
		return nil, err
	}

	const uploadStartReserved = 3 // docs/protocol.md §5.1

	path := dir
	if path == "" {
		path = `\`
	}

	payload := make([]byte, 0, 4+2+1+len(path)+1+len(name)+1+uploadStartReserved)
	payload = binary.LittleEndian.AppendUint32(payload, size)
	payload = append(payload, 0, 0) // reserved
	payload = append(payload, byte(len(path)&0xFF))
	payload = append(payload, path...)
	payload = append(payload, 0)
	payload = append(payload, name...)
	payload = append(payload, 0)
	payload = append(payload, make([]byte, uploadStartReserved)...)

	return probeFrame(masso.TypeUploadStart, payload), nil
}

// nChunkFileData returns comment-only, harmless-to-load content sized to
// span exactly n upload chunks (the last necessarily shorter than
// masso.MaxChunkData), tagged with label so a capture is easy to attribute
// to the probe that sent it.
func nChunkFileData(label string, n int) []byte {
	data := fmt.Appendf(nil, "(mink-lasso probe: %s)\n", label)

	target := masso.MaxChunkData*(n-1) + 1
	for len(data) < target {
		data = fmt.Appendf(data, "(padding line, currently %d of %d target bytes)\n", len(data), target)
	}

	return data
}
