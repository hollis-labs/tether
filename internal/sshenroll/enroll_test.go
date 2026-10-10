package sshenroll

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/daemon"
	"github.com/hollis-labs/tether/internal/environment"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/store"
)

func testRelease(t *testing.T, arch string) (string, string, []byte) {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	binary := []byte("#!/bin/sh\nexit 0\n")
	if err := tw.WriteHeader(&tar.Header{Name: "tether", Mode: 0700, Size: int64(len(binary)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	_, _ = tw.Write(binary)
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	name := "tether_0.8.0_linux_" + arch + ".tar.gz"
	root := t.TempDir()
	path := filepath.Join(root, name)
	if err := os.WriteFile(path, buf.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(buf.Bytes())
	checksums := filepath.Join(root, "checksums.txt")
	if err := os.WriteFile(checksums, []byte(hex.EncodeToString(digest[:])+"  "+name+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return path, checksums, buf.Bytes()
}

type fixtureTunnel struct {
	base   string
	closed *int
}

func (t fixtureTunnel) BaseURL() string { return t.base }
func (t fixtureTunnel) Close() error    { *t.closed++; return nil }

type fixtureRemote struct {
	preflight                                     Preflight
	ids                                           *identity.Store
	server                                        *httptest.Server
	environmentID                                 string
	installs, grants, inspects, rollbacks, closed int
	failInstall                                   bool
	exchanges                                     atomic.Int32
	discardExchange                               atomic.Bool
	mismatch                                      atomic.Bool
	scope                                         []string
}

func (f *fixtureRemote) Preflight(context.Context, Options) (Preflight, error) {
	return f.preflight, nil
}
func (f *fixtureRemote) state(r WorkerRequest) WorkerState {
	return WorkerState{OperationID: r.OperationID, EnvironmentID: f.environmentID, Authority: r.Authority, Version: r.Version, RemotePort: r.RemotePort, Managed: true, Phase: "running"}
}
func (f *fixtureRemote) Install(_ context.Context, r WorkerRequest, a *Artifact) (WorkerState, error) {
	f.installs++
	if f.failInstall {
		return WorkerState{}, problem("service", "fixture-failure", "Retain the partial state.")
	}
	reader, err := a.Reader()
	if err != nil {
		return WorkerState{}, err
	}
	b, err := io.ReadAll(reader)
	digest := sha256.Sum256(b)
	if err != nil || hex.EncodeToString(digest[:]) != r.ArchiveSHA256 {
		return WorkerState{}, errors.New("fixture upload differs")
	}
	return f.state(r), nil
}
func (f *fixtureRemote) Inspect(_ context.Context, r WorkerRequest) (WorkerState, error) {
	f.inspects++
	return f.state(r), nil
}
func (f *fixtureRemote) Grant(ctx context.Context, r WorkerRequest) (identity.IssuedGrant, error) {
	f.grants++
	f.scope = r.Scopes
	return f.ids.CreatePairingGrant(ctx, identity.Principal{ID: identity.OperatorID, Kind: "operator"}, "enrollment-"+r.OperationID, r.Scopes, time.Minute, "")
}
func (f *fixtureRemote) Forward(context.Context, int) (Tunnel, error) {
	return fixtureTunnel{f.server.URL, &f.closed}, nil
}
func (f *fixtureRemote) Rollback(context.Context, WorkerRequest) error { f.rollbacks++; return nil }

func enrollmentFixture(t *testing.T) (*Manager, Options, *fixtureRemote) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	db, err := store.Open(filepath.Join(t.TempDir(), "worker.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	f := &fixtureRemote{ids: identity.NewStore(db.DB()), environmentID: uuid.NewString()}
	descriptor, err := environment.NewDescriptor(environment.Descriptor{EnvironmentID: f.environmentID, Label: "worker", ServerVersion: "0.8.0"})
	if err != nil {
		t.Fatal(err)
	}
	daemonServer := &daemon.Server{Config: daemon.Config{IdentityMode: identity.Off}, Identity: f.ids, Environment: descriptor, Catalog: enrollmentCatalog{}}
	t.Cleanup(daemonServer.CloseIdentityAudit)
	f.server = httptest.NewUnstartedServer(nil)
	handler := daemonServer.RemoteHandler(f.server.Listener.Addr().String())
	f.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if f.mismatch.Load() && r.URL.Path == environment.DescriptorPath {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"environmentId":"00000000-0000-4000-8000-000000000000","protocol":1}`))
			return
		}
		if r.URL.Path == "/auth/pair/exchange" {
			f.exchanges.Add(1)
			if f.discardExchange.Load() {
				record := httptest.NewRecorder()
				handler.ServeHTTP(record, r)
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
		}
		handler.ServeHTTP(w, r)
	})
	f.server.Start()
	t.Cleanup(f.server.Close)
	f.preflight = Preflight{OS: "Linux", Arch: runtime.GOARCH, Home: t.TempDir(), Path: "/usr/bin", Git: true, Bubblewrap: true, UserManager: true, Linger: true, Writable: true, Providers: map[string]string{"codex": "/usr/bin/codex"}}
	archive, checksums, _ := testRelease(t, runtime.GOARCH)
	port := f.server.Listener.Addr().(*net.TCPAddr).Port
	o := Options{Target: "worker", Authority: "worker", Version: "0.8.0", Archive: archive, Checksums: checksums, ReceiptDir: filepath.Join(t.TempDir(), "receipt"), Providers: []string{"codex"}, Scopes: []string{"read", "operate"}, RemotePort: port, Timeout: time.Minute}
	return &Manager{Remote: f}, o, f
}

func TestEnrollmentActualDaemonPairingAndIdempotentRetry(t *testing.T) {
	m, o, f := enrollmentFixture(t)
	r, err := m.Add(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if r.Phase != "complete" || r.EnvironmentID != f.environmentID || r.DeviceID == "" || r.CredentialReference == "" || f.grants != 1 || f.exchanges.Load() != 1 {
		t.Fatal("enrollment did not complete exactly once")
	}
	token, err := identity.ReadTokenFile(filepath.Join(o.ReceiptDir, "device.token"))
	if err != nil {
		t.Fatal("private device file", err)
	}
	data, err := os.ReadFile(filepath.Join(o.ReceiptDir, "receipt.json"))
	if err != nil || bytes.Contains(data, []byte(token)) || bytes.Contains(data, []byte("tpg_")) {
		t.Fatal("secret entered receipt")
	}
	r2, err := m.Add(context.Background(), o)
	if err != nil || r2.DeviceID != r.DeviceID || r2.OperationID != r.OperationID || f.installs != 1 || f.grants != 1 || f.exchanges.Load() != 1 || f.closed != 2 {
		t.Fatal("retry replayed installation or pairing", err)
	}
	if err := f.ids.RevokeDevice(context.Background(), r.DeviceID); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Add(context.Background(), o); err == nil {
		t.Fatal("revoked token reused or silently replaced")
	}
	if f.grants != 1 {
		t.Fatal("revocation caused replacement authority")
	}
}
func TestEnrollmentUncertainExchangeIsRetainedAndNeverReplayed(t *testing.T) {
	m, o, f := enrollmentFixture(t)
	f.discardExchange.Store(true)
	r, err := m.Add(context.Background(), o)
	if err == nil || r.Phase != "exchange-uncertain" {
		t.Fatal("uncertain exchange was reported complete")
	}
	devices, err := f.ids.ListDevices(context.Background())
	if err != nil || len(devices) != 1 {
		t.Fatal("fixture did not actually mint before losing response")
	}
	_, err = m.Add(context.Background(), o)
	var p *Problem
	if !errors.As(err, &p) || p.Code != "outcome-unknown" || f.exchanges.Load() != 1 || f.grants != 1 {
		t.Fatal("uncertain mutation replayed", err)
	}
}
func TestEnrollmentPreflightStopsBeforeWorkerOrHubChanges(t *testing.T) {
	m, o, f := enrollmentFixture(t)
	f.preflight.Linger = false
	if _, err := m.Add(context.Background(), o); err == nil {
		t.Fatal("missing linger accepted")
	}
	if f.installs != 0 || f.grants != 0 {
		t.Fatal("preflight changed worker")
	}
	if _, err := os.Lstat(o.ReceiptDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("preflight created hub receipt")
	}
}
func TestEnrollmentPartialInstallRetryPreservesOperation(t *testing.T) {
	m, o, f := enrollmentFixture(t)
	f.failInstall = true
	r, err := m.Add(context.Background(), o)
	if err == nil || r.Phase != "installing" {
		t.Fatal("partial install lost")
	}
	f.failInstall = false
	r2, err := m.Add(context.Background(), o)
	if err != nil || r2.OperationID != r.OperationID || r2.Phase != "complete" {
		t.Fatal("partial retry changed operation", err)
	}
	if _, err := m.Rollback(context.Background(), o.ReceiptDir); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Rollback(context.Background(), o.ReceiptDir); err != nil || f.rollbacks != 1 {
		t.Fatal("rollback not idempotent", err)
	}
	if _, err := os.Lstat(filepath.Join(o.ReceiptDir, "device.token")); err != nil {
		t.Fatal("rollback deleted retained credential")
	}
}
func TestEnrollmentDescriptorMismatchBeforeGrant(t *testing.T) {
	m, o, f := enrollmentFixture(t)
	f.mismatch.Store(true)
	_, err := m.Add(context.Background(), o)
	if err == nil || f.grants != 0 || f.exchanges.Load() != 0 {
		t.Fatal("mismatched identity reached pairing")
	}
}
func TestEnrollmentInputCannotWidenOrInject(t *testing.T) {
	m, o, f := enrollmentFixture(t)
	for _, target := range []string{"-oProxyCommand=bad", "worker;bad", "worker\nsecret", "user@worker bad"} {
		o.Target = target
		if _, err := m.Add(context.Background(), o); err == nil {
			t.Fatal("target injection accepted")
		}
	}
	o.Target = "worker"
	o.Scopes = []string{"*"}
	if _, err := m.Add(context.Background(), o); err == nil {
		t.Fatal("wildcard scope accepted")
	}
	if f.installs != 0 {
		t.Fatal("invalid input changed worker")
	}
}
func TestEnrollmentPrivateStorageRefusesModeAndSymlink(t *testing.T) {
	m, o, _ := enrollmentFixture(t)
	if err := os.Mkdir(o.ReceiptDir, 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Add(context.Background(), o); err == nil {
		t.Fatal("public receipt directory accepted")
	}
	if err := os.Chmod(o.ReceiptDir, 0700); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(t.TempDir(), "unrelated")
	if err := os.WriteFile(dest, []byte("retain"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(dest, filepath.Join(o.ReceiptDir, "receipt.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Add(context.Background(), o); err == nil || strings.Contains(err.Error(), dest) {
		t.Fatal("symlink accepted or private destination reflected")
	}
	b, _ := os.ReadFile(dest)
	if string(b) != "retain" {
		t.Fatal("symlink destination modified")
	}
}

type enrollmentCatalog struct{}

func (enrollmentCatalog) Load() (*config.Catalog, error) { return &config.Catalog{}, nil }

func TestMalformedReceiptDoesNotProjectPrivateInput(t *testing.T) {
	m, o, _ := enrollmentFixture(t)
	s, unlock, err := openReceipts(context.Background(), o.ReceiptDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := privateAtomic(s.root, "receipt.json", []byte(`{"operation_id":"synthetic-private-input","phase":"synthetic-private-input"}`), false); err != nil {
		t.Fatal(err)
	}
	unlock()
	r, err := m.Add(context.Background(), o)
	if err == nil || r.OperationID != "" || r.Phase != "" || strings.Contains(err.Error(), "synthetic-private-input") {
		t.Fatal("invalid private receipt escaped through error projection")
	}
}
