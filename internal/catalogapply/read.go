package catalogapply

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const auditFile = "apply-audit.jsonl"

// AuditRecord is one applied change: what, by whom, when, and the backup that
// undoes it.
type AuditRecord struct {
	At         string     `json:"at"`
	PlanID     string     `json:"plan_id"`
	Name       string     `json:"name"`
	TaskID     string     `json:"task_id,omitempty"`
	Digest     string     `json:"digest"`
	Files      []string   `json:"files"`
	BackupID   string     `json:"backup_id"`
	Activation string     `json:"activation"`
	Privileged []string   `json:"privileged,omitempty"`
	RollbackOf string     `json:"rollback_of,omitempty"`
	PlannedBy  Caller     `json:"planned_by"`
	ApprovedAt *time.Time `json:"approved_at,omitempty"`
}

// History returns the newest applied changes first, at most limit (default 20).
func (e *Engine) History(limit int) ([]AuditRecord, error) {
	if limit <= 0 || limit > 200 {
		limit = 20
	}
	f, err := os.Open(filepath.Join(e.BackupRoot, auditFile)) //nolint:gosec // G304: fixed file under the daemon's backup root
	if errors.Is(err, fs.ErrNotExist) {
		return []AuditRecord{}, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close() //nolint:errcheck
	var all []AuditRecord
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4<<20)
	for sc.Scan() {
		var r AuditRecord
		if json.Unmarshal(sc.Bytes(), &r) == nil {
			all = append(all, r)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	for i, j := 0, len(all)-1; i < j; i, j = i+1, j-1 {
		all[i], all[j] = all[j], all[i]
	}
	if len(all) > limit {
		all = all[:limit]
	}
	return all, nil
}

// readableDirs are the catalog directories whose files an agent may read raw.
// providers/, mcp-servers/, global.yaml and sandbox-profiles/ are left out:
// they carry or point at credentials and daemon policy, and reading a redacted
// copy to edit it would write the redaction back.
var readableDirs = map[string]bool{
	"agents": true, "launches": true, "projects": true, "skills": true,
	"boot-profiles": true, "team-definitions": true,
}

// FileInfo is one readable catalog file.
type FileInfo struct {
	Path string `json:"path"`
	Size int64  `json:"size"`
}

func readablePath(p string) error {
	if err := ValidatePath(p); err != nil {
		return err
	}
	if !readableDirs[strings.SplitN(p, "/", 2)[0]] {
		return fmt.Errorf("%s is not a file an agent reads raw (readable: agents, launches, projects, skills, boot-profiles, team-definitions); providers, MCP servers and global.yaml can hold credentials", p)
	}
	return nil
}

// ListFiles lists the raw-readable catalog files, sorted.
func (e *Engine) ListFiles() ([]FileInfo, error) {
	out := []FileInfo{}
	for dir := range readableDirs {
		root := filepath.Join(e.Root, dir)
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				if errors.Is(err, fs.ErrNotExist) {
					return nil
				}
				return err
			}
			if d.IsDir() || !d.Type().IsRegular() {
				return nil
			}
			rel, rerr := filepath.Rel(e.Root, path)
			if rerr != nil {
				return rerr
			}
			rel = filepath.ToSlash(rel)
			if readablePath(rel) != nil {
				return nil //nolint:nilerr // files outside the allowlist are not listed
			}
			if fi, ierr := d.Info(); ierr == nil {
				out = append(out, FileInfo{Path: rel, Size: fi.Size()})
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// ReadFile returns one catalog file's raw content so an agent can edit it and
// plan the change. It refuses a file that holds an obvious secret, because
// the bundle scan would refuse it coming back.
func (e *Engine) ReadFile(rel string) (string, error) {
	if err := readablePath(rel); err != nil {
		return "", err
	}
	if err := checkNoSymlinks(e.Root, rel); err != nil {
		return "", err
	}
	b, err := os.ReadFile(filepath.Join(e.Root, filepath.FromSlash(rel))) //nolint:gosec // G304: Root joined with an allowlist-validated relative path
	if errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("%s does not exist", rel)
	}
	if err != nil {
		return "", err
	}
	if len(b) > maxFileBytes {
		return "", fmt.Errorf("%s is larger than %d bytes", rel, maxFileBytes)
	}
	if why := ScanSecrets(string(b)); why != "" {
		return "", fmt.Errorf("%s is not shown: it %s; the operator edits it", rel, why)
	}
	return string(b), nil
}
