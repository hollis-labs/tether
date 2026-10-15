package config

import (
	"context"
	"errors"
	"strings"
	"testing"

	secrets "github.com/hollis-labs/libs/security/secrets"
	"github.com/hollis-labs/libs/security/secrets/secretstest"
	"github.com/hollis-labs/tether/internal/secretsetup"
)

// A missing keychain entry reaches the caller as a typed go-secrets error, with
// the field and reference named and no value in the text.
func TestResolveSecretRefReturnsTypedError(t *testing.T) {
	prev := secretResolver
	secretResolver = secretsetup.New(
		secrets.WithStore(&secretstest.FakeStore{}),
		secrets.WithEnvLookup(func(string) (string, bool) { return "", false }),
	)
	t.Cleanup(func() { secretResolver = prev })

	_, err := resolveSecretRef(context.Background(), "env.OPENAI_API_KEY", "keychain://openai/work")
	if !errors.Is(err, secrets.ErrNotFound) {
		t.Fatalf("err = %v, want errors.Is not_found", err)
	}
	var se *secrets.Error
	if !errors.As(err, &se) || se.Code() != "secret_not_found" {
		t.Fatalf("want *secrets.Error secret_not_found, got %#v", err)
	}
	for _, want := range []string{"env.OPENAI_API_KEY", "keychain://openai/work"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
}

// An entry written under the old tether service is still read.
func TestResolveSecretRefReadsLegacyTetherEntry(t *testing.T) {
	store := &secretstest.FakeStore{}
	store.Put(secretsetup.LegacyService, "provider-api-key:openai/work", "legacy-value")
	prev := secretResolver
	secretResolver = secretsetup.New(secrets.WithStore(store))
	t.Cleanup(func() { secretResolver = prev })

	got, err := resolveSecretRef(context.Background(), "token", "keychain://openai/work")
	if err != nil || got != "legacy-value" {
		t.Fatalf("got %q, err %v", got, err)
	}
}
