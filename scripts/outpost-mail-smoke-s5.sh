#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
# S2's real wire/store smoke includes grant denial/acceptance, managed fencing,
# exact worker-ID namespaces, sender default deny, and device revocation.
go test ./internal/app -run TestOutpostMailS2 -count=1 -v
