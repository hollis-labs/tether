package a2aadapter_test

// config_test.go — NewAdapter's fail-closed construction-time validation.
// Complements adapter_test.go's fixture interop coverage.

import (
	"path/filepath"
	"testing"

	"github.com/hollis-labs/tether/internal/a2aadapter"
	"github.com/hollis-labs/tether/internal/store"
)

func openTestStore(t *testing.T) *store.Store {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "a2a-config.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestNewAdapter_RejectsMisconfiguredBindings(t *testing.T) {
	cases := []struct {
		name    string
		binding a2aadapter.AgentBinding
	}{
		{"empty ID", a2aadapter.AgentBinding{TargetURN: targetURN, BaseURL: "http://x"}},
		{"ID with slash", a2aadapter.AgentBinding{ID: "a/b", TargetURN: targetURN, BaseURL: "http://x"}},
		{"ID with whitespace", a2aadapter.AgentBinding{ID: "a b", TargetURN: targetURN, BaseURL: "http://x"}},
		{"empty TargetURN", a2aadapter.AgentBinding{ID: "a", BaseURL: "http://x"}},
		{"invalid TargetURN", a2aadapter.AgentBinding{ID: "a", TargetURN: "not-a-urn", BaseURL: "http://x"}},
		{"empty BaseURL", a2aadapter.AgentBinding{ID: "a", TargetURN: targetURN}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := openTestStore(t)
			if _, err := a2aadapter.NewAdapter(a2aadapter.Config{Bindings: []a2aadapter.AgentBinding{tc.binding}}, db.MessagingStore()); err == nil {
				t.Fatalf("NewAdapter with %s: expected an error, got nil", tc.name)
			}
		})
	}
}

func TestNewAdapter_RejectsDuplicateBindingID(t *testing.T) {
	db := openTestStore(t)
	cfg := a2aadapter.Config{Bindings: []a2aadapter.AgentBinding{
		{ID: "dup", TargetURN: targetURN, BaseURL: "http://x"},
		{ID: "dup", TargetURN: targetURN, BaseURL: "http://y"},
	}}
	if _, err := a2aadapter.NewAdapter(cfg, db.MessagingStore()); err == nil {
		t.Fatal("expected an error for duplicate binding IDs, got nil")
	}
}

func TestNewAdapter_EmptyConfigIsValidAndServesNothing(t *testing.T) {
	db := openTestStore(t)
	adapter, err := a2aadapter.NewAdapter(a2aadapter.Config{}, db.MessagingStore())
	if err != nil {
		t.Fatalf("NewAdapter with empty config: %v", err)
	}
	if adapter.Mux() == nil {
		t.Fatal("Mux() returned nil for an empty config")
	}
}
