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
	"errors"
	"net"
	"runtime"
	"strings"
	"testing"
	"time"

	"msrl.dev/mink-lasso/internal/masso"
	"msrl.dev/mink-lasso/internal/masso/sim"
)

// TestAddr_NonUDPLocalAddr covers Addr's fallback when the underlying
// net.PacketConn's LocalAddr is not a *net.UDPAddr — impossible with a real
// "udp" socket, so it is exercised through an injected fake.
func TestAddr_NonUDPLocalAddr(t *testing.T) {
	t.Parallel()
	base := listenLoopback(t)
	fc := &fakeConn{PacketConn: base, localAddr: func() net.Addr { return nonUDPAddr{} }}
	ctrl := newController(t, sim.Options{
		ListenPacket: func(string, string) (net.PacketConn, error) { return fc, nil },
	})
	if got := ctrl.Addr(); got != nil {
		t.Fatalf("Addr() = %v, want nil", got)
	}
}

// TestDiscovery_BadSourceAddress covers both of replyAddrFor's error
// paths — a source address with no "host:port" separator, and one whose
// host does not parse as an IP — via a discovery request whose source
// address a real "udp" socket could never produce; it is only reachable
// through an injected fake.
func TestDiscovery_BadSourceAddress(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		src  net.Addr
	}{
		{"no host:port separator", badAddr{}},
		{"host is not an IP", badHostAddr{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			base := listenLoopback(t)
			conn, ready := injectOnce(base, masso.Discovery(11000), tt.src)

			var logBuf syncBuffer
			ctrl := newController(t, sim.Options{
				Logger:       newDebugLogger(&logBuf),
				ListenPacket: func(string, string) (net.PacketConn, error) { return conn, nil },
			})
			<-ready

			if !strings.Contains(logBuf.String(), "cannot determine reply target") {
				t.Fatalf("log = %q, want it to mention the bad source address", logBuf.String())
			}
			if got := ctrl.Discoveries(); got != 0 {
				t.Fatalf("Discoveries() = %d, want 0: a request with no derivable reply target is not answered", got)
			}
			if got := ctrl.LastReplyTarget(); got != nil {
				t.Fatalf("LastReplyTarget() = %v, want nil", got)
			}
		})
	}
}

// TestWriteFailure_Logged covers the reply-write error path: the request
// is still processed, but the caller simply never sees a reply.
func TestWriteFailure_Logged(t *testing.T) {
	t.Parallel()
	base := listenLoopback(t)
	fc := &fakeConn{
		PacketConn: base,
		writeTo:    func([]byte, net.Addr) (int, error) { return 0, errors.New("write boom") },
	}
	ctrl := newController(t, sim.Options{
		ListenPacket: func(string, string) (net.PacketConn, error) { return fc, nil },
	})

	client := newClient(t)
	mustWrite(t, client, ctrl.Addr(), masso.Discovery(clientPort(t, client)))
	expectSilence(t, client, 150*time.Millisecond)
}

// TestServe_UnexpectedReadError covers serve's non-Close read-error path.
// It waits for the log line by polling instead of by racing Close against
// the goroutine under test, since nothing else observably signals that the
// error branch (rather than the Close branch) has run.
func TestServe_UnexpectedReadError(t *testing.T) {
	t.Parallel()
	base := listenLoopback(t)
	fc := &fakeConn{
		PacketConn: base,
		readFrom:   func([]byte) (int, net.Addr, error) { return 0, nil, errors.New("read boom") },
	}
	var logBuf syncBuffer
	newController(t, sim.Options{
		Logger:       newDebugLogger(&logBuf),
		ListenPacket: func(string, string) (net.PacketConn, error) { return fc, nil },
	})

	for !strings.Contains(logBuf.String(), "read failed, stopping") {
		runtime.Gosched()
	}
}
