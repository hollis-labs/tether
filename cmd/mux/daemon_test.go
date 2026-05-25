package main

import (
	"testing"

	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/llm/router"
)

func TestEffectiveAIDefaultPolicy(t *testing.T) {
	globalFalse := false
	providerTrue := true

	allowReasoning, allowTools, allowAttachments := effectiveAIDefaultPolicy(
		config.AIPolicyConfig{
			AllowReasoning:   &globalFalse,
			AllowTools:       &globalFalse,
			AllowAttachments: &globalFalse,
		},
		config.AIPolicyConfig{
			AllowReasoning: &providerTrue,
		},
	)

	if allowReasoning == nil || !*allowReasoning {
		t.Fatalf("allowReasoning = %v, want true", allowReasoning)
	}
	if allowTools == nil || *allowTools {
		t.Fatalf("allowTools = %v, want false", allowTools)
	}
	if allowAttachments == nil || *allowAttachments {
		t.Fatalf("allowAttachments = %v, want false", allowAttachments)
	}
}

func TestEffectiveAIRoutePolicy(t *testing.T) {
	globalFalse := false
	providerTrue := true
	routeFalse := false

	allowReasoning, allowTools, allowAttachments := effectiveAIRoutePolicy(
		config.AIPolicyConfig{
			AllowReasoning:   &globalFalse,
			AllowTools:       &globalFalse,
			AllowAttachments: &globalFalse,
		},
		config.AIPolicyConfig{
			AllowReasoning: &providerTrue,
			AllowTools:     &providerTrue,
		},
		config.AIRouteConfig{
			AllowTools:       &routeFalse,
			AllowAttachments: &providerTrue,
		},
	)

	if allowReasoning == nil || !*allowReasoning {
		t.Fatalf("allowReasoning = %v, want true", allowReasoning)
	}
	if allowTools == nil || *allowTools {
		t.Fatalf("allowTools = %v, want false", allowTools)
	}
	if allowAttachments == nil || !*allowAttachments {
		t.Fatalf("allowAttachments = %v, want true", allowAttachments)
	}
}

func TestCoalescePolicyNumbers(t *testing.T) {
	globalMaxOutput := 2048
	providerMaxOutput := 1024
	routeMaxOutput := 512
	globalMaxCost := 0.50
	providerMaxCost := 0.20
	routeMaxCost := 0.10

	if got := coalesceInt(&routeMaxOutput, coalesceInt(&providerMaxOutput, &globalMaxOutput)); got == nil || *got != 512 {
		t.Fatalf("coalesceInt route = %v", got)
	}
	if got := coalesceInt(nil, coalesceInt(&providerMaxOutput, &globalMaxOutput)); got == nil || *got != 1024 {
		t.Fatalf("coalesceInt provider = %v", got)
	}
	if got := coalesceFloat64(&routeMaxCost, coalesceFloat64(&providerMaxCost, &globalMaxCost)); got == nil || *got != 0.10 {
		t.Fatalf("coalesceFloat64 route = %v", got)
	}
	if got := coalesceFloat64(nil, coalesceFloat64(&providerMaxCost, &globalMaxCost)); got == nil || *got != 0.20 {
		t.Fatalf("coalesceFloat64 provider = %v", got)
	}
}

func TestEffectiveAIUsageBudget(t *testing.T) {
	global := config.AIUsageBudgetPolicyConfig{MaxCostUSD: daemonFloatPtr(5.0), Window: "month"}
	provider := config.AIUsageBudgetPolicyConfig{MaxCostUSD: daemonFloatPtr(2.0), Scope: "caller"}
	route := config.AIUsageBudgetPolicyConfig{MaxCostUSD: daemonFloatPtr(1.0), Window: "day"}

	got := effectiveAIUsageBudget(global, provider, route)
	want := router.UsageBudgetPolicy{
		Level:      "route",
		MaxCostUSD: daemonFloatPtr(1.0),
		Window:     "day",
		Scope:      "caller",
	}
	if got.Level != want.Level || got.Window != want.Window || got.Scope != want.Scope || got.MaxCostUSD == nil || *got.MaxCostUSD != *want.MaxCostUSD {
		t.Fatalf("effectiveAIUsageBudget = %+v, want %+v", got, want)
	}

	defaultGot := effectiveAIDefaultUsageBudget(global, provider)
	if defaultGot.Level != "provider" || defaultGot.MaxCostUSD == nil || *defaultGot.MaxCostUSD != 2.0 || defaultGot.Window != "month" || defaultGot.Scope != "caller" {
		t.Fatalf("effectiveAIDefaultUsageBudget = %+v", defaultGot)
	}
}

func daemonFloatPtr(v float64) *float64 {
	return &v
}
