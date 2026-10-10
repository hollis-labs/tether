package sshenroll

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"

	"github.com/hollis-labs/tether/internal/environment"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/service"
	"golang.org/x/sys/unix"
	"gopkg.in/yaml.v3"
)

type workerRecord struct {
	Request       WorkerRequest `json:"request"`
	State         WorkerState   `json:"state"`
	CatalogSHA256 string        `json:"catalog_sha256"`
	Phase         string        `json:"phase"`
}
type Worker struct {
	home, root string
	record     workerRecord
	unlock     func()
}

func (r WorkerRequest) Validate() error {
	if !validOperation(r.OperationID) || r.Authority == "" || environment.ValidateAuthority(r.Authority) != nil || service.ValidateVersion(r.Version) != nil || !digestPattern.MatchString(r.ArchiveSHA256) || r.RemotePort < 1024 || r.RemotePort > 65535 {
		return problem("worker", "invalid-request", "A valid exact managed enrollment request is required.")
	}
	if _, err := identity.NormalizeDeviceScopes(r.Scopes); err != nil {
		return problem("worker", "invalid-scope", "Choose explicit independent device scopes.")
	}
	return nil
}
func sameWorkerRequest(a, b WorkerRequest) bool {
	a.DeviceID, b.DeviceID = "", ""
	return reflect.DeepEqual(a, b)
}

// OpenWorker accepts only the exact private operation. Missing provenance never
// adopts an existing catalog, service unit, daemon, or enrollment marker.
func OpenWorker(ctx context.Context, home string, r WorkerRequest) (*Worker, error) {
	if err := r.Validate(); err != nil {
		return nil, err
	}
	if !filepath.IsAbs(home) || home == "/" {
		return nil, problem("worker", "invalid-home", "Use the dedicated non-root worker account.")
	}
	root := filepath.Join(home, ".tether", "enrollment", r.OperationID)
	_, unlock, err := openReceipts(ctx, root)
	if err != nil {
		return nil, err
	}
	w := &Worker{home: home, root: root, unlock: unlock}
	f, err := openPrivate(filepath.Join(root, "worker.json"), unix.O_RDONLY)
	if errors.Is(err, os.ErrNotExist) {
		if _, err := os.Lstat(filepath.Join(home, ".tether", "catalog", "global.yaml")); !errors.Is(err, os.ErrNotExist) {
			unlock()
			return nil, problem("worker", "external-state", "Existing catalog state has no matching private enrollment receipt.")
		}
		w.record = workerRecord{Request: r, State: WorkerState{OperationID: r.OperationID, Authority: r.Authority, Version: r.Version, RemotePort: r.RemotePort}, Phase: "initializing"}
		if err := w.save(); err != nil {
			unlock()
			return nil, err
		}
		return w, nil
	}
	if err != nil {
		unlock()
		return nil, problem("worker", "unreadable-receipt", "Retain the worker operation receipt.")
	}
	data, err := readBounded(f, 64<<10)
	_ = f.Close()
	if err != nil || json.Unmarshal(data, &w.record) != nil || !sameWorkerRequest(r, w.record.Request) {
		unlock()
		return nil, problem("worker", "different-operation", "Retain the existing worker receipt; it does not authorize this operation.")
	}
	return w, nil
}
func (w *Worker) Close() { w.unlock() }
func (w *Worker) save() error {
	data, err := json.Marshal(w.record)
	if err != nil {
		return err
	}
	return privateAtomic(w.root, "worker.json", data, false)
}
func (w *Worker) catalogPath() string {
	return filepath.Join(w.home, ".tether", "catalog", "global.yaml")
}
func fileDigest(path string) (string, error) { return fileDigestBounded(path, 1<<20) }
func fileDigestBounded(path string, byteLimit int64) (string, error) {
	data, err := readInput(path, byteLimit)
	if err != nil {
		return "", err
	}
	d := sha256.Sum256(data)
	return hex.EncodeToString(d[:]), nil
}

// Prepare composes the existing init flow and adds only this new worker's
// explicit loopback/environment/shim settings. Unknown partial catalogs refuse.
func (w *Worker) Prepare(init func() error) error {
	if w.record.CatalogSHA256 != "" {
		digest, err := fileDigest(w.catalogPath())
		if err != nil || digest != w.record.CatalogSHA256 {
			return problem("worker", "catalog-changed", "Do not overwrite changed worker configuration; retain the partial receipt.")
		}
		return nil
	}
	if _, err := os.Lstat(w.catalogPath()); !errors.Is(err, os.ErrNotExist) {
		return problem("worker", "unverified-partial-catalog", "Initialization left an unverified catalog. Inspect it locally rather than overwrite it.")
	}
	if err := init(); err != nil {
		return safeStep("init", err)
	}
	data, err := readInput(w.catalogPath(), 1<<20)
	if err != nil {
		return err
	}
	var document yaml.Node
	if yaml.Unmarshal(data, &document) != nil || len(document.Content) != 1 {
		return problem("worker", "invalid-catalog", "The initializer must produce a valid catalog.")
	}
	root := document.Content[0]
	setYAML(root, "environment", map[string]any{"authority": w.record.Request.Authority, "label": w.record.Request.Authority})
	daemon := mappingYAML(root, "daemon")
	setYAML(daemon, "listen_addr", "unix:"+filepath.Join(w.home, ".tether", "run", "tetherd.sock"))
	setYAML(daemon, "pid_file", filepath.Join(w.home, ".tether", "run", "tetherd.pid"))
	remote := "127.0.0.1:" + portText(w.record.Request.RemotePort)
	setYAML(daemon, "remote_listener", map[string]any{"enabled": true, "listen_addr": "tcp:" + remote, "allowed_hosts": []string{remote}})
	defaults := mappingYAML(mappingYAML(root, "catalog"), "defaults")
	setYAML(defaults, "state_db", filepath.Join(w.home, ".tether", "state", "tether.db"))
	setYAML(defaults, "launch_host", "shim")
	setYAML(defaults, "shim_host", map[string]any{"systemd_user": true})
	data, err = yaml.Marshal(&document)
	if err != nil {
		return err
	}
	if err := os.Chmod(w.catalogPath(), 0600); err != nil {
		return safeStep("init", err)
	}
	if err := privateAtomic(filepath.Dir(w.catalogPath()), "global.yaml", data, false); err != nil {
		return err
	}
	w.record.CatalogSHA256, err = fileDigest(w.catalogPath())
	if err != nil {
		return err
	}
	w.record.Phase = "initialized"
	return w.save()
}
func mappingYAML(node *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			return node.Content[i+1]
		}
	}
	child := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	node.Content = append(node.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, child)
	return child
}
func setYAML(node *yaml.Node, key string, value any) {
	var child yaml.Node
	_ = child.Encode(value)
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			node.Content[i+1] = &child
			return
		}
	}
	node.Content = append(node.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, &child)
}

func (w *Worker) Install(ctx context.Context, m *service.Manager) (WorkerState, error) {
	if err := w.verifyManager(m); err != nil {
		return WorkerState{}, err
	}
	w.record.Phase = "installing"
	if err := w.save(); err != nil {
		return WorkerState{}, err
	}
	r := w.record.Request
	archive := filepath.Join(w.root, "tether_"+r.Version+"_linux_"+nativeArch()+".tar.gz")
	digest, err := fileDigestBounded(archive, maxArchive)
	if err != nil || digest != r.ArchiveSHA256 {
		return WorkerState{}, problem("artifact", "worker-checksum-mismatch", "The worker upload differs from the verified hub release.")
	}
	if err := m.Install(ctx, r.Version, archive, filepath.Join(w.root, "checksums.txt")); err != nil {
		return WorkerState{}, safeStep("service", err)
	}
	id, err := environment.EnsureID(filepath.Join(w.home, ".tether", "state"))
	if err != nil {
		return WorkerState{}, safeStep("identity", err)
	}
	if err := environment.BindAuthority(filepath.Join(w.home, ".tether", "state"), id, r.Authority); err != nil {
		return WorkerState{}, safeStep("identity", err)
	}
	w.record.State.EnvironmentID, w.record.State.Managed, w.record.State.Phase = id, true, "running"
	w.record.Phase = "running"
	return w.record.State, w.save()
}
func (w *Worker) verifyManager(m *service.Manager) error {
	if m == nil || m.Runtime.Root != filepath.Join(w.home, ".tether", "runtime") || m.Catalog != filepath.Dir(w.catalogPath()) {
		return problem("worker", "different-service", "The service must belong to this exact worker home and catalog.")
	}
	digest, err := fileDigest(w.catalogPath())
	if err != nil || digest != w.record.CatalogSHA256 {
		return problem("worker", "catalog-changed", "Retain the changed worker catalog; no service control is authorized.")
	}
	return nil
}
func (w *Worker) Inspect(ctx context.Context, m *service.Manager) (WorkerState, error) {
	if err := w.verifyManager(m); err != nil {
		return WorkerState{}, err
	}
	s := m.Status(ctx)
	if s.Ownership != "managed" || s.Current != w.record.Request.Version || s.Active != "active" || len(s.Problems) > 0 || w.record.Phase != "running" {
		return WorkerState{}, problem("worker", "service-unverified", "The original managed service must verify healthy before credential use.")
	}
	data, err := readInput(filepath.Join(w.home, ".tether", "state", "environment-id"), 512)
	if err != nil || string(data) != w.record.State.EnvironmentID+"\n" {
		return WorkerState{}, problem("worker", "identity-changed", "Retain the original worker identity receipt.")
	}
	return w.record.State, nil
}
func (w *Worker) Rollback(ctx context.Context, m *service.Manager, revoke func(string) error, deviceID string) (WorkerState, error) {
	if w.record.Phase == "rolled-back" {
		return w.record.State, nil
	}
	if err := w.verifyManager(m); err != nil {
		return WorkerState{}, err
	}
	// Known devices can be revoked only through the original local operator.
	// An uncertain exchange has no invented device ID; its unknown credential
	// outcome remains a separate operator reconciliation obligation.
	if deviceID != "" {
		if !strings.HasPrefix(deviceID, "msg://device/") {
			return WorkerState{}, problem("rollback", "invalid-device", "Use the exact paired device ID.")
		}
		if err := revoke(deviceID); err != nil {
			return WorkerState{}, safeStep("revoke", err)
		}
	}
	if err := m.Uninstall(ctx); err != nil {
		return WorkerState{}, safeStep("rollback", err)
	}
	w.record.Phase, w.record.State.Phase = "rolled-back", "rolled-back"
	return w.record.State, w.save()
}

func nativeArch() string { return runtime.GOARCH }
func readBounded(r io.Reader, byteLimit int64) ([]byte, error) {
	b, e := io.ReadAll(io.LimitReader(r, byteLimit+1))
	if e != nil || int64(len(b)) > byteLimit {
		return nil, problem("input", "oversized-input", "Private input exceeds its bound.")
	}
	return b, nil
}
