package daemon

import (
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/authissuer"
	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/environment"
	"github.com/hollis-labs/tether/internal/registry"
	"github.com/hollis-labs/tether/internal/store"
)

type boundServiceCatalog struct{ cat *config.Catalog }

func (c boundServiceCatalog) Load() (*config.Catalog, error) { return c.cat, nil }

func TestRemoteListenerBoundServiceMailProtocol(t *testing.T) {
	s, _, _ := remoteTestServer(t)
	db, err := store.Open(filepath.Join(t.TempDir(), "bound.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	reg := registry.NewService(registry.NewStorage(db.DB()))
	bound, err := reg.Register(t.Context(), registry.KindAgent, registry.Profile{DisplayName: "Bound"})
	if err != nil {
		t.Fatal(err)
	}
	other, err := reg.Register(t.Context(), registry.KindAgent, registry.Profile{DisplayName: "Other"})
	if err != nil {
		t.Fatal(err)
	}
	s.EventIngest = &fakeRemoteIngest{}
	s.Registry = reg
	s.MessageStore = db.MessagingStore()
	s.Aliases = db
	s.Catalog = boundServiceCatalog{&config.Catalog{Agents: map[string]config.Agent{"bound": {ID: "bound", URN: bound.URN}, "other": {ID: "other", URN: other.URN}}}}
	s.Issuer = serviceIssuer(t)
	s.Issuer.ServiceAddresses = reg
	token, _, err := s.Issuer.IssueService(evidenceOperator, authissuer.ServiceRequest{App: "surface", Audience: "tether", Scopes: []string{"messages", "read"}, Addresses: []string{bound.URN}})
	if err != nil {
		t.Fatal(err)
	}
	h := s.RemoteHandler("127.0.0.1:7331")
	call := func(method, path, body, protocol string, want int) []byte {
		r := httptest.NewRequest(method, "http://localhost:9000"+path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+token.Token)
		if protocol != "" {
			r.Header.Set(environment.ProtocolHeader, protocol)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != want {
			t.Fatalf("%s %s status=%d want=%d body=%s", method, path, w.Code, want, w.Body.String())
		}
		return w.Body.Bytes()
	}
	body := `{"kind":"message","to":["@other"],"body":"bound service"}`
	call("POST", "/messages/post", body, "", 409)
	raw := call("POST", "/messages/post", body, "1", 201)
	var typed api.MessagePostResponse
	if err := json.Unmarshal(raw, &typed); err != nil {
		t.Fatal(err)
	}
	message, err := db.MessagingStore().Get(t.Context(), typed.IDs[0])
	if err != nil || message.From.URN() != bound.URN || message.Metadata[api.MetaFromVerification] != "verified" {
		t.Fatal("typed remote sender not verified", err)
	}
	raw = call("POST", "/messages", `{"kind":"notice","from":"`+bound.URN+`","to":"`+bound.URN+`","payload":{}}`, "1", 201)
	var sent struct {
		ID       string            `json:"id"`
		Metadata map[string]string `json:"metadata"`
	}
	if err := json.Unmarshal(raw, &sent); err != nil || sent.Metadata[api.MetaFromVerification] != "verified" {
		t.Fatal("untyped remote sender not verified", err)
	}
	path := "/messages/list?to=" + url.QueryEscape(bound.URN) + "&as=" + url.QueryEscape(bound.URN)
	call("GET", path, "", "", 409)
	call("GET", path, "", "1", 200)
	call("POST", "/messages/"+sent.ID+"/read?as="+url.QueryEscape(bound.URN), "", "1", 204)
	call("GET", "/messages/list?to="+url.QueryEscape(other.URN)+"&as="+url.QueryEscape(other.URN), "", "1", 403)
	t.Log("PASS full RemoteHandler: typed/untyped verified send; required protocol header; bound list/mark-read; foreign mailbox denied")
}
