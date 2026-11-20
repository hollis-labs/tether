package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/catalogapply"
	"github.com/spf13/cobra"
)

func catalogCommandDaemon(t *testing.T, off bool) *catalogapply.Engine {
	t.Helper()
	remoteTargetFixture(t, "")
	isolateTetherPaths(t, t.TempDir())
	e := &catalogapply.Engine{Root: t.TempDir(), BackupRoot: t.TempDir(), ApprovalOff: off, Validate: func(string) error { return nil }}
	server := httptest.NewServer(api.NewHandler(api.Deps{CatalogApply: e}))
	t.Cleanup(server.Close)
	config := fmt.Sprintf("version: 0.1.0\nmodules:\n  remote_listener: true\ndaemon:\n  listen_addr: tcp:%s\n  shutdown_timeout: 10s\n", strings.TrimPrefix(server.URL, "http://"))
	writeCatalog(t, catalogPath, config)
	return e
}

func TestCatalogApproveAutoapprovedWithoutTerminal(t *testing.T) {
	e := catalogCommandDaemon(t, true)
	p, err := e.PlanBundle(context.Background(), catalogapply.Bundle{Name: "cli", Files: []catalogapply.File{{Path: "launches/dev.yaml", Content: "id: dev\nagent: dev\n"}}}, catalogapply.Options{})
	if err != nil {
		t.Fatal(err)
	}
	second, err := e.PlanBundle(context.Background(), catalogapply.Bundle{Name: "cli-batch", Files: []catalogapply.File{{Path: "launches/second.yaml", Content: "id: second\nagent: dev\n"}}}, catalogapply.Options{})
	if err != nil {
		t.Fatal(err)
	}
	// Exercise both single and batch command paths without stdin or a credential.
	for _, ids := range [][]string{{p.ID}, {p.ID, second.ID}} {
		command := &cobra.Command{Use: "approve", Args: catalogApproveCmd.Args, RunE: catalogApproveCmd.RunE}
		command.SetArgs(ids)
		command.SetIn(bytes.NewReader(nil))
		command.SetOut(&bytes.Buffer{})
		command.SetErr(&bytes.Buffer{})
		if err = command.ExecuteContext(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []string{p.ID, second.ID} {
		got, ok := e.Get(id)
		if !ok || got.ApprovedBy != "approval-disabled" || got.State != catalogapply.StateApproved {
			t.Fatalf("approval changed: %+v", got)
		}
	}
}

func TestCatalogPlanApplyCommandModes(t *testing.T) {
	for _, off := range []bool{false, true} {
		t.Run(fmt.Sprint(off), func(t *testing.T) {
			e := catalogCommandDaemon(t, off)
			command := newCatalogPlanCommand()
			command.SetArgs([]string{"--apply", "-"})
			command.SetIn(strings.NewReader(`{"name":"cli-apply","files":[{"path":"launches/dev.yaml","content":"id: dev\nagent: dev\n"}]}`))
			var output bytes.Buffer
			command.SetOut(&output)
			command.SetErr(&bytes.Buffer{})
			err := command.ExecuteContext(context.Background())
			if !off {
				if err == nil || !strings.Contains(err.Error(), "approval off") {
					t.Fatalf("on-mode error: %v", err)
				}
				if _, err = os.Stat(filepath.Join(e.Root, "launches/dev.yaml")); !os.IsNotExist(err) {
					t.Fatalf("on-mode installed: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var p catalogapply.Plan
			if err = json.Unmarshal(output.Bytes(), &p); err != nil {
				t.Fatal(err)
			}
			if p.State != catalogapply.StateApplied || p.ApprovedBy != "approval-disabled" || p.Result == nil || !p.Result.Applied {
				t.Fatalf("response: %s", output.String())
			}
			if _, err = os.Stat(filepath.Join(e.Root, "launches/dev.yaml")); err != nil {
				t.Fatal(err)
			}
		})
	}
}
