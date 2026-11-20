package app

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/runtoken"
	"github.com/hollis-labs/tether/internal/store"
)

func TestLaunchFillsCorrelationUnderGateBeforeTokenMint(t *testing.T) {
	svc, rt, id, _ := credentialLaunch(t)
	op := identity.Principal{Verified: true, ID: identity.OperatorID, Kind: "operator"}
	ctx := identity.WithPrincipal(context.Background(), op)
	input := api.SessionLaunchRequest{TaskID: "CW-20261120-0135", WorkstreamID: "01M7VQ2QWYB5RZR86W2BWG8PW3"}
	badCtx := identity.WithPrincipal(context.Background(), identity.Principal{Verified: true, ID: "session:agent", Kind: "session", SessionID: id})
	if _, err := svc.LaunchSessionWithCorrelation(badCtx, id, input); !errors.Is(err, ErrLaunchRefused) {
		t.Fatal("agent stamped through service", err)
	}
	unlock, err := svc.lockSessionLaunch(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	waitCtx, cancel := context.WithTimeout(ctx, 30*time.Millisecond)
	_, err = svc.LaunchSessionWithCorrelation(waitCtx, id, input)
	cancel()
	unlock()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("stamp bypassed canonical launch lock", err)
	}
	row, err := svc.Store.GetSession(id)
	if err != nil || row.TaskID.Valid || row.WorkstreamID.Valid {
		t.Fatal("waiting caller stamped before holding gate")
	}
	if _, err = svc.LaunchSessionWithCorrelation(ctx, id, input); err != nil {
		t.Fatal(err)
	}
	row, err = svc.Store.GetSession(id)
	if err != nil || row.TaskID.String != input.TaskID || row.WorkstreamID.String != input.WorkstreamID {
		t.Fatal("fill not persisted")
	}
	plan, err := svc.Store.GetLaunchPlan(id)
	if err != nil || plan.TaskID != input.TaskID || plan.WorkstreamID != input.WorkstreamID || plan.Env["TETHER_WORKSTREAM_ID"] != input.WorkstreamID {
		t.Fatal("plan/row fill diverged")
	}
	env := map[string]string{}
	for _, e := range rt.options.Env {
		k, v, _ := strings.Cut(e, "=")
		env[k] = v
	}
	p, err := identity.NewStore(svc.Store.DB()).Verify(ctx, env["TETHER_TOKEN"])
	if err != nil {
		t.Fatal(err)
	}
	claims, err := svc.VerifyRunToken(env[runtoken.TokenEnv], p)
	if err != nil || claims.Task != input.TaskID || claims.Workstream != input.WorkstreamID {
		t.Fatal("filled stamps absent in minted run token")
	}
	if _, err = svc.LaunchSessionWithCorrelation(ctx, id, api.SessionLaunchRequest{WorkstreamID: "other"}); !errors.Is(err, store.ErrSessionCorrelationConflict) {
		t.Fatal("replacement not refused", err)
	}
}

func TestRunningSessionCannotReceiveMissingCorrelation(t *testing.T) {
	svc, _, id, _ := credentialLaunch(t)
	if _, err := svc.LaunchSession(id); err != nil {
		t.Fatal(err)
	}
	ctx := identity.WithPrincipal(context.Background(), identity.Principal{Verified: true, ID: identity.OperatorID, Kind: "operator"})
	if _, err := svc.LaunchSessionWithCorrelation(ctx, id, api.SessionLaunchRequest{WorkstreamID: "new"}); !errors.Is(err, store.ErrSessionCorrelationConflict) {
		t.Fatal("running session filled", err)
	}
	row, err := svc.Store.GetSession(id)
	if err != nil || row.WorkstreamID.Valid {
		t.Fatal("refused fill changed row")
	}
}
