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

package sim_test

import (
	"bytes"
	"sync"
	"testing"
	"time"

	"msrl.dev/mink-lasso/internal/masso"
	"msrl.dev/mink-lasso/internal/masso/sim"
)

func TestSetStartResult(t *testing.T) {
	t.Parallel()
	ctrl := newController(t, sim.Options{})
	ctrl.SetStartResult(masso.StartNoUSB)

	client := newClient(t)
	startPkt, err := masso.UploadStart(10, "", "X.NC")
	if err != nil {
		t.Fatalf("UploadStart: %v", err)
	}
	mustWrite(t, client, ctrl.Addr(), startPkt)
	ack, ok := readReply(t, client).(masso.StartAck)
	if !ok || ack.Result != masso.StartNoUSB {
		t.Fatalf("start ack = %+v (ok=%v), want Result StartNoUSB", ack, ok)
	}
}

func TestSetChunkResult(t *testing.T) {
	t.Parallel()
	ctrl := newController(t, sim.Options{})
	ctrl.SetChunkResult(masso.ChunkUSBWriteError)

	client := newClient(t)
	data := []byte("hello")
	startPkt, err := masso.UploadStart(uint32(len(data)), "", "Y.NC")
	if err != nil {
		t.Fatalf("UploadStart: %v", err)
	}
	mustWrite(t, client, ctrl.Addr(), startPkt)
	readReply(t, client)

	chunkPkt, err := masso.UploadChunk(0, data)
	if err != nil {
		t.Fatalf("UploadChunk: %v", err)
	}
	mustWrite(t, client, ctrl.Addr(), chunkPkt)
	ack, ok := readReply(t, client).(masso.ChunkAck)
	if !ok || ack.Result != masso.ChunkUSBWriteError {
		t.Fatalf("chunk ack = %+v (ok=%v), want Result ChunkUSBWriteError", ack, ok)
	}
	// The reported result is cosmetic; the byte-accounting still advances,
	// since the fault knobs only change what is reported, not what is
	// stored.
	if ack.Accepted != 1 {
		t.Fatalf("Accepted = %d, want 1", ack.Accepted)
	}
}

func TestSetDropAck_FirstSightingOnly(t *testing.T) {
	t.Parallel()
	ctrl := newController(t, sim.Options{})

	var mu sync.Mutex
	calls := map[uint32]int{}
	ctrl.SetDropAck(func(idx uint32) bool {
		mu.Lock()
		calls[idx]++
		mu.Unlock()
		return idx == 0
	})

	client := newClient(t)
	chunk := []byte("0123456789")
	startUpload(t, client, ctrl.Addr(), "DROP.NC", 2*len(chunk))

	sendChunk(t, client, ctrl.Addr(), 0, chunk)
	expectSilence(t, client, 150*time.Millisecond) // ACK dropped

	// Chunk 0 was accepted even though its ACK was lost, so chunk 1 is in
	// order.
	sendChunk(t, client, ctrl.Addr(), 1, chunk)
	if ack := readChunkAck(t, client); ack.Accepted != 2 {
		t.Fatalf("chunk 1 ack = %+v, want Accepted 2", ack)
	}
	got, ok := ctrl.File("DROP.NC")
	if want := bytes.Repeat(chunk, 2); !ok || !bytes.Equal(got, want) {
		t.Fatalf("File() = %q (ok=%v), want %q", got, ok, want)
	}

	// Retransmit: the same index is no longer "first sighting", so this
	// one is acknowledged even though the callback would still say drop it.
	sendChunk(t, client, ctrl.Addr(), 0, chunk)
	if ack := readChunkAck(t, client); ack.Accepted != 2 {
		t.Fatalf("retransmit ack = %+v, want Accepted 2", ack)
	}

	mu.Lock()
	defer mu.Unlock()
	if calls[0] != 1 || calls[1] != 1 {
		t.Fatalf("dropAck invoked %v, want once per index (first sighting only)", calls)
	}
}

func TestSetDropChunk_FirstSightingOnly(t *testing.T) {
	t.Parallel()
	ctrl := newController(t, sim.Options{})

	var mu sync.Mutex
	dropChunkCalls := map[uint32]int{}
	dropAckCalls := map[uint32]int{}
	ctrl.SetDropChunk(func(idx uint32) bool {
		mu.Lock()
		dropChunkCalls[idx]++
		mu.Unlock()
		return idx == 0
	})
	ctrl.SetDropAck(func(idx uint32) bool {
		mu.Lock()
		dropAckCalls[idx]++
		mu.Unlock()
		return false
	})

	client := newClient(t)
	chunk := []byte("0123456789")
	startUpload(t, client, ctrl.Addr(), "LOST.NC", 2*len(chunk))

	sendChunk(t, client, ctrl.Addr(), 0, chunk)
	expectSilence(t, client, 150*time.Millisecond)

	// Chunk 0 was never accepted, so chunk 1 is out of order.
	sendChunk(t, client, ctrl.Addr(), 1, chunk)
	if ack := readChunkAck(t, client); ack.Accepted != 0 {
		t.Fatalf("chunk 1 ack = %+v, want Accepted 0", ack)
	}

	sendChunk(t, client, ctrl.Addr(), 0, chunk)
	if ack := readChunkAck(t, client); ack.Result != masso.ChunkOK || ack.Accepted != 1 {
		t.Fatalf("chunk 0 retransmit ack = %+v, want {OK 1}", ack)
	}
	sendChunk(t, client, ctrl.Addr(), 1, chunk)
	if ack := readChunkAck(t, client); ack.Accepted != 2 {
		t.Fatalf("chunk 1 retransmit ack = %+v, want Accepted 2", ack)
	}
	got, ok := ctrl.File("LOST.NC")
	if want := bytes.Repeat(chunk, 2); !ok || !bytes.Equal(got, want) {
		t.Fatalf("File() = %q (ok=%v), want %q", got, ok, want)
	}
	if n := ctrl.ChunkRequests(); n != 4 {
		t.Fatalf("ChunkRequests() = %d, want 4", n)
	}

	mu.Lock()
	defer mu.Unlock()
	if dropChunkCalls[0] != 1 || dropChunkCalls[1] != 1 {
		t.Fatalf("dropChunk invoked %v, want once per index", dropChunkCalls)
	}
	if dropAckCalls[0] != 0 || dropAckCalls[1] != 1 {
		t.Fatalf("dropAck invoked %v, want never for chunk 0, whose first sighting was dropped", dropAckCalls)
	}
}

func TestSetDropChunk_NilStopsDropping(t *testing.T) {
	t.Parallel()
	ctrl := newController(t, sim.Options{})
	ctrl.SetDropChunk(func(uint32) bool { return true })
	ctrl.SetDropChunk(nil)
	ctrl.SetDropAck(func(uint32) bool { return true })
	ctrl.SetDropAck(nil)

	client := newClient(t)
	uploadFile(t, client, ctrl.Addr(), "NIL.NC", []byte("data"))
}

func TestSetDropStartAcks(t *testing.T) {
	t.Parallel()
	ctrl := newController(t, sim.Options{})
	ctrl.SetDropStartAcks(1)

	client := newClient(t)
	data := []byte("started without an ACK")
	startPkt, err := masso.UploadStart(uint32(len(data)), "", "NOACK.NC")
	if err != nil {
		t.Fatalf("UploadStart: %v", err)
	}
	mustWrite(t, client, ctrl.Addr(), startPkt)
	expectSilence(t, client, 150*time.Millisecond)

	// The unanswered start still began the upload.
	sendChunk(t, client, ctrl.Addr(), 0, data)
	if ack := readChunkAck(t, client); ack.Accepted != 1 {
		t.Fatalf("chunk ack = %+v, want Accepted 1", ack)
	}
	if got, ok := ctrl.File("NOACK.NC"); !ok || !bytes.Equal(got, data) {
		t.Fatalf("File() = %q (ok=%v), want %q", got, ok, data)
	}

	// Only one ACK was dropped.
	startUpload(t, client, ctrl.Addr(), "NEXT.NC", 1)
}

func TestStartResults_WhichBeginAnUpload(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		result byte
		begins bool
	}{
		{"OK", masso.StartOK, true},
		{"already started", masso.StartAlreadyStarted, true},
		{"no USB", masso.StartNoUSB, false},
		{"other", 0x42, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctrl := newController(t, sim.Options{})
			ctrl.SetStartResult(tc.result)
			client := newClient(t)
			data := []byte("abc")
			startPkt, err := masso.UploadStart(uint32(len(data)), "", "R.NC")
			if err != nil {
				t.Fatalf("UploadStart: %v", err)
			}
			mustWrite(t, client, ctrl.Addr(), startPkt)
			if ack, ok := readReply(t, client).(masso.StartAck); !ok || ack.Result != tc.result {
				t.Fatalf("start ack = %+v (ok=%v), want Result 0x%02X", ack, ok, tc.result)
			}

			sendChunk(t, client, ctrl.Addr(), 0, data)
			ack := readChunkAck(t, client)
			_, stored := ctrl.File("R.NC")
			if tc.begins {
				if ack.Accepted != 1 || !stored {
					t.Fatalf("ack = %+v, stored = %v; want Accepted 1 and stored", ack, stored)
				}
				return
			}
			if ack.Accepted != 0 || stored {
				t.Fatalf("ack = %+v, stored = %v; want Accepted 0 and nothing stored", ack, stored)
			}
		})
	}
}

func TestChunkWithNoUploadInProgress(t *testing.T) {
	t.Parallel()
	ctrl := newController(t, sim.Options{})
	client := newClient(t)
	sendChunk(t, client, ctrl.Addr(), 0, []byte("stray"))
	if ack := readChunkAck(t, client); ack.Result != masso.ChunkOK || ack.Accepted != 0 {
		t.Fatalf("ack = %+v, want {OK 0}", ack)
	}
	if files := ctrl.Files(); len(files) != 0 {
		t.Fatalf("Files() = %v, want empty", files)
	}
}

func TestUploadAbort_DiscardsUploadInProgress(t *testing.T) {
	t.Parallel()
	ctrl := newController(t, sim.Options{})
	client := newClient(t)
	chunk := []byte("0123456789")
	startUpload(t, client, ctrl.Addr(), "AB.NC", 2*len(chunk))
	sendChunk(t, client, ctrl.Addr(), 0, chunk)
	if ack := readChunkAck(t, client); ack.Accepted != 1 {
		t.Fatalf("chunk 0 ack = %+v, want Accepted 1", ack)
	}

	mustWrite(t, client, ctrl.Addr(), masso.UploadAbort())
	// The abort has no reply; the keepalive's reply proves it was handled.
	mustWrite(t, client, ctrl.Addr(), masso.Keepalive(time.Now()))
	if _, ok := readReply(t, client).(masso.Status); !ok {
		t.Fatal("the reply after an abort was not the keepalive's status")
	}
	if n := ctrl.Aborts(); n != 1 {
		t.Fatalf("Aborts() = %d, want 1", n)
	}

	// Neither the rest of the aborted upload nor a restart of its chunks
	// is stored.
	sendChunk(t, client, ctrl.Addr(), 1, chunk)
	if ack := readChunkAck(t, client); ack.Accepted != 0 {
		t.Fatalf("chunk 1 ack = %+v, want Accepted 0", ack)
	}
	sendChunk(t, client, ctrl.Addr(), 0, chunk)
	if ack := readChunkAck(t, client); ack.Accepted != 0 {
		t.Fatalf("chunk 0 ack after abort = %+v, want Accepted 0", ack)
	}
	if _, ok := ctrl.File("AB.NC"); ok {
		t.Fatal("aborted upload was stored")
	}

	// A new start works normally.
	uploadFile(t, client, ctrl.Addr(), "AB.NC", chunk)
	if got, ok := ctrl.File("AB.NC"); !ok || !bytes.Equal(got, chunk) {
		t.Fatalf("File() = %q (ok=%v), want %q", got, ok, chunk)
	}
}

func TestUploadAbort_KeepsCompletedFile(t *testing.T) {
	t.Parallel()
	ctrl := newController(t, sim.Options{})
	client := newClient(t)
	uploadFile(t, client, ctrl.Addr(), "DONE.NC", []byte("done"))
	mustWrite(t, client, ctrl.Addr(), masso.UploadAbort())
	mustWrite(t, client, ctrl.Addr(), masso.Keepalive(time.Now()))
	readReply(t, client)
	if n := ctrl.Aborts(); n != 1 {
		t.Fatalf("Aborts() = %d, want 1", n)
	}
	if got, ok := ctrl.File("DONE.NC"); !ok || string(got) != "done" {
		t.Fatalf("File() = %q (ok=%v), want %q", got, ok, "done")
	}
}

func TestSilenced_StillCountsChunksAndAborts(t *testing.T) {
	t.Parallel()
	ctrl := newController(t, sim.Options{})
	ctrl.SetSilent(true)
	client := newClient(t)

	sendChunk(t, client, ctrl.Addr(), 0, []byte("x"))
	mustWrite(t, client, ctrl.Addr(), masso.UploadAbort())
	waitUntil(t, func() bool { return ctrl.Aborts() == 1 })
	if n := ctrl.ChunkRequests(); n != 1 {
		t.Fatalf("ChunkRequests() = %d, want 1", n)
	}
	expectSilence(t, client, 100*time.Millisecond)
}

func TestSetSilentAfterChunk_OutlastsAbort(t *testing.T) {
	t.Parallel()
	ctrl := newController(t, sim.Options{})
	ctrl.SetSilentAfterChunk(0)
	client := newClient(t)
	chunk := []byte("0123456789")
	startUpload(t, client, ctrl.Addr(), "SA.NC", 2*len(chunk))
	sendChunk(t, client, ctrl.Addr(), 0, chunk)
	readChunkAck(t, client)

	mustWrite(t, client, ctrl.Addr(), masso.UploadAbort())
	waitUntil(t, func() bool { return ctrl.Aborts() == 1 })
	mustWrite(t, client, ctrl.Addr(), masso.Keepalive(time.Now()))
	expectSilence(t, client, 150*time.Millisecond)

	ctrl.SetSilentAfterChunk(-1)
	mustWrite(t, client, ctrl.Addr(), masso.Keepalive(time.Now()))
	if _, ok := readReply(t, client).(masso.Status); !ok {
		t.Fatal("reply after lifting the silence was not a status")
	}
}

func TestSetDuplicateReplies(t *testing.T) {
	t.Parallel()
	ctrl := newController(t, sim.Options{Serial: 9})
	ctrl.SetDuplicateReplies(true)

	client := newClient(t)
	mustWrite(t, client, ctrl.Addr(), masso.Discovery(clientPort(t, client)))

	for range 2 {
		reply := readReply(t, client)
		id, ok := reply.(masso.Identity)
		if !ok || id.Serial != 9 {
			t.Fatalf("reply = %+v (ok=%v), want Identity{Serial:9}", reply, ok)
		}
	}
	expectSilence(t, client, 100*time.Millisecond)
}

func TestSetSilentAfterChunk_StopsEverythingAfterN(t *testing.T) {
	t.Parallel()
	ctrl := newController(t, sim.Options{})
	ctrl.SetSilentAfterChunk(0) // silence starts once chunk index 0 is accepted

	client := newClient(t)
	chunkData := []byte("0123456789")
	startPkt, err := masso.UploadStart(uint32(2*len(chunkData)), "", "SIL.NC")
	if err != nil {
		t.Fatalf("UploadStart: %v", err)
	}
	mustWrite(t, client, ctrl.Addr(), startPkt)
	readReply(t, client) // chunk 0 has not been accepted yet: still answered

	chunk0, err := masso.UploadChunk(0, chunkData)
	if err != nil {
		t.Fatalf("UploadChunk: %v", err)
	}
	mustWrite(t, client, ctrl.Addr(), chunk0)
	ack, ok := readReply(t, client).(masso.ChunkAck)
	if !ok || ack.Accepted != 1 {
		t.Fatalf("chunk 0 ack = %+v (ok=%v), want Accepted 1", ack, ok)
	}

	// Chunk 0 is now accepted: everything from here on is ignored,
	// including the very next chunk...
	chunk1, err := masso.UploadChunk(1, chunkData)
	if err != nil {
		t.Fatalf("UploadChunk: %v", err)
	}
	mustWrite(t, client, ctrl.Addr(), chunk1)
	expectSilence(t, client, 150*time.Millisecond)

	// ...and any other request type, not just upload chunks.
	mustWrite(t, client, ctrl.Addr(), masso.Keepalive(time.Now()))
	expectSilence(t, client, 150*time.Millisecond)

	if _, ok := ctrl.File("SIL.NC"); ok {
		t.Fatal("File(\"SIL.NC\") exists, but the second chunk was never acknowledged as accepted")
	}
}

func TestSetSilent(t *testing.T) {
	t.Parallel()
	ctrl := newController(t, sim.Options{})
	ctrl.SetSilent(true)

	client := newClient(t)
	mustWrite(t, client, ctrl.Addr(), masso.Discovery(clientPort(t, client)))
	expectSilence(t, client, 150*time.Millisecond)

	if got := ctrl.Discoveries(); got != 0 {
		t.Fatalf("Discoveries() = %d, want 0 (a silenced controller does not even count the request)", got)
	}
}
