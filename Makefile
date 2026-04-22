.PHONY: build install run test test-race fmt vet tidy lint vuln check tools-install coverage coverage-html clean-coverage

# ---------------------------------------------------------------------------
# Build / run
# ---------------------------------------------------------------------------

# GOBIN is the canonical user install location — always build here so the
# system `mux` command reflects the current code. Agents must use `make install`
# after code changes, then tell the user: "built to /Users/chrispian/go/bin/mux".
GOBIN ?= /Users/chrispian/go/bin

build:
	go build -o bin/mux ./cmd/mux

# install puts mux in the user's PATH ($GOBIN). Use this — not `go build` — so
# `mux tui` always runs the latest version, not a stale local binary.
install:
	GOBIN=$(GOBIN) go install ./cmd/mux/...
	@echo "installed → $(GOBIN)/mux"

run: install
	$(GOBIN)/mux

# ---------------------------------------------------------------------------
# Test
# ---------------------------------------------------------------------------

# test is the fast iteration loop — no race detector so it compiles + runs
# quickly during TDD. Collects coverage into coverage.out (cheap, zero gate).
test:
	@command -v gotestsum >/dev/null 2>&1 && \
		gotestsum -- -coverprofile=coverage.out ./... || \
		go test -coverprofile=coverage.out ./...

# test-race is the correctness gate: full suite with the race detector.
# Coverage is omitted here — race-instrumented runs are already slow.
test-race:
	@command -v gotestsum >/dev/null 2>&1 && \
		gotestsum -- -race ./... || \
		go test -race ./...

# Show total coverage percentage from the most recent `make test` run.
coverage: coverage.out
	@go tool cover -func=coverage.out | tail -1

# Open coverage in the browser.
coverage-html: coverage.out
	go tool cover -html=coverage.out

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
check: fmt vet lint test-race vuln
