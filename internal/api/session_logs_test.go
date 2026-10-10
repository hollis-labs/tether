package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/store"
)

func sessionLogFixture(t *testing.T, data []byte) (http.Handler, string) {
	t.Helper()
	ws := t.TempDir()
	if err := os.Mkdir(filepath.Join(ws, "logs"), 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(ws, "logs", "session.log")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	row := &store.SessionRow{ID: "s", Workspace: ws}
	svc := &fakeLaunchService{getRes: map[string]*store.SessionRow{"s": row}}
	return RemoteScopeMiddleware(NewHandler(Deps{Service: svc})), path
}

func requestSessionLog(t *testing.T, h http.Handler, query string, scopes []string, want int) SessionLogResponse {
	t.Helper()
	r := remotePathRequest("GET", "/sessions/s/log"+query, "")
	r = r.WithContext(identity.WithPrincipal(r.Context(), identity.Principal{ID: "device-test", Kind: "device", Scopes: scopes}))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != want {
		t.Fatalf("status %d want %d: %s", w.Code, want, w.Body.String())
	}
	var out SessionLogResponse
	if want == 200 {
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
	}
	return out
}

func TestSessionLogRangesAppendTruncateReplace(t *testing.T) {
	data := []byte{'a', 'b', 0xff, 0x00, 'c'}
	h, path := sessionLogFixture(t, data)
	first := requestSessionLog(t, h, "?offset=0&limit=3", []string{"terminal"}, 200)
	if !bytes.Equal(first.Data, data[:3]) || first.NextOffset != 3 || first.Size != 5 {
		t.Fatalf("first range: %+v", first)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.Write([]byte("more"))
	_ = f.Close()
	if err != nil {
		t.Fatal(err)
	}
	q := "?offset=3&generation=" + url.QueryEscape(first.Generation)
	appended := requestSessionLog(t, h, q, []string{"terminal"}, 200)
	if appended.Generation != first.Generation || !bytes.Equal(appended.Data, append(data[3:], []byte("more")...)) {
		t.Fatalf("append broke continuation: %+v", appended)
	}
	if err := os.Truncate(path, 1); err != nil {
		t.Fatal(err)
	}
	requestSessionLog(t, h, q, []string{"terminal"}, 409)
	short := requestSessionLog(t, h, "", []string{"terminal"}, 200)
	if err := os.Rename(path, path+".old"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("a replaced log"), 0600); err != nil {
		t.Fatal(err)
	}
	requestSessionLog(t, h, "?offset=1&generation="+short.Generation, []string{"terminal"}, 409)
	for _, bad := range []string{"?offset=-1", "?offset=9223372036854775808", "?limit=0", "?limit=65537", "?generation=x"} {
		requestSessionLog(t, h, bad, []string{"terminal"}, 400)
	}
}

func TestSessionLogTailBoundAndPermission(t *testing.T) {
	data := bytes.Repeat([]byte("a"), MaxSessionLogBytes+10)
	h, _ := sessionLogFixture(t, data)
	requestSessionLog(t, h, "", []string{"read"}, 403)
	requestSessionLog(t, h, "", []string{"admin"}, 403)
	result := requestSessionLog(t, h, "", []string{"terminal"}, 200)
	if len(result.Data) != MaxSessionLogBytes || result.Offset != 10 || result.NextOffset != int64(len(data)) {
		t.Fatalf("unbounded/incorrect tail: %+v", result)
	}
	for _, route := range []struct{ method, path string }{{"GET", "/logs/daemon"}, {"GET", "/fs/detect"}, {"POST", "/fs/validate"}} {
		r := httptest.NewRequest(route.method, route.path, nil)
		scope, _, ok := RequiredRemoteScope(r)
		if !ok || scope != "maintain" {
			t.Fatalf("diagnostic scope changed: %s %s %s", route.method, route.path, scope)
		}
	}
}

func TestSessionLogRefusesSymlinkEscapeAndSanitizesErrors(t *testing.T) {
	h, path := sessionLogFixture(t, []byte("safe"))
	outside := filepath.Join(t.TempDir(), "private-output")
	if err := os.WriteFile(outside, []byte("must never be returned"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, path); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, remotePathRequest("GET", "/sessions/s/log", ""))
	if w.Code != 409 || bytes.Contains(w.Body.Bytes(), []byte(outside)) || bytes.Contains(w.Body.Bytes(), []byte("must never")) {
		t.Fatalf("escaped or exposed host path: %s", w.Body.String())
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	requestSessionLog(t, h, "", []string{"terminal"}, 404)
	canceled := remotePathRequest("GET", "/sessions/s/log", "")
	ctx, cancel := context.WithCancel(canceled.Context())
	cancel()
	w = httptest.NewRecorder()
	h.ServeHTTP(w, canceled.WithContext(ctx))
	if w.Body.Len() != 0 {
		t.Fatalf("canceled request produced log output: %s", w.Body.String())
	}
}
