package config_test

import (
	"context"
	"testing"

	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/launchprofile"
)

func TestCatalogImplementsLaunchProfileSource(t *testing.T) {
	cat := &config.Catalog{
		Projects: map[string]config.LaunchContext{
			"demo-proj": {
				ID:       "demo-proj",
				Name:     "Demo Project",
				RepoRoot: "/repo/demo",
			},
		},
		Agents: map[string]config.LaunchProfile{
			"demo-agent": {
				ID:       "demo-agent",
				Name:     "Demo Agent",
				Provider: "claude-code",
				Skills:   []string{"git"},
			},
		},
		Launches: map[string]config.Launch{
			"legacy-launch-x": {
				ID:       "legacy-launch-x",
				Project:  "demo-proj",
				Agent:    "demo-agent",
				Provider: "claude-stream", // legacy launch specifies provider
			},
		},
	}

	ctx := context.Background()

	// Direct lookup
	p, err := cat.GetProfile(ctx, "demo-agent")
	if err != nil {
		t.Fatalf("GetProfile(demo-agent) failed: %v", err)
	}
	if p.ID != "demo-agent" || p.Provider != "claude-code" {
		t.Errorf("unexpected profile: %+v", p)
	}

	c, err := cat.GetContext(ctx, "demo-proj")
	if err != nil {
		t.Fatalf("GetContext(demo-proj) failed: %v", err)
	}
	if c.ID != "demo-proj" || c.RepoRoot != "/repo/demo" {
		t.Errorf("unexpected context: %+v", c)
	}

	// Legacy launch lookup via Source interface
	var src launchprofile.Source = cat
	lp, err := src.GetProfile(ctx, "legacy-launch-x")
	if err != nil {
		t.Fatalf("src.GetProfile(legacy-launch-x) failed: %v", err)
	}
	// Resolved profile inherits agent details and launch provider
	if lp.ID != "demo-agent" || lp.Provider != "claude-stream" {
		t.Errorf("unexpected legacy profile resolution: %+v", lp)
	}

	lc, err := src.GetContext(ctx, "legacy-launch-x")
	if err != nil {
		t.Fatalf("src.GetContext(legacy-launch-x) failed: %v", err)
	}
	if lc.ID != "demo-proj" {
		t.Errorf("unexpected legacy context resolution: %+v", lc)
	}

	// Compose legacy launch via Resolver
	resolver := launchprofile.NewResolver(src)
	comp, err := resolver.Resolve(ctx, launchprofile.CompositionInput{
		Target: "legacy-launch-x",
	})
	if err != nil {
		t.Fatalf("resolver.Resolve(legacy-launch-x) failed: %v", err)
	}
	if comp.Provider != "claude-stream" {
		t.Errorf("comp.Provider = %q, want claude-stream", comp.Provider)
	}
}
