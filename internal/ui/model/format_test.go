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
	"strings"
	"testing"
	"time"

	"msrl.dev/mink-lasso/internal/config"
	"msrl.dev/mink-lasso/internal/engine"
	"msrl.dev/mink-lasso/internal/masso"
)

func TestFormatSize(t *testing.T) {
	t.Parallel()
	cases := []struct {
		n    int64
		want string
	}{
		{0, "0 B"},
		{999, "999 B"},
		{1000, "1.0 kB"},
		{1500, "1.5 kB"},
		{1_500_000, "1.5 MB"},
		{999_000, "999.0 kB"},
		{1_000_000_000, "1.0 GB"},
		// Rounding-boundary cases: a value that rounds to 1000.0 in the
		// current unit must promote to the next unit up, not display as
		// e.g. "1000.0 kB".
		{999_950, "1.0 MB"},
		{999_999, "1.0 MB"},
		{999_999_999, "1.0 GB"},
	}
	for _, tc := range cases {
		if got := formatSize(tc.n); got != tc.want {
			t.Errorf("formatSize(%d) = %q, want %q", tc.n, got, tc.want)
		}
	}
}

func TestPluralAndPendingText(t *testing.T) {
	t.Parallel()
	if got := plural(0, "job", "jobs"); got != "0 jobs" {
		t.Errorf("plural(0) = %q", got)
	}
	if got := plural(1, "job", "jobs"); got != "1 job" {
		t.Errorf("plural(1) = %q", got)
	}
	if got := plural(2, "job", "jobs"); got != "2 jobs" {
		t.Errorf("plural(2) = %q", got)
	}

	if got := pendingText(0); got != "Nothing waiting" {
		t.Errorf("pendingText(0) = %q", got)
	}
	if got := pendingText(1); got != "1 file waiting" {
		t.Errorf("pendingText(1) = %q", got)
	}
	if got := pendingText(3); got != "3 files waiting" {
		t.Errorf("pendingText(3) = %q", got)
	}
}

func TestWatchPendingCount(t *testing.T) {
	t.Parallel()
	m := New(Options{})
	now := time.Now()

	m.Apply(engine.TransferEvent{Name: "A.NC", State: engine.Pending, At: now})
	m.Apply(engine.TransferEvent{Name: "B.NC", State: engine.Waiting, At: now})
	m.Apply(engine.TransferEvent{Name: "C.NC", State: engine.Sent, At: now})
	// Manual sends never count toward the folder's pending count.
	m.Apply(engine.TransferEvent{Name: "D.NC", State: engine.Pending, Manual: true, At: now})

	if got := m.Watch().PendingText; got != "2 files waiting" {
		t.Errorf("PendingText = %q, want 2 files waiting", got)
	}
}

func TestMachineDisconnected(t *testing.T) {
	t.Parallel()
	m := New(Options{})
	panel := m.Machine()
	if panel.StateText != "—" {
		t.Errorf("StateText = %q, want —", panel.StateText)
	}
	// Matches the reason internal/engine/gate.go's disconnected() computes
	// for this state; the model synthesizes it because the engine's Gate
	// never reaches the model while disconnected (see Machine()).
	if panel.GateText != "Not connected" {
		t.Errorf("GateText = %q, want %q", panel.GateText, "Not connected")
	}
}

// TestMachineConnectedBeforeFirstStatus checks that "—" (documented as
// "not connected") is not shown once Connected has arrived, even though
// status is polled separately and may not have arrived yet.
func TestMachineConnectedBeforeFirstStatus(t *testing.T) {
	t.Parallel()
	m := New(Options{})
	m.Apply(engine.ConnState{Kind: engine.Connected, Serial: 12345})
	panel := m.Machine()
	if panel.StateText == "—" {
		t.Errorf("StateText = %q, want not \"—\" once connected", panel.StateText)
	}
	if panel.StateText != "Machine stopped" {
		t.Errorf("StateText = %q, want Machine stopped as the interim default", panel.StateText)
	}
}

// TestMachineReconnectClearsStaleStatus checks that a status from a prior
// connection is not shown after a fresh Connected, before its own first
// StatusEvent arrives.
func TestMachineReconnectClearsStaleStatus(t *testing.T) {
	t.Parallel()
	m := New(Options{})
	m.Apply(engine.ConnState{Kind: engine.Connected, Serial: 12345})
	m.Apply(engine.StatusEvent{Status: masso.Status{Running: true}})
	if got := m.Machine().StateText; got != "Machining" {
		t.Fatalf("StateText = %q, want Machining", got)
	}

	m.Apply(engine.ConnState{Kind: engine.Lost, Serial: 12345})
	m.Apply(engine.ConnState{Kind: engine.Connected, Serial: 12345})
	if got := m.Machine().StateText; got != "Machine stopped" {
		t.Errorf("StateText = %q, want Machine stopped (stale status cleared), not the prior connection's status", got)
	}
}

func TestTrayTooltip(t *testing.T) {
	t.Parallel()
	m := New(Options{})
	m.Apply(engine.ConnState{Kind: engine.Connected, Serial: 12345})
	m.Apply(engine.TransferEvent{Name: "A.NC", State: engine.Pending, At: time.Now()})
	m.Apply(engine.TransferEvent{Name: "B.NC", State: engine.Waiting, At: time.Now()})

	want := "mink-lasso — Connected to G3-12345 — 2 files waiting"
	if got := m.TrayTooltip(); got != want {
		t.Errorf("TrayTooltip() = %q, want %q", got, want)
	}
}

func TestTitleAndAboutText(t *testing.T) {
	t.Parallel()
	m := New(Options{Version: "0.20260824.1"})
	if got := m.Title(); got != "mink-lasso" {
		t.Errorf("Title() = %q", got)
	}
	about := m.AboutText()
	if !strings.Contains(about, "0.20260824.1") {
		t.Errorf("AboutText() = %q, want version included", about)
	}
	if !strings.Contains(about, "15 characters") {
		t.Errorf("AboutText() = %q, want the file name limit mentioned", about)
	}
	if !strings.Contains(about, "Apache License") {
		t.Errorf("AboutText() = %q, want the license mentioned", about)
	}
}

func TestSerialText(t *testing.T) {
	t.Parallel()
	m := New(Options{})
	if got := m.SerialText(); got != "" {
		t.Errorf("SerialText() = %q, want empty", got)
	}
	m = New(Options{Config: config.Config{Serial: "G3-12345"}})
	if got := m.SerialText(); got != "G3-12345" {
		t.Errorf("SerialText() = %q, want G3-12345", got)
	}
	// A persisted config may hold the bare-digit form; New must normalize
	// it to the canonical "G3-nnnnn" so SerialText's contract holds from
	// startup, not only after ApplySerial runs.
	m = New(Options{Config: config.Config{Serial: "12345"}})
	if got := m.SerialText(); got != "G3-12345" {
		t.Errorf("SerialText() = %q, want G3-12345 (normalized from bare digits)", got)
	}
}

func TestStatusLineUnknownConnKind(t *testing.T) {
	t.Parallel()
	m := New(Options{})
	m.connKind = engine.ConnKind(99)
	if got := m.StatusLine(); got != "" {
		t.Errorf("StatusLine() for an unknown ConnKind = %q, want empty", got)
	}
}

func TestStatusLineConnectingWithNoAddr(t *testing.T) {
	t.Parallel()
	m := New(Options{})
	m.Apply(engine.ConnState{Kind: engine.Connecting, Serial: 1})
	want := "Connecting to G3-1 at …"
	if got := m.StatusLine(); got != want {
		t.Errorf("StatusLine() = %q, want %q", got, want)
	}
}

func TestTrayStatusEveryConnKind(t *testing.T) {
	t.Parallel()
	cases := []struct {
		kind engine.ConnKind
		want string
	}{
		{engine.Unconfigured, "Not configured"},
		{engine.Discovering, "Discovering…"},
		{engine.Connecting, "Connecting…"},
		{engine.Lost, "Lost connection"},
	}
	for _, tc := range cases {
		m := New(Options{})
		m.Apply(engine.ConnState{Kind: tc.kind, Serial: 1})
		if got := m.trayStatus(); got != tc.want {
			t.Errorf("trayStatus() for %v = %q, want %q", tc.kind, got, tc.want)
		}
	}
}

func TestTrayStatusUnknownConnKind(t *testing.T) {
	t.Parallel()
	m := New(Options{})
	m.connKind = engine.ConnKind(99)
	if got := m.trayStatus(); got != "" {
		t.Errorf("trayStatus() for an unknown ConnKind = %q, want empty", got)
	}
}

func TestTransfersTiesBrokenByName(t *testing.T) {
	t.Parallel()
	m := New(Options{})
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	m.Apply(engine.TransferEvent{Name: "B.NC", State: engine.Pending, At: at})
	m.Apply(engine.TransferEvent{Name: "A.NC", State: engine.Pending, At: at})

	rows := m.Transfers()
	if len(rows) != 2 || rows[0].Name != "A.NC" || rows[1].Name != "B.NC" {
		t.Errorf("Transfers() = %+v, want A.NC then B.NC (tie broken by name)", rows)
	}
}

func TestLogLinesCopyIsIndependent(t *testing.T) {
	t.Parallel()
	m := New(Options{})
	m.appendLog("line 1")
	lines := m.LogLines()
	lines[0] = "mutated"
	if got := m.LogLines()[0]; got != "line 1" {
		t.Errorf("LogLines() mutated internal state: got %q", got)
	}
}
