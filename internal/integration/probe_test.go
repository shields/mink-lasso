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
	"fmt"
	"strings"
	"testing"
	"time"

	"msrl.dev/mink-lasso/internal/masso"
)

// TestProbe runs the opt-in controller probes for docs/protocol-questions.md
// (Q2-Q6, Q9, Q14, and the opt-in Q12, plus raw captures for Q7 and Q8): they log
// what a real controller does in situations only real hardware can answer,
// for an operator to paste back into that document. They assert nothing
// about unknown controller behavior and fail only on a harness error or an
// unsafe condition (see README.md's "Controller probes" section); a
// controller talks to one client at a time, so nothing here may run with
// t.Parallel().
//
//nolint:paralleltest // sequential by design; see the doc comment above.
func TestProbe(t *testing.T) {
	serial := requireSerial(t)
	requireProbeEnabled(t)
	addr := resolveController(t, serial)
	requireIdleOrAllowed(t, addr)

	h := newProbeHarness(t, addr)

	// q12 carries probeQ12's observations to probeQ9 explicitly (see
	// probeQ12Result), rather than probeQ9 having to infer anything from
	// subtest order or a shared naming convention.
	var q12 probeQ12Result

	// Each probe is followed by a status request that must draw a status
	// reply (requireStatusReply), so a controller left mid-transfer stops the
	// run before later probes misread their replies.
	//nolint:paralleltest // sequential by design; see TestProbe's doc comment.
	t.Run("Q7Q8_RawCaptures", func(t *testing.T) { probeQ7Q8(t, h, serial) })
	h.requireStatusReply(t, "Q7Q8_RawCaptures")
	//nolint:paralleltest // sequential by design; see TestProbe's doc comment.
	t.Run("Q2_ResentStart", func(t *testing.T) { probeQ2(t, h) })
	h.requireStatusReply(t, "Q2_ResentStart")
	//nolint:paralleltest // sequential by design; see TestProbe's doc comment.
	t.Run("Q3_StartWhileOpen", func(t *testing.T) { probeQ3(t, h) })
	h.requireStatusReply(t, "Q3_StartWhileOpen")
	//nolint:paralleltest // sequential by design; see TestProbe's doc comment.
	t.Run("Q4_AfterTheSignal", func(t *testing.T) { probeQ4(t, h) })
	h.requireStatusReply(t, "Q4_AfterTheSignal")
	//nolint:paralleltest // sequential by design; see TestProbe's doc comment.
	t.Run("Q14_SignalFreesOpenTransfer", func(t *testing.T) { probeQ14(t, h) })
	h.requireStatusReply(t, "Q14_SignalFreesOpenTransfer")
	//nolint:paralleltest // sequential by design; see TestProbe's doc comment.
	t.Run("Q13_SignalWhenIdle", func(t *testing.T) { probeQ13(t, h) })
	h.requireStatusReply(t, "Q13_SignalWhenIdle")
	//nolint:paralleltest // sequential by design; see TestProbe's doc comment.
	t.Run("Q5_MissingDirectory", func(t *testing.T) { probeQ5(t, h) })
	h.requireStatusReply(t, "Q5_MissingDirectory")
	//nolint:paralleltest // sequential by design; see TestProbe's doc comment.
	t.Run("Q6_DirectoryNames", func(t *testing.T) { probeQ6(t, h) })
	h.requireStatusReply(t, "Q6_DirectoryNames")
	//nolint:paralleltest // sequential by design; see TestProbe's doc comment.
	t.Run("Q12_LongFileNames", func(t *testing.T) { q12 = probeQ12(t, h) })
	h.requireStatusReply(t, "Q12_LongFileNames")
	//nolint:paralleltest // sequential by design; see TestProbe's doc comment.
	t.Run("Q9_ThirtyThreeCharacterName", func(t *testing.T) { probeQ9(t, h, q12) })

	probeFinalStatus(t, h)
}

// probeQ7Q8 addresses docs/protocol-questions.md Q7 (which 16 bits of the
// config reply carry the serial, bytes 5-6 per docs/protocol.md §3.2) and Q8
// (the meaning of identity bytes 9-12, §3.1) by logging both replies raw,
// alongside the configured serial for comparison.
func probeQ7Q8(t *testing.T, h *probeHarness, serial uint32) {
	t.Helper()

	idRaw := h.roundTrip(t, masso.Discovery(h.localPort(t)))
	logPacket(t, "PROBE Q8", "identity reply", idRaw)

	const identityBytes9to13 = 13
	if len(idRaw) >= identityBytes9to13 {
		t.Logf("PROBE Q8: identity bytes 9-12 = % x (configured serial %s / %d)",
			idRaw[9:identityBytes9to13], masso.SerialString(serial), serial)
	}

	cfgRaw := h.roundTrip(t, masso.Config(time.Now()))
	logPacket(t, "PROBE Q7", "config reply", cfgRaw)

	const configBytes5to7 = 7
	if len(cfgRaw) >= configBytes5to7 {
		t.Logf("PROBE Q7: config reply bytes 5-6 = % x (low 16 bits of configured serial %d = 0x%04X)",
			cfgRaw[5:configBytes5to7], serial, uint16(serial&0xFFFF))
	}
}

// probeQ2 addresses docs/protocol-questions.md Q2 (probeQ2ContinueFromChunk0)
// and, beyond Q2 itself, also observes out-of-order chunk delivery on the
// same kind of resent-start transfer (probeQ2ChunkOutOfOrder).
func probeQ2(t *testing.T, h *probeHarness) {
	t.Helper()

	probeQ2ContinueFromChunk0(t, h)
	probeQ2ChunkOutOfOrder(t, h)
}

// probeQ2ContinueFromChunk0 sends a start, its resend, and then continues
// normally from chunk 0.
func probeQ2ContinueFromChunk0(t *testing.T, h *probeHarness) {
	t.Helper()

	const name = "MLTESTQ2A.NC"

	data := nChunkFileData("q2a", 1)

	startPkt, err := masso.UploadStart(uint32(len(data)&0xFFFFFFFF), "", name)
	if err != nil {
		t.Fatalf("PROBE Q2: building start request: %v", err)
	}

	tr, raw := h.startTransfer(t, startPkt, data)
	logPacket(t, "PROBE Q2", "first start ACK ("+name+")", raw)

	if !tr.open {
		t.Skip("PROBE Q2: first start not accepted; cannot test a resend")
	}

	h.drain(t)
	h.send(t, startPkt) // the deliberate resend under test: sent exactly once, not via roundTrip's retries

	// Silence here is itself an answer to Q2 (does the controller reply to a
	// resent start at all), not a harness failure, so this logs rather than
	// fails the test.
	if resendRaw, ok := h.recv(t); ok {
		logPacket(t, "PROBE Q2", "resent start ACK ("+name+")", resendRaw)
	} else {
		t.Logf("PROBE Q2: no reply to the resent start within %s", probeReplyWait)
	}

	tr.chunk(t, "PROBE Q2", "chunk 0 ACK after the resend ("+name+")", 0)
	tr.finish(t, "PROBE Q2 "+name)
}

// probeQ2ChunkOutOfOrder is not itself tied to a numbered question in
// docs/protocol-questions.md. It reuses probeQ2ContinueFromChunk0's
// start-then-resend setup to observe what the controller does when chunk 1
// arrives before chunk 0, then finishes the transfer in order.
func probeQ2ChunkOutOfOrder(t *testing.T, h *probeHarness) {
	t.Helper()

	const name = "MLTESTQ2B.NC"

	data := nChunkFileData("q2b", 2)

	startPkt, err := masso.UploadStart(uint32(len(data)&0xFFFFFFFF), "", name)
	if err != nil {
		t.Fatalf("PROBE Q2: building start request: %v", err)
	}

	tr, raw := h.startTransfer(t, startPkt, data)
	logPacket(t, "PROBE Q2", "first start ACK ("+name+")", raw)

	if !tr.open {
		t.Skip("PROBE Q2: first start not accepted; cannot test the out-of-order case")
	}

	h.drain(t)
	h.send(t, startPkt) // the deliberate resend under test

	// As in probeQ2ContinueFromChunk0, no reply here is a legitimate
	// observation, not a harness failure.
	if resendRaw, ok := h.recv(t); ok {
		logPacket(t, "PROBE Q2", "resent start ACK ("+name+")", resendRaw)
	} else {
		t.Logf("PROBE Q2: no reply to the resent start within %s", probeReplyWait)
	}

	if ack1, ok := tr.chunk(t, "PROBE Q2", "chunk 1 ACK, sent before chunk 0 ("+name+")", 1); ok {
		t.Logf("PROBE Q2: after chunk 1: accepted=%d/%d", ack1.Accepted, tr.total)
	}

	if ack0, ok := tr.chunk(t, "PROBE Q2", "chunk 0 ACK, sent after chunk 1 ("+name+")", 0); ok {
		t.Logf("PROBE Q2: after chunk 0: accepted=%d/%d", ack0.Accepted, tr.total)
	}

	tr.finish(t, "PROBE Q2 "+name)
}

// probeQ3 addresses docs/protocol-questions.md Q3: whether the controller
// begins an upload, and stores chunks sent afterward, when it answers a
// start request with an error result.
//
// The only known way to draw a start error is to remove the USB drive
// (0xE9), which this probe cannot do for itself, so it tells the operator
// how. Run that way, the first start is refused, and the probe sends that
// file's chunks and logs their ACKs. With the drive in place, it instead
// sends a second start, for another file, while the first transfer is
// still open; docs/protocol.md §5.1 records that the controller accepts
// that and displaces the first transfer, so question 3 goes unexercised
// unless the controller behaves differently. The probe then finishes the
// second transfer and tries the first, which the controller has ordinarily
// left stuck (§5.5).
func probeQ3(t *testing.T, h *probeHarness) {
	t.Helper()

	t.Logf("PROBE Q3: to observe a refused start (0xE9, no USB), " +
		"remove the USB drive from the controller and rerun; " +
		"this run proceeds regardless and logs whatever result the controller reports")

	// A start sent while another transfer is open leaves the first one
	// stuck on the controller until it restarts, drawing 0xF7 for its name
	// (docs/protocol.md §5.5), so each run uses fresh names: 12 bits of the
	// clock keep them within masso.MaxFileName.
	suffix := fmt.Sprintf("%03X", time.Now().UnixMilli()&0xFFF)
	nameA, nameB := "MLTESTQ3A"+suffix+".NC", "MLTESTQ3B"+suffix+".NC"

	dataA := nChunkFileData("q3a", 1)

	startA, err := masso.UploadStart(uint32(len(dataA)&0xFFFFFFFF), "", nameA)
	if err != nil {
		t.Fatalf("PROBE Q3: building first start request: %v", err)
	}

	trA, rawA := h.startTransfer(t, startA, dataA)
	logPacket(t, "PROBE Q3", "first start ACK ("+nameA+")", rawA)

	if !trA.open {
		probeQ3Refused(t, trA, nameA)

		return
	}

	dataB := nChunkFileData("q3b", 2)

	startB, err := masso.UploadStart(uint32(len(dataB)&0xFFFFFFFF), "", nameB)
	if err != nil {
		t.Fatalf("PROBE Q3: building second start request: %v", err)
	}

	// docs/protocol.md gives no assurance the controller replies to a start
	// sent while another transfer is open, so silence here is an
	// observation, not a harness failure.
	trB, rawB, ok := h.startTransferOptional(t, startB, dataB)
	if !ok {
		t.Logf("PROBE Q3: no reply to a start sent while another transfer was open within %s; "+
			"question 3 was not exercised this run", probeReplyWait)
		trA.finish(t, "PROBE Q3 "+nameA)

		return
	}

	logPacket(t, "PROBE Q3", "second start ACK, a start sent while another transfer is open ("+nameB+")", rawB)

	if trB.open {
		t.Log("PROBE Q3: a start sent while another transfer was open was accepted; question 3 was not exercised this run")
		trB.finish(t, "PROBE Q3 "+nameB)
	} else {
		probeQ3Refused(t, trB, nameB)
	}

	// The first transfer's chunk ACK shows whether the controller still
	// had it open after the second start.
	trA.finish(t, "PROBE Q3 "+nameA)
}

// probeQ3Refused sends every chunk of tr, whose start the controller did
// not accept, and logs each ACK: whether the controller stores chunks
// after refusing their start is question 3 itself, so a missing or
// odd-shaped reply is logged, not a failure.
func probeQ3Refused(t *testing.T, tr *probeTransfer, name string) {
	t.Helper()

	t.Logf("PROBE Q3: the start for %s was not accepted; "+
		"sending its chunks to see whether the controller stores them anyway", name)

	for idx := range tr.total {
		label := fmt.Sprintf("chunk %d ACK after the refused start (%s)", idx, name)
		if ack, ok := tr.chunk(t, "PROBE Q3", label, idx); ok {
			t.Logf("PROBE Q3: after chunk %d: result=0x%02X accepted=%d/%d", idx, ack.Result, ack.Accepted, tr.total)
		}
	}

	t.Logf("PROBE Q3: check the drive for %s once it is back in the controller", name)
}

// probeQ4 addresses docs/protocol-questions.md Q4: whether the
// post-transfer signal ever leaves a partial or 0-byte file behind. It first
// starts MLTESTQ4N.NC and sends the signal before any chunk, then starts
// MLTESTQ4.NC, sends two chunks, the signal, and tries to finish it,
// logging each ACK. What each leaves on the drive answers the question.
func probeQ4(t *testing.T, h *probeHarness) {
	t.Helper()

	probeQ4NoChunks(t, h)

	data := nChunkFileData("q4", 4)

	startPkt, err := masso.UploadStart(uint32(len(data)&0xFFFFFFFF), "", "MLTESTQ4.NC")
	if err != nil {
		t.Fatalf("PROBE Q4: building start request: %v", err)
	}

	tr, raw := h.startTransfer(t, startPkt, data)
	logPacket(t, "PROBE Q4", "start ACK", raw)

	if !tr.open {
		t.Skip("PROBE Q4: start not accepted; cannot test chunks after the signal")
	}

	for _, idx := range []uint32{0, 1} {
		if ack, ok := tr.chunk(t, "PROBE Q4", fmt.Sprintf("chunk %d ACK", idx), idx); ok {
			t.Logf("PROBE Q4: after chunk %d: accepted=%d/%d", idx, ack.Accepted, tr.total)
		}
	}

	t.Logf("PROBE Q4: sending the post-transfer signal mid-transfer (docs/protocol.md §5.5); accepted so far=%d/%d",
		tr.accepted, tr.total)
	tr.abort(t)

	// What the controller does with chunks sent after 0x0C is Q4 itself.
	tr.finish(t, "PROBE Q4 after the signal")
}

// probeQ14 addresses docs/protocol-questions.md Q14: whether the
// post-transfer signal frees a transfer the client left open without it,
// such as one a client killed mid-upload leaves behind, so that a new
// start for the same file draws 0x00 instead of 0xF7 (docs/protocol.md
// §5.1, §5.5). It opens a two-chunk transfer, sends chunk 0, sends the
// same start again as though from a fresh client, then the signal, then
// the start once more, and finishes whatever transfer that opens.
func probeQ14(t *testing.T, h *probeHarness) {
	t.Helper()

	// If the signal does not free the name, it stays stuck until the
	// controller restarts, so each run uses a fresh one, as probeQ3 does.
	name := fmt.Sprintf("MLTESTQ14%03X.NC", time.Now().UnixMilli()&0xFFF)
	data := nChunkFileData("q14", 2)

	startPkt, err := masso.UploadStart(uint32(len(data)&0xFFFFFFFF), "", name)
	if err != nil {
		t.Fatalf("PROBE Q14: building start request: %v", err)
	}

	tr1, raw := h.startTransfer(t, startPkt, data)
	logPacket(t, "PROBE Q14", "first start ACK ("+name+")", raw)

	if !tr1.open {
		t.Skip("PROBE Q14: first start not accepted; nothing is open to recover from")
	}

	tr1.chunk(t, "PROBE Q14", "chunk 0 ACK ("+name+")", 0)

	if !tr1.open {
		t.Skip("PROBE Q14: the transfer closed after chunk 0; nothing is open to recover from")
	}

	// The transfer is now open with data, as a client that died mid-upload
	// would leave it. A fresh client's first start for the same file:
	tr2, raw := h.startTransfer(t, startPkt, data)
	logPacket(t, "PROBE Q14", "start ACK for the same file while its transfer is open ("+name+")", raw)

	if tr2.open {
		t.Log("PROBE Q14: the controller accepted a new start for the open file; question 14 was not exercised this run")
		tr1.open = false // tr2 replaced it
		tr2.finish(t, "PROBE Q14 "+name)

		return
	}

	t.Log("PROBE Q14: sending the post-transfer signal (docs/protocol.md §5.5)")
	tr1.abort(t)
	tr1.open = false // the signal ends a transfer's data (docs/protocol.md §5.5)

	tr3, raw := h.startTransfer(t, startPkt, data)
	logPacket(t, "PROBE Q14", "start ACK after the signal ("+name+")", raw)

	if !tr3.open {
		t.Logf("PROBE Q14: the start after the signal was not accepted; the signal did not free %s, "+
			"which may now stay stuck until the controller restarts", name)

		return
	}

	if tr3.finish(t, "PROBE Q14 "+name) {
		t.Logf("PROBE Q14: the signal freed %s; check that the drive holds it complete (%d bytes)", name, len(data))
	}
}

// probeQ4NoChunks starts MLTESTQ4N.NC and sends the post-transfer signal
// before any chunk.
func probeQ4NoChunks(t *testing.T, h *probeHarness) {
	t.Helper()

	const name = "MLTESTQ4N.NC"

	data := nChunkFileData("q4n", 1)

	startPkt, err := masso.UploadStart(uint32(len(data)&0xFFFFFFFF), "", name)
	if err != nil {
		t.Fatalf("PROBE Q4: building start request for %s: %v", name, err)
	}

	tr, raw := h.startTransfer(t, startPkt, data)
	logPacket(t, "PROBE Q4", "start ACK ("+name+")", raw)

	if !tr.open {
		t.Logf("PROBE Q4: start for %s not accepted; skipping the no-chunk case", name)

		return
	}

	t.Logf("PROBE Q4: sending the post-transfer signal before any chunk of %s", name)
	tr.abort(t)
	tr.open = false // the signal ends a transfer's data (docs/protocol.md §5.5)
	t.Logf("PROBE Q4: check the drive for %s: absent, or present with 0 bytes", name)
}

// probeQ13 serves docs/protocol-questions.md Q13 indirectly: rather than
// free a displaced transfer, a client can keep one from arising by sending
// the post-transfer signal when it connects, canceling any transfer an
// earlier run left open before it starts another file. That is safe only if
// the signal is harmless with nothing open, which this checks: it completes
// a one-chunk upload, so nothing is open, then sends the signal and logs
// the replies. The operator watches the controller's screen for anything
// the signal puts there.
func probeQ13(t *testing.T, h *probeHarness) {
	t.Helper()

	const name = "MLTESTQ13.NC"

	data := nChunkFileData("q13", 1)

	startPkt, err := masso.UploadStart(uint32(len(data)&0xFFFFFFFF), "", name)
	if err != nil {
		t.Fatalf("PROBE Q13: building start request: %v", err)
	}

	tr, raw := h.startTransfer(t, startPkt, data)
	logPacket(t, "PROBE Q13", "start ACK ("+name+")", raw)

	if !tr.finish(t, "PROBE Q13 "+name) {
		t.Skip("PROBE Q13: the upload did not complete, so something may still be open; not sending the signal")
	}

	t.Log("PROBE Q13: nothing is open; sending the post-transfer signal now: watch the controller's screen")
	h.sendAbortSignal(t)
}

// probeQ5 addresses docs/protocol-questions.md Q5: whether the controller
// creates a directory named in the start packet's path field that does not
// yet exist, and which result the start ACK or first chunk ACK carries if
// not.
func probeQ5(t *testing.T, h *probeHarness) {
	t.Helper()

	dirName := fmt.Sprintf("MLTESTQ5-%d", time.Now().UnixNano())
	dir := `MLTEST\` + dirName

	data := nChunkFileData("q5", 1)

	startPkt, err := masso.UploadStart(uint32(len(data)&0xFFFFFFFF), dir, "MLTESTQ5.NC")
	if err != nil {
		t.Fatalf("PROBE Q5: building start request for %q: %v", dir, err)
	}

	// docs/protocol.md gives no assurance a start naming a missing directory
	// draws a reply at all, so silence here is an observation, not a harness
	// failure.
	tr, raw, ok := h.startTransferOptional(t, startPkt, data)
	if !ok {
		t.Logf("PROBE Q5: no reply to the start request for %q within %s; "+
			"check the controller's file browser for %s\\MLTESTQ5.NC", dir, probeReplyWait, dir)

		return
	}

	logPacket(t, "PROBE Q5", fmt.Sprintf("start ACK for new folder %q", dir), raw)

	// Unlike the sibling probes, chunk 0 goes out even if the start was not
	// accepted: Q5 asks whether a missing-directory result shows up in the
	// start ACK or the first chunk ACK.
	tr.chunk(t, "PROBE Q5", fmt.Sprintf("chunk 0 ACK for %q", dir), 0)
	tr.finish(t, "PROBE Q5 "+dir)
	t.Logf("PROBE Q5: check the controller's file browser for %s\\MLTESTQ5.NC", dir)
}

// probeQ6DirEntry is one row of probeQ6's directory-name table: a folder
// name component under MLTEST\ that passes masso.ValidateUploadDir but
// probes an edge of what a folder name may contain or how long one
// component may be (docs/protocol-questions.md Q6).
type probeQ6DirEntry struct {
	component, note string
}

// probeQ6 addresses docs/protocol-questions.md Q6: which bytes a directory
// name in the path field may contain, and whether one component has a
// length limit narrower than the 255-byte path field as a whole.
func probeQ6(t *testing.T, h *probeHarness) {
	t.Helper()

	table := []probeQ6DirEntry{
		{"MLTESTQ6 SPACE", "embedded space"},
		{"MLTESTQ6.DOT", "embedded period"},
		{"MLTESTQ6.", "trailing period"},
		{"MLTESTQ6 ", "trailing space"},
		{"MLTESTQ6-PUNCT!@#$%^&()_+-=[]{}',;~", "assorted printable punctuation"},
		{"MLTESTQ6-" + strings.Repeat("X", 200), "long single component (~209 bytes)"},
	}

	data := nChunkFileData("q6", 1)

	for i, e := range table {
		dir := `MLTEST\` + e.component
		t.Logf("PROBE Q6: [%d/%d] %s: dir=%q", i+1, len(table), e.note, dir)

		startPkt, err := masso.UploadStart(uint32(len(data)&0xFFFFFFFF), dir, "MLTESTQ6.NC")
		if err != nil {
			t.Fatalf("PROBE Q6: building start request for %q: %v", dir, err)
		}

		// docs/protocol.md gives no assurance a start naming an edge-case
		// directory component draws a reply at all, so silence here is an
		// observation, not a harness failure.
		tr, raw, ok := h.startTransferOptional(t, startPkt, data)
		if !ok {
			t.Logf("PROBE Q6: %q: no reply to the start request within %s; skipping this entry", dir, probeReplyWait)

			continue
		}

		logPacket(t, "PROBE Q6", fmt.Sprintf("start ACK for %q (%s)", dir, e.note), raw)

		if !tr.open {
			t.Logf("PROBE Q6: %q: start not accepted; skipping the chunk for this entry", dir)

			continue
		}

		tr.finish(t, fmt.Sprintf("PROBE Q6 %q", dir))
	}
}

// probeQ12FileName returns a comments-only upload name totalLen characters
// long, including its extension: the fixed prefix "MLTESTQ12-" padded with
// 'X' out to totalLen, ending in ".NC" (one of docs/protocol.md §5's
// allowed extensions).
func probeQ12FileName(t *testing.T, totalLen int) string {
	t.Helper()

	const prefix, ext = "MLTESTQ12-", ".NC"

	pad := totalLen - len(prefix) - len(ext)
	if pad < 0 {
		t.Fatalf("PROBE Q12: want totalLen %d, too short for prefix %q + extension %q", totalLen, prefix, ext)
	}

	name := prefix + strings.Repeat("X", pad) + ext
	if len(name) != totalLen {
		t.Fatalf("PROBE Q12: built name %q is %d characters, want %d", name, len(name), totalLen)
	}

	return name
}

// probeQ12Upload hand-builds and sends an upload-start request for a
// comments-only file named name (buildUploadStartRequest), then, if the
// controller accepts it, sends its one chunk and finishes the transfer —
// the same start/chunk/ACK sequence as every other probe here
// (docs/protocol.md §5.1-§5.3). Unlike those probes, the start itself may
// draw no reply at all: internal/masso's own simulator refuses to decode a
// name this long and drops the packet silently, and docs/protocol.md gives
// no assurance real firmware behaves any differently, so that is logged as
// an observation rather than failing the test — and, if a reply does
// arrive, it may not take the normal 10-byte start-ACK shape either, since
// no version of Masso Link ever sends a name this long for docs/protocol.md
// to describe a reply to; that too is logged as an observation, not a
// Fatal. It reports whether the start was accepted and, if so, whether the
// upload went on to complete.
func probeQ12Upload(t *testing.T, h *probeHarness, name string) (accepted, complete bool) {
	t.Helper()

	data := nChunkFileData("q12", 1)

	startPkt, err := buildUploadStartRequest(uint32(len(data)&0xFFFFFFFF), "", name)
	if err != nil {
		t.Fatalf("PROBE Q12: building start request for %q: %v", name, err)
	}

	tr, raw, ok := h.startTransferOptional(t, startPkt, data)
	if !ok {
		t.Logf("PROBE Q12: no reply to the start request for %q (%d characters) within %s", name, len(name), probeReplyWait)
		return false, false
	}

	logPacket(t, "PROBE Q12", fmt.Sprintf("start ACK for %q (%d characters)", name, len(name)), raw)

	// A reply that is not a StartOK start ACK, including one that does not
	// decode as a start ACK at all, is an observation here, not a harness
	// failure: no version of Masso Link ever asks for a name this long, so
	// docs/protocol.md gives no assurance the reply takes the usual shape.
	if !tr.open {
		t.Logf("PROBE Q12: %q: start not accepted", name)

		return false, false
	}

	return true, tr.finish(t, "PROBE Q12 "+name)
}

// probeQ12Result is what probeQ12 hands to probeQ9: an explicit record of
// its 33-character upload's outcome, rather than something probeQ9 would
// otherwise have to infer from subtest order or a shared file-naming
// convention.
type probeQ12Result struct {
	// ran is true only once MINK_LASSO_PROBE_LONG_NAMES=1 let probeQ12
	// attempt its uploads at all.
	ran bool
	// thirtyThreeName is the 33-character name probeQ12 uploaded.
	thirtyThreeName string
	// thirtyThreeAccepted and thirtyThreeComplete report the 33-character
	// upload's outcome: whether the controller's start ACK was
	// masso.StartOK and, if so, whether every chunk was then acknowledged.
	thirtyThreeAccepted, thirtyThreeComplete bool
}

// probeQ12 addresses docs/protocol-questions.md Q12, the longest file name
// the controller accepts: it has accepted 33 characters, and Masso Link
// itself checks nothing narrower than 255 (docs/protocol.md §5). It uploads
// two comments-only files, one whose name is one character over mink-lasso's
// own limit (masso.MaxFileName+1, 34), which masso.UploadStart refuses to
// build, and one exactly as long as the status reply's file-name field can
// hold (masso.MaxStatusFile, 33 — docs/protocol.md §4). probeQ12Upload
// hand-builds both. It returns its observations for probeQ9 to use
// explicitly.
func probeQ12(t *testing.T, h *probeHarness) probeQ12Result {
	t.Helper()

	requireLongNamesEnabled(t)

	probeQ12Upload(t, h, probeQ12FileName(t, masso.MaxFileName+1))

	name33 := probeQ12FileName(t, masso.MaxStatusFile)
	accepted, complete := probeQ12Upload(t, h, name33)

	return probeQ12Result{
		ran:                 true,
		thirtyThreeName:     name33,
		thirtyThreeAccepted: accepted,
		thirtyThreeComplete: complete,
	}
}

// probeQ9StatusPollTimeout bounds how long probeQ9 waits for the
// long-named file — probeQ12's own upload, or one the operator staged by
// hand — to appear on the controller.
const probeQ9StatusPollTimeout = 3 * time.Minute

// probeQ9 addresses docs/protocol-questions.md Q9: whether the status
// packet's current-file-name field NUL-terminates at exactly 33 characters,
// or the name runs into the reserved area beyond it (docs/protocol.md §4).
//
// masso.UploadStart refuses to build a request for a name this long — it
// exceeds masso.MaxFileName — so Q9 itself sends no upload; it only polls
// status for a 33-character name to appear. q12 says which name: when
// probeQ12's own 33-character upload (MINK_LASSO_PROBE_LONG_NAMES=1) was
// accepted and completed, Q9 asks the operator to load that file on the
// controller's own screen; otherwise it asks them to first stage one by
// hand (README.md's "Controller probes" section gives its exact name and
// content).
func probeQ9(t *testing.T, h *probeHarness, q12 probeQ12Result) {
	t.Helper()

	name := "MLTESTQ9" + strings.Repeat("9", 22) + ".NC" // 8 + 22 + 3 = 33
	if len(name) != masso.MaxStatusFile {
		t.Fatalf("PROBE Q9: expected file name is %d characters, want exactly %d", len(name), masso.MaxStatusFile)
	}

	if q12.ran && q12.thirtyThreeAccepted && q12.thirtyThreeComplete {
		name = q12.thirtyThreeName
		t.Logf("PROBE Q9: PROBE Q12 already uploaded and completed %q; load (do not run) it "+
			"on the controller's own screen now", name)
	} else {
		t.Logf("PROBE Q9: prepare %q (see README.md's \"Controller probes\" section for its exact content), "+
			"copy it to the USB drive's root from a PC, and load (do not run) it on the controller's own screen now",
			name)
	}
	t.Logf("PROBE Q9: polling status for up to %s for that file name to appear", probeQ9StatusPollTimeout)

	// A name other than the expected one, a truncated form of it included,
	// is worth reporting but does not answer Q9, which needs exactly 33
	// characters in the field.
	if lastSeen := probeQ9PollStatus(t, h, name, probeQ9StatusPollTimeout); lastSeen != name {
		t.Skipf("PROBE Q9: %q never appeared in a status reply within %s (last file name reported: %q); "+
			"prepare and load it (see README.md's \"Controller probes\" section), then rerun `make probe`",
			name, probeQ9StatusPollTimeout, lastSeen)
	}
}

// probeQ9PollStatus sends its own keepalive requests, once a second, rather
// than going through masso.Client: masso.Client's decoded Status.File would
// already have discarded exactly the raw bytes Q9 asks about. It returns
// the last non-empty file name observed by the time want matches or timeout
// elapses, so a miss still reports what the controller was showing.
func probeQ9PollStatus(t *testing.T, h *probeHarness, want string, timeout time.Duration) string {
	t.Helper()

	const pollInterval = time.Second

	deadline := time.Now().Add(timeout)

	var lastSeen string
	for time.Now().Before(deadline) {
		h.send(t, masso.Keepalive(time.Now()))

		// A decode error or a stray non-status reply must still reach the
		// sleep below rather than continue past it, or polling would spin
		// instead of holding to pollInterval.
		if raw, ok := h.recv(t); ok {
			reply, err := masso.DecodeReply(raw)
			if err != nil {
				t.Logf("PROBE Q9: undecodable reply while polling: %v", err)
			} else if st, ok := reply.(masso.Status); ok {
				if st.File != "" {
					lastSeen = st.File
				}

				if st.File == want {
					const fileStart = 17 // docs/protocol.md §4

					nameEnd := fileStart + len(want)
					contextEnd := min(nameEnd+8, len(raw))

					t.Logf("PROBE Q9: matched; raw file-name field (bytes %d-%d) = % x",
						fileStart, nameEnd-1, raw[fileStart:nameEnd])
					t.Logf("PROBE Q9: byte %d (immediately after the 33rd character) = 0x%02X", nameEnd, raw[nameEnd])
					t.Logf("PROBE Q9: bytes %d-%d for context = % x", fileStart, contextEnd-1, raw[fileStart:contextEnd])

					return want
				}
			}
		}

		time.Sleep(pollInterval)
	}

	return lastSeen
}

// probeFinalStatus reads one status reply at the end of the run, confirming
// the controller is still idle and reachable, per the Safety notes in
// README.md's "Controller probes" section.
func probeFinalStatus(t *testing.T, h *probeHarness) {
	t.Helper()

	st := h.requireStatusReply(t, "the last probe")

	if (st.Running || st.WaitingForOperator) && !allowRunning() {
		t.Errorf("PROBE: controller is not idle at the end of the run (running=%v waitingForOperator=%v)",
			st.Running, st.WaitingForOperator)
	}
}
