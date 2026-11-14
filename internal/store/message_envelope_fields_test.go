package store

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/hollis-labs/tether/internal/messaging/envelope"
)

func TestLifecyclePayloadCarriesEnvelopeRoutingFieldsOnly(t *testing.T) {
	ctx := context.Background()
	db := openEventsStore(t)
	e := envelope.Envelope{Kind: envelope.KindApprovalRequest, To: []string{"@chrispian"}, Attention: envelope.AttentionHuman, Awaiting: "@chrispian",
		Intent: "deploy", Re: "task:CW-20261114-0013", Subject: "secret-ish subject", Body: "body text"}
	raw, _ := json.Marshal(e)
	for id, ct := range map[string]string{"m1": envelope.ContentType, "m2": "application/json"} {
		if _, err := db.db.Exec(`INSERT INTO messages(id,kind,from_urn,to_urn,payload,content_type,created_at) VALUES (?,?,?,?,?,?,?)`,
			id, "request", "msg://agent/a/x", "msg://user/local/operator", string(raw), ct, "2026-11-14T04:00:00Z"); err != nil {
			t.Fatal(err)
		}
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(db.withEnvelopeFields(ctx, `{"message_id":"m1","msg_kind":"request"}`)), &got); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"message_id": "m1", "msg_kind": "request", "envelope_kind": "approval_request", "attention": "human", "awaiting": "@chrispian", "intent": "deploy", "re": "task:CW-20261114-0013"}
	if len(got) != len(want) {
		t.Fatalf("payload = %v, want %v (no subject or body)", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %v, want %v", k, got[k], v)
		}
	}
	plain := `{"message_id":"m2"}`
	if out := db.withEnvelopeFields(ctx, plain); out != plain {
		t.Fatalf("plain message changed: %s", out)
	}
}

// Attention is stored only when the sender set it; the lifecycle event carries
// the effective value, so mail to the operator says attention:human.
func TestLifecyclePayloadCarriesEffectiveAttention(t *testing.T) {
	ctx := context.Background()
	db := openEventsStore(t)
	for id, e := range map[string]envelope.Envelope{
		"to-op":   {Kind: envelope.KindUpdate, To: []string{"@chrispian"}, Body: "b"},
		"to-ag":   {Kind: envelope.KindUpdate, To: []string{"@archie"}, Body: "b"},
		"wait-op": {Kind: envelope.KindQuestion, To: []string{"@archie"}, Awaiting: "@chrispian", Body: "b"},
	} {
		raw, _ := json.Marshal(e)
		if _, err := db.db.Exec(`INSERT INTO messages(id,kind,from_urn,to_urn,payload,content_type,created_at) VALUES (?,?,?,?,?,?,?)`,
			id, "request", "msg://agent/a/x", "msg://user/local/operator", string(raw), envelope.ContentType, "2026-11-14T04:00:00Z"); err != nil {
			t.Fatal(err)
		}
	}
	for id, want := range map[string]string{"to-op": "human", "to-ag": "agent", "wait-op": "human"} {
		var got map[string]any
		if err := json.Unmarshal([]byte(db.withEnvelopeFields(ctx, `{"message_id":"`+id+`"}`)), &got); err != nil {
			t.Fatal(err)
		}
		if got["attention"] != want {
			t.Errorf("%s: attention = %v, want %s", id, got["attention"], want)
		}
	}
}
