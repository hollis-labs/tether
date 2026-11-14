package migrate

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/hollis-labs/libs/core/store"
)

// TestTriggerParity runs the schema's triggers, checks and defaults on both dialects and requires the
// same outcome from each: the same rows, the same column values, and the same refusals with the same
// message. Each scenario writes a transcript; the SQLite transcript is the reference, and a few lines are
// also pinned to expected values so both dialects cannot drift together.
func TestTriggerParity(t *testing.T) {
	for _, sc := range paritySuite {
		t.Run(sc.name, func(t *testing.T) {
			lite := sc.run(t, sqliteStore(t))
			t.Logf("transcript:\n  %s", strings.Join(lite, "\n  "))
			for _, want := range sc.expect {
				if !contains(lite, want) {
					t.Errorf("sqlite transcript lacks %q:\n  %s", want, strings.Join(lite, "\n  "))
				}
			}
			t.Run("postgres", func(t *testing.T) {
				pg := sc.run(t, pgStore(t))
				if strings.Join(lite, "\n") != strings.Join(pg, "\n") {
					t.Errorf("transcripts differ:\nsqlite:\n  %s\npostgres:\n  %s", strings.Join(lite, "\n  "), strings.Join(pg, "\n  "))
				}
			})
		})
	}
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

// transcript records what each step did, in a form both dialects render the same way.
type transcript struct {
	t     *testing.T
	s     *store.Store
	lines []string
}

// knownErrors are the messages the schema's triggers raise, and the constraint kinds, in the words each
// dialect uses for them.
var knownErrors = []string{
	"session retirement audit retained", "team shim recovery audit retained",
	"replacement lineage is immutable", "replacement lineage is retained",
	"replaced protocol is frozen", "replaced protocol history is retained", "replaced protocol cannot be reinitialized",
	"replaced execution cannot reacquire placement", "replaced custody is frozen",
	"replaced session cannot regain runtime ownership", "replaced launch plan is frozen", "replaced launch plan is retained",
	"replaced native mapping is frozen", "replaced native mapping is retained", "replaced session output is fenced",
	"session principal requires an active session", "replacement identity remains off",
	"replacement retained authority unavailable", "replacement execution credential exceeds inherited ceiling",
	"retention policy snapshots are immutable", "retention policy snapshots are permanent",
	"event_id_required_after_cutover",
}

func errText(err error) string {
	if err == nil {
		return "ok"
	}
	msg := err.Error()
	for _, k := range knownErrors {
		if strings.Contains(msg, k) {
			return "refused: " + k
		}
	}
	switch {
	case strings.Contains(msg, "CHECK constraint failed"), strings.Contains(msg, "violates check constraint"):
		return "refused: check"
	case strings.Contains(msg, "UNIQUE constraint failed"), strings.Contains(msg, "duplicate key value"):
		return "refused: unique"
	}
	return "error: " + msg
}

// exec runs a statement and records its outcome under a label.
func (tr *transcript) exec(label, q string, args ...any) {
	tr.t.Helper()
	_, err := tr.s.ExecContext(context.Background(), q, args...)
	tr.lines = append(tr.lines, label+": "+errText(err))
}

// must runs a setup statement that has to succeed.
func (tr *transcript) must(q string, args ...any) {
	tr.t.Helper()
	if _, err := tr.s.ExecContext(context.Background(), q, args...); err != nil {
		tr.t.Fatalf("%s: %s: %v", tr.s.Dialect().Name(), q, err)
	}
}

var (
	millisUTC  = regexp.MustCompile(`^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d\.\d{3}Z$`)
	secondsUTC = regexp.MustCompile(`^\d{4}-\d\d-\d\d \d\d:\d\d:\d\d$`)
)

// render turns a scanned value into dialect-neutral text: clock values become their format, JSON
// objects and arrays are re-encoded with sorted keys, and bytes become text.
func render(v any) string {
	switch x := v.(type) {
	case nil:
		return "NULL"
	case []byte:
		v = string(x)
	case int64:
		return fmt.Sprint(x)
	case time.Time:
		// modernc returns TIMESTAMP-declared columns as time.Time; Postgres returns the text.
		v = x.UTC().Format("2006-01-02 15:04:05")
	}
	s := fmt.Sprint(v)
	switch {
	case millisUTC.MatchString(s):
		return "<now ms>"
	case secondsUTC.MatchString(s):
		return "<now s>"
	case strings.HasPrefix(s, "{") || strings.HasPrefix(s, "["):
		var j any
		if json.Unmarshal([]byte(s), &j) == nil {
			b, _ := json.Marshal(j)
			return string(b)
		}
	}
	return s
}

// query records every row of a query, rendered.
func (tr *transcript) query(label, q string, args ...any) {
	tr.t.Helper()
	rows, err := tr.s.QueryContext(context.Background(), q, args...)
	if err != nil {
		tr.t.Fatalf("%s: %s: %v", tr.s.Dialect().Name(), q, err)
	}
	defer func() { _ = rows.Close() }()
	cols, _ := rows.Columns()
	n := 0
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			tr.t.Fatal(err)
		}
		parts := make([]string, len(cols))
		for i, v := range vals {
			if b, ok := v.([]byte); ok {
				v = string(b)
			}
			if strings.HasPrefix(cols[i], "raw_") {
				// Compared byte for byte, not as a JSON value.
				parts[i] = cols[i] + "=" + fmt.Sprint(v)
				continue
			}
			parts[i] = cols[i] + "=" + render(v)
		}
		tr.lines = append(tr.lines, label+": "+strings.Join(parts, " "))
		n++
	}
	if err := rows.Err(); err != nil {
		tr.t.Fatal(err)
	}
	if n == 0 {
		tr.lines = append(tr.lines, label+": no rows")
	}
}

type scenario struct {
	name   string
	expect []string
	steps  func(tr *transcript)
}

func (sc scenario) run(t *testing.T, s *store.Store) []string {
	t.Helper()
	tr := &transcript{t: t, s: s}
	parents(tr)
	sc.steps(tr)
	return tr.lines
}

// parents inserts the rows the fixtures reference, so every scenario is valid with foreign keys
// enforced (the SQLite store enforces them; the Postgres baseline declares none) and the transcripts
// show what the triggers do, not a foreign key refusal.
func parents(tr *transcript) {
	tr.t.Helper()
	tr.must(`INSERT INTO logical_agents (id, created_at, updated_at) VALUES ('la', 't', 't')`)
	for _, g := range []string{"g", "g1"} {
		tr.must(`INSERT INTO session_groups (id, created_at, updated_at) VALUES (?, 't', 't')`, g)
	}
	for _, d := range []string{"t", "team"} {
		tr.must(`INSERT INTO team_definitions (team_id, version, payload) VALUES (?, 1, ?)`, d, []byte(`{}`))
	}
}

const hash64 = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func insertMessage(tr *transcript, id, kind, metadata string, staged int, group string, groupSeq int) {
	tr.t.Helper()
	var g, meta any
	if group != "" {
		g = group
	}
	if metadata != "" {
		meta = metadata
	}
	tr.must(`INSERT INTO messages (id, kind, from_urn, to_urn, thread_id, payload, content_type, metadata, created_at,
		routing_staged, group_urn, group_seq) VALUES (?, ?, 'msg://agent/a/from', 'msg://agent/a/to', 'th', 'héllo', 'text/plain', ?,
		'2026-11-14T00:00:01Z', ?, ?, ?)`, id, kind, meta, staged, g, groupSeq)
}

func insertSession(tr *transcript, id, state string) {
	tr.t.Helper()
	tr.must(`INSERT INTO sessions (id, launch_id, project_id, logical_agent_id, provider_id, workspace, state, created_at, updated_at)
		VALUES (?, 'l', 'p', 'la', 'claude', '/w', ?, 't', 't')`, id, state)
}

func insertReplacement(tr *transcript, source, replacement, mode, scopes string) {
	tr.t.Helper()
	tr.must(`INSERT INTO session_replacements (source_session_id, replacement_session_id, actor_uri, intent_key, binding_id,
		binding_generation, source_plan_digest, credential_scopes_json, credential_mode, committed_at)
		VALUES (?, ?, 'msg://agent/a', 'ik', 'b1', 1, 'd', ?, ?, 't')`, source, replacement, scopes, mode)
}

func insertSessionPrincipal(tr *transcript, label, session, scopes, hash string) {
	tr.t.Helper()
	tr.exec(label, `INSERT INTO principals (principal_id, kind, display, token_hash, scopes_json, session_id, addresses_json, created_at)
		VALUES ('msg://session/local/'||?, 'session', 'd', ?, ?, ?, '[]', 't')`, session, hash, scopes, session)
}

// authorityFixture is everything replacement_principal_authority requires of a replacement session s4
// that holds its retained authority: binding, host and port intents, enrollment, profile, roster and run.
// teamDefinition is the profile's props_json.team_definition, as an object or as a string holding JSON.
func authorityFixture(tr *transcript, teamDefinition string) {
	tr.t.Helper()
	insertSession(tr, "s3", "completed")
	insertSession(tr, "s4", "running")
	insertReplacement(tr, "s3", "s4", "enforce", `["message.read","message.write"]`)
	tr.must(`INSERT INTO runtime_bindings (id, target_urn, session_id, host_id, attempt_id, generation, visibility, leased_at,
		created_at, updated_at) VALUES ('b1', 'msg://agent/a', 's4', 'team', 'att', 1, 'tether-hosted', 't', 't', 't')`)
	tr.must(`INSERT INTO team_host_intents (intent_key, request, member) VALUES ('ik', ?, ?)`,
		[]byte(`{"RunID":"run1"}`), []byte(`{"session_id":"s4","actor":"msg://agent/a","status":"active","enrolled":true}`))
	tr.must(`INSERT INTO team_port_intents (port_kind, intent_key, payload, state) VALUES ('session', 'ik', ?, 'done')`, []byte(`"s4"`))
	tr.must(`INSERT INTO team_port_intents (port_kind, intent_key, binding_secret, acquired_urn, payload, state)
		VALUES ('enrollment', 'ik', 'att', 'msg://agent/a', ?, 'done')`,
		[]byte(`{"Enrollment":{"actor":"msg://agent/a","agent_id":"msg://agent/a"},"Pin":{"id":"td","revision":3,"digest":"dg"}}`))
	tr.must(`INSERT INTO registry_entries (urn, kind, display_name, created_at, updated_at, props_json)
		VALUES ('msg://agent/a', 'agent', 'a', 't', 't', ?)`, `{"team_definition":`+teamDefinition+`}`)
	tr.must(`INSERT INTO team_runs (run_id, session_group_id, team_id, team_version, payload, created_at)
		VALUES ('run1', 'g1', 'team', 1, ?, 't')`, []byte(`{"status":"running"}`))
	tr.must(`INSERT INTO team_rosters (run_id, version, payload) VALUES ('run1', 1, ?)`,
		[]byte(`{"members":[{"session_id":"s9","actor":"x","status":"active"},{"session_id":"s4","actor":"msg://agent/a","status":"active"}]}`))
}

var paritySuite = []scenario{
	{
		name: "registry instance ids",
		expect: []string{
			"insert mux only: tether_instance_id=m1 mux_instance_id=m1",
			"insert tether only: tether_instance_id=x mux_instance_id=x",
		},
		steps: func(tr *transcript) {
			tr.must(`INSERT INTO registry_entries (urn, kind, display_name, created_at, updated_at, mux_instance_id) VALUES ('u1', 'agent', 'd', 't', 't', 'm1')`)
			tr.query("insert mux only", `SELECT tether_instance_id, mux_instance_id FROM registry_entries WHERE urn = 'u1'`)
			tr.must(`INSERT INTO registry_entries (urn, kind, display_name, created_at, updated_at, tether_instance_id) VALUES ('u2', 'agent', 'd', 't', 't', 'x')`)
			tr.query("insert tether only", `SELECT tether_instance_id, mux_instance_id FROM registry_entries WHERE urn = 'u2'`)
			tr.must(`UPDATE registry_entries SET tether_instance_id = 'm2' WHERE urn = 'u1'`)
			tr.query("update tether", `SELECT tether_instance_id, mux_instance_id FROM registry_entries WHERE urn = 'u1'`)
			tr.must(`UPDATE registry_entries SET mux_instance_id = 'm3' WHERE urn = 'u1'`)
			tr.query("update mux", `SELECT tether_instance_id, mux_instance_id FROM registry_entries WHERE urn = 'u1'`)
			tr.must(`UPDATE registry_entries SET display_name = 'e' WHERE urn = 'u1'`)
			tr.query("update other", `SELECT tether_instance_id, mux_instance_id FROM registry_entries WHERE urn = 'u1'`)
		},
	},
	{
		name: "message events journal and seq",
		expect: []string{
			"seq: id=m1 seq=1",
			"seq: id=m5 seq=5",
		},
		steps: func(tr *transcript) {
			tr.must(`INSERT INTO team_runs (run_id, session_group_id, team_id, team_version, payload, created_at) VALUES ('r1', 'g', 't', 1, ?, 't')`, []byte(`{}`))
			insertMessage(tr, "m1", "notice", `{"urgency":"high","team_run":"r1"}`, 0, "", 0)
			insertMessage(tr, "m2", "STREAM.delta", `{"urgency":"low"}`, 0, "", 0)
			insertMessage(tr, "m3", "request", `not json`, 0, "", 0)
			insertMessage(tr, "m4", "request", `{"urgency":"urgent"}`, 1, "", 0)
			insertMessage(tr, "m5", "request", `[1,2]`, 0, "", 0)
			// Quoting: quotes, backslash, control characters, a slash, non-ASCII and an astral character.
			tr.must(`INSERT INTO messages (id, kind, from_urn, to_urn, thread_id, in_reply_to, payload, created_at)
				VALUES (?, 'notice', ?, 't', ?, ?, ?, '2026-11-14T00:00:01Z')`,
				`m"6\`, "a/b\tc\nd\u0001\u001f\u007f", "é✓😀", "\b\f\r", "😀")
			for _, col := range []string{"delivered_at", "read_at", "consumed_at", "archived_at", "canceled_at"} {
				// One row per statement: the order in which one UPDATE visits rows is not defined on
				// either engine, and the journal's ids follow it.
				for _, id := range []string{"m1", "m2", "m3", "m4"} {
					tr.must(`UPDATE messages SET `+col+` = '2026-11-14T00:00:02Z' WHERE id = ?`, id)
				}
				tr.must(`UPDATE messages SET ` + col + ` = '2026-11-14T00:00:03Z' WHERE id = 'm1'`) // already set: no event
			}
			tr.must(`UPDATE messages SET payload = NULL, metadata = NULL WHERE id = 'm1'`)
			tr.must(`UPDATE messages SET payload = NULL, metadata = NULL WHERE id = 'm2'`)
			tr.query("journal", `SELECT kind, at, payload_json AS raw_payload_json, team_run_id FROM message_event_journal ORDER BY id`)
			tr.query("seq", `SELECT id, seq FROM messages ORDER BY seq`)
		},
	},
	{
		name: "group read events",
		steps: func(tr *transcript) {
			tr.must(`INSERT INTO registry_entries (urn, kind, display_name, created_at, updated_at) VALUES ('grp', 'group', 'g', 't', 't')`)
			tr.must(`INSERT INTO registry_entries (urn, kind, display_name, created_at, updated_at) VALUES ('msg://agent/a/me', 'agent', 'me', 't', 't')`)
			tr.must(`INSERT INTO group_members (grp_urn, member_urn, joined_at) VALUES ('grp', 'msg://agent/a/me', 't')`)
			insertMessage(tr, "g1", "notice", "", 0, "grp", 1)
			insertMessage(tr, "g2", "stream.x", "", 0, "grp", 2)
			insertMessage(tr, "g3", "notice", "", 0, "grp", 3)
			insertMessage(tr, "g4", "notice", "", 0, "grp", 4)
			tr.must(`UPDATE group_members SET last_read_seq = 3`)
			tr.must(`UPDATE group_members SET last_read_seq = 2`) // backwards: no event
			tr.query("journal", `SELECT kind, at, payload_json AS raw_payload_json, team_run_id FROM message_event_journal ORDER BY id`)
		},
	},
	{
		name: "replaced session guards",
		expect: []string{
			"codex insert: refused: replaced protocol cannot be reinitialized",
			"lineage update: refused: replacement lineage is immutable",
		},
		steps: func(tr *transcript) {
			insertSession(tr, "s1", "completed")
			insertSession(tr, "s2", "running")
			insertSession(tr, "s5", "running")
			tr.must(`INSERT INTO session_shims (session_id, shim_key, host_backend, socket_path, descriptor_path, runtime, runtime_generation,
				boot_generation, created_at, updated_at) VALUES ('s1', 'k1', 'detached', '/s', '/d', 'r', 'g', 'b', 't', 't')`)
			tr.must(`INSERT INTO codex_shim_protocol (session_id, shim_key, protocol, revision, state_json) VALUES ('s1', 'k1', 'p', '1', '{}')`)
			tr.must(`INSERT INTO launch_plans (session_id, plan_json) VALUES ('s1', '{}')`)
			tr.must(`INSERT INTO session_provider_mappings (session_id, owner, provider, created_at, updated_at) VALUES ('s1', 'o', 'p', 't', 't')`)
			insertReplacement(tr, "s1", "s2", "off", `[]`)
			tr.exec("lineage update", `UPDATE session_replacements SET actor_uri = 'x'`)
			tr.exec("lineage delete", `DELETE FROM session_replacements`)
			tr.exec("codex update", `UPDATE codex_shim_protocol SET revision = '2' WHERE session_id = 's1'`)
			tr.exec("codex delete", `DELETE FROM codex_shim_protocol WHERE session_id = 's1'`)
			tr.exec("codex insert", `INSERT INTO codex_shim_protocol (session_id, shim_key, protocol, revision, state_json) VALUES ('s1', 'k2', 'p', '1', '{}')`)
			tr.exec("shim update", `UPDATE session_shims SET unit_name = 'u' WHERE session_id = 's1'`)
			tr.exec("shim insert", `INSERT INTO session_shims (session_id, shim_key, host_backend, socket_path, descriptor_path, runtime, runtime_generation,
				boot_generation, created_at, updated_at) VALUES ('s1', 'k3', 'detached', '/s', '/d', 'r', 'g', 'b', 't', 't')`)
			tr.exec("plan update", `UPDATE launch_plans SET plan_json = '[]' WHERE session_id = 's1'`)
			tr.exec("plan delete", `DELETE FROM launch_plans WHERE session_id = 's1'`)
			tr.exec("mapping update", `UPDATE session_provider_mappings SET native_session_id = 'n' WHERE session_id = 's1'`)
			tr.exec("mapping insert", `INSERT INTO session_provider_mappings (session_id, owner, provider, created_at, updated_at) VALUES ('s1', 'o2', 'p', 't', 't')`)
			tr.exec("mapping delete", `DELETE FROM session_provider_mappings WHERE session_id = 's1'`)
			tr.exec("admission state", `UPDATE sessions SET state = 'running' WHERE id = 's1'`)
			tr.exec("admission pid", `UPDATE sessions SET pid = 7 WHERE id = 's1'`)
			tr.exec("admission same", `UPDATE sessions SET state = 'completed', updated_at = 'u' WHERE id = 's1'`)
			tr.exec("delete source", `DELETE FROM sessions WHERE id = 's1'`)
			tr.exec("delete replacement", `DELETE FROM sessions WHERE id = 's2'`)
			tr.exec("staged output", `INSERT INTO messages (id, kind, from_urn, to_urn, created_at, routing_staged) VALUES ('o1', 'k', 'msg://session/local/s1', 't', 't', 1)`)
			tr.exec("unstaged output", `INSERT INTO messages (id, kind, from_urn, to_urn, created_at, routing_staged) VALUES ('o2', 'k', 'msg://session/local/s1', 't', 't', 0)`)
			tr.must(`INSERT INTO session_shim_retirements (operation_id, session_id, request_digest, revision, phase, record_json, created_at, updated_at)
				VALUES ('op', 's5', 'd', 'r', 'intent_recorded', '{}', 't', 't')`)
			tr.exec("delete retired", `DELETE FROM sessions WHERE id = 's5'`)
			insertSession(tr, "s6", "running")
			tr.must(`INSERT INTO team_gone_shim_recoveries (shim_key, session_id, binding_id, actor_uri, binding_generation, intent_key, custody_json, state, confirmed_at)
				VALUES ('k6', 's6', 'b', 'a', 1, 'i', '{}', 'recovery_pending', 't')`)
			tr.exec("delete recovering", `DELETE FROM sessions WHERE id = 's6'`)
			insertSession(tr, "s7", "running")
			tr.exec("delete plain", `DELETE FROM sessions WHERE id = 's7'`)
		},
	},
	{
		name: "session principals",
		expect: []string{
			"replacement off: refused: replacement identity remains off",
			"after terminal: revoked_at=<now ms>",
		},
		steps: func(tr *transcript) {
			insertSession(tr, "s1", "completed")
			insertSession(tr, "s2", "running")
			insertSession(tr, "s8", "running")
			insertSession(tr, "s9", "running")
			insertReplacement(tr, "s1", "s2", "off", `[]`)
			insertSessionPrincipal(tr, "inactive", "s1", `[]`, strings.Repeat("a", 64))
			insertSessionPrincipal(tr, "missing", "nope", `[]`, strings.Repeat("b", 64))
			insertSessionPrincipal(tr, "replacement off", "s2", `[]`, strings.Repeat("c", 64))
			insertSessionPrincipal(tr, "plain", "s8", `["x"]`, strings.Repeat("d", 64))
			insertSessionPrincipal(tr, "plain 2", "s9", `["x"]`, strings.Repeat("e", 64))
			tr.must(`UPDATE sessions SET state = 'completed' WHERE id = 's8'`)
			tr.query("after terminal", `SELECT revoked_at FROM principals WHERE session_id = 's8'`)
			tr.must(`DELETE FROM sessions WHERE id = 's9'`)
			tr.query("after delete", `SELECT revoked_at FROM principals WHERE session_id = 's9'`)
			tr.exec("device without expiry", `INSERT INTO principals (principal_id, kind, display, token_hash, scopes_json, addresses_json, created_at)
				VALUES ('dev', 'device', 'd', ?, '[]', '[]', 't')`, strings.Repeat("f", 64))
			tr.exec("short hash", `INSERT INTO principals (principal_id, kind, display, token_hash, scopes_json, addresses_json, created_at)
				VALUES ('op', 'operator', 'd', 'abc', '[]', '[]', 't')`)
		},
	},
	{
		name:   "replacement authority, object definition",
		expect: []string{"within ceiling: ok"},
		steps: func(tr *transcript) {
			authorityFixture(tr, `{"id":"td","revision":3,"digest":"dg"}`)
			replacementAuthoritySteps(tr)
		},
	},
	{
		name:   "replacement authority, string definition",
		expect: []string{"within ceiling: ok"},
		steps: func(tr *transcript) {
			authorityFixture(tr, `"{\"id\":\"td\",\"revision\":3,\"digest\":\"dg\"}"`)
			replacementAuthoritySteps(tr)
		},
	},
	{
		name:   "replacement authority, pin mismatch",
		expect: []string{"within ceiling: refused: replacement retained authority unavailable"},
		steps: func(tr *transcript) {
			authorityFixture(tr, `{"id":"td","revision":"3","digest":"dg"}`) // revision as a string: json_extract types differ
			insertSessionPrincipal(tr, "within ceiling", "s4", `["message.read"]`, strings.Repeat("1", 64))
		},
	},
	{
		name: "events",
		expect: []string{
			"after seed, no id: refused: event_id_required_after_cutover",
			"digest not hex: refused: check",
		},
		steps: func(tr *transcript) {
			tr.exec("before seed, no id", `INSERT INTO events (session_id, at, kind) VALUES ('s', 't', 'k')`)
			tr.must(`INSERT INTO event_identity (singleton, environment_id, cutover, next_legacy_id, remaining) VALUES (1, 'e', 0, 0, 0)`)
			tr.exec("after seed, no id", `INSERT INTO events (session_id, at, kind) VALUES ('s', 't', 'k')`)
			tr.exec("after seed, empty id", `INSERT INTO events (session_id, at, kind, event_id) VALUES ('s', 't', 'k', '')`)
			tr.exec("after seed, id", `INSERT INTO events (session_id, at, kind, event_id) VALUES ('s', 't', 'k', 'e1')`)
			tr.exec("duplicate id", `INSERT INTO events (session_id, at, kind, event_id) VALUES ('s', 't', 'k', 'e1')`)
			tr.exec("digest ok", `INSERT INTO events (at, kind, event_id, payload_digest) VALUES ('t', 'k', 'e2', ?)`, "sha256:"+hash64)
			tr.exec("digest withheld", `INSERT INTO events (at, kind, event_id, payload_digest) VALUES ('t', 'k', 'e3', 'withheld')`)
			tr.exec("digest not hex", `INSERT INTO events (at, kind, event_id, payload_digest) VALUES ('t', 'k', 'e4', ?)`, "sha256:"+strings.Repeat("G", 64))
			tr.exec("digest upper hex", `INSERT INTO events (at, kind, event_id, payload_digest) VALUES ('t', 'k', 'e5', ?)`, "sha256:"+strings.ToUpper(hash64))
			tr.exec("digest and payload", `INSERT INTO events (at, kind, event_id, payload_digest, payload_json) VALUES ('t', 'k', 'e6', 'withheld', '{}')`)
			tr.exec("bad retention class", `INSERT INTO events (at, kind, event_id, retention_class) VALUES ('t', 'k', 'e7', 'R-x')`)
			// Ranks, not values: a Postgres identity value is spent by a refused insert, an AUTOINCREMENT
			// value is not, so only the order of ids is the same on both.
			tr.query("ids", `SELECT event_id, (SELECT count(*) FROM events e2 WHERE e2.id <= events.id) AS position FROM events ORDER BY id`)
			tr.exec("second identity", `INSERT INTO event_identity (singleton, environment_id, cutover, next_legacy_id, remaining) VALUES (2, 'e', 0, 0, 0)`)
		},
	},
	{
		name:   "immutable retention policies",
		expect: []string{"update: refused: retention policy snapshots are immutable"},
		steps: func(tr *transcript) {
			tr.must(`INSERT INTO event_retention_policies (id, policy_json, created_at) VALUES ('p1', '{}', 't')`)
			tr.exec("update", `UPDATE event_retention_policies SET policy_json = '[]'`)
			tr.exec("delete", `DELETE FROM event_retention_policies`)
		},
	},
	{
		name:   "defaults, identities and checks",
		expect: []string{"settings: scope_id= updated_at=<now s>", "aliases case: refused: unique"},
		steps: func(tr *transcript) {
			tr.must(`INSERT INTO settings (scope, key, value_json) VALUES ('global', 'k', '1')`)
			tr.query("settings", `SELECT scope_id, updated_at FROM settings`)
			tr.must(`INSERT INTO proxy_events (server, tool_name, timestamp) VALUES ('s', 't', 'ts')`)
			tr.must(`INSERT INTO proxy_events (server, tool_name, timestamp) VALUES ('s', 't', 'ts')`)
			tr.query("proxy events", `SELECT id, duration_ms, ok, attribution_json, claimed_session_id, telemetry_json FROM proxy_events ORDER BY id`)
			tr.must(`INSERT INTO identity_audit (at, mode, authentication, method, route) VALUES ('t', 'm', 'a', 'GET', '/')`)
			tr.query("identity audit", `SELECT id, principal_id, verified, would_allow FROM identity_audit`)
			tr.exec("bool check", `INSERT INTO identity_audit (at, mode, authentication, method, route, verified) VALUES ('t', 'm', 'a', 'GET', '/', 2)`)
			tr.exec("policy one", `INSERT INTO execution_policy (id, policy_json, updated_at) VALUES (1, '{}', 't')`)
			tr.exec("policy two", `INSERT INTO execution_policy (id, policy_json, updated_at) VALUES (2, '{}', 't')`)
			insertSession(tr, "s", "running")
			insertSession(tr, "s2", "running")
			tr.exec("mcp policy json", `INSERT INTO session_mcp_policy (session_id, policy_json) VALUES ('s', '{"a":1}')`)
			tr.exec("mcp policy not json", `INSERT INTO session_mcp_policy (session_id, policy_json) VALUES ('s2', 'nope')`)
			tr.exec("grant scopes object", `INSERT INTO pairing_grants (id, code_hash, label, scopes_json, created_by, created_at, expires_at)
				VALUES ('g1', ?, 'l', '{}', 'c', 't', 't')`, hash64)
			tr.exec("grant scopes array", `INSERT INTO pairing_grants (id, code_hash, label, scopes_json, created_by, created_at, expires_at)
				VALUES ('g2', ?, 'l', '[]', 'c', 't', 't')`, hash64)
			tr.exec("aliases", `INSERT INTO message_aliases (urn, alias) VALUES ('u1', 'Bob')`)
			tr.exec("aliases case", `INSERT INTO message_aliases (urn, alias) VALUES ('u2', 'bOB')`)
			tr.exec("aliases non-ascii", `INSERT INTO message_aliases (urn, alias) VALUES ('u3', 'É')`)
			tr.exec("aliases non-ascii case", `INSERT INTO message_aliases (urn, alias) VALUES ('u4', 'é')`)
			// 131071 two-byte characters plus the quotes: 262144 bytes passes, one more character does not.
			for _, n := range []int{131071, 131072} {
				tr.exec(fmt.Sprintf("record bytes %d", 2*n+2), `INSERT INTO session_shim_retention_cursors (cursor_id, request_digest, revision, record_json, created_at, updated_at)
					VALUES (?, 'd', 'r', ?, 't', 't')`, fmt.Sprint(n), `"`+strings.Repeat("é", n)+`"`)
			}
			tr.exec("scopes not array", `INSERT INTO session_replacements (source_session_id, replacement_session_id, actor_uri, intent_key, binding_id,
				binding_generation, source_plan_digest, credential_scopes_json, credential_mode, committed_at)
				VALUES ('a', 'b', 'u', 'i', 'b', 1, 'd', '{}', 'off', 't')`)
			tr.exec("self replacement", `INSERT INTO session_replacements (source_session_id, replacement_session_id, actor_uri, intent_key, binding_id,
				binding_generation, source_plan_digest, credential_scopes_json, credential_mode, committed_at)
				VALUES ('a', 'a', 'u', 'i', 'b', 1, 'd', '[]', 'off', 't')`)
			tr.exec("seat blocked without owner", `INSERT INTO seat_status (agent_urn, state, since, set_by, updated_at) VALUES ('a', 'blocked', 't', 's', 't')`)
			tr.query("checkpoints seed", `SELECT source, position FROM message_event_checkpoints ORDER BY source`)
		},
	},
}

// replacementAuthoritySteps exercises each branch of replacement_principal_authority on a fixture that
// holds its authority.
func replacementAuthoritySteps(tr *transcript) {
	insertSessionPrincipal(tr, "within ceiling", "s4", `["message.read"]`, strings.Repeat("1", 64))
	tr.must(`UPDATE principals SET revoked_at = 't' WHERE session_id = 's4'`) // one active session principal at a time
	insertSessionPrincipal(tr, "over ceiling", "s4", `["admin"]`, strings.Repeat("2", 64))
	insertSessionPrincipal(tr, "scopes not array", "s4", `{"a":1}`, strings.Repeat("3", 64))
	tr.must(`UPDATE runtime_bindings SET lease_expires_at = '2000-01-01T00:00:00Z'`)
	insertSessionPrincipal(tr, "lease expired", "s4", `["message.read"]`, strings.Repeat("4", 64))
	tr.must(`UPDATE runtime_bindings SET lease_expires_at = '2999-01-01T00:00:00.123456789Z'`)
	insertSessionPrincipal(tr, "lease live", "s4", `["message.read"]`, strings.Repeat("5", 64))
	tr.must(`UPDATE principals SET revoked_at = 't' WHERE session_id = 's4'`)
	tr.must(`UPDATE team_host_intents SET member = ? WHERE intent_key = 'ik'`, []byte(`{"session_id":"s4","actor":"msg://agent/a","status":"active","enrolled":false}`))
	insertSessionPrincipal(tr, "not enrolled", "s4", `["message.read"]`, strings.Repeat("6", 64))
	tr.must(`UPDATE team_host_intents SET member = ? WHERE intent_key = 'ik'`, []byte(`{"session_id":"s4","actor":"msg://agent/a","status":"active","enrolled":1}`))
	tr.must(`UPDATE team_runs SET payload = ? WHERE run_id = 'run1'`, []byte(`{"status":"completed"}`))
	insertSessionPrincipal(tr, "run completed", "s4", `["message.read"]`, strings.Repeat("7", 64))
	tr.must(`UPDATE team_runs SET payload = ? WHERE run_id = 'run1'`, []byte(`{"status":"running"}`))
	tr.must(`UPDATE team_host_intents SET request = ? WHERE intent_key = 'ik'`, []byte(`{"RunID":""}`))
	tr.must(`INSERT INTO team_launch_leases (launch_key, owner, fence, expires_ns) VALUES ('lk', 'o', 1, 0)`)
	tr.must(`INSERT INTO team_launches (launch_key, digest, state, revision, payload) VALUES ('lk', 'd', 's', 1, ?)`, []byte(`{"Run":{"id":"run1"}}`))
	tr.must(`INSERT INTO team_host_launch_intents (intent_key, launch_key) VALUES ('ik', 'lk')`)
	insertSessionPrincipal(tr, "run from launch", "s4", `["message.read"]`, strings.Repeat("8", 64))
	tr.must(`UPDATE principals SET revoked_at = 't' WHERE session_id = 's4'`)
}
