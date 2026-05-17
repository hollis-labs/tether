.PHONY: all build run ui-build ui-dev test vet clean

# Build the frontend then the Go binary — a production binary with the
# Sysop UI embedded.
all: ui-build build

# Build the Sysop UI into internal/webui/dist (the Go embed directory).
ui-build:
	cd web && npm install && npm run build

# Run the Agent Ops UI dev server with hot reload. Proxies /api to the Go
# server on :8947 — run `make run` in another shell.
ui-dev:
	cd web && npm install && npm run dev

# Build the Go binary. Embeds whatever is in internal/webui/dist; run
# `make ui-build` first for a binary that serves the real UI.
build:
	go build -o tether_sysop ./cmd/tether_sysop

# Build and run the server.
run: build
	./tether_sysop

test:
	go test ./...

vet:
	go vet ./...

# Remove build artifacts. Keeps internal/webui/dist/.gitkeep so the
# //go:embed directive still compiles.
clean:
	rm -f tether_sysop
	rm -rf web/node_modules
	find internal/webui/dist -mindepth 1 ! -name .gitkeep -delete
