package modelcatalog

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hollis-labs/substrate/llm-core/modelsdev"
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
	}, map[string]PriceState{
		overlayKey("openai", "llama3.3-custom"): PriceUnknown,
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

func TestOverlay_EstimateCost(t *testing.T) {
	base := (*Catalog)(nil)
	ov := NewOverlay(base, map[string]modelsdev.Model{
		overlayKey("synthetic", "free-model"): {
			ID:     "free-model",
			Family: "synthetic",
			Cost:   modelsdev.Pricing{},
		},
		overlayKey("synthetic", "priced-model"): {
			ID:     "priced-model",
			Family: "synthetic",
			Cost:   modelsdev.Pricing{Input: 1.0, Output: 2.0},
		},
	}, map[string]PriceState{
		overlayKey("synthetic", "free-model"):    PriceFree,
		overlayKey("synthetic", "unknown-model"): PriceUnknown,
		overlayKey("synthetic", "priced-model"):  PricePriced,
	})

	tests := []struct {
		providerID, modelID string
		wantCost            float64
		wantState           PriceState
	}{
		{"synthetic", "unknown-model", 0, PriceUnknown},
		{"synthetic", "free-model", 0, PriceFree},
		{"synthetic", "priced-model", 0.003, PricePriced},
	}

	for _, tt := range tests {
		t.Run(tt.providerID+"-"+tt.modelID, func(t *testing.T) {
			cost, state := ov.EstimateCost(tt.providerID, tt.modelID, 1000, 1000)
			t.Logf("extra=%v, state=%v cost=%v", ov.extra, state, cost)
			if state != tt.wantState {
				t.Errorf("state = %v, want %v", state, tt.wantState)
			}
			if cost != tt.wantCost {
				t.Errorf("cost = %v, want %v", cost, tt.wantCost)
			}
		})
	}
}
