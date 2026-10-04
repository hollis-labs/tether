//go:build linux

package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/hollis-labs/go-runner/runner"
	"github.com/hollis-labs/go-sandbox/sandbox"
	"github.com/hollis-labs/substrate/harness/shim"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/shimhost"
	"github.com/hollis-labs/tether/internal/store"
)

func TestShimFlagOffPreservesRequest(t *testing.T) {
	f := shimFixture(t)
	t.Setenv(EnvLaunchHost, "direct")
	f.svc.shimHosting.capability = func() error { t.Fatal("flag off probed shim capabilities"); return nil }
	got, err := f.svc.prepareShimStart(context.Background(), f.plan, f.req)
	if err != nil || !reflect.DeepEqual(got, f.req) {
		t.Fatalf("direct request changed: %v", err)
	}
	if _, err := f.svc.Store.SessionShim(context.Background(), f.req.ID); !errors.Is(err, store.ErrSessionShimNotFound) {
		t.Fatal("flag off wrote placement")
	}
}

func TestShimPreflightFallbackPreservesRequest(t *testing.T) {
	for _, why := range []string{"runtime", "capability", "sandbox"} {
		t.Run(why, func(t *testing.T) {
			f := shimFixture(t)
			switch why {
			case "runtime":
				f.plan.ProviderBrand = "codex"
			case "capability":
				f.svc.shimHosting.capability = func() error { return &shimhost.Failure{Code: "unsupported"} }
			case "sandbox":
				f.svc.shimHosting.prepare = func(spec shim.Launch, _ *sandbox.ResolvedAccessPolicy, _ runner.ResourceLimits) (shim.Launch, func(), error) {
					return spec, func() {}, errors.New("unavailable")
				}
			}
			got, err := f.svc.prepareShimStart(context.Background(), f.plan, f.req)
			if err != nil || !reflect.DeepEqual(got, f.req) {
				t.Fatalf("fallback changed direct request: %v", err)
			}
			if _, err := f.svc.Store.SessionShim(context.Background(), f.req.ID); !errors.Is(err, store.ErrSessionShimNotFound) {
				t.Fatal("preflight created a host")
			}
		})
	}
}

func TestShimRealProviderPolicyAndBridgeEnvironment(t *testing.T) {
	f := shimFixture(t)
	prepare := f.svc.shimHosting.prepare
	f.svc.shimHosting.prepare = func(spec shim.Launch, p *sandbox.ResolvedAccessPolicy, l runner.ResourceLimits) (shim.Launch, func(), error) {
		if p.AccessFor(f.svc.shimHosting.provider.SessionDir(f.req.ID)) != sandbox.AccessDenied || !p.DenyUserServiceManager {
			t.Fatal("private state or manager access not denied")
		}
		if !strings.Contains(strings.Join(spec.Env, "\n"), "TEST_SECRET=provider-only") {
			t.Fatal("provider environment policy was lost")
		}
		l.MaxOpenFiles = 256
		return prepare(spec, p, l)
	}
	req, err := f.svc.prepareShimStart(context.Background(), f.plan, f.req)
	if err != nil {
		t.Fatal(err)
	}
	row, err := f.svc.Store.SessionShim(context.Background(), f.req.ID)
	if err != nil {
		t.Fatal(err)
	}
	r, err := loadShimReceipt(row)
	if err != nil {
		t.Fatal(err)
	}
	var spec shim.Launch
	if err := shimhost.ReadPrivateJSON(r.DescriptorPath, shim.MaxFrame, &spec); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(spec.Argv, " "), "ulimit -n 256") {
		t.Fatal("real provider argv lacks resource limits")
	}
	if strings.Contains(strings.Join(req.Options.Env, "\n"), "provider-only") || strings.Contains(strings.Join(spec.Argv, " "), "provider-only") {
		t.Fatal("credential leaked to bridge or argv")
	}
	if req.Options.Launch.Convention.Executable != f.svc.shimHosting.bridge[0] {
		t.Fatal("bridge binary not selected")
	}
	events, err := f.svc.Store.ListEventsBySession(f.req.ID, 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if strings.Contains(event.PayloadJSON, "provider-only") {
			t.Fatal("credential leaked to event")
		}
	}
}

func TestShimStopRetiresHostAndCredentials(t *testing.T) {
	f := shimFixture(t)
	r := f.start(t)
	ids, token := recoveryCredential(t, f.svc, f.req.ID)
	if err := f.svc.StopSession(f.req.ID); err != nil {
		t.Fatal(err)
	}
	canonical, err := f.svc.Store.SessionShim(context.Background(), f.req.ID)
	if err != nil {
		t.Fatal(err)
	}
	retired, err := loadShimReceipt(canonical)
	if err != nil || !retired.Retired {
		t.Fatalf("unretired placement: %v", err)
	}
	if _, err := os.Stat(r.DescriptorPath); !os.IsNotExist(err) {
		t.Fatal("secret-bearing descriptor retained")
	}
	if _, err := ids.Verify(context.Background(), token); !errors.Is(err, identity.ErrInvalidToken) {
		t.Fatalf("credential retained: %v", err)
	}
	row, _ := f.svc.Store.GetSession(f.req.ID)
	if row.State != "killed" {
		t.Fatalf("stop state=%s", row.State)
	}
}

func TestShimShutdownLeavesProviderAndPlacementAlive(t *testing.T) {
	f := shimFixture(t)
	r := f.start(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := f.svc.DrainSessions(ctx); err != nil {
		t.Fatal(err)
	}
	row, _ := f.svc.Store.GetSession(f.req.ID)
	if row.State != "detached" {
		t.Fatalf("shutdown state=%s", row.State)
	}
	shimRow, _ := f.svc.Store.SessionShim(context.Background(), f.req.ID)
	canonical, err := loadShimReceipt(shimRow)
	if err != nil || canonical.Retired {
		t.Fatalf("shutdown retired placement: %v", err)
	}
	if _, err := os.Stat(r.DescriptorPath); err != nil {
		t.Fatal("shutdown removed controller capability")
	}
	result, err := f.svc.shimHosting.provider.Inspect(ctx, canonical)
	if err != nil || !result.Running || result.Gone {
		t.Fatalf("shutdown killed child: %+v %v", result, err)
	}
}

func TestShimStopMissingCanonicalReceiptRetainsState(t *testing.T) {
	f := shimFixture(t)
	r := f.start(t)
	path := filepath.Join(filepath.Dir(r.DescriptorPath), "placement.json")
	if err := os.Rename(path, path+".held"); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Rename(path+".held", path) }()
	err := f.svc.StopSession(f.req.ID)
	if err == nil || shimFailureCode(err) != "outcome_unknown" {
		t.Fatalf("missing receipt: %v", err)
	}
	row, _ := f.svc.Store.GetSession(f.req.ID)
	if row.State != "running" {
		t.Fatalf("unknown stop changed state: %s", row.State)
	}
	if _, err := os.Stat(r.DescriptorPath); err != nil {
		t.Fatal("unknown stop deleted capability")
	}
}

func TestShimRealSandboxDeniesWholePrivateDirectory(t *testing.T) {
	f := shimFixture(t)
	if err := bwrapAvailable(f.root); err != nil {
		t.Skipf("sandbox unavailable: %v", err)
	}
	f.svc.shimHosting.prepare = shimhost.PrepareProvider
	// The probe is a shell CLI: the Go test process's TestMain writes temporary
	// fixture state and therefore cannot run inside a read-only state parent.
	probe := filepath.Join(f.root, "probe-cli")
	script := "#!/bin/sh\nprintf '%s\\n' '{\"type\":\"system\",\"subtype\":\"init\",\"session_id\":\"fake-native\"}'\nwhile IFS= read -r line; do\nif /bin/cat \"$TEST_SHIM_STATE_DIR/launch.json\" >/dev/null 2>&1; then exit 9; fi\nprintf '%s\\n' '{\"type\":\"result\",\"subtype\":\"success\",\"uuid\":\"probe-result\",\"result\":\"reply:state-denied\"}'\ndone\n"
	if err := os.WriteFile(probe, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	f.plan.Command = probe
	f.req.Options.Launch.Convention.Argv = nil
	f.req.Options.Env = append(f.req.Options.Env, "TEST_SHIM_STATE_DIR="+f.svc.shimHosting.provider.SessionDir(f.req.ID))
	r := f.start(t)
	var spec shim.Launch
	if err := shimhost.ReadPrivateJSON(r.DescriptorPath, shim.MaxFrame, &spec); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(filepath.Base(spec.Argv[0]), "bwrap") {
		t.Fatalf("provider descriptor lacks sandbox wrapper: %s", spec.Argv[0])
	}
	if err := f.svc.SendTurn(context.Background(), f.req.ID, "probe-state"); err != nil {
		t.Fatal(err)
	}
	shimAwait(t, "sandboxed provider result", func() bool {
		outputs := outputEvents(t, f.svc)
		return len(outputs) == 1 && outputs[0].Text == "reply:state-denied"
	})
	if err := f.svc.StopSession(f.req.ID); err != nil {
		t.Fatal(err)
	}
}

func TestShimStopRefusalsKeepStateAndCanonicalIdentity(t *testing.T) {
	for _, code := range []string{"outcome_unknown", "identity_mismatch", "journal_mismatch", "unauthorized", "stale_controller", "protocol_error"} {
		t.Run(code, func(t *testing.T) {
			f := shimFixture(t)
			r := f.start(t)
			f.svc.shimHosting.stop = func(_ context.Context, canonical shimhost.Receipt) error {
				if canonical.HostPID != r.HostPID || canonical.HostStartTime == 0 || canonical.HostStartTime != r.HostStartTime {
					t.Fatal("stop used a bare database PID")
				}
				return &shimhost.Failure{Code: code}
			}
			if err := f.svc.StopSession(f.req.ID); err == nil || shimFailureCode(err) != code {
				t.Fatalf("stop: %v", err)
			}
			row, _ := f.svc.Store.GetSession(f.req.ID)
			if row.State != "running" {
				t.Fatalf("refused stop changed state: %s", row.State)
			}
			if _, err := os.Stat(r.DescriptorPath); err != nil {
				t.Fatal("refused stop cleaned capability")
			}
		})
	}
}

func TestShimUncertainPlacementNeverLaunchesAnotherChild(t *testing.T) {
	f := shimFixture(t)
	calls := 0
	f.svc.shimHosting.place = func(ctx context.Context, key string, spec shim.Launch) (shimhost.Receipt, error) {
		calls++
		r, err := f.svc.shimHosting.provider.Place(ctx, key, spec)
		if err != nil {
			return r, err
		}
		return r, &shimhost.Failure{Code: "outcome_unknown", Message: "lost placement response"}
	}
	_, err := f.svc.prepareShimStart(context.Background(), f.plan, f.req)
	if err == nil || shimFailureCode(err) != "outcome_unknown" || calls != 1 {
		t.Fatalf("uncertain placement: %v calls=%d", err, calls)
	}
	if _, live := f.svc.Manager.Get(f.req.ID); live {
		t.Fatal("unknown placement launched a direct runtime")
	}
	row, _ := f.svc.Store.GetSession(f.req.ID)
	if row.State != "detached" {
		t.Fatalf("unknown placement state=%s", row.State)
	}
	f.svc.ReconcileStaleState()
	row, _ = f.svc.Store.GetSession(f.req.ID)
	if row.State != "running" || calls != 1 {
		t.Fatalf("reconcile created a second child: state=%s calls=%d", row.State, calls)
	}
	if err := f.svc.StopSession(f.req.ID); err != nil {
		t.Fatal(err)
	}
}
