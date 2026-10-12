#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
go test ./internal/app -run TestOutpostMailS2 -count=1 -v
go test ./internal/workerhub ./internal/workerruntime -run 'TestSmokeS[12]$|TestOutpost' -count=1
go build ./cmd/tether
