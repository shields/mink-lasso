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

package model

import (
	"cmp"
	"fmt"
	"math"
	"slices"
	"strconv"

	"msrl.dev/mink-lasso/internal/engine"
	"msrl.dev/mink-lasso/internal/masso"
)

// sizeUnits are the decimal (1000-based) units above bytes, per the style
// guide: "kB"/"MB"/etc., never "KB".
var sizeUnits = []string{"kB", "MB", "GB", "TB"}

// formatSize renders n bytes as "999 B", "1.0 kB", or "1.5 MB", using
// decimal (1000-based) units. It rounds to one decimal place before
// deciding whether the value belongs in the next unit up, so a value like
// 999_999_999 reads as "1.0 GB" rather than "1000.0 MB": comparing the
// unrounded quotient against 1000 would let a value that only rounds up
// to 1000.0 in the current unit stay undisplayed as e.g. "1000.0 kB".
func formatSize(n int64) string {
	if n < 1000 {
		return fmt.Sprintf("%d B", n)
	}
	f := float64(n)
	unit := ""
	for _, u := range sizeUnits {
		f /= 1000
		unit = u
		rounded := math.Round(f*10) / 10
		if rounded < 1000 {
			f = rounded
			break
		}
	}
	return fmt.Sprintf("%.1f %s", f, unit)
}

// plural returns "1 <singular>" or "N <plural>" for n, the common shape
// behind JobsText and PendingText.
func plural(n int, singular, plural string) string {
	if n == 1 {
		return "1 " + singular
	}
	return strconv.Itoa(n) + " " + plural
}

// StatusLine reports the connection state machine's current state.
func (m *Model) StatusLine() string {
	m.mu.Lock()
	defer m.mu.Unlock()

	serial := masso.SerialString(m.connSerial)
	switch m.connKind {
	case engine.Unconfigured:
		return "Controller serial not configured"
	case engine.Discovering:
		return fmt.Sprintf("Discovering %s…", serial)
	case engine.Connecting:
		return fmt.Sprintf("Connecting to %s at %s…", serial, m.addrHost())
	case engine.Connected:
		return fmt.Sprintf("Connected to %s at %s — %s", serial, m.addrHost(), m.connIdentity.Version)
	case engine.Lost:
		return fmt.Sprintf("Lost connection to %s, retrying…", serial)
	default:
		return ""
	}
}

// addrHost returns connAddr's host, or "" if unset. Callers must hold m.mu.
func (m *Model) addrHost() string {
	if m.connAddr == nil {
		return ""
	}
	return m.connAddr.IP.String()
}

// Machine reports the ready-to-paint state of the machine status panel.
func (m *Model) Machine() MachinePanel {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.connKind != engine.Connected {
		// The engine's own Gate is only ever attached to a StatusEvent,
		// which is emitted solely from inside the connected polling loop
		// (internal/engine's runConnected), so it never reaches the model
		// while disconnected; m.gate is also zeroed on every non-Connected
		// ConnState (see applyConnState). Synthesize the same reason the
		// engine's gate.go uses for this state ("Not connected") instead
		// of leaving GateText blank.
		return MachinePanel{StateText: "—", GateText: "Not connected"}
	}

	// "—" is documented as meaning specifically "not connected"; between
	// Connected arriving and the first StatusEvent (status is polled,
	// not delivered atomically with the connection), fall through with
	// the zero-value Status, which reads as "Machine stopped".
	st := m.status
	stateText := "Machine stopped"
	switch {
	case st.WaitingForOperator:
		stateText = "Waiting for operator"
	case st.Running:
		stateText = "Machining"
	default:
		// stateText already holds the "stopped" default set above.
	}

	return MachinePanel{
		StateText: stateText,
		File:      st.File,
		LineText:  strconv.Itoa(int(st.Line)),
		Progress:  clampPercent(st.Progress),
		JobsText:  plural(int(st.Jobs), "job", "jobs"),
		GateText:  m.gateText(),
	}
}

// clampPercent clamps a raw wire progress byte to [0, 100]: the protocol
// documents that range but does not enforce it, unlike a transfer's own
// Progress (see internal/engine/dispatcher.go's percent), which is already
// clamped before it ever reaches this package.
func clampPercent(p uint8) int {
	if p > 100 {
		return 100
	}
	return int(p)
}

// gateText reports the machine gate's reason, or "Ready to send" when it
// is open. Between a Connected ConnState and the first StatusEvent (the
// engine polls status rather than delivering it atomically with the
// connection), no real Gate has arrived yet, so synthesize the same kind
// of fallback reason StatusLine and Machine's StateText use for that gap,
// rather than showing the zero-value Gate{}'s blank Reason. Callers must
// hold m.mu.
func (m *Model) gateText() string {
	if !m.hasStatus {
		return "Connecting…"
	}
	if m.gate.Open {
		return "Ready to send"
	}
	return m.gate.Reason
}

// Watch reports the ready-to-paint state of the watch folder panel.
func (m *Model) Watch() WatchPanel {
	m.mu.Lock()
	defer m.mu.Unlock()

	return WatchPanel{
		Dir:                  m.watchDir,
		ModeText:             m.modeText(),
		PendingText:          pendingText(m.pendingCountLocked()),
		UploadWhileMachining: m.cfg.UploadWhileMachining,
	}
}

// pendingText formats the watch panel's pending-files count.
func pendingText(n int) string {
	if n == 0 {
		return "Nothing waiting"
	}
	return plural(n, "file waiting", "files waiting")
}

// modeText reports the watch folder's mode string. Callers must hold m.mu.
func (m *Model) modeText() string {
	switch {
	case m.watchDir == "":
		return "No folder configured"
	case m.watchErr != nil:
		return fmt.Sprintf("Not watching: %s", m.watchErr)
	case m.watchTimerOnly:
		return "Watching (timer only)"
	default:
		return "Watching"
	}
}

// pendingCountLocked counts automatic (non-manual) transfer rows that are
// still waiting to be sent. Callers must hold m.mu.
func (m *Model) pendingCountLocked() int {
	n := 0
	for _, entry := range m.transfers {
		if entry.row.Manual {
			continue
		}
		if entry.row.State == engine.Pending || entry.row.State == engine.Waiting {
			n++
		}
	}
	return n
}

// Transfers returns every transfer row, newest first.
func (m *Model) Transfers() []TransferRow {
	m.mu.Lock()
	defer m.mu.Unlock()

	rows := make([]TransferRow, 0, len(m.transfers))
	times := make(map[string]int64, len(m.transfers))
	for k, entry := range m.transfers {
		rows = append(rows, entry.row)
		times[k] = entry.at.UnixNano()
	}
	slices.SortFunc(rows, func(a, b TransferRow) int {
		if c := cmp.Compare(times[b.Name], times[a.Name]); c != 0 {
			return c
		}
		return cmp.Compare(a.Name, b.Name)
	})
	return rows
}

// Tools returns one row per fetched tool with a non-empty name.
func (m *Model) Tools() []ToolRow {
	m.mu.Lock()
	defer m.mu.Unlock()

	rows := make([]ToolRow, 0, len(m.tools))
	for _, t := range m.tools {
		if t.Name == "" {
			continue
		}
		rows = append(rows, ToolRow{IndexText: strconv.Itoa(int(t.Index)), Name: t.Name})
	}
	return rows
}

// LogLines returns every line currently in the log ring, oldest first.
func (m *Model) LogLines() []string {
	m.mu.Lock()
	defer m.mu.Unlock()

	lines := make([]string, len(m.logLines))
	copy(lines, m.logLines)
	return lines
}

// TrayTooltip reports the tray icon's tooltip text.
func (m *Model) TrayTooltip() string {
	m.mu.Lock()
	defer m.mu.Unlock()

	return "mink-lasso — " + m.trayStatus() + " — " + pendingText(m.pendingCountLocked())
}

// trayStatus is a short connection-state phrase for the tray tooltip.
// Callers must hold m.mu.
func (m *Model) trayStatus() string {
	switch m.connKind {
	case engine.Unconfigured:
		return "Not configured"
	case engine.Discovering:
		return "Discovering…"
	case engine.Connecting:
		return "Connecting…"
	case engine.Connected:
		return "Connected to " + masso.SerialString(m.connSerial)
	case engine.Lost:
		return "Lost connection"
	default:
		return ""
	}
}

// Title is the application's window/title-bar name.
func (*Model) Title() string { return "mink-lasso" }

// AboutText is the Help > About dialog's body text.
func (m *Model) AboutText() string {
	m.mu.Lock()
	defer m.mu.Unlock()

	return fmt.Sprintf(
		"mink-lasso %s\n"+
			"Licensed under the Apache License, Version 2.0.\n\n"+
			"An auto-sending replacement for Masso Link: it watches a folder and "+
			"uploads each G-code file to a Masso controller as soon as it settles.\n\n"+
			"File names are limited to %d characters by the Masso controller.",
		m.version, masso.MaxFileName,
	)
}

// SerialText returns the configured serial as "G3-nnnnn", or "" if unset.
func (m *Model) SerialText() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cfg.Serial
}
