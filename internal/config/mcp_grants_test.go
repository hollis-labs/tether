package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"encoding/json"
	"gopkg.in/yaml.v3"
)

func TestLoadRejectsInvalidMCPGrants(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, tc := range []struct{ name, path, body, owner, bad string }{
		{"typo", "projects/p.yaml", "id: p\nmcp:\n  servers: [torqe]\n", `project "p"`, "torqe"},
		{"wrong case", "launches/l.yaml", "id: l\nmcp:\n  servers: [Torque]\n", `launch "l"`, "Torque"},
		{"disabled", "boot-profiles/b.yaml", "id: b\nmcp_servers: [disabled]\n", `boot profile "b"`, "disabled"},
		{"agent env", "agents/a.yaml", "id: a\nenv:\n  TETHER_MCP_SERVERS: missing\n", `agent "a"`, "missing"},
		{"provider override", "agents/a.yaml", "id: a\nprovider_overrides:\n  codex:\n    env:\n      TETHER_MCP_SERVERS: missing\n", `agent "a" provider "codex"`, "missing"},
		{"launch override", "launches/l.yaml", "id: l\noverrides:\n  env:\n    TETHER_MCP_SERVERS: missing\n", `launch "l"`, "missing"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			write := func(name, body string) {
				t.Helper()
				path := filepath.Join(root, name)
				if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(body), 0600); err != nil {
					t.Fatal(err)
				}
			}
			write("global.yaml", "{}\n")
			write("mcp-servers/torque.yaml", "id: torque\ntransport: stdio\ncommand: never-execute\ntoken: helper://never/resolve\n")
			write("mcp-servers/disabled.yaml", "id: disabled\nenabled: false\n")
			write(tc.path, tc.body)
			for _, load := range []func(string) (*Catalog, error){Load, LoadLayered} {
				_, err := load(root)
				if !errors.Is(err, ErrInvalidMCPGrant) || !strings.Contains(err.Error(), tc.owner) || !strings.Contains(err.Error(), tc.bad) {
					t.Fatalf("load error: %v", err)
				}
			}
			write(tc.path, strings.ReplaceAll(tc.body, tc.bad, "torque"))
			if _, err := Load(root); err != nil {
				t.Fatalf("valid grant should load without resolving secrets: %v", err)
			}
		})
	}
}

func TestMCPGrantEmptyRoundTrip(t *testing.T) {
	for _, text := range []string{"{}", "servers: null", "servers: []", "servers: [torque]"} {
		var in MCPConfig
		if err := yaml.Unmarshal([]byte(text), &in); err != nil {
			t.Fatal(err)
		}
		for _, codec := range []struct {
			marshal   func(any) ([]byte, error)
			unmarshal func([]byte, any) error
		}{{yaml.Marshal, yaml.Unmarshal}, {json.Marshal, json.Unmarshal}} {
			data, err := codec.marshal(in)
			if err != nil {
				t.Fatal(err)
			}
			var out MCPConfig
			if err := codec.unmarshal(data, &out); err != nil {
				t.Fatal(err)
			}
			if (in.Servers == nil) != (out.Servers == nil) || len(in.Servers) != len(out.Servers) {
				t.Fatalf("%s lost inheritance/empty semantics via %s", text, data)
			}
		}
	}
}

func TestLayeredAgentMCPGrantValidation(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "global.yaml"), []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(home, ".tether", "agents")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "user.yaml"), []byte("id: user\nenv:\n  TETHER_MCP_SERVERS: missing\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(root); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadLayered(root); !errors.Is(err, ErrInvalidMCPGrant) || !strings.Contains(err.Error(), `agent "user"`) {
		t.Fatalf("layered grant error: %v", err)
	}
}
