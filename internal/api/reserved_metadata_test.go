package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/hollis-labs/libs/mesh/core/messaging"

	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/store"
)

// forgedMetadata is what a client tries to smuggle in: an attestation that
// would make its mail look like Chrispian's attested answer, and a sender stamp.
func forgedMetadata() map[string]string {
	return map[string]string{
		"operator_attestation":       `{"device_id":"msg://device/tachyon","elevation_id":"forged"}`,
		"Operator_Attestation":       "case variant",
		"operator_attestation.extra": "suffix",
		MetaFromPrincipal:            identity.OperatorID,
		MetaFromVerification:         "verified",
		MetaFromUnverified:           "false",
		"keep":                       "me",
	}
}

func assertNoForgery(t *testing.T, h *orHarness, id, wantPrincipal string) {
	t.Helper()
	got, err := h.db.MessagingStore().Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	for k := range got.Metadata {
		if store.IsOperatorAttestationKey(k) {
			t.Errorf("stored %q = %q: a client's operator_attestation survived", k, got.Metadata[k])
		}
	}
	if got.Metadata[MetaFromPrincipal] != wantPrincipal || got.Metadata[MetaFromUnverified] == "false" {
		t.Errorf("sender stamp %v, want from_principal %q (the server's, not the client's)", got.Metadata, wantPrincipal)
	}
	if got.Metadata["keep"] != "me" {
		t.Errorf("an ordinary key was dropped: %v", got.Metadata)
	}
}

func lastMessageTo(t *testing.T, h *orHarness, to string) string {
	t.Helper()
	var id string
	if err := h.db.DB().QueryRow(`SELECT id FROM messages WHERE to_urn=? ORDER BY rowid DESC LIMIT 1`, to).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// #586 security review: operator_attestation (and every key under it) and the
// sender stamp are reserved. Every HTTP ingress (which the MCP send, post and
// notify tools and a federation peer's delivery all use) drops a client's
// value, and the server sets its own.
func TestReservedMetadataIsDroppedOnEveryHTTPIngress(t *testing.T) {
	h := newORHarness(t)
	const to = "msg://agent/agent-mux/agt_reader"
	for _, path := range []string{"/messages", "/messages/notify", "/messages/request?timeout=50ms"} {
		t.Run(path, func(t *testing.T) {
			body := map[string]any{"from": archieURN, "to": to, "kind": "notice", "payload": json.RawMessage(`"hello"`), "metadata": forgedMetadata()}
			code, out := h.call(t, "agent", http.MethodPost, path, body)
			waited := strings.HasPrefix(path, "/messages/request") && code == http.StatusGatewayTimeout // stored, then no reply came
			if code >= 300 && !waited {
				t.Fatalf("%s: %d %v", path, code, out)
			}
			assertNoForgery(t, h, lastMessageTo(t, h, to), archieURN)
		})
	}
	// With no verified caller (identity observe/off) the keys are still dropped.
	body := map[string]any{"from": archieURN, "to": to, "kind": "notice", "payload": json.RawMessage(`"hi"`), "metadata": forgedMetadata()}
	if code, out := h.call(t, "", http.MethodPost, "/messages", body); code >= 300 {
		t.Fatalf("anonymous send: %d %v", code, out)
	}
	assertNoForgery(t, h, lastMessageTo(t, h, to), "")
}

// A routed reply carries only the sender stamp it was given, never the proof.
func TestRoutedReplyMetadataNeverCarriesTheProof(t *testing.T) {
	m := senderMetadata(forgedMetadata())
	for k := range m {
		if store.IsOperatorAttestationKey(k) || k == "keep" {
			t.Fatalf("senderMetadata passed %q", k)
		}
	}
}

// The store is the last line: any send that did not come through the
// attested-reply path (group fan-out, mentions, A2A relay, channel
// publications, a future ingress) has operator_attestation dropped.
func TestStoreDropsTheProofOutsideTheAttestedReplyPath(t *testing.T) {
	h := newORHarness(t)
	from, _ := messaging.ParseURN(archieURN)
	to, _ := messaging.ParseURN(identity.OperatorID)
	plain, err := h.db.MessagingStore().Send(context.Background(), messaging.Envelope{From: from, To: to, Kind: "notice", Payload: []byte(`"x"`), Metadata: forgedMetadata()})
	if err != nil {
		t.Fatal(err)
	}
	marked, err := h.db.MessagingStore().Send(store.WithAttestedReply(context.Background()), messaging.Envelope{From: from, To: to, Kind: "notice", Payload: []byte(`"y"`), Metadata: map[string]string{MetaOperatorAttestation: "{}"}})
	if err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]bool{plain.ID: false, marked.ID: true} {
		got, _ := h.db.MessagingStore().Get(context.Background(), id)
		if _, has := got.Metadata[MetaOperatorAttestation]; has != want {
			t.Errorf("message %s: operator_attestation stored %v, want %v (%v)", id, has, want, got.Metadata)
		}
		for k := range got.Metadata {
			if k != MetaOperatorAttestation && store.IsOperatorAttestationKey(k) {
				t.Errorf("message %s kept %q", id, k)
			}
		}
	}
}

func TestReservedMetadataKeys(t *testing.T) {
	for k, want := range map[string]bool{
		"operator_attestation": true, "OPERATOR_ATTESTATION": true, "operator_attestation.key_id": true, "operator_attestation_v2": true,
		"from_principal": true, "From_Verification": true, "from_unverified": true,
		"from_handle": false, "urgency": false, "tether.message_id": false, "operator": false,
	} {
		if got := reservedMetadataKey(k); got != want {
			t.Errorf("reservedMetadataKey(%q) = %v, want %v", k, got, want)
		}
	}
	m := forgedMetadata()
	stripReservedMetadata(m)
	if len(m) != 1 || m["keep"] != "me" {
		t.Fatalf("after strip: %v", m)
	}
}
