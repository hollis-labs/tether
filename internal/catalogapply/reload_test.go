package catalogapply

import (
	"context"
	"errors"
	"testing"
)

func applyWithReload(t *testing.T, reload func(context.Context) ([]string, error)) *Result {
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
	if err != nil {
		t.Fatal(err)
	}
	if read(t, e.Root, "agents/helper.yaml") == "" {
		t.Fatal("files did not land")
	}
	return res
}

func TestApplyReloadClean(t *testing.T) {
	res := applyWithReload(t, func(context.Context) ([]string, error) { return nil, nil })
	if !res.Reloaded || res.ReloadError != "" || len(res.RestartRequired) != 0 || res.Activation != ActivationImmediate {
		t.Fatalf("result = %+v", res)
	}
}

func TestApplyReloadRestartRequired(t *testing.T) {
	res := applyWithReload(t, func(context.Context) ([]string, error) { return []string{"defaults.state_db"}, nil })
	if !res.Reloaded || res.ReloadError != "" || len(res.RestartRequired) != 1 || res.RestartRequired[0] != "defaults.state_db" || res.Activation != ActivationRestartRequired {
		t.Fatalf("result = %+v", res)
	}
}

func TestApplyReloadError(t *testing.T) {
	res := applyWithReload(t, func(context.Context) ([]string, error) { return nil, errors.New("boom") })
	if !res.Applied || res.Reloaded || res.ReloadError != "boom" || res.Activation != ActivationRestartRequired {
		t.Fatalf("result = %+v", res)
	}
}
