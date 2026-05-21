package main

// registry.go — CLI surface for the federation directory service
// (T-v060-01-07). Mirrors the HTTP routes via the typed
// *client.RegistryClient; no filesystem reads bypass the daemon.
//
// Subcommands implemented here:
//
//	mux registry register   --kind {agent|project} --file <path> [--print-urn-only]
//	mux registry lookup     <urn> [--json]
//	mux registry search     --kind {agent|project} [filters...] [--json]
//	mux registry update-self <urn> --file <patch-file>
//	mux registry deregister <urn>
//	mux registry sync       <urn>
//
// The `bootstrap` subcommand lands in T-v060-01-08 and is intentionally
// absent here — adding it now would ship a stub that gets rewritten
// during the next task.
//
// Exit-code wiring uses the `exitCoder` interface checked by main.go:
// each RunE wraps its error in `exitErr{code, err}` so main.go can
// classify the failure without inspecting error chains across the
// binary. The mapping (per the sprint acceptance criterion):
//
//	0 success
//	1 registry.ErrNotFound
//	2 registry.ErrInvalidRequest / argument-validation failure
//	3 client.ErrDaemonUnreachable
//	4 anything else
//
// The CLI is wired through a small `registryClientFactory` indirection
// so tests can inject a httptest-backed client without spinning up a
// real daemon. Production code falls through to `newDaemonClient`.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
	"unicode"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/hollis-labs/tether/internal/client"
	"github.com/hollis-labs/tether/internal/registry"
)

// ─── exit-code wrapper ───────────────────────────────────────────────────────

// exitErr carries a desired process exit code through main.go via the
// `exitCoder` interface declared there. The CLI never calls os.Exit
// directly so Cobra's RunE contract stays intact (RunE returns error,
// main.go translates).
type exitErr struct {
	code int
	err  error
}

func (e *exitErr) Error() string {
	if e.err == nil {
		return ""
	}
	return e.err.Error()
}

func (e *exitErr) Unwrap() error { return e.err }

// ExitCode satisfies the unexported `exitCoder` interface in main.go.
func (e *exitErr) ExitCode() int { return e.code }

// classifyErr maps an error to the documented exit code. Callers
// always wrap their final error through this so the table stays in one
// place. Nil propagates unchanged so happy paths don't allocate.
func classifyErr(err error) error {
	if err == nil {
		return nil
	}
	switch {
	case errors.Is(err, registry.ErrNotFound):
		return &exitErr{code: 1, err: err}
	case errors.Is(err, registry.ErrInvalidRequest):
		return &exitErr{code: 2, err: err}
	case errors.Is(err, client.ErrDaemonUnreachable):
		return &exitErr{code: 3, err: err}
	default:
		return &exitErr{code: 4, err: err}
	}
}

// validationErr wraps a CLI-level validation failure (missing flag,
// bad file content, etc.) at exit code 2. Sprint spec lumps these
// together with service-side validation under the same code so the
// caller's branching stays simple.
func validationErr(format string, args ...any) error {
	return &exitErr{code: 2, err: fmt.Errorf(format, args...)}
}

// ─── client factory (test seam) ─────────────────────────────────────────────

// registryClientFactory builds a *client.RegistryClient for the active
// command. Production path wires it from the catalog; tests override
// the variable to plug in a httptest-backed Client.
//
// Returning *client.Client (not the typed RegistryClient) lets us
// share the same factory whether the caller wants Registry() or any
// other typed accessor we add later.
var registryClientFactory = defaultRegistryClientFactory

func defaultRegistryClientFactory() (*client.Client, error) {
	return newDaemonClient(catalogPath)
}

func registryClient() (*client.RegistryClient, error) {
	c, err := registryClientFactory()
	if err != nil {
		return nil, err
	}
	return c.Registry(), nil
}

// ─── parent command ─────────────────────────────────────────────────────────

var registryCmd = &cobra.Command{
	Use:   "registry",
	Short: "Federation directory service (Register / Lookup / Search / UpdateSelf / Deregister / Sync)",
	Long: `Operate against the Mux federation directory.

The directory is the cross-substrate identity catalog: each row is a
public-identity Profile for an agent or project, addressable by a
canonical msg:// URN. Operational config stays in the owning
substrate's ops store (cerberus, agridd, etc.); the registry holds a
thin profile and a callback URI back to that store.

All subcommands route through the daemon's typed client; nothing here
reads or writes catalog YAML on disk directly. The daemon is the
single writer.`,
}

// ─── register ───────────────────────────────────────────────────────────────

var (
	registerKind     string
	registerFile     string
	registerPrintURN bool
)

var registryRegisterCmd = &cobra.Command{
	Use:   "register",
	Short: "Register a new agent or project Profile",
	Long: `Mint a new directory entry from a Profile file.

The server ignores any caller-supplied URN, ID, MuxInstanceID, or
created/updated timestamps and mints a fresh one (D2). The --file
argument accepts either YAML or JSON; the format is sniffed from the
first non-whitespace byte ('{' or '[' → JSON, anything else → YAML).

Output:
  default            pretty-printed canonical Profile
  --print-urn-only   only the minted URN (newline-terminated, pipeable)`,
	RunE: func(cmd *cobra.Command, args []string) error {
		if registerKind == "" {
			return validationErr("registry register: --kind is required")
		}
		kind := registry.Kind(registerKind)
		if kind != registry.KindAgent && kind != registry.KindProject {
			return validationErr("registry register: --kind must be 'agent' or 'project', got %q", registerKind)
		}
		if registerFile == "" {
			return validationErr("registry register: --file is required")
		}

		var profile registry.Profile
		if err := readDocFile(registerFile, &profile); err != nil {
			return validationErr("registry register: read %s: %v", registerFile, err)
		}

		rc, err := registryClient()
		if err != nil {
			return classifyErr(err)
		}
		out, err := rc.Register(cmdCtx(cmd), kind, profile)
		if err != nil {
			return classifyErr(err)
		}
		if registerPrintURN {
			fmt.Println(out.URN)
			return nil
		}
		printProfile(out)
		return nil
	},
}

// ─── lookup ─────────────────────────────────────────────────────────────────

var lookupJSON bool

var registryLookupCmd = &cobra.Command{
	Use:   "lookup <urn>",
	Short: "Look up a Profile by URN",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		urn := args[0]
		rc, err := registryClient()
		if err != nil {
			return classifyErr(err)
		}
		out, err := rc.Lookup(cmdCtx(cmd), urn)
		if err != nil {
			if errors.Is(err, registry.ErrNotFound) {
				fmt.Fprintf(os.Stderr, "registry: not found: %s\n", urn)
				return &exitErr{code: 1, err: err}
			}
			return classifyErr(err)
		}
		if lookupJSON {
			return printJSON(out)
		}
		printProfile(out)
		return nil
	},
}

// ─── search ─────────────────────────────────────────────────────────────────

var (
	searchKind       string
	searchRole       string
	searchTitle      string
	searchProject    string
	searchCapability string
	searchSkillName  string
	searchStatus     string
	searchJSON       bool
)

var registrySearchCmd = &cobra.Command{
	Use:   "search",
	Short: "Search the registry by filter-AND",
	Long: `List directory entries matching the given filters.

Filters combine with AND (D6). Default ordering is alphabetical on
display_name (server-side). Deprecated rows are hidden by default; use
--status deprecated to include them.

Output:
  default   one row per line — "<urn>  <display_name>  [role=X status=Y]"
  --json    raw JSON array`,
	RunE: func(cmd *cobra.Command, args []string) error {
		if searchKind == "" {
			return validationErr("registry search: --kind is required")
		}
		kind := registry.Kind(searchKind)
		if kind != registry.KindAgent && kind != registry.KindProject {
			return validationErr("registry search: --kind must be 'agent' or 'project', got %q", searchKind)
		}
		filter := registry.Filter{
			Role:       searchRole,
			Title:      searchTitle,
			Project:    searchProject,
			Capability: searchCapability,
			SkillName:  searchSkillName,
			Status:     searchStatus,
		}
		rc, err := registryClient()
		if err != nil {
			return classifyErr(err)
		}
		rows, err := rc.Search(cmdCtx(cmd), kind, filter)
		if err != nil {
			return classifyErr(err)
		}
		if searchJSON {
			return printJSON(rows)
		}
		for _, p := range rows {
			role := p.Role
			if role == "" {
				role = "-"
			}
			fmt.Printf("%s  %s  [role=%s status=%s]\n", p.URN, p.DisplayName, role, p.Status)
		}
		return nil
	},
}

// ─── update-self ────────────────────────────────────────────────────────────

var updateSelfFile string

var registryUpdateSelfCmd = &cobra.Command{
	Use:   "update-self <urn>",
	Short: "Partial-merge update of a Profile",
	Long: `Apply a partial-merge patch to the Profile identified by <urn>.

The --file argument is a YAML or JSON document containing an
UpdatePatch. The format is sniffed from the first non-whitespace byte
('{' or '[' → JSON, anything else → YAML).

Partial-merge semantics (D5):

  Scalar fields update column-wise. A field whose pointer is non-nil
  is set (including pointer-to-empty-string = explicit clear); a field
  absent from the patch (nil pointer) is left alone.

  Array fields (capabilities, skills, links) accept two shapes:

    Shorthand    [a, b, c]                              ≡ replace
    Explicit     {mode: replace|append|remove, value: [...]}

  Remove matches: capabilities by string equality, skills by name,
  links by (kind, target). An empty value is a no-op on every mode.

  last_updated_by is REQUIRED on every patch.

  kind_meta is replace-on-present (no shallow merge in v1).`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		urn := args[0]
		if updateSelfFile == "" {
			return validationErr("registry update-self: --file is required")
		}
		var patch registry.UpdatePatch
		if err := readDocFile(updateSelfFile, &patch); err != nil {
			return validationErr("registry update-self: read %s: %v", updateSelfFile, err)
		}
		// Pre-flight: server returns 400 if last_updated_by is empty,
		// but surfacing it locally gives a faster, clearer error.
		if strings.TrimSpace(patch.LastUpdatedBy) == "" {
			return validationErr("registry update-self: last_updated_by is required in the patch")
		}
		rc, err := registryClient()
		if err != nil {
			return classifyErr(err)
		}
		out, err := rc.UpdateSelf(cmdCtx(cmd), urn, patch)
		if err != nil {
			return classifyErr(err)
		}
		printProfile(out)
		return nil
	},
}

// ─── deregister ─────────────────────────────────────────────────────────────

var registryDeregisterCmd = &cobra.Command{
	Use:   "deregister <urn>",
	Short: "Soft-delete a Profile (status → deprecated)",
	Long: `Mark the Profile as deprecated. The row stays for audit and
foreign-references (D11); subsequent lookups still return it with
status='deprecated'. Search excludes deprecated rows by default;
'mux registry search --status deprecated' surfaces them.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		urn := args[0]
		rc, err := registryClient()
		if err != nil {
			return classifyErr(err)
		}
		out, err := rc.Deregister(cmdCtx(cmd), urn)
		if err != nil {
			return classifyErr(err)
		}
		fmt.Printf("deregistered: %s  status=%s\n", out.URN, out.Status)
		return nil
	},
}

// ─── bootstrap ──────────────────────────────────────────────────────────────

var bootstrapForce bool

var registryBootstrapCmd = &cobra.Command{
	Use:   "bootstrap",
	Short: "Re-run the catalog bootstrap importer (use --force after editing catalog YAMLs)",
	Long: `Re-imports ~/.tether/catalog/{agents,projects}/*.yaml into the
federation directory.

Idempotent: existing rows (matched by callback.target) are skipped on
re-run unless --force is passed. With --force, the existing row is
patched with the YAML's current thin profile and cached_at is bumped.

This command runs through the daemon's existing service (no separate
process). The daemon's auto-bootstrap on startup is non-forced; use this
to apply catalog drift after editing a YAML.

Per-file failures (malformed YAML, etc.) are reported as part of the
output; one bad file does not abort the rest of the bootstrap.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		rc, err := registryClient()
		if err != nil {
			return classifyErr(err)
		}
		report, err := rc.Bootstrap(cmdCtx(cmd), bootstrapForce)
		if err != nil {
			return classifyErr(err)
		}
		fmt.Printf("imported:  %d\nskipped:   %d\nrefreshed: %d\nerrors:    %d\n",
			report.Imported, report.Skipped, report.Refreshed, len(report.Errors))
		for _, e := range report.Errors {
			fmt.Printf("  error %s: %s\n", e.Path, e.Reason)
		}
		return nil
	},
}

// ─── sync ───────────────────────────────────────────────────────────────────

var registrySyncCmd = &cobra.Command{
	Use:   "sync <urn>",
	Short: "Refresh a Profile from its callback URI",
	Long: `Invoke the row's callback (file:// or cli:// in v1), parse the
payload, and refresh the thin-profile columns + cached_at.

A row with no callback configured is a no-op — the command prints
"no-op: <urn> has no callback" and exits 0. Raw payload is never
stored in the daemon (D18); callers needing full content read it
directly from the callback URI.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		urn := args[0]
		rc, err := registryClient()
		if err != nil {
			return classifyErr(err)
		}
		out, synced, err := rc.Sync(cmdCtx(cmd), urn)
		if err != nil {
			return classifyErr(err)
		}
		if !synced {
			fmt.Printf("no-op: %s has no callback\n", urn)
			return nil
		}
		printProfile(out)
		return nil
	},
}

// ─── pretty-printer ─────────────────────────────────────────────────────────

// printProfile writes a stable, line-oriented rendering of a Profile.
// Mirrors `mux sessions get`'s look: one field per line, label-left,
// value-right. Optional fields are elided when zero/nil so callers
// only see what's actually set.
func printProfile(p registry.Profile) {
	fmt.Printf("urn:             %s\n", p.URN)
	fmt.Printf("kind:            %s\n", p.Kind)
	fmt.Printf("display_name:    %s\n", p.DisplayName)
	if p.Title != "" {
		fmt.Printf("title:           %s\n", p.Title)
	}
	if p.Role != "" {
		fmt.Printf("role:            %s\n", p.Role)
	}
	fmt.Printf("status:          %s\n", p.Status)
	if p.Description != "" {
		fmt.Printf("description:     %s\n", p.Description)
	}
	fmt.Printf("mux_instance:    %s\n", p.MuxInstanceID)
	if p.HostAddress != "" {
		fmt.Printf("host_address:    %s\n", p.HostAddress)
	}
	if p.HealthStatus != "" {
		fmt.Printf("health_status:   %s\n", p.HealthStatus)
	}
	if p.LastSeenAt != nil {
		fmt.Printf("last_seen_at:    %s\n", p.LastSeenAt.UTC().Format(time.RFC3339))
	}
	if p.CachedAt != nil {
		fmt.Printf("cached_at:       %s\n", p.CachedAt.UTC().Format(time.RFC3339))
	}
	if p.LastUpdatedBy != "" {
		fmt.Printf("last_updated_by: %s\n", p.LastUpdatedBy)
	}
	fmt.Printf("created_at:      %s\n", p.CreatedAt.UTC().Format(time.RFC3339))
	fmt.Printf("updated_at:      %s\n", p.UpdatedAt.UTC().Format(time.RFC3339))
	if p.Callback != nil {
		fmt.Printf("callback:        %s://%s\n", p.Callback.Scheme, trimSchemePrefix(p.Callback.Target))
	}
	if len(p.Capabilities) > 0 {
		fmt.Printf("capabilities:    [%s]\n", strings.Join(p.Capabilities, ", "))
	}
	if len(p.Skills) > 0 {
		fmt.Println("skills:")
		for _, s := range p.Skills {
			fmt.Printf("  - name:        %s\n", s.Name)
			fmt.Printf("    learned_at:  %s\n", s.LearnedAt.UTC().Format(time.RFC3339))
			if s.Via != "" {
				fmt.Printf("    via:         %s\n", s.Via)
			}
			if s.Level != "" {
				fmt.Printf("    level:       %s\n", s.Level)
			}
		}
	}
	if len(p.Links) > 0 {
		fmt.Println("links:")
		for _, l := range p.Links {
			fmt.Printf("  - kind:        %s\n", l.Kind)
			fmt.Printf("    target:      %s\n", l.Target)
		}
	}
	if len(p.KindMeta) > 0 {
		// The raw json.RawMessage is already canonical JSON; print
		// inline so structured kind-meta stays compact rather than
		// pretty-expanding it into multiple lines.
		fmt.Printf("kind_meta:       %s\n", string(p.KindMeta))
	}
}

// trimSchemePrefix strips a leading "scheme://" from a target if it's
// present so the printed callback line doesn't double the scheme
// (Callback.Target may either be a bare target like /tmp/x.yaml or a
// fully-qualified file:///tmp/x.yaml).
func trimSchemePrefix(target string) string {
	if i := strings.Index(target, "://"); i >= 0 {
		return target[i+3:]
	}
	return target
}

// printJSON marshals v to stdout with two-space indentation. Used by
// --json output modes.
func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return classifyErr(fmt.Errorf("encode json: %w", err))
	}
	return nil
}

// ─── file reader (YAML or JSON sniff) ───────────────────────────────────────

// readDocFile reads path and unmarshals into out. Format is sniffed
// from the first non-whitespace byte: '{' or '[' → JSON, anything else
// → YAML. YAML is round-tripped through a generic decode + JSON
// re-marshal so the target's json tags are honored (yaml.v3 reads its
// own tags only).
func readDocFile(path string, out any) error {
	b, err := os.ReadFile(path) //nolint:gosec // G304: operator-supplied path is the whole point of --file
	if err != nil {
		return err
	}
	if isJSON(b) {
		return json.Unmarshal(b, out)
	}
	return yamlToJSONUnmarshal(b, out)
}

// isJSON returns true when the first non-whitespace byte is '{' or
// '['. Anything else (including an empty document or a leading
// comment) falls through to YAML.
func isJSON(b []byte) bool {
	for _, c := range b {
		if unicode.IsSpace(rune(c)) {
			continue
		}
		return c == '{' || c == '['
	}
	return false
}

// yamlToJSONUnmarshal decodes b as YAML into an untyped map, marshals
// that to JSON, and unmarshals the JSON into out. Cheap enough for
// CLI input documents — these are typically <1 KiB.
func yamlToJSONUnmarshal(b []byte, out any) error {
	var raw any
	if err := yaml.Unmarshal(b, &raw); err != nil {
		return fmt.Errorf("parse yaml: %w", err)
	}
	// yaml.v3 returns map[interface{}]interface{} for nested maps,
	// which encoding/json refuses to marshal. Normalize to
	// map[string]any first.
	raw = normalizeYAMLValue(raw)
	j, err := json.Marshal(raw)
	if err != nil {
		return fmt.Errorf("yaml→json: %w", err)
	}
	if err := json.Unmarshal(j, out); err != nil {
		return fmt.Errorf("decode: %w", err)
	}
	return nil
}

// normalizeYAMLValue walks a YAML-decoded value and rewrites
// map[interface{}]interface{} nodes to map[string]any so the
// result is encoding/json-safe. yaml.v3 already returns
// map[string]any for documents decoded into `any`, but legacy
// shapes (older drafts, structs) can still surface
// map[interface{}]interface{}, so the walk is defensive.
func normalizeYAMLValue(v any) any {
	switch x := v.(type) {
	case map[any]any:
		m := make(map[string]any, len(x))
		for k, val := range x {
			m[fmt.Sprintf("%v", k)] = normalizeYAMLValue(val)
		}
		return m
	case map[string]any:
		for k, val := range x {
			x[k] = normalizeYAMLValue(val)
		}
		return x
	case []any:
		for i, val := range x {
			x[i] = normalizeYAMLValue(val)
		}
		return x
	default:
		return v
	}
}

// ─── wiring ─────────────────────────────────────────────────────────────────

// resetRegistryFlags clears every command-scoped flag back to its
// zero value. Tests call this between runs because cobra flag state
// is package-global and would otherwise bleed across cases.
func resetRegistryFlags() {
	registerKind = ""
	registerFile = ""
	registerPrintURN = false
	lookupJSON = false
	searchKind = ""
	searchRole = ""
	searchTitle = ""
	searchProject = ""
	searchCapability = ""
	searchSkillName = ""
	searchStatus = ""
	searchJSON = false
	updateSelfFile = ""
	bootstrapForce = false
}

// cmdCtx returns cmd.Context() if non-nil, otherwise
// context.Background(). cobra populates the context only when
// Execute() runs; the per-test path (calling RunE directly) leaves
// it nil, which trips net/http's "nil Context" guard. This shim
// keeps both paths honest without forcing tests to construct full
// cobra Command graphs.
func cmdCtx(cmd *cobra.Command) context.Context {
	if cmd == nil {
		return context.Background()
	}
	if ctx := cmd.Context(); ctx != nil {
		return ctx
	}
	return context.Background()
}

func init() {
	registryRegisterCmd.Flags().StringVar(&registerKind, "kind", "", "registry kind ('agent' or 'project')")
	registryRegisterCmd.Flags().StringVar(&registerFile, "file", "", "path to a YAML or JSON Profile document")
	registryRegisterCmd.Flags().BoolVar(&registerPrintURN, "print-urn-only", false, "emit only the minted URN on stdout (pipeable)")

	registryLookupCmd.Flags().BoolVar(&lookupJSON, "json", false, "emit raw JSON instead of the pretty rendering")

	registrySearchCmd.Flags().StringVar(&searchKind, "kind", "", "registry kind ('agent' or 'project')")
	registrySearchCmd.Flags().StringVar(&searchRole, "role", "", "filter by role")
	registrySearchCmd.Flags().StringVar(&searchTitle, "title", "", "filter by title")
	registrySearchCmd.Flags().StringVar(&searchProject, "project", "", "filter by project")
	registrySearchCmd.Flags().StringVar(&searchCapability, "capability", "", "filter by capability membership")
	registrySearchCmd.Flags().StringVar(&searchSkillName, "skill-name", "", "filter by skill name membership")
	registrySearchCmd.Flags().StringVar(&searchStatus, "status", "", "filter by status ('active' (default) or 'deprecated')")
	registrySearchCmd.Flags().BoolVar(&searchJSON, "json", false, "emit raw JSON array instead of the line-per-row rendering")

	registryUpdateSelfCmd.Flags().StringVar(&updateSelfFile, "file", "", "path to a YAML or JSON UpdatePatch document")

	registryBootstrapCmd.Flags().BoolVar(&bootstrapForce, "force", false, "patch existing rows with the catalog YAMLs' current thin profile + bump cached_at")

	registryCmd.AddCommand(
		registryRegisterCmd,
		registryLookupCmd,
		registrySearchCmd,
		registryUpdateSelfCmd,
		registryDeregisterCmd,
		registrySyncCmd,
		registryBootstrapCmd,
	)
	rootCmd.AddCommand(registryCmd)
}
