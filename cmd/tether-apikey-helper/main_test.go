package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	secrets "github.com/hollis-labs/go-secrets"
	"github.com/hollis-labs/go-secrets/secretstest"
	"github.com/hollis-labs/tether/internal/secretsetup"
)

func useStore(t *testing.T) *secretstest.FakeStore {
	t.Helper()
	fs := &secretstest.FakeStore{}
	prev := store
	store = fs
	t.Cleanup(func() { store = prev })
	// No ambient provider keys leak into the env fallback.
	for _, k := range []string{"OPENAI_API_KEY", "ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "TETHER_APIKEY_OPENAI_TEST"} {
		t.Setenv(k, "")
	}
	return fs
}

func do(args []string, stdin string) (string, error) {
	var out bytes.Buffer
	err := run(context.Background(), args, strings.NewReader(stdin), &out)
	return out.String(), err
}

func TestSetResolveDeleteLifecycle(t *testing.T) {
	fs := useStore(t)
	if _, err := do([]string{"set", "keychain://openai/test"}, "sk-openai-store\n"); err != nil {
		t.Fatalf("set: %v", err)
	}
	if !fs.Has(secrets.DefaultService, "openai/test") {
		t.Fatal("secret not written to the shared service")
	}
	out, err := do([]string{"resolve", "keychain://openai/test"}, "")
	if err != nil || out != "sk-openai-store\n" {
		t.Fatalf("resolve = %q, %v", out, err)
	}
	if _, err := do([]string{"delete", "keychain://openai/test"}, ""); err != nil {
		t.Fatalf("delete: %v", err)
	}
	_, err = do([]string{"resolve", "keychain://openai/test"}, "")
	if !errors.Is(err, secrets.ErrNotFound) || exitCode(err) != exitNotFound {
		t.Fatalf("after delete: %v (exit %d)", err, exitCode(err))
	}
}

// The secret is never accepted as an argument, so it cannot reach argv.
func TestSetRefusesSecretArgument(t *testing.T) {
	fs := useStore(t)
	const canary = "CANARY-argv-secret"
	_, err := do([]string{"set", "keychain://openai/test", canary}, "")
	if err == nil {
		t.Fatal("set with a secret argument must fail")
	}
	if strings.Contains(err.Error(), canary) {
		t.Fatal("error echoes the argument secret")
	}
	if len(fs.Calls()) != 0 {
		t.Fatalf("store touched: %v", fs.Calls())
	}
}

func TestSetRejectsEmptyStdin(t *testing.T) {
	useStore(t)
	_, err := do([]string{"set", "keychain://openai/test"}, "  \n")
	if !errors.Is(err, secrets.ErrEmpty) {
		t.Fatalf("err = %v, want empty", err)
	}
}

func TestResolveReadsLegacyTetherEntry(t *testing.T) {
	fs := useStore(t)
	fs.Put(secretsetup.LegacyService, "provider-api-key:openai/test", "sk-legacy")
	out, err := do([]string{"resolve", "keychain://openai/test"}, "")
	if err != nil || out != "sk-legacy\n" {
		t.Fatalf("resolve = %q, %v", out, err)
	}
}

func TestDeleteAlsoRemovesLegacyEntry(t *testing.T) {
	fs := useStore(t)
	fs.Put(secretsetup.LegacyService, "provider-api-key:openai/test", "sk-legacy")
	if _, err := do([]string{"delete", "keychain://openai/test"}, ""); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if fs.Has(secretsetup.LegacyService, "provider-api-key:openai/test") {
		t.Fatal("legacy entry survived delete")
	}
}

func TestResolveFallsBackToEnvAfterKeychain(t *testing.T) {
	useStore(t)
	t.Setenv("OPENAI_API_KEY", "sk-env")
	out, err := do([]string{"resolve", "keychain://openai/test"}, "")
	if err != nil || out != "sk-env\n" {
		t.Fatalf("resolve = %q, %v", out, err)
	}
}

func TestRejectsNonKeychainRefs(t *testing.T) {
	useStore(t)
	for _, ref := range []string{"helper://tether-apikey-helper/openai/work", "file://openai/work", "env://X"} {
		if _, err := do([]string{"set", ref}, "v"); err == nil {
			t.Errorf("set %s should fail", ref)
		}
	}
}

func TestUsageAndVersion(t *testing.T) {
	if _, err := do(nil, ""); err == nil {
		t.Fatal("expected usage error")
	}
	if out, err := do([]string{"--version"}, ""); err != nil || !strings.HasPrefix(out, "tether-apikey-helper ") {
		t.Fatalf("version = %q, %v", out, err)
	}
}
