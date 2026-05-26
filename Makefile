.PHONY: all build install go-install uninstall run sysop-build sysop-install release-build release-install package-release test test-race fmt vet tidy lint vuln check tools-install coverage coverage-html coverage-report clean clean-coverage

# ---------------------------------------------------------------------------
# Install / release metadata
# ---------------------------------------------------------------------------

APP_NAME := mux
HELPER_NAME := mux-apikey-helper
GO_PACKAGES := ./cmd/mux ./cmd/mux-apikey-helper ./internal/... ./pkg/...
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
	go build -ldflags "$(LDFLAGS)" -o bin/$(APP_NAME) ./cmd/mux
	go build -ldflags "$(LDFLAGS)" -o bin/$(HELPER_NAME) ./cmd/mux-apikey-helper

# `make install` — BSD/GNU convention. Honors PREFIX/BINDIR/DESTDIR.
install: build
	install -d $(DESTDIR)$(BINDIR)
	install -m 0755 bin/$(APP_NAME) $(DESTDIR)$(BINDIR)/$(APP_NAME)
	install -m 0755 bin/$(HELPER_NAME) $(DESTDIR)$(BINDIR)/$(HELPER_NAME)

# `make go-install` — wraps `go install` for Go-native devs and local Tether work.
go-install:
	GOBIN=$(GOBIN) go install -ldflags "$(LDFLAGS)" ./cmd/mux ./cmd/mux-apikey-helper
	@echo "installed → $(GOBIN)/$(APP_NAME)"
	@echo "installed → $(GOBIN)/$(HELPER_NAME)"

uninstall:
	rm -f $(DESTDIR)$(BINDIR)/$(APP_NAME) $(DESTDIR)$(BINDIR)/$(HELPER_NAME)

sysop-build:
	$(MAKE) -C apps/sysop all

sysop-install:
	$(MAKE) -C apps/sysop install

release-build: build sysop-build

release-install: go-install sysop-install

package-release:
	VERSION=$(VERSION) BUILD_DATE=$(BUILD_DATE) COMMIT=$(COMMIT) ./scripts/release.sh

run: go-install
	$(GOBIN)/$(APP_NAME)

# ---------------------------------------------------------------------------
# Test
# ---------------------------------------------------------------------------

test:
	@command -v gotestsum >/dev/null 2>&1 && \
		gotestsum -- -coverpkg=./... -coverprofile=coverage.out ./... || \
		go test -coverpkg=./... -coverprofile=coverage.out ./...

test-race:
	@command -v gotestsum >/dev/null 2>&1 && \
		gotestsum -- -race -coverpkg=./... -coverprofile=coverage.out ./... || \
		go test -race -coverpkg=./... -coverprofile=coverage.out ./...

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

# ---------------------------------------------------------------------------
# Toolchain bootstrap
# ---------------------------------------------------------------------------

tools-install:
	go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest
	go install golang.org/x/vuln/cmd/govulncheck@latest
	go install gotest.tools/gotestsum@latest

# ---------------------------------------------------------------------------
# Gates
# ---------------------------------------------------------------------------

check: fmt vet lint test-race vuln coverage-report

clean:
	rm -rf bin dist
