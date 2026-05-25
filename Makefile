.PHONY: build install run sysop-build sysop-install release-build release-install test test-race fmt vet tidy lint vuln check tools-install coverage coverage-html coverage-report clean-coverage

# ---------------------------------------------------------------------------
# Build / run
# ---------------------------------------------------------------------------

# GOBIN is the canonical user install location — always build here so the
# system `mux` command reflects the current code. Agents must use `make install`
# after code changes, then tell the user: "built to /Users/chrispian/go/bin/mux".
GOBIN ?= $(shell go env GOPATH)/bin

build:
	go build -o bin/mux ./cmd/mux
	go build -o bin/mux-apikey-helper ./cmd/mux-apikey-helper

# install puts mux in the user's PATH ($GOBIN).
install:
	GOBIN=$(GOBIN) go install ./cmd/mux ./cmd/mux-apikey-helper
	@echo "installed → $(GOBIN)/mux"
	@echo "installed → $(GOBIN)/mux-apikey-helper"

sysop-build:
	$(MAKE) -C apps/sysop all

sysop-install:
	$(MAKE) -C apps/sysop install

release-build: build sysop-build

release-install: install sysop-install

run: install
	$(GOBIN)/mux

# ---------------------------------------------------------------------------
# Test
# ---------------------------------------------------------------------------

# test is the fast iteration loop — no race detector so it compiles + runs
# quickly during TDD. Collects cross-package coverage so integration tests
# in /api and /daemon credit the packages they actually exercise.
test:
	@command -v gotestsum >/dev/null 2>&1 && \
		gotestsum -- -coverpkg=./... -coverprofile=coverage.out ./... || \
		go test -coverpkg=./... -coverprofile=coverage.out ./...

# test-race is the correctness gate: full suite with the race detector
# and the same cross-package coverage profile feeding `make coverage`.
test-race:
	@command -v gotestsum >/dev/null 2>&1 && \
		gotestsum -- -race -coverpkg=./... -coverprofile=coverage.out ./... || \
		go test -race -coverpkg=./... -coverprofile=coverage.out ./...

# Show total coverage percentage from the most recent `make test` /
# `make test-race` / `make check` run.
coverage: coverage.out
	@go tool cover -func=coverage.out | tail -1

# Open coverage in the browser.
coverage-html: coverage.out
	go tool cover -html=coverage.out

# coverage-report is the inline reporter for `make check`. Prints the
# aggregate (cross-package) total without failing the gate — this sprint
# captures the floor; v005-10b adds a hard threshold once the floor is
# known.
coverage-report: coverage.out
	@echo ""
	@echo "Coverage (aggregate, cross-package):"
	@go tool cover -func=coverage.out | tail -1

coverage.out:
	$(MAKE) test

clean-coverage:
	rm -f coverage.out

# ---------------------------------------------------------------------------
# Format / vet / lint / vuln
# ---------------------------------------------------------------------------

fmt:
	go fmt ./...

vet:
	go vet ./...

# Full lint: golangci-lint already wraps staticcheck / errcheck / govet.
# Uncapped output so CI surfaces every issue.
lint:
	golangci-lint run --max-issues-per-linter=0 --max-same-issues=0

# Vulnerability scan — separate target so CI can run it on a schedule.
vuln:
	govulncheck ./...

tidy:
	go mod tidy

# ---------------------------------------------------------------------------
# Toolchain bootstrap
# ---------------------------------------------------------------------------
# Installs developer + CI tooling to $GOBIN (or $GOPATH/bin).
# golangci-lint v2 path: github.com/golangci/golangci-lint/v2/cmd/golangci-lint
tools-install:
	go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest
	go install golang.org/x/vuln/cmd/govulncheck@latest
	go install gotest.tools/gotestsum@latest

# ---------------------------------------------------------------------------
# Gates
# ---------------------------------------------------------------------------
# check is the pre-commit full gate used by CI and local pre-push hooks.
# test-race emits coverage.out (cross-package); coverage-report prints the
# aggregate total at the end without acting as a hard gate (v005-10
# baselines the floor; v005-10b lands the threshold).
check: fmt vet lint test-race vuln coverage-report
