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

import "msrl.dev/mink-lasso/internal/engine"

// MachinePanel is the ready-to-paint state of the machine status panel.
type MachinePanel struct {
	StateText string
	File      string
	LineText  string
	Progress  int
	JobsText  string
	GateText  string
}

// WatchPanel is the ready-to-paint state of the watch folder panel.
type WatchPanel struct {
	Dir                  string
	ModeText             string
	PendingText          string
	UploadWhileMachining bool
}

// TransferRow is one row of the transfers table.
type TransferRow struct {
	Name      string
	SizeText  string
	StateText string
	Message   string
	TimeText  string
	State     engine.TransferState
	Manual    bool
	Path      string
}

// ToolRow is one row of the tools table.
type ToolRow struct {
	IndexText string
	Name      string
}
