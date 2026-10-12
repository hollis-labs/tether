#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
worker_smoke_dir=$(mktemp -d /tmp/tether-worker-smoke.XXXXXX)
go build -o "$worker_smoke_dir/tether-worker" ./cmd/tether-worker
WORKER_SMOKE_BINARY="$worker_smoke_dir/tether-worker" go test ./internal/app -run TestSmokeS5 -count=1 -v
go build ./cmd/tether
