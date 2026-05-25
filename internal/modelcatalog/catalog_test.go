package modelcatalog

import (
	"context"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hollis-labs/go-modelsdev/modelsdev"
)

func TestCatalogLookupAndEstimateCost(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{
			"anthropic": {
				"id": "anthropic",
				"name": "Anthropic",
				"env": ["ANTHROPIC_API_KEY"],
				"models": {
					"claude-sonnet-4-5": {
						"id": "claude-sonnet-4-5",
						"name": "Claude Sonnet 4.5",
						"family": "claude",
						"open_weights": false,
						"release_date": "2026-01-01",
						"knowledge": "2025-10",
						"last_updated": "2026-05-24",
						"cost": {"input": 3, "output": 15},
						"limit": {"context": 200000, "output": 16000},
						"modalities": {"input": ["text", "image"], "output": ["text"]},
						"tool_call": true,
						"reasoning": true,
						"attachment": true,
						"temperature": true
					}
				}
			}
		}`))
	}))
	defer srv.Close()

	cat := New(
		modelsdev.WithURL(srv.URL),
		modelsdev.WithCacheDir(t.TempDir()),
	)
	if err := cat.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	m, ok := cat.Get("anthropic", "claude-sonnet-4-5")
	if !ok {
		t.Fatal("Get returned !ok")
	}
	if m.Name != "Claude Sonnet 4.5" {
		t.Fatalf("Name = %q", m.Name)
	}

	in, out, ok := cat.Pricing("anthropic", "claude-sonnet-4-5")
	if !ok || in != 3 || out != 15 {
		t.Fatalf("Pricing = (%v, %v, %v), want (3, 15, true)", in, out, ok)
	}

	ctxWindow, ok := cat.ContextWindow("anthropic", "claude-sonnet-4-5")
	if !ok || ctxWindow != 200000 {
		t.Fatalf("ContextWindow = (%d, %v)", ctxWindow, ok)
	}

	maxOutput, ok := cat.MaxOutput("anthropic", "claude-sonnet-4-5")
	if !ok || maxOutput != 16000 {
		t.Fatalf("MaxOutput = (%d, %v)", maxOutput, ok)
	}

	caps, ok := cat.Capabilities("anthropic", "claude-sonnet-4-5")
	if !ok || !caps.ToolCall || !caps.Reasoning || !caps.Attachment || !caps.Temperature {
		t.Fatalf("Capabilities = %+v, ok=%v", caps, ok)
	}

	modality, ok := cat.Modality("anthropic", "claude-sonnet-4-5")
	if !ok || len(modality.Input) != 2 || modality.Input[1] != "image" {
		t.Fatalf("Modality = %+v, ok=%v", modality, ok)
	}

	cost, ok := cat.EstimateCost("anthropic", "claude-sonnet-4-5", 1000, 500)
	if !ok {
		t.Fatal("EstimateCost returned !ok")
	}
	wantCost := 1000*3.0/1_000_000 + 500*15.0/1_000_000
	if math.Abs(cost-wantCost) > 1e-12 {
		t.Fatalf("EstimateCost = %f, want %f", cost, wantCost)
	}
}
