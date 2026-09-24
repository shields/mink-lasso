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

// Package model is the portable, fully tested view model behind the
// Windows GUI (internal/ui/walkui, which only paints what this package
// returns and forwards clicks to its action methods). It holds no walk or
// Windows dependency and builds and tests on any OS.
package model

import (
	"net"
	"sync"
	"time"

	"msrl.dev/mink-lasso/internal/clock"
	"msrl.dev/mink-lasso/internal/config"
	"msrl.dev/mink-lasso/internal/engine"
	"msrl.dev/mink-lasso/internal/masso"
)

// defaultMaxTransfers and defaultMaxLog are Options.MaxTransfers and
// Options.MaxLog's defaults when left zero.
const (
	defaultMaxTransfers = 500
	defaultMaxLog       = 2000
)

// Control is the subset of *engine.Engine the model drives. It is
// satisfied by *engine.Engine; Options.Engine may be nil in tests and in
// any other context where actions should only update local state.
type Control interface {
	SetSerial(serial uint32)
	SetWatchDir(dir string) error
	SetUploadWhileMachining(v bool)
	RefreshTools()
	Retry(name string)
	SendFile(path string) error
}

// BalloonKind selects the icon a tray balloon is shown with.
type BalloonKind int

// Balloon kinds.
const (
	BalloonInfo BalloonKind = iota
	BalloonError
)

// Balloon describes a tray notification the binding should pop up.
type Balloon struct {
	Kind  BalloonKind
	Title string
	Text  string
}

// Changes reports which parts of the UI a call to Apply or an action
// changed, so the binding repaints only what moved. Balloon is set only
// when a tray notification should be shown alongside the repaint.
type Changes struct {
	StatusLine bool
	Machine    bool
	Watch      bool
	Transfers  bool
	Tools      bool
	Log        bool
	Tray       bool
	Balloon    *Balloon
}

// Options configures a new Model.
type Options struct {
	// Config is the starting configuration; the model keeps its own copy,
	// updated by actions and persisted through Save.
	Config config.Config
	// Save persists a changed Config. Nil means no persistence: actions
	// still update local state but nothing is written to disk.
	Save func(config.Config) error
	// Engine is driven by the model's actions. Nil means actions only
	// update local state.
	Engine Control
	// Clock is used for nothing but is accepted for symmetry with the
	// rest of the codebase and in case a future action needs it. Nil
	// means clock.Real{}.
	Clock clock.Clock
	// Version is shown in AboutText.
	Version string
	// MaxTransfers caps how many transfer rows are kept, evicting the
	// oldest terminal rows first. Zero means 500.
	MaxTransfers int
	// MaxLog caps how many log lines are kept in the ring. Zero means
	// 2000.
	MaxLog int
	// OnChange is called, possibly from any goroutine, whenever
	// something changes outside of a direct Apply/action call — in
	// practice, only a log line arriving through LogHandler. The binding
	// is responsible for marshaling this onto the UI thread.
	OnChange func(Changes)
}

// transferEntry is a stored transfer row plus the timestamp it was last
// updated at, used both for "newest first" ordering and for picking the
// oldest terminal row to evict.
type transferEntry struct {
	row TransferRow
	at  time.Time
}

// Model is the portable view model. Every exported method takes and
// returns copies; there is no exported mutable state. It is safe for
// LogHandler to be called from any goroutine concurrently with Apply and
// the read accessors, all of which are otherwise expected to run on the UI
// goroutine.
type Model struct {
	mu sync.Mutex

	cfg     config.Config
	save    func(config.Config) error
	engine  Control
	clk     clock.Clock
	version string

	maxTransfers int
	maxLog       int
	onChange     func(Changes)

	connKind     engine.ConnKind
	connSerial   uint32
	connAddr     *net.UDPAddr
	connIdentity masso.Identity
	connErr      error

	status    masso.Status
	gate      engine.Gate
	hasStatus bool

	watchDir       string
	watchTimerOnly bool
	watchErr       error

	// transfers is keyed by file name, mirroring internal/engine's
	// scheduler: it tracks exactly one live item per name, keyed strictly
	// by name (see scheduler.go's item type), reused across an automatic
	// watch-folder send and a manual SendFile of the same file. Manual is
	// carried as a displayed attribute of that one row, not a second key.
	transfers map[string]*transferEntry

	tools []masso.ToolRecord

	logLines []string
}

// New returns a Model seeded from opts.
func New(opts Options) *Model {
	m := &Model{
		cfg:          opts.Config,
		save:         opts.Save,
		engine:       opts.Engine,
		clk:          opts.Clock,
		version:      opts.Version,
		maxTransfers: opts.MaxTransfers,
		maxLog:       opts.MaxLog,
		onChange:     opts.OnChange,
		transfers:    make(map[string]*transferEntry),
	}
	if m.clk == nil {
		m.clk = clock.Real{}
	}
	if m.maxTransfers <= 0 {
		m.maxTransfers = defaultMaxTransfers
	}
	if m.maxLog <= 0 {
		m.maxLog = defaultMaxLog
	}
	m.watchDir = opts.Config.WatchDir
	// config.Config.Serial accepts either "G3-nnnnn" or a bare "nnnnn";
	// Validate only checks that it parses and never rewrites it, so
	// normalize here to keep SerialText's documented "G3-nnnnn" contract
	// for a config loaded straight from disk. An unparsable value (should
	// have been caught by config.Validate) is left as-is.
	if serial, err := masso.ParseSerial(m.cfg.Serial); err == nil {
		m.cfg.Serial = masso.SerialString(serial)
	}
	return m
}

// SetEngine replaces the engine the action methods drive. It exists so the
// model can be built before the engine — the engine's logger must include
// LogHandler, which needs a Model — and pointed at the real engine once
// there is one. Calls that arrive in between behave as with a nil
// Options.Engine.
func (m *Model) SetEngine(eng Control) {
	m.mu.Lock()
	m.engine = eng
	m.mu.Unlock()
}

// SetOnChange replaces the OnChange callback (see Options.OnChange). The
// binding registers itself here once it has a UI thread to marshal onto;
// nil clears it.
func (m *Model) SetOnChange(fn func(Changes)) {
	m.mu.Lock()
	m.onChange = fn
	m.mu.Unlock()
}
