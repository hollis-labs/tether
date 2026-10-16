package teamruntime

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/hollis-labs/tether/internal/app"
	"github.com/hollis-labs/tether/internal/identity"
)

// launchGrant returns the authority a team session is launched under. The
// first launch of an intent happens inside the verb that formed the team, with
// a verified caller: that caller's scopes are recorded (first write wins). A
// recovery or reconcile launch has no caller; it reuses the recorded grant, so
// it is bounded by what the team was first launched under instead of being
// unattributed (which a global enforce flip refuses). nil means there is
// neither a caller nor a recorded grant.
func (s *Sessions) launchGrant(ctx context.Context, intentKey string) (*app.LaunchGrant, error) {
	if p, ok := identity.FromContext(ctx); ok && p.Verified && p.ID != "" {
		scopes, err := json.Marshal(p.Scopes)
		if err != nil {
			return nil, err
		}
		if string(scopes) == "null" {
			scopes = []byte("[]")
		}
		if _, err = s.db.ExecContext(ctx, `INSERT OR IGNORE INTO team_launch_grants(intent_key,principal_id,scopes_json,recorded_at) VALUES(?,?,?,?)`,
			intentKey, p.ID, string(scopes), time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
			return nil, fmt.Errorf("record team launch grant: %w", err)
		}
	}
	var principal, raw string
	err := s.db.QueryRowContext(ctx, `SELECT principal_id, scopes_json FROM team_launch_grants WHERE intent_key=?`, intentKey).Scan(&principal, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read team launch grant: %w", err)
	}
	var scopes []string
	if err = json.Unmarshal([]byte(raw), &scopes); err != nil {
		return nil, fmt.Errorf("decode team launch grant: %w", err)
	}
	return &app.LaunchGrant{Principal: principal, Scopes: scopes}, nil
}

// logRefusedLaunch says why a team launch was refused, so a recovery that
// stops under enforce is explained in the log.
func logRefusedLaunch(intentKey string, grant *app.LaunchGrant, parent string, err error) {
	if !errors.Is(err, app.ErrLaunchRefused) {
		return
	}
	switch {
	case grant == nil && parent == "":
		log.Printf("team launch %q refused: no recorded launch grant and no parent session to bound it (formed before grants were recorded?): %v", intentKey, err)
	default:
		log.Printf("team launch %q refused: %v", intentKey, err)
	}
}
