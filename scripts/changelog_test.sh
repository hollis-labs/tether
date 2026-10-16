#!/usr/bin/env bash
# Tests for scripts/changelog.sh on throwaway git repos (CW-20261016-0159).
set -uo pipefail

SCRIPT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/changelog.sh"
RELEASE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/release.sh"
root="$(mktemp -d)"
trap 'rm -rf "$root"' EXIT
fails=0
n=0

ok() { n=$((n + 1)); }
fail() { fails=$((fails + 1)); echo "FAIL: $*" >&2; }
expect_ok() { local name="$1"; shift; ok; "$@" >"$root/out" 2>&1 || { fail "$name (exit $?): $(cat "$root/out")"; return 1; }; }
expect_fail() { local name="$1" want="$2"; shift 2; ok; if "$@" >"$root/out" 2>&1; then fail "$name: expected failure"; return 1; fi; grep -q -- "$want" "$root/out" || fail "$name: output lacks '$want': $(cat "$root/out")"; }

new_repo() {
  local d
  d="$(mktemp -d "$root/repo.XXXXXX")"
  (
    cd "$d" || exit 1
    git init -q -b main
    git config user.email t@example.invalid
    git config user.name t
    mkdir changes
    printf '# Fragments\n' >changes/README.md
    cat >CHANGELOG.md <<'CL'
# Changelog

Intro text.

## [Unreleased]

- Baseline entry one. (CW-20260101-0001)
- Baseline entry two
  continues here. (CW-20260101-0002)

## [0.8.0] - 2026-10-09

- Old release entry. (CW-20251009-0001)

[Unreleased]: https://example.invalid/r/compare/v0.7.0...HEAD
[0.8.0]: https://example.invalid/r/compare/v0.7.0...v0.8.0
CL
    git add -A && git commit -q -m init
  )
  echo "$d"
}

frag() { # dir name body
  printf '%s\n' "$3" >"$1/changes/$2"
}

cl() { (cd "$1" && shift && "$SCRIPT" "$@"); }

# --- validation ---------------------------------------------------------------
r="$(new_repo)"
frag "$r" CW-20261016-0159-good.md '- Add the thing.
  More detail. (CW-20261016-0159)
- Second bullet. (CW-20261016-0160)'
expect_ok "valid multi-bullet fragment" cl "$r" check
rm "$r/changes/CW-20261016-0159-good.md"

frag "$r" notaskid.md '- x (CW-20261016-0159)'
expect_fail "bad name" "name must be" cl "$r" check
rm "$r/changes/notaskid.md"

frag "$r" CW-20261016-0159-heading.md '## Added
- x (CW-20261016-0159)'
expect_fail "heading rejected" "remove the markdown heading" cl "$r" check
rm "$r/changes/CW-20261016-0159-heading.md"

frag "$r" CW-20261016-0159-noid.md '- No task id anywhere.'
expect_fail "missing task id" "names no task id" cl "$r" check
rm "$r/changes/CW-20261016-0159-noid.md"

frag "$r" CW-20261016-0159-prose.md 'Just prose (CW-20261016-0159)'
expect_fail "prose rejected" "must start with" cl "$r" check
rm "$r/changes/CW-20261016-0159-prose.md"

: >"$r/changes/CW-20261016-0159-empty.md"
expect_fail "empty fragment" "empty" cl "$r" check
rm "$r/changes/CW-20261016-0159-empty.md"

frag "$r" CW-20261016-0159-second.md '- first (CW-20261016-0159)
- second bullet without an id'
expect_fail "every bullet needs an id" "names no task id" cl "$r" check
rm "$r/changes/CW-20261016-0159-second.md"

# --- check-pr ------------------------------------------------------------------
r="$(new_repo)"
(cd "$r" && git checkout -q -b feature && frag . CW-20261016-0200-x.md '- ok (CW-20261016-0200)' 2>/dev/null; true)
(cd "$r" && printf -- '- ok (CW-20261016-0200)\n' >changes/CW-20261016-0200-x.md && git add -A && git commit -q -m frag)
expect_ok "fragment-only PR passes" env CHANGELOG_BASE=main CHANGELOG_BRANCH=feature bash -c "cd '$r' && '$SCRIPT' check-pr"
(cd "$r" && printf -- '- sneaky (CW-20261016-0201)\n' >>CHANGELOG.md && git commit -q -am edit)
expect_fail "editing CHANGELOG fails when enforced" "Add changes/<TASK-ID>-<slug>.md" env CHANGELOG_BASE=main CHANGELOG_BRANCH=feature CHANGELOG_FRAGMENTS_ENFORCE=1 bash -c "cd '$r' && '$SCRIPT' check-pr"
expect_ok "advisory mode warns but passes" env CHANGELOG_BASE=main CHANGELOG_BRANCH=feature CHANGELOG_FRAGMENTS_ENFORCE=0 bash -c "cd '$r' && '$SCRIPT' check-pr"
ok; grep -q '::warning' "$root/out" || fail "advisory mode emitted no ::warning annotation"
expect_ok "release/* branches may edit CHANGELOG" env CHANGELOG_BASE=main CHANGELOG_BRANCH=release/v1.2.3 CHANGELOG_FRAGMENTS_ENFORCE=1 bash -c "cd '$r' && '$SCRIPT' check-pr"
# a malformed fragment fails even in advisory mode
(cd "$r" && git checkout -q main && git checkout -q -b bad && printf 'nope\n' >changes/CW-20261016-0202-bad.md && git add -A && git commit -q -m bad)
expect_fail "malformed fragment fails even when advisory" "malformed" env CHANGELOG_BASE=main CHANGELOG_FRAGMENTS_ENFORCE=0 bash -c "cd '$r' && '$SCRIPT' check-pr"

# main moving on (a CHANGELOG edit landed after the branch point) is not the PR's doing
r="$(new_repo)"
(cd "$r" && git checkout -q -b feature && printf -- '- ok (CW-20261016-0210)\n' >changes/CW-20261016-0210-x.md && git add -A && git commit -q -m frag \
  && git checkout -q main && printf -- '- landed on main (CW-20261016-0211)\n' >>CHANGELOG.md && git commit -q -am "main edits changelog" && git checkout -q feature)
expect_ok "a base that moved on is not blamed on the PR" env CHANGELOG_BASE=main CHANGELOG_BRANCH=feature CHANGELOG_FRAGMENTS_ENFORCE=1 bash -c "cd '$r' && '$SCRIPT' check-pr"

# --- two parallel PRs merge without a conflict (the acceptance test) ------------
r="$(new_repo)"
(
  cd "$r" || exit 1
  git checkout -q -b a && printf -- '- Entry A. (CW-20261016-0301)\n' >changes/CW-20261016-0301-a.md && git add -A && git commit -q -m a
  git checkout -q main && git checkout -q -b b && printf -- '- Entry B. (CW-20261016-0302)\n' >changes/CW-20261016-0302-b.md && git add -A && git commit -q -m b
  git checkout -q main && git merge -q --no-edit a && git merge -q --no-edit b
) >"$root/out" 2>&1
ok; [[ -z "$(git -C "$r" status --porcelain)" && -e "$r/changes/CW-20261016-0301-a.md" && -e "$r/changes/CW-20261016-0302-b.md" ]] || fail "parallel fragment PRs conflicted: $(cat "$root/out")"

# --- assemble -----------------------------------------------------------------
expect_ok "assemble" cl "$r" assemble 1.2.3 2026-11-01
want='# Changelog

Intro text.

## [Unreleased]

## [1.2.3] - 2026-11-01

- Entry A. (CW-20261016-0301)
- Entry B. (CW-20261016-0302)
- Baseline entry one. (CW-20260101-0001)
- Baseline entry two
  continues here. (CW-20260101-0002)

## [0.8.0] - 2026-10-09

- Old release entry. (CW-20251009-0001)

[Unreleased]: https://example.invalid/r/compare/v1.2.3...HEAD
[1.2.3]: https://example.invalid/r/compare/v0.8.0...v1.2.3
[0.8.0]: https://example.invalid/r/compare/v0.7.0...v0.8.0'
got="$(cat "$r/CHANGELOG.md")"
ok; [[ "$got" == "$want" ]] || { fail "assembled CHANGELOG differs"; diff <(printf '%s\n' "$want") <(printf '%s\n' "$got") >&2; }
ok; [[ ! -e "$r/changes/CW-20261016-0301-a.md" && ! -e "$r/changes/CW-20261016-0302-b.md" && -e "$r/changes/README.md" ]] || fail "fragments not removed (or README removed)"
expect_ok "nothing pending after assemble" cl "$r" pending
expect_fail "nothing left to assemble" "nothing to assemble" cl "$r" assemble 1.2.4 2026-11-02
expect_fail "duplicate version refused" "already has a section" bash -c "cd '$r' && printf -- '- x (CW-20261016-0399)\n' >changes/CW-20261016-0399-x.md && '$SCRIPT' assemble 1.2.3 2026-11-03"
rm -f "$r/changes/CW-20261016-0399-x.md"
expect_fail "bad version" "VERSION must look like" cl "$r" assemble v1 2026-11-03

# assemble refuses on a malformed fragment and changes nothing
r="$(new_repo)"
frag "$r" CW-20261016-0400-bad.md 'no bullet'
before="$(cat "$r/CHANGELOG.md")"
expect_fail "assemble refuses malformed" "malformed" cl "$r" assemble 2.0.0 2026-11-01
ok; [[ "$(cat "$r/CHANGELOG.md")" == "$before" && -e "$r/changes/CW-20261016-0400-bad.md" ]] || fail "failed assemble modified files"

# --- pending + release.sh guard --------------------------------------------------
r="$(new_repo)"
expect_ok "pending is quiet with no fragments" cl "$r" pending
frag "$r" CW-20261016-0500-x.md '- waiting (CW-20261016-0500)'
expect_fail "pending lists fragments" "CW-20261016-0500-x.md" cl "$r" pending
expect_fail "release.sh refuses with unassembled fragments" "unassembled fragments" env VERSION=0.0.0 CHANGES_DIR="$r/changes" "$RELEASE"

if ((fails > 0)); then
  echo "changelog_test: $fails of $n checks failed" >&2
  exit 1
fi
echo "changelog_test: $n checks passed"
