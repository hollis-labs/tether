package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

type Problem struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Hint    string `json:"hint,omitempty"`
}

func (p *Problem) Error() string {
	if p.Hint != "" {
		return p.Code + ": " + p.Message + "\n" + p.Hint
	}
	return p.Code + ": " + p.Message
}

type Status struct {
	Ownership    string    `json:"ownership"`
	Current      string    `json:"current,omitempty"`
	Previous     string    `json:"previous,omitempty"`
	Active       string    `json:"active,omitempty"`
	Enabled      string    `json:"enabled,omitempty"`
	Linger       string    `json:"linger,omitempty"`
	Problems     []Problem `json:"problems"`
	ProtocolHint string    `json:"protocol_hint,omitempty"`
}

type Manager struct {
	Runtime Runtime
	UnitDir string
	Catalog string
	PathEnv string
	UID     string
	User    string
	Run     Runner
	// The CLI supplies the selected catalog's verified daemon identity. Reading
	// a stored PID alone never authorizes service adoption or process control.
	DaemonPID func() (int, error)
}

type serviceRecord struct {
	UnitPath   string `json:"unit_path"`
	UnitSHA256 string `json:"unit_sha256"`
	Catalog    string `json:"catalog"`
	PathEnv    string `json:"path"`
}

func (m *Manager) unitPath() string      { return filepath.Join(m.UnitDir, UnitName) }
func (m *Manager) recordPath() string    { return filepath.Join(m.Runtime.Root, ".service.json") }
func (m *Manager) ownershipPath() string { return filepath.Join(m.Runtime.Root, "ownership") }
func (m *Manager) run(ctx context.Context, command string, args ...string) (string, error) {
	if m.Run == nil {
		return RunCommand(ctx, command, args...)
	}
	return m.Run(ctx, command, args...)
}
func (m *Manager) validate() error {
	if runtime.GOOS != "linux" {
		return &Problem{Code: "unsupported-platform", Message: "Worker services require Linux systemd --user."}
	}
	if !filepath.IsAbs(m.Runtime.Root) || !filepath.IsAbs(m.UnitDir) || !filepath.IsAbs(m.Catalog) {
		return fmt.Errorf("service paths must be absolute")
	}
	uid, err := strconv.ParseUint(m.UID, 10, 32)
	if err != nil {
		return fmt.Errorf("service user UID must be numeric")
	}
	if uid == 0 {
		return &Problem{Code: "root-service-refused", Message: "Install as the normal worker user; only the separate linger command needs privileges."}
	}
	if m.User == "" || strings.ContainsAny(m.User, "\n\r\x00") {
		return fmt.Errorf("invalid service user name")
	}
	if m.PathEnv == "" {
		return fmt.Errorf("installing shell PATH is empty")
	}
	_, err = renderUnit(m.Runtime.Root, m.Catalog, m.PathEnv)
	return err
}

func parseProperties(output string) map[string]string {
	properties := make(map[string]string)
	for _, line := range strings.Split(output, "\n") {
		key, value, ok := strings.Cut(line, "=")
		if ok {
			properties[key] = value
		}
	}
	return properties
}

func (m *Manager) inspectUnit(ctx context.Context) (map[string]string, error) {
	out, err := m.run(ctx, "systemctl", "--user", "show", UnitName, "--property=LoadState,ActiveState,SubState,FragmentPath,ExecMainPID,UnitFileState")
	p := parseProperties(out)
	if p["LoadState"] == "not-found" {
		return p, nil
	}
	if err != nil || p["LoadState"] == "" {
		return p, &Problem{Code: "user-manager-unavailable", Message: "Cannot inspect the systemd user manager.", Hint: "Run systemctl --user status in a login session for this user."}
	}
	return p, nil
}

func (m *Manager) linger(ctx context.Context) (string, error) {
	out, err := m.run(ctx, "loginctl", "show-user", m.UID, "--property=Linger", "--value")
	value := strings.TrimSpace(out)
	if err != nil || (value != "yes" && value != "no") {
		return "", &Problem{Code: "linger-unavailable", Message: "Cannot determine whether services survive logout.", Hint: "Run loginctl show-user " + m.UID + " --property=Linger."}
	}
	if value == "no" {
		return value, &Problem{Code: "linger-disabled", Message: "The service would stop after logout; an administrator must enable linger.", Hint: "sudo loginctl enable-linger '" + strings.ReplaceAll(m.User, "'", "'\\''") + "'"}
	}
	return value, nil
}

func readServiceRecord(root string) (serviceRecord, error) {
	var record serviceRecord
	data, err := readRegular(filepath.Join(root, ".service.json"), 64<<10)
	if err != nil {
		return record, err
	}
	if err := json.Unmarshal(data, &record); err != nil {
		return record, err
	}
	if !filepath.IsAbs(record.UnitPath) || filepath.Base(record.UnitPath) != UnitName || !sha256Hex.MatchString(record.UnitSHA256) || !filepath.IsAbs(record.Catalog) {
		return record, fmt.Errorf("invalid service provenance")
	}
	return record, nil
}

func (m *Manager) ownership() string {
	marker, err := readRegular(m.ownershipPath(), 32)
	if err != nil {
		return "unknown"
	}
	value := strings.TrimSpace(string(marker))
	if value != "managed" && value != "external" {
		return "unknown"
	}
	return value
}

func verifiedUnit(root string) (serviceRecord, error) {
	marker, err := readRegular(filepath.Join(root, "ownership"), 32)
	if err != nil || string(marker) != "managed\n" {
		return serviceRecord{}, fmt.Errorf("managed ownership is unverified")
	}
	record, err := readServiceRecord(root)
	if err != nil {
		return record, err
	}
	data, err := readRegular(record.UnitPath, 64<<10)
	if err != nil {
		return record, err
	}
	digest := sha256.Sum256(data)
	if hex.EncodeToString(digest[:]) != record.UnitSHA256 {
		return record, fmt.Errorf("unit provenance changed")
	}
	expected, err := renderUnit(root, record.Catalog, record.PathEnv)
	if err != nil || string(expected) != string(data) {
		return record, fmt.Errorf("unit provenance does not describe the managed launcher")
	}
	return record, nil
}

func (m *Manager) authorize(ctx context.Context, installing bool) (map[string]string, error) {
	p, err := m.inspectUnit(ctx)
	if err != nil {
		return p, err
	}
	external := &Problem{Code: "external-install", Message: "This daemon or unit is externally owned; it will not be adopted, stopped or changed.", Hint: "Keep the existing supervisor; choose a separate worker account/state for a managed install."}
	ownership := m.ownership()
	_, unitErr := os.Lstat(m.unitPath())
	if ownership == "external" {
		return p, external
	}
	record, proofErr := verifiedUnit(m.Runtime.Root)
	managed := proofErr == nil && record.UnitPath == m.unitPath() && record.Catalog == m.Catalog
	if unitErr == nil && !managed {
		return p, external
	}
	if unitErr != nil && !os.IsNotExist(unitErr) {
		return p, unitErr
	}
	if fragment := p["FragmentPath"]; fragment != "" && (!managed || fragment != m.unitPath()) {
		return p, external
	}
	if p["ActiveState"] == "active" && m.DaemonPID == nil {
		return p, &Problem{Code: "daemon-state-unknown", Message: "The active unit's daemon identity is unverified."}
	}
	if m.DaemonPID != nil {
		pid, err := m.DaemonPID()
		if err != nil {
			return p, &Problem{Code: "daemon-state-unknown", Message: "Cannot verify the selected state's daemon identity."}
		}
		mainPID, parseErr := strconv.Atoi(p["ExecMainPID"])
		if pid > 0 && (!managed || parseErr != nil || pid != mainPID || p["ActiveState"] != "active") {
			return p, external
		}
		if p["ActiveState"] == "active" && pid <= 0 {
			return p, &Problem{Code: "daemon-state-unknown", Message: "The active unit has not established the selected daemon's verified PID."}
		}
	}
	if !installing && !managed {
		return p, &Problem{Code: "ownership-unverified", Message: "Managed service provenance is missing or changed; refusing control."}
	}
	if installing && ownership == "managed" && !managed {
		return p, &Problem{Code: "ownership-unverified", Message: "Managed service installation is incomplete or changed."}
	}
	return p, nil
}

func (m *Manager) operation(ctx context.Context) (func(), error) {
	if err := m.validate(); err != nil {
		return nil, err
	}
	if err := m.Runtime.prepare(); err != nil {
		return nil, err
	}
	return acquireLock(ctx, filepath.Join(m.Runtime.Root, ".service.lock"))
}

func (m *Manager) Install(ctx context.Context, version, archive, checksums string) error {
	if err := ValidateVersion(version); err != nil {
		return err
	}
	if err := m.validate(); err != nil {
		return err
	}
	// Prerequisites are read before runtime/unit publication; no privileged
	// command, catalog mutation, provider probe or login hook runs here.
	if _, err := m.linger(ctx); err != nil {
		return err
	}
	unlock, err := m.operation(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	if _, err := m.authorize(ctx, true); err != nil {
		var p *Problem
		if errors.As(err, &p) && p.Code == "external-install" && m.ownership() == "unknown" {
			_ = writeAtomic(m.ownershipPath(), []byte("external\n"))
		}
		return err
	}
	if _, err := m.Runtime.Install(ctx, version, archive, checksums); err != nil {
		return err
	}
	if err := ensureDirectory(m.UnitDir); err != nil {
		return err
	}
	unit, err := renderUnit(m.Runtime.Root, m.Catalog, m.PathEnv)
	if err != nil {
		return err
	}
	if err := writeAtomic(m.unitPath(), unit); err != nil {
		return err
	}
	digest := sha256.Sum256(unit)
	record := serviceRecord{UnitPath: m.unitPath(), UnitSHA256: hex.EncodeToString(digest[:]), Catalog: m.Catalog, PathEnv: m.PathEnv}
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	if err := writeAtomic(m.recordPath(), data); err != nil {
		return err
	}
	if err := writeAtomic(m.ownershipPath(), []byte("managed\n")); err != nil {
		return err
	}
	if err := m.Runtime.Select(version); err != nil {
		return err
	}
	for _, args := range [][]string{{"--user", "daemon-reload"}, {"--user", "enable", UnitName}, {"--user", "restart", UnitName}} {
		if _, err := m.run(ctx, "systemctl", args...); err != nil {
			return &Problem{Code: "service-start-failed", Message: "The managed unit was installed but could not be activated.", Hint: "Run tether service status and journalctl --user -u " + UnitName + "; installed versions are retained."}
		}
	}
	return m.awaitRunning(ctx)
}

func (m *Manager) Restart(ctx context.Context) error {
	unlock, err := m.operation(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	if _, err := m.authorize(ctx, false); err != nil {
		return err
	}
	if _, err := m.Runtime.Selector("current"); err != nil {
		return &Problem{Code: "runtime-incomplete", Message: err.Error()}
	}
	return m.restart(ctx)
}
func (m *Manager) restart(ctx context.Context) error {
	if _, err := m.run(ctx, "systemctl", "--user", "restart", UnitName); err != nil {
		return &Problem{Code: "service-restart-failed", Message: "The managed unit did not restart.", Hint: "Run tether service status and journalctl --user -u " + UnitName + "; switch back manually if needed."}
	}
	return m.awaitRunning(ctx)
}

func (m *Manager) awaitRunning(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	for {
		p, err := m.authorize(ctx, false)
		var problem *Problem
		if err != nil && (!errors.As(err, &problem) || problem.Code != "daemon-state-unknown") {
			return err
		}
		if err == nil && p["ActiveState"] == "active" {
			return nil
		}
		if p["ActiveState"] == "failed" || p["ActiveState"] == "inactive" {
			return &Problem{Code: "service-start-failed", Message: "The managed worker exited before establishing readiness.", Hint: "Run tether service status and journalctl --user -u " + UnitName + "."}
		}
		timer := time.NewTimer(25 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return &Problem{Code: "service-start-timeout", Message: "The managed unit has not established verified daemon readiness.", Hint: "Inspect tether service status; the selected and previous runtimes are retained."}
		case <-timer.C:
		}
	}
}

func (m *Manager) Update(ctx context.Context, version, archive, checksums string) error {
	if err := ValidateVersion(version); err != nil {
		return err
	}
	unlock, err := m.operation(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	if _, err := m.authorize(ctx, false); err != nil {
		return err
	}
	if _, err := m.Runtime.Install(ctx, version, archive, checksums); err != nil {
		return err
	}
	if err := m.Runtime.Select(version); err != nil {
		return err
	}
	return m.restart(ctx)
}

func (m *Manager) SwitchBack(ctx context.Context, version string) error {
	unlock, err := m.operation(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	if _, err := m.authorize(ctx, false); err != nil {
		return err
	}
	if version == "" {
		version, err = m.Runtime.Selector("previous")
		if err != nil {
			return &Problem{Code: "previous-runtime-unavailable", Message: err.Error()}
		}
	}
	if err := m.Runtime.Select(version); err != nil {
		return err
	}
	return m.restart(ctx)
}

func (m *Manager) Uninstall(ctx context.Context) error {
	unlock, err := m.operation(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	if _, err := m.authorize(ctx, false); err != nil {
		return err
	}
	if _, err := m.run(ctx, "systemctl", "--user", "disable", "--now", UnitName); err != nil {
		return &Problem{Code: "service-stop-failed", Message: "The managed unit did not stop; ownership files were retained."}
	}
	if err := os.Remove(m.unitPath()); err != nil {
		return err
	}
	if _, err := m.run(ctx, "systemctl", "--user", "daemon-reload"); err != nil {
		return err
	}
	if err := os.Remove(m.recordPath()); err != nil {
		return err
	}
	return os.Remove(m.ownershipPath()) // immutable versions, state and credentials are retained
}

func (m *Manager) Status(ctx context.Context) Status {
	s := Status{Ownership: m.ownership(), Problems: []Problem{}}
	add := func(err error) {
		if err == nil {
			return
		}
		var p *Problem
		if errors.As(err, &p) {
			s.Problems = append(s.Problems, *p)
		} else {
			s.Problems = append(s.Problems, Problem{Code: "runtime-incomplete", Message: err.Error()})
		}
	}
	if err := m.validate(); err != nil {
		add(err)
		return s
	}
	s.Current, _ = m.Runtime.Selector("current")
	s.Previous, _ = m.Runtime.Selector("previous")
	var lingerErr error
	s.Linger, lingerErr = m.linger(ctx)
	add(lingerErr)
	p, err := m.authorize(ctx, false)
	add(err)
	if p != nil {
		s.Active, s.Enabled = p["ActiveState"], p["UnitFileState"]
	}
	if err != nil {
		var problem *Problem
		if errors.As(err, &problem) && problem.Code == "external-install" {
			s.Ownership = "external"
		}
		return s
	}
	if s.Current == "" {
		add(&Problem{Code: "runtime-incomplete", Message: "The active runtime is absent or incomplete."})
	}
	if s.Active == "failed" {
		add(&Problem{Code: "service-failed", Message: "Read journalctl --user -u " + UnitName + " for the failure."})
	} else if s.Active != "active" {
		add(&Problem{Code: "service-stopped", Message: "Run tether service restart."})
	}
	if s.Enabled != "enabled" {
		add(&Problem{Code: "service-disabled", Message: "Run tether service install with the selected exact version to repair startup."})
	}
	return s
}
