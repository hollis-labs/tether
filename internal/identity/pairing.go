package identity

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

const DefaultGrantTTL = 5 * time.Minute
const MaxGrantTTL = time.Hour
const DeviceLifetime = 30 * 24 * time.Hour
const credentialTimeFormat = "2006-01-02T15:04:05.000000000Z"

var ErrInvalidGrant = errors.New("unknown, inactive or insufficient pairing grant")

type PairingGrant struct {
	ID            string    `json:"id"`
	Label         string    `json:"label"`
	Scopes        []string  `json:"scopes"`
	CreatedAt     time.Time `json:"created_at"`
	ExpiresAt     time.Time `json:"expires_at"`
	KeyThumbprint string    `json:"key_thumbprint,omitempty"`
}

type IssuedGrant struct {
	PairingGrant
	Code string `json:"code"`
}

type DeviceExchange struct {
	Principal Principal `json:"device"`
	Token     string    `json:"token"`
}

func (s *Store) CreatePairingGrant(ctx context.Context, operator Principal, label string, scopes []string, ttl time.Duration, thumbprint string) (IssuedGrant, error) {
	if operator.ID != OperatorID || operator.Kind != "operator" {
		return IssuedGrant{}, fmt.Errorf("local operator required")
	}
	if ttl == 0 {
		ttl = DefaultGrantTTL
	}
	if ttl <= 0 || ttl > MaxGrantTTL {
		return IssuedGrant{}, fmt.Errorf("pairing ttl must be positive and at most one hour")
	}
	scopes, err := NormalizeDeviceScopes(scopes)
	if err != nil {
		return IssuedGrant{}, err
	}
	if len(label) > 128 || len(thumbprint) > 128 || safeDeviceMetadata(label, 128) != label || safeDeviceMetadata(thumbprint, 128) != thumbprint {
		return IssuedGrant{}, fmt.Errorf("invalid pairing metadata")
	}
	id, err := NewToken()
	if err != nil {
		return IssuedGrant{}, err
	}
	raw, err := NewToken()
	if err != nil {
		return IssuedGrant{}, err
	}
	code := "tpg_" + raw[4:]
	now := time.Now().UTC()
	grant := PairingGrant{ID: "grant_" + id[4:], Label: label, Scopes: scopes, CreatedAt: now, ExpiresAt: now.Add(ttl), KeyThumbprint: thumbprint}
	encoded, _ := json.Marshal(scopes)
	_, err = s.db.ExecContext(ctx, `INSERT INTO pairing_grants (id, code_hash, label, scopes_json, created_by, created_at, expires_at, key_thumbprint) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, grant.ID, HashToken(code), label, string(encoded), operator.ID, now.Format(credentialTimeFormat), grant.ExpiresAt.Format(credentialTimeFormat), thumbprint)
	if err != nil {
		return IssuedGrant{}, fmt.Errorf("persist pairing grant: %w", err)
	}
	return IssuedGrant{PairingGrant: grant, Code: code}, nil
}

// ExchangePairingGrant performs one conditional UPDATE for all grant states,
// including malformed/unknown codes. It never queries the reason for failure.
// Narrowing is inside that update; the device insert and grant consume commit
// together, so a failed mint cannot spend the grant. No DPoP claim is made.
func (s *Store) ExchangePairingGrant(ctx context.Context, code string, requested []string, thumbprint string) (DeviceExchange, error) {
	if len(code) > 256 || len(thumbprint) > 128 {
		return DeviceExchange{}, ErrInvalidGrant
	}
	var requestedJSON any
	if requested != nil {
		var err error
		requested, err = NormalizeDeviceScopes(requested)
		if err != nil {
			return DeviceExchange{}, ErrInvalidGrant
		}
		b, _ := json.Marshal(requested)
		requestedJSON = string(b)
	}
	hash := HashToken(code)
	if !strings.HasPrefix(code, "tpg_") || !validToken("tth_"+strings.TrimPrefix(code, "tpg_")) {
		hash = HashToken("invalid pairing code")
	}
	now := time.Now().UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return DeviceExchange{}, fmt.Errorf("begin pairing exchange: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var label, scopesJSON, createdBy string
	err = tx.QueryRowContext(ctx, `UPDATE pairing_grants SET consumed_at = ?
 WHERE code_hash = ? AND consumed_at IS NULL AND revoked_at IS NULL AND expires_at > ?
 AND (key_thumbprint = '' OR key_thumbprint = ?)
 AND (? IS NULL OR NOT EXISTS (
   SELECT 1 FROM json_each(?) requested WHERE requested.value NOT IN (SELECT value FROM json_each(pairing_grants.scopes_json))))
 RETURNING label, scopes_json, created_by`, now.Format(credentialTimeFormat), hash, now.Format(credentialTimeFormat), thumbprint, requestedJSON, requestedJSON).Scan(&label, &scopesJSON, &createdBy)
	if errors.Is(err, sql.ErrNoRows) {
		return DeviceExchange{}, ErrInvalidGrant
	}
	if err != nil {
		return DeviceExchange{}, fmt.Errorf("consume pairing grant: %w", err)
	}
	var granted []string
	if err := json.Unmarshal([]byte(scopesJSON), &granted); err != nil {
		return DeviceExchange{}, fmt.Errorf("decode pairing scopes: %w", err)
	}
	scopes, err := NarrowDeviceScopes(granted, requested)
	if err != nil {
		return DeviceExchange{}, ErrInvalidGrant
	}
	id, err := NewToken()
	if err != nil {
		return DeviceExchange{}, err
	}
	expires := now.Add(DeviceLifetime)
	p := Principal{ID: "msg://device/" + id[4:], Kind: "device", Display: label, Scopes: scopes, CreatedBy: createdBy, CreatedAt: now, ExpiresAt: &expires, Addresses: []string{}}
	token, err := mint(ctx, tx, p)
	if err != nil {
		return DeviceExchange{}, err
	}
	if err := tx.Commit(); err != nil {
		return DeviceExchange{}, fmt.Errorf("commit pairing exchange: %w", err)
	}
	return DeviceExchange{Principal: p, Token: token}, nil
}

func (s *Store) RevokePairingGrant(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE pairing_grants SET revoked_at = COALESCE(revoked_at, ?) WHERE id = ?`, time.Now().UTC().Format(credentialTimeFormat), id)
	return err
}
