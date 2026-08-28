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

package engine

import (
	"context"
	"net"

	"msrl.dev/mink-lasso/internal/masso"
)

// connLoop drives the connection state machine until ctx is done:
// Unconfigured idles until SetSerial; Discovering tries unicast candidates
// for UnicastFirst, then rations broadcasts; Connected forwards status and
// tools until the connection is lost (or SetSerial/shutdown interrupts it),
// then it goes back to Discovering.
func (e *Engine) connLoop(ctx context.Context) {
	for ctx.Err() == nil {
		e.runConnAttempt(ctx)
	}
}

// runConnAttempt runs one pass of the state machine: idle while
// unconfigured, or discover+connect+run for the currently configured
// serial. It returns when that pass ends, for any reason — SetSerial
// canceling it, the connection being lost, or ctx being done — so connLoop
// can decide whether to start another.
//
// A pass may start while the scheduler still has an upload in flight (the
// connection was lost mid-transfer, or SetSerial arrived). That is safe
// without coordination: masso.Client.Upload captures its destination
// before sending anything, so a Connect here cannot redirect its chunks,
// and the scheduler runs one upload at a time, so the new connection's
// first upload waits for the old one to finish or stall out.
func (e *Engine) runConnAttempt(ctx context.Context) {
	attemptCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	e.mu.Lock()
	e.cancelAttempt = cancel
	serial := e.serial
	e.mu.Unlock()

	if serial == 0 {
		e.dispatcher.emit(ConnState{Kind: Unconfigured})
		<-attemptCtx.Done()
		return
	}

	e.dispatcher.emit(ConnState{Kind: Discovering, Serial: serial})
	addr, id, err := e.discoverAndConnect(attemptCtx, serial)
	if err != nil {
		return
	}

	e.mu.Lock()
	e.lastAddr = addr
	e.mu.Unlock()

	e.gate.connected()
	e.opts.Logger.Info("engine: connected", "serial", serial, "addr", addr)
	e.dispatcher.emit(ConnState{Kind: Connected, Serial: serial, Addr: addr, Identity: id})
	e.runConnected(attemptCtx, serial, addr)
	e.gate.disconnected()
}

// discoverAndConnect finds the configured serial and completes the Connect
// handshake with it, per the discovery policy in the package doc: unicast
// Connect attempts against Config.Address and the last-known address for up
// to UnicastFirst, matching only a reply whose Identity.Serial is serial;
// then broadcast Discover at most once per BroadcastInterval, connecting to
// the first Found that matches.
func (e *Engine) discoverAndConnect(ctx context.Context, serial uint16) (*net.UDPAddr, masso.Identity, error) {
	deadline := e.opts.Clock.Now().Add(e.opts.UnicastFirst)
	if candidates := e.candidateAddrs(); len(candidates) > 0 {
		for e.opts.Clock.Now().Before(deadline) {
			for _, addr := range candidates {
				if ctx.Err() != nil {
					return nil, masso.Identity{}, ctx.Err()
				}
				e.dispatcher.emit(ConnState{Kind: Connecting, Serial: serial, Addr: addr})
				id, _, err := e.client.Connect(ctx, addr)
				switch {
				case err != nil:
					e.opts.Logger.Debug("engine: unicast connect miss", "addr", addr, "error", err)
				case id.Serial != serial:
					e.opts.Logger.Debug("engine: unicast connect wrong serial", "addr", addr, "serial", id.Serial)
				default:
					return addr, id, nil
				}
				e.dispatcher.emit(ConnState{Kind: Discovering, Serial: serial})
			}
		}
	}

	return e.broadcastConnect(ctx, serial)
}

// candidateAddrs returns Config.Address (if set) followed by the last-known
// address for the configured serial — in memory from the previous
// successful connect this run, else Config.LastAddress — each parsed as a
// host:port. An address that fails to parse is logged and skipped.
func (e *Engine) candidateAddrs() []*net.UDPAddr {
	var out []*net.UDPAddr
	add := func(s string) {
		if s == "" {
			return
		}
		addr, err := net.ResolveUDPAddr("udp", s)
		if err != nil {
			e.opts.Logger.Debug("engine: bad candidate address", "address", s, "error", err)
			return
		}
		out = append(out, addr)
	}

	add(e.opts.Config.Address)
	e.mu.Lock()
	last := e.lastAddr
	e.mu.Unlock()
	if last != nil {
		out = append(out, last)
	} else {
		add(e.opts.Config.LastAddress)
	}
	return out
}

// broadcastConnect rations a Discover broadcast to at most once per
// BroadcastInterval, connecting to the first Found whose serial matches.
func (e *Engine) broadcastConnect(ctx context.Context, serial uint16) (*net.UDPAddr, masso.Identity, error) {
	ticker := e.opts.Clock.NewTicker(e.opts.BroadcastInterval)
	defer ticker.Stop()

	for {
		found, err := e.client.Discover(ctx, e.opts.DiscoverTimeout)
		if err != nil {
			// A synchronous Discover failure (e.g. masso.ErrDiscoverySend
			// with no usable network interface) must not spin this loop:
			// log it loudly and pace the next attempt on the same ticker
			// as an unmatched discovery, rather than returning immediately
			// and letting connLoop retry with zero delay.
			e.opts.Logger.Info("engine: discovery failed", "error", err)
			select {
			case <-ctx.Done():
				return nil, masso.Identity{}, ctx.Err()
			case <-ticker.C():
				continue
			}
		}
		for _, f := range found {
			if f.Identity.Serial != serial {
				e.opts.Logger.Debug("engine: broadcast found wrong serial", "addr", f.Addr, "serial", f.Identity.Serial)
				continue
			}
			e.dispatcher.emit(ConnState{Kind: Connecting, Serial: serial, Addr: f.Addr})
			id, _, err := e.client.Connect(ctx, f.Addr)
			if err != nil || id.Serial != serial {
				e.opts.Logger.Debug("engine: broadcast connect failed", "addr", f.Addr, "error", err)
				e.dispatcher.emit(ConnState{Kind: Discovering, Serial: serial})
				continue
			}
			return f.Addr, id, nil
		}

		select {
		case <-ctx.Done():
			return nil, masso.Identity{}, ctx.Err()
		case <-ticker.C():
		}
	}
}

// runConnected fetches the tool table once, forwards every status as a
// StatusEvent alongside the current gate verdict, serves RefreshTools
// requests, and returns when the client's keepalive loop ends — emitting
// Lost unless ctx is done (a restart or shutdown already in progress, which
// the caller reports itself).
func (e *Engine) runConnected(ctx context.Context, serial uint16, addr *net.UDPAddr) {
	e.fetchTools(ctx)

	statusCtx, statusCancel := context.WithCancel(ctx)
	statusDone := make(chan struct{})
	go func() {
		defer close(statusDone)
		for {
			select {
			case <-statusCtx.Done():
				return
			case st := <-e.client.Status():
				g := e.gate.updateStatus(st)
				e.dispatcher.emit(StatusEvent{Status: st, Gate: g})
			case <-e.refreshTools:
				e.fetchTools(statusCtx)
			}
		}
	}()

	runErr := e.client.Run(ctx)
	statusCancel()
	<-statusDone

	if ctx.Err() != nil {
		return
	}
	e.dispatcher.emit(ConnState{Kind: Lost, Serial: serial, Addr: addr, Err: runErr})
}

// fetchTools queries the tool table and always emits a ToolsEvent, success
// or failure.
func (e *Engine) fetchTools(ctx context.Context) {
	tools, err := e.client.Tools(ctx)
	e.dispatcher.emit(ToolsEvent{Tools: tools, Err: err})
}

// SetSerial reconfigures the connection loop's target serial (0 means
// unconfigured) and restarts it from Discovering, with the unicast-first
// window reset.
func (e *Engine) SetSerial(serial uint16) {
	e.mu.Lock()
	e.serial = serial
	// lastAddr belongs to the previous serial; candidateAddrs must not
	// offer it for the new one, which would make discoverAndConnect send
	// a live unicast Connect (and the Discovery it starts with) to a
	// controller mink-lasso no longer targets — retargeting its status
	// stream away from whatever else may legitimately be talking to it,
	// exactly the hazard docs/protocol.md warns about.
	e.lastAddr = nil
	cancel := e.cancelAttempt
	e.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// SetUploadWhileMachining updates the machine gate's override: when true,
// the gate is open whenever connected, regardless of machine state.
func (e *Engine) SetUploadWhileMachining(v bool) {
	e.gate.setUploadWhileMachining(v)
}

// RefreshTools re-fetches the tool table from the connected controller,
// emitting a fresh ToolsEvent. A call while not connected is queued and
// applied once a connection is made.
func (e *Engine) RefreshTools() {
	select {
	case e.refreshTools <- struct{}{}:
	default:
	}
}
