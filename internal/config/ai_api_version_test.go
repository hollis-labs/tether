package config

import (
	"strings"
	"testing"
)

func TestValidateAI_APIVersionOnlyForGemini(t *testing.T) {
	ok := AIConfig{Providers: []AIProviderConfig{{
		ID: "g", Type: "gemini", Models: []string{"m"}, SecretRef: "keychain://g", APIVersion: "v1", Enabled: true,
	}}}
	if err := validateAIConfig(ok); err != nil {
		t.Fatalf("gemini api_version should be valid: %v", err)
	}
	bad := AIConfig{Providers: []AIProviderConfig{{
		ID: "a", Type: "anthropic", Models: []string{"m"}, SecretRef: "keychain://a", APIVersion: "v1", Enabled: true,
	}}}
	if err := validateAIConfig(bad); err == nil || !strings.Contains(err.Error(), "only supported for gemini") {
		t.Fatalf("error = %v", err)
	}
}
