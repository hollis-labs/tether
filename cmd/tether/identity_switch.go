package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/hollis-labs/tether/internal/client"
	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

// The verbs below edit global.yaml and append to run/identity-flips.jsonl. The
// daemon reads the mode at start, so a change applies on the next restart.
//
// Operator boundary (CW-20261010-0268): these verbs carry no authentication and
// run as the invoking uid. Tether write-protects the catalog and run/ only for
// agents it wraps (Claude, OpenCode), not Codex (CW-20261001-0230) and not
// against processes outside Tether, and the run dir is readable by agents until
// CW-20261001-0263. The session guard below is a convenience that stops a
// Tether-launched session from flipping by accident; it is not a security
// boundary, since a process can unset its environment.

func newIdentityCommand() *cobra.Command {
	root := &cobra.Command{Use: "identity", Short: "Show, flip and roll back identity enforcement modes"}
	root.AddCommand(identityModeCmd(), identityFlipCmd(), identityRollbackCmd(), identityRotateOperatorCmd(), identityUnboundedCmd())
	return root
}

// refuseInsideSession rejects invocation from a Tether-launched session. Tether
// plants TETHER_MCP_TOKEN (the adapter marker) and TETHER_TOKEN in every session
// runtime and MCP environment (internal/app/session_lifecycle.go, boot.go).
func refuseInsideSession() error {
	for _, k := range []string{"TETHER_MCP_TOKEN", "TETHER_TOKEN"} {
		if os.Getenv(k) != "" {
			return fmt.Errorf("refusing to change identity mode: %s is set, which looks like a Tether-launched session or an exported credential; run this as the operator in a plain shell (these verbs do not use tokens)", k)
		}
	}
	return nil
}

func flipLogPath(catalogRoot string) string {
	return filepath.Join(filepath.Dir(catalogRoot), "run", "identity-flips.jsonl")
}

// identityModeState is the configured mode per target; Running is filled when a
// daemon answers /health and holds only what the daemon reports.
type identityModeState struct {
	Configured map[string]string // "global" plus each config.IdentityPoints name
	Running    map[string]string // nil when the daemon is unreachable
}

func (s identityModeState) pendingRestart() []string {
	var out []string
	if s.Running == nil {
		return nil
	}
	for k, v := range s.Configured {
		if s.Running[k] != v {
			out = append(out, k)
		}
	}
	slices.Sort(out)
	return out
}

func configuredIdentityModes(g config.IdentityConfig) map[string]string {
	out := map[string]string{"global": g.EffectiveMode()}
	for _, p := range config.IdentityPoints {
		out[p] = g.ModeFor(p)
	}
	return out
}

func loadIdentityModeState(cat *config.Catalog, withRuntime bool) identityModeState {
	st := identityModeState{Configured: configuredIdentityModes(cat.Global.Identity)}
	if !withRuntime {
		return st
	}
	cfg, err := daemonConfigFromCatalog(cat)
	if err != nil {
		return st
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	h, err := client.New(cfg.ListenAddr, client.WithToken("")).Health(ctx)
	if err != nil || h.Identity == nil {
		return st
	}
	st.Running = map[string]string{"global": string(h.Identity.Mode)}
	for _, p := range config.IdentityPoints {
		m := h.Identity.Mode
		if o, ok := h.Identity.Points[p]; ok && o != "" {
			m = o
		}
		st.Running[p] = string(m)
	}
	return st
}

func identityModeCmd() *cobra.Command {
	return &cobra.Command{
		Use: "mode", Short: "Show configured and running identity mode per point, restart-pending changes and the verified flip history", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			root := config.Expand(catalogPath)
			cat, err := config.Load(root)
			if err != nil {
				return fmt.Errorf("load config: %w", err)
			}
			w := cmd.OutOrStdout()
			st := loadIdentityModeState(cat, true)
			for _, k := range append([]string{"global"}, config.IdentityPoints...) {
				run := "daemon not reachable"
				if st.Running != nil {
					run = "running " + st.Running[k]
				}
				fmt.Fprintf(w, "%-12s configured %-8s %s\n", k, st.Configured[k], run)
			}
			if p := st.pendingRestart(); len(p) > 0 {
				fmt.Fprintf(w, "RESTART PENDING for: %s\n", strings.Join(p, ", "))
			}
			recs, head, verr := verifyFlipLog(flipLogPath(root))
			for _, r := range recs {
				fmt.Fprintf(w, "%s %s %s %s->%s by %s (unauthenticated label)\n", r.At, r.Action, r.Point, r.From, r.To, r.By)
			}
			fmt.Fprintf(w, "flip log head %s (%d records); pin it in the GO record\n", head, len(recs))
			if verr != nil {
				fmt.Fprintf(w, "FLIP LOG INTEGRITY FAILURE: %v\n", verr)
				return verr
			}
			return nil
		},
	}
}

func identityFlipCmd() *cobra.Command {
	var point, to, by, evidence string
	var global, revokeAnonymous bool
	cmd := &cobra.Command{
		Use:   "flip (--point NAME | --global) --to MODE --evidence TEXT [--revoke-anonymous]",
		Short: "Set the identity mode of one enforcement point or globally, with an audit record",
		Long: "Edits identity.mode (--global) or identity.points.NAME in global.yaml and appends a hash-chained record to run/identity-flips.jsonl.\n" +
			"Points: " + strings.Join(config.IdentityPoints, ", ") + ". Restart the daemon to apply. --evidence is required (task ids, owner GO reference, review output path).\n" +
			"--by is an unauthenticated label. Refuses inside a Tether-launched session.\n" +
			"Switching anything to enforce is refused while live session credentials exist that were launched with no bounding authority (see `tether identity unbounded`); --revoke-anonymous revokes exactly that list first.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := refuseInsideSession(); err != nil {
				return err
			}
			if global == (point != "") {
				return fmt.Errorf("give exactly one of --point or --global")
			}
			if strings.TrimSpace(evidence) == "" {
				return fmt.Errorf("--evidence is required")
			}
			if global {
				point = "global"
			}
			mode := identity.Mode(strings.ToLower(strings.TrimSpace(to)))
			if mode == "" || mode.Validate() != nil {
				return fmt.Errorf("invalid --to %q (want off, observe or enforce)", to)
			}
			rec, err := flipMode(cmd.Context(), cmd.OutOrStdout(), config.Expand(catalogPath), point, mode, by, evidence, revokeAnonymous)
			if err != nil {
				return err
			}
			return printFlip(cmd.OutOrStdout(), rec)
		},
	}
	cmd.Flags().StringVar(&point, "point", "", "enforcement point: "+strings.Join(config.IdentityPoints, ", "))
	cmd.Flags().BoolVar(&global, "global", false, "change identity.mode")
	cmd.Flags().StringVar(&to, "to", "", "off, observe or enforce")
	cmd.Flags().StringVar(&by, "by", "", "label recorded as who flipped (default: OS user; not authenticated)")
	cmd.Flags().StringVar(&evidence, "evidence", "", "review evidence and GO reference (required)")
	cmd.Flags().BoolVar(&revokeAnonymous, "revoke-anonymous", false, "when switching to enforce, revoke the live session credentials that were launched with no bounding authority instead of refusing")
	_ = cmd.MarkFlagRequired("to")
	return cmd
}

// flipMode is the flip verb's body: a switch to enforce first passes the
// unbounded-credential guard, and a revocation it performs is recorded in the
// flip's evidence.
func flipMode(ctx context.Context, w io.Writer, catalogRoot, point string, mode identity.Mode, by, evidence string, revokeAnonymous bool) (*flipRecord, error) {
	if mode == identity.Enforce {
		n, err := enforceGuard(ctx, w, catalogRoot, revokeAnonymous)
		if err != nil {
			return nil, err
		}
		if n > 0 {
			evidence = fmt.Sprintf("%s (revoked %d unbounded session credentials)", evidence, n)
		}
	}
	return applyModeChange(catalogRoot, "flip", point, string(mode), by, evidence)
}

func identityRollbackCmd() *cobra.Command {
	var point, by, evidence string
	var toObserve, revokeAnonymous bool
	cmd := &cobra.Command{
		Use:   "rollback [--point NAME] [--observe] [--revoke-anonymous]",
		Short: "Undo flips: restore each target to its recorded prior state (value and whether an override existed)",
		Long: "Replays the verified flip log and undoes, newest first, every flip not yet rolled back (all targets, or --point only), restoring the prior value and removing overrides that did not exist. Targets with nothing pending are untouched.\n" +
			"--observe is the break-glass form: it sets the target (or the global mode and every point) to an explicit observe regardless of history, which also weakens points that were enforcing before the flip log began.\n" +
			"Restart the daemon to apply. Refuses inside a Tether-launched session or on a broken flip log.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := refuseInsideSession(); err != nil {
				return err
			}
			if strings.TrimSpace(evidence) == "" {
				evidence = "rollback"
			}
			root := config.Expand(catalogPath)
			recs, _, err := verifyFlipLog(flipLogPath(root))
			if err != nil {
				return err
			}
			targets := []string{point}
			if point == "" {
				targets = append([]string{"global"}, config.IdentityPoints...)
			} else if point != "global" && !slices.Contains(config.IdentityPoints, point) {
				return fmt.Errorf("unknown point %q", point)
			}
			pending := pendingFlips(recs)
			did := 0
			for _, t := range targets {
				var rec *flipRecord
				switch {
				case toObserve:
					rec, err = applyModeChange(root, "reset", t, string(identity.Observe), by, evidence)
				case len(pending[t]) > 0:
					prior := pending[t][len(pending[t])-1].Prior
					if prior == string(identity.Enforce) {
						// Restoring enforce is a switch to enforce like any other.
						if _, err = enforceGuard(cmd.Context(), cmd.OutOrStdout(), root, revokeAnonymous); err != nil {
							return err
						}
					}
					rec, err = applyModeChange(root, "rollback", t, prior, by, evidence)
				}
				if err != nil {
					return err
				}
				if rec != nil {
					did++
					if err := printFlip(cmd.OutOrStdout(), rec); err != nil {
						return err
					}
				}
			}
			if did == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "nothing to roll back (no pending flips in the log); use --observe to force observe")
				return nil
			}
			fmt.Fprintln(cmd.OutOrStdout(), "restart the daemon to apply; sessions launched under enforce keep their tokens")
			return nil
		},
	}
	cmd.Flags().StringVar(&point, "point", "", "roll back one target only (a point or global)")
	cmd.Flags().BoolVar(&toObserve, "observe", false, "break-glass: set to an explicit observe regardless of history")
	cmd.Flags().StringVar(&by, "by", "", "label recorded as who rolled back (not authenticated)")
	cmd.Flags().StringVar(&evidence, "evidence", "", "reason for the rollback")
	cmd.Flags().BoolVar(&revokeAnonymous, "revoke-anonymous", false, "when a rollback restores enforce, revoke the unbounded session credentials instead of refusing")
	return cmd
}

func printFlip(w io.Writer, r *flipRecord) error {
	_, err := fmt.Fprintf(w, "%s %s: %s -> %s (by %s); restart the daemon to apply\n", r.Action, r.Point, r.From, r.To, r.By)
	return err
}

// applyModeChange sets target to raw ("" removes the key, restoring "absent"),
// appends the audit record, then atomically replaces global.yaml. The record is
// written first so a change is never unrecorded; if the config write then
// fails, `identity mode` shows configured values that disagree with the log's
// last record.
func applyModeChange(catalogRoot, action, point, raw, by, evidence string) (*flipRecord, error) {
	if point != "global" && !slices.Contains(config.IdentityPoints, point) {
		return nil, fmt.Errorf("unknown point %q (want global or one of %s)", point, strings.Join(config.IdentityPoints, ", "))
	}
	if raw != "" && identity.Mode(raw).Validate() != nil {
		return nil, fmt.Errorf("invalid mode %q", raw)
	}
	if by == "" {
		if u, err := user.Current(); err == nil {
			by = u.Username
		} else {
			by = "unknown"
		}
	}
	path := filepath.Join(catalogRoot, "global.yaml")
	src, err := os.ReadFile(path) //nolint:gosec // G304: operator-selected catalog
	if err != nil {
		return nil, err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(src, &doc); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, fmt.Errorf("%s: top level is not a mapping", path)
	}
	var before config.Global
	if err := yaml.Unmarshal(src, &before); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	idNode := mapChild(doc.Content[0], "identity", yaml.MappingNode, raw != "")
	var parent *yaml.Node
	var key, prior string
	if point == "global" {
		parent, key, prior = idNode, "mode", before.Identity.Mode
	} else {
		parent, key, prior = mapChild(idNode, "points", yaml.MappingNode, raw != ""), point, before.Identity.Points[point]
	}
	if parent != nil {
		if raw == "" {
			deleteChild(parent, key)
		} else {
			setScalar(mapChild(parent, key, yaml.ScalarNode, true), raw)
		}
	}
	after := before.Identity
	if point == "global" {
		after.Mode = raw
	} else {
		after.Points = map[string]string{}
		for k, v := range before.Identity.Points {
			after.Points[k] = v
		}
		if raw == "" {
			delete(after.Points, point)
		} else {
			after.Points[point] = raw
		}
	}
	effTo := after.EffectiveMode()
	if point != "global" {
		effTo = after.ModeFor(point)
	}
	effFrom := before.Identity.EffectiveMode()
	if point != "global" {
		effFrom = before.Identity.ModeFor(point)
	}
	out, err := yaml.Marshal(&doc)
	if err != nil {
		return nil, err
	}
	var probe config.Global
	if err := yaml.Unmarshal(out, &probe); err != nil {
		return nil, fmt.Errorf("edited config does not parse: %w", err)
	}
	rec := &flipRecord{At: time.Now().UTC().Format(time.RFC3339), Action: action, By: by, Point: point, From: effFrom, To: effTo, Prior: prior, Evidence: evidence, Catalog: catalogRoot}
	if err := appendFlip(flipLogPath(catalogRoot), rec); err != nil {
		return nil, fmt.Errorf("audit record: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		return nil, err
	}
	if st, err := os.Stat(path); err == nil {
		_ = os.Chmod(tmp, st.Mode().Perm())
	}
	if err := os.Rename(tmp, path); err != nil {
		return nil, errors.Join(fmt.Errorf("config write failed after audit record %s", rec.Hash), err)
	}
	return rec, nil
}

// mapChild returns the child of mapping m under key, creating it with the given
// kind when create is set; without create it returns nil when absent.
func mapChild(m *yaml.Node, key string, kind yaml.Kind, create bool) *yaml.Node {
	if m == nil {
		return nil
	}
	if m.Kind != yaml.MappingNode {
		if !create {
			return nil
		}
		m.Kind, m.Tag, m.Content = yaml.MappingNode, "!!map", nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	if !create {
		return nil
	}
	v := &yaml.Node{Kind: kind, Tag: "!!str"}
	if kind == yaml.MappingNode {
		v.Tag = "!!map"
	}
	m.Content = append(m.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, v)
	return v
}

func deleteChild(m *yaml.Node, key string) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			m.Content = append(m.Content[:i], m.Content[i+2:]...)
			return
		}
	}
}

func setScalar(n *yaml.Node, v string) {
	n.Kind, n.Tag, n.Value, n.Content = yaml.ScalarNode, "!!str", v, nil
}
