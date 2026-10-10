package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"reflect"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/hollis-labs/substrate/mesh"
	"github.com/hollis-labs/substrate/mesh/teams"
	"github.com/hollis-labs/tether/internal/teamsvc"
)

// TeamOps is the handler-owned seam shared by the team verb surfaces.
// *teamsvc.Service satisfies it; only the host resolves caller identity.
type TeamOps interface {
	Form(context.Context, teamsvc.FormRequest) (teamsvc.Result, error)
	Dissolve(context.Context, teamsvc.RunRequest) (teamsvc.Result, error)
	AddMember(context.Context, teamsvc.AddMemberRequest) (teamsvc.Result, error)
	RemoveMember(context.Context, teamsvc.RemoveMemberRequest) (teamsvc.Result, error)
	Assign(context.Context, teamsvc.MessageRequest) (teamsvc.Result, error)
	Delegate(context.Context, teamsvc.MessageRequest) (teamsvc.Result, error)
	Address(context.Context, teamsvc.MessageRequest) (teamsvc.Result, error)
	Cancel(context.Context, teamsvc.CancelRequest) (teamsvc.Result, error)
	ReportResult(context.Context, teamsvc.ReportResultRequest) (teamsvc.Result, error)
}

var _ TeamOps = (*teamsvc.Service)(nil)

// TeamRequest carries verb arguments, never a caller or operator claim.
// Idempotency keys travel separately in the HTTP header or MCP/CLI argument.
type TeamRequest struct {
	Team        *teams.Team        `json:"team,omitempty"`
	Launch      TeamLaunchRequest  `json:"launch,omitempty,omitzero"`
	RunID       string             `json:"run_id,omitempty"`
	MemberID    string             `json:"member_id,omitempty"`
	Slot        string             `json:"slot,omitempty"`
	Limits      mesh.Limits        `json:"limits,omitempty,omitzero"`
	Address     string             `json:"address,omitempty"`
	Body        string             `json:"body,omitempty"`
	Kind        mesh.ActorKind     `json:"kind,omitempty"`
	History     mesh.HistoryPolicy `json:"history,omitempty"`
	Cascade     bool               `json:"cascade,omitempty"`
	DelegateKey string             `json:"delegate_key,omitempty"`
}

// TeamLaunchRequest contains formation options without a second caller key.
type TeamLaunchRequest struct {
	Counts         map[string]int        `json:"counts,omitempty"`
	Limits         mesh.Limits           `json:"limits,omitempty,omitzero"`
	PoolIdentities map[string][]mesh.URN `json:"pool_identities,omitempty"`
}

// TeamVerbs is the vocabulary shared by HTTP, MCP and CLI.
func TeamVerbs() []string {
	return []string{"form", "dissolve", "member_add", "member_remove", "assign", "delegate", "address", "cancel", "report_result"}
}

// HasTeamOps treats nil pointers stored in an interface as absent.
func HasTeamOps(ops TeamOps) bool {
	if ops == nil {
		return false
	}
	v := reflect.ValueOf(ops)
	kind := v.Kind()
	if kind == reflect.Chan || kind == reflect.Func || kind == reflect.Interface || kind == reflect.Map || kind == reflect.Pointer || kind == reflect.Slice {
		return !v.IsNil()
	}
	return true
}

// ValidateTeamKey applies one byte-bound, whitespace and control-character rule.
func ValidateTeamKey(key string) error {
	if key == "" || len(key) > teamsvc.MaxKeyBytes || strings.TrimSpace(key) != key || !utf8.ValidString(key) || strings.ContainsFunc(key, unicode.IsControl) {
		return teamsvc.ErrInvalidRequest
	}
	return nil
}

// TeamRequestFields is the allowed vocabulary for each verb.
func TeamRequestFields(verb string) []string {
	switch verb {
	case "form":
		return []string{"team", "launch"}
	case "dissolve":
		return []string{"run_id"}
	case "member_add":
		return []string{"run_id", "slot", "limits"}
	case "member_remove":
		return []string{"run_id", "member_id"}
	case "assign", "delegate", "address":
		return []string{"run_id", "address", "body", "kind", "history"}
	case "cancel":
		return []string{"run_id", "member_id", "cascade"}
	case "report_result":
		return []string{"run_id", "delegate_key", "body"}
	}
	return nil
}

// TeamRequiredFields is the required vocabulary shared by decoding and schemas.
func TeamRequiredFields(verb string) []string {
	switch verb {
	case "form":
		return []string{"team"}
	case "dissolve":
		return []string{"run_id"}
	case "member_add":
		return []string{"run_id", "slot"}
	case "member_remove", "cancel":
		return []string{"run_id", "member_id"}
	case "assign", "delegate", "address":
		return []string{"run_id", "body"}
	case "report_result":
		return []string{"run_id", "delegate_key", "body"}
	}
	return nil
}

// DecodeTeamRequest bounds and strictly decodes one transport request.
func DecodeTeamRequest(verb, key string, r io.Reader) (TeamRequest, error) {
	var req TeamRequest
	if ValidateTeamKey(key) != nil || TeamRequestFields(verb) == nil {
		return req, teamsvc.ErrInvalidRequest
	}
	b, err := io.ReadAll(io.LimitReader(r, teamsvc.MaxRequestBytes+1))
	if err != nil || len(b) > teamsvc.MaxRequestBytes || !bytes.HasPrefix(bytes.TrimSpace(b), []byte("{")) {
		return req, teamsvc.ErrInvalidRequest
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(b, &fields) != nil {
		return req, teamsvc.ErrInvalidRequest
	}
	for field := range fields {
		allowed := false
		for _, name := range TeamRequestFields(verb) {
			if name == field {
				allowed = true
				break
			}
		}
		if !allowed {
			return req, teamsvc.ErrInvalidRequest
		}
	}
	for _, field := range TeamRequiredFields(verb) {
		value, exists := fields[field]
		if !exists {
			return req, teamsvc.ErrInvalidRequest
		}
		if field == "team" {
			if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
				return req, teamsvc.ErrInvalidRequest
			}
			continue
		}
		var text string
		if json.Unmarshal(value, &text) != nil || text == "" {
			return req, teamsvc.ErrInvalidRequest
		}
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if d.Decode(&req) != nil {
		return req, teamsvc.ErrInvalidRequest
	}
	if d.Decode(new(any)) != io.EOF {
		return req, teamsvc.ErrInvalidRequest
	}
	return req, nil
}

// CallTeam converts transport arguments and invokes exactly one service verb.
func CallTeam(ctx context.Context, ops TeamOps, verb, key string, r TeamRequest) (teamsvc.Result, error) {
	if !HasTeamOps(ops) {
		return teamsvc.Result{}, teamsvc.ErrUnavailable
	}
	if ValidateTeamKey(key) != nil || len(r.Body) > teamsvc.MaxBodyBytes {
		return teamsvc.Result{}, teamsvc.ErrInvalidRequest
	}
	if r.Team != nil {
		b, err := json.Marshal(r.Team)
		if err != nil || len(b) > teamsvc.MaxTeamBytes {
			return teamsvc.Result{}, teamsvc.ErrInvalidRequest
		}
	}
	switch verb {
	case "form":
		if r.Team == nil {
			return teamsvc.Result{}, teamsvc.ErrInvalidRequest
		}
		return ops.Form(ctx, teamsvc.FormRequest{Key: key, Team: *r.Team, Launch: teams.LaunchRequest{Counts: r.Launch.Counts, Limits: r.Launch.Limits, PoolIdentities: r.Launch.PoolIdentities}})
	case "dissolve":
		return ops.Dissolve(ctx, teamsvc.RunRequest{Key: key, RunID: r.RunID})
	case "member_add":
		return ops.AddMember(ctx, teamsvc.AddMemberRequest{Key: key, RunID: r.RunID, Slot: r.Slot, Limits: r.Limits})
	case "member_remove":
		return ops.RemoveMember(ctx, teamsvc.RemoveMemberRequest{Key: key, RunID: r.RunID, MemberID: r.MemberID})
	case "assign":
		return ops.Assign(ctx, teamMessage(key, r))
	case "delegate":
		return ops.Delegate(ctx, teamMessage(key, r))
	case "address":
		return ops.Address(ctx, teamMessage(key, r))
	case "cancel":
		return ops.Cancel(ctx, teamsvc.CancelRequest{Key: key, RunID: r.RunID, MemberID: r.MemberID, Cascade: r.Cascade})
	case "report_result":
		return ops.ReportResult(ctx, teamsvc.ReportResultRequest{Key: key, RunID: r.RunID, DelegateKey: r.DelegateKey, Body: r.Body})
	default:
		return teamsvc.Result{}, teamsvc.ErrInvalidRequest
	}
}
func teamMessage(key string, r TeamRequest) teamsvc.MessageRequest {
	return teamsvc.MessageRequest{Key: key, RunID: r.RunID, Address: r.Address, Body: r.Body, Kind: r.Kind, History: r.History}
}

// TeamError hides causes and gives all surfaces identical stable classifications.
func TeamError(err error) (int, ErrorDetail) {
	status, code := http.StatusInternalServerError, CodeInternalError
	switch {
	case errors.Is(err, teamsvc.ErrUnauthenticated):
		status, code = 401, "unauthenticated"
	case errors.Is(err, teamsvc.ErrUnavailable):
		status, code = 503, "unavailable"
	case errors.Is(err, teamsvc.ErrDenied):
		status, code = 403, "denied"
	case errors.Is(err, teamsvc.ErrNotFound):
		status, code = 404, CodeNotFound
	case errors.Is(err, teamsvc.ErrConflict):
		status, code = 409, CodeConflict
	case errors.Is(err, teamsvc.ErrInvalidRequest):
		status, code = 400, CodeInvalidRequest
	}
	return status, ErrorDetail{Code: code, Message: code}
}

func (s *Server) registerTeamRoutes(mux *http.ServeMux) {
	if !HasTeamOps(s.Teams) {
		return
	}
	s.registerTeamRosterRoute(mux)
	for _, verb := range TeamVerbs() {
		mux.HandleFunc("/teams/"+verb, func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost {
				w.Header().Set("Allow", http.MethodPost)
				writeError(w, 405, CodeMethodNotAllowed, "method not allowed")
				return
			}
			req, err := DecodeTeamRequest(verb, r.Header.Get("Idempotency-Key"), r.Body)
			var result teamsvc.Result
			if err == nil {
				result, err = CallTeam(r.Context(), s.Teams, verb, r.Header.Get("Idempotency-Key"), req)
			}
			if err != nil {
				status, detail := TeamError(err)
				if status >= 500 || status == 409 {
					slog.WarnContext(r.Context(), "team verb failed", "verb", verb, "code", detail.Code)
				}
				writeError(w, status, detail.Code, detail.Message)
				return
			}
			writeJSON(w, 200, result)
		})
	}
}
