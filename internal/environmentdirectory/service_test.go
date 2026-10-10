package environmentdirectory_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	tether "github.com/hollis-labs/substrate/mesh/tetherclient"
	directory "github.com/hollis-labs/tether/internal/environmentdirectory"
	"github.com/hollis-labs/tether/internal/store"
)

func TestModePrecedence(t *testing.T) {
	for _, tc := range []struct{ instance, agent, launch, want directory.Mode }{
		{"", "", "", directory.Independent},
		{directory.HubManaged, "", "", directory.HubManaged},
		{directory.HubManaged, directory.Independent, "", directory.Independent},
		{directory.Independent, directory.HubManaged, directory.Independent, directory.Independent},
	} {
		got, err := directory.ResolveMode(tc.instance, tc.agent, tc.launch)
		if err != nil || got != tc.want {
			t.Fatalf("mode got%q err%v want%q", got, err, tc.want)
		}
	}
	if _, err := directory.ResolveMode("unknown", directory.Independent, directory.HubManaged); !errors.Is(err, directory.ErrInvalid) {
		t.Fatal("invalid declarations must refuse")
	}
}

func testWorker(t *testing.T, id string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == tether.EnvironmentDescriptorPath {
			if r.Header.Get("Authorization") != "" {
				t.Error("credential on public descriptor")
			}
			_ = json.NewEncoder(w).Encode(tether.EnvironmentDescriptor{EnvironmentID: id, Protocol: 1, ServerVersion: "synthetic", Capabilities: map[string]map[string]any{"stream": {"enabled": true}}})
		} else if r.URL.Path == "/auth/context" && r.Header.Get("Authorization") == "Bearer synthetic-test" {
			_, _ = w.Write([]byte(`{}`))
		} else {
			w.WriteHeader(403)
		}
	}))
}

func registration(id, authority, route string) directory.Registration {
	return directory.Registration{EnvironmentTarget: tether.EnvironmentTarget{EnvironmentID: id, Authority: authority, Routes: []tether.EnvironmentRoute{{BaseURL: route}}, CredentialReference: "file:///synthetic-private-reference"}, DeviceID: "synthetic-device", Ownership: "external"}
}

func TestRegisterIdentityBeforeCredentialAndImmutableBinding(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	expected := uuid.NewString()
	worker := testWorker(t, uuid.NewString())
	defer worker.Close()
	var resolutions atomic.Int32
	opts := directory.Options{Client: tether.EnvironmentOptions{ResolveCredential: func(context.Context, string) (string, error) { resolutions.Add(1); return "synthetic-test", nil }}}
	svc := directory.New(db, opts)
	in := registration(expected, "worker-one", worker.URL)
	_, err = svc.Register(context.Background(), in)
	var mismatch *tether.EnvironmentIdentityError
	if !errors.As(err, &mismatch) || resolutions.Load() != 0 {
		t.Fatalf("identity err%v resolutions%d", err, resolutions.Load())
	}
	good := testWorker(t, expected)
	defer good.Close()
	in.Routes[0].BaseURL = good.URL
	record, err := svc.Register(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if record.ManagementMode != directory.Independent || record.LastSeen == nil || record.Capabilities["stream"]["enabled"] != true {
		t.Fatalf("bad record%+v", record)
	}
	in.ManagementMode = directory.HubManaged
	before := resolutions.Load()
	if _, err = svc.Register(context.Background(), in); !errors.Is(err, directory.ErrConflict) || resolutions.Load() != before {
		t.Fatal("runtime mode change was not refused before credentials")
	}
}

func TestRetirePendingAndNoMutationReplay(t *testing.T) {
	for _, revokeOK := range []bool{false, true} {
		t.Run(map[bool]string{false: "refused", true: "authorized"}[revokeOK], func(t *testing.T) {
			db, err := store.Open(filepath.Join(t.TempDir(), "db"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			id := uuid.NewString()
			worker := testWorker(t, id)
			defer worker.Close()
			calls := 0
			svc := directory.New(db, directory.Options{Client: tether.EnvironmentOptions{ResolveCredential: func(context.Context, string) (string, error) { return "synthetic-test", nil }}, Revoke: func(_ context.Context, r directory.Record) error {
				calls++
				stored, e := db.GetEnvironment(context.Background(), id)
				if e != nil || stored.State != "retired" || !stored.RevocationPending {
					t.Error("revoke before committed tombstone")
				}
				if r.DeviceID != "synthetic-device" {
					t.Error("lost exact device")
				}
				if !revokeOK {
					return errors.New("no authorized admin")
				}
				return nil
			}})
			if _, err = svc.Register(context.Background(), registration(id, "worker-one", worker.URL)); err != nil {
				t.Fatal(err)
			}
			r, err := svc.Retire(context.Background(), id)
			if err != nil {
				t.Fatal(err)
			}
			if r.State != "retired" || r.RevocationPending == revokeOK {
				t.Fatalf("bad retirement%+v", r)
			}
			if _, err = svc.Retire(context.Background(), id); err != nil || calls != 1 {
				t.Fatalf("retire replay calls%d err%v", calls, err)
			}
			if _, err = svc.Observe(context.Background(), id, "unreachable", nil); !errors.Is(err, directory.ErrRetired) {
				t.Fatal("observation revived tombstone")
			}
		})
	}
}

func TestDeclarationRefusesInferredBindings(t *testing.T) {
	base := registration(uuid.NewString(), "worker", "https://worker.example")
	for _, tc := range []struct {
		name   string
		change func(*directory.Registration)
	}{
		{"missing device", func(r *directory.Registration) { r.DeviceID = "" }},
		{"missing ownership", func(r *directory.Registration) { r.Ownership = "" }},
		{"missing routes", func(r *directory.Registration) { r.Routes = nil }},
		{"ambient reference", func(r *directory.Registration) { r.CredentialReference = "env:TETHER_TOKEN" }},
		{"route userinfo", func(r *directory.Registration) {
			r.Routes = []tether.EnvironmentRoute{{BaseURL: "https://user:synthetic@worker.example"}}
		}},
		{"unknown mode", func(r *directory.Registration) { r.ManagementMode = "UNKNOWN" }},
		{"home move", func(r *directory.Registration) { r.Homes = []directory.Home{{URN: "msg://agent/other/agent"}} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := base
			tc.change(&in)
			if _, err := directory.Normalize(in); !errors.Is(err, directory.ErrInvalid) {
				t.Fatal("accepted invalid binding", err)
			}
		})
	}
}
