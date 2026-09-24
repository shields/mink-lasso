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
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"sync"
	"testing"
	"time"

	"msrl.dev/mink-lasso/internal/clock"
)

type uploadProgress struct{ sent, total int64 }

// uploadHarness runs Upload against a controller scripted by the test.
// Every packet the Client writes arrives, decoded, on sent instead of going
// out on the network, and the test delivers replies by calling handlePacket
// itself, so every step is ordered by the test and timed by clk.
type uploadHarness struct {
	t        *testing.T
	c        *Client
	clk      *clock.Fake
	remote   *net.UDPAddr
	sent     chan Request
	progress chan uploadProgress
	result   chan error
	stale    chan struct{}
}

// newUploadHarness builds a connected Client whose options are opts with
// the harness's clock, logger, and socket filled in. writeErr, if non-nil,
// is consulted for every write the Client makes; a non-nil result fails
// that write instead of recording it.
func newUploadHarness(t *testing.T, opts Options, writeErr func(Request) error) *uploadHarness {
	t.Helper()
	h := &uploadHarness{
		t:        t,
		clk:      clock.NewFake(time.Unix(0, 0)),
		remote:   &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: ControllerPort},
		sent:     make(chan Request, 256),
		progress: make(chan uploadProgress, 256),
		result:   make(chan error, 1),
		stale:    make(chan struct{}, 1),
	}
	realConn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("ListenPacket: %v", err)
	}
	fc := &fakeConn{PacketConn: realConn, writeTo: func(p []byte, _ net.Addr) (int, error) {
		req, decodeErr := DecodeRequest(p)
		if decodeErr != nil {
			t.Errorf("Client wrote an undecodable packet: %v", decodeErr)
			return len(p), nil
		}
		if writeErr != nil {
			if failure := writeErr(req); failure != nil {
				return 0, failure
			}
		}
		h.sent <- req
		return len(p), nil
	}}
	port := freePort(t)
	opts.Clock = h.clk
	opts.Logger = slog.New(messageSignalHandler{ch: h.stale, want: "masso: upload: chunk ACK did not advance"})
	opts.PortMin, opts.PortMax = port, port
	opts.ListenPacket = func(string, string) (net.PacketConn, error) { return fc, nil }
	c, err := NewClient(opts)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	t.Cleanup(func() {
		if err := c.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	c.setConnection(h.remote, Identity{})
	h.c = c
	return h
}

func (h *uploadHarness) start(ctx context.Context, r io.ReaderAt, size int64) {
	go func() {
		h.result <- h.c.Upload(ctx, "", "T.NC", r, size, func(sent, total int64) {
			h.progress <- uploadProgress{sent, total}
		})
	}()
}

func (h *uploadHarness) next() Request {
	h.t.Helper()
	return waitFor(h.t, h.sent)
}

func (h *uploadHarness) expectStart() UploadStartRequest {
	h.t.Helper()
	req := h.next()
	sr, ok := req.(UploadStartRequest)
	if !ok {
		h.t.Fatalf("sent %T, want UploadStartRequest", req)
	}
	return sr
}

func (h *uploadHarness) expectChunks(indices ...uint32) {
	h.t.Helper()
	for _, want := range indices {
		req := h.next()
		cr, ok := req.(UploadChunkRequest)
		if !ok || cr.Index != want {
			h.t.Fatalf("sent %#v, want chunk %d", req, want)
		}
	}
}

// expectNothingSent asserts nothing the Client wrote is waiting unread.
// It is meaningful only once the Client is known to be blocked, for example
// right after wait or settle.
func (h *uploadHarness) expectNothingSent() {
	h.t.Helper()
	select {
	case req := <-h.sent:
		h.t.Fatalf("unexpected send %#v", req)
	default:
	}
}

// settle returns once the Client has armed its next timer. The caller must
// already have observed an event (a send, a progress call, or a stale-ACK
// log) that the Client produces after stopping its previous timer.
func (h *uploadHarness) settle() {
	h.t.Helper()
	h.clk.BlockUntil(1)
	if n := h.clk.Pending(); n != 1 {
		h.t.Fatalf("Pending() = %d, want exactly one timer", n)
	}
}

func (h *uploadHarness) reply(pkt []byte) {
	h.c.handlePacket(pkt, h.remote)
}

func (h *uploadHarness) ackStart(result byte) {
	h.reply(StartAck{Result: result}.Encode())
}

func (h *uploadHarness) ackChunk(result byte, accepted uint32) {
	h.reply(ChunkAck{Result: result, Accepted: accepted}.Encode())
}

func (h *uploadHarness) expectProgress(sent, total int64) {
	h.t.Helper()
	if got := waitFor(h.t, h.progress); got != (uploadProgress{sent, total}) {
		h.t.Fatalf("progress = %+v, want {%d %d}", got, sent, total)
	}
}

func (h *uploadHarness) wait() error {
	h.t.Helper()
	return waitFor(h.t, h.result)
}

// advanceExactlyf settles, then advances d, failing with format if the
// Client's timer fires even a nanosecond early.
func (h *uploadHarness) advanceExactlyf(d time.Duration, format string, args ...any) {
	h.t.Helper()
	h.settle()
	h.clk.Advance(d - time.Nanosecond)
	if h.clk.Pending() != 1 {
		h.t.Fatalf(format, args...)
	}
	h.clk.Advance(time.Nanosecond)
}

// expectAborts returns Upload's result after asserting it is preceded by
// the three upload-abort notifications, the first being the next packet
// sent.
func (h *uploadHarness) expectAborts() error {
	h.t.Helper()
	for i := range abortNotifications {
		if i > 0 {
			h.advanceExactlyf(h.c.abortInterval, "abort %d: sent before AbortInterval elapsed", i)
		}
		if req := h.next(); req != (UploadAbortRequest{}) {
			h.t.Fatalf("abort %d: sent %#v, want UploadAbortRequest", i, req)
		}
	}
	err := h.wait()
	h.expectNothingSent()
	return err
}

func (h *uploadHarness) beginChunks(ctx context.Context, data []byte) {
	h.t.Helper()
	h.start(ctx, bytes.NewReader(data), int64(len(data)))
	h.expectStart()
	h.ackStart(StartOK)
	h.expectProgress(0, int64(len(data)))
}

func chunkData(n int) []byte {
	b := make([]byte, n*MaxChunkData-1)
	for i := range b {
		b[i] = byte(i % 251)
	}
	return b
}

func TestRetransmitTimeoutClamps(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ srtt, want time.Duration }{
		{0, minRTO},
		{10 * time.Millisecond, minRTO},
		{20 * time.Millisecond, 40 * time.Millisecond},
		{initialSRTT, 120 * time.Millisecond},
		{125 * time.Millisecond, maxRTO},
		{time.Second, maxRTO},
	} {
		if got := retransmitTimeout(tc.srtt); got != tc.want {
			t.Errorf("retransmitTimeout(%v) = %v, want %v", tc.srtt, got, tc.want)
		}
	}
}

func TestSmoothRTT(t *testing.T) {
	t.Parallel()
	if got := smoothRTT(initialSRTT, 20*time.Millisecond); got != 50*time.Millisecond {
		t.Errorf("smoothRTT(60ms, 20ms) = %v, want 50ms", got)
	}
	if got := smoothRTT(40*time.Millisecond, 200*time.Millisecond); got != 80*time.Millisecond {
		t.Errorf("smoothRTT(40ms, 200ms) = %v, want 80ms", got)
	}
}

func TestEarlier(t *testing.T) {
	t.Parallel()
	a, b := time.Unix(1, 0), time.Unix(2, 0)
	if got := earlier(a, b); !got.Equal(a) {
		t.Errorf("earlier(a, b) = %v, want %v", got, a)
	}
	if got := earlier(b, a); !got.Equal(a) {
		t.Errorf("earlier(b, a) = %v, want %v", got, a)
	}
}

func TestUploadStartResendCadenceAndGiveUp(t *testing.T) {
	t.Parallel()
	h := newUploadHarness(t, Options{}, nil)
	h.start(t.Context(), bytes.NewReader(nil), 0)
	t0 := h.clk.Now()

	first := h.expectStart()
	for i := 1; i <= 4; i++ {
		h.advanceExactlyf(time.Second, "resend %d came early", i)
		if got := h.expectStart(); got != first {
			t.Fatalf("resend %d = %+v, want identical %+v", i, got, first)
		}
		if got := h.clk.Since(t0); got != time.Duration(i)*time.Second {
			t.Fatalf("resend %d at %v, want %ds", i, got, i)
		}
	}
	h.settle()
	h.clk.Advance(time.Second)
	if err := h.wait(); !errors.Is(err, ErrNoResponse) {
		t.Fatalf("Upload = %v, want ErrNoResponse", err)
	}
	if got := h.clk.Since(t0); got != 5*time.Second {
		t.Fatalf("gave up at %v, want 5s", got)
	}
	h.expectNothingSent()
}

func TestUploadStartTimeoutNotMultipleOfRetransmit(t *testing.T) {
	t.Parallel()
	h := newUploadHarness(t, Options{StartRetransmit: 2 * time.Second, StartTimeout: 3 * time.Second}, nil)
	h.start(t.Context(), bytes.NewReader(nil), 0)
	h.expectStart()
	h.settle()
	h.clk.Advance(2 * time.Second)
	h.expectStart()
	h.advanceExactlyf(time.Second, "gave up early")
	if err := h.wait(); !errors.Is(err, ErrNoResponse) {
		t.Fatalf("Upload = %v, want ErrNoResponse", err)
	}
	h.expectNothingSent()
}

func TestUploadAlreadyStartedAfterResend(t *testing.T) {
	t.Parallel()
	h := newUploadHarness(t, Options{}, nil)
	data := []byte("G0 X0\n")
	h.start(t.Context(), bytes.NewReader(data), int64(len(data)))
	h.expectStart()
	h.settle()
	h.clk.Advance(time.Second)
	h.expectStart()
	h.ackStart(StartAlreadyStarted)
	h.expectProgress(0, int64(len(data)))
	h.expectChunks(0)
	h.ackChunk(ChunkOK, 1)
	h.expectProgress(int64(len(data)), int64(len(data)))
	if err := h.wait(); err != nil {
		t.Fatalf("Upload = %v, want nil", err)
	}
	h.expectNothingSent()
}

// nowHook runs hook once, from Now, the first time the time it reports
// reaches at.
type nowHook struct {
	*clock.Fake

	at   time.Time
	once sync.Once
	hook func()
}

func (n *nowHook) Now() time.Time {
	now := n.Fake.Now()
	if !now.Before(n.at) {
		n.once.Do(n.hook)
	}
	return now
}

func TestUploadFirstAttemptAckQueuedAsResendFallsDue(t *testing.T) {
	t.Parallel()
	h := newUploadHarness(t, Options{}, nil)
	h.c.clock = &nowHook{Fake: h.clk, at: h.clk.Now().Add(time.Second), hook: func() {
		h.ackStart(StartAlreadyStarted)
	}}
	h.start(t.Context(), bytes.NewReader([]byte("x")), 1)
	h.expectStart()
	h.settle()
	h.clk.Advance(time.Second)
	if err := h.wait(); !errors.Is(err, ErrTransfer) {
		t.Fatalf("Upload = %v, want ErrTransfer", err)
	}
	h.expectNothingSent()
}

func TestUploadStartRefused(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		result byte
		want   error
	}{
		{"already started on first attempt", StartAlreadyStarted, ErrTransfer},
		{"no USB", StartNoUSB, ErrNoUSB},
		{"other", 0x42, ErrTransfer},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newUploadHarness(t, Options{}, nil)
			h.start(t.Context(), bytes.NewReader([]byte("x")), 1)
			h.expectStart()
			h.ackStart(tc.result)
			if err := h.wait(); !errors.Is(err, tc.want) {
				t.Fatalf("Upload = %v, want %v", err, tc.want)
			}
			h.expectNothingSent()
		})
	}
}

func TestUploadNoUSBAfterResend(t *testing.T) {
	t.Parallel()
	h := newUploadHarness(t, Options{}, nil)
	h.start(t.Context(), bytes.NewReader([]byte("x")), 1)
	h.expectStart()
	h.settle()
	h.clk.Advance(time.Second)
	h.expectStart()
	h.ackStart(StartNoUSB)
	if err := h.wait(); !errors.Is(err, ErrNoUSB) {
		t.Fatalf("Upload = %v, want ErrNoUSB", err)
	}
	h.expectNothingSent()
}

func TestUploadCtxCanceledDuringStart(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	h := newUploadHarness(t, Options{}, nil)
	h.start(ctx, bytes.NewReader([]byte("x")), 1)
	h.expectStart()
	h.settle()
	cancel()
	if err := h.wait(); !errors.Is(err, context.Canceled) {
		t.Fatalf("Upload = %v, want context.Canceled", err)
	}
	h.expectNothingSent()
}

func TestUploadCtxDoneSendsNothing(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	h := newUploadHarness(t, Options{}, nil)
	h.start(ctx, bytes.NewReader([]byte("x")), 1)
	if err := h.wait(); !errors.Is(err, context.Canceled) {
		t.Fatalf("Upload = %v, want context.Canceled", err)
	}
	h.expectNothingSent()
}

func TestUploadBadDir(t *testing.T) {
	t.Parallel()
	h := newUploadHarness(t, Options{}, nil)
	err := h.c.Upload(t.Context(), `\LEADING`, "T.NC", bytes.NewReader(nil), 0, nil)
	if !errors.Is(err, ErrBadUploadDir) {
		t.Fatalf("Upload = %v, want ErrBadUploadDir", err)
	}
	h.expectNothingSent()
}

func TestUploadStartSendFailures(t *testing.T) {
	t.Parallel()
	writeErr := errors.New("write boom")
	for _, tc := range []struct {
		name   string
		failAt int
	}{
		{"first send", 1},
		{"resend", 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var starts int
			h := newUploadHarness(t, Options{}, func(req Request) error {
				if _, ok := req.(UploadStartRequest); ok {
					starts++
					if starts == tc.failAt {
						return writeErr
					}
				}
				return nil
			})
			h.start(t.Context(), bytes.NewReader([]byte("x")), 1)
			if tc.failAt > 1 {
				h.expectStart()
				h.settle()
				h.clk.Advance(time.Second)
			}
			if err := h.wait(); !errors.Is(err, ErrSend) || !errors.Is(err, writeErr) {
				t.Fatalf("Upload = %v, want ErrSend wrapping %v", err, writeErr)
			}
			h.expectNothingSent()
		})
	}
}

func TestUploadZeroBytesSendsOnlyStart(t *testing.T) {
	t.Parallel()
	h := newUploadHarness(t, Options{}, nil)
	h.start(t.Context(), bytes.NewReader(nil), 0)
	if sr := h.expectStart(); sr.Size != 0 || sr.Name != "T.NC" || sr.Path != `\` {
		t.Fatalf("start = %+v", sr)
	}
	h.ackStart(StartOK)
	h.expectProgress(0, 0)
	if err := h.wait(); err != nil {
		t.Fatalf("Upload = %v, want nil", err)
	}
	h.expectNothingSent()
}

func TestUploadNilProgress(t *testing.T) {
	t.Parallel()
	h := newUploadHarness(t, Options{}, nil)
	data := chunkData(2)
	go func() { h.result <- h.c.Upload(t.Context(), "", "T.NC", bytes.NewReader(data), int64(len(data)), nil) }()
	h.expectStart()
	h.ackStart(StartOK)
	h.expectChunks(0)
	h.ackChunk(ChunkOK, 1)
	h.expectChunks(1)
	h.ackChunk(ChunkOK, 2)
	if err := h.wait(); err != nil {
		t.Fatalf("Upload = %v, want nil", err)
	}
}

func TestUploadChunkBytesAndWindowOpens(t *testing.T) {
	t.Parallel()
	h := newUploadHarness(t, Options{}, nil)
	data := chunkData(6)
	size := int64(len(data))
	h.beginChunks(t.Context(), data)

	for i := range uint32(3) {
		req := h.next()
		cr, ok := req.(UploadChunkRequest)
		if !ok || cr.Index != i {
			t.Fatalf("sent %#v, want chunk %d", req, i)
		}
		off := int(i) * MaxChunkData
		if !bytes.Equal(cr.Data, data[off:off+MaxChunkData]) {
			t.Fatalf("chunk %d carries the wrong bytes", i)
		}
		h.settle()
		h.expectNothingSent()
		h.clk.Advance(10 * time.Millisecond)
		h.ackChunk(ChunkOK, i+1)
		h.expectProgress(int64(i+1)*MaxChunkData, size)
	}

	// Three clean ACKs: the window is now 2.
	h.expectChunks(3, 4)
	h.settle()
	h.expectNothingSent()

	h.ackChunk(ChunkOK, 4)
	h.expectProgress(4*MaxChunkData, size)
	last := h.next()
	cr, ok := last.(UploadChunkRequest)
	if !ok || cr.Index != 5 || !bytes.Equal(cr.Data, data[5*MaxChunkData:]) {
		t.Fatalf("sent %#v, want the short final chunk 5", last)
	}

	// One cumulative ACK covers both chunks still in flight.
	h.ackChunk(ChunkOK, 6)
	h.expectProgress(size, size)
	if err := h.wait(); err != nil {
		t.Fatalf("Upload = %v, want nil", err)
	}
	h.expectNothingSent()
}

func TestUploadRetransmitDropsWindowToOne(t *testing.T) {
	t.Parallel()
	h := newUploadHarness(t, Options{}, nil)
	data := chunkData(7)
	size := int64(len(data))
	h.beginChunks(t.Context(), data)

	// Three ACKs, each 20ms after its chunk: SRTT 60 -> 50 -> 42.5 ->
	// 36.875ms, so the retransmit timeout is 73.75ms.
	for i := range uint32(3) {
		h.expectChunks(i)
		h.settle()
		h.clk.Advance(20 * time.Millisecond)
		h.ackChunk(ChunkOK, i+1)
		h.expectProgress(int64(i+1)*MaxChunkData, size)
	}
	h.expectChunks(3, 4)
	h.advanceExactlyf(73750*time.Microsecond, "retransmitted before the timeout")
	h.expectChunks(3, 4)

	// The window is back to 1: accepting chunk 3 frees one slot, but chunk
	// 4 still fills it.
	h.ackChunk(ChunkOK, 4)
	h.expectProgress(4*MaxChunkData, size)
	h.settle()
	h.expectNothingSent()

	h.ackChunk(ChunkOK, 5)
	h.expectProgress(5*MaxChunkData, size)
	h.expectChunks(5)
	h.settle()
	h.expectNothingSent()
	h.ackChunk(ChunkOK, 6)
	h.expectProgress(6*MaxChunkData, size)
	h.expectChunks(6)
	h.ackChunk(ChunkOK, 7)
	h.expectProgress(size, size)
	if err := h.wait(); err != nil {
		t.Fatalf("Upload = %v, want nil", err)
	}
}

func TestUploadRetransmittedChunkGivesNoSample(t *testing.T) {
	t.Parallel()
	h := newUploadHarness(t, Options{}, nil)
	data := chunkData(3)
	size := int64(len(data))
	h.beginChunks(t.Context(), data)

	// Chunk 0 is lost; its retransmit is acknowledged at once. With no
	// sample taken SRTT stays 60ms, so chunk 1 retransmits after 120ms.
	h.expectChunks(0)
	h.settle()
	h.clk.Advance(120 * time.Millisecond)
	h.expectChunks(0)
	h.ackChunk(ChunkOK, 1)
	h.expectProgress(MaxChunkData, size)
	h.expectChunks(1)
	h.advanceExactlyf(120*time.Millisecond, "retransmitted before the timeout")
	h.expectChunks(1)

	h.ackChunk(ChunkOK, 2)
	h.expectProgress(2*MaxChunkData, size)
	h.expectChunks(2)
	h.ackChunk(ChunkOK, 3)
	h.expectProgress(size, size)
	if err := h.wait(); err != nil {
		t.Fatalf("Upload = %v, want nil", err)
	}
}

func TestUploadSampleShortensRetransmitTimeout(t *testing.T) {
	t.Parallel()
	h := newUploadHarness(t, Options{}, nil)
	data := chunkData(2)
	size := int64(len(data))
	h.beginChunks(t.Context(), data)

	// A 20ms sample: SRTT (3*60 + 20)/4 = 50ms, timeout 100ms.
	h.expectChunks(0)
	h.settle()
	h.clk.Advance(20 * time.Millisecond)
	h.ackChunk(ChunkOK, 1)
	h.expectProgress(MaxChunkData, size)
	h.expectChunks(1)
	h.advanceExactlyf(100*time.Millisecond, "retransmitted before the timeout")
	h.expectChunks(1)
	h.ackChunk(ChunkOK, 2)
	h.expectProgress(size, size)
	if err := h.wait(); err != nil {
		t.Fatalf("Upload = %v, want nil", err)
	}
}

func TestUploadAckCountClampedToChunksSent(t *testing.T) {
	t.Parallel()
	h := newUploadHarness(t, Options{}, nil)
	data := chunkData(3)
	size := int64(len(data))
	h.beginChunks(t.Context(), data)

	h.expectChunks(0)
	h.ackChunk(ChunkOK, 100)
	h.expectProgress(MaxChunkData, size)
	h.expectChunks(1)
	h.ackChunk(ChunkOK, 2)
	h.expectProgress(2*MaxChunkData, size)
	h.expectChunks(2)
	h.ackChunk(ChunkOK, 3)
	h.expectProgress(size, size)
	if err := h.wait(); err != nil {
		t.Fatalf("Upload = %v, want nil", err)
	}
}

func TestUploadStaleAckIsActivity(t *testing.T) {
	t.Parallel()
	h := newUploadHarness(t, Options{StallTimeout: 100 * time.Millisecond}, nil)
	data := chunkData(2)
	size := int64(len(data))
	h.beginChunks(t.Context(), data)

	h.expectChunks(0)
	h.settle()
	h.clk.Advance(50 * time.Millisecond)
	h.ackChunk(ChunkOK, 0)
	waitFor(t, h.stale)

	// Without that ACK the stall would have fired at 100ms; now the chunk
	// retransmits at 120ms and the stall waits for 150ms.
	h.advanceExactlyf(70*time.Millisecond, "stale ACK did not count as activity")
	h.expectChunks(0)

	h.ackChunk(ChunkOK, 1)
	h.expectProgress(MaxChunkData, size)
	h.expectChunks(1)
	h.settle()

	// A duplicate of the ACK just processed, at 150ms, also counts without
	// advancing: the stall moves from 220ms to 250ms, so chunk 1's
	// retransmit at 240ms comes first.
	h.clk.Advance(30 * time.Millisecond)
	h.ackChunk(ChunkOK, 1)
	waitFor(t, h.stale)
	h.settle()
	h.clk.Advance(70 * time.Millisecond)
	if h.clk.Pending() != 1 {
		t.Fatal("duplicate ACK did not count as activity")
	}
	h.clk.Advance(20 * time.Millisecond)
	h.expectChunks(1)
	h.settle()
	h.clk.Advance(10 * time.Millisecond)
	if err := h.expectAborts(); !errors.Is(err, ErrNoResponse) {
		t.Fatalf("Upload = %v, want ErrNoResponse", err)
	}
}

// slowReaderAt serves data, calling during in place of the time a slow
// disk or share takes to read the chunk at offset slowAt.
type slowReaderAt struct {
	data   []byte
	slowAt int64
	during func()
}

func (s slowReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if off == s.slowAt {
		s.during()
	}
	return bytes.NewReader(s.data).ReadAt(p, off)
}

func TestUploadSlowReadIsNotAStall(t *testing.T) {
	t.Parallel()
	h := newUploadHarness(t, Options{}, nil)
	data := chunkData(2)
	size := int64(len(data))
	h.start(t.Context(), slowReaderAt{data: data, slowAt: MaxChunkData, during: func() {
		h.clk.Advance(20 * time.Second)
	}}, size)
	h.expectStart()
	h.ackStart(StartOK)
	h.expectProgress(0, size)
	h.expectChunks(0)
	h.ackChunk(ChunkOK, 1)
	h.expectProgress(MaxChunkData, size)
	h.expectChunks(1)
	h.ackChunk(ChunkOK, 2)
	h.expectProgress(size, size)
	if err := h.wait(); err != nil {
		t.Fatalf("Upload = %v, want nil", err)
	}
	h.expectNothingSent()
}

func TestUploadAckQueuedDuringReadCountsFirst(t *testing.T) {
	t.Parallel()
	h := newUploadHarness(t, Options{}, nil)
	data := chunkData(6)
	size := int64(len(data))
	h.start(t.Context(), slowReaderAt{data: data, slowAt: 5 * MaxChunkData, during: func() {
		h.ackChunk(ChunkOK, 5)
		h.clk.Advance(time.Second)
	}}, size)
	h.expectStart()
	h.ackStart(StartOK)
	h.expectProgress(0, size)
	for i := range uint32(3) {
		h.expectChunks(i)
		h.ackChunk(ChunkOK, i+1)
		h.expectProgress(int64(i+1)*MaxChunkData, size)
	}
	h.expectChunks(3, 4)
	h.ackChunk(ChunkOK, 4)
	h.expectProgress(4*MaxChunkData, size)

	// Chunk 4's ACK arrived while chunk 5 was being read, by then a second
	// after chunk 4 was sent: counting it first means chunk 4 is not resent.
	h.expectChunks(5)
	h.expectProgress(5*MaxChunkData, size)
	h.settle()
	h.expectNothingSent()
	h.ackChunk(ChunkOK, 6)
	h.expectProgress(size, size)
	if err := h.wait(); err != nil {
		t.Fatalf("Upload = %v, want nil", err)
	}
	h.expectNothingSent()
}

func TestUploadStallGivesUpThenAborts(t *testing.T) {
	t.Parallel()
	h := newUploadHarness(t, Options{StallTimeout: 300 * time.Millisecond}, nil)
	data := chunkData(2)
	h.beginChunks(t.Context(), data)
	t0 := h.clk.Now()

	h.expectChunks(0)
	for _, at := range []time.Duration{120 * time.Millisecond, 240 * time.Millisecond} {
		h.settle()
		h.clk.Advance(at - h.clk.Since(t0))
		h.expectChunks(0)
	}
	h.advanceExactlyf(60*time.Millisecond, "gave up early")
	if err := h.expectAborts(); !errors.Is(err, ErrNoResponse) {
		t.Fatalf("Upload = %v, want ErrNoResponse", err)
	}
	if got := h.clk.Since(t0); got != 300*time.Millisecond+2*h.c.abortInterval {
		t.Fatalf("finished at %v", got)
	}
}

func TestUploadDefaultStallTimeout(t *testing.T) {
	t.Parallel()
	h := newUploadHarness(t, Options{}, nil)
	h.beginChunks(t.Context(), chunkData(1))
	t0 := h.clk.Now()

	h.expectChunks(0)
	for h.clk.Since(t0) < 15*time.Second-120*time.Millisecond {
		h.settle()
		h.clk.Advance(120 * time.Millisecond)
		h.expectChunks(0)
	}
	h.settle()
	h.clk.Advance(15*time.Second - h.clk.Since(t0))
	if err := h.expectAborts(); !errors.Is(err, ErrNoResponse) {
		t.Fatalf("Upload = %v, want ErrNoResponse", err)
	}
}

func TestUploadChunkErrorResults(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		result byte
		want   error
	}{
		{"usb write", ChunkUSBWriteError, ErrUSBWrite},
		{"canceled", ChunkCanceled, ErrCanceled},
		{"other", 0x42, ErrTransfer},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newUploadHarness(t, Options{}, nil)
			h.beginChunks(t.Context(), chunkData(2))
			h.expectChunks(0)
			h.ackChunk(tc.result, 1)
			if err := h.expectAborts(); !errors.Is(err, tc.want) {
				t.Fatalf("Upload = %v, want %v", err, tc.want)
			}
		})
	}
}

type fixedReaderAt struct {
	n   func(p []byte) int
	err error
}

func (f fixedReaderAt) ReadAt(p []byte, _ int64) (int, error) {
	n := f.n(p)
	for i := range n {
		p[i] = 'x'
	}
	return n, f.err
}

func TestUploadReadResults(t *testing.T) {
	t.Parallel()
	boom := errors.New("disk exploded")
	full := func(p []byte) int { return len(p) }
	short := func(p []byte) int { return len(p) - 1 }
	for _, tc := range []struct {
		name string
		r    fixedReaderAt
		want error
	}{
		{"full read with EOF", fixedReaderAt{full, io.EOF}, nil},
		{"full read with error", fixedReaderAt{full, boom}, boom},
		{"short read with EOF", fixedReaderAt{short, io.EOF}, io.EOF},
		{"short read without error", fixedReaderAt{short, nil}, io.ErrUnexpectedEOF},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newUploadHarness(t, Options{}, nil)
			h.start(t.Context(), tc.r, 10)
			h.expectStart()
			h.ackStart(StartOK)
			h.expectProgress(0, 10)
			if tc.want == nil {
				h.expectChunks(0)
				h.ackChunk(ChunkOK, 1)
				h.expectProgress(10, 10)
				if err := h.wait(); err != nil {
					t.Fatalf("Upload = %v, want nil", err)
				}
				return
			}
			if err := h.expectAborts(); !errors.Is(err, ErrRead) || !errors.Is(err, tc.want) {
				t.Fatalf("Upload = %v, want ErrRead wrapping %v", err, tc.want)
			}
		})
	}
}

func TestUploadChunkSendFailures(t *testing.T) {
	t.Parallel()
	writeErr := errors.New("write boom")
	for _, tc := range []struct {
		name       string
		failAt     int
		failAborts bool
	}{
		{"first send", 1, false},
		{"retransmit", 2, false},
		{"first send, aborts fail too", 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var chunks int
			abortErrs := make(chan struct{}, abortNotifications)
			h := newUploadHarness(t, Options{}, func(req Request) error {
				switch req.(type) {
				case UploadChunkRequest:
					chunks++
					if chunks == tc.failAt {
						return writeErr
					}
				case UploadAbortRequest:
					if tc.failAborts {
						abortErrs <- struct{}{}
						return writeErr
					}
				default:
				}
				return nil
			})
			h.beginChunks(t.Context(), chunkData(1))
			if tc.failAt > 1 {
				h.expectChunks(0)
				h.settle()
				h.clk.Advance(120 * time.Millisecond)
			}
			var err error
			if tc.failAborts {
				for i := range abortNotifications {
					if i > 0 {
						h.settle()
						h.clk.Advance(h.c.abortInterval)
					}
					waitFor(t, abortErrs)
				}
				err = h.wait()
			} else {
				err = h.expectAborts()
			}
			if !errors.Is(err, ErrSend) || !errors.Is(err, writeErr) {
				t.Fatalf("Upload = %v, want ErrSend wrapping %v", err, writeErr)
			}
		})
	}
}

func TestUploadAckArrivingDuringSend(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		result byte
		want   error
	}{
		{"completes the transfer", ChunkOK, nil},
		{"fails the transfer", ChunkUSBWriteError, ErrUSBWrite},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var h *uploadHarness
			h = newUploadHarness(t, Options{}, func(req Request) error {
				if _, ok := req.(UploadChunkRequest); ok {
					h.ackChunk(tc.result, 1)
				}
				return nil
			})
			data := chunkData(1)
			h.beginChunks(t.Context(), data)
			h.expectChunks(0)
			var err error
			if tc.want == nil {
				h.expectProgress(int64(len(data)), int64(len(data)))
				err = h.wait()
			} else {
				err = h.expectAborts()
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("Upload = %v, want %v", err, tc.want)
			}
			h.expectNothingSent()
		})
	}
}

func TestUploadKeepsItsControllerAcrossConnect(t *testing.T) {
	t.Parallel()
	h := newUploadHarness(t, Options{}, nil)
	data := chunkData(2)
	size := int64(len(data))
	other := &net.UDPAddr{IP: net.ParseIP("192.0.2.99"), Port: ControllerPort}

	h.start(t.Context(), bytes.NewReader(data), size)
	h.expectStart()
	h.c.setConnection(other, Identity{})
	h.c.handlePacket(StartAck{Result: StartNoUSB}.Encode(), other)
	h.ackStart(StartOK)
	h.expectProgress(0, size)
	h.expectChunks(0)
	h.c.handlePacket(ChunkAck{Result: ChunkUSBWriteError}.Encode(), other)
	h.ackChunk(ChunkOK, 1)
	h.expectProgress(MaxChunkData, size)
	h.expectChunks(1)
	h.ackChunk(ChunkOK, 2)
	h.expectProgress(size, size)
	if err := h.wait(); err != nil {
		t.Fatalf("Upload = %v, want nil", err)
	}
	h.expectNothingSent()
}

func TestConnectRepliesAreNotSourceFiltered(t *testing.T) {
	t.Parallel()
	h := newUploadHarness(t, Options{}, nil)
	target := &net.UDPAddr{IP: net.ParseIP("192.0.2.10"), Port: ControllerPort}
	elsewhere := &net.UDPAddr{IP: net.ParseIP("192.0.2.99"), Port: ControllerPort}
	done := make(chan error, 1)
	go func() {
		_, _, err := h.c.Connect(t.Context(), target)
		done <- err
	}()

	if req, ok := h.next().(DiscoveryRequest); !ok {
		t.Fatalf("first request = %#v, want DiscoveryRequest", req)
	}
	h.c.handlePacket(Identity{Serial: 9, Version: "v"}.Encode(), elsewhere)
	if req, ok := h.next().(ConfigRequest); !ok {
		t.Fatalf("second request = %#v, want ConfigRequest", req)
	}
	h.c.handlePacket(ConfigReply{Serial: 9}.Encode(), elsewhere)

	if err := waitFor(t, done); err != nil {
		t.Fatalf("Connect: %v", err)
	}
}

func TestHandlePacketSourceFiltering(t *testing.T) {
	t.Parallel()
	controller := &net.UDPAddr{IP: net.ParseIP("192.0.2.10"), Port: ControllerPort}
	sameIP := &net.UDPAddr{IP: net.ParseIP("192.0.2.10").To16(), Port: 4242}
	foreign := &net.UDPAddr{IP: net.ParseIP("192.0.2.99"), Port: ControllerPort}
	nonUDP := &net.TCPAddr{IP: net.ParseIP("192.0.2.10"), Port: ControllerPort}

	replies := []struct {
		typ byte
		pkt []byte
	}{
		{TypeDiscovery, Identity{Serial: 9, Version: "v"}.Encode()},
		{TypeConfig, ConfigReply{Serial: 9}.Encode()},
		{TypeTool, ToolRecord{Index: 1, Name: "drill"}.Encode()},
		{TypeUploadStart, StartAck{}.Encode()},
		{TypeUploadChunk, ChunkAck{Accepted: 1}.Encode()},
	}

	// delivered reports whether pkt from addr reached a waiter for typ
	// bound to from.
	delivered := func(c *Client, typ byte, from *net.UDPAddr, pkt []byte, addr net.Addr) bool {
		ch, cancel := c.expect(typ, from, anyReply)
		defer cancel()
		c.handlePacket(pkt, addr)
		select {
		case <-ch:
			return true
		default:
			return false
		}
	}
	// statusDelivered reports whether a status from addr reached Run's
	// input channel.
	statusDelivered := func(c *Client, addr net.Addr) bool {
		c.handlePacket(Status{Progress: 7}.Encode(), addr)
		select {
		case <-c.statusIn:
			return true
		default:
			return false
		}
	}

	c := newTestClient(t, clock.Real{})
	for _, r := range replies {
		for _, addr := range []net.Addr{foreign, nonUDP} {
			if !delivered(c, r.typ, nil, r.pkt, addr) {
				t.Errorf("type 0x%02X from %v was dropped by a waiter bound to no source", r.typ, addr)
			}
			if delivered(c, r.typ, controller, r.pkt, addr) {
				t.Errorf("type 0x%02X from %v reached a waiter bound to %v", r.typ, addr, controller)
			}
		}
		if !delivered(c, r.typ, controller, r.pkt, sameIP) {
			t.Errorf("type 0x%02X from the controller's IP was dropped", r.typ)
		}
	}

	if !statusDelivered(c, nonUDP) {
		t.Error("before Connect: status from a non-UDP address was dropped")
	}
	c.setConnection(controller, Identity{})
	if statusDelivered(c, foreign) {
		t.Error("status from a foreign IP was delivered")
	}
	if statusDelivered(c, nonUDP) {
		t.Error("status from a non-UDP address was delivered")
	}
	if !statusDelivered(c, sameIP) {
		t.Error("status from the controller's IP was dropped")
	}
}
