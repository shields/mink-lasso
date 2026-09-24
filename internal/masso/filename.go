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
	"fmt"
	"strings"
)

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
	return validateComponent(name, ErrBadFileName)
}

// ValidateUploadDir reports whether dir is a valid upload directory: ""
// for the drive root, or at most MaxUploadDir bytes of backslash-separated
// components, each non-empty and following ValidateFileName's character
// rules but not its length limit. It wraps ErrBadUploadDir with the
// specific reason.
func ValidateUploadDir(dir string) error {
	if dir == "" {
		return nil
	}
	if len(dir) > MaxUploadDir {
		return fmt.Errorf("%w: %q is %d bytes, want at most %d", ErrBadUploadDir, dir, len(dir), MaxUploadDir)
	}
	for component := range strings.SplitSeq(dir, `\`) {
		if component == "" {
			return fmt.Errorf("%w: %q has an empty component", ErrBadUploadDir, dir)
		}
		if err := validateComponent(component, ErrBadUploadDir); err != nil {
			return err
		}
	}
	return nil
}

// validateComponent applies the character rules shared by file names and
// upload directory components, wrapping sentinel with the reason.
func validateComponent(s string, sentinel error) error {
	if s == "." || s == ".." {
		return fmt.Errorf("%w: %q is a reserved name", sentinel, s)
	}
	for i := range len(s) {
		b := s[i]
		if b < 0x20 || b > 0x7E {
			return fmt.Errorf("%w: %q has a non-printable byte at %d", sentinel, s, i)
		}
		if b == '\\' || b == '/' || b == ':' {
			return fmt.Errorf("%w: %q contains %q", sentinel, s, string(b))
		}
	}
	return nil
}
