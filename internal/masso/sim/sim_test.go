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
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"msrl.dev/mink-lasso/internal/masso"
	"msrl.dev/mink-lasso/internal/masso/sim"
)

func TestNew_Defaults(t *testing.T) {
	t.Parallel()
	ctrl := newController(t, sim.Options{})
	addr := ctrl.Addr()
	if addr == nil {
		t.Fatal("Addr() = nil")
	}
	if !addr.IP.IsLoopback() {
		t.Fatalf("Addr().IP = %v, want loopback", addr.IP)
	}
	if addr.Port == 0 {
		t.Fatal("Addr().Port = 0, want an OS-assigned port")
	}
}

func TestNew_ListenPacketError(t *testing.T) {
	t.Parallel()
	wantErr := errors.New("boom")
	_, err := sim.New(sim.Options{
		ListenPacket: func(string, string) (net.PacketConn, error) {
			return nil, wantErr
		},
	})
	if !errors.Is(err, sim.ErrListen) || !errors.Is(err, wantErr) {
		t.Fatalf("New() error = %v, want it to wrap ErrListen and %v", err, wantErr)
	}
}

func TestClose_Idempotent(t *testing.T) {
	t.Parallel()
	ctrl, err := sim.New(sim.Options{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := ctrl.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := ctrl.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func TestDiscovery_ReplyPortHonored(t *testing.T) {
	t.Parallel()
	ctrl := newController(t, sim.Options{Serial: 42, Version: "5-Axis v5.13"})

	sender := newClient(t) // port A: sends the discovery
	target := newClient(t) // port B: advertised, receives every reply
	replyPort := clientPort(t, target)

	mustWrite(t, sender, ctrl.Addr(), masso.Discovery(replyPort))

	reply := readReply(t, target)
	id, ok := reply.(masso.Identity)
	if !ok {
		t.Fatalf("reply type = %T, want Identity", reply)
	}
	if id.Serial != 42 || id.Version != "5-Axis v5.13" {
		t.Fatalf("Identity = %+v", id)
	}

	expectSilence(t, sender, 100*time.Millisecond)

	if got := ctrl.Discoveries(); got != 1 {
		t.Fatalf("Discoveries() = %d, want 1", got)
	}
	got := ctrl.LastReplyTarget()
	if got == nil || got.Port != int(replyPort) || !got.IP.IsLoopback() {
		t.Fatalf("LastReplyTarget() = %v, want loopback port %d", got, replyPort)
	}
}

func TestDiscovery_CountsMultiple(t *testing.T) {
	t.Parallel()
	ctrl := newController(t, sim.Options{})
	client := newClient(t)
	discover(t, client, ctrl.Addr())
	discover(t, client, ctrl.Addr())
	if got := ctrl.Discoveries(); got != 2 {
		t.Fatalf("Discoveries() = %d, want 2", got)
	}
}

func TestPreDiscovery_RepliesGoToSourceAddress(t *testing.T) {
	t.Parallel()
	ctrl := newController(t, sim.Options{Serial: 7})
	if got := ctrl.LastReplyTarget(); got != nil {
		t.Fatalf("LastReplyTarget() before any discovery = %v, want nil", got)
	}

	client := newClient(t)
	mustWrite(t, client, ctrl.Addr(), masso.Config(time.Now()))
	reply := readReply(t, client)
	cr, ok := reply.(masso.ConfigReply)
	if !ok {
		t.Fatalf("reply type = %T, want ConfigReply", reply)
	}
	if cr.Serial != 7 {
		t.Fatalf("Serial = %d, want 7", cr.Serial)
	}
}

func TestPostDiscovery_RepliesIgnoreRequestSource(t *testing.T) {
	t.Parallel()
	ctrl := newController(t, sim.Options{})
	target := newClient(t)
	discover(t, target, ctrl.Addr())

	other := newClient(t)
	mustWrite(t, other, ctrl.Addr(), masso.Keepalive(time.Now()))

	reply := readReply(t, target)
	if _, ok := reply.(masso.Status); !ok {
		t.Fatalf("reply type = %T, want Status", reply)
	}
	expectSilence(t, other, 100*time.Millisecond)

	if got := ctrl.Keepalives(); got != 1 {
		t.Fatalf("Keepalives() = %d, want 1", got)
	}
}

func TestKeepalive_DefaultStatus(t *testing.T) {
	t.Parallel()
	ctrl := newController(t, sim.Options{})
	client := newClient(t)
	mustWrite(t, client, ctrl.Addr(), masso.Keepalive(time.Now()))
	reply := readReply(t, client)
	status, ok := reply.(masso.Status)
	if !ok {
		t.Fatalf("reply type = %T, want Status", reply)
	}
	if status.Progress != 0 || status.Running || status.WaitingForOperator {
		t.Fatalf("default Status = %+v, want idle, not waiting, progress 0", status)
	}
}

func TestKeepalive_SetStatus(t *testing.T) {
	t.Parallel()
	ctrl := newController(t, sim.Options{})
	want := masso.Status{Progress: 42, State: 0x02, Jobs: 3, Prompt: 0x01, Line: 100, File: "PART.NC"}
	ctrl.SetStatus(want)

	client := newClient(t)
	mustWrite(t, client, ctrl.Addr(), masso.Keepalive(time.Now()))
	reply := readReply(t, client)
	got, ok := reply.(masso.Status)
	if !ok {
		t.Fatalf("reply type = %T, want Status", reply)
	}
	if got.Progress != want.Progress || !got.Running || got.Jobs != want.Jobs ||
		got.Line != want.Line || got.File != want.File {
		t.Fatalf("Status = %+v, want %+v", got, want)
	}
}

func TestToolQuery(t *testing.T) {
	t.Parallel()
	ctrl := newController(t, sim.Options{Tools: []string{"1/4 End Mill", "1/2 Drill"}})
	client := newClient(t)

	tests := []struct {
		index uint8
		want  string
	}{
		{1, "1/4 End Mill"},
		{2, "1/2 Drill"},
		{3, ""}, // past the end of the table
		{0, ""}, // below the 1-based range
	}
	for _, tt := range tests {
		mustWrite(t, client, ctrl.Addr(), masso.ToolQuery(tt.index))
		reply := readReply(t, client)
		rec, ok := reply.(masso.ToolRecord)
		if !ok {
			t.Fatalf("reply type = %T, want ToolRecord", reply)
		}
		if rec.Index != tt.index || rec.Name != tt.want {
			t.Errorf("ToolQuery(%d) = %+v, want Name %q", tt.index, rec, tt.want)
		}
	}
}

func TestUpload_HappyPathSingleChunk(t *testing.T) {
	t.Parallel()
	ctrl := newController(t, sim.Options{})
	client := newClient(t)
	data := []byte("G0 X0 Y0\nG1 X10\n")
	uploadFile(t, client, ctrl.Addr(), "PART.NC", data)

	got, ok := ctrl.File("PART.NC")
	if !ok {
		t.Fatal("File(\"PART.NC\") not found")
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("File() = %q, want %q", got, data)
	}
}

func TestUpload_ChunkSizes(t *testing.T) {
	t.Parallel()
	sizes := []int{1, masso.MaxChunkData, masso.MaxChunkData + 1, 3*masso.MaxChunkData + 37}
	for _, n := range sizes {
		ctrl := newController(t, sim.Options{})
		client := newClient(t)
		data := make([]byte, n)
		for i := range data {
			data[i] = byte(i % 256)
		}
		uploadFile(t, client, ctrl.Addr(), "SIZE.NC", data)

		got, ok := ctrl.File("SIZE.NC")
		if !ok || !bytes.Equal(got, data) {
			t.Errorf("size %d: File() mismatch (ok=%v, len=%d, want %d)", n, ok, len(got), n)
		}
	}
}

func TestUpload_Overwrite(t *testing.T) {
	t.Parallel()
	ctrl := newController(t, sim.Options{})
	client := newClient(t)
	uploadFile(t, client, ctrl.Addr(), "SAME.NC", []byte("first"))
	uploadFile(t, client, ctrl.Addr(), "SAME.NC", []byte("second-and-longer"))

	got, ok := ctrl.File("SAME.NC")
	if !ok || string(got) != "second-and-longer" {
		t.Fatalf("File() = %q (ok=%v), want %q", got, ok, "second-and-longer")
	}
}

func TestUpload_RetransmitOfAcceptedChunkIsIdempotent(t *testing.T) {
	t.Parallel()
	ctrl := newController(t, sim.Options{})
	client := newClient(t)
	data := []byte("a whole file that fits in one chunk")

	startPkt, err := masso.UploadStart(uint32(len(data)), "R.NC")
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
	first, ok := readReply(t, client).(masso.ChunkAck)
	if !ok || first.Accepted != 1 {
		t.Fatalf("first ack = %+v (ok=%v), want Accepted 1", first, ok)
	}

	// A client retransmit of the same, already-accepted chunk must not
	// advance Accepted or duplicate the stored bytes.
	mustWrite(t, client, ctrl.Addr(), chunk0)
	second, ok := readReply(t, client).(masso.ChunkAck)
	if !ok || second.Accepted != 1 {
		t.Fatalf("retransmit ack = %+v (ok=%v), want Accepted 1", second, ok)
	}

	got, ok := ctrl.File("R.NC")
	if !ok || !bytes.Equal(got, data) {
		t.Fatalf("File() = %q (ok=%v), want %q", got, ok, data)
	}
}

func TestUpload_OutOfOrderChunkDoesNotAdvance(t *testing.T) {
	t.Parallel()
	ctrl := newController(t, sim.Options{})
	client := newClient(t)

	startPkt, err := masso.UploadStart(300, "OO.NC")
	if err != nil {
		t.Fatalf("UploadStart: %v", err)
	}
	mustWrite(t, client, ctrl.Addr(), startPkt)
	readReply(t, client)

	chunk2, err := masso.UploadChunk(2, bytes.Repeat([]byte{'x'}, 50))
	if err != nil {
		t.Fatalf("UploadChunk: %v", err)
	}
	mustWrite(t, client, ctrl.Addr(), chunk2)
	ack, ok := readReply(t, client).(masso.ChunkAck)
	if !ok || ack.Accepted != 0 {
		t.Fatalf("out-of-order ack = %+v (ok=%v), want Accepted 0", ack, ok)
	}

	chunk0, err := masso.UploadChunk(0, bytes.Repeat([]byte{'y'}, 50))
	if err != nil {
		t.Fatalf("UploadChunk: %v", err)
	}
	mustWrite(t, client, ctrl.Addr(), chunk0)
	ack0, ok := readReply(t, client).(masso.ChunkAck)
	if !ok || ack0.Accepted != 1 {
		t.Fatalf("chunk 0 ack = %+v (ok=%v), want Accepted 1", ack0, ok)
	}
}

func TestMalformedPacket_DroppedWithoutBreakingTheController(t *testing.T) {
	t.Parallel()
	base := listenLoopback(t)
	conn, ready := injectOnce(base, []byte{0x01, 0x02, 0x03}, &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 1})

	var logBuf syncBuffer
	logger := newDebugLogger(&logBuf)
	ctrl := newController(t, sim.Options{
		Logger:       logger,
		ListenPacket: func(string, string) (net.PacketConn, error) { return conn, nil },
	})
	<-ready

	if !strings.Contains(logBuf.String(), "dropping malformed packet") {
		t.Fatalf("log = %q, want it to mention the dropped packet", logBuf.String())
	}

	// The controller must still work normally afterward.
	client := newClient(t)
	discover(t, client, ctrl.Addr())
}

func TestFiles_ReturnsIndependentCopies(t *testing.T) {
	t.Parallel()
	ctrl := newController(t, sim.Options{})
	client := newClient(t)
	uploadFile(t, client, ctrl.Addr(), "COPY.NC", []byte("original"))

	files := ctrl.Files()
	files["COPY.NC"][0] = 'X'
	again := ctrl.Files()
	if string(again["COPY.NC"]) != "original" {
		t.Fatalf("Files() mutation leaked into stored state: %q", again["COPY.NC"])
	}

	data, ok := ctrl.File("COPY.NC")
	if !ok {
		t.Fatal("File not found")
	}
	data[0] = 'Y'
	data2, _ := ctrl.File("COPY.NC")
	if string(data2) != "original" {
		t.Fatalf("File() mutation leaked into stored state: %q", data2)
	}
}

func TestFile_NotFound(t *testing.T) {
	t.Parallel()
	ctrl := newController(t, sim.Options{})
	if _, ok := ctrl.File("NOPE.NC"); ok {
		t.Fatal("File() ok = true for a name that was never uploaded")
	}
	if files := ctrl.Files(); len(files) != 0 {
		t.Fatalf("Files() = %v, want empty", files)
	}
}
