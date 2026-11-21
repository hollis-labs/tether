package api

import (
	"encoding/json"
	"net"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hollis-labs/libs/mesh/core/messaging"
	"github.com/hollis-labs/tether/internal/authissuer"
	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/registry"
	"github.com/hollis-labs/tether/internal/store"
)

// Real registry, issuer, signature verifier, remote admission, scope guard and
// mailbox store. No live credentials or daemon configuration are touched.
func TestBoundServiceRemoteMail(t *testing.T) {
	for _, mode := range []identity.Mode{identity.Observe, identity.Enforce} {
		t.Run(string(mode), func(t *testing.T) {
			db, err := store.Open(filepath.Join(t.TempDir(), "mail.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			reg := registry.NewService(registry.NewStorage(db.DB()))
			register := func(kind registry.Kind, name string, status registry.Status) registry.Profile {
				p, err := reg.Register(t.Context(), kind, registry.Profile{DisplayName: name, Status: status})
				if err != nil {
					t.Fatal(err)
				}
				return p
			}
			bound := register(registry.KindAgent, "bound", registry.StatusActive)
			other := register(registry.KindAgent, "other", registry.StatusActive)
			inactive := register(registry.KindAgent, "inactive", registry.StatusDeprecated)
			project := register(registry.KindProject, "project", registry.StatusActive)
			dir := t.TempDir()
			keys, err := authissuer.OpenKeyStore(dir, 0)
			if err != nil {
				t.Fatal(err)
			}
			state, err := authissuer.OpenState(filepath.Join(dir, "state.json"))
			if err != nil {
				t.Fatal(err)
			}
			is := authissuer.New("test", keys, state)
			is.ServiceAddresses = reg
			op := identity.Principal{ID: identity.OperatorID, Kind: "operator", Verified: true, Scopes: []string{"*"}}
			issue := func(addresses []string, want int) ServiceTokenResponse {
				body, _ := json.Marshal(ServiceTokenRequest{App: "surface", Audience: "tether", Scopes: []string{"messages", "read"}, Addresses: addresses})
				req := httptest.NewRequest("POST", ServiceTokensPath, strings.NewReader(string(body)))
				req = req.WithContext(identity.WithPrincipal(identity.ConnectionContext(req.Context(), &net.UnixConn{}), op))
				w := httptest.NewRecorder()
				NewTokenIssuerHandler(is).ServeHTTP(w, req)
				if w.Code != want {
					t.Fatalf("issuance status=%d want=%d", w.Code, want)
				}
				var out ServiceTokenResponse
				if want == 200 {
					if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
						t.Fatal(err)
					}
				}
				return out
			}
			for _, address := range []string{"msg://agent/agent-mux/agt_missing", inactive.URN, project.URN, "not-a-urn"} {
				issue([]string{address}, 400)
			}
			issued := issue([]string{bound.URN}, 200)
			if len(issued.Record.Addresses) != 1 || issued.Record.Addresses[0] != bound.URN {
				t.Fatal("binding missing from metadata")
			}
			legacy := issue(nil, 200)
			verifier, err := authissuer.NewVerifier(nil, is)
			if err != nil {
				t.Fatal(err)
			}
			p, err := verifier.Verify(t.Context(), issued.Token.Token)
			if err != nil || !p.MayActAs(bound.URN) || p.MayActAs(other.URN) {
				t.Fatal("JWT binding not confined", err)
			}
			cat := fixedCatalog{&config.Catalog{Agents: map[string]config.Agent{"bound": {ID: "bound", URN: bound.URN}, "other": {ID: "other", URN: other.URN}}}}
			api := NewHandler(Deps{Service: &fakeLaunchService{}, MessageStore: db.MessagingStore(), Registry: reg, Catalog: cat, Aliases: db, IdentityMode: mode})
			h := identity.RemoteMiddleware(nil, nil, RemoteScopeMiddleware(api), identity.WithServiceJWT(identity.ServiceJWT{Verify: verifier, Known: is.LiveService}))
			call := func(method, path, token, body string, want int) []byte {
				req := httptest.NewRequest(method, "http://localhost"+path, strings.NewReader(body))
				req.Header.Set("Authorization", "Bearer "+token)
				w := httptest.NewRecorder()
				h.ServeHTTP(w, req)
				if w.Code != want {
					t.Fatalf("%s %s status=%d want=%d body=%s", method, path, w.Code, want, w.Body.String())
				}
				return w.Body.Bytes()
			}
			send := func(from, to, token string, want int) messaging.Envelope {
				raw := call("POST", "/messages", token, `{"kind":"notice","from":"`+from+`","to":"`+to+`","payload":{"body":"test"}}`, want)
				if want != 201 {
					return messaging.Envelope{}
				}
				return envelopeFrom(t, raw, false)
			}
			sent := send(bound.URN, bound.URN, issued.Token.Token, 201)
			if sent.Metadata[MetaFromVerification] != "verified" {
				t.Fatal("untyped sender not verified")
			}
			wantSpoof := 201
			if mode == identity.Enforce {
				wantSpoof = 403
			}
			spoof := send(other.URN, bound.URN, issued.Token.Token, wantSpoof)
			if mode == identity.Observe && spoof.Metadata[MetaFromVerification] != "unverified" {
				t.Fatal("spoof not marked unverified")
			}
			old := send(legacy.Record.Sub, bound.URN, legacy.Token.Token, 201)
			if old.Metadata[MetaFromVerification] != "verified" {
				t.Fatal("legacy service sender changed")
			}
			call("POST", "/messages/post", issued.Token.Token, `{"kind":"message","from":"@other","to":["@other"],"body":"spoof"}`, 400)
			typedRaw := call("POST", "/messages/post", issued.Token.Token, `{"kind":"question","to":["@other"],"body":"Choose?","options":[{"id":"a","label":"A"},{"id":"b","label":"B"}],"default_option":"a"}`, 201)
			var typed MessagePostResponse
			if err := json.Unmarshal(typedRaw, &typed); err != nil {
				t.Fatal(err)
			}
			if typed.Envelope.From != "@bound" || typed.Envelope.EffectiveAttention() != "agent" || len(typed.IDs) != 1 {
				t.Fatalf("typed envelope %+v", typed.Envelope)
			}
			pending, err := db.PendingReplies(t.Context(), []string{other.URN}, "@other", 10)
			if err != nil || len(pending) != 1 || pending[0].MessageID != typed.IDs[0] {
				t.Fatal("typed question did not derive awaited recipient", err)
			}
			stored, err := db.MessagingStore().Get(t.Context(), typed.IDs[0])
			if err != nil || stored.From.URN() != bound.URN || stored.Metadata[MetaFromVerification] != "verified" {
				t.Fatal("typed sender not verified", err)
			}
			listPath := func(as string) string {
				return "/messages/list?as=" + url.QueryEscape(as) + "&to=" + url.QueryEscape(as)
			}
			raw := call("GET", listPath(bound.URN)+"&limit=2&offset=0&unread_only=true&include_archived=false&kind=notice", issued.Token.Token, "", 200)
			var page struct {
				Messages []store.Message
				Total    int
			}
			if err := json.Unmarshal(raw, &page); err != nil || page.Total < 1 {
				t.Fatal("bound list failed", err)
			}
			call("GET", listPath(other.URN), issued.Token.Token, "", 403)
			call("GET", listPath(bound.URN), legacy.Token.Token, "", 403)
			call("POST", "/messages/"+sent.ID+"/read?as="+url.QueryEscape(bound.URN), issued.Token.Token, "", 204)
			foreign := send(bound.URN, other.URN, issued.Token.Token, 201)
			call("POST", "/messages/"+foreign.ID+"/read?as="+url.QueryEscape(other.URN), issued.Token.Token, "", 403)
			call("POST", "/messages/"+foreign.ID+"/read?as="+url.QueryEscape(bound.URN), issued.Token.Token, "", 409)
			// CW-20261121-0142: GET /messages/{id} allows fetching own inbound or
			// outbound messages (to == bound or from == bound) and refuses another
			// mailbox's message with 403.
			otherAddr, err := messaging.ParseURN(other.URN)
			if err != nil {
				t.Fatal(err)
			}
			boundAddr, err := messaging.ParseURN(bound.URN)
			if err != nil {
				t.Fatal(err)
			}
			inbound, err := db.MessagingStore().Send(t.Context(), messaging.Envelope{
				Kind:    messaging.MsgKindNotice,
				From:    otherAddr,
				To:      boundAddr,
				Payload: json.RawMessage(`{"body":"inbound-for-bound"}`),
			})
			if err != nil {
				t.Fatal(err)
			}
			otherOnly, err := db.MessagingStore().Send(t.Context(), messaging.Envelope{
				Kind:    messaging.MsgKindNotice,
				From:    otherAddr,
				To:      otherAddr,
				Payload: json.RawMessage(`{"body":"private-to-other"}`),
			})
			if err != nil {
				t.Fatal(err)
			}
			for _, ownID := range []string{sent.ID, inbound.ID, foreign.ID} {
				rawGet := call("GET", "/messages/"+ownID+"?as="+url.QueryEscape(bound.URN), issued.Token.Token, "", 200)
				var got messaging.Envelope
				if err := json.Unmarshal(rawGet, &got); err != nil || got.ID != ownID {
					t.Fatalf("GET /messages/%s decoded id=%q err=%v", ownID, got.ID, err)
				}
			}
			// Another mailbox's message -> 403 (with either bound or foreign ?as=).
			call("GET", "/messages/"+otherOnly.ID+"?as="+url.QueryEscape(bound.URN), issued.Token.Token, "", 403)
			call("GET", "/messages/"+otherOnly.ID+"?as="+url.QueryEscape(other.URN), issued.Token.Token, "", 403)
			call("GET", "/messages/"+sent.ID+"?as="+url.QueryEscape(bound.URN), legacy.Token.Token, "", 403)
			// A mutable alias must not turn either the selector or a known message id
			// into permission to read another stored recipient's mail.
			if _, _, err := db.AddAddressAlias(t.Context(), bound.URN, other.URN, identity.OperatorID); err != nil {
				t.Fatal(err)
			}
			aliasTypedRaw := call("POST", "/messages/post", issued.Token.Token, `{"kind":"message","to":["@other"],"body":"literal sender after alias"}`, 201)
			var aliasTyped MessagePostResponse
			if err := json.Unmarshal(aliasTypedRaw, &aliasTyped); err != nil || aliasTyped.Envelope.From != "@bound" {
				t.Fatal("alias renamed typed service sender", err)
			}
			aliasSpoof := send(other.URN, other.URN, issued.Token.Token, wantSpoof)
			if mode == identity.Observe && aliasSpoof.Metadata[MetaFromVerification] != "unverified" {
				t.Fatal("alias widened sender grant")
			}
			raw = call("GET", listPath(bound.URN), issued.Token.Token, "", 200)
			if err := json.Unmarshal(raw, &page); err != nil {
				t.Fatal(err)
			}
			for _, m := range page.Messages {
				if m.To.URN() != bound.URN {
					t.Fatal("alias expanded mailbox authority")
				}
			}
			// Alias must not widen GET /messages/{id} to another mailbox's message either.
			call("GET", "/messages/"+otherOnly.ID+"?as="+url.QueryEscape(bound.URN), issued.Token.Token, "", 403)
			// Fetching own outbound message (from == bound, to == other) still works
			// after the alias and must not mark other's receipt as read.
			call("GET", "/messages/"+foreign.ID+"?as="+url.QueryEscape(bound.URN), issued.Token.Token, "", 200)
			call("POST", "/messages/"+foreign.ID+"/read?as="+url.QueryEscape(bound.URN), issued.Token.Token, "", 409)
			call("GET", listPath("msg://agent/agent-mux/bound"), issued.Token.Token, "", 403)
			for _, path := range []string{
				listPath(bound.URN) + "&as=" + url.QueryEscape(other.URN),
				listPath(bound.URN) + "&to=" + url.QueryEscape(other.URN),
				listPath(bound.URN) + "&unknown=1",
				listPath(bound.URN) + "&limit=1&limit=2",
				"/messages/list?as=x&to=y",
				"/messages/list?as=x&to=x&bad=%zz",
				"/messages/list?as=x;to=x",
				"/messages/" + sent.ID,
				"/messages/" + sent.ID + "/extra?as=" + url.QueryEscape(bound.URN),
				"/messages/" + sent.ID + "?as=" + url.QueryEscape(bound.URN) + "&to=" + url.QueryEscape(bound.URN),
				"/messages/inbox?as=" + url.QueryEscape(bound.URN),
				"/registry/agent",
				"/registry/agent/" + url.PathEscape(other.URN),
				"/registry/agents",
				"/registry/agents/",
				"/registry/agents/" + url.PathEscape(other.URN) + "/extra",
				"/registry/agents/" + url.PathEscape(other.URN) + "?full=true",
				"/registry/projects/" + url.PathEscape(project.URN),
			} {
				call("GET", path, issued.Token.Token, "", 401)
			}
			for _, path := range []string{"/messages/post?as=" + bound.URN, "/messages/post/", "/messages/%70ost", "/messages/x/archive?as=" + bound.URN, "/messages/x/read/", "/messages/x%2Fy/read?as=" + bound.URN, "/messages/x/read?as=" + bound.URN + "&as=" + other.URN, "/messages/x/read?as=" + bound.URN + "&to=" + bound.URN} {
				call("POST", path, issued.Token.Token, "{}", 401)
			}
			// CW-20261121-0142: registry agent read works with read scope (both Go
			// url.PathEscape and %3A-escaped forms) and registry_read scope, while
			// registry writes stay refused even for a registry-scoped service JWT.
			for _, regPath := range []string{
				"/registry/agents/" + url.PathEscape(other.URN),
				"/registry/agents/" + strings.ReplaceAll(url.PathEscape(other.URN), ":", "%3A"),
			} {
				regRaw := call("GET", regPath, issued.Token.Token, "", 200)
				var prof registry.Profile
				if err := json.Unmarshal(regRaw, &prof); err != nil || prof.URN != other.URN || prof.DisplayName != "other" {
					t.Fatalf("GET %s decoded=%+v err=%v", regPath, prof, err)
				}
			}
			regReadOnly, _, err := is.IssueService(op, authissuer.ServiceRequest{App: "surface", Audience: "tether", Scopes: []string{"registry_read"}})
			if err != nil {
				t.Fatal(err)
			}
			call("GET", "/registry/agents/"+url.PathEscape(other.URN), regReadOnly.Token, "", 200)
			regWriteSvc, _, err := is.IssueService(op, authissuer.ServiceRequest{App: "surface", Audience: "tether", Scopes: []string{"registry", "read"}})
			if err != nil {
				t.Fatal(err)
			}
			for _, c := range []struct{ method, path, body string }{
				{"POST", "/registry/agents", `{"display_name":"nope"}`},
				{"PATCH", "/registry/agents/" + url.PathEscape(other.URN), `{"title":"nope","last_updated_by":"svc"}`},
				{"PUT", "/registry/agents/" + url.PathEscape(other.URN), `{}`},
				{"DELETE", "/registry/agents/" + url.PathEscape(other.URN), ""},
				{"POST", "/registry/agents/" + url.PathEscape(other.URN) + "/sync", "{}"},
				{"POST", "/registry/agents/" + url.PathEscape(other.URN) + "/merge", `{"into":"` + bound.URN + `"}`},
				{"POST", "/registry/agents/" + url.PathEscape(other.URN) + "/external-id", `{"substrate":"x","external_id":"y"}`},
			} {
				call(c.method, c.path, regWriteSvc.Token, c.body, 401)
			}
			// Remote mailbox and registry scope checks remain separate from address grants.
			narrow, _, err := is.IssueService(op, authissuer.ServiceRequest{App: "surface", Audience: "tether", Scopes: []string{"messages"}, Addresses: []string{bound.URN}})
			if err != nil {
				t.Fatal(err)
			}
			call("GET", listPath(bound.URN), narrow.Token, "", 403)
			call("GET", "/messages/"+sent.ID+"?as="+url.QueryEscape(bound.URN), narrow.Token, "", 403)
			call("GET", "/registry/agents/"+url.PathEscape(other.URN), narrow.Token, "", 403)
			t.Log("PASS issuance 400 invalid/inactive/non-agent; JWT binding; typed+untyped verified; spoof observe/enforce; bound-only list/get/read with alias and selector attacks; registry agent read + write refusal; legacy sender; scope and URL gates")
		})
	}
}
