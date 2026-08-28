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
	"fmt"
	"time"

	"msrl.dev/mink-lasso/internal/engine"
	"msrl.dev/mink-lasso/internal/masso"
)

// Apply folds ev into the model's state and reports what changed, so the
// binding knows what to repaint.
func (m *Model) Apply(ev engine.Event) Changes {
	m.mu.Lock()
	defer m.mu.Unlock()

	switch e := ev.(type) {
	case engine.ConnState:
		return m.applyConnState(e)
	case engine.StatusEvent:
		return m.applyStatusEvent(e)
	case engine.ToolsEvent:
		m.tools = nil
		changes := Changes{Tools: true}
		if e.Err != nil {
			// Every other error-carrying event (WatchState, TransferEvent)
			// pops a balloon; a failed refresh otherwise just goes blank
			// with no visible explanation of why.
			changes.Balloon = &Balloon{
				Kind:  BalloonError,
				Title: "Tool table error",
				Text:  e.Err.Error(),
			}
		} else {
			m.tools = e.Tools
		}
		return changes
	case engine.TransferEvent:
		return m.applyTransferEvent(e)
	case engine.WatchState:
		return m.applyWatchState(e)
	default:
		// Event is a closed interface (internal/engine's isEvent is
		// unexported), so every implementer is one of the cases above;
		// this default only guards against a future engine change that
		// forgets to update this switch.
		return Changes{}
	}
}

// applyConnState handles a ConnState event.
func (m *Model) applyConnState(e engine.ConnState) Changes {
	m.connKind = e.Kind
	m.connSerial = e.Serial
	m.connAddr = e.Addr
	m.connIdentity = e.Identity
	m.connErr = e.Err
	if e.Kind != engine.Connected {
		// A stale status/gate from the previous connection would
		// otherwise keep painting the machine panel as if still
		// connected. On a fresh Connected, Machine() shows this
		// zero-value Status ("Machine stopped") until the first
		// StatusEvent replaces it.
		m.status = masso.Status{}
		m.gate = engine.Gate{}
	}
	// hasStatus tracks whether a StatusEvent has landed for the current
	// connection; Machine() uses it to distinguish "no status yet" from
	// a genuine Gate{} (which the engine never emits: gate.go's
	// evaluateLocked always sets a non-empty Reason when Open is false).
	m.hasStatus = false

	changes := Changes{StatusLine: true, Machine: true, Tray: true}
	if e.Kind == engine.Lost {
		changes.Balloon = &Balloon{
			Kind:  BalloonError,
			Title: "Connection lost",
			Text:  fmt.Sprintf("Lost connection to %s, retrying…", masso.SerialString(e.Serial)),
		}
	}
	return changes
}

// applyStatusEvent handles a StatusEvent.
func (m *Model) applyStatusEvent(e engine.StatusEvent) Changes {
	m.status = e.Status
	m.gate = e.Gate
	m.hasStatus = true
	return Changes{Machine: true}
}

// applyWatchState handles a WatchState event.
func (m *Model) applyWatchState(e engine.WatchState) Changes {
	m.watchDir = e.Dir
	m.watchTimerOnly = e.TimerOnly
	m.watchErr = e.Err

	changes := Changes{Watch: true}
	if e.Err != nil {
		changes.Balloon = &Balloon{
			Kind:  BalloonError,
			Title: "Watch folder error",
			Text:  e.Err.Error(),
		}
	}
	return changes
}

// applyTransferEvent handles a TransferEvent: upserts the row keyed by
// Name, evicting the oldest terminal row if that pushed the table over its
// cap, and picks a balloon for a state that warrants one. There is only
// ever one row per name, mirroring the engine's scheduler, whether the
// event came from the watch folder or a manual SendFile of that name.
func (m *Model) applyTransferEvent(e engine.TransferEvent) Changes {
	row := TransferRow{
		Name:      e.Name,
		SizeText:  formatSize(e.Size),
		StateText: e.State.String(),
		Message:   e.Message,
		TimeText:  e.At.Format("15:04:05"),
		State:     e.State,
		Manual:    e.Manual,
		Path:      e.Path,
	}

	_, existed := m.transfers[e.Name]
	m.transfers[e.Name] = &transferEntry{row: row, at: e.At}
	if !existed {
		m.evictOldestTerminal()
	}

	// A transfer entering or leaving the pending count changes both the
	// Watch panel's PendingText and the tray tooltip, which reads the same
	// count (see pendingCountLocked) — Tray must move whenever Watch does
	// here, or the tooltip goes stale until an unrelated ConnState event.
	changes := Changes{Transfers: true, Watch: true, Tray: true}
	switch e.State {
	case engine.Sent:
		changes.Balloon = &Balloon{Kind: BalloonInfo, Title: "File sent", Text: e.Name + " sent"}
	case engine.Failed, engine.Rejected, engine.SentUnfiled:
		changes.Balloon = &Balloon{Kind: BalloonError, Title: "Transfer problem", Text: e.Name + ": " + e.Message}
	default:
		// Pending, Waiting, and Sending are routine progress, not
		// something the operator needs interrupted for.
	}
	return changes
}

// evictOldestTerminal removes the oldest terminal-state row when the table
// exceeds maxTransfers, never an active one. Callers must hold m.mu.
func (m *Model) evictOldestTerminal() {
	for len(m.transfers) > m.maxTransfers {
		var oldestKey string
		var oldestAt time.Time
		found := false
		for k, entry := range m.transfers {
			if !entry.row.State.Terminal() {
				continue
			}
			// Break exact-timestamp ties deterministically (by name)
			// rather than leaving the outcome to Go's randomized map
			// iteration order.
			if !found || entry.at.Before(oldestAt) ||
				(entry.at.Equal(oldestAt) && k < oldestKey) {
				oldestKey = k
				oldestAt = entry.at
				found = true
			}
		}
		if !found {
			return
		}
		delete(m.transfers, oldestKey)
	}
}
