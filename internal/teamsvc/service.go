package teamsvc

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/hollis-labs/substrate/mesh"
	"github.com/hollis-labs/substrate/mesh/teams"
)

var (
	ErrUnauthenticated = errors.New("teamsvc: unauthenticated")
	ErrInvalidRequest  = errors.New("teamsvc: invalid request")
	ErrDenied          = teams.ErrDenied
	ErrNotFound        = teams.ErrNotFound
	ErrConflict        = teams.ErrConflict
	ErrUnavailable     = teams.ErrUnavailable
)

type Error struct {
	Code  string
	Cause error
}

func (e *Error) Error() string { return "teamsvc: " + e.Code }
func (e *Error) Unwrap() error { return e.Cause }

func typed(err error) error {
	if err == nil {
		return nil
	}
	code := "operation_failed"
	switch {
	case errors.Is(err, ErrUnauthenticated):
		code = "unauthenticated"
	case errors.Is(err, ErrInvalidRequest):
		code = "invalid_request"
	case errors.Is(err, ErrNotFound):
		code = "not_found"
	case errors.Is(err, ErrDenied):
		code = "denied"
	case errors.Is(err, ErrConflict):
		code = "conflict"
	case errors.Is(err, ErrUnavailable):
		code = "unavailable"
	}
	return &Error{Code: code, Cause: err}
}

type Service struct {
	deps     Deps
	router   *teams.Router
	launcher *teams.Launcher
}

func New(d Deps) (*Service, error) {
	if d.Ceilings == nil || d.Runs == nil || d.Calls == nil || d.Definitions == nil || d.Roster == nil || d.Ledger == nil || d.Provisioner == nil || d.Workflows == nil || d.Sender == nil || d.Routing == nil || d.Trust == nil || d.Clock == nil || d.IDs == nil {
		return nil, typed(ErrInvalidRequest)
	}
	if _, ok := d.Sender.(teams.DeliveryStore); !ok {
		return nil, typed(ErrInvalidRequest)
	}
	if err := d.Defaults.Validate(); err != nil {
		return nil, typed(fmt.Errorf("%w: %w", ErrInvalidRequest, err))
	}
	return &Service{deps: d, router: &teams.Router{Roster: d.Roster, Sender: d.Sender}, launcher: &teams.Launcher{Definitions: d.Definitions, Roster: d.Roster, Ledger: d.Ledger, Provisioner: d.Provisioner, Workflows: d.Workflows, Routing: d.Routing, Clock: d.Clock, IDs: d.IDs, Defaults: d.Defaults}}, nil
}

func (s *Service) principal(ctx context.Context) (Principal, error) {
	if s.deps.Principals == nil {
		return Principal{}, ErrUnavailable
	}
	p, err := s.deps.Principals.ResolvePrincipal(ctx)
	if err != nil {
		switch {
		case errors.Is(err, ErrUnauthenticated):
			return Principal{}, ErrUnauthenticated
		case errors.Is(err, ErrDenied):
			return Principal{}, ErrDenied
		default:
			return Principal{}, ErrUnavailable
		}
	}
	if p.ID == LocalOperator && !p.LocalOperator {
		return Principal{}, ErrUnauthenticated
	}
	authenticated := p.Verified || (p.LocalOperator && p.ID == LocalOperator)
	if (mesh.Actor{URN: p.ID, Kind: p.Kind}).Validate() != nil || !authenticated {
		return Principal{}, ErrUnauthenticated
	}
	return p, nil
}

func serviceKey(scope Scope) string {
	b, _ := json.Marshal(scope)
	return fmt.Sprintf("teamsvc-%x", sha256.Sum256(b))
}

// MemberView exposes membership without host intent, session or quota details.
type MemberView struct {
	ID     string         `json:"id"`
	Slot   string         `json:"slot"`
	Actor  mesh.URN       `json:"actor"`
	Status string         `json:"status"`
	Kind   mesh.ActorKind `json:"kind"`
}

func memberView(m teams.Member) MemberView {
	return MemberView{ID: m.ID, Slot: m.Slot, Actor: m.Actor, Status: m.Status, Kind: m.Kind}
}

type Result struct {
	Run          *teams.TeamRun `json:"run,omitempty"`
	Member       *MemberView    `json:"member,omitempty"`
	Recipients   []MemberView   `json:"recipients,omitempty"`
	DeliveryKeys []string       `json:"delivery_keys,omitempty"`
}

const (
	MaxKeyBytes     = 256
	MaxBodyBytes    = 64 * 1024
	MaxTeamBytes    = 64 * 1024
	MaxRequestBytes = 128 * 1024
)

func requestBounds(req any) (string, error) {
	var run, body string
	switch r := req.(type) {
	case FormRequest:
		b, err := json.Marshal(r.Team)
		if err != nil || len(b) > MaxTeamBytes {
			return "", ErrInvalidRequest
		}
	case RunRequest:
		run = r.RunID
	case AddMemberRequest:
		run = r.RunID
	case RemoveMemberRequest:
		run = r.RunID
	case CancelRequest:
		run = r.RunID
	case MessageRequest:
		run, body = r.RunID, r.Body
	case ReportResultRequest:
		run, body = r.RunID, r.Body
	default:
		return "", ErrInvalidRequest
	}
	if len(body) > MaxBodyBytes {
		return "", ErrInvalidRequest
	}
	if _, form := req.(FormRequest); !form && (run == "" || len(run) > MaxKeyBytes) {
		return "", ErrInvalidRequest
	}
	return run, nil
}

// membership permits retained, ended members to recover completed receipts;
// each operation still checks live authority before starting new effects.
func (s *Service) membership(ctx context.Context, id string, p Principal) error {
	r, err := s.deps.Roster.Snapshot(ctx, id)
	if err != nil {
		return err
	}
	for _, m := range r.Members {
		if m.Actor == p.ID && m.Kind == p.Kind {
			return nil
		}
	}
	return ErrNotFound
}

// call authenticates even completed retries. Scope is actor/verb/key, not a
// session credential or a claimed actor. The host journal survives restarts.
func (s *Service) call(ctx context.Context, verb, key string, request any, fn func(context.Context, Principal, Scope, Record) (Result, error)) (Result, error) {
	p, err := s.principal(ctx)
	if err != nil {
		return Result{}, typed(err)
	}
	if key == "" || len(key) > MaxKeyBytes {
		return Result{}, typed(ErrInvalidRequest)
	}
	runID, err := requestBounds(request)
	if err != nil {
		return Result{}, typed(err)
	}
	if runID != "" {
		if err = s.membership(ctx, runID, p); err != nil {
			return Result{}, typed(err)
		}
	}
	var encoded bytes.Buffer
	encoder := json.NewEncoder(&encoded)
	encoder.SetEscapeHTML(false)
	err = encoder.Encode(request)
	if err != nil {
		return Result{}, typed(fmt.Errorf("%w: %w", ErrInvalidRequest, err))
	}
	b := bytes.TrimSuffix(encoded.Bytes(), []byte("\n"))
	if len(b) > MaxRequestBytes {
		return Result{}, typed(ErrInvalidRequest)
	}
	scope := Scope{Principal: p.ID, Verb: verb, Key: key}
	intent := Intent{Scope: scope, Digest: fmt.Sprintf("%x", sha256.Sum256(b)), Request: b}
	var result Result
	err = s.deps.Ledger.WithLease(ctx, serviceKey(scope), func(ctx context.Context) error {
		record, err := s.deps.Calls.GetOrCreate(ctx, intent)
		if err != nil {
			return err
		}
		if record.Intent.Scope != intent.Scope || record.Intent.Digest != intent.Digest || !bytes.Equal(record.Intent.Request, intent.Request) {
			return ErrConflict
		}
		if len(record.Result) != 0 {
			return json.Unmarshal(record.Result, &result)
		}
		result, err = fn(ctx, p, scope, record)
		if err != nil {
			return err
		}
		payload, err := json.Marshal(result)
		if err != nil {
			return err
		}
		return s.deps.Calls.Complete(ctx, scope, payload)
	})
	if err != nil {
		return Result{}, typed(err)
	}
	return result, nil
}

// run exposes no definition or roster to a stranger, regardless of actor kind.
func (s *Service) run(ctx context.Context, id string, p Principal) (teams.Team, teams.Member, error) {
	roster, err := s.deps.Roster.Snapshot(ctx, id)
	if err != nil {
		return teams.Team{}, teams.Member{}, err
	}
	var actor teams.Member
	for _, m := range roster.Members {
		if m.Actor == p.ID && m.Kind == p.Kind && m.Status == "active" {
			actor = m
			break
		}
	}
	if actor.ID == "" {
		return teams.Team{}, teams.Member{}, ErrNotFound
	}
	run, err := s.deps.Runs.GetRun(ctx, id)
	if err != nil {
		return teams.Team{}, actor, err
	}
	t, err := s.deps.Definitions.GetDefinition(ctx, run.TeamID, run.TeamVersion)
	if err == nil && t.Authority.Mode != teams.Strict {
		err = ErrDenied
	}
	return t, actor, err
}
