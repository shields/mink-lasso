# Copyright © 2026 Michael Shields
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

.PHONY: build coverage fmt integration lint run sim test version winres

GO_TEST_FLAGS ?= -race -count=1
COVERAGE_FILE := coverage.out
EXE := dist/mink-lasso.exe

# Packages held to 100% statement coverage. The walk binding needs a Windows
# desktop session and the main packages are one-liners, so they are excluded.
COVER_PKGS := $(shell go list ./... | grep -v -e /cmd/ -e /internal/ui/walkui)

# gitcalver.org: the committer date of HEAD in UTC, then the number of commits
# reachable from HEAD that share that UTC date.
VERSION ?= $(shell d=$$(TZ=UTC git log -1 --date=format-local:%Y%m%d --format=%cd 2>/dev/null); \
	if [ -n "$$d" ]; then echo "$$d.$$(TZ=UTC git log --date=format-local:%Y%m%d --format=%cd | grep -c "^$$d$$")"; \
	else echo dev; fi)

LDFLAGS := -H windowsgui -s -w -X msrl.dev/mink-lasso/internal/app.version=$(VERSION)

# `go tool` builds tools for $(GOOS), so the Windows lint pass needs a
# host-built linter binary.
TOOLS := dist/tools
ifeq ($(OS),Windows_NT)
HOST_EXE := .exe
endif
GOLANGCI := $(TOOLS)/golangci-lint$(HOST_EXE)

$(GOLANGCI): go.mod go.sum
	go build -o $@ github.com/golangci/golangci-lint/v2/cmd/golangci-lint

lint: $(GOLANGCI)
	go mod tidy -diff
	$(GOLANGCI) run ./...
	GOOS=windows $(GOLANGCI) run ./...
	bunx prettier --check .

fmt: $(GOLANGCI)
	$(GOLANGCI) fmt ./...
	bunx prettier --write .

test:
	go test $(GO_TEST_FLAGS) ./...

coverage:
	@untested=$$(go list -f '{{if not (or .TestGoFiles .XTestGoFiles)}}{{.ImportPath}}{{end}}' $(COVER_PKGS)); \
	if [ -n "$$untested" ]; then echo "FAIL: packages without tests:"; echo "$$untested"; exit 1; fi
	go test $(GO_TEST_FLAGS) -covermode=atomic -coverprofile=$(COVERAGE_FILE) $(COVER_PKGS)
	@LC_ALL=C awk 'NR>1{t+=$$2;if($$3>0)c+=$$2} \
	  END{printf "Coverage: %.1f%%\n",(t>0?100*c/t:0); \
	  if(c!=t){print "FAIL: coverage is not 100.0%";exit 1}}' $(COVERAGE_FILE)

version:
	@echo $(VERSION)

winres:
	go tool go-winres make --in build/winres.json --out cmd/mink-lasso/rsrc --arch amd64

build:
	CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -trimpath -ldflags "$(LDFLAGS)" -o $(EXE) ./cmd/mink-lasso

# Run the app natively in headless mode, e.g. against the simulator:
#   make run RUN_ARGS="-watch /tmp/w -serial G3-1 -address 127.0.0.1:65535"
run:
	go run ./cmd/mink-lasso -headless $(RUN_ARGS)

sim:
	go run ./cmd/masso-sim $(SIM_ARGS)

# Runs against a real controller; every test skips unless MINK_LASSO_SERIAL is set.
integration: build
	MINK_LASSO_EXE=$(abspath $(EXE)) go test -tags integration -count=1 -v -timeout 15m ./internal/integration/...
