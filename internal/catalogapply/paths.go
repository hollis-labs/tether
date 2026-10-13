package catalogapply

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
)

// allowedDirs are the catalog subdirectories a bundle may write into.
// credentials/ is deliberately absent: credential material is never authored
// through an agent-reachable tool.
var allowedDirs = map[string]bool{
	"agents":           true,
	"launches":         true,
	"projects":         true,
	"providers":        true,
	"skills":           true,
	"boot-profiles":    true,
	"mcp-servers":      true,
	"sandbox-profiles": true,
	"team-definitions": true,
}

// allowedFiles are single files at the catalog root a bundle may write.
var allowedFiles = map[string]bool{
	"global.yaml":       true,
	"teams-policy.yaml": true,
}

// privilegedPaths are files whose every change is flagged for the approver:
// they hold daemon defaults, upstream credentials references or sandbox rules.
func privilegedPath(p string) bool {
	if allowedFiles[p] {
		return p == "global.yaml"
	}
	dir := strings.SplitN(p, "/", 2)[0]
	return dir == "mcp-servers" || dir == "sandbox-profiles"
}

var allowedExts = map[string]bool{".yaml": true, ".yml": true, ".md": true}

const maxPathDepth = 3

// maxFileBytes bounds a single bundle file; catalog files are small text.
const maxFileBytes = 256 * 1024

// ValidatePath reports why p may not be written by a bundle, or nil when it
// may. p is slash-separated and relative to the catalog root.
func ValidatePath(p string) error {
	if p == "" {
		return fmt.Errorf("empty path")
	}
	if strings.ContainsAny(p, "\x00\\") {
		return fmt.Errorf("path %q contains a NUL or backslash", p)
	}
	if path.IsAbs(p) || strings.HasPrefix(p, "~") {
		return fmt.Errorf("path %q must be relative to the catalog root", p)
	}
	if path.Clean(p) != p {
		return fmt.Errorf("path %q is not in canonical form (no '.', '..', '//' or trailing '/')", p)
	}
	parts := strings.Split(p, "/")
	for _, seg := range parts {
		if seg == "" || seg == "." || seg == ".." || strings.HasPrefix(seg, ".") {
			return fmt.Errorf("path %q has a hidden or traversal segment", p)
		}
	}
	if len(parts) > maxPathDepth {
		return fmt.Errorf("path %q is nested deeper than %d segments", p, maxPathDepth)
	}
	if !allowedExts[strings.ToLower(path.Ext(p))] {
		return fmt.Errorf("path %q must end in .yaml, .yml or .md", p)
	}
	if len(parts) == 1 {
		if !allowedFiles[p] {
			return fmt.Errorf("path %q is not a catalog file a bundle may write (allowed root files: global.yaml, teams-policy.yaml)", p)
		}
		return nil
	}
	if !allowedDirs[parts[0]] {
		return fmt.Errorf("path %q is outside the directories a bundle may write", p)
	}
	return nil
}

// checkNoSymlinks refuses a destination whose existing path components
// (below root) are symlinks, so an install can never be steered out of the
// catalog by a link that is already there.
func checkNoSymlinks(root, rel string) error {
	cur := root
	for _, seg := range strings.Split(rel, "/") {
		cur = filepath.Join(cur, seg)
		fi, err := os.Lstat(cur)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%q is a symlink; a bundle does not write through links", rel)
		}
	}
	return nil
}

var secretValuePatterns = []*regexp.Regexp{
	regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`),
	regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{20,}`),
	regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{30,}`),
	regexp.MustCompile(`\bxox[abprs]-[A-Za-z0-9-]{10,}`),
	regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`),
	regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}`),
}

// secretKeyLine matches a YAML scalar assigned to a credential-looking key.
var secretKeyLine = regexp.MustCompile(`(?im)^\s*[-\s]*["']?[\w.-]*(token|secret|password|passwd|api[_-]?key|apikey|private[_-]?key|credential)[\w.-]*["']?\s*:\s*(\S.*)$`)

// ScanSecrets returns a description of the first obvious secret in content,
// or "" when none is found. It is a tripwire for plaintext credentials, not a
// guarantee: references (${VAR}, file paths named *_file/_ref/_env) pass.
func ScanSecrets(content string) string {
	for _, re := range secretValuePatterns {
		if re.MatchString(content) {
			return "matches a credential pattern (" + re.String() + ")"
		}
	}
	for _, m := range secretKeyLine.FindAllStringSubmatch(content, -1) {
		key := strings.ToLower(m[0])
		val := strings.TrimSpace(m[2])
		val = strings.Trim(val, `"'`)
		if i := strings.Index(val, " #"); i >= 0 {
			val = strings.TrimSpace(val[:i])
		}
		if val == "" || val == "|" || val == ">" || len(val) < 8 {
			continue
		}
		if strings.HasPrefix(val, "$") || strings.HasPrefix(val, "file:") || strings.HasPrefix(val, "env:") || strings.HasPrefix(val, "ref:") || strings.HasPrefix(val, "[") || strings.HasPrefix(val, "{") {
			continue
		}
		name := key[:strings.Index(key, ":")]
		if strings.HasSuffix(strings.TrimSpace(name), "_file") || strings.HasSuffix(strings.TrimSpace(name), "_ref") || strings.HasSuffix(strings.TrimSpace(name), "_env") || strings.HasSuffix(strings.TrimSpace(name), "_path") {
			continue
		}
		return "has a literal value for credential-like key " + strings.TrimSpace(name)
	}
	return ""
}
