package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hollis-labs/tether/internal/agencyregistry"
	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/launch"
	"github.com/hollis-labs/tether/internal/runtoken"
	"github.com/hollis-labs/tether/internal/store"
	"github.com/hollis-labs/tether/internal/workerwire"
	"go.opentelemetry.io/otel/trace"
)

func TestLaunchRegistersCorrelationAsynchronouslyWithStableRetry(t *testing.T) {
	svc, rt, id, _ := credentialLaunch(t)
	release := make(chan struct{})
	entered := make(chan struct{}, 1)
	got := make(chan agencyregistry.Facts, 1)
	var mu sync.Mutex
	var requests []string
	var keys []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var raw json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
			t.Error(err)
			return
		}
		mu.Lock()
		requests = append(requests, string(raw))
		keys = append(keys, r.Header.Get("Idempotency-Key"))
		n := len(requests)
		mu.Unlock()
		if n == 1 {
			entered <- struct{}{}
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
			w.WriteHeader(503)
			return
		}
		var f agencyregistry.Facts
		_ = json.Unmarshal(raw, &f)
		got <- f
		_, _ = w.Write([]byte(`{"id":"correlation:01M7VQ2QWYB5RZR86W2BWG8PW3","created":true,"version_token":"test"}`))
	}))
	defer srv.Close()
	t.Cleanup(svc.stopLaunchCorrelations)
	path := filepath.Join(t.TempDir(), "registry-credential")
	if err := os.WriteFile(path, []byte("fake-registry-test-credential"), 0600); err != nil {
		t.Fatal(err)
	}
	svc.Catalog.Global.AgencyRegistry = agencyregistry.Config{URL: srv.URL, CredentialFile: path}
	ctx := identity.WithPrincipal(context.Background(), identity.Principal{Verified: true, ID: identity.OperatorID, Kind: "operator"})
	sc := trace.NewSpanContext(trace.SpanContextConfig{TraceID: trace.TraceID{1}, SpanID: trace.SpanID{2}, TraceFlags: trace.FlagsSampled})
	ctx = trace.ContextWithSpanContext(ctx, sc)
	done := make(chan error, 1)
	const workstream = "workstream:01M7VQ2QWYB5RZR86W2BWG8PW3"
	go func() {
		_, err := svc.LaunchSessionWithCorrelation(ctx, id, api.SessionLaunchRequest{TaskID: "CW-20261121-0030", WorkstreamID: workstream})
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("launch waited on Registry")
	}
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("no Registry request")
	}
	close(release)
	var f agencyregistry.Facts
	select {
	case f = <-got:
	case <-time.After(5 * time.Second):
		t.Fatal("no retry")
	}
	if f.Session != id || f.URN != "msg://agent/agent-mux/agent" || f.TaskID != "CW-20261121-0030" || f.WorkstreamID != workstream || f.TokenJTI == "" || f.Traceparent != "00-"+sc.TraceID().String()+"-"+sc.SpanID().String()+"-01" {
		t.Fatal("missing launch join facts")
	}
	env := map[string]string{}
	for _, v := range rt.options.Env {
		k, v, _ := strings.Cut(v, "=")
		env[k] = v
	}
	if f.RunID != env[runtoken.IDEnv] {
		t.Fatal("wrong execution run")
	}
	p, err := identity.NewStore(svc.Store.DB()).Verify(ctx, env["TETHER_TOKEN"])
	if err != nil {
		t.Fatal(err)
	}
	claims, err := svc.VerifyRunToken(testRunProof(t, env), p)
	if err != nil || f.TokenJTI != claims.JTI {
		t.Fatal("wrong JTI")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(requests) != 2 || requests[0] != requests[1] || keys[0] != keys[1] || keys[0] == "" {
		t.Fatal("retry changed identity or facts")
	}
	if strings.Contains(requests[0], testRunProof(t, env)) || strings.Contains(requests[0], env["TETHER_TOKEN"]) {
		t.Fatal("token leaked into facts")
	}
	if _, err := svc.Store.GetWorkstream(workstream); err != nil {
		t.Fatal("no local Registry reference", err)
	}
}

func TestCorrelationCapacityAndShutdownAreBounded(t *testing.T) {
	svc, _, id, _ := credentialLaunch(t)
	svc.Catalog.Global.AgencyRegistry = agencyregistry.Config{URL: "http://unused.invalid", CredentialFile: "/missing"}
	svc.correlationSlots = make(chan struct{}, 32)
	svc.correlationContext, svc.correlationCancel = context.WithCancel(context.Background())
	for i := 0; i < 32; i++ {
		svc.correlationSlots <- struct{}{}
	}
	row, err := svc.Store.GetSession(id)
	if err != nil {
		t.Fatal(err)
	}
	row.TaskID.String = "task"
	start := time.Now()
	svc.queueLaunchCorrelation(context.Background(), row, map[string]string{runtoken.IDEnv: "run"}, "", "")
	if time.Since(start) > time.Second {
		t.Fatal("full queue delayed caller")
	}
	svc.stopLaunchCorrelations()
	svc.queueLaunchCorrelation(context.Background(), row, map[string]string{runtoken.IDEnv: "run"}, "", "")
}

func TestWorkerLaunchRegistersKnownExecutionFacts(t *testing.T) {
	r := newIdentityRig(t, workerwire.FeatureSessionCred, workerwire.FeatureExecutionRunToken)
	got := make(chan agencyregistry.Facts, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var f agencyregistry.Facts
		if err := json.NewDecoder(req.Body).Decode(&f); err != nil {
			t.Error(err)
			return
		}
		got <- f
		w.WriteHeader(200)
	}))
	defer srv.Close()
	t.Cleanup(r.svc.stopLaunchCorrelations)
	path := filepath.Join(t.TempDir(), "credential")
	if err := os.WriteFile(path, []byte("fake-test-credential"), 0600); err != nil {
		t.Fatal(err)
	}
	r.svc.Catalog = &config.Catalog{Global: config.Global{AgencyRegistry: agencyregistry.Config{URL: srv.URL, CredentialFile: path}}}
	p := &launch.Plan{LaunchID: "test", LogicalAgentID: "agent-worker", ProviderID: "claude", ProviderBrand: "claude"}
	row := store.SessionRow{ID: "worker-correlation", LogicalAgentID: p.LogicalAgentID, WorkerID: r.worker, ProviderID: "claude", Workspace: t.TempDir(), State: "created", TaskID: sql.NullString{Valid: true, String: "task"}, RunID: sql.NullString{Valid: true, String: "torque-run"}, WorkstreamID: sql.NullString{Valid: true, String: "workstream:01M7VQ2QWYB5RZR86W2BWG8PW3"}}
	if err := r.db.CreateSession(row, p); err != nil {
		t.Fatal(err)
	}
	if err := r.db.UpsertSessionProviderMapping(row.ID, "tether", "claude", "native-known"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.svc.launchWorkerSession(context.Background(), &row, p); err != nil {
		t.Fatal(err)
	}
	spec := <-r.specs
	select {
	case f := <-got:
		if f.RunID != spec.Env[runtoken.IDEnv] || f.RunID == row.RunID.String || f.TokenJTI == "" || f.ProviderSessionID != "native-known" || f.WorkstreamID != row.WorkstreamID.String {
			t.Fatal("worker join facts diverged")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("worker correlation absent")
	}
}

func TestCorrelationRetryBudgetAndCancellation(t *testing.T) {
	for _, status := range []int{403, 503} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			svc, _, id, _ := credentialLaunch(t)
			var mu sync.Mutex
			count := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { mu.Lock(); count++; mu.Unlock(); w.WriteHeader(status) }))
			defer srv.Close()
			t.Cleanup(svc.stopLaunchCorrelations)
			path := filepath.Join(t.TempDir(), "credential")
			if err := os.WriteFile(path, []byte("fake-test-credential"), 0600); err != nil {
				t.Fatal(err)
			}
			svc.Catalog.Global.AgencyRegistry = agencyregistry.Config{URL: srv.URL, CredentialFile: path}
			row, _ := svc.Store.GetSession(id)
			row.TaskID.String = "task"
			svc.queueLaunchCorrelation(context.Background(), row, map[string]string{runtoken.IDEnv: "run"}, "", "")
			done := make(chan struct{})
			go func() { svc.correlationWG.Wait(); close(done) }()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("retry budget unbounded")
			}
			mu.Lock()
			defer mu.Unlock()
			want := 1
			if status == 503 {
				want = 3
			}
			if count != want {
				t.Fatalf("requests=%d want=%d", count, want)
			}
		})
	}
}

func TestLaunchManifestDigest(t *testing.T) {
	root := t.TempDir()
	if got := launchManifestDigest(root); got != "" {
		t.Fatal("missing manifest invented a digest")
	}
	if err := os.Mkdir(filepath.Join(root, ".materialize"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".materialize/manifest.json"), []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	if got := launchManifestDigest(root); got != "sha256:44136fa355b3678a1146ad16f7e8649e94fb4fc21fe77e8310c060f61caaff8a" {
		t.Fatal("wrong committed manifest digest", got)
	}
}

func TestHungRegistryIsCanceledAtShutdown(t *testing.T) {
	svc, _, id, _ := credentialLaunch(t)
	entered := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		entered <- struct{}{}
		<-r.Context().Done()
	}))
	defer srv.Close()
	t.Cleanup(svc.stopLaunchCorrelations)
	path := filepath.Join(t.TempDir(), "credential")
	if err := os.WriteFile(path, []byte("fake-test-credential"), 0600); err != nil {
		t.Fatal(err)
	}
	svc.Catalog.Global.AgencyRegistry = agencyregistry.Config{URL: srv.URL, CredentialFile: path}
	row, _ := svc.Store.GetSession(id)
	row.TaskID.String = "task"
	svc.queueLaunchCorrelation(context.Background(), row, map[string]string{runtoken.IDEnv: "run"}, "", "")
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("no request")
	}
	done := make(chan struct{})
	go func() { svc.stopLaunchCorrelations(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("shutdown waited for hung Registry")
	}
}
