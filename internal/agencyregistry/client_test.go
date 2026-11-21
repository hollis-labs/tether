package agencyregistry

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestRegisterContractAndRetryClassification(t *testing.T) {
	for _, status := range []int{200, 400, 401, 403, 409, 429, 503} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "POST" || r.URL.Path != "/v1/correlations/register" || r.Header.Get("Idempotency-Key") != "stable-key" || r.Header.Get("Authorization") != "Bearer fake-test-credential" {
					t.Error("wrong wire contract")
				}
				w.WriteHeader(status)
				_, _ = w.Write([]byte("untrusted remote response, never propagate"))
			}))
			defer srv.Close()
			path := filepath.Join(t.TempDir(), "credential")
			if err := os.WriteFile(path, []byte("fake-test-credential"), 0600); err != nil {
				t.Fatal(err)
			}
			retry, err := Register(context.Background(), Config{URL: srv.URL, CredentialFile: path}, "stable-key", Facts{URN: "urn", Session: "session", RunID: "run"})
			if (err == nil) != (status == 200) || retry != (status == 429 || status >= 500) {
				t.Fatalf("status %d retry=%v err=%v", status, retry, err)
			}
		})
	}
}

func TestRegisterRefusesRedirectAndUnsafeCredential(t *testing.T) {
	calls := 0
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls++ }))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer source.Close()
	path := filepath.Join(t.TempDir(), "credential")
	if err := os.WriteFile(path, []byte("fake-test-credential"), 0600); err != nil {
		t.Fatal(err)
	}
	if retry, err := Register(context.Background(), Config{URL: source.URL, CredentialFile: path}, "key", Facts{}); err == nil || retry {
		t.Fatal("redirect accepted")
	}
	if calls != 0 {
		t.Fatal("credential followed redirect")
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := Register(context.Background(), Config{URL: target.URL, CredentialFile: path}, "key", Facts{}); err == nil {
		t.Fatal("unsafe credential accepted")
	}
	if calls != 0 {
		t.Fatal("unsafe credential sent")
	}
}
