package main

import (
	"testing"

	"github.com/hollis-labs/tether/internal/config"
)

func TestSyntheticConfiguredModelsCollision(t *testing.T) {
	providers := map[string]config.AIProviderConfig{
		"my-internal": {
			ID:              "my-internal",
			Type:            "openai-compatible",
			CatalogProvider: "internal-corp",
			Models:          []string{"gpt-4-turbo"},
		},
		"public": {
			ID:     "public",
			Type:   "openai",
			Models: []string{"gpt-4-turbo"},
		},
	}

	models, _ := syntheticConfiguredModels(providers)

	internalKey := "internal-corp\x00gpt-4-turbo"
	if _, ok := models[internalKey]; !ok {
		t.Errorf("missing internal model at %q", internalKey)
	}

	publicKey := "openai\x00gpt-4-turbo"
	if _, ok := models[publicKey]; !ok {
		t.Errorf("missing public model at %q", publicKey)
	}
}
