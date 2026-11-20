package correlationregistry

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRegisterWireAndReplay(t *testing.T) {
	o := Observation{URN: "msg://agent/local/one", Session: "s", RunID: "execution", TaskID: "task", WorkstreamID: "workstream:registered", JTI: "jti", ProviderSessionID: "native"}
	var bodies []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/v1/records" || r.Header.Get("Authorization") != "Bearer test-credential" {
			t.Errorf("incorrect request method/path/auth")
		}
		var b struct {
			Record struct {
				Schema, Key string
				Payload     Payload
			}
			IdempotencyKey string `json:"idempotency_key"`
		}
		if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
			t.Error(err)
		}
		if b.Record.Schema != "correlation/1" || b.Record.Key != "tether-execution-execution" || b.IdempotencyKey != b.Record.Key || b.Record.Payload.Metadata != o || b.Record.Payload.Links["workstream"].ID != o.WorkstreamID {
			t.Errorf("incorrect correlation: %+v", b)
		}
		raw, _ := json.Marshal(b)
		bodies = append(bodies, string(raw))
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	file := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(file, []byte("test-credential"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := Config{URL: server.URL, TokenFile: file}
	for range 2 {
		if err := Register(context.Background(), cfg, nil, o); err != nil {
			t.Fatal(err)
		}
	}
	if bodies[0] != bodies[1] {
		t.Fatal("replay request changed")
	}
}

func TestLegacyWorkstreamIsNotRegistryReference(t *testing.T) {
	p := (Observation{Session: "s", RunID: "r", WorkstreamID: "legacy-local"}).Payload()
	if _, exists := p.Links["workstream"]; exists {
		t.Fatal("legacy ID promoted to Registry authority")
	}
	if p.Metadata.WorkstreamID != "legacy-local" {
		t.Fatal("lost source observation")
	}
}

func TestRegisterDeadlineAndRedaction(t *testing.T) {
	file := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(file, []byte("private-credential"), 0600); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(time.Second):
		}
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := Register(ctx, Config{URL: server.URL, TokenFile: file}, nil, Observation{Session: "s", RunID: "r"}); err == nil || err.Error() != "correlation Registry unavailable" {
		t.Fatalf("unexpected error %v", err)
	}
}

func TestRegisterRejectsCredentialRedirect(t *testing.T) {
	file := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(file, []byte("private-credential"), 0600); err != nil {
		t.Fatal(err)
	}
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("followed redirect") }))
	defer target.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	if err := Register(context.Background(), Config{URL: server.URL, TokenFile: file}, nil, Observation{Session: "s", RunID: "r"}); err == nil || err.Error() != "correlation Registry HTTP 307" {
		t.Fatalf("unexpected error %v", err)
	}
}
