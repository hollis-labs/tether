#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
go test ./internal/outpostmail -run 'TestOutpostMail' -count=1 -v
go build ./cmd/tether-worker
