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

//go:build !windows

package main

import (
	"context"

	"msrl.dev/mink-lasso/internal/app"
)

// gui is nil off Windows: mink-lasso's GUI depends on
// github.com/tailscale/walk, which only builds on Windows. app.Main reports
// that the GUI is unavailable and to use -headless.
var gui func(ctx context.Context, g *app.GUI) error
