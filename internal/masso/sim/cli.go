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

package sim

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"math"
	"strings"
)

// Main implements cmd/masso-sim: it parses args, starts a Controller
// logging to stdout, and serves until ctx is done. It returns 0 on a normal
// (context-canceled) exit, 2 for a flag error, or 1 if the socket could not
// be bound.
func Main(ctx context.Context, args []string, stdout io.Writer) int {
	fs := flag.NewFlagSet("masso-sim", flag.ContinueOnError)
	fs.SetOutput(stdout)
	addr := fs.String("addr", "127.0.0.1:65535", "UDP address to listen on")
	serialFlag := fs.Uint("serial", 1, "controller serial number (0-65535)")
	version := fs.String("version", "5-Axis v5.13", "firmware version string")
	tools := fs.String("tools", "", "comma-separated tool names, index 1..n")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *serialFlag > math.MaxUint16 {
		printf(stdout, "masso-sim: -serial %d is out of range (0-65535)\n", *serialFlag)
		return 2
	}
	// The range check above makes this conversion safe; the mask just
	// proves that to the static analyzer, matching internal/masso's
	// clockByte convention.
	serial := uint16(*serialFlag & math.MaxUint16)

	var toolNames []string
	if *tools != "" {
		toolNames = strings.Split(*tools, ",")
	}

	logger := slog.New(slog.NewTextHandler(stdout, nil))
	ctrl, err := New(Options{
		Addr:    *addr,
		Serial:  serial,
		Version: *version,
		Tools:   toolNames,
		Logger:  logger,
	})
	if err != nil {
		printf(stdout, "masso-sim: %v\n", err)
		return 1
	}
	defer func() {
		closeErr := ctrl.Close()
		logger.Debug("sim: stopped", "error", closeErr)
	}()

	printf(stdout, "listening on %s\n", ctrl.Addr())

	<-ctx.Done()
	return 0
}

// printf writes a formatted line to w, best effort: if the destination
// itself is broken (stdout closed, a full pipe) there is nothing more Main
// could usefully do about it.
func printf(w io.Writer, format string, args ...any) {
	_, _ = fmt.Fprintf(w, format, args...) //nolint:errcheck // best-effort output; see doc comment
}
