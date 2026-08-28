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

// Command mink-lasso watches a folder and automatically uploads new G-code
// files to a Masso G3 Touch CNC controller. See README.md for usage.
package main

import (
	"os"

	"msrl.dev/mink-lasso/internal/app"
)

func main() {
	os.Exit(app.Main(os.Args[1:], app.Deps{GUI: gui}))
}
