// Package astore is the store code for class A tables (plan section 13.2) whose SQL lived in the old
// internal/store and internal/settings: execution policy, event identity, logical agents and settings.
// Every write goes through the class A handle; reads of other classes go through their own handle's
// read pool. The class A tables owned by other slices reach the same handle from their own packages:
// principals and pairing grants (S3), team definitions, fabric definitions and workers (their domain
// slices).
//
// Timestamps keep the old text formats, so rows written before and after the cutover read the same.
package astore

import (
	"time"

	"github.com/hollis-labs/tether/next/internal/store"
)

// Store reaches the class A tables.
type Store struct {
	s   *store.Stores
	now func() time.Time
}

// New returns the class A store over the class handles. now is the clock (time.Now when nil).
func New(s *store.Stores, now func() time.Time) *Store {
	if now == nil {
		now = time.Now
	}
	return &Store{s: s, now: now}
}

// nullIfEmpty stores an empty string as NULL, as the old store did for nullable text columns.
func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
