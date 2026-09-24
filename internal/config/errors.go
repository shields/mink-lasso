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

package config

import "errors"

// Sentinel errors returned by the JSON codec and Validate. Callers can
// match them with errors.Is; the messages carry the offending value.
var (
	// ErrInvalidDuration reports a duration string time.ParseDuration could
	// not parse.
	ErrInvalidDuration = errors.New("config: invalid duration")

	// ErrNegativeDuration reports a duration string that parsed to a
	// negative value.
	ErrNegativeDuration = errors.New("config: duration must not be negative")

	// ErrNonPositiveDuration reports a ScanInterval, SettleDelay, or
	// PauseGrace that is zero or negative.
	ErrNonPositiveDuration = errors.New("config: duration must be positive")

	// ErrListenPort reports a ListenPort outside 11000-11050.
	ErrListenPort = errors.New("config: listenPort must be between 11000 and 11050")

	// ErrLogLevel reports a LogLevel other than debug, info, warn, or error.
	ErrLogLevel = errors.New("config: logLevel must be one of debug, info, warn, error")

	// ErrExtension reports an extension that does not start with a dot.
	ErrExtension = errors.New(`config: extension must start with "."`)

	// ErrSerial reports a serial that is not a decimal number
	// 0-4294967295, optionally prefixed with "G3-".
	ErrSerial = errors.New("config: invalid serial")

	// ErrDefaultPath reports that neither os.UserConfigDir nor
	// os.Executable could locate a default config path.
	ErrDefaultPath = errors.New("config: cannot determine default config path")
)
