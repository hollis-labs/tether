#!/usr/bin/env bash
set -euo pipefail
script="$(cd "$(dirname "$0")" && pwd)/public-hygiene.sh"
fixture=$(mktemp -d)
trap 'rm -rf "$fixture"' EXIT
cd "$fixture"
git init -q
printf 'https://tether.example 192.0.2.10 100.63.255.255 100.128.0.1\n' > example.txt
git add example.txt
"$script" >/dev/null
for second in 64 69 70 99 100 119 120 127; do
  printf 'http://100.%s.0.1/path\n' "$second" > example.txt
  if "$script" >/dev/null 2>&1; then
    printf 'Guard missed shared address range boundary %s\n' "$second" >&2
    exit 1
  fi
done
for host in 'nanite' 'hub.nanite' 'TETHER.HUB.NANITE'; do
  printf 'https://%s.cloud/path\n' "$host" > example.txt
  if "$script" >/dev/null 2>&1; then
    printf 'Guard missed internal domain\n' >&2
    exit 1
  fi
done
printf 'https://tether.example 192.0.2.10\n' > example.txt
# An untracked private fixture does not belong to the published tree.
printf 'https://hub.nanite%scloud\n' '.' > untracked.txt
"$script" >/dev/null
printf 'Public infrastructure hygiene fixtures: passed.\n'
