package teamhost

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/hollis-labs/substrate/mesh/teams"
)

// ServiceKey is the exact ledger lease key for one caller-scoped receipt.
func ServiceKey(scope Scope) string {
	b, _ := json.Marshal(scope)
	sum := sha256.Sum256(b)
	return fmt.Sprintf("teamsvc-%x", sum)
}
func validateScope(s Scope) error {
	if s.Principal.Validate() != nil || s.Verb == "" || s.Key == "" {
		return errors.New("call: principal, verb and key required")
	}
	return nil
}
func loadCall(ctx context.Context, q rowReader, scope Scope) (Record, error) {
	out := Record{Intent: Intent{Scope: scope}}
	var request, plan, result []byte
	err := q.QueryRowContext(ctx, `SELECT digest,request,plan,result FROM team_host_calls WHERE principal=? AND verb=? AND call_key=?`, scope.Principal, scope.Verb, scope.Key).Scan(&out.Intent.Digest, &request, &plan, &result)
	out.Intent.Request = request
	out.Plan = plan
	out.Result = result
	return out, notFound(err)
}
func (h *Host) GetOrCreate(ctx context.Context, intent Intent) (Record, error) {
	if err := validateScope(intent.Scope); err != nil {
		return Record{}, err
	}
	if intent.Digest == "" || !json.Valid(intent.Request) {
		return Record{}, errors.New("call: digest and JSON request required")
	}
	var out Record
	err := h.store.WithLeasedTransaction(ctx, ServiceKey(intent.Scope), func(conn *sql.Conn) error {
		var err error
		out, err = loadCall(ctx, conn, intent.Scope)
		if err == nil {
			if out.Intent.Digest != intent.Digest || !bytes.Equal(out.Intent.Request, intent.Request) {
				return teams.ErrConflict
			}
			return nil
		}
		if !errors.Is(err, teams.ErrNotFound) {
			return err
		}
		_, err = conn.ExecContext(ctx, `INSERT INTO team_host_calls(principal,verb,call_key,digest,request) VALUES(?,?,?,?,?)`, intent.Scope.Principal, intent.Scope.Verb, intent.Scope.Key, intent.Digest, []byte(intent.Request))
		if err != nil {
			return err
		}
		out, err = loadCall(ctx, conn, intent.Scope)
		return err
	})
	if err != nil {
		return Record{}, err
	}
	return out, nil
}
func (h *Host) SetPlan(ctx context.Context, scope Scope, plan json.RawMessage) error {
	return h.writeCall(ctx, scope, plan, false)
}
func (h *Host) Complete(ctx context.Context, scope Scope, result json.RawMessage) error {
	return h.writeCall(ctx, scope, result, true)
}
func (h *Host) writeCall(ctx context.Context, scope Scope, value json.RawMessage, result bool) error {
	if err := validateScope(scope); err != nil {
		return err
	}
	if !json.Valid(value) {
		return errors.New("call: JSON value required")
	}
	return h.store.WithLeasedTransaction(ctx, ServiceKey(scope), func(conn *sql.Conn) error {
		old, err := loadCall(ctx, conn, scope)
		if err != nil {
			return err
		}
		prior := old.Plan
		if result {
			prior = old.Result
		}
		if prior != nil {
			if !bytes.Equal(prior, value) {
				return teams.ErrConflict
			}
			return nil
		}
		query := `UPDATE team_host_calls SET plan=? WHERE principal=? AND verb=? AND call_key=?`
		if result {
			query = `UPDATE team_host_calls SET result=? WHERE principal=? AND verb=? AND call_key=?`
		}
		_, err = conn.ExecContext(ctx, query, []byte(value), scope.Principal, scope.Verb, scope.Key)
		return err
	})
}

var _ Calls = (*Host)(nil)
