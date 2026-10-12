#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
go test ./internal/workerruntime -run TestSmokeS6 -count=1 -v
worker_smoke_dir=$(mktemp -d /tmp/tether-worker-build.XXXXXX)
for platform in darwin linux; do
  for arch in amd64 arm64; do
    GOOS="$platform" GOARCH="$arch" CGO_ENABLED=0 go build -o "$worker_smoke_dir/tether-worker-$platform-$arch" ./cmd/tether-worker
  done
done
echo "worker cross-platform builds: $worker_smoke_dir"
