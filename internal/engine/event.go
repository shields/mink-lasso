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

package engine

import (
	"net"
	"strconv"
	"time"

	"msrl.dev/mink-lasso/internal/masso"
)

// Event is implemented by every value delivered on Engine.Events. The
// marker method is unexported so the set of implementers is closed to this
// package.
type Event interface {
	isEvent()
}

// ConnKind identifies the connection state machine's current state.
type ConnKind int

// Connection states, in the order the loop moves through them:
// Unconfigured (no serial set) idles until SetSerial; Discovering →
// Connecting → Connected is the normal path; Lost drops back to
// Discovering.
const (
	Unconfigured ConnKind = iota
	Discovering
	Connecting
	Connected
	Lost
)

// String implements fmt.Stringer.
func (k ConnKind) String() string {
	switch k {
	case Unconfigured:
		return "Unconfigured"
	case Discovering:
		return "Discovering"
	case Connecting:
		return "Connecting"
	case Connected:
		return "Connected"
	case Lost:
		return "Lost"
	default:
		return "ConnKind(" + strconv.Itoa(int(k)) + ")"
	}
}

// ConnState reports a change in the connection state machine. Addr and
// Identity are set once a connection attempt has an address to report;
// Err is set only for Lost.
type ConnState struct {
	Kind     ConnKind
	Serial   uint32
	Addr     *net.UDPAddr
	Identity masso.Identity
	Err      error
}

func (ConnState) isEvent() {}

// Gate explains why an upload is, or is not, currently allowed to start —
// a short, operator-facing phrase such as "Machining" or "Not connected".
type Gate struct {
	Open   bool
	Reason string
}

// StatusEvent reports the latest status from the connected controller,
// alongside the machine gate's current verdict.
type StatusEvent struct {
	Status masso.Status
	Gate   Gate
}

func (StatusEvent) isEvent() {}

// ToolsEvent reports the tool table fetched from the controller, or an
// error if the fetch failed.
type ToolsEvent struct {
	Tools []masso.ToolRecord
	Err   error
}

func (ToolsEvent) isEvent() {}

// TransferState is a file's position in the upload scheduler's state
// machine.
type TransferState int

// Transfer states. Pending and Waiting precede a send attempt; Sending is
// in progress; Sent, Failed, Rejected, and SentUnfiled are terminal for
// that attempt (Failed and SentUnfiled can still be retried).
const (
	Pending TransferState = iota
	Waiting
	Sending
	Sent
	Failed
	Rejected
	SentUnfiled
)

// String implements fmt.Stringer.
func (s TransferState) String() string {
	switch s {
	case Pending:
		return "Pending"
	case Waiting:
		return "Waiting"
	case Sending:
		return msgSending
	case Sent:
		return "Sent"
	case Failed:
		return "Failed"
	case Rejected:
		return "Rejected"
	case SentUnfiled:
		return "SentUnfiled"
	default:
		return "TransferState(" + strconv.Itoa(int(s)) + ")"
	}
}

// Retryable reports whether Retry(name) accepts a file currently in state
// s. The single home for this classification keeps the scheduler's own
// retry acceptance and a UI's "is the Retry control enabled" check from
// silently drifting apart.
func (s TransferState) Retryable() bool {
	switch s {
	case Failed, Rejected, SentUnfiled:
		return true
	default:
		return false
	}
}

// Terminal reports whether s is a terminal state for that send attempt
// (Failed and SentUnfiled can still be retried; see Retryable).
func (s TransferState) Terminal() bool {
	switch s {
	case Sent, Failed, Rejected, SentUnfiled:
		return true
	default:
		return false
	}
}

// TransferEvent reports a change in one file's transfer state. Manual is
// true only for a file queued through SendFile.
type TransferEvent struct {
	// Name identifies the file, and is what Retry takes: its path relative
	// to the watch folder, with OS-native separators, or just its base
	// name for a manual send.
	Name    string
	Path    string
	Size    int64
	Sent    int64
	State   TransferState
	Message string
	At      time.Time
	Manual  bool
}

func (TransferEvent) isEvent() {}

// WatchState reports the watch folder's lifecycle: emitted when it starts
// (Err set if it failed to start — the engine keeps running so the
// operator can fix the folder from the GUI), and with Dir "" when no watch
// folder is configured.
type WatchState struct {
	Dir       string
	TimerOnly bool
	Err       error
}

func (WatchState) isEvent() {}
