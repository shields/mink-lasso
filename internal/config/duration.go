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

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"time"
)

// Duration is a time.Duration that marshals to and from JSON as a Go
// duration string (e.g. "3s") rather than an integer count of nanoseconds,
// so the config file stays human-readable and human-editable.
type Duration time.Duration //nolint:recvcheck // MarshalJSONTo reads a value; UnmarshalJSONFrom must mutate a pointer.

// MarshalJSONTo implements json.MarshalerTo.
func (d Duration) MarshalJSONTo(enc *jsontext.Encoder) error {
	// Marshaling a string cannot fail, so there is no error path to test
	// here: no invalid UTF-8 or unsupported-type case exists for a string.
	return json.MarshalEncode(enc, time.Duration(d).String())
}

// UnmarshalJSONFrom implements json.UnmarshalerFrom. It rejects strings that
// time.ParseDuration cannot parse and durations that parse as negative.
func (d *Duration) UnmarshalJSONFrom(dec *jsontext.Decoder) error {
	var s string
	if err := json.UnmarshalDecode(dec, &s); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidDuration, err)
	}

	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("%w: %q: %w", ErrInvalidDuration, s, err)
	}
	if parsed < 0 {
		return fmt.Errorf("%w: %q", ErrNegativeDuration, s)
	}

	*d = Duration(parsed)
	return nil
}
