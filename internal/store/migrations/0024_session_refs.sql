-- 0024_session_refs.sql
--
-- S2 of sprint SP-20260912-0001 (CW-20260912-0060), design record
-- CW-20260912-0023.
--
-- What a session touched: one row per (object, relation) a session created,
-- updated, read or referenced.
--
-- THE TRIPLE IS CLOSED. kind / ref_id / uri is the entire payload, and it is
-- closed to extension by default -- per Chrispian, "any change to that would
-- require a discussion and investigation with a default position of closed to
-- changes."
--
-- That is enforced structurally rather than by convention: there is NO content
-- column and NO JSON blob, so a caller who decides it would be convenient to
-- park a task body or a memory payload here has nowhere to put it. This is the
-- single property that keeps the store from becoming a second source of truth
-- (tracking-integrity.md: designate one source, derive the rest). A session ref
-- POINTS AT a Torque task or a Tesseract revision; it never copies one.
--
-- The three non-triple columns each earn their place:
--
--   source   -- the trust boundary. See the CHECK below for what it does and
--               does not prove; the distinction is load-bearing and is easy to
--               overstate.
--   relation -- what makes audit mean anything. torque_task_get and
--               torque_task_create are both "touched"; only one is "left
--               behind."
--   at       -- ordering.
--
-- NO workstream_id COLUMN, deliberately. Workstream-level roll-up joins
-- through sessions.workstream_id (migration 0023). A ref belongs to a session
-- and a session belongs to a workstream; a second path to the same truth is a
-- second thing that can disagree.
--
-- FK declarations follow ADR-0008 (deferred enforcement) -- documentation
-- only; PRAGMA foreign_keys stays off, matching every migration since 0015.
--
-- CONSEQUENCE, stated because "declarative" is easy to read as "harmless":
-- the ON DELETE CASCADE below does NOT run. Delete a session row and its refs
-- are left behind pointing at a session that no longer exists -- silently, with
-- nothing reporting it. Anything walking session_refs must tolerate a dangling
-- session_id rather than assume the join succeeds. Cleaning them up is a
-- retention question and belongs to CW-20260912-0069, not here.

CREATE TABLE session_refs (
    id         INTEGER PRIMARY KEY,
    session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,

    -- kind names the sort of object ref_id identifies: torque_task,
    -- git_commit, git_pr, tesseract_revision, cerberus_deploy, adr, url.
    -- Deliberately NOT CHECK-constrained. A new kind means another system
    -- became correlatable, which is ordinary growth and should not need a
    -- migration -- and an unexpected kind cannot turn this table into a
    -- content store, which is the property the closedness actually protects.
    -- source and relation ARE constrained below, because a new value there
    -- would change what an existing row MEANS.
    kind   TEXT NOT NULL,
    ref_id TEXT NOT NULL,
    uri    TEXT,

    -- relation distinguishes touching from leaving behind.
    relation TEXT NOT NULL
        CHECK(relation IN ('created', 'updated', 'read', 'referenced')),

    -- source records HOW Tether came to know about this ref.
    --
    --   proxy -- OBSERVED. The mux proxy saw this session make this call
    --            carrying this identifier. The calling agent cannot fabricate
    --            that.
    --   api   -- recorded through Tether's own HTTP surface.
    --   agent -- ASSERTED. An agent said so.
    --
    -- proxy means observed, NOT validated. The proxy records what it saw; it
    -- does not check that the identifier refers to anything. An agent calling
    -- torque_task_get("CW-fake-0000") produces an honest observation of a
    -- meaningless id, and nothing downstream validates it either -- the
    -- gateway forwards without validation. Anything reading this column must
    -- carry that distinction; the stronger reading ("unforgeable") will
    -- mislead whoever audits with it.
    --
    -- ABSENT MUST NEVER IMPLY FORGED. A ref with source='agent', or the
    -- absence of any ref, is not evidence of anything. Direct MCP children,
    -- HTTP callers and the CLI all bypass the proxy entirely, so a session
    -- that did real work through those paths produces no proxy-observed refs.
    -- That is normal, not suspicious. (Discipline adopted from Tesseract's
    -- gatewayMetadata, CW-20260912-0055; provenance rationale in
    -- CW-20260912-0024.)
    source TEXT NOT NULL
        CHECK(source IN ('proxy', 'api', 'agent')),

    at TEXT NOT NULL,

    -- Idempotency is a schema property, not caller discipline: the hooks that
    -- write git refs (commit / PR / end-session) can run twice, and the second
    -- run must be a no-op rather than an error.
    UNIQUE(session_id, kind, ref_id, relation)
);

CREATE INDEX IF NOT EXISTS idx_session_refs_session ON session_refs(session_id);
CREATE INDEX IF NOT EXISTS idx_session_refs_lookup ON session_refs(kind, ref_id);
