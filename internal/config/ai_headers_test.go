package config

import (
	"strings"
	"testing"
)

func TestValidateAI_ProviderHeaders(t *testing.T) {
	ai := func(typ string, headers map[string]string) AIConfig {
		return AIConfig{Providers: []AIProviderConfig{{
			ID: "p", Type: typ, BaseURL: "http://x", SecretRef: "keychain://x", Models: []string{"m"}, Headers: headers, Enabled: true,
		}}}
	}
	cases := []struct {
		name    string
		cfg     AIConfig
		wantErr string
	}{
		{"ok compatible", ai("openai-compatible", map[string]string{"x-opencode-session": "tether"}), ""},
		{"ok openai", ai("openai", map[string]string{"x-client": "tether"}), ""},
		{"authorization", ai("openai", map[string]string{"Authorization": "Bearer x"}), "credential header"},
		{"api key", ai("openai", map[string]string{"X-Api-Key": "x"}), "credential header"},
		{"empty name", ai("openai", map[string]string{" ": "x"}), "empty header name"},
		{"ok anthropic", ai("anthropic", map[string]string{"x-opencode-session": "tether"}), ""},
		{"wrong type", ai("gemini", map[string]string{"x-a": "b"}), "only supported for openai"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateAIConfig(tc.cfg)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("unexpected error: %v", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Fatalf("error = %v, want containing %q", err, tc.wantErr)
			}
		})
	}
}
