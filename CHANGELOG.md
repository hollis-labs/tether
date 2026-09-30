# Changelog

All notable changes to Tether are recorded here.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project uses [Semantic Versioning](https://semver.org/spec/v2.0.0.html). Pre-1.0: minor bumps for additive surface, patch bumps for fixes — breaking changes can land in any minor.

This file was backfilled from the git history and is a good-faith summary, not an exhaustive one; `git log` is the complete record. Entries are grouped by tagged release. Consumers should watch this file for new HTTP routes, MCP tools, CLI commands and catalog/configuration changes.

## [Unreleased]

Changes on `main` since v0.6.0.

### Added

- **Messaging surfaces.** Scoped role/slot binding across CLI, MCP, HTTP and the Go client; a durable participant registry with canonical sessions and leased runtime bindings; durable hosted-session handoff with route fencing; delivery trace, operator repair and privacy-safe retention; and a bounded, server-only inbound A2A relay adapter.
- **MCP adapter: streamable-HTTP transport**, so an upstream restart no longer severs connected agents. Upstream `sse`/`http` connections and supervision now use the shared `go-mcp` client and supervisor packages.
- **Registry: derived/authored field split on project entries**, callback-backed derivation, an onboarding/lookup surface, flat authored `props`, and minter/owner provenance with symmetric redaction on write routes.
- **Launch profiles.** `config.Agent` is now `LaunchProfile`, with compositional launch resolution and a Global > Project > User settings cascade for onboarding configuration.
- **Runtimes.** An `opencode` runtime on the structured go-providers adapter (with resume verification and a skills compiler) and an Antigravity (`agy`) runtime with detect/seed/doctor support and provider events.
- **MCP proxy:** typed identifier extraction from proxied calls into session refs, and bounded workstream provenance on forwarded calls.

### Changed

- `POST /sessions/{id}/input` and `/turn` answer a lost provider resume id with the new `provider_session_lost` error code (still HTTP 409) instead of `conflict`, and the MCP `mux_session_send_input`/`mux_session_send_turn` tools report the same code. Callers that branched on `conflict` for this case should match the new code.
- Dependency refreshes: `agentkit` v0.8.0, `go-providers` v0.29.0, `go-messaging` v0.5.2.
- Planning history and design-dead docs were archived out of the repository tree.

### Fixed

- Catalog reload failures are redacted in MCP output, and launch catalog reads refresh correctly.
- Six registry re-onboarding and group-props bugs found in review.
- The messaging pull-only fencing gap, a binding-lease TOCTOU race, and a late-Nack-reverses-Consume race.

## [0.6.0] - 2026-09-11

### Added

- Group messaging, the federation directory (registry) and authority-routing peer configuration for messaging.
- An AI gateway runtime with streaming chat events over MCP, Gemini/embeddings/multimodal shorthands, an Ollama-compatible fallback, and OpenAI key setup through the keychain.
- OpenTelemetry propagation and AI spans; adoption of `agentkit` v0.2.0 for the runtime.
- Curated MCP proxy discovery, a notify wake path for messages, and Task-bundle planting for Codex worktrees.
- Sysop UI served at the site root with Operations as the home view.

### Changed

- Storage paths fall back to `go-apppaths`; release-readiness hardening of install and Sysop UI build paths.

## [0.3.0] - 2026-05-14

### Added

- Three-layer catalog discovery with schema extensions, a skills package with Claude and Codex compilers, and `mux agents create|edit|show`.
- A caller-provided launch surface and session-mutating MCP tools routed through the daemon.
- An ACP server (`mux acp`) bridging to the session service.

### Fixed

- Boot-command namespace collision; `acpsvc` infinite loop on attach error.

## [0.2.1] - 2026-05-09

### Fixed

- Patch release over v0.2.0; see `git log v0.2.0..v0.2.1` for detail.

## [0.2.0] - 2026-04-27

### Added

- Codex CLI support (boot, launch); ephemeral temp-dir model for Claude sessions; selective flat MCP proxy with a `--servers` filter (`--broker` deprecated).
- Message routing contract and durable messaging store with `/messages/*` HTTP routes; event durability and an observation query surface; a provider compliance suite and capability matrix.
- Sandbox profile registry integrated with the catalog; checkpoint/resume with logical-agent resume.
- `mux_discover` returns `totalMatches` so callers can detect truncation.

### Fixed

- MCP event forwarder and proxy-polling bugs; `mux_message_inbox` kind filter description aligned with the valid enum.

## [0.1.0] - 2026-04-21

### Added

- Initial release: the `muxd` daemon with session lifecycle, PTY/process management, an attach broker with ring-buffer fan-out and `since_seq` resume, session lifecycle events, an API/stub runtime, launch resolution with boot-prompt composition, the `mux` CLI, and golangci-lint-gated quality checks.

[Unreleased]: https://github.com/hollis-labs/tether/compare/v0.6.0...HEAD
[0.6.0]: https://github.com/hollis-labs/tether/compare/v0.3.0...v0.6.0
[0.3.0]: https://github.com/hollis-labs/tether/compare/v0.2.1...v0.3.0
[0.2.1]: https://github.com/hollis-labs/tether/compare/v0.2.0...v0.2.1
[0.2.0]: https://github.com/hollis-labs/tether/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/hollis-labs/tether/releases/tag/v0.1.0
