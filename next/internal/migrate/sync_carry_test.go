package migrate

import (
	"context"
	"database/sql"
	"testing"

	gostore "github.com/hollis-labs/libs/core/store"

	"github.com/hollis-labs/tether/next/internal/store/storetest"
)

// TestBulkThenFinalCarriesEveryChange is plan 7.3's two-phase method on fixtures: a bulk run on snapshot A,
// then the old hub keeps running (updates in place, deletes by retention and purges, inserts, a deleted
// newest row whose rowid is reused), and a final run on snapshot B. The final run rewrites exactly the
// ranges that changed, and the result equals B: the same digests as a fresh full run on B.
func TestBulkThenFinalCarriesEveryChange(t *testing.T) {
	eachTarget(t, func(t *testing.T, d gostore.Driver) {
		ctx := context.Background()
		a := oldFixture(t)
		srcA, err := OpenSource(a)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = srcA.Close() }()
		tgt := storetest.Raw(t, d)
		mustOK(t)(New(srcA, a, tgt).Run(ctx, Options{Phase: PhaseBulk, Batch: 10}))

		b := copyFile(t, a)
		db, err := sql.Open("sqlite", b)
		if err != nil {
			t.Fatal(err)
		}
		for _, q := range []string{
			// In-place updates: delivery and read state, a purge (payload nulled, row kept), a revocation.
			`UPDATE messages SET delivered_at = '2026-11-15T00:00:00Z', read_at = '2026-11-15T00:00:01Z' WHERE id IN ('m-010', 'm-011')`,
			`UPDATE messages SET payload = NULL, metadata = NULL WHERE id = 'm-099'`,
			`UPDATE principals SET revoked_at = '2026-11-15T00:00:02Z' WHERE id = 3`,
			`UPDATE settings SET value_json = '2', updated_at = '2026-11-15 00:00:03' WHERE scope = 'global'`,
			// Deletes: a routing-reply cleanup of one message, a whole early range, a logical agent.
			`DELETE FROM messages WHERE id = 'm-030'`,
			`DELETE FROM messages WHERE id BETWEEN 'm-001' AND 'm-005'`,
			`DELETE FROM logical_agents WHERE id = 'agent-20'`,
			// Inserts: new mail, a new principal (AUTOINCREMENT above the carried mark), a new setting.
			`INSERT INTO messages (id, kind, from_urn, to_urn, created_at) VALUES ('n-001', 'notice', 'f', 't', '2026-11-15T00:00:04Z')`,
			`INSERT INTO principals (principal_id, kind, display, token_hash, scopes_json, addresses_json, created_at)
				VALUES ('svc-new', 'service', 'd', '` + "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaab" + `', '[]', '[]', 't')`,
			`INSERT INTO settings (scope, scope_id, key, value_json, updated_at) VALUES ('user', 'u', 'k', '1', 't')`,
		} {
			if _, err := db.Exec(q); err != nil {
				t.Fatalf("%s: %v", q, err)
			}
		}
		_ = db.Close()
		srcB, err := OpenSource(b)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = srcB.Close() }()
		final := mustOK(t)(New(srcB, b, tgt).Run(ctx, Options{Phase: PhaseFinal, Batch: 10}))
		written := map[string]int{}
		for _, c := range final.Copy {
			written[c.Table] = c.Written
		}
		for table, max := range map[string]int{"messages": 6, "principals": 1, "settings": 1, "logical_agents": 1, "workers": 0, "pairing_grants": 0} {
			if written[table] == 0 && max > 0 {
				t.Errorf("%s: no range rewritten, but it changed", table)
			}
			if written[table] > max {
				t.Errorf("%s: %d ranges rewritten, want at most %d (only changed ranges)", table, written[table], max)
			}
		}

		// The result equals a fresh full run on B, table by table.
		fresh := storetest.Raw(t, d)
		full := mustOK(t)(New(srcB, b, fresh).Run(ctx, Options{Phase: PhaseFinal, Batch: 10}))
		digests := map[string]string{}
		for _, v := range full.Verify {
			digests[v.Table] = v.Digest
		}
		for _, v := range final.Verify {
			if digests[v.Table] != v.Digest {
				t.Errorf("%s: two-phase digest differs from a full run on B", v.Table)
			}
		}
		if got := queryInt(t, tgt, `SELECT count(*) FROM messages WHERE id = 'm-030' OR id BETWEEN 'm-001' AND 'm-005'`); got != 0 {
			t.Errorf("%d deleted messages survived the sync", got)
		}
		if got := queryInt(t, tgt, `SELECT count(*) FROM messages WHERE id = 'm-099' AND payload IS NULL`); got != 1 {
			t.Error("the purge (payload nulled) was not carried")
		}
	})
}
