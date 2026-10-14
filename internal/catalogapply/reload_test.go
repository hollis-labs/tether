package catalogapply

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

func applyWithReload(t *testing.T, reload func(context.Context) ([]string, error)) (*Engine, *Result, error) {
	t.Helper()
	e := newEngine(t)
	e.Reload = reload
	ctx := context.Background()
	p, err := e.PlanBundle(ctx, Bundle{Name: "reload", Files: []File{
		{Path: "agents/helper.yaml", Content: "id: helper\nname: Helper\n"},
	}}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	approve(t, e, p)
	res, err := e.Apply(ctx, p.ID)
	return e, res, err
}

func applyLanded(t *testing.T, reload func(context.Context) ([]string, error)) *Result {
	t.Helper()
	e, res, err := applyWithReload(t, reload)
	if err != nil {
		t.Fatal(err)
	}
	if read(t, e.Root, "agents/helper.yaml") == "" {
		t.Fatal("files did not land")
	}
	return res
}

func TestApplyReloadClean(t *testing.T) {
	res := applyLanded(t, func(context.Context) ([]string, error) { return nil, nil })
	if !res.Reloaded || res.ReloadError != "" || len(res.RestartRequired) != 0 || res.Activation != ActivationImmediate {
		t.Fatalf("result = %+v", res)
	}
}

func TestApplyReloadRestartRequired(t *testing.T) {
	res := applyLanded(t, func(context.Context) ([]string, error) { return []string{"defaults.state_db"}, nil })
	if !res.Reloaded || res.ReloadError != "" || len(res.RestartRequired) != 1 || res.RestartRequired[0] != "defaults.state_db" || res.Activation != ActivationRestartRequired {
		t.Fatalf("result = %+v", res)
	}
}

func TestApplyReloadRefusedRestoresFiles(t *testing.T) {
	e, res, err := applyWithReload(t, func(context.Context) ([]string, error) { return nil, errors.New("boom") })
	if err == nil || res == nil || res.Applied || !res.RolledBack || res.ReloadError != "boom" {
		t.Fatalf("res = %+v err = %v", res, err)
	}
	if _, serr := os.Stat(filepath.Join(e.Root, "agents/helper.yaml")); !errors.Is(serr, fs.ErrNotExist) {
		t.Fatalf("refused file was not restored: %v", serr)
	}
}

func TestTeamsRestartNote(t *testing.T) {
	plan := func(path, action string) *Plan { return &Plan{Files: []PlannedFile{{Path: path, Action: action}}} }
	if teamsRestartNote(plan("teams-policy.yaml", "modify")) == "" || teamsRestartNote(plan("team-definitions/a.md", "create")) == "" {
		t.Fatal("teams policy and definition edits must report restart_required")
	}
	if teamsRestartNote(plan("teams-policy.yaml", "unchanged")) != "" || teamsRestartNote(plan("agents/a.yaml", "modify")) != "" {
		t.Fatal("unrelated or unchanged files must not")
	}
}
