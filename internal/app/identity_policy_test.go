package app

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/hollis-labs/substrate/harness/adapters/agentsessions"
	messaging "github.com/hollis-labs/substrate/mesh/messaging"
	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/app/turnrouting"
	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/launch"
	"github.com/hollis-labs/tether/internal/launchprofile"
	"github.com/hollis-labs/tether/internal/messaging/channels"
	"github.com/hollis-labs/tether/internal/store"
)

type auditSink struct {
	mu   sync.Mutex
	rows []identity.Observation
}

func (a *auditSink) record(_ context.Context, o identity.Observation) error {
	a.mu.Lock()
	a.rows = append(a.rows, o)
	a.mu.Unlock()
	return nil
}

func (a *auditSink) events() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []string
	for _, r := range a.rows {
		out = append(out, r.Event)
	}
	return out
}

func verifiedPrincipal(id string, scopes ...string) identity.Principal {
	return identity.Principal{Verified: true, ID: id, Kind: "user", Scopes: scopes}
}

func replyReq(parent string, caller identity.Principal) api.RoutingReplyRequest {
	return api.RoutingReplyRequest{ParentID: parent, Body: "answer", Caller: caller, Verified: caller.Verified}
}

const (
	sessionOwner = "msg://session/local/s1"
	strangerID   = "msg://user/local/mallory"
)

func TestReplyAuthorizationEnforceRefusesNonParticipant(t *testing.T) {
	h := newReplyHarness(t)
	parent := h.routed("s1")
	h.rt.setAlive("s1", true, agentsessions.LiveStateProcessing)
	sink := &auditSink{}
	h.svc.InstallIdentityPolicy(NewIdentityPolicy(identity.Enforce, sink.record, turnrouting.ServiceURN))
	ctx := context.Background()

	if _, err := h.svc.SubmitRoutingReply(ctx, replyReq(parent.ID, verifiedPrincipal(strangerID))); !errors.Is(err, api.ErrReplyForbidden) {
		t.Fatalf("non-participant: %v", err)
	}
	if _, err := h.svc.SubmitRoutingReply(ctx, replyReq(parent.ID, identity.Principal{ID: sessionOwner})); !errors.Is(err, api.ErrReplyForbidden) {
		t.Fatalf("unverified claim of participant identity: %v", err)
	}
	if _, err := h.svc.SubmitRoutingReply(ctx, replyReq(parent.ID, verifiedPrincipal("msg://session/local/s1", ""))); err != nil {
		t.Fatalf("sender session: %v", err)
	}
	if _, err := h.svc.SubmitRoutingReply(ctx, replyReq(parent.ID, verifiedPrincipal("msg://user/local/operator", "*"))); err != nil {
		t.Fatalf("operator: %v", err)
	}
	if _, err := h.svc.SubmitRoutingReply(ctx, replyReq(parent.ID, verifiedPrincipal(strangerID, identity.ScopeSendAs))); err != nil {
		t.Fatalf("send_as holder: %v", err)
	}
	if rows, _ := h.st.RoutingRepliesInState(ctx, store.RoutingReplyQueued, 10); len(rows) != 3 {
		t.Fatalf("refused replies must not queue; queued = %d", len(rows))
	}
	if got := sink.events(); len(got) != 2 || got[0] != eventReplyRefused || got[1] != eventReplyRefused {
		t.Fatalf("audit = %v", got)
	}
}

func TestReplyAuthorizationObserveLogsAndAllows(t *testing.T) {
	h := newReplyHarness(t)
	parent := h.routed("s1")
	h.rt.setAlive("s1", true, agentsessions.LiveStateProcessing)
	sink := &auditSink{}
	h.svc.InstallIdentityPolicy(NewIdentityPolicy(identity.Observe, sink.record, turnrouting.ServiceURN))
	if _, err := h.svc.SubmitRoutingReply(context.Background(), replyReq(parent.ID, verifiedPrincipal(strangerID))); err != nil {
		t.Fatalf("observe must not refuse: %v", err)
	}
	if got := sink.events(); len(got) != 1 || got[0] != eventReplyUnauthorized {
		t.Fatalf("audit = %v", got)
	}
	if sink.rows[0].PrincipalID != strangerID || sink.rows[0].Mode != identity.Observe || sink.rows[0].Authentication != "verified" {
		t.Fatalf("row = %+v", sink.rows[0])
	}
}

func TestOffPolicyInstallsNothing(t *testing.T) {
	h := newReplyHarness(t)
	h.svc.InstallIdentityPolicy(NewIdentityPolicy(identity.Off, nil))
	if h.svc.ReplyAuthorization != nil || h.svc.DeliveryAuthorization != nil {
		t.Fatal("off mode installed a hook")
	}
}

func TestDeliveryReadGatedByParticipant(t *testing.T) {
	for _, mode := range []identity.Mode{identity.Enforce, identity.Observe} {
		t.Run(string(mode), func(t *testing.T) {
			h := newReplyHarness(t)
			parent := h.routed("s1")
			h.rt.setAlive("s1", true, agentsessions.LiveStateProcessing)
			receipt, err := h.svc.SubmitRoutingReply(context.Background(), replyReq(parent.ID, verifiedPrincipal("msg://user/local/operator", "*")))
			if err != nil {
				t.Fatal(err)
			}
			sink := &auditSink{}
			h.svc.InstallIdentityPolicy(NewIdentityPolicy(mode, sink.record, turnrouting.ServiceURN))
			read := func(p identity.Principal) error {
				_, err := h.svc.RoutingReplyDelivery(identity.WithPrincipal(context.Background(), p), receipt.ReplyID)
				return err
			}
			if err := read(verifiedPrincipal("msg://user/local/operator", "*")); err != nil {
				t.Fatalf("author/operator: %v", err)
			}
			if err := read(verifiedPrincipal("msg://session/local/s1")); err != nil {
				t.Fatalf("target session: %v", err)
			}
			err = read(verifiedPrincipal(strangerID))
			if mode == identity.Enforce {
				if !errors.Is(err, api.ErrReplyForbidden) {
					t.Fatalf("non-participant read: %v", err)
				}
				if got := sink.events(); len(got) != 1 || got[0] != eventDeliveryRefused {
					t.Fatalf("audit = %v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("observe refused: %v", err)
			}
			if got := sink.events(); len(got) != 1 || got[0] != eventDeliveryUnauthorized {
				t.Fatalf("audit = %v", got)
			}
		})
	}
}

// Channel publication: the turn router is authorized by an explicit rule and
// needs no scope; removing the rule blocks it in enforce.
func TestTurnRouterPublishRuleIsExplicit(t *testing.T) {
	cases := []struct {
		name      string
		mode      identity.Mode
		publisher []string
		wantBlock bool
	}{
		{"enforce with rule", identity.Enforce, []string{turnrouting.ServiceURN}, false},
		{"enforce rule removed", identity.Enforce, nil, true},
		{"observe rule removed", identity.Observe, nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			db, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = db.Close() })
			if err := db.CreateSession(store.SessionRow{ID: "routed", LaunchID: "launch", State: "running", CreatedAt: time.Now().UTC().Format(time.RFC3339Nano)},
				&launch.Plan{Route: &launchprofile.Route{Channel: "ops", Kinds: []string{"final"}}}); err != nil {
				t.Fatal(err)
			}
			sink := &auditSink{}
			svc := &Service{Store: db}
			svc.InstallIdentityPolicy(NewIdentityPolicy(c.mode, sink.record, c.publisher...))
			staged, err := db.StageTurnOutput(context.Background(), messaging.Envelope{
				From:    messaging.Address{Kind: messaging.KindSession, Authority: "local", ID: "routed"},
				Payload: []byte(`{"text":"answer"}`), ContentType: "application/json",
				Metadata: map[string]string{"session_id": "routed", "turn_id": uuid.NewString(), "kind": "final"}})
			if err != nil {
				t.Fatal(err)
			}
			bus := events.NewBus(events.BusOptions{Persister: db})
			router := turnrouting.New(db, bus, channels.New(db, nil))
			if err := router.Start(context.Background()); err != nil {
				t.Fatal(err)
			}
			defer router.Close()
			deadline := time.Now().Add(5 * time.Second)
			for {
				hist, err := db.ReadChannel(context.Background(), "ops", 0, 10)
				if err != nil {
					t.Fatal(err)
				}
				published := len(hist) == 1 && hist[0].ID == staged.ID
				if !c.wantBlock && published {
					return
				}
				if c.wantBlock && len(sink.events()) > 0 {
					if published {
						t.Fatal("publication committed despite enforce refusal")
					}
					if sink.events()[0] != eventChannelRefused {
						t.Fatalf("audit = %v", sink.events())
					}
					return
				}
				if time.Now().After(deadline) {
					t.Fatalf("published=%v audit=%v", published, sink.events())
				}
				time.Sleep(10 * time.Millisecond)
			}
		})
	}
}

func TestChannelPolicyForOtherCallers(t *testing.T) {
	policy := NewIdentityPolicy(identity.Enforce, nil, turnrouting.ServiceURN)
	check := policy.Channel()
	sess := messaging.Address{Kind: messaging.KindSession, Authority: "local", ID: "s1"}
	ctx := context.Background()
	if err := check(ctx, "publish", "ops", verifiedPrincipal(sess.URN()), sess); err != nil {
		t.Fatalf("verified owner: %v", err)
	}
	if err := check(ctx, "publish", "ops", verifiedPrincipal(strangerID), sess); !errors.Is(err, channels.ErrForbidden) {
		t.Fatalf("verified stranger publishing as session: %v", err)
	}
	// An unverified principal, even naming the session and carrying the
	// router's CreatedBy, has no rule without the process-local marker.
	forged := identity.Principal{ID: sess.URN(), Kind: "session", CreatedBy: turnrouting.ServiceURN}
	if err := check(ctx, "publish", "ops", forged, sess); !errors.Is(err, channels.ErrForbidden) {
		t.Fatalf("forged router attribution: %v", err)
	}
	marked := identity.WithInternalService(identity.WithPrincipal(ctx, forged), turnrouting.ServiceURN)
	if err := check(marked, "publish", "ops", forged, sess); err != nil {
		t.Fatalf("router: %v", err)
	}
	// The marker alone does not let an internal publisher speak as another address.
	other := messaging.Address{Kind: messaging.KindSession, Authority: "local", ID: "s2"}
	if err := check(marked, "publish", "ops", forged, other); !errors.Is(err, channels.ErrForbidden) {
		t.Fatalf("router publishing as another session: %v", err)
	}
	if err := check(ctx, "history", "ops", verifiedPrincipal(strangerID), messaging.Address{}); err != nil {
		t.Fatalf("verified read: %v", err)
	}
	if err := check(ctx, "history", "ops", identity.Principal{ID: strangerID}, messaging.Address{}); !errors.Is(err, channels.ErrForbidden) {
		t.Fatalf("unverified read: %v", err)
	}
}
