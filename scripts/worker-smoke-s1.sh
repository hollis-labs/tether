#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
go test ./internal/workerhub -run TestSmokeS1 -count=1 -v
go build ./cmd/tether
