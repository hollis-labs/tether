# Sprint v060-05 — Group Messaging

**Epic:** [v0.6 — Federation Directory](../epics/v0.6-federation-directory.md)
**Scope:** `group` as a new federation-directory kind + group-mailbox messaging semantics (per-member read cursor, mention notifications, archive) + the `@` / `!` / `:` symbol vocabulary. Enables multi-agent coordination rooms without the fan-out-CC ergonomic disaster.
**Target duration:** ~8 business days.
**Transport/shape inheritance:** matches v060-01 / v060-02 conventions — UDS default, typed error envelope, registry-CRUD shape for group identity, messaging-store extension for group delivery.

**Consumers waiting on this:**
- Multi-agent design coordination (the FU-31 round had three substrates pinging each other bilaterally — a group would have collapsed that into one room).
- Incident bridges (cerberus + on-call + affected service-owner agents in one room when something breaks).
- Project coordination rooms (multiple agents working on a sprint, central thread instead of cross-mailbox archaeology).

**Coordination refs:** No cross-substrate coordination needed before this sprint — group kind is internal to the federation directory and reuses the registry surface from v060-01 / v060-04. Once it ships, send a `notice` to agridd-keeper + cerberus-registry-design announcing the new kind.

**Dependencies:** v060-01 (Registry Foundation) must ship first — group is a registry kind and uses the existing CRUD surface. v060-02 and v060-04 are NOT prerequisites; v060-05 can run in parallel with either.

---

## Exit criteria

- [ ] Migration `0016_group_messaging.sql` lands: `group_members` sibling table; `messages.group_urn` column + index; URN-scheme variant config for the `group` kind.
- [ ] `group` registered as a registry kind. `Register({kind: 'group', ...})` mints `grp_<10alnum>` and returns URN `msg://group/<authority>/<grp_id>` (e.g. `msg://group/agent-mux/grp_x9k2p4` — distinct kind-path from agent's `msg://agent/agent-mux/<id>`, same 3-segment structure).
- [ ] Six membership ops live: `AddMember(grp_urn, member_urn, role)`, `RemoveMember(grp_urn, member_urn)`, `ListMembers(grp_urn)`, `SetMemberRole(grp_urn, member_urn, role)`, `ArchiveGroup(grp_urn)`, `LeaveGroup(grp_urn, member_urn)`.
- [ ] Four messaging ops live: `SendToGroup(grp_urn, from_urn, kind, body, thread_id?)`, `ListGroupMessages(grp_urn, since_seq?, limit, thread_id?)`, `MarkRead(grp_urn, member_urn, up_to_seq)`, `GetMyMentions(member_urn, since_ts?)`.
- [ ] Mention parser runs server-side on `SendToGroup`: extracts `@<urn>` and `@<display_name>` patterns, resolves short-form via registry Lookup, emits a `notice` envelope to each mentioned URN's *personal* inbox with `{group: <grp_urn>, message_id, mentioned_by, snippet}`.
- [ ] Per-member read cursor (`group_members.last_read_seq`) bumps on `MarkRead` and on destructive group reads. Non-destructive `ListGroupMessages` does NOT bump.
- [ ] All ops exposed at parity across HTTP (`/groups[/{urn}]`, `/groups/{urn}/messages`, `/groups/{urn}/members`), MCP (`tether_group_*`), CLI (`mux group create|invite|kick|post|read|archive|mentions`).
- [ ] ADR `0042-group-messaging.md` captures the mailbox-not-CC choice, the symbol vocabulary (especially the directives-package distinction), the moderator model, and the URN-scheme variant. (Sprint doc originally named ADR 0015, which is taken by `0015-checkpoint-payload-schema.md`; pre-flight v060-05 picked next-free 0042 — 0016 is a pre-existing gap in ADR numbering, convention is sequential.)
- [ ] `make check` green.
- [ ] Cross-substrate `notice` sent to agridd-keeper + cerberus-registry-design announcing the new kind.

---

## Decisions locked

- **D1 Flat URN, no hierarchical topics.** Groups are `msg://group/<authority>/<grp_id>` — flat. Sub-conversations use the existing envelope `thread_id` field. Rationale: hierarchical URNs couple identity to organization (rename → broken refs); threads already exist. If a thread grows into its own thing, promote it to a new group.
- **D2 Group is a registry kind.** `kind='group'`, ID prefix `grp_<10alnum>`. Inherits URN, display_name, description, role (= group category like "design-room"|"incident-bridge"|"project-coord"), capabilities[] (= topic tags for discovery), status, avatar, links from the registry schema. The only group-specific addition is the membership table.
- **D3 URN-scheme variant.** Groups address as `msg://group/<authority>/<grp_id>` (e.g. `msg://group/agent-mux/grp_x9k2p4`), NOT `msg://agent/agent-mux/<grp_id>`. The routing layer dispatches on the **kind segment** (`agent` vs `group`, segment 1 after `msg://`); the authority segment continues to drive ADR-0040 federation routing for both kinds. Three-segment structure preserves ADR-0023 §1's canonical URN shape and keeps group messages federation-routable cross-substrate. ADR-0023 §1's closed kind enum is extended to include `group` — the extension is captured inside ADR 0042 (the new group ADR) per agridd-keeper response to msg `019e4b9f-7a36-77ac-940d-d413e609a240`. Future kinds may add their own URN paths (`msg://service/<authority>/<svc_id>`?), or stay under `msg://agent/agent-mux/` if they're just identities with no special delivery. Decision is per-kind at Register time; structural shape is fixed at 3 segments.
- **D4 Mailbox-pull, not fan-out CC.** A group message lands in ONE row, not N rows per member. Reads are non-destructive; each member tracks their own `last_read_seq`. Storage scales with message count, not message-count × member-count.
- **D5 Membership in sibling table.** `group_members(grp_urn, member_urn, role, joined_at, last_read_seq)`. Not `registry_links` with `kind=member`, because membership carries per-member state (role + read cursor). `registry_links` stays for shape-fluid relationships like `team_lead`.
- **D6 Symbol vocabulary — `@` is daemon, `!` and `:` are agent-side.** See dedicated section below.
- **D7 Mention short-form supported, registry-resolved.** Both `@msg://agent/agent-mux/agt_a8k3xn92pq` (full URN) and `@torque-operator` (display_name short-form) work. Daemon resolves short-form via registry Lookup at parse time; ambiguous short-form (two members with same display_name) returns a validation error to the sender, asking them to use the full URN.
- **D8 Default post policy: open-post for members, moderator-only invite.** Any member can post. Only the owner or members with `role='moderator'` can invite/kick/archive. Non-members posting → `403 not_member`. Owner = creator at CreateGroup time; owner can `SetMemberRole(member, 'moderator')` to delegate.
- **D9 Archive, not delete.** `ArchiveGroup` sets `status='archived'`; group becomes read-only (members can still read, no new messages accepted). Consistent with registry soft-delete pattern. Hard-delete deferred.
- **D10 Sequence number per group.** Each group has a monotonic `next_seq` counter; every `SendToGroup` assigns the next value. Read cursors and `since_seq` parameters are based on this. Avoids per-member ordering ambiguity that timestamps would introduce.
- **D11 Mention notice envelope is plain `notice` kind.** No new envelope kind invented. The notice's payload structure is `{group: <grp_urn>, message_id, mentioned_by, snippet, seq}` so consumers can navigate back to the group thread at the right offset.
- **D12 Mentions create notices, not deliveries.** A mention does NOT cause the group message to be copied into the mentioned agent's personal inbox. The notice is a pointer ("you were mentioned at <group>/<msg_id>"); to read context, the agent calls `ListGroupMessages(grp_urn, thread_id=<msg's thread>)`. Preserves D4 storage discipline.

### D6 detail — symbol vocabulary

Three symbols compose the group-chat language. They are deliberately separated by who interprets them:

**`@` — mention / notify. Daemon-parsed.**
- Format: `@<urn>` (full) or `@<display_name>` (short-form, resolved via registry).
- Daemon scans every `SendToGroup` body for the `@` pattern, resolves each match, emits a `notice` envelope to that URN's *personal* inbox (NOT the group inbox).
- The `notice` payload references the group message ID + seq so the mentioned agent can navigate back to the thread at the right offset.
- This is the only daemon-side parsing. It's a delivery primitive, not a semantic one.
- Escaped form: `\@<text>` — sent literally, no resolution attempted. Use for documentation, examples, etc.

**`!` — action / command. Agent-side.**
- Format: `!<command> [args...]`.
- The daemon does NOT parse or interpret `!` patterns. The transport delivers the message verbatim; the consuming agent's command handler decides what (if anything) `!deploy --branch=main` means.
- Reserved namespace: agents SHOULD NOT use `!` for anything other than command invocation, so future tooling can rely on the convention (linters, audit log filters, etc.).
- Escaped form: `\!<text>` — sent literally.

**`:` — directive. Agent-side, dispatches to the directives package.**
- Format: `:<directive> <prompt>`.
- The daemon does NOT parse. The consuming agent recognizes the `:` prefix and routes the directive through whatever directives-package implementation it has installed locally (existing concept in chrispian's ecosystem — not built or governed by this sprint).
- This sprint preserves the `:` namespace and documents the convention so directive authors can rely on it; the actual directive vocabulary, security model, and dispatch mechanics are owned by the directives package itself.
- Escaped form: `\:<text>` — sent literally.

**Why three symbols?** They have semantically distinct intents that don't overlap. Conflating them (e.g. forcing directives through `!`) would make linting/routing/escaping ambiguous. Three symbols keep each lane crisp:
- `@` = "notify someone"
- `!` = "do something"
- `:` = "invoke a directive"

**What this sprint does NOT do for `!` and `:`:**
- No daemon parsing.
- No registration of command/directive vocabularies.
- No security model for `!<command>` invocations or `:<directive>` payloads — that's per-agent concern.
- No discoverability layer ("what commands does agent X accept?") — could land later, separate sprint.

This sprint reserves the namespace and documents the convention. Agents opt in by handling whichever symbols they want.

---

## Tasks

### T-v060-05-01: Migration 0016 + group registry kind config + URN routing

**kind:** agent
**priority:** 1
**manual:** true
**tags:** [registry, groups, schema, migration, foundation]
**depends_on:** v060-01 ship

#### Fix direction

- New migration `internal/store/migrations/0016_group_messaging.sql`:
  ```sql
  CREATE TABLE group_members (
    grp_urn       TEXT NOT NULL REFERENCES registry_entries(urn) ON DELETE CASCADE,
    member_urn    TEXT NOT NULL REFERENCES registry_entries(urn) ON DELETE CASCADE,
    role          TEXT NOT NULL DEFAULT 'member' CHECK(role IN ('member','moderator','owner')),
    joined_at     DATETIME NOT NULL,
    last_read_seq INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (grp_urn, member_urn)
  );
  CREATE INDEX idx_group_members_by_member ON group_members (member_urn);

  ALTER TABLE messages ADD COLUMN group_urn TEXT REFERENCES registry_entries(urn) ON DELETE CASCADE;
  ALTER TABLE messages ADD COLUMN group_seq INTEGER;
  CREATE INDEX idx_messages_group_seq ON messages (group_urn, group_seq);
  ```
- Extend the messages-store sequence counter: per-group monotonic `group_seq` assigned at insert time. Use a `groups_next_seq(grp_urn, next_seq)` companion table OR derive on insert via `SELECT COALESCE(MAX(group_seq),0)+1 FROM messages WHERE group_urn=?` (the latter is simpler but needs a write-lock).
- Register `group` as a valid `kind` value in the `registry_entries.kind` CHECK constraint (currently 'agent'|'project'; extend to 'agent'|'project'|'group').
- ID minting: `MintGroupURN(authority)` returns `msg://group/<authority>/grp_<10alnum>` (e.g. `msg://group/agent-mux/grp_x9k2p4` for the default `agent-mux` authority). Note the URN-kind variant (`msg://group/<authority>/...` not `msg://agent/<authority>/...`) — same 3-segment shape, dispatch on kind segment.
- URN parser in the message router: dispatch on `urn[1]` (path segment after `msg://`) — `agent` vs `group` selects delivery semantics. Document the routing rule.

#### Acceptance criteria

- [ ] Migration applies cleanly on a DB with 0015 already applied (last on `main` at v060-05 start).
- [ ] `MintGroupURN(authority)` returns well-formed `msg://group/<authority>/grp_xxxxxxxxxx` (3-segment, ADR-0023-compliant).
- [ ] URN parser correctly distinguishes agent vs group URNs.
- [ ] FK cascade verified: deleting a group's registry_entries row removes its group_members rows and nulls out group_urn on referenced messages (or cascades the deletes — pick during impl).
- [ ] `make check` green.

#### Scope fences

- No service-layer logic in this task — pure schema + ID minting + URN parsing.

---

### T-v060-05-02: Group registry service (Create / List / Archive / Profile)

**kind:** agent
**priority:** 1
**manual:** true
**tags:** [registry, groups, service]
**depends_on:** T-v060-05-01

#### Fix direction

- Extend `Service.Register` to handle `kind='group'`:
  - Mint via `MintGroupURN()` (different URN path).
  - Caller-supplied profile fields: `display_name` (required), `description`, `role` (group category — free-form), `capabilities[]` (topic tags), `avatar`.
  - On successful Register: auto-add the caller's URN to `group_members` with `role='owner'`. Caller URN comes from message-send auth context (TBD — for v1 the CLI/MCP caller passes `creator_urn` explicitly; v060-03 token auth will replace this).
- `Service.ListGroupsForMember(member_urn) ([]Profile, error)` — joins through `group_members`.
- `Service.ArchiveGroup(grp_urn, by_urn)` — checks `by_urn` is owner or moderator; sets `status='archived'`; rejects subsequent `SendToGroup`.
- `Search` already handles filter on `kind` and `capabilities` — groups are discoverable for free.

#### Acceptance criteria

- [ ] Register with `kind='group'` mints a `grp_` URN and creates an owner membership row.
- [ ] Non-member calling `SendToGroup` rejected (covered in T-04).
- [ ] Search by `kind=group, capability=<topic>` returns matching groups.
- [ ] ArchiveGroup gates by role; non-owner/non-moderator → 403.

---

### T-v060-05-03: Membership service (AddMember / RemoveMember / ListMembers / SetMemberRole / LeaveGroup)

**kind:** agent
**priority:** 1
**manual:** true
**tags:** [registry, groups, membership]
**depends_on:** T-v060-05-02

#### Fix direction

- `AddMember(grp_urn, member_urn, by_urn, role='member')`: checks `by_urn` is owner|moderator; validates `member_urn` exists in registry; inserts row with `joined_at=NOW()`, `last_read_seq=0` (new members see only messages sent after they joined).
- `RemoveMember(grp_urn, member_urn, by_urn)`: owner/moderator only; cannot remove owner (owner must `LeaveGroup` after transferring ownership).
- `LeaveGroup(grp_urn, member_urn)`: self-remove; if leaver is owner and no other moderator exists, fails with `cannot_leave_without_owner_transfer` — caller must `SetMemberRole(<other_member>, 'owner')` first or `ArchiveGroup`.
- `SetMemberRole(grp_urn, member_urn, role, by_urn)`: owner only for promoting/demoting `owner` role; moderators can promote members to `moderator` but not owner.
- `ListMembers(grp_urn)`: returns `[{member_urn, role, joined_at, display_name (joined from registry)}]`.

#### Acceptance criteria

- [ ] Role enforcement tested for every op (member, moderator, owner, non-member).
- [ ] Owner-transfer semantics work end-to-end.
- [ ] `joined_at` correctly gates new members' visible history (they should NOT see messages from before they joined when reading with `since_seq=0`).

---

### T-v060-05-04: Group messaging service (SendToGroup / ListGroupMessages / MarkRead / GetMyMentions)

**kind:** agent
**priority:** 1
**manual:** true
**tags:** [messaging, groups, service]
**depends_on:** T-v060-05-03

#### Fix direction

- `SendToGroup(grp_urn, from_urn, kind, body, thread_id?, payload_json?)`:
  - Validate `from_urn` is a member of `grp_urn` (per D8 — open post for members).
  - Validate group `status='active'` (rejects archived).
  - Assign next `group_seq` for this group.
  - Insert row into `messages` with `group_urn`, `group_seq`, `thread_id` (nullable), and standard envelope fields.
  - Invoke mention parser (T-05) on the body — emits notices as side effect.
  - Return `{message_id, group_seq}`.
- `ListGroupMessages(grp_urn, member_urn, since_seq?, limit=100, thread_id?)`:
  - Validate `member_urn` is a member.
  - Default `since_seq` = caller's `last_read_seq`. `since_seq=0` returns full history visible to member (gated by `joined_at`).
  - Optional `thread_id` filters to one sub-conversation.
  - Returns ordered list of envelopes plus `next_seq` for pagination.
  - Does NOT bump the read cursor — that's `MarkRead`'s job.
- `MarkRead(grp_urn, member_urn, up_to_seq)`:
  - Sets `group_members.last_read_seq = max(last_read_seq, up_to_seq)`.
  - Idempotent / monotonic.
- `GetMyMentions(member_urn, since_ts?, limit=50)`:
  - Returns the mention `notice` envelopes from this member's personal inbox, filtered to those with `payload.group` set.
  - Convenience wrapper over `mux_message_list` — saves callers from constructing the filter.

#### Acceptance criteria

- [ ] Race-safe `group_seq` assignment under concurrent SendToGroup (no duplicate or skipped seq).
- [ ] Non-member SendToGroup → 403; archived group SendToGroup → 423 locked.
- [ ] `since_seq` pagination correct; `joined_at` gating works.
- [ ] MarkRead is monotonic (smaller `up_to_seq` doesn't lower the cursor).
- [ ] `make test-race` green.

---

### T-v060-05-05: Mention parser + notification fan-out (daemon-side @ handling)

**kind:** agent
**priority:** 1
**manual:** true
**tags:** [messaging, groups, mentions, parser]
**depends_on:** T-v060-05-04

#### Fix direction

- New `internal/messaging/mentions.go`.
- Parser scans body for `@<token>` patterns where `<token>` is either:
  - A full URN: `msg://agent/agent-mux/agt_<10>`, `msg://agent/agent-mux/prj_<10>`, etc. — accept any registered URN, not just agents.
  - A short-form: `[a-z0-9_-]+` — resolved via `registry.LookupByDisplayName(short_form)` to a URN.
- Escaped: `\@token` — literal, no resolution, no notice.
- Resolution failures (unknown URN or ambiguous short-form):
  - Unknown URN: send anyway, log a warn (don't fail the message — agent might intentionally reference something deferred).
  - Ambiguous short-form (two agents share `display_name`): REJECT the SendToGroup with `400 ambiguous_mention` and a hint listing the candidate URNs. Caller picks the full URN.
- For each successfully-resolved unique mention URN: emit one `mux_message_send` with:
  - `from`: a synthetic system URN `msg://system/group-mention-notifier` (or reuse `msg://agent/agent-mux/muxd-system` if a system URN already exists)
  - `to`: the mentioned URN
  - `kind`: `notice`
  - `payload_json`: `{"subject": "Mention in <group display_name>", "group": "<grp_urn>", "message_id": "<id>", "group_seq": <seq>, "mentioned_by": "<from_urn>", "thread_id": "<thread_id_or_null>", "snippet": "<first 240 chars of body>"}`
- Mention emission is fire-and-forget after the group message is committed. If notice emission fails (e.g. mentioned URN doesn't exist), log + continue — the group message itself was delivered successfully.

#### Acceptance criteria

- [ ] Parser correctly handles: single mention, multiple mentions, duplicate mentions (one notice per unique URN), escaped `\@`, mention inside code block (decide: parse or skip? Default: parse — agents who don't want it escape).
- [ ] Short-form resolution works via registry Lookup.
- [ ] Ambiguous short-form is a 400 with helpful payload.
- [ ] Notice envelopes are well-formed and consumable by `GetMyMentions`.
- [ ] Self-mention (member @-ing themselves) emits a notice — useful for "save for later" patterns.
- [ ] Bot-mention (mentioning the group's own URN) is a no-op silently (no notice).

#### Scope fences

- NO parsing of `!` or `:` symbols. Reserved for agent-side per D6.
- NO permission-based filtering (e.g. "this member can't be mentioned"). Open mention model v1.

---

### T-v060-05-06: HTTP + CLI + MCP surfaces

**kind:** agent
**priority:** 1
**manual:** true
**tags:** [groups, http, cli, mcp, surfaces]
**depends_on:** T-v060-05-04, T-v060-05-05

#### Fix direction

- HTTP (`internal/api/groups.go`):
  - `POST /groups` — create (delegates to Register).
  - `GET /groups/{urn}` — read (delegates to Lookup with kind=group enforcement).
  - `GET /groups?member=<urn>` — list groups a member belongs to.
  - `DELETE /groups/{urn}` — archive (soft).
  - `POST /groups/{urn}/members` — add member.
  - `DELETE /groups/{urn}/members/{member_urn}` — remove member.
  - `GET /groups/{urn}/members` — list members.
  - `PATCH /groups/{urn}/members/{member_urn}` — set role.
  - `POST /groups/{urn}/messages` — send (returns `{message_id, group_seq}`).
  - `GET /groups/{urn}/messages?since_seq=N&thread_id=...&limit=N` — list (non-destructive).
  - `POST /groups/{urn}/read` body `{up_to_seq: N}` — mark read.
  - `GET /mentions?since=<ts>` — own mentions across all groups.
- CLI (`cmd/mux/group.go`):
  - `mux group create --name <n> [--description <d>] [--category <c>] [--capability <cap>]...`
  - `mux group list [--mine]`
  - `mux group show <urn>`
  - `mux group invite <urn> <member_urn> [--role member|moderator]`
  - `mux group kick <urn> <member_urn>`
  - `mux group leave <urn>`
  - `mux group post <urn> <message-body> [--thread <thread_id>]` (body via stdin if `-` given)
  - `mux group read <urn> [--since-seq N] [--thread <thread_id>] [--mark-read]`
  - `mux group mentions [--since <ts>]`
  - `mux group archive <urn>`
- MCP (`tether_group_*` tools): one tool per HTTP endpoint. `tether_group_post` description explicitly documents the `@` / `!` / `:` symbol vocabulary so agents understand the parsing semantics.

#### Acceptance criteria

- [ ] All ops exposed at all three surfaces.
- [ ] CLI integration tests against a fixture daemon cover create → invite → post → read → mention-roundtrip.
- [ ] MCP tool descriptions are explicit about which symbols are daemon-parsed (`@` only) vs reserved (`!`, `:`).

---

### T-v060-05-07: ADR 0042 + symbol vocabulary docs

**kind:** agent
**priority:** 2
**manual:** true
**tags:** [groups, adr, docs, symbols]
**depends_on:** T-v060-05-06

#### Fix direction

- `docs/adr/0042-group-messaging.md`:
  - Context: multi-agent coordination via bilateral messaging fragments quickly; need a group-mailbox primitive; fan-out CC is the wrong shape.
  - Decision: groups are a registry kind with `msg://group/<authority>/<grp_id>` URN kind variant (3-segment, ADR-0023-compliant); mailbox-pull with per-member read cursor; mentions emit notices to personal inboxes; `@` is daemon-parsed and `!`/`:` are reserved-for-agent. ADR-0023 §1 kind enum extended here to include `group`.
  - Consequences: storage scales with messages, not messages × members; mentions integrate cleanly with personal mailboxes (familiar Slack-style activity feed); symbol vocabulary creates space for the directives package without entangling daemon and agent concerns.
  - Alternatives considered: (a) fan-out CC (rejected: storage cost + thread fragmentation + mark-read ambiguity); (b) hierarchical URN topics (rejected: rename-breaks-refs + threads already handle sub-conversations); (c) merge `!` and `:` into one symbol (rejected: conflates "do something" with "invoke a directive" — distinct intents).
- New `docs/groups/symbols.md`: standalone reference for the `@` / `!` / `:` vocabulary. Audience: agent authors. Topics: format, who parses, escape syntax, examples, anti-patterns (don't reuse `!` for non-commands; don't mix `@` and `\@` inconsistently). Cross-link from the directives-package docs (when they exist).
- Extend `docs/api/README.md` with `## Groups` section.

#### Acceptance criteria

- [ ] ADR 0042 lands, dated, linked from API + groups docs.
- [ ] Symbol-vocabulary doc covers all three symbols with format + parsing-side + escape syntax.
- [ ] At least one example for each symbol.

---

### T-v060-05-08: Cross-substrate ship notice

**kind:** agent
**priority:** 3
**manual:** true
**tags:** [groups, ship-notice]
**depends_on:** T-v060-05-06, T-v060-05-07

#### Fix direction

- After all tasks land + `make check` green: send `notice` from `msg://agent/agent-mux/tether-registry-design` to both:
  - `msg://agent/agent-mux/agridd-keeper`
  - `msg://agent/agent-mux/cerberus-registry-design`
- Subject: `"Sprint v060-05 SHIPPED — group messaging live"`.
- Body: announce the new `group` kind + URN path variant, summarize the four messaging ops, document the `@` mention semantics, point at the symbol-vocabulary doc, suggest concrete use cases (coordination rooms for multi-agent sprints, incident bridges, design rounds — like the FU-31 / cerberus-input rounds we did, which could now happen in a single room instead of three bilateral threads).

#### Acceptance criteria

- [ ] Both notices sent; message_ids captured in sprint close notes.

---

## Review / readiness notes

- **Sequence-counter race.** T-01 leaves two options for `group_seq` assignment. Pick one in impl: dedicated counter table is faster but adds a row; `MAX+1` under write-lock is simpler but serializes. Test both under high contention if unsure.
- **Mention parser performance.** O(message_length) per send. For very long messages, cap parsing at first 32 mentions to prevent malicious blow-up. Document the cap.
- **Display-name ambiguity** is a real risk now that mentions resolve short-forms. The directory currently allows two agents to share `display_name` (uniqueness is on URN only). T-05 handles by rejecting ambiguous mentions; a follow-up sprint could add `display_name` uniqueness per substrate (or globally) if the ambiguity rate gets annoying.
- **Group avatars.** The `avatar` field on the registry row works for groups too (e.g. a project logo, an incident icon). No new field needed.
- **Member privacy.** v1 group member lists are visible to all members. No private-membership model. Adequate for trusted multi-agent collaboration; would need rethinking if groups span less-trusted boundaries.
- **`!` and `:` security.** Out of scope. The directives package + each agent's command handler own their own security model. Daemon transports bytes verbatim.
- **Bot-friendliness.** All ops are MCP-exposed so agents can drive group lifecycle as part of orchestration flows (auto-create incident bridge when alert fires, auto-invite relevant service-owner agents, etc.).
- **No event emission on group writes.** Same scope-fence as v060-01 (registry doesn't yet feed the event bus). Cache invalidation in consumers happens via re-pull. Event emission lands when the broader registry-events sprint does.

---

## Done checklist (at sprint close)

- [ ] All eight task acceptance sections ticked.
- [ ] Exit criteria above all ticked.
- [ ] `make check` green.
- [ ] ADR 0042 committed.
- [ ] Branch FF-merged to `main`, branch deleted.
- [ ] Ship notices sent to agridd-keeper + cerberus-registry-design; message_ids recorded.
- [ ] Self-test: create a coordination group with self + a fixture agent + post + @-mention + verify the fixture agent's personal inbox shows the notice + verify `mux group read` shows the message.

---

## Hand-off snippet for the parallel agent

> You're executing Sprint v060-05 — Group Messaging. Scope is the `group` federation-directory kind + group-mailbox messaging semantics + the `@` / `!` / `:` symbol vocabulary. v060-01 must be shipped first. Read the epic at `planning/docs/epics/v0.6-federation-directory.md` and this sprint file end-to-end before starting. The big architecture choices (mailbox-pull not CC, flat URN with thread_id for sub-conversations, daemon parses only `@`, sibling table for membership not registry_links) are locked in D1–D12 — do not reopen. The symbol vocabulary section (D6 detail) is the unique deliverable beyond plumbing — read it carefully and make sure the ADR + symbol-vocabulary doc preserve the daemon vs agent-side distinction explicitly. `!` and `:` are reserved namespace; the directives package owns `:` semantics and is out of scope for this sprint. `make check` green is non-negotiable. Ship notices to agridd + cerberus are the last step.
