package teamstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"

	"github.com/hollis-labs/substrate/mesh/teams"
)

func encode(v any) ([]byte, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("encode team value: %w", err)
	}
	return b, nil
}
func decode(b []byte, out any) error {
	if err := json.Unmarshal(b, out); err != nil {
		return fmt.Errorf("decode team value: %w", err)
	}
	return nil
}
func (s *Store) GetDefinition(ctx context.Context, id string, version uint64) (teams.Team, error) {
	var payload []byte
	err := s.db.QueryRowContext(ctx, `SELECT payload FROM team_definitions WHERE team_id=? AND version=?`, id, version).Scan(&payload)
	if err != nil {
		return teams.Team{}, readError("team definition", err)
	}
	var t teams.Team
	err = decode(payload, &t)
	return t, err
}
func (s *Store) PutDefinition(ctx context.Context, t teams.Team) error {
	if err := teams.Validate(t); err != nil {
		return fmt.Errorf("validate definition: %w", err)
	}
	payload, err := encode(t)
	if err != nil {
		return err
	}
	var normalized teams.Team
	if err = decode(payload, &normalized); err != nil {
		return err
	}
	return s.immediate(ctx, func(conn *sql.Conn) error {
		var old []byte
		err := conn.QueryRowContext(ctx, `SELECT payload FROM team_definitions WHERE team_id=? AND version=?`, t.ID, t.Version).Scan(&old)
		if err == nil {
			var stored teams.Team
			if err := decode(old, &stored); err != nil {
				return err
			}
			if !reflect.DeepEqual(stored, normalized) {
				return teams.ErrConflict
			}
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("read immutable definition: %w", err)
		}
		if _, err = conn.ExecContext(ctx, `INSERT INTO team_definitions(team_id,version,payload) VALUES(?,?,?)`, t.ID, t.Version, payload); err != nil {
			return fmt.Errorf("store definition: %w", err)
		}
		return nil
	})
}
