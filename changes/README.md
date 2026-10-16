# Changelog fragments

Parallel PRs used to conflict on `CHANGELOG.md` while main moved during the CI
gate. Instead, **each PR that deserves a changelog entry adds a fragment here and
does not edit `CHANGELOG.md`.** Nothing else touches your fragment, so fragments
never conflict.

## Adding one

Create `changes/<TASK-ID>-<slug>.md`, for example
`changes/CW-20261016-0159-changelog-fragments.md`. A PR may add several (one per
task). The body is exactly the markdown bullet(s) you would have put under
`## [Unreleased]`:

```markdown
- Add the thing (CW-20261016-0159): what changed and what a user or operator must
  know, in the same prose style as the existing entries. Continuation lines are
  indented two spaces.
- A second bullet in the same fragment, naming its task. (CW-20261016-0160)
```

Rules (`scripts/changelog.sh check`, run by `make check` and CI):

- file name `<TASK-ID>-<slug>.md` (task id like `CW-20261016-0159`, lowercase slug);
- bullets only: every line starts with `- `, or with two spaces as a continuation;
  no markdown headings;
- every bullet names a task id (`CW-YYYYMMDD-NNNN`) somewhere in its text.

CI (`scripts/changelog.sh check-pr`) also objects when a PR edits `CHANGELOG.md`
directly: "add `changes/<TASK>-x.md` instead". It is **advisory (a warning) for
now** and becomes failing in a one-line follow-up
(`CHANGELOG_FRAGMENTS_ENFORCE` in `.github/workflows/ci.yml`). A malformed
fragment fails regardless. Branches named `release/*` may edit `CHANGELOG.md`.

## Releasing (release owner)

In its own small reviewed PR on a `release/*` branch, titled
`release: changelog for vX.Y.Z`:

```bash
make changelog-assemble VERSION=X.Y.Z            # DATE=YYYY-MM-DD optional, default today (UTC)
```

This writes `## [X.Y.Z] - <date>` under `## [Unreleased]` with the fragments in
filename order followed by the bullets that were already under `[Unreleased]`
(the pre-fragment baseline), leaves `[Unreleased]` empty, updates the
`[Unreleased]`/`[X.Y.Z]` compare links at the bottom, and `git rm`s the fragments
(this README stays). Merge that PR, then tag. `scripts/release.sh` refuses to
run while fragments are waiting, so nobody tags without assembling.
