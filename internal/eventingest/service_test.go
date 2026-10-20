package eventingest_test

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hollis-labs/libs/harness/core/adapters/runtimeevents/ingest"

	"github.com/hollis-labs/tether/internal/eventingest"
	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/store"
)

type fixture struct {
	svc *eventingest.Service
	db  *store.Store
	bus events.Bus
}

func newFixture(t *testing.T, cfg eventingest.Config) fixture {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "ingest.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	bus := events.NewBus(events.BusOptions{Persister: db})
	svc, err := eventingest.New(bus, db, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return fixture{svc: svc, db: db, bus: bus}
}

func raw(id, app, kind string, mut func(map[string]any)) json.RawMessage {
	m := map[string]any{
		"schema_version": "1", "id": id, "kind": kind, "time": time.Now().UTC().Format(time.RFC3339Nano),
		"app": app, "sequence": 1, "generation": 1,
		"source":  map[string]any{"channel": "test", "confidence": "exact"},
		"subject": "msg://task/" + app + "/T-1", "payload": map[string]any{"entity": map[string]any{"id": "T-1"}, "to": "doing"},
	}
	if mut != nil {
		mut(m)
	}
	b, _ := json.Marshal(m)
	return b
}

var operator = eventingest.Caller{Key: "op", Actor: events.Actor{Kind: "user", URN: "msg://user/local/operator", Verified: "token", Role: "operator"}}

func TestAcceptThenDuplicateThenConflict(t *testing.T) {
	f := newFixture(t, eventingest.Config{AllowUnknownKinds: true})
	ctx := context.Background()
	id := "evt_00000000000000000000000000000c01"

	res, err := f.svc.Ingest(ctx, operator, []json.RawMessage{raw(id, "torque", "task.state_changed", nil)})
	if err != nil || len(res) != 1 || res[0].Status != ingest.StatusAccepted || res[0].Cursor == "" {
		t.Fatalf("first: %+v %v", res, err)
	}
	first := res[0].Cursor

	// A retry of the same event is a duplicate carrying the original cursor,
	// even though only the time field differs.
	retry := raw(id, "torque", "task.state_changed", nil)
	res, err = f.svc.Ingest(ctx, operator, []json.RawMessage{retry})
	if err != nil || res[0].Status != ingest.StatusDuplicate || res[0].Cursor != first {
		t.Fatalf("retry: %+v %v", res, err)
	}

	// Same id, different payload: conflict, nothing stored.
	other := raw(id, "torque", "task.state_changed", func(m map[string]any) { m["payload"] = map[string]any{"to": "done"} })
	res, err = f.svc.Ingest(ctx, operator, []json.RawMessage{other})
	if err != nil || res[0].Status != ingest.StatusRejected || res[0].Code != ingest.CodeIDConflict || res[0].Cursor != "" {
		t.Fatalf("conflict: %+v %v", res, err)
	}
	got, ok, _ := f.db.EventByEventID(ctx, id)
	if !ok || strings.Contains(got.PayloadJSON, "done") {
		t.Fatalf("stored event changed: %+v", got)
	}
}

func TestPayloadKeyOrderIsNotAConflict(t *testing.T) {
	f := newFixture(t, eventingest.Config{})
	ctx := context.Background()
	id := "evt_00000000000000000000000000000c02"
	a := raw(id, "torque", "task.state_changed", func(m map[string]any) { m["payload"] = json.RawMessage(`{"a":1,"b":2}`) })
	b := raw(id, "torque", "task.state_changed", func(m map[string]any) { m["payload"] = json.RawMessage(`{ "b": 2, "a": 1 }`) })
	if _, err := f.svc.Ingest(ctx, operator, []json.RawMessage{a}); err != nil {
		t.Fatal(err)
	}
	res, _ := f.svc.Ingest(ctx, operator, []json.RawMessage{b})
	if res[0].Status != ingest.StatusDuplicate {
		t.Fatalf("%+v", res[0])
	}
}

func TestBatchResultsAreIndependentAndOrdered(t *testing.T) {
	f := newFixture(t, eventingest.Config{})
	items := []json.RawMessage{
		raw("evt_00000000000000000000000000000d01", "torque", "task.state_changed", nil),
		raw("evt_00000000000000000000000000000d02", "torque", "task.invented", nil), // unknown kind
		raw("evt_00000000000000000000000000000d03", "torque", "alert.raised", func(m map[string]any) { // fine
			m["subject"] = "msg://alert/torque/1"
		}),
		raw("evt_00000000000000000000000000000d04", "torque", "audit.recorded", nil), // torque does not own audit
		json.RawMessage(`{"id":"evt_00000000000000000000000000000d05"}`),             // invalid envelope
	}
	res, err := f.svc.Ingest(context.Background(), operator, items)
	if err != nil {
		t.Fatal(err)
	}
	want := []struct {
		st   ingest.Status
		code ingest.Code
	}{
		{ingest.StatusAccepted, ""}, {ingest.StatusRejected, ingest.CodeUnknownKind}, {ingest.StatusAccepted, ""},
		{ingest.StatusRejected, ingest.CodeForbiddenKind}, {ingest.StatusRejected, ingest.CodeSchemaInvalid},
	}
	for i, w := range want {
		if res[i].Status != w.st || res[i].Code != w.code {
			t.Errorf("item %d: %+v, want %v/%v", i, res[i], w.st, w.code)
		}
	}
	if resp := ingest.NewResponse(res); resp.Accepted != 2 || resp.Rejected != 3 {
		t.Errorf("%+v", resp)
	}
}

func TestUnknownKindsAcceptedOnlyInShadow(t *testing.T) {
	shadow := newFixture(t, eventingest.Config{AllowUnknownKinds: true})
	res, _ := shadow.svc.Ingest(context.Background(), operator, []json.RawMessage{raw("evt_00000000000000000000000000000e01", "torque", "task.invented", nil)})
	if res[0].Status != ingest.StatusAccepted {
		t.Fatalf("shadow: %+v", res[0])
	}
}

func TestAppBinding(t *testing.T) {
	f := newFixture(t, eventingest.Config{})
	cerberus := operator
	cerberus.App = "cerberus"
	_, err := f.svc.Ingest(context.Background(), cerberus, []json.RawMessage{
		raw("evt_00000000000000000000000000000f01", "cerberus", "alert.raised", func(m map[string]any) { m["subject"] = "msg://alert/cerberus/1" }),
		raw("evt_00000000000000000000000000000f02", "torque", "task.state_changed", nil),
	})
	if !errors.Is(err, eventingest.ErrAppMismatch) {
		t.Fatalf("err = %v", err)
	}
	// Nothing was written, not even the first, valid event.
	if _, ok, _ := f.db.EventByEventID(context.Background(), "evt_00000000000000000000000000000f01"); ok {
		t.Fatal("a request with a mismatched app wrote an event")
	}
}

func TestHubOwnsActorVisibilityAndEnvironmentDims(t *testing.T) {
	f := newFixture(t, eventingest.Config{})
	id := "evt_00000000000000000000000000001001"
	forged := raw(id, "torque", "task.state_changed", func(m map[string]any) {
		m["actor"] = map[string]any{"urn": "msg://user/local/operator", "kind": "user", "verified": "token"}
		m["scope"] = map[string]any{"task_id": "T-1", "environment_id": "forged-env", "worker_id": "forged-worker", "authority": "forged"}
		m["class"] = map[string]any{"category": "state", "severity": "info", "visibility": "mesh", "sanitized": false}
	})
	caller := eventingest.Caller{App: "torque", Key: "svc", Actor: events.Actor{Kind: "service", URN: "msg://service/torque/dev", Verified: "token", Role: "system"}}
	if _, err := f.svc.Ingest(context.Background(), caller, []json.RawMessage{forged}); err != nil {
		t.Fatal(err)
	}
	e, ok, err := f.db.EventByEventID(context.Background(), id)
	if err != nil || !ok {
		t.Fatalf("%v %v", ok, err)
	}
	env := e.Envelope
	if env.Actor.URN != "msg://service/torque/dev" || env.Actor.Verified != "token" {
		t.Errorf("actor = %+v", env.Actor)
	}
	if env.Class.Visibility != "environment" {
		t.Errorf("visibility = %q, a producer cannot widen it to mesh", env.Class.Visibility)
	}
	if env.Class.Category != "state" || env.Class.Severity != "info" || !env.Class.Sanitized {
		t.Errorf("class = %+v", env.Class)
	}
	if env.Dims.TaskID != "T-1" {
		t.Errorf("task dim lost: %+v", env.Dims)
	}
	if env.Dims.EnvironmentID == "forged-env" || env.Dims.WorkerID == "forged-worker" || env.Dims.Authority == "forged" {
		t.Errorf("producer set hub-owned dims: %+v", env.Dims)
	}
	if e.Scope != events.ScopeDaemon {
		t.Errorf("scope = %q", e.Scope)
	}
}

func TestSecretsAreScrubbedBeforeStorage(t *testing.T) {
	f := newFixture(t, eventingest.Config{})
	id := "evt_00000000000000000000000000001002"
	secret := "ghp_" + strings.Repeat("a1B2c3D4", 5)
	ev := raw(id, "torque", "data.emitted", func(m map[string]any) {
		m["subject"] = "msg://task/torque/T-1"
		m["payload"] = map[string]any{"note": "token " + secret, "password": "hunter2hunter2"}
	})
	res, err := f.svc.Ingest(context.Background(), operator, []json.RawMessage{ev})
	if err != nil || res[0].Status != ingest.StatusAccepted {
		t.Fatalf("%+v %v", res, err)
	}
	e, _, _ := f.db.EventByEventID(context.Background(), id)
	if strings.Contains(e.PayloadJSON, secret) || strings.Contains(e.PayloadJSON, "hunter2hunter2") {
		t.Fatalf("secret reached the log: %s", e.PayloadJSON)
	}
	if e.Envelope.Class.Redactions == 0 {
		t.Errorf("redactions not counted: %+v", e.Envelope.Class)
	}
}

func TestRateLimit(t *testing.T) {
	now := time.Now()
	f := newFixture(t, eventingest.Config{RatePerSecond: 1, Burst: 2, Now: func() time.Time { return now }})
	mk := func(n string) json.RawMessage {
		return raw("evt_0000000000000000000000000000"+n, "torque", "task.state_changed", nil)
	}
	if _, err := f.svc.Ingest(context.Background(), operator, []json.RawMessage{mk("2001"), mk("2002")}); err != nil {
		t.Fatal(err)
	}
	_, err := f.svc.Ingest(context.Background(), operator, []json.RawMessage{mk("2003")})
	var rl *eventingest.RateLimited
	if !errors.As(err, &rl) || rl.RetryAfter < time.Second {
		t.Fatalf("err = %v", err)
	}
	now = now.Add(3 * time.Second)
	if _, err := f.svc.Ingest(context.Background(), operator, []json.RawMessage{mk("2003")}); err != nil {
		t.Fatalf("after waiting: %v", err)
	}
}

func TestAcceptedEventReachesSubscribersAndDuplicateDoesNot(t *testing.T) {
	f := newFixture(t, eventingest.Config{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sub, stop, err := f.bus.Subscribe(ctx, events.Filter{SinceSeq: -1})
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	id := "evt_00000000000000000000000000003001"
	ev := raw(id, "torque", "task.state_changed", nil)
	for i := 0; i < 2; i++ {
		if _, err := f.svc.Ingest(ctx, operator, []json.RawMessage{ev}); err != nil {
			t.Fatal(err)
		}
	}
	got := 0
	for {
		select {
		case e := <-sub:
			if e.EventID == id {
				got++
			}
			continue
		case <-time.After(200 * time.Millisecond):
		}
		break
	}
	if got != 1 {
		t.Fatalf("subscriber saw the event %d times, want 1", got)
	}
}

func TestProducerSecretClaimIsWithheldNotRefused(t *testing.T) {
	f := newFixture(t, eventingest.Config{})
	id := "evt_00000000000000000000000000004001"
	ev := raw(id, "torque", "data.emitted", func(m map[string]any) {
		m["subject"] = "msg://task/torque/T-1"
		m["class"] = map[string]any{"sensitivity": "secret"}
		m["payload"] = map[string]any{"value": "something the producer called secret"}
	})
	res, err := f.svc.Ingest(context.Background(), operator, []json.RawMessage{ev})
	if err != nil || res[0].Status != ingest.StatusAccepted {
		t.Fatalf("%+v %v", res, err)
	}
	e, _, _ := f.db.EventByEventID(context.Background(), id)
	if strings.Contains(e.PayloadJSON, "something") || e.Envelope.Class.Sensitivity != "private" {
		t.Fatalf("stored %q as %q", e.PayloadJSON, e.Envelope.Class.Sensitivity)
	}
}

// A paired worker device is a host credential: no single app (App ""), so it
// can relay events for any registered app, but kind ownership is still checked
// against each event's own app, and the stored actor is the device.
func TestDeviceCallerPublishesForRegisteredAppsOnly(t *testing.T) {
	f := newFixture(t, eventingest.Config{})
	device := eventingest.Caller{Key: "msg://device/w1", Actor: events.Actor{Kind: "device", URN: "msg://device/w1", Verified: "token", Role: "system"}}
	ok := raw("evt_00000000000000000000000000005001", "cerberus", "alert.raised", func(m map[string]any) { m["subject"] = "msg://alert/cerberus/1" })
	notOwned := raw("evt_00000000000000000000000000005002", "cerberus", "task.state_changed", nil) // torque's kind
	noApp := raw("evt_00000000000000000000000000005003", "nobody", "alert.raised", func(m map[string]any) { m["subject"] = "msg://alert/nobody/1" })
	res, err := f.svc.Ingest(context.Background(), device, []json.RawMessage{ok, notOwned, noApp})
	if err != nil {
		t.Fatal(err)
	}
	if res[0].Status != ingest.StatusAccepted {
		t.Errorf("owned kind: %+v", res[0])
	}
	if res[1].Code != ingest.CodeForbiddenKind || res[2].Code != ingest.CodeForbiddenKind {
		t.Errorf("not owned %+v, unregistered %+v", res[1], res[2])
	}
	e, found, _ := f.db.EventByEventID(context.Background(), "evt_00000000000000000000000000005001")
	if !found || e.Envelope.Actor.Kind != "device" || e.Envelope.Actor.URN != "msg://device/w1" || e.Envelope.Actor.Verified != "token" || e.Envelope.App != "cerberus" {
		t.Fatalf("stored actor/app: %+v", e.Envelope)
	}
}

// The lifecycle kinds added in harness/core v0.14.1 are admitted for the apps
// that own them (Cerberus resources and deploys) and not for others.
func TestLifecycleKindsAreAdmittedForTheirOwners(t *testing.T) {
	f := newFixture(t, eventingest.Config{})
	res, err := f.svc.Ingest(context.Background(), operator, []json.RawMessage{
		raw("evt_00000000000000000000000000006001", "cerberus", "resource.state_changed", func(m map[string]any) {
			m["subject"] = "msg://resource/cerberus/hub-hadron"
			m["payload"] = map[string]any{"entity": map[string]any{"type": "resource", "id": "hub-hadron"}, "from": "running", "to": "wedged", "trigger": map[string]any{"kind": "system"}}
		}),
		raw("evt_00000000000000000000000000006002", "cerberus", "deploy.state_changed", func(m map[string]any) {
			m["subject"] = "msg://deployment/cerberus/d1"
			m["payload"] = map[string]any{"entity": map[string]any{"type": "deployment", "id": "d1"}, "resource": map[string]any{"id": "hub-hadron"}, "to": "started", "trigger": map[string]any{"kind": "human"}}
		}),
		raw("evt_00000000000000000000000000006003", "tesseract", "resource.state_changed", func(m map[string]any) {
			m["subject"] = "msg://resource/tesseract/x"
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if res[0].Status != ingest.StatusAccepted || res[1].Status != ingest.StatusAccepted {
		t.Fatalf("owners: %+v %+v", res[0], res[1])
	}
	if res[2].Code != ingest.CodeForbiddenKind {
		t.Fatalf("non-owner: %+v", res[2])
	}
	e, _, _ := f.db.EventByEventID(context.Background(), "evt_00000000000000000000000000006001")
	if !strings.Contains(e.PayloadJSON, `"to":"wedged"`) {
		t.Fatalf("payload lost declared fields: %s", e.PayloadJSON)
	}
}
