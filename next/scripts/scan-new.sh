#!/usr/bin/env bash
# Fail only on scan-public findings that the base ref does not already have.
# The old tree carries findings that predate the port (owner-triaged, out of scope for next/);
# the port must add none. Usage (from next/): scripts/scan-new.sh <base-ref> [<head-ref>]
set -uo pipefail
base=${1:?base ref}; head=${2:-HEAD}
scan() { go run github.com/hollis-labs/substrate/cmd/substrate scan-public --repo .. --ref "$1" 2>&1 | grep -E '^  \[' | sort -u; }
# Each finding line carries rule, match and path:line, so identical text in a new file is a new line.
tmp=$(mktemp -d); trap 'rm -rf "$tmp"' EXIT
scan "$base" > "$tmp/base.txt"
scan "$head" > "$tmp/head.txt"
new=$(comm -13 "$tmp/base.txt" "$tmp/head.txt")
echo "base $base: $(wc -l < "$tmp/base.txt") finding line(s); head $head: $(wc -l < "$tmp/head.txt") finding line(s)"
if [ -n "$new" ]; then
  echo "NEW findings:"; echo "$new"; exit 1
fi
echo "no new findings"
