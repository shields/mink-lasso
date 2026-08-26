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

package masso

import (
	"errors"
	"strings"
	"testing"
)

func TestValidateFileNameValid(t *testing.T) {
	t.Parallel()

	tests := []string{
		"A",
		"CLTEST.NC",
		"123456789012345", // exactly 15 characters
		"a b~c!",
	}
	for _, name := range tests {
		if err := ValidateFileName(name); err != nil {
			t.Errorf("ValidateFileName(%q): unexpected error: %v", name, err)
		}
	}
}

func TestValidateFileNameInvalid(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		reason string
	}{
		{"", "empty"},
		{"1234567890123456", "16 characters, one too many"},
		{".", "reserved current-directory name"},
		{"..", "reserved parent-directory name"},
		{"a\\b", "backslash"},
		{"a/b", "forward slash"},
		{"a:b", "colon"},
		{"a\tb", "control character (tab)"},
		{"a\x7fb", "DEL byte"},
		{"café", "non-ASCII byte"},
	}
	for _, tt := range tests {
		if err := ValidateFileName(tt.name); !errors.Is(err, ErrBadFileName) {
			t.Errorf("ValidateFileName(%q) [%s]: err = %v, want ErrBadFileName", tt.name, tt.reason, err)
		}
	}
}

func TestValidateFileNameMaxLength(t *testing.T) {
	t.Parallel()

	if MaxFileName != 15 {
		t.Fatalf("MaxFileName = %d, want 15", MaxFileName)
	}
	ok := strings.Repeat("x", MaxFileName)
	if err := ValidateFileName(ok); err != nil {
		t.Errorf("ValidateFileName(%d-char name): unexpected error: %v", MaxFileName, err)
	}
	tooLong := strings.Repeat("x", MaxFileName+1)
	if err := ValidateFileName(tooLong); !errors.Is(err, ErrBadFileName) {
		t.Errorf("ValidateFileName(%d-char name): err = %v, want ErrBadFileName", MaxFileName+1, err)
	}
}
