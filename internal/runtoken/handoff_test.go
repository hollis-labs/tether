//go:build linux

package runtoken

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func envValue(env []string, key string) string {
	for _, v := range env {
		k, v, _ := strings.Cut(v, "=")
		if k == key {
			return v
		}
	}
	return ""
}
func TestRuntimeHandoffNeverPersistsTokenAndRefusesStaleRecovery(t *testing.T) {
	const token = "synthetic-run-token-that-must-not-persist"
	original := []string{"KEEP=1", TokenEnv + "=" + token}
	durable, release, err := PrepareEnvironment(original, "session")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	b, _ := json.Marshal(durable)
	if strings.Contains(string(b), token) || envValue(durable, TokenEnv) != "" {
		t.Fatal("token entered durable environment")
	}
	runtimeEnv, err := RestoreEnvironment(durable, "session")
	if err != nil || envValue(runtimeEnv, TokenEnv) != token || envValue(runtimeEnv, handoffPathEnv) != "" {
		t.Fatal("runtime token handoff failed")
	}
	if envValue(original, TokenEnv) != token {
		t.Fatal("caller environment mutated")
	}
	if _, err := RestoreEnvironment(durable, "other"); !errors.Is(err, ErrHandoff) {
		t.Fatal("cross-session handoff accepted")
	}
	if _, err := RestoreEnvironment(original, "session"); !errors.Is(err, ErrHandoff) {
		t.Fatal("durable raw token accepted")
	}
	release()
	if _, err := RestoreEnvironment(durable, "session"); !errors.Is(err, ErrHandoff) {
		t.Fatal("closed handoff downgraded to legacy")
	}
	next, closeNext, err := PrepareEnvironment(original, "session")
	if err != nil {
		t.Fatal(err)
	}
	defer closeNext()
	// Even a recycled parent FD must not turn an old descriptor into a new run.
	stale := append([]string{}, durable...)
	for i, v := range stale {
		if strings.HasPrefix(v, handoffPathEnv+"=") {
			stale[i] = handoffPathEnv + "=" + envValue(next, handoffPathEnv)
		}
	}
	if _, err := RestoreEnvironment(stale, "session"); !errors.Is(err, ErrHandoff) {
		t.Fatal("recycled memory reference accepted")
	}
}
func TestExecutionEnvironmentSealsAmbientValues(t *testing.T) {
	env := []string{"TETHER_RUN_ID=42", IDEnv + "=ambient", IDEnv + "=duplicate", TokenEnv + "=ambient", handoffPathEnv + "=/bad"}
	got := SealEnvironment(env, map[string]string{IDEnv: "fresh", TokenEnv: "fresh-token"})
	if envValue(got, "TETHER_RUN_ID") != "42" || envValue(got, IDEnv) != "fresh" || envValue(got, TokenEnv) != "fresh-token" {
		t.Fatal("authoritative fields lost")
	}
	count := 0
	for _, v := range got {
		if strings.HasPrefix(v, IDEnv+"=") {
			count++
		}
	}
	if count != 1 {
		t.Fatal("duplicate execution id")
	}
	legacy := SealEnvironment(env, nil)
	if envValue(legacy, IDEnv) != "" || envValue(legacy, TokenEnv) != "" {
		t.Fatal("legacy inherited ambient execution authority")
	}
}
