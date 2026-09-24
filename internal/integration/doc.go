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

//go:build integration

// Package integration runs mink-lasso's automated checks against a real
// Masso G3 Touch controller: discovery, status, the tool table, file
// uploads, and a headless end-to-end run of the built exe against a watched
// folder. It is built with the "integration" tag and skips itself unless
// MINK_LASSO_SERIAL names the controller to use; see README.md's
// "Integration tests" section for how to run it.
//
// A controller talks to one client at a time, so every test in this package
// runs sequentially, never with t.Parallel().
//
// # Manual checks
//
// These require conditions this suite cannot arrange from a test process,
// so check them by hand after a change that could affect them:
//
//   - No USB drive connected: Upload should surface masso.ErrNoUSB and the
//     engine should present it clearly.
//   - Canceling a transfer on the Masso's own screen: Upload should surface
//     masso.ErrCanceled.
//   - A feed hold or operator prompt during a job: Status.WaitingForOperator
//     should gate automatic upload as documented, and clear once resumed.
//
// # File naming
//
// Every file this suite writes to the controller's USB drive is named
// MLTEST<n>.NC, and the subfolder upload writes into an MLTEST folder at the
// drive root, so both are easy to find and delete from the controller
// afterward; nothing else on the drive should match those names.
package integration
