# ADR 0042: Group Messaging — Registry Kind + Mailbox-Pull Delivery

**Status:** Accepted — 2026-05-21
**Context:** Sprint v060-05 (Group Messaging) of the v0.6 Federation Directory epic. Coordination thread `msg://agent/agent-mux/tether-sprint-5-implementer` ↔ `msg://agent/agent-mux/agridd-keeper` for the pre-flight URN-structure escalation (`msg://019e4b9f-7a36-77ac-940d-d413e609a240` → response `msg://019e4ba2-c14b-7c2d-91aa-66a23a78a461`).
**Extends:** ADR-0023 (Message Routing Contract), ADR-0040 (Messaging Federation Peer Routing), ADR-0041 (Federation Directory Service).

## Context

Multi-agent coordination today fragments quickly. Agents send bilateral messages, threads scatter across mailboxes, and there is no first-class room concept for "this agent + that agent + me discussing X." Real instances of the problem:

- Cross-substrate design rounds pinged three substrates bilaterally; a single room would have collapsed the conversation.
- Incident bridges need cerberus + on-call + service-owner agents in one place when something breaks.
- Project coordination across multiple agents on the same sprint has no central thread.

Three approaches were considered:

1. **Hierarchical URN topics** (`msg://group/agent-mux/<id>/<subtopic>`). Rejected — hierarchical URNs couple identity to organization; renames break refs; thread_id already groups sub-conversations.
2. **Fan-out CC** (one message → N rows, one per recipient member). Rejected — storage cost scales as message-count × member-count; mark-read ambiguity (per-row vs per-thread); thread fragmentation between agents reading at different rates.
3. **Mailbox-pull with a group kind + sibling membership table** — the decision below.

## Decision

Adopt option 3. Groups are a federation-directory kind with a dedicated URN-path variant, a sibling membership table, and a per-group monotonic sequence counter. The daemon parses `@` mentions; `!` and `:` are reserved-namespace for agent-side interpretation.

### URN scheme (D1, D2, D3)

Groups address as `msg://group/<authority>/<grp_id>` (3-segment, e.g. `msg://group/agent-mux/grp_x9k2p4`). The structure preserves ADR-0023 §1's canonical `msg://<kind>/<authority>/<id>` shape — the only change from the agent path is the `kind` segment. The routing layer dispatches on the kind segment (`agent` vs `group`); ADR-0040's authority-based federation router continues to dispatch on the authority segment for both kinds.

**ADR-0023 §1 closed-AddressKind-enum extension.** ADR-0023 lists `kind` as a closed enum `{agent, user, service, session, workflow}`. This ADR extends the enum to include `group` for Tether's local URN-parsing (`internal/registry.ParseRegistryURN`). go-messaging v0.2.1's `AddressKind` enum is unchanged here — see Consequences. Future kinds may add their own URN paths (`msg://service/<authority>/<svc_id>`?), but the 3-segment structural invariant is fixed.

**ID minting (D2).** Stripe-style: `grp_<10alnum>` prefix, `crypto/rand`-sourced with rejection sampling against the 36-char alphabet. Callers MUST NOT supply IDs. Server mints at Register time. `MintGroupURN(authority)` is exposed in `internal/registry`.

### Mailbox-pull, not fan-out CC (D4)

A group message lands in ONE row in the existing `messages` table with `group_urn` + `group_seq` columns. Reads are non-destructive; each member tracks their own `last_read_seq` on `group_members`. Storage scales with message count, not message-count × member-count.

### Membership in a sibling table (D5)

`group_members(grp_urn, member_urn, role, joined_at, last_read_seq)`. Not stored in `registry_links` with `kind=member`, because membership carries per-member state (role + read cursor). `registry_links` remains for shape-fluid relationships like `team_lead`.

### Symbol vocabulary (D6)

Three symbols compose the group-chat language. They are separated by who interprets them:

- **`@` — mention / notify. Daemon-parsed.** Format: `@<urn>` (full) or `@<display_name>` (short-form, resolved via registry Lookup). The daemon scans every `SendToGroup` body for the pattern, resolves each match, and emits a `notice` envelope to the mentioned URN's *personal* inbox (NOT the group inbox). The notice payload references the group message ID + seq so the mentioned agent can navigate back to the thread at the right offset. Escaped form: `\@<text>` — sent literally.

- **`!` — action / command. Agent-side.** Format: `!<command> [args...]`. The daemon does NOT parse or interpret `!` patterns. The transport delivers the message verbatim; the consuming agent's command handler decides what (if anything) `!deploy --branch=main` means. Reserved namespace: agents SHOULD NOT use `!` for anything other than command invocation. Escaped form: `\!<text>`.

- **`:` — directive. Agent-side, dispatches to the directives package.** Format: `:<directive> <prompt>`. The daemon does NOT parse. The consuming agent recognizes the `:` prefix and routes the directive through whatever directives-package implementation it has installed locally — an existing concept in chrispian's ecosystem, not built or governed by this sprint. Escaped form: `\:<text>`.

The three symbols have semantically distinct intents: `@` = "notify someone", `!` = "do something", `:` = "invoke a directive". Conflating them would make linting/routing/escaping ambiguous.

### Mention resolution + dispatch (D7, D11, D12)

Short-forms resolve via `Storage.FindByDisplayName` against the active-status set. Ambiguous short-forms (≥2 matches) return `*ErrAmbiguousMention` with the candidate URN list; SendToGroup aborts with HTTP 400 — the row is NOT written. Unknown full URNs are kept as mentions with empty `ResolvedURN`; the group message commits, the notice is logged-and-skipped.

Mentions become `notice` envelopes (D11 — no new envelope kind invented). Notice payload:
```json
{
  "subject": "Mention in <group display_name>",
  "group": "<grp_urn>",
  "message_id": "<id>",
  "group_seq": <seq>,
  "mentioned_by": "<from_urn>",
  "thread_id": "<thread_id_or_null>",
  "snippet": "<first 240 chars of body>"
}
```

The notice is a *pointer*, not a copy of the group message (D12 — preserves D4 storage discipline). To read context, the mentioned agent calls `ListGroupMessages(grp_urn, thread_id=<msg's thread>)`.

**Two-phase parser interface.** The parser runs twice around the SendToGroup commit:
- `Parse` runs BEFORE commit. Returning `*ErrAmbiguousMention` (or any error) aborts the send — the row is NOT written.
- `Dispatch` runs AFTER commit, fire-and-forget per D11/D12. Errors logged via slog; failures do not roll back the group message.

This shape replaced T-04's single-method `ParseAndDispatch` design — the ambiguous-mention pre-commit failure semantic required pre-commit validation.

### Post policy + roles (D8)

Open-post for members, moderator-only invite/kick/archive. Any member can post; only owners or moderators can invite, remove, or archive. Non-members posting → `ErrForbidden` (HTTP 403). Owner = creator at Register time; owner can `SetMemberRole(member, 'moderator')` to delegate.

Promotion to **owner** is owner-only (transfers ownership). Moderators can promote members to moderator but not to owner. The owner cannot be removed via `RemoveMember`; the owner must `LeaveGroup` after transferring ownership (or archive the group).

### Archive, not delete (D9)

`ArchiveGroup` sets `status='archived'`. The group becomes read-only — members can still `ListGroupMessages`, but `SendToGroup` returns `ErrGroupArchived` (HTTP 423 Locked). Hard-delete deferred.

### Sequence number per group (D10)

Each group has a monotonic `group_seq` counter; every `SendToGroup` assigns the next value via `SELECT MAX(group_seq)+1 FROM messages WHERE group_urn=?` inside a transaction. Race-safe via the `MaxOpenConns=1` connection-pool serialization (the same invariant the rest of the storage layer relies on). Read cursors and `since_seq` parameters are based on this counter; timestamp-based ordering would introduce per-member ambiguity.

### LeaveGroup constraint

If the leaver is the owner and no other owner/moderator exists, `LeaveGroup` refuses with `ErrForbidden` and a hint to `SetMemberRole(other, 'owner')` first or `ArchiveGroup`. Prevents groups from being orphaned to a member-only steady state with no admin path.

## Consequences

- **Cross-substrate group routability via ADR-0040 works because the 3-segment URN preserves an Authority segment.** Group rooms spanning cerberus + agridd + tether are structurally possible — the original sprint D3 (2-segment) form was ruled out in pre-flight precisely to keep this property.

- **go-messaging v0.2.1's closed `AddressKind` enum doesn't include `group`.** Tether-internal group ops use `internal/registry.ParseRegistryURN` for URN parsing; group URNs never traverse the `/messages/*` boundary that calls `go-messaging.ParseURN`. The mention parser emits notices addressed to `msg://agent/...` URNs (the recipient is an agent, not a group), so the notice-emission path is unaffected. **A future go-messaging bump (≥ v0.3 with `KindGroup`) unblocks two improvements**: (a) group URNs could appear in any go-messaging envelope field (currently we route around them); (b) cross-substrate group routing via the federation Router becomes fully end-to-end without authority-aware glue.

- **`to_urn` on group-message rows is set to the group URN itself**, for NOT-NULL column compliance with the shared `messages` table. The existing `/messages/*` HTTP path rejects group URNs at the `go-messaging.ParseURN` boundary, so personal-inbox queries cannot accidentally surface group rows. Group reads go through `ListGroupMessages` (queries by `group_urn`, not `to_urn`). Clean separation by route.

- **MAX(group_seq)+1 serializes through the connection pool.** Fine for v1's expected throughput. If the connection pool widens, switch to a dedicated `groups_next_seq(grp_urn, next_seq)` companion table or to `SELECT ... FOR UPDATE` semantics. The migration is forward-compatible; the existing `idx_messages_group_seq` covers both implementations.

- **Display-name ambiguity is now a real failure mode.** v060-01 allowed two agents to share `display_name` (uniqueness is on URN only). v060-05's short-form mention resolution makes that ambiguity user-visible — ambiguous mentions abort the send with the candidate list. A follow-up sprint may add per-substrate or global `display_name` uniqueness if the ambiguity rate is annoying in practice.

- **Mention parser caps at 32 mentions per message** (review-note hardening). Additional matches in a single body are silently dropped — guards against malicious-blowup payloads. Tunable via `messaging.MaxMentions`.

- **System sender URN for mention notices** is `msg://agent/agent-mux/agt_mxsysmnt00` — synthetic, reuses the agent kind so go-messaging.ParseURN accepts it. Does not need to be registered in the directory. Future iterations may swap to a registered system-agent row.

- **FK enforcement deferred per ADR-0008.** The `0016_group_messaging.sql` migration declares `REFERENCES registry_entries(urn) ON DELETE CASCADE` on `group_members` and `ON DELETE SET NULL` on `messages.group_urn` as documentation; `PRAGMA foreign_keys` stays off in v1. The v060-02 T-08 reopen will audit and flip enforcement globally — at which point the cascade semantics become live.

- **Member privacy:** v1 group member lists are visible to all members. No private-membership model. Adequate for trusted multi-agent collaboration; would need rethinking if groups span less-trusted boundaries.

- **No event emission on group writes** — same scope-fence as v060-01 (the registry doesn't yet feed the event bus). Consumer cache invalidation happens via re-pull. Event emission lands when the broader registry-events sprint does.

## Alternatives Considered

- **(a) Hierarchical URN topics.** Rejected — coupling identity to organization breaks refs on rename; thread_id already groups sub-conversations.
- **(b) Fan-out CC** (one row per recipient member). Rejected — storage cost scales as message-count × member-count; mark-read ambiguity; thread fragmentation between members reading at different rates.
- **(c) Membership in `registry_links` with `kind=member`.** Rejected — membership carries per-member state (role + last_read_seq) that doesn't fit the (subject, kind, target) shape of `registry_links`. A sibling table is the natural shape.
- **(d) Merge `!` and `:` into one symbol.** Rejected — they have distinct intents ("do something" vs "invoke a directive"); merging would conflate routing/linting/escaping.
- **(e) 2-segment URN `msg://group/<grp_id>`** (the original sprint D3 form). Rejected in pre-flight after ADR-conflict audit caught violations of ADR-0023 §1 (canonical 3-segment structure + closed kind enum), ADR-0040 (authority-based routing), and ADR-0041 D3 ("URN scheme everywhere in v1"). 3-segment form preserves all four invariants and unlocks federation routability as a bonus. See coordination msg `019e4b9f` → `019e4ba2`.

## References

- Sprint file: `planning/docs/sprints/v060-05-group-messaging.md`
- Epic file: `planning/docs/epics/v0.6-federation-directory.md`
- ADR-0008: SQLite Foreign-Key Enforcement — Deferred Until Post-Launch
- ADR-0023: Message Routing Contract (extended here for the `group` kind enum entry)
- ADR-0040: Messaging Federation — Authority-Routing Peer Configuration
- ADR-0041: Federation Directory Service (Mux Registry) — predecessor; group URN scheme extends D3
- Symbol vocabulary doc: `docs/groups/symbols.md`
- API docs: `docs/api/README.md` §Groups
- Implementation packages: `internal/registry` (Service.SendToGroup et al), `internal/messaging` (Parser)
- Coordination thread for the URN-structure pre-flight escalation: msg `019e4b9f-7a36-77ac-940d-d413e609a240` → `019e4ba2-c14b-7c2d-91aa-66a23a78a461`
