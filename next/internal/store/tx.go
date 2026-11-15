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

// readOnly is a Handle's read pool: it refuses anything but a read.
type readOnly struct{ store.Querier }

func readOnlyCheck(q string) error {
	tables, err := writeTargets(q)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrWriteOnReader, err)
	}
	if len(tables) > 0 {
		return fmt.Errorf("%w: writes %s", ErrWriteOnReader, strings.Join(tables, ", "))
	}
	return nil
}

func (r readOnly) ExecContext(ctx context.Context, q string, args ...any) (sql.Result, error) {
	if err := readOnlyCheck(q); err != nil {
		return nil, err
	}
	return r.Querier.ExecContext(ctx, q, args...)
}

func (r readOnly) QueryContext(ctx context.Context, q string, args ...any) (*sql.Rows, error) {
	if err := readOnlyCheck(q); err != nil {
		return nil, err
	}
	return r.Querier.QueryContext(ctx, q, args...)
}

func (r readOnly) QueryRowContext(ctx context.Context, q string, args ...any) *sql.Row {
	if err := readOnlyCheck(q); err != nil {
		return r.Querier.QueryRowContext(ctx, "SELECT 1 WHERE CAST(? AS TEXT) IS NULL", refusal{err})
	}
	return r.Querier.QueryRowContext(ctx, q, args...)
}

func (t *checkedTx) check(q string) error {
	tables, err := writeTargets(q)
	if err != nil {
		return fmt.Errorf("%w: %s: %w", ErrUnclassified, t.label, err)
	}
	for _, table := range tables {
		c, ok := ClassOf(table)
		if !ok {
			return fmt.Errorf("%w: %s writes %s, which has no class", ErrUnclassified, t.label, table)
		}
		if !t.allowed[c] {
			return fmt.Errorf("%w: %s writes %s (class %s)", ErrCrossClassWrite, t.label, table, c)
		}
	}
	return nil
}

var (
	lineCommentRe = regexp.MustCompile(`--[^\n]*`)
	quotedRe      = regexp.MustCompile(`'(?:[^']|'')*'`)
	// writeRe finds the target of a statement that starts with DML.
	writeRe = regexp.MustCompile(`(?is)^(INSERT\s+(?:OR\s+\w+\s+)?INTO|REPLACE\s+INTO|UPDATE(?:\s+OR\s+\w+)?|DELETE\s+FROM)\s+"?([A-Za-z_][A-Za-z0-9_]*)"?`)
	readRe  = regexp.MustCompile(`(?is)^(SELECT|VALUES)\b`)
	withRe  = regexp.MustCompile(`(?is)^WITH\b`)
	// dmlRe finds every DML keyword in a WITH statement: the main statement's and any in a CTE body
	// (Postgres runs data-modifying CTEs). Group 1 is the keyword, group 2 the word after it.
	dmlRe = regexp.MustCompile(`(?is)\b(INSERT\s+(?:OR\s+\w+\s+)?INTO|REPLACE\s+INTO|UPDATE(?:\s+OR\s+\w+)?|DELETE\s+FROM|INSERT|DELETE|REPLACE)\b\s*("?[A-Za-z_][A-Za-z0-9_]*"?|\()?`)
	// prevWordRe is the word before a match, to tell ON CONFLICT DO UPDATE and FOR [NO KEY] UPDATE from
	// a DML UPDATE.
	prevWordRe = regexp.MustCompile(`(?i)(\w+)\s*$`)
)

// writeTargets returns every table a statement writes (none for a read), or an error for a statement it
// cannot attribute: DDL, PRAGMA, several statements in one string, or DML it cannot find the target of.
func writeTargets(q string) ([]string, error) {
	s := strings.TrimSpace(quotedRe.ReplaceAllString(lineCommentRe.ReplaceAllString(q, " "), "''"))
	if i := strings.Index(s, ";"); i >= 0 && strings.TrimSpace(s[i+1:]) != "" {
		return nil, fmt.Errorf("more than one statement in one call: %.60q", q)
	}
	if m := writeRe.FindStringSubmatch(s); m != nil {
		return []string{strings.ToLower(m[2])}, nil
	}
	if readRe.MatchString(s) {
		return nil, nil
	}
	if !withRe.MatchString(s) {
		return nil, fmt.Errorf("not a read or a DML write: %.60q", q)
	}
	var tables []string
	seen := map[string]bool{}
	for _, m := range dmlRe.FindAllStringSubmatchIndex(s, -1) {
		kw := strings.ToUpper(strings.Join(strings.Fields(s[m[2]:m[3]]), " "))
		next := ""
		if m[4] >= 0 {
			next = strings.Trim(s[m[4]:m[5]], `"`)
		}
		prev := ""
		if pm := prevWordRe.FindStringSubmatch(s[:m[0]]); pm != nil {
			prev = strings.ToUpper(pm[1])
		}
		switch {
		case strings.HasPrefix(kw, "UPDATE") && (prev == "DO" || prev == "FOR" || prev == "KEY"):
			continue // ON CONFLICT DO UPDATE, FOR [NO KEY] UPDATE: not a target
		case kw == "REPLACE" && next == "(":
			continue // the replace() function
		case kw == "INSERT" || kw == "DELETE" || kw == "REPLACE" || next == "" || next == "(":
			return nil, fmt.Errorf("DML in a WITH statement without a recognizable target: %.60q", q)
		}
		t := strings.ToLower(next)
		if !seen[t] {
			seen[t] = true
			tables = append(tables, t)
		}
	}
	return tables, nil
}
