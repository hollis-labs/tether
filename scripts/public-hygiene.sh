#!/usr/bin/env bash
# Reject private infrastructure identifiers in tracked text files. Filenames
# only are reported, so adjacent source content cannot leak into CI output.
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"

octet='([0-9]|[1-9][0-9]|1[0-9][0-9]|2[0-4][0-9]|25[0-5])'
tailnet="(^|[^[:alnum:]_])100[.](6[4-9]|[7-9][0-9]|1[01][0-9]|12[0-7])[.]${octet}[.]${octet}($|[^[:alnum:]_])"
domain='(^|[^[:alnum:]_-])([[:alnum:]_-]+[.])*nanite[.]cloud($|[^[:alnum:]_-])'
if matches=$(git grep -I -l -i -E -- "${domain}|${tailnet}"); then
  printf 'Private infrastructure identifiers in tracked files:\n%s\nUse example domains and documentation-only IP addresses.\n' "$matches" >&2
  exit 1
else
  result=$?
  if [[ $result != 1 ]]; then
    printf 'Public hygiene scan failed (git grep exit %s).\n' "$result" >&2
    exit "$result"
  fi
fi
printf 'Public infrastructure hygiene: clean.\n'
