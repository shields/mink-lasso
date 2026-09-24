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

// Package engine is the orchestration layer between internal/watch, the
// Masso client (internal/masso), the filesystem, and a UI or headless front
// end. It owns the connection state machine (discovery, connect, keepalive,
// reconnect), the machine gate that decides when an upload may start, the
// one-at-a-time upload scheduler with retry and backoff, and archiving a
// sent file into WatchDir/sent, under the same subfolder of WatchDir it was
// found in. Every state change is delivered, in order,
// as an Event on the channel returned by Events; nothing here blocks a slow
// or absent consumer.
//
// The engine is built and driven through Options, whose function fields
// default to adapters over the real *masso.Client, *watch.Watcher, and OS
// filesystem calls; tests substitute fakes for error paths and the real
// internal/masso/sim controller plus a t.TempDir() watch folder for the
// end-to-end scenarios.
package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"msrl.dev/mink-lasso/internal/clock"
	"msrl.dev/mink-lasso/internal/config"
	"msrl.dev/mink-lasso/internal/masso"
	"msrl.dev/mink-lasso/internal/watch"
	"msrl.dev/mink-lasso/internal/winutil"
)

// ErrBind is returned by New when opts.NewClient (or its default adapter)
// could not bind the client's UDP socket — most commonly because another
// program, such as Masso Link itself, is already using it.
var ErrBind = errors.New("engine: could not bind Masso client")

// ErrAlreadyRunning is returned by a second call to Run on the same Engine.
var ErrAlreadyRunning = errors.New("engine: Run called more than once")

// Client is the subset of *masso.Client the engine needs. It exists so
// tests can substitute a fake for error paths that are impractical to
// provoke over a real socket.
type Client interface {
	Discover(ctx context.Context, timeout time.Duration) ([]masso.Found, error)
	Connect(ctx context.Context, addr *net.UDPAddr) (masso.Identity, masso.ConfigReply, error)
	Run(ctx context.Context) error
	Status() <-chan masso.Status
	Tools(ctx context.Context) ([]masso.ToolRecord, error)
	Upload(ctx context.Context, dir, name string, r io.ReaderAt, size int64, progress func(sent, total int64)) error
	Remote() *net.UDPAddr
	Close() error
}

// Watcher is the subset of *watch.Watcher the engine needs.
type Watcher interface {
	Run(ctx context.Context) error
	Ready() <-chan watch.File
	Rejected() <-chan watch.Rejected
}

// clientAdapter adapts *masso.Client to Client. Every method already
// matches; it exists only to give the concrete type a name that satisfies
// the interface without depending on masso.Client's exact method set at the
// call site.
type clientAdapter struct{ c *masso.Client }

func (a clientAdapter) Discover(ctx context.Context, timeout time.Duration) ([]masso.Found, error) {
	return a.c.Discover(ctx, timeout)
}

func (a clientAdapter) Connect(ctx context.Context, addr *net.UDPAddr) (masso.Identity, masso.ConfigReply, error) {
	return a.c.Connect(ctx, addr)
}

func (a clientAdapter) Run(ctx context.Context) error { return a.c.Run(ctx) }

func (a clientAdapter) Status() <-chan masso.Status { return a.c.Status() }

func (a clientAdapter) Tools(ctx context.Context) ([]masso.ToolRecord, error) { return a.c.Tools(ctx) }

func (a clientAdapter) Upload(
	ctx context.Context, dir, name string, r io.ReaderAt, size int64, progress func(sent, total int64),
) error {
	return a.c.Upload(ctx, dir, name, r, size, progress)
}

func (a clientAdapter) Remote() *net.UDPAddr { return a.c.Remote() }

func (a clientAdapter) Close() error { return a.c.Close() }

// defaultNewClient adapts masso.NewClient to Options.NewClient.
func defaultNewClient(opts masso.Options) (Client, error) {
	c, err := masso.NewClient(opts)
	if err != nil {
		return nil, err
	}
	return clientAdapter{c}, nil
}

// watcherAdapter adapts *watch.Watcher to Watcher.
type watcherAdapter struct{ w *watch.Watcher }

func (a watcherAdapter) Run(ctx context.Context) error   { return a.w.Run(ctx) }
func (a watcherAdapter) Ready() <-chan watch.File        { return a.w.Ready() }
func (a watcherAdapter) Rejected() <-chan watch.Rejected { return a.w.Rejected() }

// defaultNewWatcher adapts watch.New to Options.NewWatcher.
func defaultNewWatcher(opts watch.Options) (Watcher, error) {
	w, err := watch.New(opts)
	if err != nil {
		return nil, err
	}
	return watcherAdapter{w}, nil
}

// Options configures a new Engine. Every duration and function field takes
// the documented default when left zero/nil; only Config need be set.
type Options struct {
	// Config is the already-validated application configuration. The
	// engine reads Serial, Address, LastAddress, WatchDir, ListenPort,
	// ScanInterval, SettleDelay, PauseGrace, Extensions, and
	// UploadWhileMachining from it.
	Config config.Config

	// Logger receives operator-relevant Info lines (connected, file sent,
	// failures) and Debug lines (discovery misses). Nil means
	// slog.New(slog.DiscardHandler).
	Logger *slog.Logger
	// Clock supplies every timeout, retry, and gate timer. Nil means
	// clock.Real{}.
	Clock clock.Clock

	// NewClient constructs the Masso client. Nil adapts masso.NewClient.
	NewClient func(masso.Options) (Client, error)
	// NewWatcher constructs the folder watcher. Nil adapts watch.New.
	NewWatcher func(watch.Options) (Watcher, error)

	// Open opens a file deny-write before uploading it. Nil means
	// winutil.OpenDenyWrite.
	Open func(string) (*os.File, error)
	// IsRemote reports whether a directory is a network share, passed to
	// the watcher. Nil means winutil.IsRemote.
	IsRemote func(string) bool
	// Stat, MkdirAll, and Rename back SetWatchDir validation and
	// archiving. Nil means os.Stat, os.MkdirAll, and os.Rename.
	Stat     func(string) (os.FileInfo, error)
	MkdirAll func(path string, perm os.FileMode) error
	Rename   func(oldpath, newpath string) error

	// ClientOptions seeds the masso.Options passed to NewClient. The
	// engine overwrites Logger, Clock, PortMin (from Config.ListenPort),
	// and PortMax (masso.ListenPortMax); every other field — including
	// zero-value timeouts a test wants shortened — passes through as
	// given.
	ClientOptions masso.Options

	// IdleHold is how long status must show neither Running nor
	// WaitingForOperator, continuously, before the machine gate opens.
	// Default 5s.
	IdleHold time.Duration
	// BroadcastInterval is the minimum time between discovery broadcasts.
	// Default 5s.
	BroadcastInterval time.Duration
	// UnicastFirst is how long, after a (re)start of the connection loop,
	// only unicast Connect attempts are tried before falling back to
	// broadcast discovery. Default 10s.
	UnicastFirst time.Duration
	// DiscoverTimeout bounds a single broadcast Discover call. Default 1s.
	DiscoverTimeout time.Duration
	// Backoff is the per-file consecutive-failure retry schedule; the
	// last value repeats. Default {5s, 10s, 30s, 60s}.
	Backoff []time.Duration
	// MoveRetries is how many times the archive rename is retried on
	// failure. Default 5.
	MoveRetries int
	// MoveRetryInterval is the delay between archive rename retries.
	// Default 1s.
	MoveRetryInterval time.Duration
	// ProgressInterval bounds how often a Sending event is emitted for an
	// in-progress transfer that has not crossed a 5% boundary. Default
	// 100ms.
	ProgressInterval time.Duration
}

// normalize returns a copy of opts with every zero field replaced by its
// documented default.
func normalize(opts Options) Options {
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	if opts.Clock == nil {
		opts.Clock = clock.Real{}
	}
	if opts.NewClient == nil {
		opts.NewClient = defaultNewClient
	}
	if opts.NewWatcher == nil {
		opts.NewWatcher = defaultNewWatcher
	}
	if opts.Open == nil {
		opts.Open = winutil.OpenDenyWrite
	}
	if opts.IsRemote == nil {
		opts.IsRemote = winutil.IsRemote
	}
	if opts.Stat == nil {
		opts.Stat = os.Stat
	}
	if opts.MkdirAll == nil {
		opts.MkdirAll = os.MkdirAll
	}
	if opts.Rename == nil {
		opts.Rename = os.Rename
	}
	if opts.IdleHold <= 0 {
		opts.IdleHold = 5 * time.Second
	}
	if opts.BroadcastInterval <= 0 {
		opts.BroadcastInterval = 5 * time.Second
	}
	if opts.UnicastFirst <= 0 {
		opts.UnicastFirst = 10 * time.Second
	}
	if opts.DiscoverTimeout <= 0 {
		opts.DiscoverTimeout = time.Second
	}
	if len(opts.Backoff) == 0 {
		opts.Backoff = []time.Duration{5 * time.Second, 10 * time.Second, 30 * time.Second, 60 * time.Second}
	}
	if opts.MoveRetries <= 0 {
		opts.MoveRetries = 5
	}
	if opts.MoveRetryInterval <= 0 {
		opts.MoveRetryInterval = time.Second
	}
	if opts.ProgressInterval <= 0 {
		opts.ProgressInterval = 100 * time.Millisecond
	}

	opts.ClientOptions.Logger = opts.Logger
	opts.ClientOptions.Clock = opts.Clock
	opts.ClientOptions.PortMin = opts.Config.ListenPort
	opts.ClientOptions.PortMax = masso.ListenPortMax

	return opts
}

// Engine orchestrates discovery/connection, the upload scheduler, and the
// event stream. Build one with New and drive it with Run.
type Engine struct {
	opts   Options
	client Client
	gate   *gate

	dispatcher   *dispatcher
	refreshTools chan struct{}
	scheduler    *scheduler

	started atomic.Bool

	mu            sync.Mutex
	serial        uint32 // 0 means unconfigured
	lastAddr      *net.UDPAddr
	cancelAttempt context.CancelFunc
	watchDir      string
	cancelWatch   context.CancelFunc
	watchStopped  chan struct{}
}

// New validates and normalizes opts, parses opts.Config.Serial (empty means
// "unconfigured": the connection loop idles until SetSerial), and binds the
// Masso client immediately via opts.NewClient so a port conflict — most
// commonly Masso Link itself already running — fails fast. New never starts
// goroutines; call Run for that.
func New(opts Options) (*Engine, error) {
	var serial uint32
	if opts.Config.Serial != "" {
		s, err := masso.ParseSerial(opts.Config.Serial)
		if err != nil {
			return nil, fmt.Errorf("engine: %w", err)
		}
		serial = s
	}

	opts = normalize(opts)

	client, err := opts.NewClient(opts.ClientOptions)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrBind, err)
	}

	e := &Engine{
		opts:   opts,
		client: client,
		gate: newGate(
			opts.Clock,
			opts.IdleHold,
			time.Duration(opts.Config.PauseGrace),
			opts.Config.UploadWhileMachining,
		),
		dispatcher:   newDispatcher(),
		refreshTools: make(chan struct{}, 1),
		serial:       serial,
		watchDir:     opts.Config.WatchDir,
	}
	e.scheduler = newScheduler(e)
	return e, nil
}

// Events returns the channel every state change is delivered on, in order.
// It is closed once Run returns. A caller must keep draining Events() for
// as long as Run is running — including through shutdown, once ctx is
// done — since Run waits for every queued event to be delivered before it
// returns; a consumer that stops early will make Run hang.
func (e *Engine) Events() <-chan Event { return e.dispatcher.out }

// Run starts the dispatcher, the connection loop, the machine gate's timer,
// the watcher lifecycle, and the upload scheduler, then blocks until ctx is
// done. It then waits for the gate, watcher, and scheduler goroutines to
// stop — the scheduler only does once any in-flight, already-acknowledged
// transfer has finished, never aborting it — stops producing events, waits
// for the dispatcher to drain and close Events(), closes the client, and
// returns nil: a clean shutdown is not an error. Run may be called once; a
// second call returns ErrAlreadyRunning immediately rather than starting a
// second dispatcher-drain goroutine against the same channels.
func (e *Engine) Run(ctx context.Context) error {
	if !e.started.CompareAndSwap(false, true) {
		return ErrAlreadyRunning
	}

	go e.dispatcher.run()

	var wg sync.WaitGroup
	wg.Go(func() { e.gate.run(ctx) })
	wg.Go(func() { e.watchLoop(ctx) })
	wg.Go(func() { e.scheduler.run(ctx) })

	e.connLoop(ctx)
	wg.Wait()

	// Every producer goroutine above has now stopped, so no further emit
	// calls will happen; close and wait join the dispatcher's goroutine
	// rather than leaving it running after Run returns, once every event
	// already queued has been delivered to whatever is reading Events().
	e.dispatcher.close()
	e.dispatcher.wait()

	if err := e.client.Close(); err != nil {
		return fmt.Errorf("engine: close client: %w", err)
	}
	return nil
}
