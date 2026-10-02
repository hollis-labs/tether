package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	gomsg "github.com/hollis-labs/go-messaging"
	"github.com/hollis-labs/go-messaging/delivery"
	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/launch"
	"github.com/hollis-labs/tether/internal/launchprofile"
	"github.com/hollis-labs/tether/internal/messaging/channels"
	"github.com/hollis-labs/tether/internal/store"
)

func channelStage(t *testing.T) (*store.Store, gomsg.Envelope, channels.ExistingMessage) {
	t.Helper()
	db := openRetentionDB(t)
	err := db.CreateSession(store.SessionRow{ID: "routed", LaunchID: "ops-launch", State: "running", CreatedAt: time.Now().UTC().Format(time.RFC3339Nano)}, &launch.Plan{Route: &launchprofile.Route{Channel: "ops", Kinds: []string{"final"}}})
	if err != nil {
		t.Fatal(err)
	}
	env, err := db.StageTurnOutput(context.Background(), gomsg.Envelope{From: gomsg.Address{Kind: gomsg.KindSession, Authority: "local", ID: "routed"}, Payload: []byte(`{"text":"full reply"}`), ContentType: "application/json", Metadata: map[string]string{"session_id": "routed", "turn_id": "turn-1", "kind": "final", "confidence": "exact", "runtime": "claude", "logical_agent_id": "agent", "project_id": "project", "workstream_id": "work"}})
	if err != nil {
		t.Fatal(err)
	}
	return db, env, channels.ExistingMessage{MessageID: env.ID, Channel: "ops", SessionID: "routed", Actor: gomsg.Address{Kind: gomsg.KindService, Authority: "local", ID: "turn-router"}}
}

func TestAttachExistingChannelMessageOnceWithoutDelivery(t *testing.T) {
	db, staged, req := channelStage(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	subscriber, err := db.MessagingStore().Subscribe(ctx, gomsg.Address{}, gomsg.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	service := channels.New(db, nil)
	for range 2 {
		env, err := service.AttachExisting(ctx, req)
		if err != nil {
			t.Fatal(err)
		}
		if env.ID != staged.ID || string(env.Payload) != string(staged.Payload) || !env.CreatedAt.Equal(staged.CreatedAt) || env.From != staged.From || env.ThreadID != "routed" {
			t.Fatalf("envelope copied or changed: %+v", env)
		}
		if env.Metadata["workstream_id"] != "work" || env.Metadata["launch_display_name"] != "ops-launch" {
			t.Fatalf("metadata lost: %+v", env.Metadata)
		}
	}
	history, err := db.ReadChannel(ctx, "ops", 0, 100)
	if err != nil || len(history) != 1 || history[0].ID != staged.ID {
		t.Fatalf("history: %+v %v", history, err)
	}
	var bodies, publications int
	if err := db.DB().QueryRow(`SELECT count(*) FROM messages`).Scan(&bodies); err != nil {
		t.Fatal(err)
	}
	if err := db.DB().QueryRow(`SELECT count(*) FROM channel_publications`).Scan(&publications); err != nil {
		t.Fatal(err)
	}
	if bodies != 1 || publications != 1 {
		t.Fatalf("copies: bodies=%d publications=%d", bodies, publications)
	}
	if _, err := db.StagedTurnOutput(ctx, staged.ID); !errors.Is(err, gomsg.ErrNotFound) {
		t.Fatalf("still staged: %v", err)
	}
	if _, err := db.MessagingStore().Get(ctx, staged.ID); err != nil {
		t.Fatal(err)
	}
	if imported, err := store.ImportLegacyMessagesIntoDelivery(ctx, db.DB(), db.DeliveryStore(), store.ImportHoldAmbiguousDelivered); err != nil || imported.Imported != 0 {
		t.Fatalf("delivery import: %+v %v", imported, err)
	}
	queue, err := db.DeliveryStore().ListDeliveries(ctx, delivery.Filter{ReadyOnly: true})
	if err != nil || len(queue) != 0 {
		t.Fatalf("queued channel message: %+v %v", queue, err)
	}
	select {
	case env := <-subscriber:
		t.Fatalf("mailbox fanout: %+v", env)
	default:
	}
	audits, err := db.QueryEvents(store.EventFilter{Kinds: []string{events.KindSessionTurnRouted}})
	if err != nil || len(audits) != 1 {
		t.Fatalf("audit: %+v %v", audits, err)
	}
	var payload map[string]string
	if err := json.Unmarshal([]byte(audits[0].PayloadJSON), &payload); err != nil {
		t.Fatal(err)
	}
	if payload["actor"] != req.Actor.URN() || payload["publisher"] != staged.From.URN() || payload["message_id"] != staged.ID {
		t.Fatalf("wrong attribution: %+v", payload)
	}
	wrong := req
	wrong.Channel = "other"
	if _, err := service.AttachExisting(ctx, wrong); err == nil {
		t.Fatal("retargeted attachment")
	}
	wrong = req
	wrong.SessionID = "other"
	if _, err := service.AttachExisting(ctx, wrong); err == nil {
		t.Fatal("changed originating session")
	}
}

func TestAttachExistingAuthorizationAndAtomicAudit(t *testing.T) {
	for _, mode := range []string{"denied", "audit failure", "allowed"} {
		t.Run(mode, func(t *testing.T) {
			db, env, req := channelStage(t)
			calls := 0
			service := channels.New(db, func(_ context.Context, operation, name string, p identity.Principal, from gomsg.Address) error {
				calls++
				if operation != "publish" || name != "ops" || p.ID != env.From.URN() || p.Kind != "session" || from != env.From {
					t.Fatalf("authorization bypass/misattribution: %s %s %+v %+v", operation, name, p, from)
				}
				if mode == "denied" {
					return channels.ErrForbidden
				}
				return nil
			})
			if mode == "audit failure" {
				_, err := db.DB().Exec(`CREATE TRIGGER fail_routing_audit BEFORE INSERT ON events WHEN NEW.kind='session.turn_routed' BEGIN SELECT RAISE(ABORT,'audit unavailable'); END`)
				if err != nil {
					t.Fatal(err)
				}
			}
			ctx := identity.WithPrincipal(context.Background(), identity.Principal{ID: env.From.URN(), Kind: "session", SessionID: req.SessionID, CreatedBy: req.Actor.URN()})
			_, err := service.AttachExisting(ctx, req)
			if calls != 1 {
				t.Fatal("authorization hook not used")
			}
			if mode == "allowed" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil {
				t.Fatal("failed authorization/audit committed")
			}
			if _, err := db.StagedTurnOutput(ctx, env.ID); err != nil {
				t.Fatal("failure exposed/lost stage", err)
			}
			history, err := db.ReadChannel(ctx, "ops", 0, 100)
			if err != nil || len(history) != 0 {
				t.Fatalf("partial publication: %+v %v", history, err)
			}
		})
	}
}

func TestAttachExistingRejectsPurgedAndUnselectedStages(t *testing.T) {
	for _, mode := range []string{"purged", "expired", "unselected"} {
		t.Run(mode, func(t *testing.T) {
			db, env, req := channelStage(t)
			if mode == "purged" || mode == "expired" {
				_, err := db.DB().Exec(`UPDATE messages SET created_at=? WHERE id=?`, time.Now().Add(-store.RoutingStageRetention-time.Hour).UTC().Format(time.RFC3339Nano), env.ID)
				if err != nil {
					t.Fatal(err)
				}
				if mode == "purged" {
					if _, err := db.PurgeMessageBody(context.Background(), env.ID, identity.OperatorID); err != nil {
						t.Fatal(err)
					}
				}
			} else {
				_, err := db.DB().Exec(`UPDATE messages SET metadata='{"session_id":"routed","kind":"approval"}' WHERE id=?`, env.ID)
				if err != nil {
					t.Fatal(err)
				}
			}
			if _, err := channels.New(db, nil).AttachExisting(context.Background(), req); err == nil {
				t.Fatal("attached ineligible body")
			}
			history, err := db.ReadChannel(context.Background(), "ops", 0, 100)
			if err != nil || len(history) != 0 {
				t.Fatalf("ineligible publication: %+v %v", history, err)
			}
		})
	}
}

func TestAttachExistingRejectsStageRetargeting(t *testing.T) {
	for _, mode := range []string{"channel", "sender", "metadata"} {
		t.Run(mode, func(t *testing.T) {
			db, env, req := channelStage(t)
			if err := db.CreateSession(store.SessionRow{ID: "other", State: "running", CreatedAt: time.Now().UTC().Format(time.RFC3339Nano)}, &launch.Plan{Route: &launchprofile.Route{Channel: "ops", Kinds: []string{"final"}}}); err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "channel":
				req.Channel = "other"
			case "sender":
				req.SessionID = "other"
			case "metadata":
				if _, err := db.DB().Exec(`UPDATE messages SET metadata='{"session_id":"other","kind":"final"}' WHERE id=?`, env.ID); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := channels.New(db, nil).AttachExisting(context.Background(), req); err == nil {
				t.Fatal("retargeted stage")
			}
			if _, err := db.StagedTurnOutput(context.Background(), env.ID); err != nil {
				t.Fatal("stage released", err)
			}
		})
	}
}
func TestAttachExistingReleasesStageInsidePublicationTransaction(t *testing.T) {
	db, _, req := channelStage(t)
	_, err := db.DB().Exec(`CREATE TRIGGER require_released_stage BEFORE INSERT ON channel_publications WHEN (SELECT routing_staged FROM messages WHERE id=NEW.message_id)!=0 BEGIN SELECT RAISE(ABORT,'stage not released atomically'); END`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := channels.New(db, nil).AttachExisting(context.Background(), req); err != nil {
		t.Fatal(err)
	}
}
