package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hollis-labs/libs/harness/core/adapters/turnoutput"
	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/launchprofile"
	"github.com/hollis-labs/tether/internal/store"
)

func TestNativeSecretOutputWithholdsBeforeJournalAndReply(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(map[bool]string{false: "new", true: "upgrade"}[legacy], func(t *testing.T) {
			route := &launchprofile.Route{Channel: "ops", Kinds: []string{"final"}}
			svc, _ := outputHarness(t, route)
			row, err := svc.Store.GetSession("s1")
			if err != nil {
				t.Fatal(err)
			}
			origin, err := derivedRuntimeOrigin(nativeOriginExample())
			if err != nil {
				t.Fatal(err)
			}
			secret := strings.Repeat("private-output-marker-", 8)
			job := &turnOutputWrite{row: *row, route: route, origin: origin, result: turnoutput.Output{TurnID: "native-turn", Kind: turnoutput.KindFinal, Text: secret, StopReason: secret}}
			if legacy {
				job.journalID = store.TurnOutputID(row.ID, job.result.TurnID, string(job.result.Kind), "", secret)
				raw, err := json.Marshal(outputRetryRecord{Version: 1, Origin: origin, SessionID: row.ID, Route: route, Result: job.result})
				if err != nil {
					t.Fatal(err)
				}
				if err := svc.Store.WriteTurnOutputRetry(job.journalID, raw); err != nil {
					t.Fatal(err)
				}
				oldID := job.journalID
				if err := svc.rewriteSecretOutputRetries(context.Background()); err != nil {
					t.Fatal(err)
				}
				if _, err := svc.Store.ReadTurnOutputRetry(oldID); !os.IsNotExist(err) {
					t.Fatalf("old content-hash filename remains: %v", err)
				}
				job.journalID = store.WithheldTurnOutputID(row.ID, job.result.TurnID, string(job.result.Kind), "")
			} else if err := job.journal(svc); err != nil {
				t.Fatal(err)
			}
			raw, err := svc.Store.ReadTurnOutputRetry(job.journalID)
			if err != nil || strings.Contains(string(raw), secret) || !strings.Contains(string(raw), "reply withheld: secret") {
				t.Fatalf("journal is not a safe notice: %v %s", err, raw)
			}
			job, err = readOutputRetry(svc, job.journalID)
			if err != nil {
				t.Fatal(err)
			}
			if err := job.persist(context.Background(), svc); err != nil {
				t.Fatal(err)
			}
			if err := job.persist(context.Background(), svc); err != nil {
				t.Fatal(err)
			}
			pending, err := svc.Store.PendingTurnOutputRetries("", 128)
			if err != nil || len(pending) != 0 {
				t.Fatalf("completed secret notice left a retry file: %v %v", pending, err)
			}
			payloads := outputEvents(t, svc)
			if len(payloads) != 1 {
				t.Fatalf("duplicate/missing notice event: %+v", payloads)
			}
			payload := payloads[0]
			if payload.Reason != "secret_withheld" || payload.Size != len(secret) || payload.Text != "" || payload.StopReason != "" || payload.MessageID == "" {
				t.Fatalf("withholding event contains content or loses metadata: %+v", payload)
			}
			staged, err := svc.Store.StagedTurnOutput(context.Background(), payload.MessageID)
			if err != nil || strings.Contains(string(staged.Payload), secret) || !strings.Contains(string(staged.Payload), "reply withheld: secret") || !strings.Contains(string(staged.Payload), "native-turn") {
				t.Fatalf("routed reply silently dropped or contains secret: %s %v", staged.Payload, err)
			}
			rows, err := svc.Store.EventsSince(0)
			if err != nil {
				t.Fatal(err)
			}
			for _, e := range rows {
				if strings.Contains(e.PayloadJSON, secret) {
					t.Fatal("raw secret in event")
				}
				if e.Kind == events.KindSessionTurnOutput && (e.Envelope.PayloadDigest != "" || e.Envelope.Class.Sensitivity != "private" || !e.Envelope.Class.Sanitized) {
					t.Fatalf("invalid withholding metadata envelope: %+v", e.Envelope)
				}
			}
			tx, err := svc.Store.DB().BeginTx(context.Background(), nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := svc.Store.VerifyTurnOutputContent(context.Background(), tx, row.ID, payload.OutputID, payload.MessageID, "native-turn", "", secret, "final"); err != nil {
				_ = tx.Rollback()
				t.Fatalf("notice stranded native receipt acceptance: %v", err)
			}
			if err := svc.Store.VerifyTurnOutputContent(context.Background(), tx, row.ID, payload.OutputID, payload.MessageID, "native-turn", "", "wrong native text", "final"); err == nil {
				_ = tx.Rollback()
				t.Fatal("notice bypassed its expected size")
			}
			// Withheld verification intentionally has no content fingerprint:
			// same-size original text produces the same safe receipt identity.
			if err := svc.Store.VerifyTurnOutputContent(context.Background(), tx, row.ID, payload.OutputID, payload.MessageID, "native-turn", "", strings.Repeat("x", len(secret)), "final"); err != nil {
				_ = tx.Rollback()
				t.Fatalf("withheld identity still depends on text: %v", err)
			}
			_ = tx.Rollback()
			if payload.OutputID != store.WithheldTurnOutputID(row.ID, "native-turn", "final", "") || payload.OutputID == store.TurnOutputID(row.ID, "native-turn", "final", "", secret) {
				t.Fatal("withheld output retained a secret-content fingerprint")
			}
			if _, err := svc.Store.DB().Exec(`UPDATE messages SET payload='{"text":"wrong notice"}' WHERE id=?`, payload.MessageID); err != nil {
				t.Fatal(err)
			}
			tx, err = svc.Store.DB().BeginTx(context.Background(), nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := svc.Store.VerifyTurnOutputContent(context.Background(), tx, row.ID, payload.OutputID, payload.MessageID, "native-turn", "", secret, "final"); err == nil {
				_ = tx.Rollback()
				t.Fatal("notice bypassed staged body verification")
			}
			_ = tx.Rollback()
			if count := svc.TurnOutputPersistenceStats()["secret_withheld"]; count != 1 {
				t.Fatalf("retry counted withholding again: %d", count)
			}
		})
	}
}

func TestNativeSecretUpgradeRewritesExpiredWithoutReviving(t *testing.T) {
	svc, _ := outputHarness(t, nil)
	origin, err := derivedRuntimeOrigin(nativeOriginExample())
	if err != nil {
		t.Fatal(err)
	}
	secret := "expired-private-output-marker"
	id := store.TurnOutputID("s1", "native-turn", "final", "", secret)
	raw, err := json.Marshal(outputRetryRecord{Version: 1, Origin: origin, SessionID: "s1", Result: turnoutput.Output{TurnID: "native-turn", Kind: turnoutput.KindFinal, Text: secret}})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Store.WriteTurnOutputRetry(id, raw); err != nil {
		t.Fatal(err)
	}
	var dbPath string
	if err := svc.Store.DB().QueryRow(`SELECT file FROM pragma_database_list WHERE name='main'`).Scan(&dbPath); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dbPath+".turn-output-retries", id+".json")
	old := time.Now().Add(-store.RoutingStageRetention - time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	// A malformed record sorted before the secret must not stop cleanup of
	// later expired entries. The pass still reports that repair is needed.
	if err := svc.Store.WriteTurnOutputRetry(strings.Repeat("0", 64), []byte("invalid record")); err != nil {
		t.Fatal(err)
	}
	if err := svc.rewriteSecretOutputRetries(context.Background()); err == nil {
		t.Fatal("corrupt entry was not reported")
	}
	if err := svc.Store.CompleteTurnOutputRetry(strings.Repeat("0", 64)); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Store.ReadTurnOutputRetry(id); !os.IsNotExist(err) {
		t.Fatalf("expired content-hash filename remains: %v", err)
	}
	id = store.WithheldTurnOutputID("s1", "native-turn", "final", "")
	path = filepath.Join(dbPath+".turn-output-retries", id+".json")
	raw, err = svc.Store.ReadTurnOutputRetry(id)
	if err != nil || strings.Contains(string(raw), secret) || !strings.Contains(string(raw), "reply withheld: secret") {
		t.Fatalf("expired raw record survived: %s %v", raw, err)
	}
	after, err := os.Stat(path)
	if err != nil || !after.ModTime().Equal(before.ModTime()) || after.Mode().Perm() != 0600 {
		t.Fatalf("rewrite changed expiry or privacy: %+v %v", after, err)
	}
	pending, err := svc.Store.PendingTurnOutputRetries("", 128)
	if err != nil || len(pending) != 0 {
		t.Fatalf("expired record revived: %v %v", pending, err)
	}
	if count := svc.TurnOutputPersistenceStats()["secret_withheld"]; count != 1 {
		t.Fatalf("upgrade count=%d", count)
	}
}

func TestNativeSecretUpgradeFinishesInterruptedRekey(t *testing.T) {
	for _, linked := range []bool{false, true} {
		t.Run(map[bool]string{false: "body_replaced", true: "new_name_linked"}[linked], func(t *testing.T) {
			svc, _ := outputHarness(t, nil)
			origin, err := derivedRuntimeOrigin(nativeOriginExample())
			if err != nil {
				t.Fatal(err)
			}
			origin.Envelope.Class.Sensitivity = "private"
			oldID := store.TurnOutputID("s1", "native-turn", "final", "", "old private marker")
			newID := store.WithheldTurnOutputID("s1", "native-turn", "final", "")
			notice := events.SecretWithheldNotice("s1", "native-turn", 18)
			raw, err := json.Marshal(outputRetryRecord{Version: 1, Origin: origin, SessionID: "s1", WithheldReason: "secret_withheld", WithheldSize: 18, Result: turnoutput.Output{TurnID: "native-turn", Kind: turnoutput.KindFinal, Text: notice}})
			if err != nil {
				t.Fatal(err)
			}
			// Crash after the atomic body replacement, before retiring the old name.
			if err := svc.Store.WriteTurnOutputRetry(oldID, raw); err != nil {
				t.Fatal(err)
			}
			if linked {
				var dbPath string
				if err := svc.Store.DB().QueryRow(`SELECT file FROM pragma_database_list WHERE name='main'`).Scan(&dbPath); err != nil {
					t.Fatal(err)
				}
				dir := dbPath + ".turn-output-retries"
				if err := os.Link(filepath.Join(dir, oldID+".json"), filepath.Join(dir, newID+".json")); err != nil {
					t.Fatal(err)
				}
			}
			if err := svc.rewriteSecretOutputRetries(context.Background()); err != nil {
				t.Fatal(err)
			}
			if _, err := svc.Store.ReadTurnOutputRetry(oldID); !os.IsNotExist(err) {
				t.Fatalf("old hash filename survived restart: %v", err)
			}
			job, err := readOutputRetry(svc, newID)
			if err != nil || job.result.Text != notice || job.journalID != newID {
				t.Fatalf("safe rekey did not resume: %+v %v", job, err)
			}
			if count := svc.TurnOutputPersistenceStats()["secret_withheld"]; count != 0 {
				t.Fatalf("already withheld entry counted again: %d", count)
			}
		})
	}
}

func TestNativeSecretDuplicateIdentityRefused(t *testing.T) {
	svc, _ := outputHarness(t, nil)
	row, err := svc.Store.GetSession("s1")
	if err != nil {
		t.Fatal(err)
	}
	makeJob := func(text string) *turnOutputWrite {
		origin, err := derivedRuntimeOrigin(nativeOriginExample())
		if err != nil {
			t.Fatal(err)
		}
		return &turnOutputWrite{row: *row, origin: origin, result: turnoutput.Output{TurnID: "native-turn", Kind: turnoutput.KindFinal, Text: text}}
	}
	first := makeJob("first private marker")
	if err := first.journal(svc); err != nil {
		t.Fatal(err)
	}
	before, err := svc.Store.ReadTurnOutputRetry(first.journalID)
	if err != nil {
		t.Fatal(err)
	}
	second := makeJob("second private marker")
	for range 2 {
		if err := second.journal(svc); !errors.Is(err, store.ErrDuplicateWithheldOutput) {
			t.Fatalf("duplicate not refused: %v", err)
		}
	}
	after, err := svc.Store.ReadTurnOutputRetry(first.journalID)
	if err != nil || string(before) != string(after) {
		t.Fatalf("duplicate replaced first notice: %v", err)
	}
	if err := first.persist(context.Background(), svc); err != nil {
		t.Fatal(err)
	}
	// After journal retirement the published event still owns the identity.
	third := makeJob("first private marker")
	if err := third.persist(context.Background(), svc); !errors.Is(err, store.ErrDuplicateWithheldOutput) {
		t.Fatalf("published identity silently deduped: %v", err)
	}
	if count := svc.TurnOutputPersistenceStats()["withheld_identity_violation"]; count != 2 {
		t.Fatalf("violation count=%d", count)
	}
	if got := outputEvents(t, svc); len(got) != 1 {
		t.Fatalf("duplicate output event: %+v", got)
	}
}
