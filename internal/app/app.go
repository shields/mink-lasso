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

// Package app owns mink-lasso's process lifecycle: flags, config, logging,
// the single-instance guard, and wiring the engine, the model, and (on
// Windows) the GUI together. It has no opinion about how anything looks —
// internal/ui/walkui paints, and internal/ui/model decides what there is to
// paint — and no opinion about CNC-specific behavior, which lives in
// internal/engine. Main is the whole of the package's public surface; every
// other exported name exists to satisfy the GUI/model construction contract
// or to be overridden by a test.
package app

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"msrl.dev/mink-lasso/internal/clock"
	"msrl.dev/mink-lasso/internal/config"
	"msrl.dev/mink-lasso/internal/engine"
	"msrl.dev/mink-lasso/internal/logfile"
	"msrl.dev/mink-lasso/internal/ui/model"
	"msrl.dev/mink-lasso/internal/winutil"
)

// version is set by -ldflags at build time; "dev" otherwise.
var version = "dev"

// Engine is the subset of *engine.Engine that Main drives. It exists so
// tests can substitute a fake for the fast paths; at least one test still
// exercises the real engine against internal/masso/sim end to end.
type Engine interface {
	Run(ctx context.Context) error
	Events() <-chan engine.Event
	model.Control
}

// GUI is handed to Deps.GUI once the engine and model are built. Field
// order and names are a fixed contract with internal/ui/walkui and
// cmd/mink-lasso/gui_windows.go; do not change them.
type GUI struct {
	Config config.Config
	Model  *model.Model
	Events <-chan engine.Event
	Logger *slog.Logger
}

// Deps collects Main's side effects so tests can substitute fakes. Every
// field is optional; a nil field takes the documented default.
type Deps struct {
	// GUI runs the GUI's message loop on the calling goroutine, returning
	// when the user exits or ctx is done. Nil means the GUI is
	// unavailable on this platform (cmd/mink-lasso/gui_other.go leaves
	// it nil off Windows).
	GUI func(ctx context.Context, g *GUI) error

	// Stdout and Stderr default to os.Stdout and os.Stderr.
	Stdout, Stderr io.Writer

	// Notify returns a context canceled on an interrupt/termination
	// signal. Nil means signal.NotifyContext(ctx, os.Interrupt,
	// syscall.SIGTERM).
	Notify func(context.Context) (context.Context, context.CancelFunc)

	// UserConfigDir, UserCacheDir, and Executable default to os.UserConfigDir,
	// os.UserCacheDir, and os.Executable.
	UserConfigDir, UserCacheDir, Executable func() (string, error)

	// SingleInstance guards against a second instance running. Nil means
	// winutil.SingleInstance.
	SingleInstance func(string) (func(), error)

	// OpenLog opens the rotating log file. Nil means logfile.Open.
	OpenLog func(logfile.Options) (io.WriteCloser, error)

	// NewEngine constructs the engine. Nil means engine.New.
	NewEngine func(engine.Options) (Engine, error)

	// Clock supplies every timeout used by the engine and, indirectly,
	// the model. Nil means clock.Real{}.
	Clock clock.Clock
}

func (d Deps) withDefaults() Deps {
	if d.Stdout == nil {
		d.Stdout = os.Stdout
	}
	if d.Stderr == nil {
		d.Stderr = os.Stderr
	}
	if d.Notify == nil {
		d.Notify = func(ctx context.Context) (context.Context, context.CancelFunc) {
			return signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
		}
	}
	if d.UserConfigDir == nil {
		d.UserConfigDir = os.UserConfigDir
	}
	if d.UserCacheDir == nil {
		d.UserCacheDir = os.UserCacheDir
	}
	if d.Executable == nil {
		d.Executable = os.Executable
	}
	if d.SingleInstance == nil {
		d.SingleInstance = winutil.SingleInstance
	}
	if d.OpenLog == nil {
		// Not just "return logfile.Open(o)": that implicitly converts a nil
		// *logfile.Writer into a non-nil io.WriteCloser holding a nil
		// pointer, so an err != nil caller-side check would no longer be a
		// reliable guard against later using the value.
		d.OpenLog = func(o logfile.Options) (io.WriteCloser, error) {
			w, err := logfile.Open(o)
			if err != nil {
				return nil, err
			}

			return w, nil
		}
	}
	if d.NewEngine == nil {
		// See the OpenLog default above: engine.New's *engine.Engine has the
		// same nil-vs-typed-nil hazard when returned through the Engine
		// interface.
		d.NewEngine = func(o engine.Options) (Engine, error) {
			e, err := engine.New(o)
			if err != nil {
				return nil, err
			}

			return e, nil
		}
	}
	if d.Clock == nil {
		d.Clock = clock.Real{}
	}

	return d
}

// flags holds the parsed command line, documented in README.md.
type flags struct {
	headless    bool
	configPath  string
	logDir      string
	watchDir    string
	serial      string
	address     string
	logLevel    string
	showVersion bool
}

// parseFlags parses args into a flags value. The returned int is the exit
// code to use when ok is false: 0 for -h/-help, 2 for any other parse
// error.
func parseFlags(name string, args []string, stderr io.Writer) (f flags, code int, ok bool) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.BoolVar(&f.headless, "headless", false, "run without a GUI, logging to the log file and stdout")
	fs.StringVar(&f.configPath, "config", "", "path to the config file (default: per-user config dir)")
	fs.StringVar(&f.logDir, "log-dir", "", "directory for log files (default: per-user cache dir)")
	fs.StringVar(&f.watchDir, "watch", "", "override the configured watch folder for this run")
	fs.StringVar(&f.serial, "serial", "", "override the configured controller serial (e.g. G3-12345) for this run")
	fs.StringVar(&f.address, "address", "", "override the configured controller address (host:port) for this run")
	fs.StringVar(&f.logLevel, "log-level", "", "override the configured log level for this run")
	fs.BoolVar(&f.showVersion, "version", false, "print the version and exit")

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return flags{}, 0, false
		}

		return flags{}, 2, false
	}

	return f, 0, true
}

// diagf writes a "mink-lasso: "-prefixed diagnostic to w. Every caller
// discards the write error deliberately: in -headless mode w may be a
// console-less process's stdout/stderr, which has nowhere to report a
// failure to write a failure report to.
func diagf(w io.Writer, format string, args ...any) {
	fmt.Fprintf(w, "mink-lasso: "+format+"\n", args...) //nolint:errcheck // best-effort diagnostic
}

// failWith reports err to w — friendly, verbatim, if err matches sentinel;
// the generic "mink-lasso: <err>" diagnostic otherwise — and returns Main's
// exit code for a runtime failure.
func failWith(w io.Writer, err, sentinel error, friendly string) int {
	if errors.Is(err, sentinel) {
		fmt.Fprintln(w, friendly) //nolint:errcheck // best-effort diagnostic
	} else {
		diagf(w, "%v", err)
	}

	return 1
}

// loadedConfig is loadConfig's result.
type loadedConfig struct {
	// Cfg is the config as loaded from disk with flag overrides applied
	// and validated; it is what the engine and model run with.
	Cfg config.Config
	// Raw is Cfg exactly as loaded from disk, before any flag override.
	// It is what newConfigStore must be seeded with, so that a
	// LastAddress persisted automatically on a later connection (see
	// configStore.persistLastAddress) writes back the real on-disk
	// settings rather than a run's throwaway
	// -watch/-serial/-address/-log-level overrides.
	Raw config.Config
	// Path is the file Cfg and Raw were loaded from.
	Path string
}

// loadConfig resolves the config path (flag override or config.DefaultPath),
// loads it, and applies flag overrides. Any failure is already reported to
// deps.Stderr before it is returned; the caller only needs to turn a
// non-nil error into an exit code.
func loadConfig(f flags, deps Deps) (loadedConfig, error) {
	path := f.configPath
	if path == "" {
		p, pathErr := config.DefaultPath(deps.UserConfigDir, deps.Executable)
		if pathErr != nil {
			diagf(deps.Stderr, "%v", pathErr)

			return loadedConfig{}, pathErr
		}
		path = p
	}

	raw, err := config.Load(path)
	if err != nil {
		diagf(deps.Stderr, "%s: %v", path, err)

		return loadedConfig{}, err
	}

	// raw is already normalized (config.Load's job); none of the four
	// overrides below touch Extensions, the only field Normalize changes,
	// so cfg needs no second pass.
	cfg := raw
	if f.watchDir != "" {
		cfg.WatchDir = f.watchDir
	}
	if f.serial != "" {
		cfg.Serial = f.serial
	}
	if f.address != "" {
		cfg.Address = f.address
	}
	if f.logLevel != "" {
		cfg.LogLevel = f.logLevel
	}

	if err := cfg.Validate(); err != nil {
		diagf(deps.Stderr, "%s: %v", path, err)

		return loadedConfig{}, err
	}

	return loadedConfig{Cfg: cfg, Raw: raw, Path: path}, nil
}

// resolveLogDir applies the same "per-user dir, else next to the
// executable" fallback config.DefaultPath uses, for the log directory.
func resolveLogDir(f flags, deps Deps) (string, error) {
	if f.logDir != "" {
		return f.logDir, nil
	}
	dir, cacheErr := deps.UserCacheDir()
	if cacheErr == nil {
		return filepath.Join(dir, "mink-lasso", "logs"), nil
	}
	exe, exeErr := deps.Executable()
	if exeErr != nil {
		return "", fmt.Errorf("cannot determine log directory: %w; %w", cacheErr, exeErr)
	}

	return filepath.Join(filepath.Dir(exe), "logs"), nil
}

// Main runs mink-lasso: it is the entire program, parameterized by deps for
// testing. It returns 0 on a clean exit, 1 on a runtime failure, and 2 on a
// usage error or an unsupported GUI request.
func Main(args []string, deps Deps) int {
	deps = deps.withDefaults()

	f, code, ok := parseFlags("mink-lasso", args, deps.Stderr)
	if !ok {
		return code
	}

	if f.showVersion {
		fmt.Fprintln(deps.Stdout, version) //nolint:errcheck // best-effort diagnostic

		return 0
	}

	// Checked before loadConfig: it depends only on f.headless and
	// deps.GUI, and a usage error should be free of side effects, but
	// config.Load creates a default config file on disk when none exists.
	if !f.headless && deps.GUI == nil {
		fmt.Fprintln(deps.Stderr, //nolint:errcheck // best-effort diagnostic
			"the GUI is only available on Windows; run with -headless")

		return 2
	}

	lc, err := loadConfig(f, deps)
	if err != nil {
		return 1
	}
	cfg, path := lc.Cfg, lc.Path

	logDir, err := resolveLogDir(f, deps)
	if err != nil {
		diagf(deps.Stderr, "%v", err)

		return 1
	}
	logPath := filepath.Join(logDir, "mink-lasso.log")

	// Acquired before OpenLog so that, on every return path below, the
	// deferred logWriter.Close() (registered after, so it runs first) has
	// closed the log file before the deferred release() (registered
	// first, so it runs last) lets a second instance start. This also
	// skips creating the log file at all when another instance already
	// holds it.
	release, err := deps.SingleInstance("mink-lasso")
	if err != nil {
		return failWith(deps.Stderr, err, winutil.ErrAlreadyRunning, "mink-lasso is already running")
	}
	defer release()

	logWriter, err := deps.OpenLog(logfile.Options{Path: logPath})
	if err != nil {
		diagf(deps.Stderr, "%s: %v", logPath, err)

		return 1
	}
	defer logWriter.Close() //nolint:errcheck // best-effort on exit; nothing more to do about it

	store := newConfigStore(path, lc.Raw)

	// The engine's logger must include the model's LogHandler, so the model
	// is built first and pointed at the engine below; the GUI registers its
	// OnChange callback itself through Model.SetOnChange.
	mdl := model.New(model.Options{
		Config:  cfg,
		Save:    store.save,
		Clock:   deps.Clock,
		Version: version,
	})

	level := cfg.SlogLevel()
	handlers := []slog.Handler{
		slog.NewTextHandler(logWriter, &slog.HandlerOptions{Level: level}),
		mdl.LogHandler(level),
	}
	if f.headless {
		handlers = append(handlers, slog.NewTextHandler(deps.Stdout, &slog.HandlerOptions{Level: level}))
	}
	logger := slog.New(slog.NewMultiHandler(handlers...))
	// Also routes github.com/tailscale/walk's own internal log.Print/
	// log.Fatal calls (it doesn't use slog) through these same handlers,
	// rather than to a console this windowsgui-subsystem binary doesn't
	// have.
	slog.SetDefault(logger)

	eng, err := deps.NewEngine(engine.Options{Config: cfg, Logger: logger, Clock: deps.Clock})
	if err != nil {
		return failWith(deps.Stderr, err, engine.ErrBind,
			"mink-lasso: could not bind the Masso UDP port range; it is most likely in use "+
				"by another program, such as Masso Link")
	}
	mdl.SetEngine(eng)

	mode := "gui"
	if f.headless {
		mode = "headless"
	}
	logger.Info("starting mink-lasso", "version", version, "config", path, "log", logPath, "mode", mode)

	return run(f, deps, cfg, eng, mdl, store, logger)
}
