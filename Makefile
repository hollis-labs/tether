.PHONY: all build install go-install uninstall run sysop-build sysop-install sysop-check release-build release-install package-release test test-race fmt vet tidy lint vuln check tools-install coverage coverage-html coverage-report clean clean-coverage

# ---------------------------------------------------------------------------
# Install / release metadata
# ---------------------------------------------------------------------------

APP_NAME := tether
HELPER_NAME := tether-apikey-helper
GO_PACKAGES := ./cmd/tether ./cmd/tether-apikey-helper ./internal/... ./pkg/...
PREFIX ?= /usr/local
BINDIR ?= $(PREFIX)/bin
GOBIN ?= $(shell go env GOPATH)/bin
VERSION ?= dev
COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
BUILD_DATE ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -X 'main.version=$(VERSION)' -X 'main.commit=$(COMMIT)' -X 'main.buildDate=$(BUILD_DATE)'

# ---------------------------------------------------------------------------
# Build / run
# ---------------------------------------------------------------------------

all: build

build:
	mkdir -p bin
	go build -ldflags "$(LDFLAGS)" -o bin/$(APP_NAME) ./cmd/tether
	go build -ldflags "$(LDFLAGS)" -o bin/$(HELPER_NAME) ./cmd/tether-apikey-helper

# `make install` — BSD/GNU convention. Honors PREFIX/BINDIR/DESTDIR.
install: build
	install -d $(DESTDIR)$(BINDIR)
	install -m 0755 bin/$(APP_NAME) $(DESTDIR)$(BINDIR)/$(APP_NAME)
	install -m 0755 bin/$(HELPER_NAME) $(DESTDIR)$(BINDIR)/$(HELPER_NAME)

# `make go-install` — wraps `go install` for Go-native devs and local Tether work.
go-install:
	GOBIN=$(GOBIN) go install -ldflags "$(LDFLAGS)" ./cmd/tether ./cmd/tether-apikey-helper
	@echo "installed → $(GOBIN)/$(APP_NAME)"
	@echo "installed → $(GOBIN)/$(HELPER_NAME)"

uninstall:
	rm -f $(DESTDIR)$(BINDIR)/$(APP_NAME) $(DESTDIR)$(BINDIR)/$(HELPER_NAME)

sysop-build:
	$(MAKE) -C apps/sysop all

sysop-install:
	$(MAKE) -C apps/sysop install

# Check the separate Go module without building frontend assets.
sysop-check:
	$(MAKE) -C apps/sysop vet test

release-build: build sysop-build

release-install: go-install sysop-install

package-release:
	VERSION=$(VERSION) BUILD_DATE=$(BUILD_DATE) COMMIT=$(COMMIT) ./scripts/release.sh

run: go-install
	$(GOBIN)/$(APP_NAME)

# ---------------------------------------------------------------------------
# Test
# ---------------------------------------------------------------------------

# go test gives every package binary 10 minutes unless told otherwise. CI sets
# TEST_TIMEOUT=20m as a workaround: the slowest package (internal/app) can need
# more under -race with cross-package coverage. Empty keeps go test's default.
TEST_TIMEOUT ?=

# Packages test-race runs. Locally the default is everything. CI passes the list
# without the store package and runs that package in its own job (see ci.yml).
RACE_PKGS ?= ./...

# gotestsum when installed, otherwise go test: one or the other, never both.
# The former `gotestsum ... || go test ...` also ran go test when gotestsum
# ran fine but a test FAILED, re-running the whole suite: a failure could be
# masked by a passing re-run, and CI hit its job timeout running the suite
# twice.
test:
	@if command -v gotestsum >/dev/null 2>&1; then \
		gotestsum -- $(if $(TEST_TIMEOUT),-timeout $(TEST_TIMEOUT)) -coverpkg=./... -coverprofile=coverage.out ./...; \
	else \
		go test $(if $(TEST_TIMEOUT),-timeout $(TEST_TIMEOUT)) -coverpkg=./... -coverprofile=coverage.out ./...; \
	fi

test-race:
	@if command -v gotestsum >/dev/null 2>&1; then \
		gotestsum -- -race $(if $(TEST_TIMEOUT),-timeout $(TEST_TIMEOUT)) -coverpkg=./... -coverprofile=coverage.out $(RACE_PKGS); \
	else \
		go test -race $(if $(TEST_TIMEOUT),-timeout $(TEST_TIMEOUT)) -coverpkg=./... -coverprofile=coverage.out $(RACE_PKGS); \
	fi

coverage: coverage.out
	@go tool cover -func=coverage.out | tail -1

coverage-html: coverage.out
	go tool cover -html=coverage.out

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

lint:
	golangci-lint run --max-issues-per-linter=0 --max-same-issues=0

vuln:
	govulncheck ./...

tidy:
	go mod tidy
	cd apps/sysop && go mod tidy

# ---------------------------------------------------------------------------
# Toolchain bootstrap
# ---------------------------------------------------------------------------

tools-install:
	go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.14.0
	go install golang.org/x/vuln/cmd/govulncheck@latest
	go install gotest.tools/gotestsum@latest

# ---------------------------------------------------------------------------
# Gates
# ---------------------------------------------------------------------------

check: fmt vet lint test-race vuln coverage-report sysop-check

clean:
	rm -rf bin dist
