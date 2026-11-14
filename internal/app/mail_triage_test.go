package app

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hollis-labs/libs/harness/core/adapters/agentsessions"
	"github.com/hollis-labs/libs/mesh/core/messaging"
	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/messaging/envelope"
	"github.com/hollis-labs/tether/internal/registry"
	"github.com/hollis-labs/tether/internal/store"
)

type triageRig struct {
	t      *testing.T
	st     *store.Store
	rt     *fakeRuntime
	seam   wakeRuntime
	to     messaging.Address
	mu     sync.Mutex
	events []events.Event
	now    time.Time
	tri    *mailTriage
	park   wakeParkSet
}

func newTriageRig(t *testing.T) *triageRig {
	t.Helper()
	st, _ := newWakeHarness(t)
	r := &triageRig{t: t, st: st, rt: newFakeRuntime(), to: messaging.Address{Kind: messaging.KindAgent, Authority: "agent-mux", ID: "worker"},
		now: time.Date(2026, 11, 14, 8, 0, 0, 0, time.UTC)}
	if err := st.CreateSession(store.SessionRow{ID: "live", LogicalAgentID: "worker", State: "running"}, nil); err != nil {
		t.Fatal(err)
	}
	r.rt.setAlive("live", true, agentsessions.LiveStateIdle)
	r.tri = &mailTriage{st: st, now: func() time.Time { return r.now },
		policy: func(to messaging.Address) seatPolicy {
			return seatPolicy{seat: r.to.URN(), handle: "@worker", enabled: true, interval: 3 * time.Minute}
		},
		publish: func(_ context.Context, e events.Event) error {
			r.mu.Lock()
			r.events = append(r.events, e)
			r.mu.Unlock()
			return nil
		}}
	r.seam = r.rt.seam()
	r.seam.triage = r.tri
	return r
}

func (r *triageRig) send(from string, kind messaging.Kind, contentType string, payload any, meta map[string]string) messaging.Envelope {
	r.t.Helper()
	raw, _ := json.Marshal(payload)
	f, _ := messaging.ParseURN(from)
	env, err := r.st.MessagingStore().Send(context.Background(), messaging.Envelope{Kind: kind, From: f, To: r.to, ContentType: contentType, Payload: raw, Metadata: meta})
	if err != nil {
		r.t.Fatal(err)
	}
	return env
}

func (r *triageRig) wake(env messaging.Envelope) string {
	r.t.Helper()
	return recipientWake(context.Background(), r.st, nil, r.seam, env).Reason
}

func (r *triageRig) triaged() map[string]int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := map[string]int{}
	for _, e := range r.events {
		if e.Kind == KindMailTriaged {
			var p struct{ Class string }
			_ = json.Unmarshal([]byte(e.PayloadJSON), &p)
			n[p.Class]++
		}
	}
	return n
}

const peer = "msg://agent/agent-mux/agt_peer"

// Acceptance: 10 FYI mails produce 0 turns (at send and through the sweep);
// one action mail produces 1 wake; every message gets a mail.triaged event.
func TestMailTriageFYIDoesNotWakeAndActionWakesOnce(t *testing.T) {
	r := newTriageRig(t)
	for i := 0; i < 10; i++ {
		env := r.send(peer, messaging.MsgKindNotice, "", map[string]string{"body": "fyi"}, nil)
		if reason := r.wake(env); reason != "triaged-fyi" {
			t.Fatalf("fyi %d: reason %q", i, reason)
		}
	}
	if _, err := runWakeSweep(context.Background(), r.st, nil, r.seam, &r.park); err != nil {
		t.Fatal(err)
	}
	if n := r.rt.sendCallCount("live"); n != 0 {
		t.Fatalf("10 fyi mails produced %d turns", n)
	}
	action := r.send(peer, messaging.MsgKindRequest, "", map[string]string{"body": "please"}, nil)
	r.wake(action)
	if n := r.rt.sendCallCount("live"); n != 1 {
		t.Fatalf("one action mail produced %d wakes", n)
	}
	if got := r.triaged(); got["fyi"] != 10 || got["action"] != 1 {
		t.Fatalf("mail.triaged events = %v", got)
	}
}

// The interrupt cap: a second action within the interval is deferred, a third
// is covered by the wake that ends the deferral; human mail is never capped.
func TestMailTriageCapDefersCoversAndHumanBypasses(t *testing.T) {
	r := newTriageRig(t)
	a := r.send(peer, messaging.MsgKindRequest, "", map[string]string{"body": "a"}, nil)
	if reason := r.wake(a); reason != "" || r.rt.sendCallCount("live") != 1 {
		t.Fatalf("first action: %q, %d turns", reason, r.rt.sendCallCount("live"))
	}
	r.now = r.now.Add(time.Minute)
	b := r.send(peer, messaging.MsgKindRequest, "", map[string]string{"body": "b"}, nil)
	c := r.send(peer, messaging.MsgKindEscalation, "", map[string]string{"body": "c"}, nil)
	for _, env := range []messaging.Envelope{b, c} {
		if reason := r.wake(env); reason != "deferred-cap" {
			t.Fatalf("action within the interval: %q", reason)
		}
	}
	if d := r.tri.gate(context.Background(), b); d.wake || !d.until.Equal(r.now.Add(2*time.Minute)) {
		t.Fatalf("deferral = %+v", d)
	}
	// Human mail bypasses the cap.
	h := r.send(triageOperator, messaging.MsgKindNotice, "", map[string]string{"body": "now"}, nil)
	if reason := r.wake(h); reason != "" || r.rt.sendCallCount("live") != 2 {
		t.Fatalf("human: %q, %d turns", reason, r.rt.sendCallCount("live"))
	}
	// The human wake covered the pending actions: they never wake separately.
	r.now = r.now.Add(5 * time.Minute)
	if _, err := runWakeSweep(context.Background(), r.st, nil, r.seam, &r.park); err != nil {
		t.Fatal(err)
	}
	if n := r.rt.sendCallCount("live"); n != 2 {
		t.Fatalf("covered actions woke again: %d turns", n)
	}
	for _, env := range []messaging.Envelope{b, c} {
		row, _, _ := r.st.TriageGet(context.Background(), env.ID)
		if row.State != store.TriageCovered {
			t.Fatalf("%s state %q, want covered", env.ID, row.State)
		}
	}
}

const triageOperator = "msg://user/local/operator"

// A deferred action wakes once its interval has passed (through the sweep).
func TestMailTriageDeferredActionWakesAfterTheInterval(t *testing.T) {
	r := newTriageRig(t)
	r.wake(r.send(peer, messaging.MsgKindRequest, "", map[string]string{"body": "a"}, nil))
	r.now = r.now.Add(time.Minute)
	b := r.send(peer, messaging.MsgKindRequest, "", map[string]string{"body": "b"}, nil)
	if reason := r.wake(b); reason != "deferred-cap" {
		t.Fatalf("b: %q", reason)
	}
	if _, err := runWakeSweep(context.Background(), r.st, nil, r.seam, &r.park); err != nil {
		t.Fatal(err)
	}
	if n := r.rt.sendCallCount("live"); n != 1 {
		t.Fatalf("deferred action woke early: %d", n)
	}
	r.now = r.now.Add(3 * time.Minute)
	r.park = wakeParkSet{} // the in-memory park would hold it until the real clock passes
	if _, err := runWakeSweep(context.Background(), r.st, nil, r.seam, &r.park); err != nil {
		t.Fatal(err)
	}
	if n := r.rt.sendCallCount("live"); n != 2 {
		t.Fatalf("deferred action after the interval: %d turns, want 2", n)
	}
}

// A typed attention=none envelope is read by the hub at once; untyped
// very-low mail is left unread.
func TestMailTriageReadClass(t *testing.T) {
	r := newTriageRig(t)
	quiet := r.send(peer, messaging.MsgKindNotice, envelope.ContentType, envelope.Envelope{Schema: envelope.Schema, Kind: envelope.KindUpdate, To: []string{"@worker"}, Attention: envelope.AttentionNone, Body: "build green"}, nil)
	if reason := r.wake(quiet); reason != "triaged-read" {
		t.Fatalf("attention none: %q", reason)
	}
	if !r.isRead(quiet.ID) {
		t.Fatal("attention=none was not marked read")
	}
	low := r.send(peer, messaging.MsgKindNotice, "", map[string]string{"body": "x"}, map[string]string{"urgency": "very-low"})
	if reason := r.wake(low); reason != "triaged-read" {
		t.Fatalf("very-low: %q", reason)
	}
	if r.isRead(low.ID) {
		t.Fatal("untyped very-low was marked read")
	}
	if r.rt.sendCallCount("live") != 0 {
		t.Fatal("read-class mail woke the seat")
	}
}

// FYI rides along on a human turn: one line naming the senders and subjects;
// the commit marks it surfaced and typed fyi read; a second line is empty.
func TestMailTriageFYILine(t *testing.T) {
	r := newTriageRig(t)
	typedFYI := r.send(peer, messaging.MsgKindNotice, envelope.ContentType, envelope.Envelope{Schema: envelope.Schema, Kind: envelope.KindUpdate, To: []string{"@worker"}, Attention: envelope.AttentionAgent, Subject: "Deploy done", Body: "b"}, nil)
	untyped := r.send(peer, messaging.MsgKindNotice, "", map[string]string{"subject": "Heads up", "body": "x"}, nil)
	r.wake(typedFYI)
	r.wake(untyped)
	pol := seatPolicy{seat: r.to.URN(), handle: "@worker", enabled: true}
	line, commit := r.tri.fyiLine(context.Background(), pol)
	if !strings.Contains(line, "FYI: 2 unread mail(s)") || !strings.Contains(line, "@agt_peer: Deploy done") || !strings.Contains(line, "Heads up") {
		t.Fatalf("line = %q", line)
	}
	commit()
	if again, _ := r.tri.fyiLine(context.Background(), pol); again != "" {
		t.Fatalf("surfaced fyi listed again: %q", again)
	}
	if !r.isRead(typedFYI.ID) {
		t.Fatal("typed fyi not marked read after surfacing")
	}
	if r.isRead(untyped.ID) {
		t.Fatal("untyped fyi marked read")
	}
}

// Triage off (the default): nothing changes.
func TestMailTriageOffWakesAsBefore(t *testing.T) {
	r := newTriageRig(t)
	r.tri.policy = func(messaging.Address) seatPolicy { return seatPolicy{} }
	if reason := r.wake(r.send(peer, messaging.MsgKindNotice, "", map[string]string{"body": "fyi"}, nil)); reason != "" || r.rt.sendCallCount("live") != 1 {
		t.Fatalf("triage off: %q, %d turns", reason, r.rt.sendCallCount("live"))
	}
	if len(r.triaged()) != 0 {
		t.Fatal("events emitted with triage off")
	}
}

func TestFYINeverRidesOnASlashCommand(t *testing.T) {
	ctx := withTurnInput(context.Background(), turnInputNote{Source: store.InputChatSend})
	if !fyiRidesAlong(ctx, "what's next?") {
		t.Fatal("a human prompt should carry the fyi line")
	}
	for _, cmd := range []string{"/clear", "/compact", "  /model opus", "/"} {
		if fyiRidesAlong(ctx, cmd) {
			t.Fatalf("fyi appended to %q", cmd)
		}
	}
}

func TestHumanOriginated(t *testing.T) {
	ctx := context.Background()
	if !humanOriginated(withTurnInput(ctx, turnInputNote{Source: store.InputChatSend})) {
		t.Fatal("chat send is human")
	}
	if humanOriginated(withTurnInput(ctx, turnInputNote{Source: store.InputMailWake, Class: store.ClassOperator})) {
		t.Fatal("a mail wake is never human")
	}
	if !humanOriginated(withTurnInput(ctx, turnInputNote{Source: store.InputSessionTurn, Class: store.ClassOperator})) {
		t.Fatal("an operator turn is human")
	}
	if humanOriginated(withTurnInput(ctx, turnInputNote{Source: store.InputMCPSendTurn, Class: store.ClassAgent})) {
		t.Fatal("an agent turn is not human")
	}
}

func (r *triageRig) isRead(id string) bool {
	r.t.Helper()
	var readAt *string
	if err := r.st.DB().QueryRow(`SELECT read_at FROM messages WHERE id=?`, id).Scan(&readAt); err != nil {
		r.t.Fatal(err)
	}
	return readAt != nil
}

// Senders are named by handle: catalog agents by handle:/id, other seats by
// their registry handle (archie is not a catalog agent), never as @agt_....
func TestSenderHandleUsesCatalogThenRegistry(t *testing.T) {
	st, reg := newWakeHarness(t)
	ctx := context.Background()
	archie, err := reg.Register(ctx, registry.KindAgent, registry.Profile{DisplayName: "Archie"})
	if err != nil {
		t.Fatal(err)
	}
	if err := reg.AttachExternalID(ctx, archie.URN, "handle", "archie"); err != nil {
		t.Fatal(err)
	}
	s := &Service{Store: st, Registry: reg, Catalog: &config.Catalog{Agents: map[string]config.Agent{
		"cmd-builder-1": {ID: "cmd-builder-1", Handle: "builder-1", URN: "msg://agent/agent-mux/agt_b1"},
		"photon-tether": {ID: "photon-tether"},
	}}}
	for urn, want := range map[string]string{
		archie.URN:                            "@archie",
		"msg://agent/agent-mux/agt_b1":        "@builder-1",
		"msg://agent/agent-mux/photon-tether": "@photon-tether",
		triageOperator:                        "@chrispian",
		"msg://agent/agent-mux/agt_unknown":   "",
	} {
		if got := s.senderHandle(ctx, urn); got != want {
			t.Errorf("senderHandle(%s) = %q, want %q", urn, got, want)
		}
	}
	// A caller-registered id never makes a sender render as the operator or as
	// an invalid handle; the sender then falls back to its address segment.
	for i, spoofID := range []string{"chrispian", "Bad Handle", "@x"} {
		sp, err := reg.Register(ctx, registry.KindAgent, registry.Profile{DisplayName: fmt.Sprintf("Spoof %d", i)})
		if err != nil {
			t.Fatal(err)
		}
		if err := reg.AttachExternalID(ctx, sp.URN, "tether", spoofID); err != nil {
			t.Fatal(err)
		}
		if got := s.senderHandle(ctx, sp.URN); got != "" {
			t.Errorf("tether id %q named the sender %q", spoofID, got)
		}
	}
	// The triage uses it for the fyi line and the event.
	r := newTriageRig(t)
	r.tri.handle = func(_ context.Context, urn string) string {
		if urn == peer {
			return "@archie"
		}
		return ""
	}
	r.wake(r.send(peer, messaging.MsgKindNotice, "", map[string]string{"subject": "Heads up"}, nil))
	line, _ := r.tri.fyiLine(ctx, seatPolicy{seat: r.to.URN(), handle: "@worker", enabled: true})
	if !strings.Contains(line, "@archie: Heads up") || strings.Contains(line, "@agt_peer") {
		t.Fatalf("fyi line = %q", line)
	}
	r.mu.Lock()
	payload := r.events[0].PayloadJSON
	r.mu.Unlock()
	if !strings.Contains(payload, `"from_handle":"@archie"`) {
		t.Fatalf("mail.triaged payload = %s", payload)
	}
}
