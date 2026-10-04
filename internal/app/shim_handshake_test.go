//go:build linux

package app

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/hollis-labs/substrate/harness/shim"
	"github.com/hollis-labs/tether/internal/shimbridge"
	"github.com/hollis-labs/tether/internal/shimhost"
	"github.com/hollis-labs/tether/internal/store"
)

func uncertainShim(t *testing.T, f *shimAppFixture) (store.SessionShimRow, shimhost.Receipt) {
	t.Helper()
	f.svc.shimHosting.place = func(ctx context.Context, key string, spec shim.Launch) (shimhost.Receipt, error) {
		r, err := f.svc.shimHosting.provider.Place(ctx, key, spec)
		if err != nil {
			return r, err
		}
		return r, &shimhost.Failure{Code: "outcome_unknown"}
	}
	if _, err := f.svc.prepareShimStart(context.Background(), f.plan, f.req); shimFailureCode(err) != "outcome_unknown" {
		t.Fatalf("placement: %v", err)
	}
	row, err := f.svc.Store.SessionShim(context.Background(), f.req.ID)
	if err != nil {
		t.Fatal(err)
	}
	r, err := loadShimReceipt(row)
	if err != nil {
		t.Fatal(err)
	}
	return row, r
}

func shimStatusEvents(t *testing.T, f *shimAppFixture) []shimStatus {
	t.Helper()
	events, err := f.svc.Store.QueryEvents(store.EventFilter{SessionID: f.req.ID, Kinds: []string{"session.shim_status"}, Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	var result []shimStatus
	for _, ev := range events {
		var status shimStatus
		if err := json.Unmarshal([]byte(ev.PayloadJSON), &status); err != nil {
			t.Fatal(err)
		}
		result = append(result, status)
	}
	return result
}

func TestShimReadinessWaitsForDurableHandshake(t *testing.T) {
	f := shimFixture(t)
	_, r := uncertainShim(t, f)
	gate := filepath.Join(f.root, "hello-gate")
	if err := syscall.Mkfifo(gate, 0600); err != nil {
		t.Fatal(err)
	}
	// O_RDWR keeps a reader available for cleanup even if an assertion fails.
	fifo, err := os.OpenFile(gate, os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	release := sync.OnceFunc(func() { _, _ = fifo.Write([]byte{1}); _ = fifo.Close() })
	defer release()
	f.svc.shimHosting.bridge = append(f.svc.shimHosting.bridge, "--handshake-gate", gate)
	observed := make(chan struct{})
	proceed := make(chan struct{})
	resume := sync.OnceFunc(func() { close(proceed) })
	defer resume()
	f.svc.shimHosting.handshake = func(ctx context.Context, receipt shimhost.Receipt, previous string) error {
		close(observed)
		select {
		case <-proceed:
		case <-ctx.Done():
		}
		return waitShimHandshake(ctx, receipt, previous, func(path string) (shimbridge.Checkpoint, error) {
			return readShimHandshakeCheckpoint(ctx, path)
		})
	}
	done := make(chan struct{})
	go func() { f.svc.ReconcileStaleState(); close(done) }()
	defer func() {
		resume()
		release()
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			t.Error("recovery did not finish")
		}
	}()
	select {
	case <-observed:
	case <-done:
		t.Fatal("recovery published readiness before the blocked controller handshake")
	case <-time.After(8 * time.Second):
		t.Fatal("handshake wait never entered")
	}
	for _, status := range shimStatusEvents(t, f) {
		if status.State == "running" || status.Reason == "reattached" {
			t.Fatalf("premature readiness: %+v", status)
		}
	}
	row, err := f.svc.Store.SessionShim(context.Background(), f.req.ID)
	if err != nil {
		t.Fatal(err)
	}
	retained, err := loadShimReceipt(row)
	if err != nil || retained.Retired || retained.HostPID != r.HostPID {
		t.Fatalf("blocked handshake lost placement: %+v %v", retained, err)
	}
	release()
	resume()
	select {
	case <-done:
	case <-time.After(8 * time.Second):
		t.Fatal("released handshake did not finish")
	}
	found := false
	for _, status := range shimStatusEvents(t, f) {
		if status.State == "running" && status.Reason == "reattached" {
			found = true
		}
	}
	if !found {
		t.Fatal("settled handshake emitted no readiness")
	}
	checkpoint, err := shimbridge.ReadCheckpoint(filepath.Join(filepath.Dir(r.DescriptorPath), "bridge.json"))
	if err != nil || checkpoint.ControllerEpoch == "" {
		t.Fatalf("readiness lacked durable epoch: %+v %v", checkpoint, err)
	}
	if err := f.svc.StopSession(f.req.ID); err != nil {
		t.Fatalf("Stop immediately after readiness: %v", err)
	}
}

func TestShimHandshakeExpiryRetainsPlacement(t *testing.T) {
	f := shimFixture(t)
	_, r := uncertainShim(t, f)
	f.svc.shimHosting.handshake = func(ctx context.Context, receipt shimhost.Receipt, previous string) error {
		expired, cancel := context.WithDeadline(ctx, time.Now().Add(-time.Second))
		defer cancel()
		return waitShimHandshake(expired, receipt, previous, shimbridge.ReadCheckpoint)
	}
	f.svc.ReconcileStaleState()
	pending := false
	for _, status := range shimStatusEvents(t, f) {
		if status.State == "running" || status.Reason == "reattached" {
			t.Fatalf("expired handshake claimed readiness: %+v", status)
		}
		if status.State == "detached" && status.Reason == "handshake_pending" {
			pending = true
		}
	}
	if !pending {
		t.Fatal("expiry emitted no typed pending outcome")
	}
	row, err := f.svc.Store.SessionShim(context.Background(), f.req.ID)
	if err != nil {
		t.Fatal(err)
	}
	retained, err := loadShimReceipt(row)
	if err != nil || retained.Retired || retained.HostPID != r.HostPID {
		t.Fatalf("expiry lost placement: %+v %v", retained, err)
	}
	session, _ := f.svc.Store.GetSession(f.req.ID)
	if session.State != "detached" {
		t.Fatalf("pending state: %s", session.State)
	}
}

func TestShimHandshakeRequiresFreshMatchingEpoch(t *testing.T) {
	r := shimhost.Receipt{Session: "session", Instance: "instance", Generation: 1, Journal: "journal", DescriptorPath: "/private/launch.json"}
	for _, name := range []string{"fresh", "stale", "absent", "identity", "journal"} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			state := shimbridge.Checkpoint{Session: r.Session, Instance: r.Instance, Generation: r.Generation, Journal: r.Journal, ControllerEpoch: "new"}
			want := ""
			switch name {
			case "stale":
				state.ControllerEpoch = "old"
				want = "handshake_pending"
			case "absent":
				state.ControllerEpoch = ""
				want = "handshake_pending"
			case "identity":
				state.Generation++
				want = "identity_mismatch"
			case "journal":
				state.Journal = "another"
				want = "identity_mismatch"
			}
			err := waitShimHandshake(ctx, r, "old", func(string) (shimbridge.Checkpoint, error) {
				// End a pending wait deterministically after its first observation.
				if name == "stale" || name == "absent" {
					cancel()
				}
				return state, nil
			})
			if want == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if shimFailureCode(err) != want {
				t.Fatalf("readiness: %v, want %s", err, want)
			}
		})
	}
}

func TestShimHandshakeCheckpointWaitsForCommit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bridge.json")
	if err := os.Chmod(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	state := shimbridge.Checkpoint{ControllerEpoch: "fresh"}
	if err := shimhost.WritePrivateJSON(path, state); err != nil {
		t.Fatal(err)
	}
	lock, err := shimhost.Lock(filepath.Join(filepath.Dir(path), "record.lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lock.Close() }()
	// Model a visible rename whose writer still holds the commit lock. Even
	// an already-visible fresh epoch cannot pass this barrier.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := readShimHandshakeCheckpoint(ctx, path); err == nil {
		t.Fatal("accepted epoch before commit lock released")
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	got, err := readShimHandshakeCheckpoint(context.Background(), path)
	if err != nil || got.ControllerEpoch != "fresh" {
		t.Fatalf("completed commit: %+v %v", got, err)
	}
}
