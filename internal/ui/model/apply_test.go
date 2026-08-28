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
	"errors"
	"net"
	"testing"
	"time"

	"msrl.dev/mink-lasso/internal/engine"
	"msrl.dev/mink-lasso/internal/masso"
)

func TestApplyConnState(t *testing.T) {
	t.Parallel()

	addr := &net.UDPAddr{IP: net.IPv4(192, 168, 1, 50), Port: 11000}
	cases := []struct {
		name        string
		ev          engine.ConnState
		wantStatus  string
		wantBalloon bool
	}{
		{
			name:       "unconfigured",
			ev:         engine.ConnState{Kind: engine.Unconfigured},
			wantStatus: "Controller serial not configured",
		},
		{
			name:       "discovering",
			ev:         engine.ConnState{Kind: engine.Discovering, Serial: 12345},
			wantStatus: "Discovering G3-12345…",
		},
		{
			name:       "connecting",
			ev:         engine.ConnState{Kind: engine.Connecting, Serial: 12345, Addr: addr},
			wantStatus: "Connecting to G3-12345 at 192.168.1.50…",
		},
		{
			name: "connected",
			ev: engine.ConnState{
				Kind: engine.Connected, Serial: 12345, Addr: addr,
				Identity: masso.Identity{Serial: 12345, Version: "5-Axis v5.13"},
			},
			wantStatus: "Connected to G3-12345 at 192.168.1.50 — 5-Axis v5.13",
		},
		{
			name:        "lost",
			ev:          engine.ConnState{Kind: engine.Lost, Serial: 12345, Addr: addr, Err: errors.New("boom")},
			wantStatus:  "Lost connection to G3-12345, retrying…",
			wantBalloon: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m := New(Options{})
			changes := m.Apply(tc.ev)

			if !changes.StatusLine || !changes.Machine || !changes.Tray {
				t.Errorf("changes = %+v, want StatusLine/Machine/Tray set", changes)
			}
			if got := m.StatusLine(); got != tc.wantStatus {
				t.Errorf("StatusLine() = %q, want %q", got, tc.wantStatus)
			}
			if (changes.Balloon != nil) != tc.wantBalloon {
				t.Errorf("Balloon = %v, want present = %v", changes.Balloon, tc.wantBalloon)
			}
			if tc.wantBalloon && changes.Balloon.Kind != BalloonError {
				t.Errorf("Balloon.Kind = %v, want BalloonError", changes.Balloon.Kind)
			}
			if tc.wantBalloon {
				if changes.Balloon.Title != "Connection lost" {
					t.Errorf("Balloon.Title = %q, want %q", changes.Balloon.Title, "Connection lost")
				}
				wantText := "Lost connection to G3-12345, retrying…"
				if changes.Balloon.Text != wantText {
					t.Errorf("Balloon.Text = %q, want %q", changes.Balloon.Text, wantText)
				}
			}
		})
	}
}

func TestMachineGateTextBeforeFirstStatus(t *testing.T) {
	t.Parallel()
	m := New(Options{})

	m.Apply(engine.ConnState{Kind: engine.Connected, Serial: 12345})
	// Regression: between Connected and the first StatusEvent, m.gate is
	// still Gate{} (zero value); GateText must not read as that zero
	// value's blank Reason.
	if got := m.Machine().GateText; got != "Connecting…" {
		t.Errorf("GateText before first status = %q, want %q", got, "Connecting…")
	}

	m.Apply(engine.StatusEvent{Status: masso.Status{Running: true}, Gate: engine.Gate{Reason: "Machining"}})
	if got := m.Machine().GateText; got != "Machining" {
		t.Errorf("GateText after first status = %q, want %q", got, "Machining")
	}
}

func TestApplyConnStateClearsStatusOnDisconnect(t *testing.T) {
	t.Parallel()
	m := New(Options{})

	m.Apply(engine.ConnState{Kind: engine.Connected, Serial: 1})
	m.Apply(engine.StatusEvent{Status: masso.Status{Running: true}, Gate: engine.Gate{Reason: "Machining"}})
	if got := m.Machine().StateText; got != "Machining" {
		t.Fatalf("StateText = %q, want Machining", got)
	}

	m.Apply(engine.ConnState{Kind: engine.Lost, Serial: 1})
	panel := m.Machine()
	if panel.StateText != "—" {
		t.Errorf("StateText after Lost = %q, want —", panel.StateText)
	}
	// Regression: GateText must not just go blank once a live "Machining"
	// gate reason is superseded by a disconnect.
	if panel.GateText != "Not connected" {
		t.Errorf("GateText after Lost = %q, want %q", panel.GateText, "Not connected")
	}
}

func TestApplyStatusEvent(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name         string
		status       masso.Status
		gate         engine.Gate
		wantTxt      string
		wantGT       string
		wantFile     string
		wantLineText string
		wantProgress int
		wantJobsText string
	}{
		{
			name: "stopped", status: masso.Status{}, gate: engine.Gate{Reason: "Ready to send"},
			wantTxt: "Machine stopped", wantGT: "Ready to send", wantLineText: "0", wantJobsText: "0 jobs",
		},
		{
			name: "running", status: masso.Status{Running: true}, gate: engine.Gate{Reason: "Machining"},
			wantTxt: "Machining", wantGT: "Machining", wantLineText: "0", wantJobsText: "0 jobs",
		},
		{
			name: "waiting", status: masso.Status{WaitingForOperator: true},
			gate:    engine.Gate{Reason: "Waiting for operator"},
			wantTxt: "Waiting for operator", wantGT: "Waiting for operator",
			wantLineText: "0", wantJobsText: "0 jobs",
		},
		{
			name: "open gate", status: masso.Status{}, gate: engine.Gate{Open: true},
			wantTxt: "Machine stopped", wantGT: "Ready to send", wantLineText: "0", wantJobsText: "0 jobs",
		},
		{
			// Regression: exercise File/LineText/Progress/JobsText with
			// real, non-zero data through the actual Status->MachinePanel
			// wiring, including the singular/plural JobsText boundary,
			// rather than only ever at their zero-value defaults.
			name:    "populated",
			status:  masso.Status{Running: true, File: "PART.NC", Line: 42, Progress: 57, Jobs: 3},
			gate:    engine.Gate{Reason: "Machining"},
			wantTxt: "Machining", wantGT: "Machining", wantFile: "PART.NC",
			wantLineText: "42", wantProgress: 57, wantJobsText: "3 jobs",
		},
		{
			name:    "single job",
			status:  masso.Status{File: "PART.NC", Jobs: 1},
			gate:    engine.Gate{Reason: "Ready to send", Open: true},
			wantTxt: "Machine stopped", wantGT: "Ready to send", wantFile: "PART.NC",
			wantLineText: "0", wantJobsText: "1 job",
		},
		{
			// The wire byte is documented 0-100 but not enforced by the
			// protocol; an out-of-range value must clamp for display
			// rather than show e.g. "230%".
			name:    "out of range progress clamps to 100",
			status:  masso.Status{Running: true, Progress: 230},
			gate:    engine.Gate{Reason: "Machining"},
			wantTxt: "Machining", wantGT: "Machining",
			wantLineText: "0", wantProgress: 100, wantJobsText: "0 jobs",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m := New(Options{})
			m.Apply(engine.ConnState{Kind: engine.Connected, Serial: 1})
			changes := m.Apply(engine.StatusEvent{Status: tc.status, Gate: tc.gate})
			if !changes.Machine {
				t.Error("changes.Machine = false, want true")
			}
			panel := m.Machine()
			if panel.StateText != tc.wantTxt {
				t.Errorf("StateText = %q, want %q", panel.StateText, tc.wantTxt)
			}
			if panel.GateText != tc.wantGT {
				t.Errorf("GateText = %q, want %q", panel.GateText, tc.wantGT)
			}
			if panel.File != tc.wantFile {
				t.Errorf("File = %q, want %q", panel.File, tc.wantFile)
			}
			if panel.LineText != tc.wantLineText {
				t.Errorf("LineText = %q, want %q", panel.LineText, tc.wantLineText)
			}
			if panel.Progress != tc.wantProgress {
				t.Errorf("Progress = %d, want %d", panel.Progress, tc.wantProgress)
			}
			if panel.JobsText != tc.wantJobsText {
				t.Errorf("JobsText = %q, want %q", panel.JobsText, tc.wantJobsText)
			}
		})
	}
}

func TestApplyToolsEvent(t *testing.T) {
	t.Parallel()
	m := New(Options{})

	changes := m.Apply(engine.ToolsEvent{Tools: []masso.ToolRecord{
		{Index: 1, Name: "Endmill"},
		{Index: 2, Name: ""},
		{Index: 3, Name: "Drill"},
	}})
	if !changes.Tools {
		t.Error("changes.Tools = false, want true")
	}
	rows := m.Tools()
	if len(rows) != 2 {
		t.Fatalf("Tools() = %v, want 2 rows (empty name omitted)", rows)
	}
	if rows[0] != (ToolRow{IndexText: "1", Name: "Endmill"}) {
		t.Errorf("rows[0] = %+v", rows[0])
	}
	if rows[1] != (ToolRow{IndexText: "3", Name: "Drill"}) {
		t.Errorf("rows[1] = %+v", rows[1])
	}

	// A failed fetch clears the table rather than leaving stale rows.
	m.Apply(engine.ToolsEvent{Err: errors.New("boom")})
	if rows := m.Tools(); len(rows) != 0 {
		t.Errorf("Tools() after error = %v, want empty", rows)
	}
}

func TestApplyWatchState(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		ev       engine.WatchState
		wantMode string
		wantBall bool
	}{
		{"unconfigured", engine.WatchState{Dir: ""}, "No folder configured", false},
		{"watching", engine.WatchState{Dir: "/w"}, "Watching", false},
		{"timer only", engine.WatchState{Dir: "/w", TimerOnly: true}, "Watching (timer only)", false},
		{
			"error",
			engine.WatchState{Dir: "/w", Err: errors.New("no such file")},
			"Not watching: no such file", true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m := New(Options{})
			changes := m.Apply(tc.ev)
			if !changes.Watch {
				t.Error("changes.Watch = false, want true")
			}
			if got := m.Watch().ModeText; got != tc.wantMode {
				t.Errorf("ModeText = %q, want %q", got, tc.wantMode)
			}
			if (changes.Balloon != nil) != tc.wantBall {
				t.Errorf("Balloon = %v, want present = %v", changes.Balloon, tc.wantBall)
			}
			if tc.wantBall {
				if changes.Balloon.Title != "Watch folder error" {
					t.Errorf("Balloon.Title = %q, want %q", changes.Balloon.Title, "Watch folder error")
				}
				if changes.Balloon.Text != tc.ev.Err.Error() {
					t.Errorf("Balloon.Text = %q, want %q", changes.Balloon.Text, tc.ev.Err.Error())
				}
			}
		})
	}
}

func TestApplyTransferEventBalloons(t *testing.T) {
	t.Parallel()
	cases := []struct {
		state     engine.TransferState
		wantKind  BalloonKind
		wantNone  bool
		wantTitle string
		wantText  string
	}{
		{engine.Sent, BalloonInfo, false, "File sent", "A.NC sent"},
		{engine.Failed, BalloonError, false, "Transfer problem", "A.NC: boom"},
		{engine.Rejected, BalloonError, false, "Transfer problem", "A.NC: boom"},
		{engine.SentUnfiled, BalloonError, false, "Transfer problem", "A.NC: boom"},
		{engine.Pending, 0, true, "", ""},
		{engine.Waiting, 0, true, "", ""},
		{engine.Sending, 0, true, "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.state.String(), func(t *testing.T) {
			t.Parallel()
			m := New(Options{})
			changes := m.Apply(engine.TransferEvent{Name: "A.NC", State: tc.state, Message: "boom", At: time.Now()})
			if !changes.Transfers {
				t.Errorf("changes = %+v, want Transfers set", changes)
			}
			if tc.wantNone {
				if changes.Balloon != nil {
					t.Errorf("Balloon = %v, want nil", changes.Balloon)
				}
				return
			}
			if changes.Balloon == nil || changes.Balloon.Kind != tc.wantKind {
				t.Errorf("Balloon = %v, want kind %v", changes.Balloon, tc.wantKind)
			}
			if changes.Balloon.Title != tc.wantTitle {
				t.Errorf("Balloon.Title = %q, want %q", changes.Balloon.Title, tc.wantTitle)
			}
			if changes.Balloon.Text != tc.wantText {
				t.Errorf("Balloon.Text = %q, want %q", changes.Balloon.Text, tc.wantText)
			}
		})
	}
}

func TestApplyTransferEventUpsertAndOrdering(t *testing.T) {
	t.Parallel()
	m := New(Options{})
	base := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

	m.Apply(engine.TransferEvent{Name: "A.NC", State: engine.Pending, At: base, Size: 500})
	m.Apply(engine.TransferEvent{Name: "B.NC", State: engine.Pending, At: base.Add(time.Second), Size: 1500})
	// Update A.NC in place (same key): row count should stay 2.
	m.Apply(engine.TransferEvent{Name: "A.NC", State: engine.Sending, At: base.Add(2 * time.Second), Size: 500})

	rows := m.Transfers()
	if len(rows) != 2 {
		t.Fatalf("Transfers() = %v, want 2 rows", rows)
	}
	if rows[0].Name != "A.NC" || rows[0].State != engine.Sending {
		t.Errorf("rows[0] = %+v, want updated A.NC first (newest)", rows[0])
	}
	if rows[1].Name != "B.NC" {
		t.Errorf("rows[1] = %+v, want B.NC", rows[1])
	}
	if rows[1].SizeText != "1.5 kB" {
		t.Errorf("SizeText = %q, want 1.5 kB", rows[1].SizeText)
	}
}

// TestApplyTransferEventManualAndAutoShareRow checks that a manual send
// and an automatic watch-folder send of the same name share one row,
// mirroring internal/engine/scheduler.go's item map (keyed strictly by
// name: ready() and sendFile() look up or create the same entry and just
// flip its manual bit). A stale row from an earlier track must not
// survive once a later event on the other track supersedes it.
func TestApplyTransferEventManualAndAutoShareRow(t *testing.T) {
	t.Parallel()
	m := New(Options{})
	now := time.Now()
	m.Apply(engine.TransferEvent{Name: "A.NC", State: engine.Pending, Manual: false, At: now})
	m.Apply(engine.TransferEvent{Name: "A.NC", State: engine.Sending, Manual: true, At: now.Add(time.Second)})

	rows := m.Transfers()
	if len(rows) != 1 {
		t.Fatalf("Transfers() = %v, want 1 row (manual and automatic share the same name)", rows)
	}
	if rows[0].State != engine.Sending || !rows[0].Manual {
		t.Errorf("rows[0] = %+v, want the latest event's state and Manual", rows[0])
	}
}

// TestRetryEnabledNotStaleAcrossManualAndAuto is the regression test for
// the bug where a Failed manual attempt left RetryEnabled/Retry believing
// a later, resolved automatic send was still retryable (or vice versa).
// Since transfers share one row per name, a later event on either track
// always supersedes an earlier one on the other.
func TestRetryEnabledNotStaleAcrossManualAndAuto(t *testing.T) {
	t.Parallel()
	m := New(Options{})
	now := time.Now()

	m.Apply(engine.TransferEvent{Name: "A.NC", State: engine.Failed, Manual: true, At: now})
	if !m.RetryEnabled("A.NC") {
		t.Fatal("RetryEnabled(A.NC) = false after a Failed manual send, want true")
	}

	m.Apply(engine.TransferEvent{Name: "A.NC", State: engine.Pending, Manual: false, At: now.Add(time.Second)})
	m.Apply(engine.TransferEvent{Name: "A.NC", State: engine.Sent, Manual: false, At: now.Add(2 * time.Second)})

	if m.RetryEnabled("A.NC") {
		t.Error("RetryEnabled(A.NC) = true after the file later Sent via the auto track, want false (no stale row)")
	}
}

func TestApplyTransferEventEvictsOnlyTerminal(t *testing.T) {
	t.Parallel()
	m := New(Options{MaxTransfers: 2})
	now := time.Now()

	m.Apply(engine.TransferEvent{Name: "active", State: engine.Sending, At: now})
	m.Apply(engine.TransferEvent{Name: "old-terminal", State: engine.Sent, At: now.Add(time.Second)})
	m.Apply(engine.TransferEvent{Name: "new-terminal", State: engine.Failed, At: now.Add(2 * time.Second)})

	rows := m.Transfers()
	if len(rows) != 2 {
		t.Fatalf("Transfers() = %v, want 2 rows after eviction", rows)
	}
	for _, r := range rows {
		if r.Name == "old-terminal" {
			t.Errorf("old-terminal row survived eviction: %+v", rows)
		}
	}
}

func TestApplyTransferEventNoEvictionWhenNoneTerminal(t *testing.T) {
	t.Parallel()
	m := New(Options{MaxTransfers: 1})
	now := time.Now()

	m.Apply(engine.TransferEvent{Name: "one", State: engine.Sending, At: now})
	m.Apply(engine.TransferEvent{Name: "two", State: engine.Waiting, At: now.Add(time.Second)})

	rows := m.Transfers()
	if len(rows) != 2 {
		t.Errorf("Transfers() = %v, want both active rows kept even over MaxTransfers", rows)
	}
}

// TestApplyTransferEventEvictsDeterministicallyOnTie checks that when two
// terminal rows share the exact same timestamp, evictOldestTerminal always
// picks the same one rather than depending on Go's randomized map
// iteration order; run under -count to catch nondeterminism.
func TestApplyTransferEventEvictsDeterministicallyOnTie(t *testing.T) {
	t.Parallel()
	now := time.Now()

	m := New(Options{MaxTransfers: 1})
	m.Apply(engine.TransferEvent{Name: "b-tied", State: engine.Sent, At: now})
	m.Apply(engine.TransferEvent{Name: "a-tied", State: engine.Failed, At: now})

	rows := m.Transfers()
	if len(rows) != 1 {
		t.Fatalf("Transfers() = %v, want 1 row after eviction", rows)
	}
	// "a-tied" sorts before "b-tied", so the tie-break treats it as the
	// older of the two and evicts it, leaving "b-tied".
	if rows[0].Name != "b-tied" {
		t.Errorf("Transfers() = %v, want b-tied to survive the tie", rows)
	}
}

func TestApplyUnknownEventIsNoOp(t *testing.T) {
	t.Parallel()
	m := New(Options{})
	changes := m.Apply(unknownEvent{})
	if changes != (Changes{}) {
		t.Errorf("Apply(unknown) = %+v, want zero Changes", changes)
	}
}

// unknownEvent implements engine.Event's marker method so Apply's default
// case is reachable; it is otherwise never produced by internal/engine.
type unknownEvent struct{ engine.ConnState }

func TestApplyTransferEventWatchAndTrayFollowPendingCount(t *testing.T) {
	t.Parallel()
	m := New(Options{})
	at := time.Now()
	steps := []struct {
		name  string
		event engine.TransferEvent
		want  bool
	}{
		{"pending joins the count", engine.TransferEvent{Name: "A.NC", State: engine.Pending, At: at}, true},
		{"waiting stays in the count", engine.TransferEvent{Name: "A.NC", State: engine.Waiting, At: at}, false},
		{"sending leaves the count", engine.TransferEvent{Name: "A.NC", State: engine.Sending, At: at}, true},
		{"progress tick", engine.TransferEvent{Name: "A.NC", State: engine.Sending, Sent: 1422, At: at}, false},
		{"sent", engine.TransferEvent{Name: "A.NC", State: engine.Sent, At: at}, false},
		{
			"manual pending is not counted",
			engine.TransferEvent{Name: "B.NC", State: engine.Pending, Manual: true, At: at},
			false,
		},
	}
	for _, s := range steps {
		changes := m.Apply(s.event)
		if !changes.Transfers {
			t.Errorf("%s: Transfers = false, want true", s.name)
		}
		if changes.Watch != s.want || changes.Tray != s.want {
			t.Errorf("%s: Watch = %v, Tray = %v, want both %v", s.name, changes.Watch, changes.Tray, s.want)
		}
	}
}
