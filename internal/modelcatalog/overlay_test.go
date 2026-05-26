package modelcatalog

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hollis-labs/go-modelsdev/modelsdev"
)

func TestOverlayIncludesSyntheticConfiguredModel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{
			"openai": {
				"id": "openai",
				"name": "OpenAI",
				"models": {
					"gpt-5-mini": {
						"id": "gpt-5-mini",
						"name": "GPT-5 mini",
						"family": "gpt-5",
						"open_weights": false,
						"cost": {"input": 1, "output": 2},
						"limit": {"context": 128000, "output": 16000},
						"modalities": {"input": ["text"], "output": ["text"]},
						"tool_call": true
					}
				}
			}
		}`))
	}))
	defer srv.Close()

	base := New(
		modelsdev.WithURL(srv.URL),
		modelsdev.WithCacheDir(t.TempDir()),
	)
	if err := base.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	overlay := NewOverlay(base, map[string]modelsdev.Model{
		overlayKey("openai", "llama3.3-custom"): {
			ID:     "llama3.3-custom",
			Name:   "llama3.3-custom",
			Family: "openai",
			Modality: modelsdev.Modality{
				Input:  []string{"text"},
				Output: []string{"text"},
			},
		},
	})

	if _, ok := overlay.Get("openai", "llama3.3-custom"); !ok {
		t.Fatal("Get synthetic model returned !ok")
	}
	refs := overlay.List()
	if len(refs) != 2 {
		t.Fatalf("List len = %d, want 2", len(refs))
	}
	foundSynthetic := false
	for _, ref := range refs {
		if ref.ID == "llama3.3-custom" {
			foundSynthetic = true
			if len(ref.Modality.Input) != 1 || ref.Modality.Input[0] != "text" {
				t.Fatalf("synthetic modalities = %+v", ref.Modality)
			}
		}
	}
	if !foundSynthetic {
		t.Fatal("synthetic model missing from List")
	}
}
