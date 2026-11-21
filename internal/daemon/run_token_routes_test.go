package daemon

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/identity"
)

type runRevokeService struct {
	api.LaunchService
	targets chan string
}

func (s runRevokeService) RevokeRunToken(_ context.Context, id string) error {
	s.targets <- id
	return nil
}

// Drive the actual daemon mount table and identity middleware: a directly
// registered API route can still be unreachable from the production router.
func TestRunTokenRevokeWorksThroughDaemonUnixSocket(t *testing.T) {
	s, _, operator := remoteTestServer(t)
	targets := make(chan string, 1)
	s.Service = runRevokeService{targets: targets}
	dir, err := os.MkdirTemp("", "rtrevoke-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "s")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: s.Handler(), ConnContext: identity.ConnectionContext, ReadHeaderTimeout: time.Second}
	go func() { _ = server.Serve(ln) }()
	t.Cleanup(func() { _ = server.Close() })
	client := DialHTTPClient("unix:" + sock)
	client.Timeout = 5 * time.Second
	req, err := http.NewRequest(http.MethodPost, BaseURL("unix:"+sock)+api.RunTokenRevokePath, strings.NewReader(`{"target":"execution-to-revoke"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+operator)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("daemon revoke status %d", resp.StatusCode)
	}
	select {
	case target := <-targets:
		if target != "execution-to-revoke" {
			t.Fatal("revocation target changed")
		}
	default:
		t.Fatal("daemon never invoked revocation")
	}
	remote := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "http://remote"+api.RunTokenRevokePath, strings.NewReader(`{"target":"remote-target"}`))
	r.Header.Set("Authorization", "Bearer "+operator)
	s.RemoteHandler("127.0.0.1:7331").ServeHTTP(remote, r)
	if remote.Code != http.StatusForbidden && remote.Code != http.StatusUnauthorized {
		t.Fatalf("remote revoke status %d", remote.Code)
	}
	select {
	case <-targets:
		t.Fatal("remote request revoked a proof")
	default:
	}
}
