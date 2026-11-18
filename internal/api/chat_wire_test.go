package api

import (
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

type recorded struct {
	status int
	header http.Header
	body   string
}

func serve(h http.Handler, method, path string) recorded {
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(method, path, strings.NewReader("{}")))
	res := rr.Result()
	b, _ := io.ReadAll(res.Body)
	return recorded{res.StatusCode, res.Header, string(b)}
}

var chatWirePaths = []struct{ method, path string }{
	{http.MethodPost, "/chat/v1/agents/seat/conversation"},
	{http.MethodGet, "/chat/v1/conversations/c1"},
	{http.MethodGet, "/chat/v1/conversations/c1/events?attachment=a&after=0"},
	{http.MethodPost, "/chat/v1/conversations/c1/turns"},
	{http.MethodGet, "/chat/v1/"},
	{http.MethodGet, "/chat/v1"},
}

// With the flag off (no Deps.ChatWire) the wire's paths are not routes: every answer is byte for byte the answer to
// a path nothing registers, and neither scope table knows them.
func TestChatWireOffChangesNothing(t *testing.T) {
	h := NewHandler(Deps{Service: &fakeLaunchService{}, Catalog: chatCatalog()})
	for _, p := range chatWirePaths {
		got := serve(h, p.method, p.path)
		want := serve(h, p.method, strings.Replace(p.path, "/chat/v1", "/unregistered/v1", 1))
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s %s = %+v, want the unregistered answer %+v", p.method, p.path, got, want)
		}
		r := httptest.NewRequest(p.method, p.path, nil)
		if _, _, known := RequiredRemoteScope(r); known {
			t.Errorf("%s %s has a remote scope with the wire off", p.method, p.path)
		}
		if _, _, known := RequiredLocalScope(r); known {
			t.Errorf("%s %s has a local scope with the wire off", p.method, p.path)
		}
	}
	// The existing chat roster is untouched.
	if got := serve(h, http.MethodGet, "/chat/agents"); got.status != http.StatusOK {
		t.Errorf("GET /chat/agents = %d", got.status)
	}
}

// With the flag on the handler is mounted at /chat/v1 with the prefix stripped, beside the roster.
func TestChatWireOnMountsHandler(t *testing.T) {
	var seen []string
	wire := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Method+" "+r.URL.Path)
		w.WriteHeader(http.StatusTeapot)
	})
	h := NewHandler(Deps{Service: &fakeLaunchService{}, Catalog: chatCatalog(), ChatWire: wire})
	if got := serve(h, http.MethodPost, "/chat/v1/agents/seat/conversation"); got.status != http.StatusTeapot {
		t.Fatalf("wire route = %d", got.status)
	}
	if got := serve(h, http.MethodGet, "/chat/agents"); got.status != http.StatusOK {
		t.Errorf("GET /chat/agents = %d", got.status)
	}
	if !reflect.DeepEqual(seen, []string{"POST /agents/seat/conversation"}) {
		t.Errorf("wire saw %q", seen)
	}
	// Every wire route has a policy in the table the daemon enables with the flag.
	for _, p := range chatWirePaths[:4] {
		r := httptest.NewRequest(p.method, p.path, nil)
		rule := scopeRule(ChatWireRouteScopes, r)
		if rule == nil || rule.Methods[p.method] == "" {
			t.Errorf("%s %s has no chat wire scope", p.method, p.path)
		}
	}
}
