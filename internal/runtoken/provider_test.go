package runtoken

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hollis-labs/libs/harness/core/sandbox"
)

func providerValue(env []string, key string) string {
	for _, e := range env {
		k, v, _ := strings.Cut(e, "=")
		if k == key {
			return v
		}
	}
	return ""
}

func TestProviderFileDeliveryAndExplicitFallback(t *testing.T) {
	const proof = "synthetic-provider-run-proof"
	env, release, err := ProviderEnvironment(SealEnvironment([]string{"KEEP=1", FileEnv + "=/untrusted"}, map[string]string{TokenEnv: proof}))
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	path := providerValue(env, FileEnv)
	if path == "" || providerValue(env, TokenEnv) != "" || strings.Contains(strings.Join(env, "\n"), proof) {
		t.Fatal("proof exposed in provider environment")
	}
	st, err := os.Stat(path)
	if err != nil || st.Mode().Perm() != 0600 {
		t.Fatal("proof file is not private")
	}
	st, err = os.Stat(filepath.Dir(path))
	if err != nil || st.Mode().Perm() != 0700 {
		t.Fatal("proof directory is not private")
	}
	if _, _, err := ProviderEnvironment([]string{FileEnv + "=" + path, TokenEnv + "=different-proof"}); err == nil {
		t.Fatal("file and handoff mismatch accepted")
	}
	if got, err := ReadProviderToken(path, "untrusted", true); err != nil || got != proof {
		t.Fatal("file authority lost")
	}
	if _, err := ReadProviderToken(path+"missing", proof, true); err == nil {
		t.Fatal("unreadable file fell back to environment")
	}
	if _, err := ReadProviderToken("", proof, false); err == nil {
		t.Fatal("implicit env fallback")
	}
	if got, err := ReadProviderToken("", proof, true); err != nil || got != proof {
		t.Fatal("explicit fallback failed")
	}
	fallback, closeFallback, err := ProviderEnvironment([]string{TokenEnv + "=" + proof, EnvFallback + "=1"})
	if err != nil {
		t.Fatal(err)
	}
	defer closeFallback()
	if providerValue(fallback, TokenEnv) != proof || providerValue(fallback, FileEnv) != "" {
		t.Fatal("fallback not honored")
	}
	release()
	if _, err := ReadProviderToken(path, "", false); err == nil {
		t.Fatal("released proof retained")
	}
}

func TestReadProviderFileRejectsUnsafeAndSymlink(t *testing.T) {
	p := filepath.Join(t.TempDir(), "proof")
	if err := os.WriteFile(p, []byte("synthetic"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadProviderToken(p, "", false); err == nil {
		t.Fatal("unsafe mode accepted")
	}
	if err := os.Chmod(p, 0600); err != nil {
		t.Fatal(err)
	}
	link := p + "-link"
	if err := os.Symlink(p, link); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadProviderToken(link, "", false); err == nil {
		t.Fatal("symlink accepted")
	}
}

func TestProviderFilePolicyPreservesDenies(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "run.token")
	policy := &sandbox.ResolvedAccessPolicy{FS: sandbox.ResolvedFilesystemAccess{Deny: []sandbox.ResolvedPath{{Kind: sandbox.AccessDeny, Path: dir}}}}
	if _, err := ReadablePolicy(policy, file); err == nil {
		t.Fatal("deny overridden")
	}
	policy.FS.Deny = nil
	got, err := ReadablePolicy(policy, file)
	if err != nil || len(policy.Runtime) != 0 || got.AccessFor(file) != sandbox.AccessReadOnly {
		t.Fatal("isolated read grant failed")
	}
}
