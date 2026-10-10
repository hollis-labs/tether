package environment

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestProfilesPreserveLegacyAndWorkerIsolation(t *testing.T) {
	legacy, err := ResolveProfile("", nil, true, true)
	if err != nil || !legacy.Enabled(Teams) || !legacy.Enabled(LLMGateway) || legacy.Enabled(EnvironmentDirectory) {
		t.Fatal(legacy, err)
	}
	legacyOff, err := ResolveProfile("", nil, false, false)
	if err != nil || legacyOff.Enabled(Teams) || legacyOff.Enabled(LLMGateway) {
		t.Fatal(legacyOff, err)
	}
	worker, err := ResolveProfile("worker", nil, true, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, module := range []string{Teams, LLMGateway, EnvironmentDirectory, ConnectionManager} {
		if worker.Enabled(module) {
			t.Fatalf("worker enabled hub module %s", module)
		}
	}
	for _, module := range []string{SessionCore, LocalMCP, Messaging, CredentialBroker, RemoteListener, StreamAPI, Lifecycle} {
		if !worker.Enabled(module) {
			t.Fatalf("worker disabled %s", module)
		}
	}
	overrides := map[string]bool{Teams: true, Messaging: false}
	p, err := ResolveProfile("worker", overrides, true, true)
	if err != nil || !p.Enabled(Teams) || p.Enabled(Messaging) {
		t.Fatal(p, err)
	}
	overrides[Messaging] = true
	if p.Enabled(Messaging) {
		t.Fatal("profile retained mutable configuration map")
	}
	if _, err := ResolveProfile("misspelled", nil, false, false); err == nil {
		t.Fatal("unknown role accepted")
	}
	if _, err := ResolveProfile("worker", map[string]bool{"misspelled": true}, false, false); err == nil {
		t.Fatal("unknown module accepted")
	}
}

func TestModuleGateLeavesHealthAndExistingReadsAvailable(t *testing.T) {
	p, err := ResolveProfile("worker", map[string]bool{Messaging: false, StreamAPI: false, Lifecycle: false}, true, true)
	if err != nil {
		t.Fatal(err)
	}
	gate := p.ModuleGate(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusAccepted) }))
	for _, tc := range []struct {
		method, path string
		want         int
	}{
		{http.MethodGet, "/health", 202}, {http.MethodGet, DescriptorPath, 202},
		{http.MethodGet, "/sessions/existing", 202}, {http.MethodPost, "/sessions", 404},
		{http.MethodPost, "/sessions/existing/stop", 404}, {http.MethodGet, "/environment/snapshot", 404},
		{http.MethodGet, "/sessions/existing/stream", 404}, {http.MethodPost, "/messages", 404},
		{http.MethodGet, "/ai/providers", 404}, {http.MethodGet, "/teams/existing", 404},
	} {
		w := httptest.NewRecorder()
		gate.ServeHTTP(w, httptest.NewRequest(tc.method, tc.path, nil))
		if w.Code != tc.want {
			t.Errorf("%s %s = %d want %d", tc.method, tc.path, w.Code, tc.want)
		}
	}
}
