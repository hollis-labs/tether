# Launch Guides

This folder collects operator-facing docs for catalog-backed agent launches.
Use it when adding a launch profile, checking which provider/runtime mode to
choose, or re-running live provider smoke tests.

## Guides

- [Setup guide](setup-guide.md) - launch profile setup for Claude, Codex, and
  Opencode, with working catalog examples and limitations.
- [Boot prompt generation](bootgen.md) - boot profiles, generated boot prompts,
  and the supported `boot` / `boot-exec` paths.
- [Smoke results](smoke-results.md) - the latest live smoke run against local
  Claude, Codex, and Opencode binaries.

## Related References

- [Catalog launch and boot profiles](../catalog-launch-profiles.md)
- [Provider runtime sessions](../provider-runtime-sessions.md)
- [Provider launch smoke matrix](../provider-launch-smoke-matrix.md)
- [ADR 0037: Provider runtime kind matrix](../adr/0037-provider-runtime-kind-matrix.md)
- [ADR 0039: boot-exec Claude-only scope](../adr/0039-boot-exec-claude-only-scope.md)
