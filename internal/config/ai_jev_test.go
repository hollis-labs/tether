package config

import (
	"strings"
	"testing"
)

func TestValidateAI_JEVProvider(t *testing.T) {
	price := 0.042
	negative := -1.0
	jev := func(mod func(*AIProviderConfig)) AIConfig {
		p := AIProviderConfig{
			ID:                   "jev",
			Type:                 "jev",
			Models:               []string{"jev-1.13.0"},
			SecretRef:            "keychain://typesafe/jev",
			InputPriceUSDPerMTok: &price,
			Enabled:              true,
		}
		if mod != nil {
			mod(&p)
		}
		return AIConfig{Providers: []AIProviderConfig{p}}
	}

	tests := []struct {
		name    string
		ai      AIConfig
		wantErr string
	}{
		{"valid", jev(nil), ""},
		{"missing secret_ref", jev(func(p *AIProviderConfig) { p.SecretRef = "" }), "missing secret_ref"},
		{"missing model", jev(func(p *AIProviderConfig) { p.Models = nil }), "missing model"},
		{"negative price", jev(func(p *AIProviderConfig) { p.InputPriceUSDPerMTok = &negative }), "must be >= 0"},
		{"price on another type", AIConfig{Providers: []AIProviderConfig{{
			ID: "a", Type: "anthropic", Models: []string{"m"}, SecretRef: "keychain://a", InputPriceUSDPerMTok: &price, Enabled: true,
		}}}, "only supported for jev"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateAIConfig(tc.ai)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("unexpected error: %v", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Fatalf("error = %v, want containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestValidateAI_PurposeBudgetScope(t *testing.T) {
	if err := validateAIUsageBudgetPolicyConfig("p", AIUsageBudgetPolicyConfig{Scope: "purpose"}); err != nil {
		t.Fatalf("purpose scope should be valid: %v", err)
	}
	if err := validateAIUsageBudgetPolicyConfig("p", AIUsageBudgetPolicyConfig{Scope: "galaxy"}); err == nil {
		t.Fatal("unknown scope must be rejected")
	}
}
