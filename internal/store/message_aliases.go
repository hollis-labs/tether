package store

import (
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/hollis-labs/go-messaging"
)

type MessageAlias struct {
	URN   string `json:"urn"`
	Alias string `json:"alias"`
}

var messageAliasPattern = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9._-]{0,63}$`)

func (s *Store) SetMessageAlias(urn, alias string) error {
	address, err := messaging.ParseURN(urn)
	if err != nil {
		return fmt.Errorf("invalid alias URN: %w", err)
	}
	alias = strings.TrimSpace(alias)
	if !messageAliasPattern.MatchString(alias) {
		return errors.New("alias must start with a letter and contain at most 64 letters, digits, dots, underscores or hyphens")
	}
	_, err = s.db.Exec(`INSERT INTO message_aliases(urn,alias) VALUES(?,?) ON CONFLICT(urn) DO UPDATE SET alias=excluded.alias`, address.URN(), alias)
	return err
}

func (s *Store) ListMessageAliases() ([]MessageAlias, error) {
	rows, err := s.db.Query(`SELECT urn,alias FROM message_aliases ORDER BY alias COLLATE NOCASE`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []MessageAlias{}
	for rows.Next() {
		var a MessageAlias
		if err := rows.Scan(&a.URN, &a.Alias); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ResolveMessageAddress accepts a canonical URN or an explicit alias. It never
// guesses equivalent identities from a display name or a URN tail.
func (s *Store) ResolveMessageAddress(value string) (messaging.Address, error) {
	value = strings.TrimSpace(value)
	if strings.HasPrefix(value, "msg://") {
		return messaging.ParseURN(value)
	}
	var urn string
	err := s.db.QueryRow(`SELECT urn FROM message_aliases WHERE alias=? COLLATE NOCASE`, strings.TrimPrefix(value, "@")).Scan(&urn)
	if errors.Is(err, sql.ErrNoRows) {
		return messaging.Address{}, fmt.Errorf("unknown message alias %q", value)
	}
	if err != nil {
		return messaging.Address{}, err
	}
	return messaging.ParseURN(urn)
}
