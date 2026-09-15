package settings

import (
	"fmt"
	"time"
)

// Scope identifies the precedence tier in the settings cascade.
// Precedence: User > Project > Global (closest wins).
type Scope string

const (
	// ScopeGlobal represents deployment/fleet-wide default settings.
	ScopeGlobal Scope = "global"

	// ScopeProject represents settings specific to a project entity.
	ScopeProject Scope = "project"

	// ScopeUser represents user-specific settings.
	ScopeUser Scope = "user"
)

// ValidScope reports whether s is a supported scope.
func ValidScope(s Scope) bool {
	switch s {
	case ScopeGlobal, ScopeProject, ScopeUser:
		return true
	default:
		return false
	}
}

// KeyOnboarding is the standard settings key for OnboardingSettings.
const KeyOnboarding = "onboarding"

// Setting represents a raw scoped configuration entry in the settings store.
type Setting struct {
	Scope     Scope     `json:"scope"`
	ScopeID   string    `json:"scope_id"` // Empty for ScopeGlobal; project URN/ID for ScopeProject; user URN/ID for ScopeUser.
	Key       string    `json:"key"`
	ValueJSON string    `json:"value_json"`
	UpdatedAt time.Time `json:"updated_at"`
}

// OnboardingSettings models configuration governing what a given deployment,
// project, or user expects during project onboarding (CW-20260914-0042).
type OnboardingSettings struct {
	// RequiredProps lists prop keys that must be supplied during Step 1/Step 2 onboarding.
	RequiredProps []string `json:"required_props,omitempty"`

	// MCPOptInOffered specifies whether Step 5 (MCP tool opt-in) is enabled during onboarding.
	MCPOptInOffered *bool `json:"mcp_opt_in_offered,omitempty"`

	// DefaultLLMPolicy specifies the default LLM policy or model family preference.
	DefaultLLMPolicy string `json:"default_llm_policy,omitempty"`

	// Custom holds arbitrary key-value settings for forward compatibility.
	Custom map[string]string `json:"custom,omitempty"`
}

// Validate checks that the scope and scope_id are consistent.
func (s Setting) Validate() error {
	if !ValidScope(s.Scope) {
		return fmt.Errorf("invalid settings scope %q", s.Scope)
	}
	if s.Scope == ScopeGlobal && s.ScopeID != "" {
		return fmt.Errorf("global scope must have empty scope_id")
	}
	if (s.Scope == ScopeProject || s.Scope == ScopeUser) && s.ScopeID == "" {
		return fmt.Errorf("%s scope requires non-empty scope_id", s.Scope)
	}
	if s.Key == "" {
		return fmt.Errorf("settings key cannot be empty")
	}
	return nil
}
