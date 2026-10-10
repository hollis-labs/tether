package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/hollis-labs/tether/internal/config"
	llmopenai "github.com/hollis-labs/tether/internal/llm/openai"
)

func modelsServer(t *testing.T, status int, body string) (*httptest.Server, *int) {
	t.Helper()
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

func TestApplyModelDiscoveryMergesAfterConfigured(t *testing.T) {
	srv, calls := modelsServer(t, http.StatusOK, `{"data":[{"id":"vendor/b"},{"id":"vendor/a"},{"id":"vendor/c"}]}`)
	p := config.AIProviderConfig{
		ID: "gw", Type: "openai-compatible", BaseURL: srv.URL + "/v1",
		Models: []string{"vendor/a"}, DiscoverModels: true, Enabled: true,
	}

	got := applyModelDiscovery(context.Background(), p, nil, llmopenai.DiscoverModels)

	if *calls != 1 {
		t.Fatalf("/models calls = %d, want 1", *calls)
	}
	if want := []string{"vendor/a", "vendor/b", "vendor/c"}; !reflect.DeepEqual(got.Models, want) {
		t.Fatalf("Models = %v, want %v", got.Models, want)
	}
	if got.EffectiveDefaultModel() != "vendor/a" {
		t.Fatalf("default model = %q, want the configured vendor/a", got.EffectiveDefaultModel())
	}
}

func TestApplyModelDiscoveryKeepsSingleModelAsDefault(t *testing.T) {
	srv, _ := modelsServer(t, http.StatusOK, `{"data":[{"id":"x"},{"id":"only"}]}`)
	p := config.AIProviderConfig{
		ID: "gw", Type: "openai-compatible", BaseURL: srv.URL + "/v1",
		Model: "only", DiscoverModels: true, Enabled: true,
	}

	got := applyModelDiscovery(context.Background(), p, nil, llmopenai.DiscoverModels)

	if want := []string{"only", "x"}; !reflect.DeepEqual(got.EffectiveModels(), want) {
		t.Fatalf("EffectiveModels = %v, want %v", got.EffectiveModels(), want)
	}
	if got.EffectiveDefaultModel() != "only" {
		t.Fatalf("default model = %q, want only", got.EffectiveDefaultModel())
	}
}

func TestApplyModelDiscoverySendsResolvedKey(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"data":[{"id":"gpt-x"}]}`))
	}))
	t.Cleanup(srv.Close)
	p := config.AIProviderConfig{
		ID: "oa", Type: "openai", BaseURL: srv.URL, SecretRef: "keychain://x",
		Models: []string{"gpt-4o"}, DiscoverModels: true, Enabled: true,
	}

	got := applyModelDiscovery(context.Background(), p, func(context.Context) (string, error) { return "sk-test", nil }, llmopenai.DiscoverModels)

	if gotAuth != "Bearer sk-test" {
		t.Fatalf("Authorization = %q, want bearer token", gotAuth)
	}
	if want := []string{"gpt-4o", "gpt-x"}; !reflect.DeepEqual(got.Models, want) {
		t.Fatalf("Models = %v, want %v", got.Models, want)
	}
}

func TestApplyModelDiscoveryFailuresKeepConfigured(t *testing.T) {
	notFound, _ := modelsServer(t, http.StatusNotFound, `not found`)
	malformed, _ := modelsServer(t, http.StatusOK, `{"data":`)
	empty, _ := modelsServer(t, http.StatusOK, `{"data":[]}`)

	tests := []struct {
		name     string
		baseURL  string
		resolve  func(context.Context) (string, error)
		discover modelDiscoverer
	}{
		{name: "404", baseURL: notFound.URL + "/v1", discover: llmopenai.DiscoverModels},
		{name: "malformed body", baseURL: malformed.URL + "/v1", discover: llmopenai.DiscoverModels},
		{name: "empty list", baseURL: empty.URL + "/v1", discover: llmopenai.DiscoverModels},
		{
			name: "timeout",
			discover: func(context.Context, llmopenai.DiscoveryConfig) ([]string, error) {
				return nil, errors.New("discover models: timed out after 5s")
			},
		},
		{
			name:     "secret resolution fails",
			resolve:  func(context.Context) (string, error) { return "", errors.New("no such secret") },
			discover: func(context.Context, llmopenai.DiscoveryConfig) ([]string, error) { panic("discover must not run") },
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := config.AIProviderConfig{
				ID: "gw", Type: "openai-compatible", BaseURL: tc.baseURL,
				Models: []string{"m1", "m2"}, DiscoverModels: true, Enabled: true,
			}
			got := applyModelDiscovery(context.Background(), p, tc.resolve, tc.discover)
			if !reflect.DeepEqual(got, p) {
				t.Fatalf("provider changed on failure: got %+v, want %+v", got, p)
			}
		})
	}
}

func TestApplyModelDiscoveryOptInOnly(t *testing.T) {
	srv, calls := modelsServer(t, http.StatusOK, `{"data":[{"id":"extra"}]}`)
	for _, p := range []config.AIProviderConfig{
		{ID: "off", Type: "openai-compatible", BaseURL: srv.URL + "/v1", Models: []string{"m"}, Enabled: true},
		{ID: "an", Type: "anthropic", BaseURL: srv.URL + "/v1", Models: []string{"m"}, DiscoverModels: true, Enabled: true},
	} {
		got := applyModelDiscovery(context.Background(), p, nil, llmopenai.DiscoverModels)
		if !reflect.DeepEqual(got, p) {
			t.Fatalf("%s: provider changed: got %+v", p.ID, got)
		}
	}
	if *calls != 0 {
		t.Fatalf("/models calls = %d, want 0 when discovery is not enabled for the connection", *calls)
	}
}

// Discovered ids must land under the connection's own catalog identity, not
// under the wire-type id, so they cannot collide with the public catalog.
func TestApplyModelDiscoveryCatalogCollision(t *testing.T) {
	srv, _ := modelsServer(t, http.StatusOK, `{"data":[{"id":"gpt-4o"},{"id":"vendor/new"}]}`)
	gw := applyModelDiscovery(context.Background(), config.AIProviderConfig{
		ID: "gw", Type: "openai-compatible", CatalogProvider: "my-gateway", BaseURL: srv.URL + "/v1",
		Models: []string{"vendor/a"}, DiscoverModels: true, Enabled: true,
	}, nil, llmopenai.DiscoverModels)
	public := config.AIProviderConfig{
		ID: "oa", Type: "openai", SecretRef: "keychain://x", Models: []string{"gpt-4o"}, Enabled: true,
	}

	models := syntheticConfiguredModels(map[string]config.AIProviderConfig{"gw": gw, "oa": public})

	for _, key := range []string{"my-gateway\x00vendor/a", "my-gateway\x00gpt-4o", "my-gateway\x00vendor/new", "openai\x00gpt-4o"} {
		if _, ok := models[key]; !ok {
			t.Errorf("missing synthetic model %q", key)
		}
	}
	if _, ok := models["openai\x00vendor/new"]; ok {
		t.Errorf("discovered id leaked under the wire-type catalog id")
	}
	if len(models) != 4 {
		t.Errorf("synthetic models = %d, want 4", len(models))
	}
}
