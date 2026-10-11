package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

// flipRecord is one line of run/identity-flips.jsonl, the append-only audit of
// identity mode changes (CW-20261010-0268). The daemon reads the mode at start,
// so a flip takes effect on the next daemon restart.
type flipRecord struct {
	At       string `json:"at"`
	Action   string `json:"action"` // flip | rollback
	By       string `json:"by"`
	Point    string `json:"point"` // a config.IdentityPoints name, or "global"
	From     string `json:"from"`
	To       string `json:"to"`
	Evidence string `json:"evidence,omitempty"`
	Catalog  string `json:"catalog"`
}

func newIdentityCommand() *cobra.Command {
	root := &cobra.Command{Use: "identity", Short: "Show, flip and roll back identity enforcement modes"}
	root.AddCommand(identityModeCmd(), identityFlipCmd(), identityRollbackCmd())
	return root
}

func identityModeCmd() *cobra.Command {
	return &cobra.Command{
		Use: "mode", Short: "Show configured identity mode per enforcement point and the flip history", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cat, err := config.Load(config.Expand(catalogPath))
			if err != nil {
				return fmt.Errorf("load config: %w", err)
			}
			w := cmd.OutOrStdout()
			fmt.Fprintf(w, "global\t%s\n", cat.Global.Identity.EffectiveMode())
			for _, p := range config.IdentityPoints {
				fmt.Fprintf(w, "%s\t%s\n", p, cat.Global.Identity.ModeFor(p))
			}
			fmt.Fprintln(w, "(configured; the daemon applies changes on restart)")
			recs, _ := readFlips(flipLogPath())
			for _, r := range recs {
				fmt.Fprintf(w, "%s %s %s %s->%s by %s\n", r.At, r.Action, r.Point, r.From, r.To, r.By)
			}
			return nil
		},
	}
}

func identityFlipCmd() *cobra.Command {
	var point, to, by, evidence string
	var global bool
	cmd := &cobra.Command{
		Use:   "flip (--point NAME | --global) --to MODE --evidence TEXT",
		Short: "Set the identity mode of one enforcement point or globally, with an audit record",
		Long: "Edits identity.mode (--global) or identity.points.NAME in global.yaml and appends an audit record to run/identity-flips.jsonl.\n" +
			"Points: " + strings.Join(config.IdentityPoints, ", ") + ". Restart the daemon to apply. --evidence is required (task ids, owner GO reference, review query output path).",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if global == (point != "") {
				return fmt.Errorf("give exactly one of --point or --global")
			}
			if strings.TrimSpace(evidence) == "" {
				return fmt.Errorf("--evidence is required")
			}
			if global {
				point = "global"
			}
			rec, err := applyModeChange(config.Expand(catalogPath), "flip", point, to, by, evidence)
			if err != nil {
				return err
			}
			return printFlip(cmd.OutOrStdout(), rec)
		},
	}
	cmd.Flags().StringVar(&point, "point", "", "enforcement point: "+strings.Join(config.IdentityPoints, ", "))
	cmd.Flags().BoolVar(&global, "global", false, "change identity.mode")
	cmd.Flags().StringVar(&to, "to", "", "off, observe or enforce")
	cmd.Flags().StringVar(&by, "by", "", "who is flipping (default: OS user)")
	cmd.Flags().StringVar(&evidence, "evidence", "", "review evidence and GO reference (required)")
	_ = cmd.MarkFlagRequired("to")
	return cmd
}

func identityRollbackCmd() *cobra.Command {
	var point, by, evidence string
	cmd := &cobra.Command{
		Use:   "rollback [--point NAME]",
		Short: "Return to observe: one point, or (no flag) the global mode and every point override",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if strings.TrimSpace(evidence) == "" {
				evidence = "rollback"
			}
			root := config.Expand(catalogPath)
			targets := []string{point}
			if point == "" {
				targets = append([]string{"global"}, config.IdentityPoints...)
			}
			for _, t := range targets {
				rec, err := applyModeChange(root, "rollback", t, string(identity.Observe), by, evidence)
				if err != nil {
					return err
				}
				if rec == nil {
					continue
				}
				if err := printFlip(cmd.OutOrStdout(), rec); err != nil {
					return err
				}
			}
			fmt.Fprintln(cmd.OutOrStdout(), "restart the daemon to apply; sessions launched under enforce keep their tokens")
			return nil
		},
	}
	cmd.Flags().StringVar(&point, "point", "", "roll back one enforcement point only")
	cmd.Flags().StringVar(&by, "by", "", "who is rolling back (default: OS user)")
	cmd.Flags().StringVar(&evidence, "evidence", "", "reason for the rollback")
	return cmd
}

func printFlip(w io.Writer, r *flipRecord) error {
	_, err := fmt.Fprintf(w, "%s %s: %s -> %s (by %s); restart the daemon to apply\n", r.Action, r.Point, r.From, r.To, r.By)
	return err
}

func flipLogPath() string {
	return filepath.Join(filepath.Dir(config.Expand(catalogPath)), "run", "identity-flips.jsonl")
}

// applyModeChange validates and writes the new mode, then appends the audit
// record. The audit line is written first: a change is never applied without a
// record, and a failed config write leaves a record whose To is not in effect,
// which `identity mode` shows against the configured value. A rollback of a
// point already at observe is a no-op and returns a nil record.
func applyModeChange(catalogRoot, action, point, to, by, evidence string) (*flipRecord, error) {
	mode := identity.Mode(strings.ToLower(strings.TrimSpace(to)))
	if mode == "" || mode.Validate() != nil {
		return nil, fmt.Errorf("invalid --to %q (want off, observe or enforce)", to)
	}
	if point != "global" && !slices.Contains(config.IdentityPoints, point) {
		return nil, fmt.Errorf("unknown point %q (want global or one of %s)", point, strings.Join(config.IdentityPoints, ", "))
	}
	if by == "" {
		if u, err := user.Current(); err == nil {
			by = u.Username
		} else {
			by = "unknown"
		}
	}
	path := filepath.Join(catalogRoot, "global.yaml")
	raw, err := os.ReadFile(path) //nolint:gosec // G304: operator-selected catalog
	if err != nil {
		return nil, err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, fmt.Errorf("%s: top level is not a mapping", path)
	}
	idNode := mapChild(doc.Content[0], "identity")
	var from string
	if point == "global" {
		n := mapChild(idNode, "mode")
		from = config.IdentityConfig{Mode: n.Value}.EffectiveMode()
		if action == "rollback" && from == string(mode) {
			return nil, nil
		}
		setScalar(n, string(mode))
	} else {
		pts := mapChild(idNode, "points")
		n := mapChild(pts, point)
		from = config.IdentityConfig{Mode: mapChild(idNode, "mode").Value, Points: map[string]string{point: n.Value}}.ModeFor(point)
		if action == "rollback" && from == string(mode) && n.Value == "" {
			return nil, nil
		}
		setScalar(n, string(mode))
	}
	rec := &flipRecord{At: time.Now().UTC().Format(time.RFC3339), Action: action, By: by, Point: point, From: from, To: string(mode), Evidence: evidence, Catalog: catalogRoot}
	if err := appendFlip(filepath.Join(filepath.Dir(catalogRoot), "run", "identity-flips.jsonl"), rec); err != nil {
		return nil, fmt.Errorf("audit record: %w", err)
	}
	out, err := yaml.Marshal(&doc)
	if err != nil {
		return nil, err
	}
	// Re-validate the edited document before replacing global.yaml.
	var probe config.Global
	if err := yaml.Unmarshal(out, &probe); err != nil {
		return nil, fmt.Errorf("edited config does not parse: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		return nil, err
	}
	if st, err := os.Stat(path); err == nil {
		_ = os.Chmod(tmp, st.Mode().Perm())
	}
	if err := os.Rename(tmp, path); err != nil {
		return nil, err
	}
	return rec, nil
}

func mapChild(m *yaml.Node, key string) *yaml.Node {
	if m.Kind != yaml.MappingNode {
		m.Kind, m.Tag, m.Content = yaml.MappingNode, "!!map", nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	v := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str"}
	if key == "identity" || key == "points" {
		v = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	}
	m.Content = append(m.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, v)
	return v
}

func setScalar(n *yaml.Node, v string) {
	n.Kind, n.Tag, n.Value, n.Content = yaml.ScalarNode, "!!str", v, nil
}

func appendFlip(path string, r *flipRecord) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600) //nolint:gosec // G304: run dir beside the catalog
	if err != nil {
		return err
	}
	b, _ := json.Marshal(r)
	_, werr := f.Write(append(b, '\n'))
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	return werr
}

func readFlips(path string) ([]flipRecord, error) {
	b, err := os.ReadFile(path) //nolint:gosec // G304: run dir beside the catalog
	if err != nil {
		return nil, err
	}
	var out []flipRecord
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var r flipRecord
		if json.Unmarshal([]byte(line), &r) == nil && r.At != "" {
			out = append(out, r)
		}
	}
	return out, nil
}
