package api

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/hollis-labs/libs/mesh/core/messaging"

	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/messaging/envelope"
	"github.com/hollis-labs/tether/internal/operatorattest"
	"github.com/hollis-labs/tether/internal/store"
)

const (
	tachyonDevice = "msg://device/tachyon"
	tangentDevice = "msg://device/tangent"
)

// The callers a test can act as (header X-Test-As).
var orPrincipals = map[string]identity.Principal{
	"tachyon":       {Verified: true, ID: tachyonDevice, Kind: "device", CreatedBy: identity.OperatorID, Scopes: []string{identity.ScopeRead, identity.ScopeOperate, identity.ScopeOperatorReply}},
	"tangent":       {Verified: true, ID: tangentDevice, Kind: "device", CreatedBy: identity.OperatorID, Scopes: []string{identity.ScopeOperatorReplyView}},
	"tachyon-2":     {Verified: true, ID: "msg://device/tachyon-2", Kind: "device", CreatedBy: identity.OperatorID, Scopes: []string{identity.ScopeOperatorReply}},
	"no-scope":      {Verified: true, ID: "msg://device/plain", Kind: "device", CreatedBy: identity.OperatorID, Scopes: []string{identity.ScopeRead}},
	"token":         {Verified: true, ID: tachyonDevice, Kind: "device", CreatedBy: identity.OperatorID, TokenID: "tok_1", Scopes: []string{identity.ScopeOperatorReply}},
	"other-pairing": {Verified: true, ID: "msg://device/elsewhere", Kind: "device", CreatedBy: "msg://agent/x/pairer", Scopes: []string{identity.ScopeOperatorReply}},
	"agent":         {Verified: true, ID: archieURN, Kind: "agent", Scopes: []string{"*"}},
	"unverified":    {ID: tachyonDevice, Kind: "device", CreatedBy: identity.OperatorID, Scopes: []string{identity.ScopeOperatorReply}},
	"operator":      {Verified: true, ID: identity.OperatorID, Kind: "operator", Scopes: []string{"*"}},
}

type orHarness struct {
	srv   *httptest.Server
	db    *store.Store
	priv  ed25519.PrivateKey
	cfg   *operatorattest.Config
	off   bool
	mu    sync.Mutex
	audit []identity.Observation
}

func newORHarness(t *testing.T) *orHarness {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "or.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	h := &orHarness{db: db, priv: priv, cfg: &operatorattest.Config{Issuer: "tangent", MaxAge: 300 * time.Second, Keys: []operatorattest.Key{{ID: "tangent-1", Public: pub}}}}
	cat := &config.Catalog{Agents: map[string]config.Agent{"archie": {ID: "archie", URN: archieURN}}}
	inner := NewHandler(Deps{Service: &fakeLaunchService{}, MessageStore: db.MessagingStore(), Seats: db, Aliases: db, Catalog: fixedCatalog{cat},
		OperatorAttestation: func() *operatorattest.Config {
			if h.off {
				return nil
			}
			return h.cfg
		},
		OperatorReplies: db,
		IdentityAudit: func(_ context.Context, o identity.Observation) error {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.audit = append(h.audit, o)
			return nil
		}})
	h.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if p, ok := orPrincipals[r.Header.Get("X-Test-As")]; ok {
			r = r.WithContext(identity.WithPrincipal(r.Context(), p))
		}
		inner.ServeHTTP(w, r)
	}))
	t.Cleanup(h.srv.Close)
	return h
}

func (h *orHarness) call(t *testing.T, as, method, path string, body any) (int, map[string]any) {
	t.Helper()
	var raw []byte
	if body != nil {
		raw, _ = json.Marshal(body)
	}
	req, _ := http.NewRequest(method, h.srv.URL+path, bytes.NewReader(raw))
	req.Header.Set("X-Test-As", as)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func errCode(out map[string]any) string {
	e, _ := out["error"].(map[string]any)
	c, _ := e["code"].(string)
	return c
}

// card stores a typed card from @archie to `to` and returns its id.
func (h *orHarness) card(t *testing.T, to string, e envelope.Envelope) string {
	t.Helper()
	e.Schema, e.From = envelope.Schema, "@archie"
	if len(e.To) == 0 {
		e.To = []string{envelope.Operator}
	}
	body, _ := json.Marshal(e)
	from, _ := messaging.ParseURN(archieURN)
	dest, _ := messaging.ParseURN(to)
	sent, err := h.db.MessagingStore().Send(context.Background(), messaging.Envelope{From: from, To: dest, Kind: envelope.TransportKind(e.Kind), Payload: body,
		ContentType: envelope.ContentType, Metadata: map[string]string{MetaFromVerification: "verified", MetaFromPrincipal: archieURN}})
	if err != nil {
		t.Fatal(err)
	}
	return sent.ID
}

func (h *orHarness) approval(t *testing.T) string {
	return h.card(t, identity.OperatorID, envelope.Envelope{Kind: envelope.KindApprovalRequest, Subject: "Ship #562?", Body: "Impact: hub restart. Rollback: redeploy the previous build."})
}

// challenge asks for a challenge as Tachyon and returns it.
func (h *orHarness) challenge(t *testing.T, as, id, option string) map[string]any {
	t.Helper()
	code, out := h.call(t, as, http.MethodPost, "/messages/"+id+"/operator-reply/challenge", map[string]string{"option": option})
	if code != http.StatusCreated {
		t.Fatalf("challenge: %d %v", code, out)
	}
	return out
}

// claims are what Tangent signs for a challenge, as the view returned it.
func (h *orHarness) claims(t *testing.T, ch map[string]any, note *string) operatorattest.Claims {
	t.Helper()
	code, view := h.call(t, "tangent", http.MethodGet, "/operator-attestation/challenges/"+ch["challenge_id"].(string), nil)
	if code != http.StatusOK {
		t.Fatalf("view: %d %v", code, view)
	}
	now := time.Now().Unix()
	c := operatorattest.Claims{Iss: "tangent", Aud: operatorattest.Audience, Sub: operatorattest.Subject, Jti: "jti-" + ch["challenge_id"].(string),
		Iat: now - 2, Exp: now + 120, ChallengeID: ch["challenge_id"].(string), Nonce: ch["nonce"].(string), MessageID: view["message_id"].(string),
		Option: view["option"].(map[string]any)["id"].(string), Binding: view["binding"].(string), BodySHA256: view["body_sha256"].(string),
		DeviceID: view["device_id"].(string), ElevationID: "el-" + ch["challenge_id"].(string), ElevatedAt: now - 5}
	if note != nil {
		sum := operatorattest.SHA256Hex(*note)
		c.NoteSHA256 = &sum
	}
	return c
}

func (h *orHarness) sign(t *testing.T, c operatorattest.Claims) string {
	t.Helper()
	tok, err := operatorattest.Sign(c, "tangent-1", h.priv)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func (h *orHarness) reply(t *testing.T, as, id string, ch map[string]any, option string, note *string, assertion string) (int, map[string]any) {
	t.Helper()
	body := map[string]any{"challenge_id": ch["challenge_id"], "option": option, "assertion": assertion}
	if note != nil {
		body["note"] = *note
	}
	return h.call(t, as, http.MethodPost, "/messages/"+id+"/operator-reply", body)
}

func (h *orHarness) replies(t *testing.T, id string) int {
	t.Helper()
	var n int
	if err := h.db.DB().QueryRow(`SELECT count(*) FROM messages WHERE in_reply_to=?`, id).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func (h *orHarness) lastAudit(t *testing.T) (string, map[string]any) {
	t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.audit) == 0 {
		t.Fatal("no audit row")
	}
	o := h.audit[len(h.audit)-1]
	var d map[string]any
	_ = json.Unmarshal([]byte(o.Detail), &d)
	return o.Event, d
}

// The accepted path end to end: challenge, Tangent's view of Tether's card,
// the signed assertion, the stored verified reply that settles the parent.
func TestOperatorAttestedReplyIsStoredAsTheOperatorsVerifiedAnswer(t *testing.T) {
	h := newORHarness(t)
	id := h.approval(t)
	ch := h.challenge(t, "tachyon", id, envelope.OptionApprove)
	if ch["binding"] != operatorattest.Binding(id, "approve") {
		t.Fatalf("binding %v", ch["binding"])
	}
	code, view := h.call(t, "tangent", http.MethodGet, "/operator-attestation/challenges/"+ch["challenge_id"].(string), nil)
	asker, _ := view["asker"].(map[string]any)
	if code != http.StatusOK || view["kind"] != "approval_request" || view["subject"] != "Ship #562?" || asker["from_verification"] != "verified" || asker["address"] != archieURN {
		t.Fatalf("view: %d %v", code, view)
	}
	if code, full := h.call(t, "tangent", http.MethodGet, view["full_url"].(string), nil); code != http.StatusOK || full["body"] != "Impact: hub restart. Rollback: redeploy the previous build." {
		t.Fatalf("full body: %d %v", code, full)
	}

	note := "ship it after lunch"
	code, out := h.reply(t, "tachyon", id, ch, "approve", &note, h.sign(t, h.claims(t, ch, &note)))
	if code != http.StatusCreated {
		t.Fatalf("reply: %d %v", code, out)
	}
	stored, err := h.db.MessagingStore().Get(context.Background(), out["message_id"].(string))
	if err != nil {
		t.Fatal(err)
	}
	if stored.From.URN() != identity.OperatorID || stored.To.URN() != archieURN || stored.InReplyTo != id ||
		stored.Metadata[MetaFromPrincipal] != identity.OperatorID || stored.Metadata[MetaFromVerification] != "verified" {
		t.Fatalf("stored reply %+v", stored)
	}
	var proof map[string]any
	if json.Unmarshal([]byte(stored.Metadata[MetaOperatorAttestation]), &proof) != nil || proof["device_id"] != tachyonDevice || proof["key_id"] != "tangent-1" ||
		proof["challenge_id"] != ch["challenge_id"] || proof["assertion_id"] == "" || proof["elevation_id"] == "" {
		t.Fatalf("operator_attestation %v", stored.Metadata[MetaOperatorAttestation])
	}
	var e envelope.Envelope
	if json.Unmarshal(stored.Payload, &e) != nil || e.Kind != envelope.KindMessage || e.Answer == nil || e.Answer.Option != "approve" || e.Answer.Note != note || e.From != envelope.Operator {
		t.Fatalf("reply envelope %s", stored.Payload)
	}
	// The parent no longer waits on the operator.
	code, pending := h.call(t, "operator", http.MethodGet, "/messages/pending-replies?as="+identity.OperatorID, nil)
	if list, _ := pending["pending"].([]any); code != http.StatusOK || len(list) != 0 {
		t.Fatalf("pending after the reply: %d %v", code, pending)
	}
	if event, d := h.lastAudit(t); event != AuditOperatorReplyAttested || d["device_id"] != tachyonDevice || d["message_id"] != id || d["elevation_id"] == nil || d["parent_from_verification"] != "verified" {
		t.Fatalf("audit %s %v", event, d)
	}
	// Answered: no new challenge.
	if code, out := h.call(t, "tachyon", http.MethodPost, "/messages/"+id+"/operator-reply/challenge", map[string]string{"option": "deny"}); code != http.StatusConflict || errCode(out) != CodeAlreadyAnswered {
		t.Fatalf("challenge after the answer: %d %v", code, out)
	}
}

func TestOperatorReplyCallerRules(t *testing.T) {
	h := newORHarness(t)
	id := h.approval(t)
	for as, want := range map[string]string{
		"token": CodeNotOperatorDevice, "other-pairing": CodeNotOperatorDevice, "agent": CodeNotOperatorDevice,
		"unverified": CodeNotOperatorDevice, "operator": CodeNotOperatorDevice, "no-scope": CodeScopeRequired, "tangent": CodeScopeRequired,
	} {
		code, out := h.call(t, as, http.MethodPost, "/messages/"+id+"/operator-reply/challenge", map[string]string{"option": "approve"})
		if code != http.StatusForbidden || errCode(out) != want {
			t.Errorf("challenge as %s: %d %v, want 403 %s", as, code, out, want)
		}
		code, out = h.call(t, as, http.MethodPost, "/messages/"+id+"/operator-reply", map[string]string{"challenge_id": "orc_x", "option": "approve", "assertion": "x"})
		if code != http.StatusForbidden || errCode(out) != want {
			t.Errorf("reply as %s: %d %v, want 403 %s", as, code, out, want)
		}
	}
	ch := h.challenge(t, "tachyon", id, "approve")
	if code, out := h.call(t, "tachyon", http.MethodGet, "/operator-attestation/challenges/"+ch["challenge_id"].(string), nil); code != http.StatusForbidden || errCode(out) != CodeScopeRequired {
		t.Fatalf("Tachyon reading Tangent's view: %d %v", code, out)
	}
	h.off = true
	for _, path := range []string{"/messages/" + id + "/operator-reply/challenge", "/messages/" + id + "/operator-reply"} {
		if code, out := h.call(t, "tachyon", http.MethodPost, path, map[string]string{"option": "approve"}); code != http.StatusNotFound || errCode(out) != CodeFeatureDisabled {
			t.Errorf("%s with no approved trust root: %d %v", path, code, out)
		}
	}
	if code, out := h.call(t, "tangent", http.MethodGet, "/operator-attestation/challenges/"+ch["challenge_id"].(string), nil); code != http.StatusNotFound || errCode(out) != CodeFeatureDisabled {
		t.Fatalf("view with no approved trust root: %d %v", code, out)
	}
}

func TestOperatorReplyParentRules(t *testing.T) {
	h := newORHarness(t)
	toArchie := h.card(t, archieURN, envelope.Envelope{Kind: envelope.KindQuestion, To: []string{"@archie"}, Body: "?", Options: []envelope.Option{{ID: "a", Label: "A"}}})
	update := h.card(t, identity.OperatorID, envelope.Envelope{Kind: envelope.KindUpdate, Body: "fyi"})
	question := h.card(t, identity.OperatorID, envelope.Envelope{Kind: envelope.KindQuestion, Body: "which?", Options: []envelope.Option{{ID: "a", Label: "A"}, {ID: "b", Label: "B"}}})
	for name, c := range map[string]struct {
		id, option, code, field string
		status                  int
	}{
		"no such message":        {"nope", "a", CodeParentNotFound, "", http.StatusNotFound},
		"not the operator's":     {toArchie, "a", CodeParentNotOperators, "", http.StatusForbidden},
		"not answerable":         {update, "a", CodeNotAnswerable, "in_reply_to", http.StatusBadRequest},
		"not one of its options": {question, "c", CodeInvalidOption, "answer.option", http.StatusBadRequest},
	} {
		code, out := h.call(t, "tachyon", http.MethodPost, "/messages/"+c.id+"/operator-reply/challenge", map[string]string{"option": c.option})
		details, _ := out["error"].(map[string]any)["details"].(map[string]any)
		if code != c.status || errCode(out) != c.code || (c.field != "" && details["field"] != c.field) {
			t.Errorf("%s: %d %v, want %d %s", name, code, out, c.status, c.code)
		}
	}
	for i := range 10 {
		_ = i
		h.challenge(t, "tachyon", question, "a")
	}
	if code, out := h.call(t, "tachyon", http.MethodPost, "/messages/"+question+"/operator-reply/challenge", map[string]string{"option": "a"}); code != http.StatusTooManyRequests || errCode(out) != CodeTooManyChallenges {
		t.Fatalf("11th challenge: %d %v", code, out)
	}
}

// Every refusal stores nothing, audits the code, and leaves a named challenge consumed.
func TestOperatorReplyRefusals(t *testing.T) {
	sign := func(h *orHarness, t *testing.T, c operatorattest.Claims) string { return h.sign(t, c) }
	note := "ok"
	cases := map[string]struct {
		code   string
		status int
		m      func(t *testing.T, h *orHarness, c *operatorattest.Claims) (string, string, *string)
	}{
		"forged signature": {CodeAssertionInvalid, http.StatusUnauthorized, func(t *testing.T, h *orHarness, c *operatorattest.Claims) (string, string, *string) {
			_, other, _ := ed25519.GenerateKey(rand.Reader)
			tok, _ := operatorattest.Sign(*c, "tangent-1", other)
			return tok, "approve", nil
		}},
		"unknown kid": {CodeAssertionInvalid, http.StatusUnauthorized, func(t *testing.T, h *orHarness, c *operatorattest.Claims) (string, string, *string) {
			tok, _ := operatorattest.Sign(*c, "tangent-9", h.priv)
			return tok, "approve", nil
		}},
		"six-minute-old step-up (elevation_ttl always)": {CodeAssertionStale, http.StatusUnauthorized, func(t *testing.T, h *orHarness, c *operatorattest.Claims) (string, string, *string) {
			c.ElevatedAt = time.Now().Unix() - 360
			return sign(h, t, *c), "approve", nil
		}},
		"note without its hash": {CodeAssertionUnbound, http.StatusUnauthorized, func(t *testing.T, h *orHarness, c *operatorattest.Claims) (string, string, *string) {
			return sign(h, t, *c), "approve", &note
		}},
		"request option differs": {CodeAssertionUnbound, http.StatusUnauthorized, func(t *testing.T, h *orHarness, c *operatorattest.Claims) (string, string, *string) {
			return sign(h, t, *c), "deny", nil
		}},
		"signed for another option": {CodeAssertionUnbound, http.StatusUnauthorized, func(t *testing.T, h *orHarness, c *operatorattest.Claims) (string, string, *string) {
			c.Option, c.Binding = "deny", operatorattest.Binding(c.MessageID, "deny")
			return sign(h, t, *c), "approve", nil
		}},
		"another device": {CodeAssertionUnbound, http.StatusUnauthorized, func(t *testing.T, h *orHarness, c *operatorattest.Claims) (string, string, *string) {
			c.DeviceID = "msg://device/other"
			return sign(h, t, *c), "approve", nil
		}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			h := newORHarness(t)
			id := h.approval(t)
			ch := h.challenge(t, "tachyon", id, "approve")
			c := h.claims(t, ch, nil)
			assertion, option, n := tc.m(t, h, &c)
			code, out := h.reply(t, "tachyon", id, ch, option, n, assertion)
			if code != tc.status || errCode(out) != tc.code {
				t.Fatalf("%d %v, want %d %s", code, out, tc.status, tc.code)
			}
			if h.replies(t, id) != 0 {
				t.Fatal("a refused reply was stored")
			}
			if event, d := h.lastAudit(t); event != AuditOperatorReplyRefused || d["code"] != tc.code {
				t.Fatalf("audit %s %v", event, d)
			}
			// The challenge was consumed by the failed attempt: a good assertion is now a replay.
			good := h.sign(t, h.claimsNoView(ch, id))
			if code, out = h.reply(t, "tachyon", id, ch, "approve", nil, good); code != http.StatusConflict || errCode(out) != CodeReplayed {
				t.Fatalf("retry on the consumed challenge: %d %v", code, out)
			}
		})
	}
}

// claimsNoView builds valid claims straight from the challenge (the view
// refuses a consumed challenge).
func (h *orHarness) claimsNoView(ch map[string]any, id string) operatorattest.Claims {
	now := time.Now().Unix()
	return operatorattest.Claims{Iss: "tangent", Aud: operatorattest.Audience, Sub: operatorattest.Subject, Jti: "jti-retry", Iat: now - 1, Exp: now + 60,
		ChallengeID: ch["challenge_id"].(string), Nonce: ch["nonce"].(string), MessageID: id, Option: "approve", Binding: operatorattest.Binding(id, "approve"),
		BodySHA256: operatorattest.SHA256Hex("Impact: hub restart. Rollback: redeploy the previous build."), DeviceID: tachyonDevice, ElevationID: "el-retry", ElevatedAt: now - 2}
}

func TestOperatorReplyChallengeRules(t *testing.T) {
	h := newORHarness(t)
	id := h.approval(t)
	other := h.approval(t)
	ch := h.challenge(t, "tachyon", id, "approve")
	good := h.sign(t, h.claims(t, ch, nil))
	for name, c := range map[string]struct {
		as, id string
		ch     map[string]any
	}{
		"unknown challenge":   {"tachyon", id, map[string]any{"challenge_id": "orc_nope"}},
		"another device's":    {"tachyon-2", id, ch},
		"for another message": {"tachyon", other, ch},
	} {
		if code, out := h.reply(t, c.as, c.id, c.ch, "approve", nil, good); code != http.StatusBadRequest || errCode(out) != CodeChallengeUnknown {
			t.Errorf("%s: %d %v", name, code, out)
		}
	}
	// Those did not consume it: the right device still can.
	if code, out := h.reply(t, "tachyon", id, ch, "approve", nil, good); code != http.StatusCreated {
		t.Fatalf("after the foreign attempts: %d %v", code, out)
	}

	// Expired: refused, and consumed anyway.
	id2 := h.approval(t)
	ch2 := h.challenge(t, "tachyon", id2, "approve")
	c2 := h.claims(t, ch2, nil)
	if _, err := h.db.DB().Exec(`UPDATE operator_reply_challenges SET expires_at=? WHERE challenge_id=?`, time.Now().Add(-time.Second).UTC().Format(time.RFC3339Nano), ch2["challenge_id"]); err != nil {
		t.Fatal(err)
	}
	if code, out := h.reply(t, "tachyon", id2, ch2, "approve", nil, h.sign(t, c2)); code != http.StatusBadRequest || errCode(out) != CodeChallengeExpired {
		t.Fatalf("expired: %d %v", code, out)
	}
	if c, _ := h.db.OperatorReplyChallenge(context.Background(), ch2["challenge_id"].(string)); c.ConsumedAt == nil {
		t.Fatal("an expired challenge was not consumed")
	}
}

// One step-up, one answer: a jti or elevation_id already spent is a replay on
// any other challenge.
func TestOperatorReplyProofsAreSingleUse(t *testing.T) {
	h := newORHarness(t)
	first := h.approval(t)
	ch := h.challenge(t, "tachyon", first, "approve")
	c := h.claims(t, ch, nil)
	if code, out := h.reply(t, "tachyon", first, ch, "approve", nil, h.sign(t, c)); code != http.StatusCreated {
		t.Fatalf("first: %d %v", code, out)
	}
	for name, reuse := range map[string]func(*operatorattest.Claims){
		"jti":          func(n *operatorattest.Claims) { n.Jti = c.Jti },
		"elevation_id": func(n *operatorattest.Claims) { n.ElevationID = c.ElevationID },
	} {
		id := h.approval(t)
		ch2 := h.challenge(t, "tachyon", id, "approve")
		n := h.claims(t, ch2, nil)
		reuse(&n)
		if code, out := h.reply(t, "tachyon", id, ch2, "approve", nil, h.sign(t, n)); code != http.StatusConflict || errCode(out) != CodeReplayed {
			t.Errorf("%s reused: %d %v", name, code, out)
		}
		if h.replies(t, id) != 0 {
			t.Errorf("%s reused: a reply was stored", name)
		}
	}
}

// Two open challenges for one card (approve and deny): the first reply wins,
// and the second is refused already_answered at reply time.
func TestOperatorReplyAlreadyAnsweredIsRecheckedAtReplyTime(t *testing.T) {
	h := newORHarness(t)
	id := h.approval(t)
	approve := h.challenge(t, "tachyon", id, "approve")
	deny := h.challenge(t, "tachyon", id, "deny")
	ca, cd := h.claims(t, approve, nil), h.claims(t, deny, nil)
	if code, out := h.reply(t, "tachyon", id, approve, "approve", nil, h.sign(t, ca)); code != http.StatusCreated {
		t.Fatalf("approve: %d %v", code, out)
	}
	if code, out := h.reply(t, "tachyon", id, deny, "deny", nil, h.sign(t, cd)); code != http.StatusConflict || errCode(out) != CodeAlreadyAnswered {
		t.Fatalf("deny after approve: %d %v", code, out)
	}
	if h.replies(t, id) != 1 {
		t.Fatalf("%d replies stored", h.replies(t, id))
	}
}

// Concurrent replies to one card through two challenges: exactly one is stored.
func TestOperatorReplyConcurrentRepliesStoreOne(t *testing.T) {
	h := newORHarness(t)
	id := h.approval(t)
	var chs []map[string]any
	var tokens []string
	for range 5 {
		ch := h.challenge(t, "tachyon", id, "approve")
		chs = append(chs, ch)
		tokens = append(tokens, h.sign(t, h.claims(t, ch, nil)))
	}
	var wg sync.WaitGroup
	codes := make([]int, len(chs))
	for i := range chs {
		wg.Go(func() { codes[i], _ = h.reply(t, "tachyon", id, chs[i], "approve", nil, tokens[i]) })
	}
	wg.Wait()
	created := 0
	for _, c := range codes {
		if c == http.StatusCreated {
			created++
		}
	}
	if created != 1 || h.replies(t, id) != 1 {
		t.Fatalf("codes %v, stored %d", codes, h.replies(t, id))
	}
}

// The reply-time re-check also sees an answer that arrived another way after
// the challenge (the operator replying from his own console).
func TestOperatorReplyRefusedWhenAnsweredAnotherWay(t *testing.T) {
	h := newORHarness(t)
	id := h.approval(t)
	ch := h.challenge(t, "tachyon", id, "approve")
	c := h.claims(t, ch, nil)
	from, _ := messaging.ParseURN(identity.OperatorID)
	to, _ := messaging.ParseURN(archieURN)
	if _, err := h.db.MessagingStore().Send(context.Background(), messaging.Envelope{From: from, To: to, Kind: "response", InReplyTo: id, Payload: []byte(`"no"`)}); err != nil {
		t.Fatal(err)
	}
	if code, out := h.reply(t, "tachyon", id, ch, "approve", nil, h.sign(t, c)); code != http.StatusConflict || errCode(out) != CodeAlreadyAnswered {
		t.Fatalf("after the operator answered directly: %d %v", code, out)
	}
	if h.replies(t, id) != 1 {
		t.Fatalf("%d replies", h.replies(t, id))
	}
}
