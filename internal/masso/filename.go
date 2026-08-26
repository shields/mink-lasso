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

import "fmt"

// Extensions are the file extensions, lower case with the leading dot, that
// Masso Link offers for upload; the controller runs programs with these
// extensions from its USB drive.
var Extensions = []string{".nc", ".txt", ".cnc", ".tap", ".eia", ".htg", ".wiz", ".gcode", ".ngc"}

// ValidateFileName reports whether name is a valid Masso upload file name:
// 1-15 printable-ASCII bytes (0x20-0x7E), excluding '\', '/', and ':', and
// excluding the reserved names "." and "..". It wraps ErrBadFileName with
// the specific reason.
func ValidateFileName(name string) error {
	if len(name) < 1 || len(name) > MaxFileName {
		return fmt.Errorf("%w: %q is %d bytes, want 1-%d", ErrBadFileName, name, len(name), MaxFileName)
	}
	if name == "." || name == ".." {
		return fmt.Errorf("%w: %q is a reserved name", ErrBadFileName, name)
	}
	for i := range len(name) {
		b := name[i]
		if b < 0x20 || b > 0x7E {
			return fmt.Errorf("%w: %q has a non-printable byte at %d", ErrBadFileName, name, i)
		}
		if b == '\\' || b == '/' || b == ':' {
			return fmt.Errorf("%w: %q contains %q", ErrBadFileName, name, string(b))
		}
	}
	return nil
}
