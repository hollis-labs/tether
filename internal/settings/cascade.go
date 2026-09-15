package settings

// ResolveEffectiveOnboarding resolves the effective onboarding settings using the
// closest-wins precedence cascade:
//  1. User settings (if specified and non-empty)
//  2. Project settings (if specified and non-empty)
//  3. Global settings (if specified and non-empty)
//  4. Code-level fallbacks
//
// This reuses the exact closest-wins resolution pattern proven in
// internal/config/extract_refs.go (EffectiveExtractRefs) and
// internal/config/permission.go (EffectivePermissionMode).
func ResolveEffectiveOnboarding(global, project, user *OnboardingSettings) OnboardingSettings {
	var effective OnboardingSettings

	// 1. RequiredProps: closest non-nil slice wins.
	if user != nil && user.RequiredProps != nil {
		effective.RequiredProps = make([]string, len(user.RequiredProps))
		copy(effective.RequiredProps, user.RequiredProps)
	} else if project != nil && project.RequiredProps != nil {
		effective.RequiredProps = make([]string, len(project.RequiredProps))
		copy(effective.RequiredProps, project.RequiredProps)
	} else if global != nil && global.RequiredProps != nil {
		effective.RequiredProps = make([]string, len(global.RequiredProps))
		copy(effective.RequiredProps, global.RequiredProps)
	} else {
		effective.RequiredProps = []string{}
	}

	// 2. MCPOptInOffered: closest non-nil *bool wins.
	if user != nil && user.MCPOptInOffered != nil {
		val := *user.MCPOptInOffered
		effective.MCPOptInOffered = &val
	} else if project != nil && project.MCPOptInOffered != nil {
		val := *project.MCPOptInOffered
		effective.MCPOptInOffered = &val
	} else if global != nil && global.MCPOptInOffered != nil {
		val := *global.MCPOptInOffered
		effective.MCPOptInOffered = &val
	}

	// 3. DefaultLLMPolicy: closest non-empty string wins.
	if user != nil && user.DefaultLLMPolicy != "" {
		effective.DefaultLLMPolicy = user.DefaultLLMPolicy
	} else if project != nil && project.DefaultLLMPolicy != "" {
		effective.DefaultLLMPolicy = project.DefaultLLMPolicy
	} else if global != nil && global.DefaultLLMPolicy != "" {
		effective.DefaultLLMPolicy = global.DefaultLLMPolicy
	}

	// 4. Custom map: closest-wins key overlay (global -> project -> user).
	customCount := 0
	if global != nil {
		customCount += len(global.Custom)
	}
	if project != nil {
		customCount += len(project.Custom)
	}
	if user != nil {
		customCount += len(user.Custom)
	}

	if customCount > 0 {
		effective.Custom = make(map[string]string)
		if global != nil {
			for k, v := range global.Custom {
				effective.Custom[k] = v
			}
		}
		if project != nil {
			for k, v := range project.Custom {
				effective.Custom[k] = v
			}
		}
		if user != nil {
			for k, v := range user.Custom {
				effective.Custom[k] = v
			}
		}
	}

	return effective
}

// EffectiveValue resolves a single string setting across the cascade.
// Precedence: user (if non-nil/non-empty) > project (if non-nil/non-empty) > global (if non-nil/non-empty) > fallback.
func EffectiveValue(global, project, user *string, fallback string) string {
	if user != nil && *user != "" {
		return *user
	}
	if project != nil && *project != "" {
		return *project
	}
	if global != nil && *global != "" {
		return *global
	}
	return fallback
}
