- Changelog fragments (CW-20261016-0159): PRs add `changes/<TASK-ID>-<slug>.md` instead of
  editing `CHANGELOG.md`, so parallel PRs stop conflicting on it. `scripts/changelog.sh`
  validates fragments (`make changelog-check`), `check-pr` flags direct `CHANGELOG.md` edits
  (advisory in CI for now; `release/*` branches exempt), and `make changelog-assemble
  VERSION=X.Y.Z` turns the fragments into a dated release section. `scripts/release.sh`
  refuses to run while fragments are unassembled. See `changes/README.md`.
