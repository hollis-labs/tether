package mcpadapter

import (
	"context"
	"errors"
	"time"

	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/client"
	"github.com/hollis-labs/tether/internal/teamsvc"
)

// clientTeamOps satisfies api.TeamOps over the daemon /teams/* routes using the
// client's own credential. The daemon resolves the caller; nothing here can
// assert a principal.
type clientTeamOps struct{ c *client.Client }

var _ api.TeamOps = clientTeamOps{}

func (o clientTeamOps) call(ctx context.Context, verb, key string, req api.TeamRequest) (teamsvc.Result, error) {
	res, err := o.c.Team(ctx, verb, key, req)
	var te *client.TeamError
	if errors.As(err, &te) {
		switch te.Code {
		case "unauthenticated":
			return res, teamsvc.ErrUnauthenticated
		case "denied":
			return res, teamsvc.ErrDenied
		case "not_found":
			return res, teamsvc.ErrNotFound
		case "conflict":
			return res, teamsvc.ErrConflict
		case "invalid_request":
			return res, teamsvc.ErrInvalidRequest
		case "unavailable", "teams_disabled":
			return res, teamsvc.ErrUnavailable
		}
		return res, errors.New(te.Code)
	}
	return res, err
}

func (o clientTeamOps) Form(ctx context.Context, r teamsvc.FormRequest) (teamsvc.Result, error) {
	team := r.Team
	return o.call(ctx, "form", r.Key, api.TeamRequest{Team: &team, Launch: api.TeamLaunchRequest{Counts: r.Launch.Counts, Limits: r.Launch.Limits, PoolIdentities: r.Launch.PoolIdentities}})
}
func (o clientTeamOps) Dissolve(ctx context.Context, r teamsvc.RunRequest) (teamsvc.Result, error) {
	return o.call(ctx, "dissolve", r.Key, api.TeamRequest{RunID: r.RunID})
}
func (o clientTeamOps) AddMember(ctx context.Context, r teamsvc.AddMemberRequest) (teamsvc.Result, error) {
	return o.call(ctx, "member_add", r.Key, api.TeamRequest{RunID: r.RunID, Slot: r.Slot, Limits: r.Limits})
}
func (o clientTeamOps) RemoveMember(ctx context.Context, r teamsvc.RemoveMemberRequest) (teamsvc.Result, error) {
	return o.call(ctx, "member_remove", r.Key, api.TeamRequest{RunID: r.RunID, MemberID: r.MemberID})
}
func (o clientTeamOps) Adopt(ctx context.Context, r teamsvc.AdoptRequest) (teamsvc.Result, error) {
	return o.call(ctx, "adopt", r.Key, api.TeamRequest{RunID: r.RunID, Slot: r.Slot, Actor: r.Actor, SessionID: r.SessionID, BindingGeneration: r.BindingGeneration})
}
func (o clientTeamOps) msg(ctx context.Context, verb string, r teamsvc.MessageRequest) (teamsvc.Result, error) {
	return o.call(ctx, verb, r.Key, api.TeamRequest{RunID: r.RunID, Address: r.Address, Body: r.Body, Kind: r.Kind, History: r.History})
}
func (o clientTeamOps) Assign(ctx context.Context, r teamsvc.MessageRequest) (teamsvc.Result, error) {
	return o.msg(ctx, "assign", r)
}
func (o clientTeamOps) Delegate(ctx context.Context, r teamsvc.MessageRequest) (teamsvc.Result, error) {
	return o.msg(ctx, "delegate", r)
}
func (o clientTeamOps) Address(ctx context.Context, r teamsvc.MessageRequest) (teamsvc.Result, error) {
	return o.msg(ctx, "address", r)
}
func (o clientTeamOps) Cancel(ctx context.Context, r teamsvc.CancelRequest) (teamsvc.Result, error) {
	return o.call(ctx, "cancel", r.Key, api.TeamRequest{RunID: r.RunID, MemberID: r.MemberID, Cascade: r.Cascade})
}
func (o clientTeamOps) ReportResult(ctx context.Context, r teamsvc.ReportResultRequest) (teamsvc.Result, error) {
	return o.call(ctx, "report_result", r.Key, api.TeamRequest{RunID: r.RunID, DelegateKey: r.DelegateKey, Body: r.Body})
}

// EnableTeamsFromDaemon exposes tether_team_* through this out-of-process
// adapter when the owning daemon serves /teams/*. It probes the roster route
// once; an unreachable daemon or teams_disabled leaves the tools unregistered.
// Call before the server is created.
func (a *Adapter) EnableTeamsFromDaemon(ctx context.Context) bool {
	if a.client == nil || api.HasTeamOps(a.teams) {
		return api.HasTeamOps(a.teams)
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	_, err := a.client.TeamRoster(ctx, teamsvc.RosterRequest{Limit: 1})
	var te *client.TeamError
	if errors.As(err, &te) && (te.Code == "teams_disabled" || te.Code == "unavailable" || te.Code == "internal_error") {
		return false
	}
	a.teams = clientTeamOps{c: a.client}
	return true
}
