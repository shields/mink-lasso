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
	"context"
	"log/slog"

	"msrl.dev/mink-lasso/internal/config"
	"msrl.dev/mink-lasso/internal/engine"
	"msrl.dev/mink-lasso/internal/ui/model"
)

// run drives the engine and, in GUI mode, the GUI, until shutdown, and
// returns Main's exit code. cfg, eng, mdl, store, and logger are all
// already built; f and deps carry the rest of what run needs.
func run(
	f flags,
	deps Deps,
	cfg config.Config,
	eng Engine,
	mdl *model.Model,
	store *configStore,
	logger *slog.Logger,
) int {
	ctx, cancel := deps.Notify(context.Background())
	defer cancel()

	runErrCh := make(chan error, 1)
	go func() { runErrCh <- eng.Run(ctx) }()

	guiCh := make(chan engine.Event, 64)
	guiDone := make(chan struct{})
	forwarderDone := make(chan struct{})

	go forward(eng, store, logger, f.headless, guiCh, guiDone, forwarderDone)

	var runErr error
	if f.headless {
		// The built exe has no console (see README.md's "Command line"
		// section), so Ctrl+C only reaches this process under `go run`;
		// taskkill/Task Manager works either way.
		logger.Info("running headless; stop it with taskkill, or Ctrl+C under go run")
		<-ctx.Done()
		cancel()
		runErr = <-runErrCh
		<-forwarderDone

		if runErr != nil {
			logger.Error("engine stopped with an error", "error", runErr)

			return 1
		}

		return 0
	}

	guiErr := deps.GUI(ctx, &GUI{Config: cfg, Model: mdl, Events: guiCh, Logger: logger})
	close(guiDone)
	cancel()
	runErr = <-runErrCh
	<-forwarderDone

	if guiErr != nil {
		logger.Error("GUI exited with an error", "error", guiErr)
	}
	if runErr != nil {
		logger.Error("engine stopped with an error", "error", runErr)
	}
	if guiErr != nil || runErr != nil {
		return 1
	}

	return 0
}

// forward drains eng.Events() until it is closed (required for eng.Run to
// return, per its doc comment), persisting a newly connected address to
// disk and, outside headless mode, relaying every event to guiCh without
// ever blocking a GUI that has already returned.
//
//nolint:revive // headless mirrors Main's own -headless mode switch, not a public API smell
func forward(
	eng Engine,
	store *configStore,
	logger *slog.Logger,
	headless bool,
	guiCh chan<- engine.Event,
	guiDone <-chan struct{},
	forwarderDone chan<- struct{},
) {
	defer close(forwarderDone)
	defer close(guiCh)

	for ev := range eng.Events() {
		if cs, isConn := ev.(engine.ConnState); isConn && cs.Kind == engine.Connected && cs.Addr != nil {
			if err := store.persistLastAddress(cs.Addr.String()); err != nil {
				logger.Warn("could not persist last-known address", "error", err)
			}
		}

		if headless {
			continue
		}

		select {
		case guiCh <- ev:
		case <-guiDone:
		}
	}
}
