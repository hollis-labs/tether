# Release Readiness + Hardening Follow-up

Date: 2026-05-24

## Current state

Tether is in good shape for the build-in-public phase. The code and backend
surface have been through a focused release-readiness and hardening pass, and
the remaining work is mostly docs/cruft cleanup and frontend polish rather
than runtime safety or architectural instability.

## What landed

- MIT license at the repo root, with trademark guidance separated into
  `TRADEMARK.md`.
- Public-facing docs and examples updated toward Tether naming and
  `~/.tether/` state paths.
- Root release flow now includes Sysop build/install targets.
- Sysop nested-module build issues were fixed so the GUI can ship with Tether.
- Sysop frontend now route-splits instead of shipping a single eager app
  bundle. This came from the parallel `sysop-ui` work and appears integrated
  cleanly here.
- Sysop catalog writes now use atomic temp-file-plus-rename semantics and
  unique backup names.
- Sysop mutation endpoints now use stricter JSON decoding for malformed,
  trailing, or unknown fields.
- MCP catalog refresh now degrades gracefully when one or more upstream MCP
  servers fail, instead of failing wholesale on the first error.
- Daemon cleanup coverage now explicitly verifies artifact cleanup even when
  shutdown returns an error.

## Verification status

These checks passed during the release-readiness / hardening work:

- `go test -race ./...`
- `go test -race ./internal/app ./internal/api ./internal/daemon ./internal/mcpadapter ./internal/registry ./internal/store ./internal/launch`
- `go test ./internal/mcpadapter ./internal/daemon`
- `cd apps/sysop && go test ./cmd/tether_sysop ./...`
- `cd apps/sysop && go build ./cmd/tether_sysop`
- `cd apps/sysop/frontend && npm run typecheck`
- `cd apps/sysop/frontend && npm run build`
- `make release-build`

No known backend race-condition blocker or release-blocking runtime correctness
issue surfaced in this pass.

## Main remaining work

The remaining work is important, but it is mostly cleanup/polish:

- do another public-docs / ADR / examples sweep before release
- decide what internal/agentic material stays public vs. gets archived or
  relocated
- do one or two more Sysop frontend polish passes after the `sysop-ui`
  architecture work settles
- review bundle output and any remaining dependency warnings, but treat that as
  optimization unless a concrete user-facing problem appears

## Suggested next session

Start with a release-surface sweep, not core backend work:

1. Review root README, public docs, examples, and ADR metadata for any stale
   naming, path, or pre-release caveats.
2. Review `.nanite/`, `planning/`, and other internal-oriented files for what
   should remain in the public repo during build-in-public.
3. Run the release verification set again after any cleanup edits.

## Notes

- The `sysop-ui` follow-up is a portfolio concern, not just a Tether concern.
  The main improvement is in place here already: route-level lazy loading.
- Sysop is intended to ship with Tether and should remain part of the release
  path.
