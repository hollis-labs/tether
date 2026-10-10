package sshenroll

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/service"
	"github.com/hollis-labs/tether/internal/setup"
)

func TestWorkerStagesExistingInitAndManagedServiceWithRetainedState(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	archive, checksums, _ := testRelease(t, nativeArch())
	digest, err := fileDigestBounded(archive, maxArchive)
	if err != nil {
		t.Fatal(err)
	}
	r := WorkerRequest{OperationID: uuid.NewString(), Authority: "worker", Version: "0.8.0", ArchiveSHA256: digest, RemotePort: 7181, Scopes: []string{"read"}}
	w, err := OpenWorker(context.Background(), home, r)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	initCalls := 0
	initialize := func() error {
		initCalls++
		_, err := setup.WriteCatalog(filepath.Join(home, ".tether"), setup.WriteOpts{StateRoot: filepath.Join(home, ".tether"), ProviderCommands: map[string]string{"codex": "/synthetic/codex"}})
		return err
	}
	if err := w.Prepare(initialize); err != nil {
		t.Fatal(err)
	}
	if err := w.Prepare(initialize); err != nil || initCalls != 1 {
		t.Fatal("init repeated or config not idempotent", err)
	}
	cat, err := config.Load(filepath.Join(home, ".tether", "catalog"))
	if err != nil {
		t.Fatal(err)
	}
	if cat.Global.Environment.Authority != "worker" || !cat.Global.Daemon.RemoteListener.Enabled || cat.Global.Daemon.RemoteListener.ListenAddr != "tcp:127.0.0.1:7181" {
		t.Fatal("prepared config lacks exact remote/environment settings")
	}
	for _, pair := range []struct{ src, dst string }{{archive, filepath.Join(w.root, filepath.Base(archive))}, {checksums, filepath.Join(w.root, "checksums.txt")}} {
		b, err := os.ReadFile(pair.src)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(pair.dst, b, 0600); err != nil {
			t.Fatal(err)
		}
	}
	active := false
	commands := []string{}
	m := &service.Manager{Runtime: service.Runtime{Root: filepath.Join(home, ".tether", "runtime")}, UnitDir: filepath.Join(home, ".config", "systemd", "user"), Catalog: filepath.Join(home, ".tether", "catalog"), PathEnv: "/synthetic:/usr/bin", UID: "1000", User: "fixture"}
	m.DaemonPID = func() (int, error) {
		if active {
			return 1234, nil
		}
		return 0, nil
	}
	m.Run = func(_ context.Context, command string, args ...string) (string, error) {
		commands = append(commands, command+" "+strings.Join(args, " "))
		if command == "loginctl" {
			return "yes", nil
		}
		joined := strings.Join(args, " ")
		if strings.Contains(joined, "restart") {
			active = true
		}
		if strings.Contains(joined, "disable") {
			active = false
		}
		if strings.Contains(joined, "show") {
			if _, err := os.Lstat(filepath.Join(m.UnitDir, service.UnitName)); os.IsNotExist(err) {
				return "LoadState=not-found\n", nil
			}
			state := "inactive"
			pid := "0"
			if active {
				state, pid = "active", "1234"
			}
			return "LoadState=loaded\nActiveState=" + state + "\nFragmentPath=" + filepath.Join(m.UnitDir, service.UnitName) + "\nExecMainPID=" + pid + "\nUnitFileState=enabled\n", nil
		}
		return "", nil
	}
	state, err := w.Install(context.Background(), m)
	if err != nil || !state.Managed || state.EnvironmentID == "" {
		t.Fatal("managed service install", err)
	}
	if _, err := w.Inspect(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Rollback(context.Background(), m, func(string) error { t.Fatal("unexpected device revoke"); return nil }, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(m.Runtime.Root, "versions", "0.8.0", "tether")); err != nil {
		t.Fatal("rollback removed runtime")
	}
	if _, err := os.Lstat(w.catalogPath()); err != nil {
		t.Fatal("rollback removed catalog")
	}
	for _, cmd := range commands {
		if strings.Contains(cmd, "sudo") || strings.Contains(cmd, "shim") || strings.Contains(cmd, "tether.service") {
			t.Fatal("rollback crossed service ownership")
		}
	}
}
func TestWorkerRefusesExternalAndChangedCatalog(t *testing.T) {
	home := t.TempDir()
	archive, _, _ := testRelease(t, nativeArch())
	digest, _ := fileDigestBounded(archive, maxArchive)
	r := WorkerRequest{OperationID: uuid.NewString(), Authority: "worker", Version: "0.8.0", ArchiveSHA256: digest, RemotePort: 7181, Scopes: []string{"read"}}
	global := filepath.Join(home, ".tether", "catalog", "global.yaml")
	if err := os.MkdirAll(filepath.Dir(global), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(global, []byte("retain external"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenWorker(context.Background(), home, r); err == nil {
		t.Fatal("external catalog adopted")
	}
	b, _ := os.ReadFile(global)
	if string(b) != "retain external" {
		t.Fatal("external catalog overwritten")
	}
}
