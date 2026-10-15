package secretsetup

import (
	"context"
	"errors"
	"strings"
	"testing"

	secrets "github.com/hollis-labs/libs/security/secrets"
	"github.com/hollis-labs/libs/security/secrets/secretstest"
)

func TestLegacyAccountAndEnvNames(t *testing.T) {
	ref, _ := secrets.ParseRef("keychain://openai/work")
	if got := LegacyAccount(ref); got != "provider-api-key:openai/work" {
		t.Fatalf("LegacyAccount = %q", got)
	}
	got := EnvNames(ref)
	if len(got) != 2 || got[0] != "TETHER_APIKEY_OPENAI_WORK" || got[1] != "OPENAI_API_KEY" {
		t.Fatalf("EnvNames = %v", got)
	}
}

func TestResolveReadsLegacyTetherEntry(t *testing.T) {
	store := &secretstest.FakeStore{}
	store.Put(LegacyService, "provider-api-key:openai/work", "sk-legacy")
	r := New(secrets.WithStore(store), secrets.WithEnvLookup(func(string) (string, bool) { return "", false }))
	res, err := r.Resolve(context.Background(), "keychain://openai/work")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if res.Secret.Reveal() != "sk-legacy" || res.Source != "keychain:legacy:tether" {
		t.Fatalf("source = %q", res.Source)
	}
}

func TestEntryBeatsEnvAndEnvIsFallback(t *testing.T) {
	env := map[string]string{"OPENAI_API_KEY": "sk-env"}
	lookup := func(k string) (string, bool) { v, ok := env[k]; return v, ok }
	store := &secretstest.FakeStore{}
	r := New(secrets.WithStore(store), secrets.WithEnvLookup(lookup))
	res, err := r.Resolve(context.Background(), "keychain://openai/work")
	if err != nil || res.Source != "env:OPENAI_API_KEY" {
		t.Fatalf("fallback: %v %q", err, res.Source)
	}
	store.Put(secrets.DefaultService, "openai/work", "sk-store")
	res, err = r.Resolve(context.Background(), "keychain://openai/work")
	if err != nil || res.Secret.Reveal() != "sk-store" {
		t.Fatalf("entry should win: %v", err)
	}
}

func TestMissingIsTypedNotFoundWithoutLeaking(t *testing.T) {
	r := New(secrets.WithStore(&secretstest.FakeStore{}), secrets.WithEnvLookup(func(string) (string, bool) { return "", false }))
	_, err := r.Resolve(context.Background(), "keychain://openai/work")
	if !errors.Is(err, secrets.ErrNotFound) {
		t.Fatalf("err = %v, want not_found", err)
	}
	var se *secrets.Error
	if !errors.As(err, &se) || se.Code() != "secret_not_found" || strings.TrimSpace(se.Guidance()) == "" {
		t.Fatalf("not a typed error with guidance: %#v", err)
	}
}
