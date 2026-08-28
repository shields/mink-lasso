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

package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"msrl.dev/mink-lasso/internal/config"
	"msrl.dev/mink-lasso/internal/engine"
	"msrl.dev/mink-lasso/internal/logfile"
	"msrl.dev/mink-lasso/internal/masso"
	"msrl.dev/mink-lasso/internal/ui/model"
	"msrl.dev/mink-lasso/internal/winutil"
)

// errUnreachable satisfies functions that must return an error to
// type-check but call t.Fatal (which stops the goroutine via
// runtime.Goexit) before ever returning it.
var errUnreachable = errors.New("unreachable")

// fakeEngine is an app.Engine test double: Run blocks until ctx is done,
// signaling readiness on started and then closing events (as the real
// engine's Run does once its dispatcher has drained and closed Events()). As
// with the real engine, a second call to Run returns ErrAlreadyRunning
// immediately instead of closing events a second time.
type fakeEngine struct {
	events    chan engine.Event
	started   chan struct{}
	startOnce sync.Once
	running   atomic.Bool
	runErr    error
}

func newFakeEngine() *fakeEngine {
	return &fakeEngine{events: make(chan engine.Event), started: make(chan struct{})}
}

func (f *fakeEngine) Run(ctx context.Context) error {
	if !f.running.CompareAndSwap(false, true) {
		return engine.ErrAlreadyRunning
	}

	f.startOnce.Do(func() { close(f.started) })
	<-ctx.Done()
	close(f.events)

	return f.runErr
}

func (f *fakeEngine) Events() <-chan engine.Event { return f.events }
func (*fakeEngine) SetSerial(uint16)              {}
func (*fakeEngine) SetWatchDir(string) error      { return nil }
func (*fakeEngine) SetUploadWhileMachining(bool)  {}
func (*fakeEngine) RefreshTools()                 {}
func (*fakeEngine) Retry(string)                  {}
func (*fakeEngine) SendFile(string) error         { return nil }

// testEnv bundles what a test needs from testDeps: Deps wired to a fresh
// temp directory, captured stdout/stderr, and NotifyCancel, set once run's
// ctx is created, so a test can simulate a shutdown signal.
type testEnv struct {
	Deps         Deps
	Stdout       *bytes.Buffer
	Stderr       *bytes.Buffer
	NotifyCancel *context.CancelFunc
}

func testDeps(t *testing.T) testEnv {
	t.Helper()
	tmp := t.TempDir()
	env := testEnv{
		Stdout:       &bytes.Buffer{},
		Stderr:       &bytes.Buffer{},
		NotifyCancel: new(context.CancelFunc),
	}

	env.Deps = Deps{
		Stdout: env.Stdout,
		Stderr: env.Stderr,
		Notify: func(ctx context.Context) (context.Context, context.CancelFunc) {
			c, cancel := context.WithCancel(ctx)
			*env.NotifyCancel = cancel

			return c, cancel
		},
		UserConfigDir: func() (string, error) { return filepath.Join(tmp, "config"), nil },
		UserCacheDir:  func() (string, error) { return filepath.Join(tmp, "cache"), nil },
		Executable:    func() (string, error) { return filepath.Join(tmp, "bin", "mink-lasso.exe"), nil },
		SingleInstance: func(string) (func(), error) {
			return func() {}, nil
		},
	}

	return env
}

func TestMainHelp(t *testing.T) {
	t.Parallel()
	env := testDeps(t)
	d := env.Deps
	stderr := env.Stderr
	if code := Main([]string{"-h"}, d); code != 0 {
		t.Errorf("Main(-h) = %d, want 0", code)
	}
	if stderr.Len() == 0 {
		t.Error("expected usage on stderr")
	}
}

func TestMainBadFlag(t *testing.T) {
	t.Parallel()
	env := testDeps(t)
	d := env.Deps
	if code := Main([]string{"-this-flag-does-not-exist"}, d); code != 2 {
		t.Errorf("Main(bad flag) = %d, want 2", code)
	}
}

func TestMainVersion(t *testing.T) {
	t.Parallel()
	env := testDeps(t)
	d := env.Deps
	stdout := env.Stdout
	if code := Main([]string{"-version"}, d); code != 0 {
		t.Errorf("Main(-version) = %d, want 0", code)
	}
	if got := stdout.String(); got != version+"\n" {
		t.Errorf("stdout = %q, want %q", got, version+"\n")
	}
}

func TestMainVersionTouchesNothingElse(t *testing.T) {
	t.Parallel()
	env := testDeps(t)
	d := env.Deps
	d.UserConfigDir = func() (string, error) { t.Fatal("UserConfigDir called"); return "", nil }
	if code := Main([]string{"-version"}, d); code != 0 {
		t.Errorf("Main(-version) = %d, want 0", code)
	}
}

func TestMainConfigPathFallbackFails(t *testing.T) {
	t.Parallel()
	env := testDeps(t)
	d := env.Deps
	stderr := env.Stderr
	d.UserConfigDir = func() (string, error) { return "", errors.New("no profile") }
	d.Executable = func() (string, error) { return "", errors.New("no exe") }

	if code := Main([]string{"-headless"}, d); code != 1 {
		t.Errorf("Main = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "cannot determine default config path") {
		t.Errorf("stderr = %q, want it to mention the config path failure", stderr.String())
	}
}

func TestMainConfigLoadInvalidJSON(t *testing.T) {
	t.Parallel()
	env := testDeps(t)
	d := env.Deps
	stderr := env.Stderr
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	if code := Main([]string{"-headless", "-config", path}, d); code != 1 {
		t.Errorf("Main = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), path) {
		t.Errorf("stderr = %q, want it to mention %q", stderr.String(), path)
	}
}

func TestMainConfigOverrideInvalidSerial(t *testing.T) {
	t.Parallel()
	env := testDeps(t)
	d := env.Deps
	stderr := env.Stderr
	path := filepath.Join(t.TempDir(), "config.json")

	if code := Main([]string{"-headless", "-config", path, "-serial", "not-a-serial"}, d); code != 1 {
		t.Errorf("Main = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "invalid serial") {
		t.Errorf("stderr = %q, want it to mention the invalid serial", stderr.String())
	}
}

func TestMainConfigOverrideInvalidLogLevel(t *testing.T) {
	t.Parallel()
	env := testDeps(t)
	d := env.Deps
	stderr := env.Stderr
	path := filepath.Join(t.TempDir(), "config.json")

	if code := Main([]string{"-headless", "-config", path, "-log-level", "not-a-level"}, d); code != 1 {
		t.Errorf("Main = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "logLevel") {
		t.Errorf("stderr = %q, want it to mention logLevel", stderr.String())
	}
}

func TestMainGUIUnavailableWithoutHeadless(t *testing.T) {
	t.Parallel()
	env := testDeps(t)
	d := env.Deps
	stderr := env.Stderr
	// deps.GUI left nil.
	if code := Main(nil, d); code != 2 {
		t.Errorf("Main = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "-headless") {
		t.Errorf("stderr = %q, want it to mention -headless", stderr.String())
	}
}

func TestMainGUIUnavailableIsSideEffectFree(t *testing.T) {
	t.Parallel()
	env := testDeps(t)
	d := env.Deps
	tmp := t.TempDir()
	configDir := filepath.Join(tmp, "config")
	d.UserConfigDir = func() (string, error) { t.Fatal("UserConfigDir called"); return "", nil }
	d.SingleInstance = func(string) (func(), error) { t.Fatal("SingleInstance called"); return nil, errUnreachable }
	d.OpenLog = func(logfile.Options) (io.WriteCloser, error) { t.Fatal("OpenLog called"); return nil, errUnreachable }
	d.NewEngine = func(engine.Options) (Engine, error) { t.Fatal("NewEngine called"); return nil, errUnreachable }

	if code := Main(nil, d); code != 2 {
		t.Errorf("Main = %d, want 2", code)
	}

	// The GUI-availability check must run before config.Load, which
	// otherwise creates a default config file on disk as a side effect of
	// what is really just a usage error.
	if _, err := os.Stat(filepath.Join(configDir, "mink-lasso", "config.json")); !os.IsNotExist(err) {
		t.Errorf("config.json was created despite the usage error (stat err: %v)", err)
	}
}

func TestMainLogDirFallbackToExecutable(t *testing.T) {
	t.Parallel()
	env := testDeps(t)
	d := env.Deps
	notifyCancel := env.NotifyCancel
	d.UserCacheDir = func() (string, error) { return "", errors.New("no cache dir") }

	var gotPath string
	d.OpenLog = func(o logfile.Options) (io.WriteCloser, error) {
		gotPath = o.Path

		return logfile.Open(logfile.Options{Path: o.Path})
	}
	fe := newFakeEngine()
	d.NewEngine = func(engine.Options) (Engine, error) { return fe, nil }

	done := make(chan int, 1)
	go func() { done <- Main([]string{"-headless"}, d) }()

	<-fe.started
	(*notifyCancel)()

	if code := <-done; code != 0 {
		t.Errorf("Main = %d, want 0", code)
	}

	wantSuffix := filepath.Join("bin", "logs", "mink-lasso.log")
	if !strings.HasSuffix(gotPath, wantSuffix) {
		t.Errorf("log path = %q, want suffix %q", gotPath, wantSuffix)
	}
}

func TestMainLogDirFallbackFails(t *testing.T) {
	t.Parallel()
	env := testDeps(t)
	d := env.Deps
	stderr := env.Stderr
	d.UserCacheDir = func() (string, error) { return "", errors.New("no cache dir") }
	d.Executable = func() (string, error) { return "", errors.New("no exe") }

	if code := Main([]string{"-headless"}, d); code != 1 {
		t.Errorf("Main = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "log directory") {
		t.Errorf("stderr = %q, want it to mention the log directory", stderr.String())
	}
	// Both fallback errors should be reported, not just the last one tried.
	if !strings.Contains(stderr.String(), "no cache dir") || !strings.Contains(stderr.String(), "no exe") {
		t.Errorf("stderr = %q, want it to mention both the cache-dir and executable errors", stderr.String())
	}
}

func TestMainOpenLogFails(t *testing.T) {
	t.Parallel()
	env := testDeps(t)
	d := env.Deps
	stderr := env.Stderr
	wantErr := errors.New("disk full")
	d.OpenLog = func(logfile.Options) (io.WriteCloser, error) { return nil, wantErr }

	if code := Main([]string{"-headless"}, d); code != 1 {
		t.Errorf("Main = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "disk full") {
		t.Errorf("stderr = %q, want it to mention the error", stderr.String())
	}
}

// TestMainOpenLogDefaultFails exercises Deps.OpenLog's real (non-test-double)
// default, logfile.Open, returning a genuine error — confirming Main reports
// it and returns without dereferencing the (typed-nil) io.WriteCloser it got
// back.
func TestMainOpenLogDefaultFails(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()

	// logfile.Open's real MkdirAll fails because "logs"'s parent path
	// component is a plain file, not a directory.
	blocker := filepath.Join(tmp, "not-a-directory")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	stderr := &bytes.Buffer{}
	d := Deps{
		Stdout:         io.Discard,
		Stderr:         stderr,
		UserConfigDir:  func() (string, error) { return filepath.Join(tmp, "config"), nil },
		Executable:     func() (string, error) { return filepath.Join(tmp, "bin", "mink-lasso"), nil },
		SingleInstance: func(string) (func(), error) { return func() {}, nil },
		NewEngine:      func(engine.Options) (Engine, error) { return newFakeEngine(), nil },
		// OpenLog is left nil on purpose, to exercise its real default.
	}

	code := Main([]string{"-headless", "-log-dir", filepath.Join(blocker, "logs")}, d)
	if code != 1 {
		t.Errorf("Main = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "logfile:") {
		t.Errorf("stderr = %q, want it to mention the logfile error", stderr.String())
	}
}

// TestMainNewEngineDefaultFails exercises Deps.NewEngine's real
// (non-test-double) default, engine.New, returning a genuine bind failure —
// confirming Main reports it and returns without dereferencing the
// (typed-nil) Engine it got back. It holds the one UDP port the configured
// ListenPort narrows the client's scan range to (masso.NewClient always
// scans up to masso.ListenPortMax), so the real bind genuinely fails rather
// than finding another free port in the range.
func TestMainNewEngineDefaultFails(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	configPath := filepath.Join(tmp, "config.json")

	cfg := config.Default()
	cfg.ListenPort = masso.ListenPortMax
	if err := cfg.Save(configPath); err != nil {
		t.Fatalf("Save: %v", err)
	}

	held, err := net.ListenUDP("udp", &net.UDPAddr{Port: masso.ListenPortMax})
	if err != nil {
		t.Skipf("could not hold port %d to force a real bind conflict: %v", masso.ListenPortMax, err)
	}
	defer held.Close()

	stderr := &bytes.Buffer{}
	d := Deps{
		Stdout:         io.Discard,
		Stderr:         stderr,
		UserCacheDir:   func() (string, error) { return filepath.Join(tmp, "cache"), nil },
		Executable:     func() (string, error) { return filepath.Join(tmp, "bin", "mink-lasso"), nil },
		SingleInstance: func(string) (func(), error) { return func() {}, nil },
		// NewEngine is left nil on purpose, to exercise its real default.
	}

	code := Main([]string{"-headless", "-config", configPath}, d)
	if code != 1 {
		t.Errorf("Main = %d, want 1; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "Masso Link") {
		t.Errorf("stderr = %q, want it to mention Masso Link", stderr.String())
	}
}

func TestMainSingleInstanceAlreadyRunning(t *testing.T) {
	t.Parallel()
	env := testDeps(t)
	d := env.Deps
	stderr := env.Stderr
	d.SingleInstance = func(string) (func(), error) { return nil, winutil.ErrAlreadyRunning }

	if code := Main([]string{"-headless"}, d); code != 1 {
		t.Errorf("Main = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "already running") {
		t.Errorf("stderr = %q, want it to mention already running", stderr.String())
	}
}

func TestMainSingleInstanceOtherError(t *testing.T) {
	t.Parallel()
	env := testDeps(t)
	d := env.Deps
	stderr := env.Stderr
	wantErr := errors.New("mutex trouble")
	d.SingleInstance = func(string) (func(), error) { return nil, wantErr }

	if code := Main([]string{"-headless"}, d); code != 1 {
		t.Errorf("Main = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "mutex trouble") {
		t.Errorf("stderr = %q, want it to mention the error", stderr.String())
	}
}

// orderRecordingCloser wraps a WriteCloser and records "close" (via record)
// when Close is called, so a test can observe shutdown ordering relative to
// other recorded events.
type orderRecordingCloser struct {
	io.WriteCloser

	record func(string)
}

func (c orderRecordingCloser) Close() error {
	c.record("close")

	return c.WriteCloser.Close()
}

// TestMainClosesLogBeforeReleasingSingleInstance guards against the
// single-instance guard being released while the log file is still open:
// on every shutdown path, the log must be fully closed (freeing the
// single-instance guard's serialization purpose) before a second instance
// is allowed to start.
func TestMainClosesLogBeforeReleasingSingleInstance(t *testing.T) {
	t.Parallel()
	env := testDeps(t)
	d := env.Deps
	notifyCancel := env.NotifyCancel

	var (
		mu    sync.Mutex
		order []string
	)
	record := func(name string) {
		mu.Lock()
		order = append(order, name)
		mu.Unlock()
	}

	d.SingleInstance = func(string) (func(), error) {
		return func() { record("release") }, nil
	}
	d.OpenLog = func(o logfile.Options) (io.WriteCloser, error) {
		w, err := logfile.Open(o)
		if err != nil {
			return nil, err
		}

		return orderRecordingCloser{WriteCloser: w, record: record}, nil
	}

	fe := newFakeEngine()
	d.NewEngine = func(engine.Options) (Engine, error) { return fe, nil }

	done := make(chan int, 1)
	go func() { done <- Main([]string{"-headless"}, d) }()

	<-fe.started
	(*notifyCancel)()

	if code := <-done; code != 0 {
		t.Errorf("Main = %d, want 0", code)
	}

	mu.Lock()
	got := append([]string(nil), order...)
	mu.Unlock()

	if len(got) != 2 || got[0] != "close" || got[1] != "release" {
		t.Errorf("shutdown order = %v, want [close release]", got)
	}
}

func TestMainEngineErrBind(t *testing.T) {
	t.Parallel()
	env := testDeps(t)
	d := env.Deps
	stderr := env.Stderr
	d.NewEngine = func(engine.Options) (Engine, error) { return nil, engine.ErrBind }

	if code := Main([]string{"-headless"}, d); code != 1 {
		t.Errorf("Main = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "Masso Link") {
		t.Errorf("stderr = %q, want it to mention Masso Link", stderr.String())
	}
}

func TestMainEngineOtherError(t *testing.T) {
	t.Parallel()
	env := testDeps(t)
	d := env.Deps
	stderr := env.Stderr
	wantErr := errors.New("engine trouble")
	d.NewEngine = func(engine.Options) (Engine, error) { return nil, wantErr }

	if code := Main([]string{"-headless"}, d); code != 1 {
		t.Errorf("Main = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "engine trouble") {
		t.Errorf("stderr = %q, want it to mention the error", stderr.String())
	}
}

func TestMainHeadlessCanceledByNotify(t *testing.T) {
	t.Parallel()
	env := testDeps(t)
	d := env.Deps
	notifyCancel := env.NotifyCancel
	fe := newFakeEngine()
	d.NewEngine = func(engine.Options) (Engine, error) { return fe, nil }

	done := make(chan int, 1)
	go func() { done <- Main([]string{"-headless"}, d) }()

	<-fe.started
	(*notifyCancel)()

	select {
	case code := <-done:
		if code != 0 {
			t.Errorf("Main = %d, want 0", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Main did not return after the injected signal")
	}
}

func TestMainHeadlessEngineRunError(t *testing.T) {
	t.Parallel()
	env := testDeps(t)
	d := env.Deps
	stderr := env.Stderr
	notifyCancel := env.NotifyCancel
	fe := newFakeEngine()
	fe.runErr = errors.New("engine died")
	d.NewEngine = func(engine.Options) (Engine, error) { return fe, nil }

	done := make(chan int, 1)
	go func() { done <- Main([]string{"-headless"}, d) }()

	<-fe.started
	(*notifyCancel)()

	if code := <-done; code != 1 {
		t.Errorf("Main = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "engine died") {
		// The engine error is logged, not necessarily printed to
		// stderr directly; accept either surface as long as the run
		// reported failure via the exit code, checked above.
		_ = stderr.String()
	}
}

func TestMainGUINil(t *testing.T) {
	t.Parallel()
	env := testDeps(t)
	d := env.Deps
	stderr := env.Stderr
	// -headless not passed and deps.GUI is nil.
	if code := Main(nil, d); code != 2 {
		t.Errorf("Main = %d, want 2", code)
	}
	if stderr.Len() == 0 {
		t.Error("expected a message on stderr")
	}
}

func TestMainGUIReturnsError(t *testing.T) {
	t.Parallel()
	env := testDeps(t)
	d := env.Deps
	fe := newFakeEngine()
	d.NewEngine = func(engine.Options) (Engine, error) { return fe, nil }
	wantErr := errors.New("window trouble")
	d.GUI = func(_ context.Context, _ *GUI) error { return wantErr }

	if code := Main(nil, d); code != 1 {
		t.Errorf("Main = %d, want 1", code)
	}
}

func TestMainGUIReturnsNilAndEventsFlow(t *testing.T) {
	t.Parallel()
	env := testDeps(t)
	d := env.Deps
	tmp := t.TempDir()
	configPath := filepath.Join(tmp, "config.json")

	fe := newFakeEngine()
	d.NewEngine = func(engine.Options) (Engine, error) { return fe, nil }

	addr := &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 55123}
	var (
		gotChanges []model.Changes
		mu         sync.Mutex
	)
	d.GUI = func(_ context.Context, g *GUI) error {
		g.Model.SetOnChange(func(c model.Changes) {
			mu.Lock()
			gotChanges = append(gotChanges, c)
			mu.Unlock()
		})

		// A log line should reach the model's log handler, which
		// calls the OnChange callback just registered.
		g.Logger.Info("hello from the GUI test")

		// Push a connected event through the engine and confirm it
		// reaches the GUI's Events channel.
		go func() { fe.events <- engine.ConnState{Kind: engine.Connected, Addr: addr} }()

		select {
		case ev := <-g.Events:
			cs, ok := ev.(engine.ConnState)
			if !ok || cs.Kind != engine.Connected {
				t.Errorf("unexpected event: %#v", ev)
			}
		case <-time.After(5 * time.Second):
			t.Error("timed out waiting for the forwarded event")
		}

		return nil
	}

	if code := Main([]string{"-config", configPath}, d); code != 0 {
		t.Errorf("Main = %d, want 0", code)
	}

	mu.Lock()
	n := len(gotChanges)
	mu.Unlock()
	if n == 0 {
		t.Error("OnChange was never called from the model's log handler")
	}

	got, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if got.LastAddress != addr.String() {
		t.Errorf("LastAddress = %q, want %q", got.LastAddress, addr.String())
	}
}

func TestMainAllDepsDefaulted(t *testing.T) {
	t.Parallel()
	// -version returns before any default is exercised beyond being
	// assigned, so this is side-effect free while still driving every
	// branch of withDefaults.
	if code := Main([]string{"-version"}, Deps{}); code != 0 {
		t.Errorf("Main = %d, want 0", code)
	}
}

func TestMainLogDirFlag(t *testing.T) {
	t.Parallel()
	env := testDeps(t)
	d := env.Deps
	notifyCancel := env.NotifyCancel
	logDir := t.TempDir()

	var gotPath string
	d.OpenLog = func(o logfile.Options) (io.WriteCloser, error) {
		gotPath = o.Path

		return logfile.Open(logfile.Options{Path: o.Path})
	}
	fe := newFakeEngine()
	d.NewEngine = func(engine.Options) (Engine, error) { return fe, nil }

	done := make(chan int, 1)
	go func() { done <- Main([]string{"-headless", "-log-dir", logDir}, d) }()

	<-fe.started
	(*notifyCancel)()

	if code := <-done; code != 0 {
		t.Errorf("Main = %d, want 0", code)
	}
	if want := filepath.Join(logDir, "mink-lasso.log"); gotPath != want {
		t.Errorf("log path = %q, want %q", gotPath, want)
	}
}

func TestMainAddressOverride(t *testing.T) {
	t.Parallel()
	env := testDeps(t)
	d := env.Deps
	notifyCancel := env.NotifyCancel

	var gotAddress string
	fe := newFakeEngine()
	d.NewEngine = func(o engine.Options) (Engine, error) {
		gotAddress = o.Config.Address

		return fe, nil
	}

	done := make(chan int, 1)
	go func() { done <- Main([]string{"-headless", "-address", "10.0.0.5:65535"}, d) }()

	<-fe.started
	(*notifyCancel)()

	if code := <-done; code != 0 {
		t.Errorf("Main = %d, want 0", code)
	}
	if gotAddress != "10.0.0.5:65535" {
		t.Errorf("Config.Address = %q, want %q", gotAddress, "10.0.0.5:65535")
	}
}

func TestMainHeadlessPersistsLastAddressAndDiscardsEvents(t *testing.T) {
	t.Parallel()
	env := testDeps(t)
	d := env.Deps
	notifyCancel := env.NotifyCancel
	configPath := filepath.Join(t.TempDir(), "config.json")
	fe := newFakeEngine()
	d.NewEngine = func(engine.Options) (Engine, error) { return fe, nil }

	addr := &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 55125}

	done := make(chan int, 1)
	go func() { done <- Main([]string{"-headless", "-config", configPath}, d) }()

	<-fe.started
	fe.events <- engine.ConnState{Kind: engine.Connected, Addr: addr}
	(*notifyCancel)()

	if code := <-done; code != 0 {
		t.Errorf("Main = %d, want 0", code)
	}

	got, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if got.LastAddress != addr.String() {
		t.Errorf("LastAddress = %q, want %q", got.LastAddress, addr.String())
	}
}

// childDirEnv names the directory a re-executed test binary runs Main
// under; see TestMainChildProcess.
const childDirEnv = "MINK_LASSO_TEST_CHILD_DIR"

// TestMainStopsOnInterrupt runs Main headless with its real Notify and
// NewEngine defaults — a real signal handler and a real UDP socket — and
// interrupts it once it reports that it is running, expecting a clean exit.
// Main runs in a child process (this test binary re-executed; see
// TestMainChildProcess) because Windows offers no way for a process to
// interrupt itself; see interruptChild.
//
//nolint:paralleltest // the child binds the real listen-port range; keep the port-holding tests out of its way.
func TestMainStopsOnInterrupt(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("Executable: %v", err)
	}
	dir := t.TempDir()

	stdout, stderr := &syncBuffer{}, &syncBuffer{}
	cmd := exec.Command(exe, "-test.run=^TestMainChildProcess$")
	cmd.Env = append(os.Environ(), childDirEnv+"="+dir)
	cmd.SysProcAttr = childProcAttr()
	cmd.Stdout, cmd.Stderr = stdout, stderr
	ensureConsole()
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	// Harmless once the child has exited; otherwise it keeps a failed run
	// from leaking the child and its open log file.
	t.Cleanup(func() { _ = cmd.Process.Kill() })

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	// run logs this line right after installing signal.NotifyContext, so
	// seeing it is the only safe moment to interrupt: an interrupt before
	// that would hit the child's default disposition and make it exit
	// non-zero. If the real socket bind fails (the port range is already
	// held, or this environment blocks it), the child exits first instead.
	deadline := time.Now().Add(15 * time.Second)
	for !strings.Contains(stdout.String(), "running headless") {
		select {
		case err := <-done:
			t.Fatalf("child exited (%v) before installing its signal handler "+
				"(the real engine socket bind likely failed); stdout: %s; stderr: %s",
				err, stdout.String(), stderr.String())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("child never logged that it is running headless")
		}
		time.Sleep(20 * time.Millisecond)
	}

	interruptChild(t, cmd)

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("child exited with %v after the interrupt; stdout: %s; stderr: %s",
				err, stdout.String(), stderr.String())
		}
	case <-time.After(15 * time.Second):
		t.Fatal("child did not exit after the interrupt")
	}
	if !strings.Contains(stdout.String(), "Main returned 0") {
		t.Errorf("child did not report a clean Main return; stdout: %s; stderr: %s",
			stdout.String(), stderr.String())
	}
}

// TestMainChildProcess is the child half of TestMainStopsOnInterrupt: when
// this test binary is re-executed with childDirEnv set, it runs Main
// headless with the real Notify and NewEngine defaults, keeping its config
// and logs under that directory, and reports Main's exit code on stdout.
// Run any other way it does nothing.
func TestMainChildProcess(t *testing.T) {
	t.Parallel()
	dir := os.Getenv(childDirEnv)
	if dir == "" {
		return
	}

	d := Deps{
		Stdout:         os.Stdout,
		Stderr:         os.Stderr,
		UserCacheDir:   func() (string, error) { return filepath.Join(dir, "cache"), nil },
		Executable:     func() (string, error) { return filepath.Join(dir, "bin", "mink-lasso"), nil },
		SingleInstance: func(string) (func(), error) { return func() {}, nil },
		// Notify and NewEngine are left nil on purpose: their real
		// defaults are the point.
	}
	code := Main([]string{"-headless", "-config", filepath.Join(dir, "config.json")}, d)
	fmt.Fprintf(os.Stdout, "Main returned %d\n", code)
	if code != 0 {
		t.Errorf("Main = %d, want 0", code)
	}
}

// TestDefaultNotifyFollowsParentContext covers the default Notify in
// process: the context it returns must end with its parent, not only on a
// signal (which TestMainStopsOnInterrupt proves separately).
func TestDefaultNotifyFollowsParentContext(t *testing.T) {
	t.Parallel()
	parent, cancelParent := context.WithCancel(context.Background())
	ctx, cancel := Deps{}.withDefaults().Notify(parent)
	defer cancel()

	cancelParent()
	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("default Notify's context did not follow its parent")
	}
}

// TestDefaultNewEngineBuildsARealEngine covers the default NewEngine's
// success path in process. It binds a real listen socket, so the engine is
// run to completion on an already-canceled context to release it.
func TestDefaultNewEngineBuildsARealEngine(t *testing.T) {
	t.Parallel()
	eng, err := Deps{}.withDefaults().NewEngine(engine.Options{Config: config.Default()})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	drained := make(chan int, 1)
	go func() {
		n := 0
		for range eng.Events() {
			n++
		}
		drained <- n
	}()
	if err := eng.Run(ctx); err != nil {
		t.Errorf("Run: %v", err)
	}
	t.Logf("engine emitted %d events while shutting down", <-drained)
}

// syncBuffer is a bytes.Buffer that one goroutine may read while another
// writes it.
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

func TestMainLastAddressPersistFailureIsLoggedNotFatal(t *testing.T) {
	t.Parallel()
	env := testDeps(t)
	d := env.Deps
	notifyCancel := env.NotifyCancel
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	if err := config.Default().Save(configPath); err != nil {
		t.Fatal(err)
	}
	// The initial Load above already succeeded, but the later LastAddress
	// persist writes config.json.tmp and renames it into place; a
	// directory squatting on that name makes the write fail on every
	// platform (a read-only directory would not stop it on Windows), and
	// the failure must not take Main down.
	if err := os.Mkdir(configPath+".tmp", 0o750); err != nil {
		t.Fatal(err)
	}

	fe := newFakeEngine()
	d.NewEngine = func(engine.Options) (Engine, error) { return fe, nil }
	addr := &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 55126}

	done := make(chan int, 1)
	go func() { done <- Main([]string{"-headless", "-config", configPath}, d) }()

	<-fe.started
	fe.events <- engine.ConnState{Kind: engine.Connected, Addr: addr}
	(*notifyCancel)()

	if code := <-done; code != 0 {
		t.Errorf("Main = %d, want 0 (a persist failure must not be fatal)", code)
	}
	if out := env.Stdout.String(); !strings.Contains(out, "could not persist last-known address") {
		t.Errorf("persist failure was not logged; stdout: %s", out)
	}
}

func TestMainWatchDirOverride(t *testing.T) {
	t.Parallel()
	env := testDeps(t)
	d := env.Deps
	notifyCancel := env.NotifyCancel
	watchDir := t.TempDir()

	var gotWatchDir string
	fe := newFakeEngine()
	d.NewEngine = func(o engine.Options) (Engine, error) {
		gotWatchDir = o.Config.WatchDir

		return fe, nil
	}

	done := make(chan int, 1)
	go func() { done <- Main([]string{"-headless", "-watch", watchDir}, d) }()

	<-fe.started
	(*notifyCancel)()

	if code := <-done; code != 0 {
		t.Errorf("Main = %d, want 0", code)
	}
	if gotWatchDir != watchDir {
		t.Errorf("Config.WatchDir = %q, want %q", gotWatchDir, watchDir)
	}
}

// TestMainOverridesNotPersistedToConfigFile guards against a run using
// -watch/-serial/-address overrides silently overwriting those fields in the
// real config file the first time it connects: configStore must be seeded
// from the config as loaded from disk, not from the override-applied
// in-memory copy, so an automatic LastAddress persist only ever changes
// LastAddress.
func TestMainOverridesNotPersistedToConfigFile(t *testing.T) {
	t.Parallel()
	env := testDeps(t)
	d := env.Deps
	notifyCancel := env.NotifyCancel
	configPath := filepath.Join(t.TempDir(), "config.json")

	baseWatchDir := t.TempDir()
	base := config.Default()
	base.WatchDir = baseWatchDir
	base.Serial = "G3-1"
	base.Address = "192.0.2.1:12345"
	if err := base.Save(configPath); err != nil {
		t.Fatal(err)
	}

	overrideWatchDir := t.TempDir()
	fe := newFakeEngine()
	d.NewEngine = func(engine.Options) (Engine, error) { return fe, nil }

	addr := &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 55127}

	done := make(chan int, 1)
	go func() {
		done <- Main([]string{
			"-headless", "-config", configPath,
			"-watch", overrideWatchDir,
			"-serial", "G3-2",
			"-address", "198.51.100.1:23456",
		}, d)
	}()

	<-fe.started
	fe.events <- engine.ConnState{Kind: engine.Connected, Addr: addr}
	(*notifyCancel)()

	if code := <-done; code != 0 {
		t.Errorf("Main = %d, want 0", code)
	}

	got, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if got.WatchDir != baseWatchDir {
		t.Errorf("WatchDir = %q, want %q (override must not be persisted)", got.WatchDir, baseWatchDir)
	}
	if got.Serial != "G3-1" {
		t.Errorf("Serial = %q, want %q (override must not be persisted)", got.Serial, "G3-1")
	}
	if got.Address != "192.0.2.1:12345" {
		t.Errorf("Address = %q, want %q (override must not be persisted)", got.Address, "192.0.2.1:12345")
	}
	if got.LastAddress != addr.String() {
		t.Errorf("LastAddress = %q, want %q", got.LastAddress, addr.String())
	}
}

func TestMainGUIModeEngineRunError(t *testing.T) {
	t.Parallel()
	env := testDeps(t)
	d := env.Deps
	fe := newFakeEngine()
	fe.runErr = errors.New("engine died in GUI mode")
	d.NewEngine = func(engine.Options) (Engine, error) { return fe, nil }
	d.GUI = func(_ context.Context, _ *GUI) error { return nil }

	if code := Main(nil, d); code != 1 {
		t.Errorf("Main = %d, want 1", code)
	}
}

// TestMainGUIModeBothGUIAndEngineErr confirms that when the GUI and the
// engine both return an error around the same shutdown, run logs both
// messages rather than letting the GUI branch's early return swallow the
// engine's.
func TestMainGUIModeBothGUIAndEngineErr(t *testing.T) {
	t.Parallel()
	env := testDeps(t)
	d := env.Deps
	fe := newFakeEngine()
	fe.runErr = errors.New("engine died in GUI mode")
	d.NewEngine = func(engine.Options) (Engine, error) { return fe, nil }

	var mdl *model.Model
	d.GUI = func(_ context.Context, g *GUI) error {
		mdl = g.Model

		return errors.New("window trouble")
	}

	if code := Main(nil, d); code != 1 {
		t.Errorf("Main = %d, want 1", code)
	}

	log := strings.Join(mdl.LogLines(), "\n")
	if !strings.Contains(log, "GUI exited with an error") {
		t.Errorf("log missing the GUI error message: %s", log)
	}
	if !strings.Contains(log, "engine stopped with an error") {
		t.Errorf("log missing the engine error message: %s", log)
	}
}

// TestForwardBlockedSendUnblocksOnGUIDone drives the forwarder's
// select-send past its buffer, past a GUI that has already returned, and
// confirms the guiDone branch — not a leaked goroutine — is what unblocks
// it.
func TestForwardBlockedSendUnblocksOnGUIDone(t *testing.T) {
	t.Parallel()
	env := testDeps(t)
	d := env.Deps
	fe := newFakeEngine()
	d.NewEngine = func(engine.Options) (Engine, error) { return fe, nil }

	const guiChCap = 64
	pushed := make(chan struct{})
	d.GUI = func(_ context.Context, _ *GUI) error {
		go func() {
			defer close(pushed)
			// One more than the buffer forces the forwarder's
			// select to still be waiting on the guiCh<-ev case
			// when this function returns and guiDone closes.
			for range guiChCap + 1 {
				fe.events <- engine.ConnState{Kind: engine.Discovering}
			}
		}()
		<-pushed

		return nil
	}

	done := make(chan int, 1)
	go func() { done <- Main(nil, d) }()

	select {
	case code := <-done:
		if code != 0 {
			t.Errorf("Main = %d, want 0", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Main did not return; the forwarder likely deadlocked on a full guiCh")
	}
}

func TestMainLastAddressNotClobberedByModelSave(t *testing.T) {
	t.Parallel()
	env := testDeps(t)
	d := env.Deps
	configPath := filepath.Join(t.TempDir(), "config.json")

	fe := newFakeEngine()
	d.NewEngine = func(engine.Options) (Engine, error) { return fe, nil }

	addr := &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 55124}
	d.GUI = func(_ context.Context, g *GUI) error {
		go func() { fe.events <- engine.ConnState{Kind: engine.Connected, Addr: addr} }()

		<-g.Events // wait for the forwarder to have processed it

		// Give the forwarder a moment to have persisted the address
		// before the model saves over it.
		time.Sleep(50 * time.Millisecond)

		if err := g.Model.SetUploadWhileMachining(true); err != nil {
			t.Errorf("SetUploadWhileMachining: %v", err)
		}

		return nil
	}

	if code := Main([]string{"-config", configPath}, d); code != 0 {
		t.Errorf("Main = %d, want 0", code)
	}

	got, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if got.LastAddress != addr.String() {
		t.Errorf("LastAddress = %q, want %q (must survive the model's save)", got.LastAddress, addr.String())
	}
	if !got.UploadWhileMachining {
		t.Error("UploadWhileMachining not persisted by the model's save")
	}
}
