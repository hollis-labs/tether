package main

import (
	"testing"

	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/modelcatalog"
)

func TestSyntheticModelsCarryDeclaredCapabilities(t *testing.T) {
	providers := map[string]config.AIProviderConfig{
		"local": {
			ID: "local", Type: "openai-compatible", Models: []string{"qwen3-coder"}, Price: "free",
			Capabilities: &config.AICapabilitiesConfig{ToolCall: true, Reasoning: true},
		},
		"bare": {ID: "bare", Type: "openai-compatible", Models: []string{"plain"}},
	}
	models, prices := syntheticConfiguredModels(providers)

	withCaps := models["openai\x00qwen3-coder"]
	if !withCaps.Capabilities.ToolCall || !withCaps.Capabilities.Reasoning || withCaps.Capabilities.Attachment {
		t.Fatalf("declared capabilities = %+v", withCaps.Capabilities)
	}
	if prices["openai\x00qwen3-coder"] != modelcatalog.PriceFree {
		t.Fatalf("price state = %v, want free", prices["openai\x00qwen3-coder"])
	}
	bare := models["openai\x00plain"]
	if bare.Capabilities.ToolCall || bare.Capabilities.Reasoning {
		t.Fatalf("undeclared capabilities must stay empty: %+v", bare.Capabilities)
	}
}
