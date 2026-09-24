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
	"io"
	"log/slog"
	"net"
	"sync"
	"testing"
	"time"

	"msrl.dev/mink-lasso/internal/masso"
	"msrl.dev/mink-lasso/internal/masso/sim"
)

// newDebugLogger returns a slog.Logger, at debug level, that writes to w.
func newDebugLogger(w io.Writer) *slog.Logger {
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// newController creates a Controller and registers its Close for cleanup.
func newController(t *testing.T, opts sim.Options) *sim.Controller {
	t.Helper()
	ctrl, err := sim.New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		if err := ctrl.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return ctrl
}

// fakeConn wraps a real net.PacketConn, usually a live loopback UDP socket,
// so a test can override one or two methods while delegating everything
// else — including real timing and a real Close — to the underlying
// connection.
type fakeConn struct {
	net.PacketConn

	writeTo   func(p []byte, addr net.Addr) (int, error)
	readFrom  func(p []byte) (int, net.Addr, error)
	localAddr func() net.Addr
}

func (f *fakeConn) WriteTo(p []byte, addr net.Addr) (int, error) {
	if f.writeTo != nil {
		return f.writeTo(p, addr)
	}
	return f.PacketConn.WriteTo(p, addr)
}

func (f *fakeConn) ReadFrom(p []byte) (int, net.Addr, error) {
	if f.readFrom != nil {
		return f.readFrom(p)
	}
	return f.PacketConn.ReadFrom(p)
}

func (f *fakeConn) LocalAddr() net.Addr {
	if f.localAddr != nil {
		return f.localAddr()
	}
	return f.PacketConn.LocalAddr()
}

// badAddr is a net.Addr whose String form has no "host:port" separator,
// used to force replyAddrFor's SplitHostPort error path.
type badAddr struct{}

func (badAddr) Network() string { return "fake" }
func (badAddr) String() string  { return "not-an-address" }

// badHostAddr is a net.Addr whose String form splits into a host and port,
// but whose host is not a valid IP address, used to force replyAddrFor's
// ParseIP error path.
type badHostAddr struct{}

func (badHostAddr) Network() string { return "fake" }
func (badHostAddr) String() string  { return "not-an-ip:12345" }

// nonUDPAddr is a plausible-looking net.Addr that is not a *net.UDPAddr,
// used to force Addr's type-assertion fallback.
type nonUDPAddr struct{}

func (nonUDPAddr) Network() string { return "fake" }
func (nonUDPAddr) String() string  { return "127.0.0.1:0" }

// listenLoopback opens a real UDP socket on loopback, closing it on test
// cleanup.
func listenLoopback(t *testing.T) net.PacketConn {
	t.Helper()
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("ListenPacket: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// injectOnce wraps base so its very first ReadFrom returns pkt as if it
// came from src; every call after that delegates to base. The channel it
// returns closes right as the second call begins — which, because the
// serve loop this feeds is single-goroutine and strictly sequential, can
// only happen once the first packet's handling (including any log write)
// has completed — so a test can wait on it instead of sleeping or polling.
func injectOnce(base net.PacketConn, pkt []byte, src net.Addr) (net.PacketConn, <-chan struct{}) {
	ready := make(chan struct{})
	var once sync.Once
	delivered := false
	fc := &fakeConn{PacketConn: base}
	fc.readFrom = func(p []byte) (int, net.Addr, error) {
		if !delivered {
			delivered = true
			n := copy(p, pkt)
			return n, src, nil
		}
		once.Do(func() { close(ready) })
		return base.ReadFrom(p)
	}
	return fc, ready
}

// syncBuffer is a concurrency-safe io.Writer, for a slog handler that may
// be written from a server goroutine while a test polls its content.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// newClient returns a UDP socket bound to loopback, standing in for a
// protocol client.
func newClient(t *testing.T) *net.UDPConn {
	t.Helper()
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatalf("ListenUDP: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// clientPort returns the port conn is bound to, for advertising as a
// discovery reply port.
func clientPort(t *testing.T, conn *net.UDPConn) uint16 {
	t.Helper()
	addr, ok := conn.LocalAddr().(*net.UDPAddr)
	if !ok {
		t.Fatalf("LocalAddr() = %T, want *net.UDPAddr", conn.LocalAddr())
	}
	return uint16(addr.Port)
}

// mustWrite sends pkt to addr from conn, failing the test on error.
func mustWrite(t *testing.T, conn *net.UDPConn, addr *net.UDPAddr, pkt []byte) {
	t.Helper()
	if _, err := conn.WriteToUDP(pkt, addr); err != nil {
		t.Fatalf("WriteToUDP: %v", err)
	}
}

// discover sends a discovery request from conn, advertising conn's own
// port for replies, and returns the decoded Identity.
func discover(t *testing.T, conn *net.UDPConn, addr *net.UDPAddr) masso.Identity {
	t.Helper()
	mustWrite(t, conn, addr, masso.Discovery(clientPort(t, conn)))
	reply := readReply(t, conn)
	id, ok := reply.(masso.Identity)
	if !ok {
		t.Fatalf("reply type = %T, want Identity", reply)
	}
	return id
}

// uploadFile drives a complete start-and-chunks upload from conn to addr
// and asserts every ACK along the way.
func uploadFile(t *testing.T, conn *net.UDPConn, addr *net.UDPAddr, name string, data []byte) {
	t.Helper()
	startUpload(t, conn, addr, name, len(data))
	sent := 0
	for idx := uint32(0); sent < len(data); idx++ {
		end := min(sent+masso.MaxChunkData, len(data))
		sendChunk(t, conn, addr, idx, data[sent:end])
		if ca := readChunkAck(t, conn); ca.Result != masso.ChunkOK || ca.Accepted != idx+1 {
			t.Fatalf("chunk %d ack = %+v, want {Result:OK Accepted:%d}", idx, ca, idx+1)
		}
		sent = end
	}
}

func startUpload(t *testing.T, conn *net.UDPConn, addr *net.UDPAddr, name string, size int) {
	t.Helper()
	startPkt, err := masso.UploadStart(uint32(size), "", name)
	if err != nil {
		t.Fatalf("UploadStart: %v", err)
	}
	mustWrite(t, conn, addr, startPkt)
	if reply := readReply(t, conn); reply != (masso.StartAck{Result: masso.StartOK}) {
		t.Fatalf("start ack = %#v, want StartOK", reply)
	}
}

func sendChunk(t *testing.T, conn *net.UDPConn, addr *net.UDPAddr, index uint32, data []byte) {
	t.Helper()
	pkt, err := masso.UploadChunk(index, data)
	if err != nil {
		t.Fatalf("UploadChunk(%d): %v", index, err)
	}
	mustWrite(t, conn, addr, pkt)
}

func readChunkAck(t *testing.T, conn *net.UDPConn) masso.ChunkAck {
	t.Helper()
	reply := readReply(t, conn)
	ack, ok := reply.(masso.ChunkAck)
	if !ok {
		t.Fatalf("reply type = %T, want ChunkAck", reply)
	}
	return ack
}

// replyTimeout bounds how long a test waits for an expected reply. Loopback
// UDP delivery is effectively instant, so this only matters when something
// is actually broken.
const replyTimeout = 2 * time.Second

// readReply reads and decodes one reply from conn within replyTimeout.
func readReply(t *testing.T, conn *net.UDPConn) masso.Reply {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(replyTimeout)); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}
	buf := make([]byte, masso.MaxPacket)
	n, _, err := conn.ReadFromUDP(buf)
	if err != nil {
		t.Fatalf("ReadFromUDP: %v", err)
	}
	reply, err := masso.DecodeReply(buf[:n])
	if err != nil {
		t.Fatalf("DecodeReply: %v", err)
	}
	return reply
}

// expectSilence asserts that no reply arrives on conn within timeout.
func expectSilence(t *testing.T, conn *net.UDPConn, timeout time.Duration) {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(timeout)); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}
	buf := make([]byte, masso.MaxPacket)
	_, _, err := conn.ReadFromUDP(buf)
	if err == nil {
		t.Fatal("expected no reply, but one arrived")
	}
	var netErr net.Error
	if !errors.As(err, &netErr) || !netErr.Timeout() {
		t.Fatalf("ReadFromUDP error = %v, want a timeout", err)
	}
}
