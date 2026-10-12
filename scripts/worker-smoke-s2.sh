#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
go test ./internal/workerruntime -run TestSmokeS2 -count=1 -v
go build ./cmd/tether-worker
