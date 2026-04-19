# ADR 0000: Record Architecture Decisions

**Status:** Accepted — 2026-04-19
**Context:** Sprint v002-07 (Repo Quality), task T-v002-s07-03
**Deciders:** agent-mux v0.0.2 execution session

## Context

During v0.0.2 development, several consequential decisions (migration
framework, daemon transport, env composition, attach broker design,
event-bus drop policy, provider contract shape, local API shape,
logical-agents seeding, FK-enforcement deferral, …) shaped the codebase
in ways that are not self-evident from reading the code. Future
contributors who try to reverse-engineer rationale from commit messages
will spend time reconstructing decisions that were already made
deliberately.

## Decision

Record architecture-shifting decisions as ADRs in `docs/adr/`, one file
per decision, using a lightweight MADR-style format:

- **Filename:** `NNNN-short-kebab-title.md`, zero-padded to 4 digits.
- **Numbering:** sequential as ADRs are written. Do not reserve numbers
  ahead of time. The ledger reflects what has been decided, not a
  forecast.
- **Structure:**
  - Title line: `# ADR NNNN: <Title>`
  - Header block: `Status` / `Context` (sprint or epic) / `Deciders`
  - Sections: `## Context`, `## Decision`, `## Consequences`. Optionally
    `## Alternatives Considered` or `## Follow-ups`.
- **Status values:** `Proposed`, `Accepted`, `Deferred`, `Superseded by
  NNNN`, `Deprecated`. A `Deferred` ADR must name the trigger for
  reopening.

## What qualifies as an ADR

- Cross-cutting decisions with consequences beyond a single file: schema
  choices, transport protocols, interface shapes, lifecycle ownership,
  environment-composition semantics, drop/queue policies, deliberate
  deferrals.
- Decisions that were reversed at least once during review, or that
  would surprise a future contributor coming to the code cold.

## What does not qualify

- Style choices (file naming, test layout, import ordering).
- Bug fixes that do not change architecture.
- Routine dependency updates.
- Configuration tuning that a follow-up could undo without code churn.

## Consequences

- ADRs decay less than boot prompts or session logs. They persist across
  sessions, branches, and contributors.
- The indirection cost is ~1 ADR per sprint — cheap.
- If a decision turns out to be wrong, supersede the ADR with a new one
  and mark the old as `Superseded by NNNN`. Do not edit accepted ADRs in
  place except to correct factual errors.
