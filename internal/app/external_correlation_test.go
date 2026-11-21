package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hollis-labs/tether/internal/agencyregistry"
	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/identity"
)

func TestExternalCorrelationEndpointAsyncRetry(t *testing.T) {
	svc, _, localID, _ := credentialLaunch(t)
	entered, release, received := make(chan struct{}), make(chan struct{}), make(chan agencyregistry.Facts, 1)
	var mu sync.Mutex
	var bodies, keys []string
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/correlations/register" || r.Header.Get("Authorization") != "Bearer fake-registry-test-credential" {
			t.Error("wrong Registry route or credential")
		}
		var f agencyregistry.Facts
		if err := json.NewDecoder(r.Body).Decode(&f); err != nil {
			t.Error(err)
		}
		b, _ := json.Marshal(f)
		mu.Lock()
		bodies = append(bodies, string(b))
		keys = append(keys, r.Header.Get("Idempotency-Key"))
		n := len(keys)
		mu.Unlock()
		if n == 1 {
			close(entered)
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
			w.WriteHeader(503)
			return
		}
		received <- f
		w.WriteHeader(200)
	}))
	defer remote.Close()
	defer svc.stopLaunchCorrelations()
	path := filepath.Join(t.TempDir(), "credential")
	if err := os.WriteFile(path, []byte("fake-registry-test-credential"), 0600); err != nil {
		t.Fatal(err)
	}
	svc.Catalog.Global.AgencyRegistry = agencyregistry.Config{URL: remote.URL, CredentialFile: path}
	h := api.NewHandler(api.Deps{Correlations: svc})
	op := identity.Principal{Verified: true, ID: identity.OperatorID, Kind: "operator"}
	call := func(body string, p identity.Principal) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/correlation/register", strings.NewReader(body)).WithContext(identity.WithPrincipal(context.Background(), p))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	const body = `{"urn":"msg://agent/local/kit","session":"provider-session","task_id":"CW-20261120-0007","workstream_id":"workstream:01M7VQ2QWYB5RZR86W2BWG8PW3","export_receipt_id":"receipt:boot","activity_sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- call(body, op) }()
	select {
	case w := <-done:
		if w.Code != 202 || !strings.Contains(w.Body.String(), `"queued":true`) {
			t.Fatal(w.Code, w.Body.String())
		}
	case <-time.After(time.Second):
		t.Fatal("endpoint waited on Registry")
	}
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("no remote registration")
	}
	close(release)
	select {
	case f := <-received:
		if f.RunID != "provider-session" || f.Session != "provider-session" || f.ExportReceiptID != "receipt:boot" || f.TokenJTI != "" || f.TaskID != "CW-20261120-0007" || f.ActivitySHA256 != strings.Repeat("a", 64) {
			t.Fatal("wrong external facts", f)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("no retry")
	}
	mu.Lock()
	if len(keys) != 2 || keys[0] == "" || keys[0] != keys[1] || bodies[0] != bodies[1] {
		t.Error("retry changed facts or key")
	}
	mu.Unlock()
	w := call(`{"urn":"msg://agent/local/kit","session":"`+localID+`"}`, op)
	if w.Code != 202 || !strings.Contains(w.Body.String(), "tether_session") {
		t.Fatal("did not skip local session", w.Code, w.Body.String())
	}
	for _, bad := range []string{`{"urn":"msg://agent/local/kit","session":"s","id":"caller-minted"}`, `{"urn":"msg://agent/local/kit","session":"s","token_jti":"claimed"}`, `{"urn":"msg://agent/local/kit","session":"s","workstream_id":"legacy-label"}`, `{"urn":"msg://agent/local/kit","session":"s"} {}`} {
		if w := call(bad, op); w.Code != 400 {
			t.Fatal("invalid facts admitted", w.Code)
		}
	}
	for _, p := range []identity.Principal{{}, {Verified: true, ID: "msg://agent/local/kit", Kind: "agent"}, {Verified: true, ID: identity.OperatorID, Kind: "operator", TokenID: "narrow"}} {
		if w := call(body, p); w.Code != 403 {
			t.Fatal("non-operator admitted", w.Code)
		}
	}
}

func TestExternalCorrelationDisabledAndShutdown(t *testing.T) {
	svc, _, _, _ := credentialLaunch(t)
	in := api.CorrelationRegisterRequest{URN: "msg://agent/local/kit", Session: "external", RunID: "execution"}
	out, err := svc.RegisterExternalCorrelation(context.Background(), in)
	if err != nil || out.Queued || out.Reason != "disabled" {
		t.Fatal(out, err)
	}
	svc.Catalog.Global.AgencyRegistry = agencyregistry.Config{URL: "http://invalid.example"}
	svc.stopLaunchCorrelations()
	out, err = svc.RegisterExternalCorrelation(context.Background(), in)
	if err != nil || out.Queued || out.Reason != "queue_unavailable" {
		t.Fatal(out, err)
	}
}
