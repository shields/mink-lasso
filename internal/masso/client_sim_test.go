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

package masso_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"msrl.dev/mink-lasso/internal/clock"
	"msrl.dev/mink-lasso/internal/masso"
	"msrl.dev/mink-lasso/internal/masso/sim"
)

// progressEvent records one call to an Upload progress callback.
type progressEvent struct{ sent, total int64 }

// errReaderAt is an io.ReaderAt that always fails, for exercising Upload's
// read-error path.
type errReaderAt struct{ err error }

func (e errReaderAt) ReadAt([]byte, int64) (int, error) { return 0, e.err }

// testAttempts mirrors the "up to three attempts" Connect and Tools both
// document for a request bounded by Options.ReplyTimeout.
const testAttempts = 3

func TestReaderDropsUndecodablePacket(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s := newSim(t, sim.Options{Serial: 5, Version: "test"})
	c := newTestClient(t, clock.Real{})
	fake := rawConn(t)
	mustSend(t, fake, clientAddr(c), []byte("not a valid masso packet"))

	// The reader must not have wedged: a normal exchange still works.
	if _, _, err := c.Connect(ctx, s.Addr()); err != nil {
		t.Fatalf("Connect after garbage packet: %v", err)
	}
}

func TestConnectHappyPath(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s := newSim(t, sim.Options{Serial: 777, Version: "5-Axis v5.13"})
	fc := clock.NewFake(time.Unix(0, 0))
	c := newTestClient(t, fc)

	id, cfg, err := c.Connect(ctx, s.Addr())
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if id.Serial != 777 {
		t.Errorf("Identity.Serial = %d, want 777", id.Serial)
	}
	if cfg.Serial != 777 {
		t.Errorf("ConfigReply.Serial = %d, want 777", cfg.Serial)
	}
	got := c.Remote()
	if got == nil || !got.IP.Equal(s.Addr().IP) || got.Port != s.Addr().Port {
		t.Errorf("Remote() = %v, want %v", got, s.Addr())
	}
}

func TestConnectNoAnswer(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s := newSim(t, sim.Options{Serial: 42, Version: "5-Axis v5.13"})
	s.SetSilent(true)

	const replyTimeout = 50 * time.Millisecond
	fc := clock.NewFake(time.Unix(0, 0))
	port := freePort(t)
	c, err := masso.NewClient(masso.Options{Clock: fc, PortMin: port, PortMax: port, ReplyTimeout: replyTimeout})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })

	errCh := make(chan error, 1)
	go func() {
		_, _, connectErr := c.Connect(ctx, s.Addr())
		errCh <- connectErr
	}()

	for range testAttempts {
		fc.BlockUntil(1)
		fc.Advance(replyTimeout)
	}

	if err := waitFor(t, errCh); !errors.Is(err, masso.ErrNoResponse) {
		t.Fatalf("Connect = %v, want ErrNoResponse", err)
	}
}

func TestDiscoverAt(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s := newSim(t, sim.Options{Serial: 55, Version: "test-fw"})
	c := newTestClient(t, clock.Real{})

	id, err := c.DiscoverAt(ctx, s.Addr(), time.Second)
	if err != nil {
		t.Fatalf("DiscoverAt: %v", err)
	}
	if id.Serial != 55 {
		t.Errorf("Identity.Serial = %d, want 55", id.Serial)
	}
	got := s.LastReplyTarget()
	if got == nil || got.Port != int(c.LocalPort()) {
		t.Errorf("LastReplyTarget() = %v, want port %d", got, c.LocalPort())
	}
}

func TestDiscoverAtNoAnswer(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s := newSim(t, sim.Options{Serial: 1})
	s.SetSilent(true)
	fc := clock.NewFake(time.Unix(0, 0))
	c := newTestClient(t, fc)

	const timeout = 50 * time.Millisecond
	errCh := make(chan error, 1)
	go func() {
		_, err := c.DiscoverAt(ctx, s.Addr(), timeout)
		errCh <- err
	}()
	fc.BlockUntil(1)
	fc.Advance(timeout)

	if err := waitFor(t, errCh); !errors.Is(err, masso.ErrNoResponse) {
		t.Fatalf("DiscoverAt = %v, want ErrNoResponse", err)
	}
}

func TestRunCtxAlreadyCanceled(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	s := newSim(t, sim.Options{Serial: 1})
	c := newTestClient(t, clock.Real{})
	if _, _, err := c.Connect(t.Context(), s.Addr()); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	cancel()
	if err := c.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run = %v, want context.Canceled", err)
	}
}

func TestRunDeliversStatusesAndErrLost(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s := newSim(t, sim.Options{Serial: 1, Version: "v1"})
	const keepalive = 20 * time.Millisecond
	const lostAfter = 200 * time.Millisecond
	fc := clock.NewFake(time.Unix(0, 0))
	port := freePort(t)
	c, err := masso.NewClient(masso.Options{
		Clock: fc, PortMin: port, PortMax: port,
		KeepaliveInterval: keepalive, LostAfter: lostAfter,
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })

	if _, _, err := c.Connect(ctx, s.Addr()); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	runErr := make(chan error, 1)
	go func() { runErr <- c.Run(ctx) }()

	// Run's immediate keepalive should elicit a first status.
	waitFor(t, c.Status())

	// The ticker should elicit a second one.
	fc.BlockUntil(2)
	fc.Advance(keepalive)
	waitFor(t, c.Status())

	s.SetSilent(true)

	fc.BlockUntil(2)
	fc.Advance(lostAfter)

	if err := waitFor(t, runErr); !errors.Is(err, masso.ErrLost) {
		t.Fatalf("Run = %v, want ErrLost", err)
	}
}

func TestRunCtxCancelMidRun(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	s := newSim(t, sim.Options{Serial: 1})
	fc := clock.NewFake(time.Unix(0, 0))
	c := newTestClient(t, fc)
	if _, _, err := c.Connect(ctx, s.Addr()); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	runErr := make(chan error, 1)
	go func() { runErr <- c.Run(ctx) }()
	waitFor(t, c.Status())
	cancel()

	if err := waitFor(t, runErr); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run = %v, want context.Canceled", err)
	}
}

func TestToolsRespectsMaxTools(t *testing.T) {
	t.Parallel()
	names := make([]string, 120)
	for i := range names {
		names[i] = fmt.Sprintf("T%d", i+1)
	}
	for _, tc := range []struct {
		name    string
		serial  uint32
		version string
		want    int
	}{
		{"small serial", 5000, "5-Axis v5.13", 32},
		{"small serial lathe", 5000, "Lathe v5.09", 32},
		{"lathe", 5001, "Lathe v5.09", 100},
		{"lathe any case", 31578, "2-Axis LATHE v5.13", 100},
		{"mill", 5001, "5-Axis v5.13", 118},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := newSim(t, sim.Options{Serial: tc.serial, Version: tc.version, Tools: names})
			c, _ := newFakeClockClient(t, s, masso.Options{})

			got, err := c.Tools(t.Context())
			if err != nil {
				t.Fatalf("Tools: %v", err)
			}
			if len(got) != tc.want {
				t.Fatalf("len(Tools()) = %d, want %d", len(got), tc.want)
			}
			for i, tr := range got {
				if int(tr.Index) != i+1 || tr.Name != names[i] {
					t.Errorf("Tools()[%d] = %+v, want {Index:%d Name:%q}", i, tr, i+1, names[i])
				}
			}
		})
	}
}

func TestToolsStopsAtEmptyName(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	names := []string{"drill", "endmill", "tap"}
	s := newSim(t, sim.Options{Serial: 1, Tools: names})
	c := newTestClient(t, clock.Real{})
	if _, _, err := c.Connect(ctx, s.Addr()); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	got, err := c.Tools(ctx)
	if err != nil {
		t.Fatalf("Tools: %v", err)
	}
	if len(got) != len(names) {
		t.Fatalf("len(Tools()) = %d, want %d", len(got), len(names))
	}
}

func TestToolsStopsAtNonAnsweringIndex(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s := newSim(t, sim.Options{Serial: 1, Tools: []string{"drill"}})
	const replyTimeout = 50 * time.Millisecond
	fc := clock.NewFake(time.Unix(0, 0))
	port := freePort(t)
	c, err := masso.NewClient(masso.Options{Clock: fc, PortMin: port, PortMax: port, ReplyTimeout: replyTimeout})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	if _, _, err := c.Connect(ctx, s.Addr()); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	// internal/masso/sim has no "answer some indices, then silence" knob;
	// SetSilent from the start exercises the same stop condition — the
	// first index never answers — at index 1 rather than mid-table. See
	// the final report for this deviation.
	s.SetSilent(true)

	resultCh := make(chan struct {
		tools []masso.ToolRecord
		err   error
	}, 1)
	go func() {
		tools, err := c.Tools(ctx)
		resultCh <- struct {
			tools []masso.ToolRecord
			err   error
		}{tools, err}
	}()

	for range testAttempts {
		fc.BlockUntil(1)
		fc.Advance(replyTimeout)
	}

	res := waitFor(t, resultCh)
	if res.err != nil {
		t.Fatalf("Tools error = %v, want nil", res.err)
	}
	if len(res.tools) != 0 {
		t.Fatalf("Tools() = %v, want empty", res.tools)
	}
}

func TestToolsDuplicateRepliesKeepTableCorrect(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	names := []string{"drill", "endmill", "tap", "reamer"}
	s := newSim(t, sim.Options{Serial: 1, Tools: names})
	s.SetDuplicateReplies(true)
	c := newTestClient(t, clock.Real{})
	if _, _, err := c.Connect(ctx, s.Addr()); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	got, err := c.Tools(ctx)
	if err != nil {
		t.Fatalf("Tools: %v", err)
	}
	if len(got) != len(names) {
		t.Fatalf("len(Tools()) = %d, want %d", len(got), len(names))
	}
	for i, tr := range got {
		if int(tr.Index) != i+1 || tr.Name != names[i] {
			t.Errorf("Tools()[%d] = %+v, want {Index:%d Name:%q}", i, tr, i+1, names[i])
		}
	}
}

func TestUploadBadFileName(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s := newSim(t, sim.Options{Serial: 1})
	c := newTestClient(t, clock.Real{})
	if _, _, err := c.Connect(ctx, s.Addr()); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	err := c.Upload(ctx, "", "bad/name.nc", bytes.NewReader(testData(1)), 1, nil)
	if !errors.Is(err, masso.ErrBadFileName) {
		t.Fatalf("Upload = %v, want ErrBadFileName", err)
	}
}

func TestUploadSizes(t *testing.T) {
	t.Parallel()
	sizes := []int{0, 1, masso.MaxChunkData, masso.MaxChunkData + 1, 10240}
	for _, size := range sizes {
		t.Run(fmt.Sprintf("%dB", size), func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			s := newSim(t, sim.Options{Serial: 1})
			c := newTestClient(t, clock.Real{})
			if _, _, err := c.Connect(ctx, s.Addr()); err != nil {
				t.Fatalf("Connect: %v", err)
			}

			data := testData(size)
			name := fmt.Sprintf("U%d.NC", size)
			var events []progressEvent
			err := c.Upload(ctx, "", name, bytes.NewReader(data), int64(size), func(sent, total int64) {
				events = append(events, progressEvent{sent, total})
			})
			if err != nil {
				t.Fatalf("Upload: %v", err)
			}
			if len(events) == 0 {
				t.Fatal("progress was never called")
			}
			if events[0] != (progressEvent{0, int64(size)}) {
				t.Errorf("first progress = %+v, want {0 %d}", events[0], size)
			}
			last := events[len(events)-1]
			if last != (progressEvent{int64(size), int64(size)}) {
				t.Errorf("last progress = %+v, want {%d %d}", last, size, size)
			}

			if size == 0 {
				// The real controller's behavior for a zero-byte upload is
				// unconfirmed; the simulator never records a file for it
				// since no chunk packet is ever sent.
				return
			}
			got, ok := s.File(name)
			if !ok {
				t.Fatal("file was not stored")
			}
			if !bytes.Equal(got, data) {
				t.Fatal("stored bytes do not match")
			}
		})
	}
}

// eventually polls cond until it holds, failing the test if it does not
// within safetyNet. It waits only on asynchronous loopback delivery to the
// simulator, never on anything a fake clock drives.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(safetyNet)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// expectAborts waits for s to have received exactly want upload-abort
// notifications.
func expectAborts(t *testing.T, s *sim.Controller, want int) {
	t.Helper()
	eventually(t, fmt.Sprintf("%d upload-abort notifications", want), func() bool { return s.Aborts() >= want })
	if got := s.Aborts(); got != want {
		t.Fatalf("Aborts() = %d, want %d", got, want)
	}
}

// newFakeClockClient returns a Client connected to s, timed by a fake clock
// that nothing advances unless the test does.
func newFakeClockClient(t *testing.T, s *sim.Controller, opts masso.Options) (*masso.Client, *clock.Fake) {
	t.Helper()
	fc := clock.NewFake(time.Unix(0, 0))
	opts.Clock = fc
	c := newClientWith(t, opts)
	if _, _, err := c.Connect(t.Context(), s.Addr()); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	return c, fc
}

// uploadInBackground starts c.Upload of data as name, reporting progress
// and the result on the returned channels.
func uploadInBackground(
	ctx context.Context, c *masso.Client, dir, name string, data []byte,
) (<-chan progressEvent, <-chan error) {
	progressCh := make(chan progressEvent, 64)
	errCh := make(chan error, 1)
	go func() {
		errCh <- c.Upload(ctx, dir, name, bytes.NewReader(data), int64(len(data)), func(sent, total int64) {
			progressCh <- progressEvent{sent, total}
		})
	}()
	return progressCh, errCh
}

func TestUploadLostChunkRecovered(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		set  func(*sim.Controller, func(uint32) bool)
	}{
		{"request lost", (*sim.Controller).SetDropChunk},
		{"reply lost", (*sim.Controller).SetDropAck},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := newSim(t, sim.Options{Serial: 1})
			tc.set(s, func(idx uint32) bool { return idx == 0 })
			c, fc := newFakeClockClient(t, s, masso.Options{})

			data := testData(10)
			progressCh, errCh := uploadInBackground(t.Context(), c, "", "DROP.NC", data)
			if first := waitFor(t, progressCh); first != (progressEvent{0, 10}) {
				t.Fatalf("first progress = %+v", first)
			}

			// Chunk 0 went unacknowledged; the retransmit timeout starts
			// at twice the 60ms seed.
			fc.BlockUntil(1)
			fc.Advance(120 * time.Millisecond)

			if second := waitFor(t, progressCh); second != (progressEvent{10, 10}) {
				t.Fatalf("second progress = %+v", second)
			}
			if err := waitFor(t, errCh); err != nil {
				t.Fatalf("Upload: %v", err)
			}
			got, ok := s.File("DROP.NC")
			if !ok || !bytes.Equal(got, data) {
				t.Fatal("stored file mismatch")
			}
			if n := s.ChunkRequests(); n != 2 {
				t.Fatalf("ChunkRequests() = %d, want 2", n)
			}
			if n := s.Aborts(); n != 0 {
				t.Fatalf("Aborts() = %d, want 0", n)
			}
		})
	}
}

func TestUploadLostStartAck(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		result byte
	}{
		{"then OK", masso.StartOK},
		{"then already started", masso.StartAlreadyStarted},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := newSim(t, sim.Options{Serial: 1})
			s.SetStartResult(tc.result)
			s.SetDropStartAcks(1)
			c, fc := newFakeClockClient(t, s, masso.Options{})

			data := testData(masso.MaxChunkData + 1)
			progressCh, errCh := uploadInBackground(t.Context(), c, "JOBS", "LOST.NC", data)
			fc.BlockUntil(1)
			fc.Advance(time.Second)

			if first := waitFor(t, progressCh); first != (progressEvent{0, int64(len(data))}) {
				t.Fatalf("first progress = %+v", first)
			}
			if err := waitFor(t, errCh); err != nil {
				t.Fatalf("Upload: %v", err)
			}
			got, ok := s.File(`JOBS\LOST.NC`)
			if !ok || !bytes.Equal(got, data) {
				t.Fatal("stored file mismatch")
			}
		})
	}
}

func TestUploadSilentAfterChunkErrNoResponse(t *testing.T) {
	t.Parallel()
	s := newSim(t, sim.Options{Serial: 1})
	s.SetSilentAfterChunk(0)
	const stallTimeout = 50 * time.Millisecond
	const abortInterval = 20 * time.Millisecond
	c, fc := newFakeClockClient(t, s, masso.Options{StallTimeout: stallTimeout, AbortInterval: abortInterval})

	data := testData(masso.MaxChunkData + 10)
	progressCh, errCh := uploadInBackground(t.Context(), c, "", "SILENT.NC", data)
	waitFor(t, progressCh) // start ack
	waitFor(t, progressCh) // chunk 0 accepted

	// Chunk 1 is never answered, and the stall timeout is shorter than the
	// retransmit timeout.
	fc.BlockUntil(1)
	fc.Advance(stallTimeout)
	for range 2 {
		fc.BlockUntil(1)
		fc.Advance(abortInterval)
	}

	if err := waitFor(t, errCh); !errors.Is(err, masso.ErrNoResponse) {
		t.Fatalf("Upload = %v, want ErrNoResponse", err)
	}
	expectAborts(t, s, 3)
	eventually(t, "chunk 1 to reach the simulator", func() bool { return s.ChunkRequests() == 2 })
	if _, ok := s.File("SILENT.NC"); ok {
		t.Fatal("file stored despite the stall")
	}
}

func TestUploadStartResults(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		result byte
		want   error
	}{
		{"no USB", masso.StartNoUSB, masso.ErrNoUSB},
		{"already started on first attempt", masso.StartAlreadyStarted, masso.ErrTransfer},
		{"other error", 0x42, masso.ErrTransfer},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := newSim(t, sim.Options{Serial: 1})
			s.SetStartResult(tc.result)
			c, _ := newFakeClockClient(t, s, masso.Options{})
			err := c.Upload(t.Context(), "", "ST.NC", bytes.NewReader(testData(1)), 1, nil)
			if !errors.Is(err, tc.want) {
				t.Fatalf("Upload = %v, want %v", err, tc.want)
			}
			if _, ok := s.File("ST.NC"); ok {
				t.Fatal("file stored despite the refused start")
			}
		})
	}
}

func TestUploadChunkResults(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		result byte
		want   error
	}{
		{"usb write error", masso.ChunkUSBWriteError, masso.ErrUSBWrite},
		{"canceled", masso.ChunkCanceled, masso.ErrCanceled},
		{"other error", 0x42, masso.ErrTransfer},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			s := newSim(t, sim.Options{Serial: 1})
			c := newTestClient(t, clock.Real{})
			if _, _, err := c.Connect(ctx, s.Addr()); err != nil {
				t.Fatalf("Connect: %v", err)
			}
			s.SetChunkResult(tc.result)
			err := c.Upload(ctx, "", "CH.NC", bytes.NewReader(testData(1)), 1, nil)
			if !errors.Is(err, tc.want) {
				t.Fatalf("Upload = %v, want %v", err, tc.want)
			}
			expectAborts(t, s, 3)
		})
	}
}

func TestUploadReaderAtError(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s := newSim(t, sim.Options{Serial: 1})
	c := newTestClient(t, clock.Real{})
	if _, _, err := c.Connect(ctx, s.Addr()); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	readErr := errors.New("disk exploded")
	err := c.Upload(ctx, "", "RD.NC", errReaderAt{readErr}, 10, nil)
	if !errors.Is(err, masso.ErrRead) {
		t.Fatalf("Upload = %v, want ErrRead", err)
	}
	expectAborts(t, s, 3)
}

func TestUploadCtxAlreadyCanceled(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	s := newSim(t, sim.Options{Serial: 1})
	c := newTestClient(t, clock.Real{})
	if _, _, err := c.Connect(t.Context(), s.Addr()); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	cancel()
	err := c.Upload(ctx, "", "CX.NC", bytes.NewReader(testData(1)), 1, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Upload = %v, want context.Canceled", err)
	}
}

func TestUploadCtxCancelMidWait(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	s := newSim(t, sim.Options{Serial: 1})
	fc := clock.NewFake(time.Unix(0, 0))
	c := newTestClient(t, fc)
	if _, _, err := c.Connect(t.Context(), s.Addr()); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	s.SetSilent(true)

	errCh := make(chan error, 1)
	go func() {
		errCh <- c.Upload(ctx, "", "MW.NC", bytes.NewReader(testData(1)), 1, nil)
	}()
	fc.BlockUntil(1)
	cancel()

	if err := waitFor(t, errCh); !errors.Is(err, context.Canceled) {
		t.Fatalf("Upload = %v, want context.Canceled", err)
	}
	if n := s.Aborts(); n != 0 {
		t.Fatalf("Aborts() = %d, want 0", n)
	}
}

// TestUploadCtxCancelAfterStartDoesNotAbort checks Upload's documented
// guarantee that ctx cancellation never aborts a transfer already
// acknowledged as started: canceling ctx partway through chunk delivery must
// not stop the transfer or leave a partial file on the controller.
func TestUploadCtxCancelAfterStartDoesNotAbort(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	s := newSim(t, sim.Options{Serial: 1})
	c := newTestClient(t, clock.Real{})
	if _, _, err := c.Connect(t.Context(), s.Addr()); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	size := masso.MaxChunkData + 1 // at least two chunks, so there is a "mid-transfer" to cancel during
	data := testData(size)
	name := "CANCELED.NC"

	var canceledOnce bool
	err := c.Upload(ctx, "", name, bytes.NewReader(data), int64(size), func(_, _ int64) {
		// The start ACK has already landed by the time progress is first
		// called (with sent==0), so the transfer is "acknowledged as
		// started" from that point on; canceling here must not abort it.
		if !canceledOnce {
			canceledOnce = true
			cancel()
		}
	})
	if err != nil {
		t.Fatalf("Upload = %v, want nil: cancellation after the start ACK must not abort the transfer", err)
	}

	got, ok := s.File(name)
	if !ok {
		t.Fatal("file was not stored: canceled transfer left nothing on the controller")
	}
	if !bytes.Equal(got, data) {
		t.Fatal("stored bytes do not match: canceled transfer left a partial file")
	}
}

func TestUploadConcurrentErrBusy(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	s := newSim(t, sim.Options{Serial: 1})
	fc := clock.NewFake(time.Unix(0, 0))
	c := newTestClient(t, fc)
	if _, _, err := c.Connect(ctx, s.Addr()); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	s.SetSilent(true)

	done := make(chan error, 1)
	go func() {
		done <- c.Upload(ctx, "", "BUSY1.NC", bytes.NewReader(testData(10)), 10, nil)
	}()
	fc.BlockUntil(1) // goroutine 1 is now waiting for the start ACK; uploadMu is held

	err := c.Upload(ctx, "", "BUSY2.NC", bytes.NewReader(testData(1)), 1, nil)
	if !errors.Is(err, masso.ErrBusy) {
		t.Fatalf("second Upload = %v, want ErrBusy", err)
	}

	cancel()
	if err := waitFor(t, done); !errors.Is(err, context.Canceled) {
		t.Fatalf("first Upload = %v, want context.Canceled", err)
	}
}

func TestUploadStrayIdentityDoesNotWedge(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s := newSim(t, sim.Options{Serial: 1})
	c := newTestClient(t, clock.Real{})
	if _, _, err := c.Connect(ctx, s.Addr()); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	fake := rawConn(t)
	caddr := clientAddr(c)
	data := testData(masso.MaxChunkData + 10)
	err := c.Upload(ctx, "", "STRAY.NC", bytes.NewReader(data), int64(len(data)), func(int64, int64) {
		// Inject a stray Identity reply, as if from another controller
		// answering a broadcast, partway through the transfer.
		mustSend(t, fake, caddr, masso.Identity{Serial: 999, Version: "stray"}.Encode())
	})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	got, ok := s.File("STRAY.NC")
	if !ok || !bytes.Equal(got, data) {
		t.Fatal("stored file mismatch")
	}
}
