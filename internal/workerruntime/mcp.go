package workerruntime

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/hollis-labs/tether/internal/workerwire"
)

type mcpEntry struct {
	Type    string            `json:"type,omitempty"`
	URL     string            `json:"url,omitempty"`
	Command string            `json:"command,omitempty"`
	Args    []string          `json:"args"`
	Env     map[string]string `json:"env,omitempty"`
}
type mcpConfig struct {
	Servers map[string]mcpEntry `json:"mcpServers"`
}

func ImportHubMCP(root, source string) error {
	b, err := os.ReadFile(source) //nolint:gosec // G304: source is the operator-supplied --mcp-config path
	if err != nil {
		return err
	}
	var config mcpConfig
	if err = json.Unmarshal(b, &config); err != nil {
		return err
	}
	selected := mcpConfig{Servers: map[string]mcpEntry{}}
	for _, name := range []string{"torque", "tesseract", "tether"} {
		entry, ok := config.Servers[name]
		if !ok || entry.Command == "" {
			return fmt.Errorf("configured hub MCP entry %s required", name)
		}
		selected.Servers[name] = entry
	}
	// Any further entry (tangent, nanite, ...) is imported as configured; which sessions get it is decided
	// per launch by the hub's resolved mcp.servers grant.
	for name, entry := range config.Servers {
		if _, done := selected.Servers[name]; done {
			continue
		}
		if entry.Command == "" && entry.URL == "" {
			return fmt.Errorf("configured hub MCP entry %s needs a command or url", name)
		}
		selected.Servers[name] = entry
	}
	if err := validateHubMCP(selected); err != nil {
		return err
	}
	return SaveJSON(filepath.Join(root, "hub-mcp.json"), selected)
}
func validateHubMCP(config mcpConfig) error {
	entry, ok := config.Servers["tether"]
	if !ok || entry.Command != "ssh" {
		return fmt.Errorf("tether MCP must run on the hub over SSH")
	}
	for name, other := range config.Servers {
		for _, arg := range other.Args {
			if arg == "--proxy" {
				return fmt.Errorf("MCP entry %s: local/legacy Tether MCP proxy is not an outpost", name)
			}
		}
	}
	daemonOnly := false
	for _, arg := range entry.Args {
		if arg == "--daemon-only" {
			daemonOnly = true
		}
	}
	if !daemonOnly {
		return fmt.Errorf("hub Tether MCP requires --daemon-only")
	}
	return nil
}
func (e *Executor) plantMCP(cwd string, granted []string) error {
	b, err := os.ReadFile(filepath.Join(e.Root, "hub-mcp.json"))
	if err != nil {
		return err
	}
	var config mcpConfig
	if err = json.Unmarshal(b, &config); err != nil {
		return err
	}
	if err = validateHubMCP(config); err != nil {
		return err
	}
	out, err := json.Marshal(grantedOnly(config, granted))
	if err != nil {
		return err
	}
	return plant(cwd, ".mcp.json", out)
}

// grantedOnly keeps the tether entry (the session's own link to the hub) plus the entries named in granted.
// A nil granted keeps everything; granted names the hub does not define are skipped.
func grantedOnly(config mcpConfig, granted []string) mcpConfig {
	if granted == nil {
		return config
	}
	out := mcpConfig{Servers: map[string]mcpEntry{"tether": config.Servers["tether"]}}
	for _, name := range granted {
		if entry, ok := config.Servers[name]; ok {
			out.Servers[name] = entry
		}
	}
	return out
}

// plantHubMCP plants the hub MCP config. With a session credential the tether entry no longer runs ssh
// directly: it runs this worker's own "mcp-relay", which feeds the credential (read from a 0600 file kept
// outside the checkout) to the hub-side `tether mcp --token-stdin` over the ssh pipe. The credential is
// therefore never in any process argument or environment.
func (e *Executor) plantHubMCP(cwd, credDir string, spec workerwire.LaunchSpec) error {
	if spec.Credential == "" {
		return e.plantMCP(cwd, spec.MCPServers)
	}
	b, err := os.ReadFile(filepath.Join(e.Root, "hub-mcp.json"))
	if err != nil {
		return err
	}
	var config mcpConfig
	if err = json.Unmarshal(b, &config); err != nil {
		return err
	}
	if err = validateHubMCP(config); err != nil {
		return err
	}
	if err = os.MkdirAll(credDir, 0700); err != nil {
		return err
	}
	tokenFile := filepath.Join(credDir, "token")
	if err = os.WriteFile(tokenFile, []byte(spec.Credential+"\n"), 0600); err != nil {
		return err
	}
	config = grantedOnly(config, spec.MCPServers)
	entry := config.Servers["tether"]
	args := withScopes(entry.Args, spec.MCPScopes)
	entry.Args = append([]string{"mcp-relay", "--token-file", tokenFile, "--", entry.Command}, append(args, "--token-stdin")...)
	entry.Command = e.Binary
	config.Servers["tether"] = entry
	out, err := json.Marshal(config)
	if err != nil {
		return err
	}
	return plant(cwd, ".mcp.json", out)
}

// withScopes returns args with the --scopes value replaced by scopes (or appended when absent).
func withScopes(args, scopes []string) []string {
	if len(scopes) == 0 {
		return append([]string(nil), args...)
	}
	csv := strings.Join(scopes, ",")
	out := make([]string, 0, len(args)+2)
	replaced := false
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--scopes" && i+1 < len(args):
			out = append(out, "--scopes", csv)
			i++
			replaced = true
		case strings.HasPrefix(args[i], "--scopes="):
			out = append(out, "--scopes="+csv)
			replaced = true
		default:
			out = append(out, args[i])
		}
	}
	if !replaced {
		out = append(out, "--scopes", csv)
	}
	return out
}

// RunRelay execs argv (which must be ssh) and writes the credential from tokenFile as the first line of its
// stdin, then relays the MCP stdio stream unchanged.
func RunRelay(tokenFile string, argv []string, stdin io.Reader, stdout, stderr io.Writer) error {
	if len(argv) == 0 || argv[0] != "ssh" {
		return fmt.Errorf("mcp-relay only runs ssh")
	}
	token, err := os.ReadFile(tokenFile) //nolint:gosec // G304: path planted by this worker in the MCP config
	if err != nil {
		return err
	}
	cmd := exec.Command(argv[0], argv[1:]...) //nolint:gosec // G204: argv comes from the operator's hub-mcp.json; argv[0] is pinned to ssh
	cmd.Stdout, cmd.Stderr = stdout, stderr
	in, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	if err = cmd.Start(); err != nil {
		return err
	}
	line := strings.TrimSpace(string(token)) + "\n"
	go func() {
		defer func() { _ = in.Close() }()
		if _, err := io.WriteString(in, line); err != nil {
			return
		}
		_, _ = io.Copy(in, stdin)
	}()
	return cmd.Wait()
}
