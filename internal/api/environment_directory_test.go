package api

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	tether "github.com/hollis-labs/substrate/mesh/tetherclient"
	directory "github.com/hollis-labs/tether/internal/environmentdirectory"
	"github.com/hollis-labs/tether/internal/identity"
)

type directoryStub struct{ calls int }

func (d *directoryStub) List(context.Context) ([]directory.Record, error) {
	return []directory.Record{{Registration: directory.Registration{EnvironmentTarget: tether.EnvironmentTarget{CredentialReference: "file:///private-reference"}, Label: "worker"}, State: "reachable"}}, nil
}
func (d *directoryStub) Get(context.Context, string) (directory.Record, error) {
	return directory.Record{Registration: directory.Registration{EnvironmentTarget: tether.EnvironmentTarget{CredentialReference: "file:///private-reference"}}}, nil
}
func (d *directoryStub) Register(_ context.Context, in directory.Registration) (directory.Record, error) {
	d.calls++
	return directory.Record{Registration: in}, nil
}
func (d *directoryStub) Rename(_ context.Context, id, label string) (directory.Record, error) {
	d.calls++
	return directory.Record{Registration: directory.Registration{Label: label}}, nil
}
func (d *directoryStub) Retire(context.Context, string) (directory.Record, error) {
	d.calls++
	return directory.Record{State: "retired", RevocationPending: true}, nil
}

func TestDirectoryHTTPPrivateProjectionAndOperatorBoundary(t *testing.T) {
	dep := &directoryStub{}
	h := NewHandler(Deps{Directory: dep})
	for _, path := range []string{"/environments", "/environments/id"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != 200 || strings.Contains(w.Body.String(), "private-reference") {
			t.Fatalf("private projection %d %s", w.Code, w.Body.String())
		}
	}
	for _, tc := range []struct {
		name      string
		principal identity.Principal
		local     bool
	}{
		{"anonymous", identity.Principal{}, true},
		{"session", identity.Principal{ID: "session", Kind: "session", Scopes: []string{"*"}}, true},
		{"device-admin", identity.Principal{ID: "device", Kind: "device", Scopes: []string{"admin"}}, true},
		{"operator-tcp", identity.Principal{ID: identity.OperatorID, Kind: "operator"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("DELETE", "/environments/id", nil)
			ctx := identity.WithPrincipal(r.Context(), tc.principal)
			if tc.local {
				ctx = identity.ConnectionContext(ctx, &net.UnixConn{})
			}
			r.Header.Set("X-Forwarded-Proto", "unix")
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r.WithContext(ctx))
			if w.Code != 403 || dep.calls != 0 {
				t.Fatalf("invalid operator boundary status%d calls%d", w.Code, dep.calls)
			}
		})
	}
	r := httptest.NewRequest("DELETE", "/environments/id", nil)
	ctx := identity.WithPrincipal(identity.ConnectionContext(r.Context(), &net.UnixConn{}), identity.Principal{ID: identity.OperatorID, Kind: "operator"})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r.WithContext(ctx))
	if w.Code != 200 || dep.calls != 1 || !strings.Contains(w.Body.String(), `"revocationPending":true`) {
		t.Fatal("missing honest retirement", w.Code, w.Body.String())
	}
}

func TestDirectoryHTTPPatchCannotRetargetAuthority(t *testing.T) {
	dep := &directoryStub{}
	h := NewHandler(Deps{Directory: dep})
	r := httptest.NewRequest(http.MethodPatch, "/environments/id", strings.NewReader(`{"label":"label","authority":"new"}`))
	ctx := identity.WithPrincipal(identity.ConnectionContext(r.Context(), &net.UnixConn{}), identity.Principal{ID: identity.OperatorID, Kind: "operator"})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r.WithContext(ctx))
	if w.Code != 400 || dep.calls != 0 {
		t.Fatal("authority changed through rename", w.Code)
	}
}

func TestDirectoryRemoteScopeExactPolicy(t *testing.T) {
	for _, tc := range []struct{ method, path, want string }{
		{"GET", "/environments", readScope}, {"POST", "/environments", localScope},
		{"GET", "/environments/id", readScope}, {"PATCH", "/environments/id", localScope}, {"DELETE", "/environments/id", localScope},
	} {
		scope, _, ok := RequiredRemoteScope(httptest.NewRequest(tc.method, tc.path, nil))
		if !ok || scope != tc.want {
			t.Fatalf("scope%s %s=%s known%v", tc.method, tc.path, scope, ok)
		}
	}
	if _, _, known := RequiredRemoteScope(httptest.NewRequest("POST", "/environments/id/launch", nil)); known {
		t.Fatal("invented management path classified")
	}
}
