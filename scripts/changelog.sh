#!/usr/bin/env bash
# Changelog fragments (CW-20261016-0159).
#
# Every PR that wants a CHANGELOG entry adds changes/<TASK-ID>-<slug>.md instead
# of editing CHANGELOG.md, so parallel PRs never conflict on it. The release
# owner assembles the fragments into CHANGELOG.md in a release/* PR.
#
#   scripts/changelog.sh check                 validate every fragment
#   scripts/changelog.sh check-pr              check + the "do not edit CHANGELOG.md" rule
#   scripts/changelog.sh pending               exit 1 (and list them) if fragments are waiting
#   scripts/changelog.sh assemble VERSION [DATE]
#
# Environment:
#   CHANGES_DIR   fragment directory                    (default: changes)
#   CHANGELOG     changelog file                        (default: CHANGELOG.md)
#   CHANGELOG_FRAGMENTS_ENFORCE   1 = editing CHANGELOG.md in a PR fails check-pr,
#                                 0 = ::warning only    (default: 1)
#   CHANGELOG_BRANCH   the PR's head branch; release/* is exempt
#   CHANGELOG_BASE     git revision the PR is compared with (default: HEAD^1, the
#                      base side of the pull_request merge commit)
set -euo pipefail

CHANGES_DIR="${CHANGES_DIR:-changes}"
CHANGELOG="${CHANGELOG:-CHANGELOG.md}"
ENFORCE="${CHANGELOG_FRAGMENTS_ENFORCE:-1}"

work=""

die() { echo "changelog: $*" >&2; exit 1; }

# trim_blank drops leading and trailing blank lines from stdin.
trim_blank() {
  awk 'NF || seen { seen = 1 } seen { lines[++n] = $0 } END { while (n > 0 && lines[n] ~ /^[[:space:]]*$/) n--; for (i = 1; i <= n; i++) print lines[i] }'
}

# fragments prints the fragment paths in filename order (README.md excluded).
fragments() {
  [[ -d "$CHANGES_DIR" ]] || return 0
  local f
  for f in "$CHANGES_DIR"/*.md; do
    [[ -e "$f" ]] || continue
    [[ "$(basename "$f")" == "README.md" ]] && continue
    printf '%s\n' "$f"
  done | LC_ALL=C sort
}

# validate_one prints one problem per line for a fragment and returns 1 if any.
validate_one() {
  local f="$1" base problems=0 line n=0 bullets=0 has_id=1 in_bullet=0
  base="$(basename "$f")"
  if ! [[ "$base" =~ ^[A-Z]+-[0-9]{8}-[0-9]{4}-[a-z0-9][a-z0-9-]*\.md$ ]]; then
    echo "$f: name must be <TASK-ID>-<slug>.md (for example CW-20261016-0159-changelog-fragments.md)"
    problems=1
  fi
  if ! grep -q '[^[:space:]]' "$f"; then
    echo "$f: fragment is empty"
    return 1
  fi
  while IFS= read -r line || [[ -n "$line" ]]; do
    n=$((n + 1))
    if [[ "$line" =~ ^[[:space:]]*$ ]]; then
      continue
    elif [[ "$line" =~ ^# ]]; then
      echo "$f:$n: a fragment holds bullets only; remove the markdown heading"
      problems=1
    elif [[ "$line" =~ ^-\  ]]; then
      if ((in_bullet && !has_id)); then
        echo "$f: a bullet before line $n names no task id (CW-YYYYMMDD-NNNN)"
        problems=1
      fi
      bullets=$((bullets + 1))
      in_bullet=1
      has_id=0
      [[ "$line" =~ [A-Z]+-[0-9]{8}-[0-9]{4} ]] && has_id=1
    elif [[ "$line" =~ ^[[:space:]][[:space:]] ]]; then
      ((in_bullet)) || { echo "$f:$n: continuation line outside a bullet"; problems=1; }
      [[ "$line" =~ [A-Z]+-[0-9]{8}-[0-9]{4} ]] && has_id=1
    else
      echo "$f:$n: lines must start with '- ' (a bullet) or two spaces (its continuation)"
      problems=1
    fi
  done <"$f"
  if ((bullets == 0)); then
    echo "$f: no bullet found (start the entry with '- ')"
    problems=1
  elif ((in_bullet && !has_id)); then
    echo "$f: the last bullet names no task id (CW-YYYYMMDD-NNNN)"
    problems=1
  fi
  return "$problems"
}

cmd_check() {
  local f bad=0 out count=0
  while IFS= read -r f; do
    [[ -n "$f" ]] || continue
    count=$((count + 1))
    if ! out="$(validate_one "$f")"; then
      printf '%s\n' "$out" >&2
      bad=1
    fi
  done < <(fragments)
  ((bad == 0)) || die "malformed fragment(s); see $CHANGES_DIR/README.md"
  echo "changelog: $count fragment(s) ok"
}

cmd_pending() {
  local list f
  list="$(fragments)"
  [[ -z "$list" ]] && return 0
  echo "changelog: unassembled fragments in $CHANGES_DIR/:" >&2
  while IFS= read -r f; do printf '  %s\n' "$f"; done <<<"$list" >&2
  return 1
}

cmd_check_pr() {
  cmd_check
  case "${CHANGELOG_BRANCH:-}" in
    release/*) echo "changelog: release/* branch, CHANGELOG.md edits allowed"; return 0 ;;
  esac
  local base="${CHANGELOG_BASE:-HEAD^1}" changed
  changed="$(git diff --name-only "$base" HEAD -- "$CHANGELOG" 2>/dev/null)" || die "cannot diff against $base (checkout needs fetch-depth: 2)"
  [[ -z "$changed" ]] && return 0
  local msg="this PR edits $CHANGELOG. Add $CHANGES_DIR/<TASK-ID>-<slug>.md instead of editing $CHANGELOG (see $CHANGES_DIR/README.md); the release owner assembles fragments."
  if [[ "$ENFORCE" == "1" ]]; then
    echo "::error file=$CHANGELOG::$msg"
    echo "changelog: $msg" >&2
    return 1
  fi
  echo "::warning file=$CHANGELOG::$msg (advisory for now; CHANGELOG_FRAGMENTS_ENFORCE=0)"
  echo "changelog: WARNING: $msg" >&2
}

cmd_assemble() {
  local version="${1:-}" date="${2:-$(date -u +%F)}"
  [[ -n "$version" ]] || die "usage: changelog.sh assemble VERSION [DATE]"
  [[ "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$ ]] || die "VERSION must look like 1.2.3 (got '$version')"
  [[ "$date" =~ ^[0-9]{4}-[0-9]{2}-[0-9]{2}$ ]] || die "DATE must be YYYY-MM-DD (got '$date')"
  [[ -f "$CHANGELOG" ]] || die "$CHANGELOG not found"
  grep -q '^## \[Unreleased\]' "$CHANGELOG" || die "$CHANGELOG has no '## [Unreleased]' section"
  if grep -q "^## \[$version\]" "$CHANGELOG"; then die "$CHANGELOG already has a section for $version"; fi
  cmd_check >/dev/null
  local list
  list="$(fragments)"
  [[ -n "$list" ]] || die "no fragments in $CHANGES_DIR/: nothing to assemble"

  local tmp body f first_prev
  work="$(mktemp -d)"
  trap 'rm -rf "${work:-}"' EXIT
  tmp="$work/changelog"
  body="$work/body"
  # fragments first (filename order), then the bullets already under [Unreleased]
  while IFS= read -r f; do
    trim_blank <"$f"
  done <<<"$list" >"$body"
  awk '
    /^## \[Unreleased\]/ { in_u = 1; next }
    in_u && /^## \[/ { exit }
    in_u && /^\[[^]]+\]: / { exit }
    in_u { print }
  ' "$CHANGELOG" | trim_blank >"$work/base"
  if [[ -s "$work/base" ]]; then
    cat "$work/base" >>"$body"
  fi

  first_prev="$(grep -m1 -E '^## \[[0-9]' "$CHANGELOG" | sed -E 's/^## \[([^]]+)\].*/\1/' || true)"

  awk -v version="$version" -v date="$date" -v bodyfile="$body" '
    function emit_section(   line) {
      print "## [" version "] - " date
      print ""
      while ((getline line < bodyfile) > 0) print line
      close(bodyfile)
      print ""
    }
    /^## \[Unreleased\]/ { print; print ""; unreleased = 1; skipping = 1; next }
    skipping && (/^## \[/ || /^\[[^]]+\]: /) { emit_section(); skipping = 0 }
    skipping { next }
    { print }
    END { if (skipping) emit_section() }
  ' "$CHANGELOG" >"$tmp"

  # keep the link references current when the file carries them
  if grep -q '^\[Unreleased\]: ' "$tmp"; then
    local repo
    repo="$(grep -m1 '^\[Unreleased\]: ' "$tmp" | sed -E 's|^\[Unreleased\]: (.*)/compare/.*|\1|')"
    awk -v repo="$repo" -v version="$version" -v prev="$first_prev" '
      /^\[Unreleased\]: / {
        print "[Unreleased]: " repo "/compare/v" version "...HEAD"
        if (prev != "") print "[" version "]: " repo "/compare/v" prev "...v" version
        else print "[" version "]: " repo "/releases/tag/v" version
        next
      }
      { print }
    ' "$tmp" >"$tmp.2" && mv "$tmp.2" "$tmp"
  fi

  cat "$tmp" >"$CHANGELOG"
  while IFS= read -r f; do
    if git ls-files --error-unmatch "$f" >/dev/null 2>&1; then git rm -q -f "$f"; else rm -f "$f"; fi
  done <<<"$list"
  echo "changelog: assembled $(wc -l <<<"$list" | tr -d ' ') fragment(s) into $CHANGELOG as [$version] - $date"
}

case "${1:-}" in
  check) cmd_check ;;
  check-pr) cmd_check_pr ;;
  pending) cmd_pending ;;
  assemble) shift; cmd_assemble "$@" ;;
  *) die "usage: changelog.sh check | check-pr | pending | assemble VERSION [DATE]" ;;
esac
