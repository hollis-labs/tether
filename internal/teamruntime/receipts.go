// Package teamruntime implements explicitly scheduled team host ports over the
// daemon's session, registry and message services. Construction starts no work.
package teamruntime

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"reflect"
	"sync"

	"github.com/hollis-labs/substrate/mesh/teams"
	"github.com/hollis-labs/tether/internal/teamstore"
)

type receipts struct {
	db    *sql.DB
	store *teamstore.Store
}
type receipt struct {
	request, payload []byte
	state, ended     string
	nonce, acquired  string
	bindingSecret    string
	bindingEnded     bool
}

// The daemon is the runtime writer. Locks serialize keyed port effects across
// adapter instances; the durable fences survive process restarts. No lock holds
// a database transaction over a runtime or registry call.
var effectLocks sync.Map

func (r receipts) lock(kind, key string) func() {
	token := struct {
		db        *sql.DB
		kind, key string
	}{r.db, kind, key}
	value, _ := effectLocks.LoadOrStore(token, &sync.Mutex{})
	mu := value.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}
func (r receipts) read(ctx context.Context, kind, key string) (receipt, error) {
	var v receipt
	err := r.db.QueryRowContext(ctx, `SELECT request,payload,state,ended,binding_ended,nonce,acquired_urn,binding_secret FROM team_port_intents WHERE port_kind=? AND intent_key=?`, kind, key).Scan(&v.request, &v.payload, &v.state, &v.ended, &v.bindingEnded, &v.nonce, &v.acquired, &v.bindingSecret)
	if errors.Is(err, sql.ErrNoRows) {
		err = teams.ErrNotFound
	}
	return v, err
}
func (r receipts) reserve(ctx context.Context, kind, key string, request any) (receipt, error) {
	if key == "" {
		return receipt{}, teams.ErrProvisionFailed
	}
	raw, err := json.Marshal(request)
	if err != nil {
		return receipt{}, err
	}
	nonce := make([]byte, 32)
	if _, err = rand.Read(nonce); err != nil {
		return receipt{}, err
	}
	bindingSecret := make([]byte, 32)
	if _, err = rand.Read(bindingSecret); err != nil {
		return receipt{}, err
	}
	err = r.store.WithTransaction(ctx, func(conn *sql.Conn) error {
		_, err := conn.ExecContext(ctx, `INSERT OR IGNORE INTO team_port_intents(port_kind,intent_key,request,nonce,binding_secret) VALUES(?,?,?,?,?)`, kind, key, raw, hex.EncodeToString(nonce), hex.EncodeToString(bindingSecret))
		return err
	})
	if err != nil {
		return receipt{}, err
	}
	v, err := r.read(ctx, kind, key)
	if err != nil {
		return v, err
	}
	if v.ended != "" || kind == "enrollment" && v.bindingEnded {
		return v, teams.ErrDenied
	}
	var previous, current any
	if err = decodeReceipt(v.request, &previous); err != nil {
		return v, err
	}
	if err = decodeReceipt(raw, &current); err != nil {
		return v, err
	}
	if !reflect.DeepEqual(previous, current) {
		return v, teams.ErrConflict
	}
	return v, nil
}
func (r receipts) complete(ctx context.Context, kind, key string, payload any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return r.store.WithTransaction(ctx, func(conn *sql.Conn) error {
		result, err := conn.ExecContext(ctx, `UPDATE team_port_intents SET payload=?,state='done' WHERE port_kind=? AND intent_key=? AND ended='' AND (port_kind<>'enrollment' OR binding_ended=0)`, raw, kind, key)
		if err != nil {
			return err
		}
		n, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if n != 1 {
			return teams.ErrDenied
		}
		return nil
	})
}
func (r receipts) end(ctx context.Context, kind, key, mode string, binding bool) error {
	if key == "" {
		return teams.ErrProvisionFailed
	}
	return r.store.WithTransaction(ctx, func(conn *sql.Conn) error {
		if _, err := conn.ExecContext(ctx, `INSERT OR IGNORE INTO team_port_intents(port_kind,intent_key) VALUES(?,?)`, kind, key); err != nil {
			return err
		}
		if binding {
			_, err := conn.ExecContext(ctx, `UPDATE team_port_intents SET binding_ended=1 WHERE port_kind=? AND intent_key=?`, kind, key)
			return err
		}
		_, err := conn.ExecContext(ctx, `UPDATE team_port_intents SET ended=CASE WHEN ended='retire' THEN ended ELSE ? END WHERE port_kind=? AND intent_key=?`, mode, kind, key)
		return err
	})
}
func (r receipts) cleaned(ctx context.Context, kind, key string) error {
	_, err := r.db.ExecContext(ctx, `UPDATE team_port_intents SET state='cleaned' WHERE port_kind=? AND intent_key=?`, kind, key)
	return err
}

// UseNumber preserves integer request fields when comparing decoded requests.
func decodeReceipt(raw []byte, value *any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	return decoder.Decode(value)
}

// recordAcquisition retains ownership even when an end raced the effect. Only
// the matching write-ahead nonce may publish the acquired identity.
func (r receipts) recordAcquisition(ctx context.Context, key, nonce, actor string) error {
	return r.store.WithTransaction(ctx, func(conn *sql.Conn) error {
		result, err := conn.ExecContext(ctx, `UPDATE team_port_intents SET acquired_urn=? WHERE port_kind='enrollment' AND intent_key=? AND nonce=? AND (acquired_urn='' OR acquired_urn=?)`, actor, key, nonce, actor)
		if err != nil {
			return err
		}
		n, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if n != 1 {
			return teams.ErrConflict
		}
		return nil
	})
}
func (r receipts) failed(ctx context.Context, kind, key string) error {
	_, err := r.db.ExecContext(ctx, `UPDATE team_port_intents SET state='failed' WHERE port_kind=? AND intent_key=?`, kind, key)
	return err
}
