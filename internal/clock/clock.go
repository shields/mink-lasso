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

// Package clock abstracts time so that timeouts, retransmits, and backoff can
// be tested without waiting.
package clock

import "time"

// Clock is the subset of package time that the application uses.
type Clock interface {
	Now() time.Time
	Since(t time.Time) time.Duration
	After(d time.Duration) <-chan time.Time
	NewTimer(d time.Duration) Timer
	NewTicker(d time.Duration) Ticker
}

// Timer mirrors *time.Timer.
type Timer interface {
	C() <-chan time.Time
	Stop() bool
	Reset(d time.Duration) bool
}

// Ticker mirrors *time.Ticker.
type Ticker interface {
	C() <-chan time.Time
	Stop()
}

// Real is the wall clock.
type Real struct{}

// Now implements Clock.
func (Real) Now() time.Time { return time.Now() }

// Since implements Clock.
func (Real) Since(t time.Time) time.Duration { return time.Since(t) }

// After implements Clock.
func (Real) After(d time.Duration) <-chan time.Time { return time.After(d) }

// NewTimer implements Clock.
func (Real) NewTimer(d time.Duration) Timer { return realTimer{time.NewTimer(d)} }

// NewTicker implements Clock.
func (Real) NewTicker(d time.Duration) Ticker { return realTicker{time.NewTicker(d)} }

type realTimer struct{ t *time.Timer }

func (t realTimer) C() <-chan time.Time        { return t.t.C }
func (t realTimer) Stop() bool                 { return t.t.Stop() }
func (t realTimer) Reset(d time.Duration) bool { return t.t.Reset(d) }

type realTicker struct{ t *time.Ticker }

func (t realTicker) C() <-chan time.Time { return t.t.C }
func (t realTicker) Stop()               { t.t.Stop() }
