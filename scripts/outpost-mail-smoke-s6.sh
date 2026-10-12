#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
# Shared smoke checks operator-only defaults, notice suppression, bounded rate
# admission and retry via the existing shared wake sweep.
go test ./internal/app -run TestOutpostMailS2 -count=1 -v
