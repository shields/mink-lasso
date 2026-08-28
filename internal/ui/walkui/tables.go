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
	"github.com/tailscale/walk"

	"msrl.dev/mink-lasso/internal/ui/model"
)

// transfersModel adapts model.Model.Transfers to walk.TableView. It holds no
// state of its own beyond the last snapshot handed to it by paint, so a
// PublishRowsReset after refresh() is always consistent with Value.
type transfersModel struct {
	walk.TableModelBase

	rows []model.TransferRow
}

// refresh replaces the model's rows and tells the bound TableView to reread
// them. It must run on the UI goroutine.
func (m *transfersModel) refresh(rows []model.TransferRow) {
	m.rows = rows
	m.PublishRowsReset()
}

func (m *transfersModel) RowCount() int {
	return len(m.rows)
}

// ID implements walk.IDProvider so a RowsReset (fired on every progress
// tick of an in-progress upload; see internal/engine Options.ProgressInterval)
// can restore the current row by name instead of clearing the selection.
// model.applyTransferEvent guarantees at most one row per Name.
func (m *transfersModel) ID(index int) any {
	if index < 0 || index >= len(m.rows) {
		return nil
	}
	return m.rows[index].Name
}

func (m *transfersModel) Value(row, col int) any {
	if row < 0 || row >= len(m.rows) {
		return ""
	}

	r := m.rows[row]

	switch col {
	case transferColName:
		return r.Name
	case transferColSize:
		return r.SizeText
	case transferColState:
		return r.StateText
	case transferColMessage:
		return r.Message
	case transferColTime:
		return r.TimeText
	default:
		return ""
	}
}

// Column indexes for transfersModel, matching the declarative column order
// built in run.go.
const (
	transferColName = iota
	transferColSize
	transferColState
	transferColMessage
	transferColTime
)

// toolsModel adapts model.Model.Tools to walk.TableView.
type toolsModel struct {
	walk.TableModelBase

	rows []model.ToolRow
}

func (m *toolsModel) refresh(rows []model.ToolRow) {
	m.rows = rows
	m.PublishRowsReset()
}

func (m *toolsModel) RowCount() int {
	return len(m.rows)
}

// ID implements walk.IDProvider, keyed by tool index (unique per row),
// so a RowsReset restores the current row instead of clearing the
// selection.
func (m *toolsModel) ID(index int) any {
	if index < 0 || index >= len(m.rows) {
		return nil
	}
	return m.rows[index].IndexText
}

func (m *toolsModel) Value(row, col int) any {
	if row < 0 || row >= len(m.rows) {
		return ""
	}

	r := m.rows[row]

	switch col {
	case toolColIndex:
		return r.IndexText
	case toolColName:
		return r.Name
	default:
		return ""
	}
}

const (
	toolColIndex = iota
	toolColName
)
