#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
OUTPOST_NATIVE_SMOKE=1 go test ./internal/outpostmail -run TestOutpostMailS3 -count=1 -v
