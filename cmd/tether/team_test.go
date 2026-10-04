package main

import (
	"bytes"
	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/config"
	"github.com/spf13/cobra"
	"io"
	"strings"
	"testing"
)

func TestTeamDefaultOffCommandSeam(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		missing    bool
	}{
		{"omitted", "version: 1\n", false}, {"off", "version: 1\nteams:\n  enabled: false\n", false},
		{"invalid", "teams: [invalid\n", false}, {"missing", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if !tc.missing {
				dir = writeCatalog(t, dir, tc.body)
			}
			if tc.name == "omitted" || tc.name == "off" {
				cat, err := config.Load(dir)
				if err != nil || cat.Global.Teams.Enabled {
					t.Fatal("default on", err)
				}
			}
			root := &cobra.Command{Use: "tether"}
			existing := 0
			root.AddCommand(&cobra.Command{Use: "existing", Run: func(*cobra.Command, []string) { existing++ }})
			root = buildRootCommand(root, []string{"--catalog", dir}, "unused")
			root.SetOut(io.Discard)
			root.SetErr(io.Discard)
			for _, command := range root.Commands() {
				if command.Name() == "team" {
					t.Fatal("disabled namespace registered")
				}
			}
			for _, verb := range api.TeamVerbs() {
				for _, flags := range [][]string{nil, {"--key", "key", "--request", "{}"}} {
					root.SetArgs(append([]string{"team", verb}, flags...))
					err := root.Execute()
					if err == nil || !strings.Contains(err.Error(), "unknown command") {
						t.Fatal("disabled command", verb, err)
					}
				}
			}
			var help bytes.Buffer
			root.SetOut(&help)
			root.SetArgs([]string{"--help"})
			if err := root.Execute(); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(help.String(), "Manage teams") {
				t.Fatal("team visible in help")
			}
			help.Reset()
			root.SetArgs([]string{"__complete", ""})
			if err := root.Execute(); err != nil {
				t.Fatal(err)
			}
			for _, line := range strings.Split(help.String(), "\n") {
				if line == "team" || strings.HasPrefix(line, "team\t") {
					t.Fatal("disabled team completion present")
				}
			}
			root.SetArgs([]string{"existing"})
			if err := root.Execute(); err != nil || existing != 1 {
				t.Fatal("existing command changed", err)
			}
		})
	}
}

func TestTeamExplicitConfigCommandSeam(t *testing.T) {
	dir := writeCatalog(t, t.TempDir(), "version: 1\nteams:\n  enabled: true\n")
	for _, args := range [][]string{{"--catalog=" + dir}, {"--catalog", dir}} {
		root := buildRootCommand(&cobra.Command{Use: "tether"}, args, "unused")
		root = buildRootCommand(root, args, "unused")
		count := 0
		for _, command := range root.Commands() {
			if command.Name() == "team" {
				count++
			}
		}
		if count != 1 {
			t.Fatal("enabled namespace count", count)
		}
		for _, verb := range api.TeamVerbs() {
			cmd, _, err := root.Find([]string{"team", verb})
			if err != nil || cmd.Name() != verb {
				t.Fatal("enabled verb missing", verb, err)
			}
		}
		var output bytes.Buffer
		root.SetOut(&output)
		root.SetErr(io.Discard)
		root.SetArgs([]string{"team", "--help"})
		if err := root.Execute(); err != nil || !strings.Contains(output.String(), "report_result") {
			t.Fatal("enabled help", err)
		}
	}
}
