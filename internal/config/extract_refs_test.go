package config

import "testing"

func TestEffectiveExtractRefs(t *testing.T) {
	tests := []struct {
		name   string
		global Global
		proj   Project
		launch Launch
		want   bool
	}{
		{
			name:   "all unset defaults to false",
			global: Global{},
			proj:   Project{},
			launch: Launch{},
			want:   false,
		},
		{
			name: "global default true",
			global: Global{
				Catalog: CatalogRoots{
					Defaults: Defaults{
						ExtractRefs: true,
					},
				},
			},
			proj:   Project{},
			launch: Launch{},
			want:   true,
		},
		{
			name: "project true overrides global false",
			global: Global{
				Catalog: CatalogRoots{
					Defaults: Defaults{
						ExtractRefs: false,
					},
				},
			},
			proj: Project{
				MCP: MCPConfig{
					ExtractRefs: boolPtr(true),
				},
			},
			launch: Launch{},
			want:   true,
		},
		{
			name: "project false overrides global true",
			global: Global{
				Catalog: CatalogRoots{
					Defaults: Defaults{
						ExtractRefs: true,
					},
				},
			},
			proj: Project{
				MCP: MCPConfig{
					ExtractRefs: boolPtr(false),
				},
			},
			launch: Launch{},
			want:   false,
		},
		{
			name: "launch true overrides project false",
			global: Global{
				Catalog: CatalogRoots{
					Defaults: Defaults{
						ExtractRefs: false,
					},
				},
			},
			proj: Project{
				MCP: MCPConfig{
					ExtractRefs: boolPtr(false),
				},
			},
			launch: Launch{
				MCP: MCPConfig{
					ExtractRefs: boolPtr(true),
				},
			},
			want: true,
		},
		{
			name: "launch false overrides project true and global true",
			global: Global{
				Catalog: CatalogRoots{
					Defaults: Defaults{
						ExtractRefs: true,
					},
				},
			},
			proj: Project{
				MCP: MCPConfig{
					ExtractRefs: boolPtr(true),
				},
			},
			launch: Launch{
				MCP: MCPConfig{
					ExtractRefs: boolPtr(false),
				},
			},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := EffectiveExtractRefs(tt.global, tt.proj, tt.launch)
			if got != tt.want {
				t.Errorf("EffectiveExtractRefs() = %v, want %v", got, tt.want)
			}
		})
	}
}
