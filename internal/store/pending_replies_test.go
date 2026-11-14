package store

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/hollis-labs/tether/internal/messaging/envelope"
)

func TestPendingRepliesNeedTheAgentsAnswer(t *testing.T) {
	ctx := context.Background()
	db := openEventsStore(t)
	me, archie := "msg://agent/agent-mux/agt_me", "msg://agent/agent-mux/agt_archie"
	at := time.Date(2026, 11, 14, 4, 0, 0, 0, time.UTC)
	insert := func(id, from, to, contentType string, e any, inReplyTo string) {
		t.Helper()
		raw, _ := json.Marshal(e)
		at = at.Add(time.Second)
		if _, err := db.db.Exec(`INSERT INTO messages(id,kind,from_urn,to_urn,payload,content_type,in_reply_to,created_at) VALUES (?,?,?,?,?,?,?,?)`,
			id, "request", from, to, string(raw), contentType, inReplyTo, at.Format(time.RFC3339Nano)); err != nil {
			t.Fatal(err)
		}
	}
	q := envelope.Envelope{Kind: envelope.KindQuestion, Subject: "Ship?", Body: "b", Options: []envelope.Option{{ID: "a", Label: "A"}, {ID: "b", Label: "B"}}}
	insert("q1", archie, me, envelope.ContentType, q, "")                                                                        // pending
	insert("q2", archie, me, envelope.ContentType, q, "")                                                                        // answered below
	insert("r2", me, archie, envelope.ContentType, envelope.Envelope{Kind: envelope.KindMessage, Body: "a"}, "q2")               // my reply
	insert("u1", archie, me, envelope.ContentType, envelope.Envelope{Kind: envelope.KindUpdate, Body: "fyi"}, "")                // no reply needed
	insert("u2", archie, me, envelope.ContentType, envelope.Envelope{Kind: envelope.KindUpdate, Body: "x", Awaiting: "@me"}, "") // awaits me
	insert("q3", archie, me, envelope.ContentType, envelope.Envelope{Kind: envelope.KindQuestion, Body: "b", Awaiting: "@other"}, "")
	insert("q4", archie, me, envelope.ContentType, q, "") // read below
	insert("p1", archie, me, "application/json", map[string]string{"text": "plain"}, "")
	if _, err := db.db.Exec(`UPDATE messages SET read_at=? WHERE id='q4'`, at.Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	got, err := db.PendingReplies(ctx, me, "@me", 10)
	if err != nil {
		t.Fatal(err)
	}
	ids := []string{}
	for _, p := range got {
		ids = append(ids, p.MessageID)
	}
	if len(ids) != 2 || ids[0] != "q1" || ids[1] != "u2" || got[0].Subject != "Ship?" || got[0].From != archie {
		t.Fatalf("pending = %+v", got)
	}
}

func TestPendingRepliesAreNotCrowdedOutByUnreadUpdates(t *testing.T) {
	ctx := context.Background()
	db := openEventsStore(t)
	me := "msg://agent/agent-mux/agt_me"
	at := time.Date(2026, 11, 14, 4, 0, 0, 0, time.UTC)
	insert := func(id string, e envelope.Envelope) {
		raw, _ := json.Marshal(e)
		at = at.Add(time.Second)
		if _, err := db.db.Exec(`INSERT INTO messages(id,kind,from_urn,to_urn,payload,content_type,created_at) VALUES (?,?,?,?,?,?,?)`,
			id, "notice", "msg://agent/agent-mux/agt_x", me, string(raw), envelope.ContentType, at.Format(time.RFC3339Nano)); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 300; i++ {
		insert(fmt.Sprintf("u%03d", i), envelope.Envelope{Kind: envelope.KindUpdate, Body: "fyi"})
	}
	insert("late-q", envelope.Envelope{Kind: envelope.KindQuestion, Body: "?", Options: []envelope.Option{{ID: "a", Label: "A"}, {ID: "b", Label: "B"}}})
	got, err := db.PendingReplies(ctx, me, "@me", 20)
	if err != nil || len(got) != 1 || got[0].MessageID != "late-q" {
		t.Fatalf("pending = %+v err=%v", got, err)
	}
}
