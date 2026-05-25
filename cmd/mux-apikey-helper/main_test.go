package main

import (
	"bytes"
	"errors"
	"testing"

	"github.com/zalando/go-keyring"
)

func TestAccountName(t *testing.T) {
	ref, err := parseKeychainRef("keychain://openai/work")
	if err != nil {
		t.Fatalf("parse ref: %v", err)
	}
	if got, want := accountName(ref), "provider-api-key:openai/work"; got != want {
		t.Fatalf("accountName = %q, want %q", got, want)
	}
}

func TestEnvKeysForRef(t *testing.T) {
	ref, err := parseKeychainRef("keychain://openai/work")
	if err != nil {
		t.Fatalf("parse ref: %v", err)
	}
	got := envKeysForRef(ref)
	want := []string{"MUX_APIKEY_OPENAI_WORK", "OPENAI_API_KEY"}
	if len(got) != len(want) {
		t.Fatalf("len(envKeysForRef) = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("envKeysForRef[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestResolvePrefersEnv(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "sk-openai-test")
	secret, err := resolveRef("keychain://openai/work")
	if err != nil {
		t.Fatalf("resolveRef: %v", err)
	}
	if secret != "sk-openai-test" {
		t.Fatalf("secret = %q, want %q", secret, "sk-openai-test")
	}
}

func TestRunSetDeleteLifecycle(t *testing.T) {
	store := map[string]string{}
	readSecret = func(account string) (string, error) {
		value, ok := store[account]
		if !ok {
			return "", keyring.ErrNotFound
		}
		return value, nil
	}
	writeSecret = func(account, password string) error {
		store[account] = password
		return nil
	}
	deleteSecret = func(account string) error {
		delete(store, account)
		return nil
	}
	t.Cleanup(func() {
		readSecret = defaultReadSecret
		writeSecret = defaultWriteSecret
		deleteSecret = defaultDeleteSecret
	})

	if err := run([]string{"set", "keychain://openai/test"}, bytes.NewBufferString("sk-openai-store\n"), &bytes.Buffer{}); err != nil {
		t.Fatalf("run set: %v", err)
	}
	got, err := readSecret("provider-api-key:openai/test")
	if err != nil {
		t.Fatalf("keyring get: %v", err)
	}
	if got != "sk-openai-store" {
		t.Fatalf("stored secret = %q, want %q", got, "sk-openai-store")
	}

	var out bytes.Buffer
	if err := run([]string{"resolve", "keychain://openai/test"}, bytes.NewBuffer(nil), &out); err != nil {
		t.Fatalf("run resolve: %v", err)
	}
	if out.String() != "sk-openai-store\n" {
		t.Fatalf("resolve output = %q", out.String())
	}

	if err := run([]string{"delete", "keychain://openai/test"}, bytes.NewBuffer(nil), &bytes.Buffer{}); err != nil {
		t.Fatalf("run delete: %v", err)
	}
	if _, err := readSecret("provider-api-key:openai/test"); !errors.Is(err, keyring.ErrNotFound) {
		t.Fatalf("expected deleted key to be missing")
	}
}

func TestParseRejectsHelperRefs(t *testing.T) {
	if _, err := parseKeychainRef("helper://mux-apikey-helper/openai/work"); err == nil {
		t.Fatal("expected helper:// ref to fail")
	}
}

func TestMainRunUsage(t *testing.T) {
	err := run(nil, bytes.NewBuffer(nil), &bytes.Buffer{})
	if err == nil {
		t.Fatal("expected usage error")
	}
}
