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

import "testing"

func TestConnKindString(t *testing.T) {
	t.Parallel()
	cases := []struct {
		k    ConnKind
		want string
	}{
		{Unconfigured, "Unconfigured"},
		{Discovering, "Discovering"},
		{Connecting, "Connecting"},
		{Connected, "Connected"},
		{Lost, "Lost"},
		{ConnKind(99), "ConnKind(99)"},
	}
	for _, c := range cases {
		if got := c.k.String(); got != c.want {
			t.Errorf("ConnKind(%d).String() = %q, want %q", int(c.k), got, c.want)
		}
	}
}

func TestTransferStateString(t *testing.T) {
	t.Parallel()
	cases := []struct {
		s    TransferState
		want string
	}{
		{Pending, "Pending"},
		{Waiting, "Waiting"},
		{Sending, "Sending"},
		{Sent, "Sent"},
		{Failed, "Failed"},
		{Rejected, "Rejected"},
		{SentUnfiled, "SentUnfiled"},
		{TransferState(-1), "TransferState(-1)"},
	}
	for _, c := range cases {
		if got := c.s.String(); got != c.want {
			t.Errorf("TransferState(%d).String() = %q, want %q", int(c.s), got, c.want)
		}
	}
}

func TestTransferStateRetryable(t *testing.T) {
	t.Parallel()
	cases := []struct {
		s    TransferState
		want bool
	}{
		{Pending, false},
		{Waiting, false},
		{Sending, false},
		{Sent, false},
		{Failed, true},
		{Rejected, true},
		{SentUnfiled, true},
	}
	for _, c := range cases {
		if got := c.s.Retryable(); got != c.want {
			t.Errorf("%v.Retryable() = %v, want %v", c.s, got, c.want)
		}
	}
}

func TestTransferStateTerminal(t *testing.T) {
	t.Parallel()
	cases := []struct {
		s    TransferState
		want bool
	}{
		{Pending, false},
		{Waiting, false},
		{Sending, false},
		{Sent, true},
		{Failed, true},
		{Rejected, true},
		{SentUnfiled, true},
	}
	for _, c := range cases {
		if got := c.s.Terminal(); got != c.want {
			t.Errorf("%v.Terminal() = %v, want %v", c.s, got, c.want)
		}
	}
}

// TestEventMarker exercises every implementer's unexported isEvent method,
// which otherwise has no other caller in this package's non-test code —
// its only purpose is to seal the Event interface.
func TestEventMarker(t *testing.T) {
	t.Parallel()
	events := []Event{
		ConnState{},
		StatusEvent{},
		ToolsEvent{},
		TransferEvent{},
		WatchState{},
	}
	for _, ev := range events {
		if ev == nil {
			t.Fatal("event unexpectedly nil")
		}
		ev.isEvent()
	}
}
