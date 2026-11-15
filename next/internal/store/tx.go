package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"regexp"
	"strings"

	"github.com/hollis-labs/libs/core/store"
)

// Tx is the transaction a WriteTx or Joint callback receives. Placeholders are rebound for the dialect.
// Every statement is attributed before it runs: a write to a table outside the transaction's classes is
// refused with ErrCrossClassWrite, and a statement that cannot be attributed with ErrUnclassified.
type Tx interface {
	store.Querier
}

type checkedTx struct {
	store.Tx
	label   string
	allowed map[Class]bool
}

func (t *checkedTx) ExecContext(ctx context.Context, q string, args ...any) (sql.Result, error) {
	if err := t.check(q); err != nil {
		return nil, err
	}
	return t.Tx.ExecContext(ctx, q, args...)
}

func (t *checkedTx) QueryContext(ctx context.Context, q string, args ...any) (*sql.Rows, error) {
	if err := t.check(q); err != nil {
		return nil, err
	}
	return t.Tx.QueryContext(ctx, q, args...)
}

// QueryRowContext checks too (an INSERT ... RETURNING is a write). A refused statement still has to
// come back as a *sql.Row; running it with no rows makes Scan return the refusal.
func (t *checkedTx) QueryRowContext(ctx context.Context, q string, args ...any) *sql.Row {
	if err := t.check(q); err != nil {
		return t.Tx.QueryRowContext(ctx, "SELECT 1 WHERE CAST(? AS TEXT) IS NULL", refusal{err})
	}
	return t.Tx.QueryRowContext(ctx, q, args...)
}

// refusal is an argument whose conversion fails, so the driver returns err from Scan.
type refusal struct{ err error }

func (r refusal) Value() (driver.Value, error) { return nil, r.err }

func (t *checkedTx) check(q string) error {
	table, write, err := writeTarget(q)
	if err != nil {
		return fmt.Errorf("%w: %s: %w", ErrUnclassified, t.label, err)
	}
	if !write {
		return nil
	}
	c, ok := ClassOf(table)
	if !ok {
		return fmt.Errorf("%w: %s writes %s, which has no class", ErrUnclassified, t.label, table)
	}
	if !t.allowed[c] {
		return fmt.Errorf("%w: %s writes %s (class %s)", ErrCrossClassWrite, t.label, table, c)
	}
	return nil
}

var (
	lineCommentRe = regexp.MustCompile(`--[^\n]*`)
	quotedRe      = regexp.MustCompile(`'(?:[^']|'')*'`)
	// writeRe finds the target of a DML write, after an optional leading WITH clause: the statement it
	// qualifies follows a ')' that closes a CTE body.
	writeRe = regexp.MustCompile(`(?is)^(?:WITH\b.*?\)\s*)?(INSERT\s+(?:OR\s+\w+\s+)?INTO|REPLACE\s+INTO|UPDATE(?:\s+OR\s+\w+)?|DELETE\s+FROM)\s+"?([A-Za-z_][A-Za-z0-9_]*)"?`)
	readRe  = regexp.MustCompile(`(?is)^(SELECT|VALUES|WITH)\b`)
	dmlRe   = regexp.MustCompile(`(?i)\b(INSERT|UPDATE|DELETE)\b`)
)

// writeTarget returns the table a statement writes, write=false for a read, or an error for a statement
// it cannot attribute (DDL, PRAGMA, several statements in one string).
func writeTarget(q string) (table string, write bool, err error) {
	s := strings.TrimSpace(quotedRe.ReplaceAllString(lineCommentRe.ReplaceAllString(q, " "), "''"))
	if i := strings.Index(s, ";"); i >= 0 && strings.TrimSpace(s[i+1:]) != "" {
		return "", false, fmt.Errorf("more than one statement in one call: %.60q", q)
	}
	if m := writeRe.FindStringSubmatch(s); m != nil {
		return strings.ToLower(m[2]), true, nil
	}
	if readRe.MatchString(s) {
		if strings.HasPrefix(strings.ToUpper(s), "WITH") && dmlRe.MatchString(s) {
			return "", false, fmt.Errorf("a WITH statement that writes but has no recognizable target: %.60q", q)
		}
		return "", false, nil
	}
	return "", false, fmt.Errorf("not a read or a DML write: %.60q", q)
}
