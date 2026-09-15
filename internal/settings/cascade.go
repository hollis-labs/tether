package settings

// ResolveEffectiveOnboarding resolves the effective onboarding settings across the
// Global > Project > User cascade, splitting resolution rules by kind rather than by field:
//
//   - Scalars (MCPOptInOffered, DefaultLLMPolicy): closest-wins override.
//     Precedence: User > Project > Global > code-level fallbacks.
//
//   - Collections (RequiredProps, Custom): accumulate across scopes.
//     RequiredProps forms a union (Global ∪ Project ∪ User) — closer scopes can only add
//     requirements, never remove one mandated by a farther scope.
//     Custom maps overlay key-by-key from Global -> Project -> User.
func ResolveEffectiveOnboarding(global, project, user *OnboardingSettings) OnboardingSettings {
	var effective OnboardingSettings

	// 1. RequiredProps: collection accumulation (global ∪ project ∪ user).
	// Closer scopes can add requirements, never remove one mandated by a farther scope.
	seenProps := make(map[string]struct{})
	effective.RequiredProps = []string{}
	addProps := func(props []string) {
		for _, p := range props {
			if _, ok := seenProps[p]; !ok {
				seenProps[p] = struct{}{}
				effective.RequiredProps = append(effective.RequiredProps, p)
			}
		}
	}
	if global != nil {
		addProps(global.RequiredProps)
	}
	if project != nil {
		addProps(project.RequiredProps)
	}
	if user != nil {
		addProps(user.RequiredProps)
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
