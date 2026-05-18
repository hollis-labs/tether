.PHONY: all build run install ui-build ui-dev test vet clean

# Build the frontend then the Go binary — a production binary with the
# Sysop UI embedded.
all: ui-build build

# Build the Sysop UI into internal/webui/dist (the Go embed directory).
ui-build:
	cd frontend && npm install && npm run build

# Run the Agent Ops UI dev server with hot reload. Proxies /api to the Go
# server on :8947 — run `make run` in another shell. The dev UI is served
# under /operations/ (see `base` in frontend/vite.config.ts).
ui-dev:
	cd frontend && npm install && npm run dev

# Build the Go binary. Embeds whatever is in internal/webui/dist; run
# `make ui-build` first for a binary that serves the real UI.
build:
	go build -o tether_sysop ./cmd/tether_sysop

# Build and run the server.
run: build
	./tether_sysop

# Build the embedded UI and install the binary to $GOBIN — a self-contained
# tether_sysop on PATH that serves the real Sysop UI.
install: ui-build
	go install ./cmd/tether_sysop

test:
	go test ./...

vet:
	go vet ./...

# Remove build artifacts. Keeps internal/webui/dist/.gitkeep so the
# //go:embed directive still compiles.
clean:
	rm -f tether_sysop
	rm -rf frontend/node_modules
	find internal/webui/dist -mindepth 1 ! -name .gitkeep -delete
