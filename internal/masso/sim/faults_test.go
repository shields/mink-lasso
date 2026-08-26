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
	startPkt, err := masso.UploadStart(10, "X.NC")
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
	startPkt, err := masso.UploadStart(uint32(len(data)), "Y.NC")
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
	data := []byte("payload for chunk zero, exactly one chunk")
	startPkt, err := masso.UploadStart(uint32(len(data)), "DROP.NC")
	if err != nil {
		t.Fatalf("UploadStart: %v", err)
	}
	mustWrite(t, client, ctrl.Addr(), startPkt)
	readReply(t, client)

	chunk0, err := masso.UploadChunk(0, data)
	if err != nil {
		t.Fatalf("UploadChunk: %v", err)
	}
	mustWrite(t, client, ctrl.Addr(), chunk0)
	expectSilence(t, client, 150*time.Millisecond) // dropped

	// Retransmit: the same index is no longer "first sighting", so this
	// one goes through even though the callback would still say drop it.
	mustWrite(t, client, ctrl.Addr(), chunk0)
	ack, ok := readReply(t, client).(masso.ChunkAck)
	if !ok || ack.Accepted != 1 {
		t.Fatalf("retransmit ack = %+v (ok=%v), want Accepted 1", ack, ok)
	}

	got, ok := ctrl.File("DROP.NC")
	if !ok || !bytes.Equal(got, data) {
		t.Fatalf("File() = %q (ok=%v), want %q", got, ok, data)
	}

	mu.Lock()
	defer mu.Unlock()
	if calls[0] != 1 {
		t.Fatalf("dropAck invoked %d times for chunk 0, want 1 (first sighting only)", calls[0])
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
	startPkt, err := masso.UploadStart(uint32(2*len(chunkData)), "SIL.NC")
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
