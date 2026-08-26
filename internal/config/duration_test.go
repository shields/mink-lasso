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
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func TestDuration_MarshalJSON(t *testing.T) {
	t.Parallel()

	data, err := json.Marshal(Duration(3 * time.Second))
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if got, want := string(data), `"3s"`; got != want {
		t.Errorf("Marshal(3s) = %s, want %s", got, want)
	}
}

func TestDuration_UnmarshalJSON(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		json    string
		want    time.Duration
		wantErr error
	}{
		{name: "seconds", json: `"3s"`, want: 3 * time.Second},
		{name: "zero", json: `"0s"`, want: 0},
		{name: "compound", json: `"1h2m3s"`, want: time.Hour + 2*time.Minute + 3*time.Second},
		{name: "not a JSON string", json: `123`, wantErr: ErrInvalidDuration},
		{name: "unparsable", json: `"soon"`, wantErr: ErrInvalidDuration},
		{name: "negative", json: `"-1s"`, wantErr: ErrNegativeDuration},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var d Duration
			err := json.Unmarshal([]byte(tt.json), &d)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("Unmarshal(%s) error = %v, want %v", tt.json, err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Unmarshal(%s): %v", tt.json, err)
			}
			if time.Duration(d) != tt.want {
				t.Errorf("Unmarshal(%s) = %v, want %v", tt.json, time.Duration(d), tt.want)
			}
		})
	}
}

func TestDuration_RoundTrip(t *testing.T) {
	t.Parallel()

	type wrapper struct {
		D Duration `json:"d"`
	}

	original := wrapper{D: Duration(90 * time.Second)}
	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var decoded wrapper
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if decoded != original {
		t.Errorf("round trip = %+v, want %+v", decoded, original)
	}
}
