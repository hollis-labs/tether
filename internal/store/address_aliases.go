package store

// address_aliases.go — address aliases (CW-20261114-0067). An alias is another
// name for a canonical address's mailbox: mail sent to the alias is stored
// under the canonical address, and mailbox reads of either form return the
// mail stored under every form. Stored envelopes are never rewritten, so a
// message that was addressed to an alias before it became one keeps its
// to_urn and its read/archive state; it is only seen from one more mailbox.
//
// Not to be confused with message_aliases (migration 0056), which gives an
// address a readable display name for Sysop.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/hollis-labs/libs/mesh/core/messaging"
)

// AddressAlias is one alias row.
type AddressAlias struct {
	Alias     string    `json:"alias"`
	Canonical string    `json:"canonical"`
	CreatedAt time.Time `json:"created_at"`
	CreatedBy string    `json:"created_by,omitempty"`
}

// ErrAddressAliasInvalid marks an alias request the rules refuse (a kind that
// cannot be aliased, a chain, an alias that already points elsewhere).
var ErrAddressAliasInvalid = errors.New("address alias refused")

// ErrAddressAliasNotFound is returned by RemoveAddressAlias for an unknown alias.
var ErrAddressAliasNotFound = errors.New("address alias not found")

// addressAliasTTL bounds how stale the in-memory alias map may be when another
// process changed the table. A change made through this Store is seen at once.
const addressAliasTTL = 5 * time.Second

// addressAliases is the alias table held in memory: every send and every
// mailbox read resolves through it, and the table is a handful of rows.
type addressAliases struct {
	mu        sync.Mutex
	loadedAt  time.Time
	canonical map[string]string   // alias -> canonical
	aliases   map[string][]string // canonical -> aliases, sorted
}

// aliasableAddress parses urn and refuses the kinds that are not a person's or
// an agent's mailbox: sessions are leased, groups and channels fan out, and
// service addresses are daemon-internal.
func aliasableAddress(field, urn string) (messaging.Address, error) {
	a, err := messaging.ParseURN(urn)
	if err != nil {
		return messaging.Address{}, fmt.Errorf("%w: %s %q is not a msg:// address", ErrAddressAliasInvalid, field, urn)
	}
	if a.Kind != messaging.KindAgent && a.Kind != messaging.KindUser {
		return messaging.Address{}, fmt.Errorf("%w: %s must be an agent or user address, not %s", ErrAddressAliasInvalid, field, a.Kind)
	}
	if a.SubID != "" {
		return messaging.Address{}, fmt.Errorf("%w: %s must not carry a sub-id", ErrAddressAliasInvalid, field)
	}
	return a, nil
}

// AddAddressAlias declares alias another name for canonical. It is idempotent
// for an existing identical row. Refused: an alias that already points at
// another canonical address, a canonical address that is itself an alias, and
// an alias that is the canonical address of other aliases (aliases are one hop).
//
// An alias that already holds mail of its own is accepted: nothing is copied or
// rewritten, its messages simply become visible in the canonical mailbox too.
// The returned count is how many stored messages that is.
func (s *Store) AddAddressAlias(ctx context.Context, alias, canonical, by string) (AddressAlias, int, error) {
	a, err := aliasableAddress("alias", alias)
	if err != nil {
		return AddressAlias{}, 0, err
	}
	c, err := aliasableAddress("canonical", canonical)
	if err != nil {
		return AddressAlias{}, 0, err
	}
	if a.URN() == c.URN() {
		return AddressAlias{}, 0, fmt.Errorf("%w: an address cannot be an alias of itself", ErrAddressAliasInvalid)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return AddressAlias{}, 0, err
	}
	defer func() { _ = tx.Rollback() }()

	var existing string
	switch err := tx.QueryRowContext(ctx, `SELECT canonical_urn FROM address_aliases WHERE alias_urn=?`, a.URN()).Scan(&existing); {
	case err == nil && existing != c.URN():
		return AddressAlias{}, 0, fmt.Errorf("%w: %s is already an alias of %s; remove it first", ErrAddressAliasInvalid, a.URN(), existing)
	case err != nil && !errors.Is(err, sql.ErrNoRows):
		return AddressAlias{}, 0, err
	}
	var target string
	switch err := tx.QueryRowContext(ctx, `SELECT canonical_urn FROM address_aliases WHERE alias_urn=?`, c.URN()).Scan(&target); {
	case err == nil:
		return AddressAlias{}, 0, fmt.Errorf("%w: %s is itself an alias of %s; alias to that address instead", ErrAddressAliasInvalid, c.URN(), target)
	case !errors.Is(err, sql.ErrNoRows):
		return AddressAlias{}, 0, err
	}
	var dependents int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM address_aliases WHERE canonical_urn=?`, a.URN()).Scan(&dependents); err != nil {
		return AddressAlias{}, 0, err
	}
	if dependents > 0 {
		return AddressAlias{}, 0, fmt.Errorf("%w: %s is the canonical address of %d alias(es); re-point those first", ErrAddressAliasInvalid, a.URN(), dependents)
	}
	if existing == "" {
		if _, err := tx.ExecContext(ctx, `INSERT INTO address_aliases(alias_urn, canonical_urn, created_at, created_by) VALUES(?,?,?,?)`,
			a.URN(), c.URN(), time.Now().UTC().Format(time.RFC3339Nano), by); err != nil {
			return AddressAlias{}, 0, err
		}
	}
	var row AddressAlias
	var created string
	if err := tx.QueryRowContext(ctx, `SELECT alias_urn, canonical_urn, created_at, created_by FROM address_aliases WHERE alias_urn=?`, a.URN()).
		Scan(&row.Alias, &row.Canonical, &created, &row.CreatedBy); err != nil {
		return AddressAlias{}, 0, err
	}
	row.CreatedAt = parseMsgTime(created)
	var merged int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM messages WHERE routing_staged=0 AND to_urn=?`, a.URN()).Scan(&merged); err != nil {
		return AddressAlias{}, 0, err
	}
	if err := tx.Commit(); err != nil {
		return AddressAlias{}, 0, err
	}
	s.addrAliases.invalidate()
	return row, merged, nil
}

// RemoveAddressAlias deletes one alias. Mail sent to the alias while it was one
// was stored under the canonical address and stays there; mail stored under the
// alias itself is again visible only from the alias.
func (s *Store) RemoveAddressAlias(ctx context.Context, alias string) error {
	a, err := messaging.ParseURN(alias)
	if err != nil {
		return fmt.Errorf("%w: alias %q is not a msg:// address", ErrAddressAliasInvalid, alias)
	}
	res, err := s.db.ExecContext(ctx, `DELETE FROM address_aliases WHERE alias_urn=?`, a.URN())
	if err != nil {
		return err
	}
	s.addrAliases.invalidate()
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrAddressAliasNotFound
	}
	return nil
}

// ListAddressAliases returns every alias, or only those of canonical when it is
// not empty, ordered by canonical address then alias.
func (s *Store) ListAddressAliases(ctx context.Context, canonical string) ([]AddressAlias, error) {
	q := `SELECT alias_urn, canonical_urn, created_at, created_by FROM address_aliases`
	var args []any
	if canonical != "" {
		q += ` WHERE canonical_urn=?`
		args = append(args, canonical)
	}
	rows, err := s.db.QueryContext(ctx, q+` ORDER BY canonical_urn, alias_urn`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AddressAlias{}
	for rows.Next() {
		var a AddressAlias
		var created string
		if err := rows.Scan(&a.Alias, &a.Canonical, &created, &a.CreatedBy); err != nil {
			return nil, err
		}
		a.CreatedAt = parseMsgTime(created)
		out = append(out, a)
	}
	return out, rows.Err()
}

// CanonicalAddress is the address urn's mail is stored under: the canonical
// address when urn is an alias, else urn itself.
func (s *Store) CanonicalAddress(ctx context.Context, urn string) (string, error) {
	return s.addrAliases.canonicalOf(ctx, s.db, urn)
}

// AddressForms is every address of urn's mailbox: the canonical address first,
// then its aliases. An address with no alias rows is its own only form.
func (s *Store) AddressForms(ctx context.Context, urn string) ([]string, error) {
	return s.addrAliases.forms(ctx, s.db, urn)
}

func (c *addressAliases) invalidate() {
	c.mu.Lock()
	c.loadedAt = time.Time{}
	c.mu.Unlock()
}

// load refreshes the map when it is older than addressAliasTTL. A failed
// refresh is an error, never an empty map: resolving against a table that could
// not be read would store mail under an alias as if it were not one.
func (c *addressAliases) load(ctx context.Context, db *sql.DB) error {
	if c.canonical != nil && time.Since(c.loadedAt) < addressAliasTTL {
		return nil
	}
	rows, err := db.QueryContext(ctx, `SELECT alias_urn, canonical_urn FROM address_aliases`)
	if err != nil {
		return fmt.Errorf("address aliases: load: %w", err)
	}
	defer rows.Close()
	canonical, aliases := map[string]string{}, map[string][]string{}
	for rows.Next() {
		var a, t string
		if err := rows.Scan(&a, &t); err != nil {
			return fmt.Errorf("address aliases: load: %w", err)
		}
		canonical[a] = t
		aliases[t] = append(aliases[t], a)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("address aliases: load: %w", err)
	}
	for _, list := range aliases {
		sort.Strings(list)
	}
	c.canonical, c.aliases, c.loadedAt = canonical, aliases, time.Now()
	return nil
}

func (c *addressAliases) canonicalOf(ctx context.Context, db *sql.DB, urn string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.load(ctx, db); err != nil {
		return "", err
	}
	if t, ok := c.canonical[urn]; ok {
		return t, nil
	}
	return urn, nil
}

func (c *addressAliases) forms(ctx context.Context, db *sql.DB, urn string) ([]string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.load(ctx, db); err != nil {
		return nil, err
	}
	if t, ok := c.canonical[urn]; ok {
		urn = t
	}
	return append([]string{urn}, c.aliases[urn]...), nil
}

// canonicalizeMail rewrites a mailbox envelope's To from an alias to its
// canonical address before it is stored, and records the address the caller
// used in metadata so a sender still using an old address can be found.
//
// From is deliberately left as sent. Receivers decide what a message is by its
// sender (mail triage and agent boot texts single out the operator address),
// so an alias must not change who a message appears to be from. A reply goes
// to that From, which resolves to the canonical mailbox like any other send.
func (c *addressAliases) canonicalizeMail(ctx context.Context, db *sql.DB, env *messaging.Envelope) error {
	rewrite := func(addr *messaging.Address, key string) error {
		if addr.IsZero() {
			return nil
		}
		was := addr.URN()
		canon, err := c.canonicalOf(ctx, db, was)
		if err != nil || canon == was {
			return err
		}
		parsed, err := messaging.ParseURN(canon)
		if err != nil {
			return fmt.Errorf("address aliases: canonical address %q of %s: %w", canon, was, err)
		}
		*addr = parsed
		meta := make(map[string]string, len(env.Metadata)+1)
		for k, v := range env.Metadata {
			meta[k] = v
		}
		meta[key] = was
		env.Metadata = meta
		return nil
	}
	return rewrite(&env.To, MetaAliasTo)
}

// MetaAliasTo is the envelope metadata written when a send was addressed to an
// alias: the address the caller used, before it was resolved to the canonical one.
const MetaAliasTo = "tether.alias_to"

// mailboxForms is AddressForms for a mailbox read.
func (ms *messagingStore) mailboxForms(ctx context.Context, to messaging.Address) ([]string, error) {
	if ms.owner == nil {
		return []string{to.URN()}, nil
	}
	return ms.owner.addrAliases.forms(ctx, ms.db, to.URN())
}

// sameMailbox reports whether stored to_urn belongs to recipient's mailbox.
func (ms *messagingStore) sameMailbox(ctx context.Context, toURN string, recipient messaging.Address) (bool, error) {
	forms, err := ms.mailboxForms(ctx, recipient)
	if err != nil {
		return false, err
	}
	for _, f := range forms {
		if f == toURN {
			return true, nil
		}
	}
	return false, nil
}

// toURNIn is the "to_urn IN (?, ...)" predicate over forms and its arguments.
func toURNIn(forms []string) (string, []any) {
	args := make([]any, len(forms))
	for i, f := range forms {
		args[i] = f
	}
	return "to_urn IN (?" + repeatCommaQ(len(forms)-1) + ")", args
}

// CanonicalizeMail resolves env's alias recipient to its canonical address, as
// Send does, for a caller that must check or route the recipient first.
func (s *Store) CanonicalizeMail(ctx context.Context, env *messaging.Envelope) error {
	return s.addrAliases.canonicalizeMail(ctx, s.db, env)
}
