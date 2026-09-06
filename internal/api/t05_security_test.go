package api

// t05_security_test.go — T05 (messaging vNext, CW-20260906-0036) acceptance
// #1 evidence: "Positive/negative transport integration tests cover sender
// spoofing, unrelated session overrides, group get/thread/list access,
// subscribe and claim/ack ownership."
//
// Unrelated session overrides is covered separately in
// messages_notify_test.go (TestMessageNotify_RejectsUnrelatedSessionOverride
// / TestMessageNotify_ExplicitSessionIDMustMatchSessionKindRecipient) --
// those tests prove a real fix, not just documentation. Group non-member
// send/read spoofing already had HTTP-level coverage before this task
// (TestGroups_Send_NonMember403, TestGroups_ListMessages_RequiresAs,
// TestGroups_Mentions_RequiresAs in groups_test.go) -- this file adds the
// still-missing cases: impersonating an EXISTING different member (not
// just a non-member), the /messages sender-spoofing trust-model baseline,
// SSE subscribe scoping (negative case), and claim/ack ownership via HTTP.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	messaging "github.com/hollis-labs/go-messaging"
)

// TestMessageSend_SenderIdentityIsSelfAssertedByDesign documents, as an
// explicit and tested fact rather than an unexamined gap, Tether's current
// same-host trust model for the general messaging surface (T05 acceptance
// #3: "trusted per-user mode is explicit, not assumed universal
// authentication"). No peer-credential mechanism exists anywhere in this
// codebase (confirmed by the T05 design research: no SO_PEERCRED/ucred
// code, and the daemon's transport can be UDS OR a documented TCP-loopback
// option per ADR-0002) -- so /messages' `from` field is, today, whatever
// the caller claims. This test locks in that fact so a future change to
// the trust model is a deliberate, reviewed decision, not a silent
// regression discovered by surprise.
func TestMessageSend_SenderIdentityIsSelfAssertedByDesign(t *testing.T) {
	srv, _ := newMessageTestServer(t)

	body := []byte(`{
		"from":"msg://agent/tether/impersonated-sender",
		"to":"msg://agent/tether/recipient",
		"kind":"notice",
		"payload":{"text":"hi"}
	}`)
	resp, err := http.Post(srv.URL+"/messages", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (same-host trust model accepts the self-asserted sender)", resp.StatusCode)
	}
	var got struct {
		From string `json:"from"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.From != "msg://agent/tether/impersonated-sender" {
		t.Fatalf("expected the claimed sender to be recorded verbatim (no verification exists), got %q", got.From)
	}
}

// TestHandleMessagesSubscribe_ScopedToRecipient_ExcludesOthers is the
// negative case TestHandleMessagesSubscribe_SSEFraming didn't cover: a
// message addressed to a DIFFERENT recipient must never appear on a
// subscriber's stream, even though both are live at once. This is the
// actual "claim/ack ownership" boundary for the live-push surface --
// subscribing "as" someone only ever gets you their own traffic.
func TestHandleMessagesSubscribe_ScopedToRecipient_ExcludesOthers(t *testing.T) {
	srv, db := newMessageTestServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	alice := messaging.Address{Kind: messaging.KindAgent, Authority: "test", ID: "alice"}
	carol := messaging.Address{Kind: messaging.KindAgent, Authority: "test", ID: "carol"}

	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/messages/subscribe?to="+alice.URN(), nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer resp.Body.Close()
	time.Sleep(50 * time.Millisecond)

	ms := db.MessagingStore()
	bob := messaging.Address{Kind: messaging.KindAgent, Authority: "test", ID: "bob"}
	// Addressed to carol, NOT alice -- must not reach alice's subscription.
	unrelated, err := ms.Send(context.Background(), messaging.Envelope{Kind: messaging.MsgKindNotice, From: bob, To: carol})
	if err != nil {
		t.Fatalf("send unrelated: %v", err)
	}
	// Addressed to alice -- must reach it, proving the stream is alive and
	// the absence of the unrelated message isn't just a dead connection.
	forAlice, err := ms.Send(context.Background(), messaging.Envelope{Kind: messaging.MsgKindNotice, From: bob, To: alice})
	if err != nil {
		t.Fatalf("send for alice: %v", err)
	}

	sawUnrelated := false
	sawForAlice := false
	scanner := bufio.NewScanner(resp.Body)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && !sawForAlice {
		if !scanner.Scan() {
			break
		}
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var env messaging.Envelope
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &env); err != nil {
			continue
		}
		if env.ID == unrelated.ID {
			sawUnrelated = true
		}
		if env.ID == forAlice.ID {
			sawForAlice = true
		}
	}
	if !sawForAlice {
		t.Fatalf("expected alice's subscription to receive the message actually addressed to her")
	}
	if sawUnrelated {
		t.Fatalf("alice's subscription received a message addressed to a different recipient (carol) -- subscribe scoping is broken")
	}
}

// TestMessageConsume_OwnershipEnforcedOverHTTP is the HTTP-transport proof
// for "claim/ack ownership": Consume's ErrWrongRecipient check (already
// unit-tested at the store layer) must also hold through the real ?as=
// query-param path, positive and negative.
func TestMessageConsume_OwnershipEnforcedOverHTTP(t *testing.T) {
	srv, db := newMessageTestServer(t)
	ctx := context.Background()
	ms := db.MessagingStore()

	from := messaging.Address{Kind: messaging.KindAgent, Authority: "test", ID: "sender"}
	to := messaging.Address{Kind: messaging.KindAgent, Authority: "test", ID: "owner"}
	sent, err := ms.Send(ctx, messaging.Envelope{Kind: messaging.MsgKindNotice, From: from, To: to})
	if err != nil {
		t.Fatalf("send: %v", err)
	}

	// Negative: a caller claiming to be someone else cannot consume it.
	imposter := messaging.Address{Kind: messaging.KindAgent, Authority: "test", ID: "imposter"}
	wrongResp, err := http.Post(srv.URL+"/messages/"+sent.ID+"/consume?as="+imposter.URN(), "application/json", nil)
	if err != nil {
		t.Fatalf("consume as imposter: %v", err)
	}
	defer wrongResp.Body.Close()
	if wrongResp.StatusCode != http.StatusConflict {
		t.Fatalf("consume as imposter: status = %d, want 409", wrongResp.StatusCode)
	}

	// Positive: the actual recipient succeeds.
	okResp, err := http.Post(srv.URL+"/messages/"+sent.ID+"/consume?as="+to.URN(), "application/json", nil)
	if err != nil {
		t.Fatalf("consume as owner: %v", err)
	}
	defer okResp.Body.Close()
	if okResp.StatusCode != http.StatusNoContent {
		t.Fatalf("consume as owner: status = %d, want 204", okResp.StatusCode)
	}
}

// TestMessageMailboxReads_RequireAsClaim covers the gap a distinct review
// pass found in this task: /messages' Get/Inbox/List/Thread reads had zero
// identity check at all (worse than the write actions on the same file,
// which already required ?as=), even though this task specifically
// rewired mux_message_get/inbox/list/thread (mcpadapter) to hit these very
// endpoints. Positive and negative cases per handler.
func TestMessageMailboxReads_RequireAsClaim(t *testing.T) {
	srv, db := newMessageTestServer(t)
	ctx := context.Background()
	ms := db.MessagingStore()

	from := messaging.Address{Kind: messaging.KindAgent, Authority: "test", ID: "sender"}
	owner := messaging.Address{Kind: messaging.KindAgent, Authority: "test", ID: "mailbox-owner"}
	stranger := messaging.Address{Kind: messaging.KindAgent, Authority: "test", ID: "stranger"}

	sent, err := ms.Send(ctx, messaging.Envelope{Kind: messaging.MsgKindNotice, From: from, To: owner})
	if err != nil {
		t.Fatalf("send: %v", err)
	}

	t.Run("Get", func(t *testing.T) {
		// Missing as -> 400.
		if got := httpGetStatus(t, srv.URL+"/messages/"+sent.ID); got != http.StatusBadRequest {
			t.Fatalf("get without as: status = %d, want 400", got)
		}
		// Unrelated caller -> 403, not the envelope contents.
		if got := httpGetStatus(t, srv.URL+"/messages/"+sent.ID+"?as="+stranger.URN()); got != http.StatusForbidden {
			t.Fatalf("get as stranger: status = %d, want 403", got)
		}
		// Sender or recipient -> 200.
		if got := httpGetStatus(t, srv.URL+"/messages/"+sent.ID+"?as="+owner.URN()); got != http.StatusOK {
			t.Fatalf("get as recipient: status = %d, want 200", got)
		}
		if got := httpGetStatus(t, srv.URL+"/messages/"+sent.ID+"?as="+from.URN()); got != http.StatusOK {
			t.Fatalf("get as sender: status = %d, want 200", got)
		}
	})

	t.Run("Inbox", func(t *testing.T) {
		if got := httpGetStatus(t, srv.URL+"/messages/inbox?to="+owner.URN()); got != http.StatusBadRequest {
			t.Fatalf("inbox without as: status = %d, want 400", got)
		}
		if got := httpGetStatus(t, srv.URL+"/messages/inbox?to="+owner.URN()+"&as="+stranger.URN()); got != http.StatusForbidden {
			t.Fatalf("inbox as stranger: status = %d, want 403", got)
		}
		if got := httpGetStatus(t, srv.URL+"/messages/inbox?to="+owner.URN()+"&as="+owner.URN()); got != http.StatusOK {
			t.Fatalf("inbox as owner: status = %d, want 200", got)
		}
	})

	t.Run("List", func(t *testing.T) {
		if got := httpGetStatus(t, srv.URL+"/messages/list?to="+owner.URN()); got != http.StatusBadRequest {
			t.Fatalf("list without as: status = %d, want 400", got)
		}
		if got := httpGetStatus(t, srv.URL+"/messages/list?to="+owner.URN()+"&as="+stranger.URN()); got != http.StatusForbidden {
			t.Fatalf("list as stranger: status = %d, want 403", got)
		}
		if got := httpGetStatus(t, srv.URL+"/messages/list?to="+owner.URN()+"&as="+owner.URN()); got != http.StatusOK {
			t.Fatalf("list as owner: status = %d, want 200", got)
		}
	})
}

// TestMessageThread_ScopedToParticipant covers the thread-read path: a
// caller only sees the turns that actually involve their claimed identity,
// so a caller unrelated to a thread sees an empty list rather than the
// other parties' conversation.
func TestMessageThread_ScopedToParticipant(t *testing.T) {
	srv, db := newMessageTestServer(t)
	ctx := context.Background()
	ms := db.MessagingStore()

	alice := messaging.Address{Kind: messaging.KindAgent, Authority: "test", ID: "alice"}
	bob := messaging.Address{Kind: messaging.KindAgent, Authority: "test", ID: "bob"}
	stranger := messaging.Address{Kind: messaging.KindAgent, Authority: "test", ID: "stranger"}
	threadID := "thread-scoped-1"

	if _, err := ms.Send(ctx, messaging.Envelope{Kind: messaging.MsgKindRequest, From: alice, To: bob, ThreadID: threadID}); err != nil {
		t.Fatalf("send 1: %v", err)
	}
	if _, err := ms.Send(ctx, messaging.Envelope{Kind: messaging.MsgKindResponse, From: bob, To: alice, ThreadID: threadID}); err != nil {
		t.Fatalf("send 2: %v", err)
	}

	// Missing as -> 400.
	if got := httpGetStatus(t, srv.URL+"/messages/thread/"+threadID); got != http.StatusBadRequest {
		t.Fatalf("thread without as: status = %d, want 400", got)
	}

	// A genuine participant sees both turns.
	resp, err := http.Get(srv.URL + "/messages/thread/" + threadID + "?as=" + alice.URN())
	if err != nil {
		t.Fatalf("thread as alice: %v", err)
	}
	defer resp.Body.Close()
	var out struct {
		Messages []messaging.Envelope `json:"messages"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Messages) != 2 {
		t.Fatalf("thread as participant: got %d messages, want 2", len(out.Messages))
	}

	// An unrelated caller sees nothing from this thread.
	resp2, err := http.Get(srv.URL + "/messages/thread/" + threadID + "?as=" + stranger.URN())
	if err != nil {
		t.Fatalf("thread as stranger: %v", err)
	}
	defer resp2.Body.Close()
	var out2 struct {
		Messages []messaging.Envelope `json:"messages"`
	}
	if err := json.NewDecoder(resp2.Body).Decode(&out2); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out2.Messages) != 0 {
		t.Fatalf("thread as stranger: got %d messages, want 0", len(out2.Messages))
	}
}

// TestGroups_Send_ImpersonatingExistingDifferentMemberStillForbidden
// strengthens the pre-existing TestGroups_Send_NonMember403 (which only
// covers a total stranger): a caller claiming to be an EXISTING,
// registered agent who simply isn't a member of THIS group still cannot
// post, because the service layer's membership check is keyed on the
// exact claimed URN belonging to this group's member list, not "is a
// member of something." This is the actual boundary self-asserted
// identity leaves in place: you can claim to be any existing URN, but the
// service still requires that URN to legitimately be a member of the
// group being posted to.
func TestGroups_Send_ImpersonatingExistingDifferentMemberStillForbidden(t *testing.T) {
	g := newGroupServer(t)
	ownerA := g.seedAgent("OwnerA")
	memberOfOtherGroup := g.seedAgent("MemberElsewhere")
	groupA := g.seedGroup("GroupA", ownerA)

	resp, _ := g.do(http.MethodPost, itemURLGroup(groupA.URN)+"/messages", sendGroupRequest{
		From: memberOfOtherGroup, Kind: "message",
		Payload: json.RawMessage(`{"text":"impersonation attempt"}`),
	})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status=%d want 403", resp.StatusCode)
	}
}

// httpGetStatus performs a GET and returns the status code, closing the
// response body so callers don't need a defer per call.
func httpGetStatus(t *testing.T, url string) int {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}
