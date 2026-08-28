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

//go:build windows

// Package walkui is the Windows GUI: a github.com/tailscale/walk binding
// over internal/ui/model. It is deliberately dumb. Every UI decision — what
// text to show, what state a control is in, what a click means — lives in
// internal/ui/model, which is portable and fully tested; this package only
// constructs widgets, paints what Model returns, and forwards clicks to
// Model's action methods. That split is what keeps the Windows-only,
// untestable half of the app as small as possible.
package walkui
