#!/usr/bin/env bash
# Fail only on scan-public findings that the base ref does not already have.
# The old tree carries findings that predate the port (owner-triaged, out of scope for next/);
# the port must add none. Usage (from next/): scripts/scan-new.sh <base-ref> [<head-ref>]
set -uo pipefail
base=${1:?base ref}; head=${2:-HEAD}
scan() { go run github.com/hollis-labs/substrate/cmd/substrate scan-public --repo .. --ref "$1" 2>&1 | grep -E '^  \[' | sort -u; }
scan "$base" > /tmp/scan-base.txt
scan "$head" > /tmp/scan-head.txt
new=$(comm -13 /tmp/scan-base.txt /tmp/scan-head.txt)
echo "base $base: $(wc -l < /tmp/scan-base.txt) finding(s); head $head: $(wc -l < /tmp/scan-head.txt) finding(s)"
if [ -n "$new" ]; then
  echo "NEW findings:"; echo "$new"; exit 1
fi
echo "no new findings"
