package teamhost_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/hollis-labs/substrate/mesh"
	"github.com/hollis-labs/substrate/mesh/teams"
	"github.com/hollis-labs/tether/internal/teamhost"
	"github.com/hollis-labs/tether/internal/teamstore"
)

func delegation(t *testing.T, s *teamstore.Store, h *teamhost.Host) (teams.Delivery, teams.Member) {
	t.Helper()
	run, root := launch(t, s, h)
	worker := spawn(t, s, h, run, root, "worker")
	roster, err := s.Snapshot(ctx, run.ID)
	must(t, err)
	route := teams.Route{RunID: run.ID, RosterVersion: roster.Version, Sender: root, Recipients: []teams.Member{worker}, Verb: mesh.Delegate}
	return teams.Delivery{IdempotencyKey: "delegate", From: root.Actor, Recipient: worker, Body: "work", Verb: mesh.Delegate, Route: route}, worker
}
func TestHostStoppedRecipientDeadLettersWithoutCallingUnavailableTransport(t *testing.T) {
	f := fake()
	db, s, h := open(t, t.TempDir()+"/db", f)
	d, worker := delegation(t, s, h)
	must(t, h.SendMessage(ctx, d))
	must(t, h.Retire(ctx, "retire", worker))
	calls := 0
	f.before = func(op, key string) error {
		if op == "deliver" {
			calls++
			return teams.ErrUnavailable
		}
		return nil
	}
	if err := h.FlushMessages(ctx, 10); !errors.Is(err, teamhost.ErrSessionGone) {
		t.Fatal("host-stopped delivery retried", err)
	}
	if calls != 0 {
		t.Fatal("transport called after owned Stop")
	}
	state, err := h.DelegationState(ctx, d.IdempotencyKey)
	must(t, err)
	if state != mesh.TaskFailed {
		t.Fatal("ended recipient delegate stayed working")
	}
	letters, err := h.ListDeadLetters(ctx, teamhost.DeadLetterCursor{}, 1)
	must(t, err)
	if len(letters) != 1 {
		t.Fatal("missing dead-letter evidence")
	}
	var dead int
	must(t, db.DB().QueryRow(`SELECT dead FROM team_host_deliveries WHERE delivery_key=?`, d.IdempotencyKey).Scan(&dead))
	if dead != 1 {
		t.Fatal("dead outcome not committed")
	}
}
func TestDeadLetterCursorAndClockSkewClamp(t *testing.T) {
	f := fake()
	db, s, _ := open(t, t.TempDir()+"/db", f)
	clock := time.Now()
	h := timedHost(t, db, s, f, &clock)
	d, _ := delegation(t, s, h)
	for _, key := range []string{"first", "second", "third"} {
		d.IdempotencyKey = key
		must(t, h.SendMessage(ctx, d))
	}
	_, err := db.DB().Exec(`UPDATE team_host_deliveries SET dead=1,last_error='fixture'`)
	must(t, err)
	var keys []string
	cursor := teamhost.DeadLetterCursor{}
	for {
		page, err := h.ListDeadLetters(ctx, cursor, 1)
		must(t, err)
		if len(page) == 0 {
			break
		}
		if page[0].Kind < cursor.Kind || page[0].Kind == cursor.Kind && page[0].Sequence <= cursor.Sequence {
			t.Fatal("dead-letter cursor did not advance", page[0], cursor)
		}
		keys = append(keys, page[0].Key)
		cursor = teamhost.DeadLetterCursor{Kind: page[0].Kind, Sequence: page[0].Sequence}
	}
	if !reflect.DeepEqual(keys, []string{"first", "second", "third"}) {
		t.Fatal("dead-letter paging skipped/repeated", keys)
	}
	d.IdempotencyKey = "skew"
	must(t, h.SendMessage(ctx, d))
	_, err = db.DB().Exec(`UPDATE team_host_deliveries SET next_attempt_at=? WHERE delivery_key=?`, clock.Add(365*24*time.Hour).UnixNano(), d.IdempotencyKey)
	must(t, err)
	must(t, h.FlushMessages(ctx, 10))
	var due int64
	must(t, db.DB().QueryRow(`SELECT next_attempt_at FROM team_host_deliveries WHERE delivery_key=?`, d.IdempotencyKey).Scan(&due))
	if due != clock.Add(time.Minute).UnixNano() {
		t.Fatal("clock skew hid delivery", time.Unix(0, due))
	}
	clock = clock.Add(time.Minute)
	must(t, h.FlushMessages(ctx, 10))
	if _, ok := f.deliveries[d.IdempotencyKey]; !ok {
		t.Fatal("clamped row never retried")
	}
}
func TestTerminalDelegateSameKeyReplaysChangedContentConflictsNewKeyWorks(t *testing.T) {
	f := fake()
	db, s, h := open(t, t.TempDir()+"/db", f)
	d, _ := delegation(t, s, h)
	must(t, h.SendMessage(ctx, d))
	f.before = func(op, key string) error {
		if op == "deliver" {
			return teamhost.ErrInvalidRequest
		}
		return nil
	}
	if err := h.FlushMessages(ctx, 10); !errors.Is(err, teamhost.ErrInvalidRequest) {
		t.Fatal(err)
	}
	before, err := h.ListDeadLetters(ctx, teamhost.DeadLetterCursor{}, 10)
	must(t, err)
	must(t, h.SendMessage(ctx, d)) // recorded receipt replay, no new delivery/state change
	changed := d
	changed.Body = "different"
	if err = h.SendMessage(ctx, changed); !errors.Is(err, teams.ErrConflict) {
		t.Fatal("changed terminal retry accepted", err)
	}
	if err = h.ReviveDeadLetter(ctx, "deliveries", d.IdempotencyKey); !errors.Is(err, teams.ErrConflict) {
		t.Fatal("terminal delegate revived", err)
	}
	after, err := h.ListDeadLetters(ctx, teamhost.DeadLetterCursor{}, 10)
	must(t, err)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("refused revival changed queue evidence")
	}
	state, err := h.DelegationState(ctx, d.IdempotencyKey)
	must(t, err)
	if state != mesh.TaskFailed {
		t.Fatal("terminal outcome reopened")
	}
	var dead, attempts int
	must(t, db.DB().QueryRow(`SELECT dead,attempts FROM team_host_deliveries WHERE delivery_key=?`, d.IdempotencyKey).Scan(&dead, &attempts))
	if dead != 1 || attempts != 1 {
		t.Fatal("refused revival changed queue row")
	}
	next := d
	next.IdempotencyKey = "new-authorized-key"
	must(t, h.SendMessage(ctx, next))
	state, err = h.DelegationState(ctx, next.IdempotencyKey)
	must(t, err)
	if state != mesh.TaskWorking {
		t.Fatal("new delegation not independent")
	}
	f.before = nil
	must(t, h.FlushMessages(ctx, 10))
	if _, ok := f.deliveries[d.IdempotencyKey]; ok {
		t.Fatal("terminal old key delivered")
	}
}
func TestFreshMintedEnrollmentIsRecordedAndReused(t *testing.T) {
	f := fake()
	db, s, h := open(t, t.TempDir()+"/db", f)
	req := direct(t, s, "minted", teams.Fresh, "")
	f.failOnce("launch")
	if _, err := h.Provision(ctx, req); !errors.Is(err, errLost) {
		t.Fatal(err)
	}
	var payload []byte
	must(t, db.DB().QueryRow(`SELECT enrollment FROM team_host_intents WHERE intent_key=?`, req.IdempotencyKey).Scan(&payload))
	var saved teamhost.Enrollment
	must(t, json.Unmarshal(payload, &saved))
	if f.ensureRequests[req.IdempotencyKey].Actor != "" || saved.Actor == "" {
		t.Fatal("host attempted to choose fresh identity")
	}
	f.before = func(op, key string) error {
		if op == "ensure" {
			t.Fatal("recorded enrollment was not reused")
		}
		return nil
	}
	member, err := h.Provision(ctx, req)
	must(t, err)
	if member.Actor != saved.Actor {
		t.Fatal("retry actor changed")
	}
}

func TestFreshReturnedActorCannotUseReservedIdentity(t *testing.T) {
	f := fake()
	f.unfenced = true
	_, s, h := open(t, t.TempDir()+"/db", f)
	req := direct(t, s, "reserved-minted", teams.Fresh, "")
	req.ReservedIdentities = []mesh.URN{mesh.URN("msg://agent/team/" + req.IdempotencyKey)}
	if _, err := h.Provision(ctx, req); !errors.Is(err, teams.ErrProvisionFailed) {
		t.Fatal("minted reserved actor accepted", err)
	}
	if len(f.launches) != 0 || len(f.bindings) != 0 {
		t.Fatal("reserved actor acquired session or binding")
	}
}

type changedEnroller struct {
	teamhost.Enroller
	change func(teamhost.Enrollment) teamhost.Enrollment
}

func (e changedEnroller) Ensure(ctx context.Context, r teamhost.EnrollmentRequest) (teamhost.Enrollment, error) {
	value, err := e.Enroller.Ensure(ctx, r)
	if err != nil {
		return value, err
	}
	return e.change(value), nil
}
func TestHostRejectsChangedStableActorAndEphemeralContract(t *testing.T) {
	for _, tc := range []string{"stable-actor", "fresh-ephemeral", "stable-ephemeral"} {
		t.Run(tc, func(t *testing.T) {
			f := fake()
			f.unfenced = true
			db, s, _ := open(t, t.TempDir()+"/db", f)
			resolution, actor := teams.Fresh, mesh.URN("")
			if tc != "fresh-ephemeral" {
				resolution = teams.Durable
				actor = "msg://agent/local/stable"
				f.enrolled[actor] = true
			}
			req := direct(t, s, tc, resolution, actor)
			enroller := changedEnroller{f, func(e teamhost.Enrollment) teamhost.Enrollment {
				if tc == "stable-actor" {
					e.Actor = "msg://agent/local/other"
				} else {
					e.Ephemeral = !e.Ephemeral
				}
				return e
			}}
			h, err := teamhost.New(db.DB(), s, teamhost.Ports{Sessions: f, Enroller: enroller, Messenger: f, Channels: f}, options())
			must(t, err)
			if _, err = h.Provision(ctx, req); !errors.Is(err, teams.ErrProvisionFailed) {
				t.Fatal("invalid returned enrollment admitted", err)
			}
			if len(f.launches) != 0 || len(f.bindings) != 0 {
				t.Fatal("invalid enrollment acquired resources")
			}
		})
	}
}
func TestProvisionRejectsEnrollmentChangedWhileEnsureAcknowledges(t *testing.T) {
	f := fake()
	f.unfenced = true
	db, s, h := open(t, t.TempDir()+"/db", f)
	req := direct(t, s, "changed-enrollment", teams.Fresh, "")
	f.hook = func(op, key string) error {
		if op == "ensure" {
			other := f.enrollments[key]
			other.SpawnCapable = !other.SpawnCapable
			raw, err := json.Marshal(other)
			if err != nil {
				return err
			}
			_, err = db.DB().Exec(`UPDATE team_host_intents SET enrollment=? WHERE intent_key=?`, raw, key)
			return err
		}
		return nil
	}
	if _, err := h.Provision(ctx, req); !errors.Is(err, teams.ErrConflict) {
		t.Fatal("concurrent changed enrollment overwritten", err)
	}
	if len(f.bindings) != 0 || len(f.launches) != 0 {
		t.Fatal("changed enrollment acquired resources")
	}
}
func TestHostStoppedRecipientMatchesBothActorAndSession(t *testing.T) {
	for _, field := range []string{"actor", "session", "pending-cleanup"} {
		t.Run(field, func(t *testing.T) {
			f := fake()
			f.unfenced = true
			db, s, h := open(t, t.TempDir()+"/db", f)
			d, worker := delegation(t, s, h)
			must(t, h.SendMessage(ctx, d))
			other := worker
			switch field {
			case "actor":
				other.Actor = "msg://agent/local/other"
			case "session":
				other.SessionID = "different-session"
			}
			raw, err := json.Marshal(other)
			must(t, err)
			_, err = db.DB().Exec(`UPDATE team_host_intents SET member=?,tombstone='retire',cleaned=? WHERE intent_key=?`, raw, field != "pending-cleanup", worker.Intent.IdempotencyKey)
			must(t, err)
			calls := 0
			f.before = func(op, key string) error {
				if op == "deliver" {
					calls++
					return teams.ErrUnavailable
				}
				return nil
			}
			if err = h.FlushMessages(ctx, 10); !errors.Is(err, teams.ErrUnavailable) || errors.Is(err, teamhost.ErrSessionGone) {
				t.Fatal("unrelated cleanup ended retained recipient", err)
			}
			if calls != 1 {
				t.Fatal("retained recipient not attempted", calls)
			}
		})
	}
}

func TestHostRefusesNonAgentEnrollmentEvenWithMatchingActorKind(t *testing.T) {
	f := fake()
	f.unfenced = true
	db, s, _ := open(t, t.TempDir()+"/db", f)
	req := direct(t, s, "wrong-kind", teams.Fresh, "")
	e := changedEnroller{f, func(value teamhost.Enrollment) teamhost.Enrollment {
		value.Kind = mesh.ActorUser
		value.Actor = "msg://user/local/foreign"
		return value
	}}
	h, err := teamhost.New(db.DB(), s, teamhost.Ports{Sessions: f, Enroller: e, Messenger: f, Channels: f}, options())
	must(t, err)
	if _, err = h.Provision(ctx, req); !errors.Is(err, teams.ErrProvisionFailed) {
		t.Fatal("non-agent enrollment admitted", err)
	}
	if len(f.bindings) != 0 || len(f.launches) != 0 {
		t.Fatal("non-agent acquired resources")
	}
}

func TestIntentClockSkewClampAndMixedKindDeadLetterCursor(t *testing.T) {
	f := fake()
	db, s, _ := open(t, t.TempDir()+"/db", f)
	clock := time.Now()
	h := timedHost(t, db, s, f, &clock)
	req := direct(t, s, "intent-skew", teams.Fresh, "")
	member, err := h.Provision(ctx, req)
	must(t, err)
	f.before = func(op, key string) error {
		if op == "stop" {
			return teams.ErrUnavailable
		}
		return nil
	}
	if err = h.Retire(ctx, "retire", member); !errors.Is(err, teams.ErrUnavailable) {
		t.Fatal(err)
	}
	_, err = db.DB().Exec(`UPDATE team_host_intents SET next_attempt_at=? WHERE intent_key=?`, clock.Add(365*24*time.Hour).UnixNano(), req.IdempotencyKey)
	must(t, err)
	must(t, h.ReconcileIntents(ctx, 10))
	var due int64
	must(t, db.DB().QueryRow(`SELECT next_attempt_at FROM team_host_intents WHERE intent_key=?`, req.IdempotencyKey).Scan(&due))
	if due != clock.Add(time.Minute).UnixNano() {
		t.Fatal("clock skew hid intent", time.Unix(0, due))
	}
	clock = clock.Add(time.Minute)
	f.before = nil
	must(t, h.ReconcileIntents(ctx, 10))
	var cleaned bool
	must(t, db.DB().QueryRow(`SELECT cleaned FROM team_host_intents WHERE intent_key=?`, req.IdempotencyKey).Scan(&cleaned))
	if !cleaned {
		t.Fatal("clamped intent not cleaned")
	}

	for _, key := range []string{"letter1", "letter2", "letter3"} {
		_, err = db.DB().Exec(`INSERT INTO team_host_deliveries(delivery_key,payload,dead) VALUES(?,'{}',1)`, key)
		must(t, err)
	}
	_, err = db.DB().Exec(`INSERT INTO team_host_intents(intent_key,request,tombstone,dead) VALUES('intent-letter','{}','retire',1)`)
	must(t, err)
	cursor := teamhost.DeadLetterCursor{}
	for _, key := range []string{"letter1", "letter2", "letter3", "intent-letter"} {
		page, err := h.ListDeadLetters(ctx, cursor, 1)
		must(t, err)
		if len(page) != 1 || page[0].Key != key {
			t.Fatal("kind cursor skipped or repeated", key, page)
		}
		cursor = teamhost.DeadLetterCursor{Kind: page[0].Kind, Sequence: page[0].Sequence}
	}
	end, err := h.ListDeadLetters(ctx, cursor, 1)
	must(t, err)
	if len(end) != 0 {
		t.Fatal("mixed cursor repeated row", end)
	}
}
