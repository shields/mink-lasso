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

//go:build windows

package walkui

import (
	"strings"

	"msrl.dev/mink-lasso/internal/ui/model"
)

// textSetter is the common shape of every walk widget's SetText, so paint
// can repaint labels, edits, and text areas through one helper instead of
// checking the same error six times.
type textSetter interface {
	SetText(text string) error
}

func (b *binding) setText(w textSetter, s string) {
	if err := w.SetText(s); err != nil {
		b.logger.Warn("set text", "error", err)
	}
}

// paint repaints exactly the parts ch marks as changed. It must run on the
// UI goroutine.
func (b *binding) paint(ch model.Changes) {
	if ch.StatusLine {
		b.setText(b.statusLabel, b.model.StatusLine())
	}

	if ch.Machine {
		b.paintMachine()
	}

	if ch.Watch {
		b.paintWatch()
	}

	if ch.Transfers {
		b.transfers.refresh(b.model.Transfers())
		b.updateRetryEnabled()
	}

	if ch.Tools {
		b.tools.refresh(b.model.Tools())
	}

	if ch.Log {
		// Rebuilding the whole pane is O(ring size) per line, but log lines
		// are rare (connects, sends, warnings) and the ring is capped at a
		// few thousand; appending only the new lines would have to detect
		// the ring's eviction, which nothing here can test.
		b.setText(b.logEdit, strings.Join(b.model.LogLines(), "\r\n"))

		// SetText alone leaves the caret at the top (WM_SETTEXT resets the
		// Win32 edit control's selection), so ScrollToCaret would otherwise
		// scroll to the oldest line instead of following the newest one.
		n := b.logEdit.TextLength()
		b.logEdit.SetTextSelection(n, n)
		b.logEdit.ScrollToCaret()
	}

	if ch.Tray {
		b.paintTrayTooltip()
	}

	if ch.Balloon != nil {
		b.paintBalloon(ch.Balloon)
	}
}

func (b *binding) paintMachine() {
	mp := b.model.Machine()

	b.setText(b.stateLabel, mp.StateText)
	b.setText(b.fileLabel, mp.File)
	b.setText(b.lineLabel, mp.LineText)
	b.setText(b.jobsLabel, mp.JobsText)
	b.setText(b.gateLabel, mp.GateText)
	b.progress.SetValue(mp.Progress)
}

func (b *binding) paintWatch() {
	w := b.model.Watch()

	b.setText(b.watchPathEdit, w.Dir)
	b.setText(b.modeLabel, w.ModeText)
	b.setText(b.pendingLabel, w.PendingText)

	// Setting Checked programmatically still fires CheckedChanged; guard it
	// so paint doesn't loop back into onUploadWhileMachiningChanged.
	b.painting = true
	b.uploadCheck.SetChecked(w.UploadWhileMachining)
	b.painting = false
}
