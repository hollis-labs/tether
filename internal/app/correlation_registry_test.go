package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/correlationregistry"
	"github.com/hollis-labs/tether/internal/launch"
	"github.com/hollis-labs/tether/internal/runtoken"
	"github.com/hollis-labs/tether/internal/store"
)

func TestLaunchCorrelationDoesNotWaitForRegistry(t *testing.T) {
	svc, _, id, _ := credentialLaunch(t)
	requests := make(chan correlationregistry.Payload, 1)
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b struct {
			Record struct{ Payload correlationregistry.Payload }
		}
		if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
			t.Error(err)
		}
		requests <- b.Record.Payload
		<-release
		w.WriteHeader(503)
	}))
	defer server.Close()
	defer close(release)
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte("test-credential"), 0600); err != nil {
		t.Fatal(err)
	}
	svc.Catalog.Global.Daemon.CorrelationRegistry = correlationregistry.Config{URL: server.URL, TokenFile: tokenFile}
	row := store.SessionRow{ID: id, LogicalAgentID: "one", ProviderID: "stub", RunID: sql.NullString{String: "dispatch-42", Valid: true}, TaskID: sql.NullString{String: "task-42", Valid: true}}
	if err := svc.Store.UpsertSessionProviderMapping(id, "tether", "stub", "native-42"); err != nil {
		t.Fatal(err)
	}
	env, err := svc.mintRunEnvironmentFor(context.Background(), &row, true)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { svc.registerLaunchCorrelation(row, env); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("registration blocked caller")
	}
	select {
	case p := <-requests:
		if p.Metadata.RunID != env[runtoken.IDEnv] || p.Metadata.JTI == "" || p.Metadata.URN != runURN(&row) || p.Metadata.DispatchRunID != "dispatch-42" || p.Metadata.TaskID != "task-42" || p.Metadata.ProviderSessionID != "native-42" {
			t.Fatalf("lost launcher claims: %+v", p)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no registration")
	}
	// A saturated Registry queue drops the next launch observation immediately.
	svc.correlationMu.Lock()
	slots := svc.correlationSlots
	svc.correlationMu.Unlock()
	for len(slots) < cap(slots) {
		slots <- struct{}{}
	}
	done = make(chan struct{})
	go func() { svc.registerLaunchCorrelation(row, env); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("capacity blocked caller")
	}
}

func TestUnconfiguredLaunchCorrelationDoesNotReadStore(t *testing.T) {
	(&Service{Catalog: &config.Catalog{}}).registerLaunchCorrelation(store.SessionRow{}, nil)
}

func TestSuccessfulLocalLaunchRegistersCorrelation(t *testing.T) {
	svc, _, id, _ := credentialLaunch(t)
	observed := make(chan correlationregistry.Payload, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b struct {
			Record struct{ Payload correlationregistry.Payload }
		}
		if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
			t.Error(err)
		}
		observed <- b.Record.Payload
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	file := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(file, []byte("test-credential"), 0600); err != nil {
		t.Fatal(err)
	}
	svc.Catalog.Global.Daemon.CorrelationRegistry = correlationregistry.Config{URL: server.URL, TokenFile: file}
	if _, err := svc.LaunchSession(id); err != nil {
		t.Fatal(err)
	}
	select {
	case p := <-observed:
		if p.Metadata.Session != id || p.Metadata.RunID == "" {
			t.Fatalf("incorrect launch observation %+v", p)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("successful local start did not register")
	}
}

func TestSuccessfulWorkerLaunchRegistersCorrelation(t *testing.T) {
	rig := newIdentityRig(t)
	observed := make(chan correlationregistry.Payload, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b struct {
			Record struct{ Payload correlationregistry.Payload }
		}
		if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
			t.Error(err)
		}
		observed <- b.Record.Payload
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	file := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(file, []byte("test-credential"), 0600); err != nil {
		t.Fatal(err)
	}
	rig.svc.Catalog.Global.Daemon.CorrelationRegistry = correlationregistry.Config{URL: server.URL, TokenFile: file}
	plan := &launch.Plan{ProviderBrand: "claude", Env: map[string]string{}}
	row := store.SessionRow{ID: "registry-worker", WorkerID: rig.worker, State: "created", Workspace: t.TempDir()}
	if err := rig.db.CreateSession(row, plan); err != nil {
		t.Fatal(err)
	}
	if _, err := rig.svc.launchWorkerSession(context.Background(), &row, plan); err != nil {
		t.Fatal(err)
	}
	select {
	case p := <-observed:
		if p.Metadata.Session != row.ID || p.Metadata.RunID == "" {
			t.Fatalf("incorrect worker observation %+v", p)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("successful worker start did not register")
	}
}

func TestFailedLocalLaunchDoesNotRegisterCorrelation(t *testing.T) {
	svc, rt, id, _ := credentialLaunch(t)
	rt.fail = true
	svc.Catalog.Global.Daemon.CorrelationRegistry = correlationregistry.Config{URL: "http://127.0.0.1:1"}
	if _, err := svc.LaunchSession(id); err == nil {
		t.Fatal("launch succeeded")
	}
	svc.correlationMu.Lock()
	defer svc.correlationMu.Unlock()
	if svc.correlationSlots != nil {
		t.Fatal("failed launch scheduled an observation")
	}
}
