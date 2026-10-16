package workerruntime

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
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
	Args    []string          `json:"args,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
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
		if err := validateHeaders(name, other); err != nil {
			return err
		}
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

// validateHeaders keeps credentials out of the planted .mcp.json: http/sse headers may only carry an
// environment reference (${VAR}), never an inline value, and stdio entries take no headers.
func validateHeaders(name string, e mcpEntry) error {
	if len(e.Headers) > 0 && e.URL == "" {
		return fmt.Errorf("MCP entry %s: headers apply only to http/sse entries with a url", name)
	}
	for h, v := range e.Headers {
		if !strings.Contains(v, "${") {
			return fmt.Errorf("MCP entry %s: header %s must reference an environment variable (${VAR}), not an inline value", name, h)
		}
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
	reason := e.hubHTTPUnavailable(spec)
	if reason == "" {
		return e.plantHubHTTP(cwd, credDir, spec)
	}
	if !e.AllowSSHFallback {
		return fmt.Errorf("hub MCP over /mcp is unavailable for this launch (%s) and the degraded ssh daemon-only path is not enabled (set mcp_ssh_fallback in the worker config)", reason)
	}
	log.Printf("session %s: DEGRADED hub MCP over ssh daemon-only (no session binding, saved policy or proxy capture): %s", spec.Session, reason)
	if spec.Credential == "" {
		if spec.ExtractRefs {
			return fmt.Errorf("extract_refs requires a session credential")
		}
		if len(spec.EditableAgents) > 0 {
			return fmt.Errorf("editable_agents needs a session credential: the static MCP entry cannot carry the allowlist")
		}
		return e.plantMCP(cwd, spec.MCPServers)
	}
	// SSH joins argv into a remote shell command. Session ids are opaque tokens,
	// not shell text; refuse malformed specs before planting or writing secrets.
	if spec.Session == "" || strings.IndexFunc(spec.Session, func(r rune) bool {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			return false
		default:
			return true
		}
	}) >= 0 {
		return fmt.Errorf("MCP relay requires a shell-safe session id")
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
	for _, id := range spec.EditableAgents {
		args = append(args, "--editable-agent", id)
	}
	// Last flag wins, including a stale session/extraction flag in the imported
	// operator template. Explicit false preserves the hub's default-off policy.
	args = append(args, "--session", spec.Session, fmt.Sprintf("--extract-refs=%t", spec.ExtractRefs))
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

// ValidateHubMCPFile checks the planted hub-mcp.json the way a launch will, so a bad edit is reported
// when the worker starts rather than as an opaque launch failure.
func ValidateHubMCPFile(root string) error {
	b, err := os.ReadFile(filepath.Join(root, "hub-mcp.json")) //nolint:gosec // G304: root is the worker's own state directory
	if err != nil {
		return err
	}
	var config mcpConfig
	if err = json.Unmarshal(b, &config); err != nil {
		return fmt.Errorf("parse hub-mcp.json: %w", err)
	}
	if len(config.Servers) == 0 {
		return nil // freshly enrolled placeholder; ImportHubMCP fills it in
	}
	if err = validateHubMCP(config); err != nil {
		return fmt.Errorf("hub-mcp.json: %w", err)
	}
	return nil
}

// sessionAgentTokenFile is where the outpost keeps the session's aud=tether agent token, in the auth directory beside
// the mcp-cred directory (startAgentTokens writes it before the agent starts and rewrites it before it expires).
func sessionAgentTokenFile(credDir string) string {
	return TokenFilePath(filepath.Join(filepath.Dir(credDir), "auth"), "tether")
}

// hubHTTPUnavailable says why this launch cannot reach the hub's /mcp with the session's agent token, or "" when it can.
func (e *Executor) hubHTTPUnavailable(spec workerwire.LaunchSpec) string {
	hasToken := false
	for _, t := range spec.AgentTokens {
		hasToken = hasToken || t.Aud == "tether"
	}
	switch {
	case e.HubMCPURL == "":
		return "no hub MCP url configured"
	case e.Tokens == nil:
		return "this worker cannot mint agent tokens"
	case !hasToken:
		return "the hub did not grant a session agent token"
	case len(spec.EditableAgents) > 0:
		// The editable_agents allowlist is enforced by a flag on the ssh-launched 'tether mcp'; the hub gateway does
		// not carry it yet, so it would silently disappear. Refuse rather than widen.
		return "the agent declares editable_agents, which /mcp does not enforce yet"
	case (&HTTPRelay{URL: e.HubMCPURL}).validate() != nil:
		return "the hub MCP url is not https"
	}
	return ""
}

// plantHubHTTP plants the tether entry as this worker's own mcp-http-relay against the hub's /mcp, authenticated with
// the session's agent token file. No credential is in an argument or the environment; the hub applies the session's
// saved policy, authority fingerprint, limits and proxy capture.
func (e *Executor) plantHubHTTP(cwd, credDir string, spec workerwire.LaunchSpec) error {
	b, err := os.ReadFile(filepath.Join(e.Root, "hub-mcp.json")) //nolint:gosec // G304: the worker's own state directory
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
	config = grantedOnly(config, spec.MCPServers)
	config.Servers["tether"] = mcpEntry{Command: e.Binary, Args: []string{"mcp-http-relay", "--url", e.HubMCPURL, "--token-file", sessionAgentTokenFile(credDir)}}
	out, err := json.Marshal(config)
	if err != nil {
		return err
	}
	return plant(cwd, ".mcp.json", out)
}
