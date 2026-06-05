package main

// init.go — mux init: guided CLI first-run setup (T-v06x-01-03).
//
// Flags: --state-dir, --yes, --force, --print-plan
// Flow: welcome → detect providers → state location → skip AI keys hint →
//
//	review → write catalog → apply migrations → next steps.
//
// Non-TTY or --yes mode auto-accepts detected paths and skips undetected
// providers without blocking on stdin. Every non-required step has an
// explicit "set later" affordance per D4.

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/setup"
	"github.com/hollis-labs/tether/internal/store"
)

// initCmd is registered in root.go.
var initCmd = &cobra.Command{
	Use:   "init",
	Short: "First-run setup: create ~/.tether catalog, detect agents, apply migrations",
	Long: `mux init guides you through first-run setup of your Tether install.

It detects installed CLI providers (claude, codex, opencode), writes a starter
catalog to the state directory, and applies database migrations. Safe to re-run:
files that already exist are skipped unless --force is given.

Every step is skippable — you can always set paths later in Settings → Providers
or by editing the YAML files in your catalog directory.

Flags:
  --state-dir  Override the state root (default ~/.tether)
  --yes        Non-interactive: accept detected paths, skip prompts
  --force      Overwrite existing files (backs up with *.bak-<stamp>)
  --print-plan Dry-run: show what would be written without writing anything`,
	RunE: func(cmd *cobra.Command, args []string) error {
		stateDir, _ := cmd.Flags().GetString("state-dir")
		yes, _ := cmd.Flags().GetBool("yes")
		force, _ := cmd.Flags().GetBool("force")
		printPlan, _ := cmd.Flags().GetBool("print-plan")

		if stateDir == "" {
			// Derive state root from the --catalog global flag.
			stateDir = filepath.Dir(config.Expand(catalogPath))
		} else {
			stateDir = config.Expand(stateDir)
		}

		return runInit(os.Stdin, os.Stdout, initOpts{
			StateDir:  stateDir,
			Yes:       yes,
			Force:     force,
			PrintPlan: printPlan,
		})
	},
}

func init() {
	initCmd.Flags().String("state-dir", "", "state root directory (default ~/.tether)")
	initCmd.Flags().Bool("yes", false, "non-interactive: accept detected paths, skip prompts")
	initCmd.Flags().Bool("force", false, "overwrite existing files (backs up with *.bak-<stamp>)")
	initCmd.Flags().Bool("print-plan", false, "dry-run: show what would be written without writing")
}

// initOpts holds the parsed flags for runInit — separated so tests can call
// runInit directly without constructing a cobra command.
type initOpts struct {
	StateDir  string
	Yes       bool
	Force     bool
	PrintPlan bool
}

// runInit drives the interactive (or --yes) mux init flow.
func runInit(in io.Reader, out io.Writer, opts initOpts) error {
	p := &prompter{r: bufio.NewReader(in), w: out, yes: opts.Yes}

	catalogRoot := filepath.Join(opts.StateDir, "catalog")

	// ── Step 1: Welcome ───────────────────────────────────────────────────
	fmt.Fprintf(out, "\nWelcome to Tether setup.\n")
	fmt.Fprintf(out, "State root: %s\n\n", opts.StateDir)

	// Check whether the catalog already exists (re-run detection).
	globalYAML := filepath.Join(catalogRoot, "global.yaml")
	existing := fileExists(globalYAML)
	if existing {
		fmt.Fprintf(out, "Existing catalog detected at %s\n", catalogRoot)
		if !opts.Force {
			fmt.Fprintf(out, "Files that already exist will be skipped (use --force to overwrite).\n")
		}
		fmt.Fprintln(out)
	}

	// ── Step 2: Detect providers ──────────────────────────────────────────
	fmt.Fprintln(out, "Detecting CLI providers...")
	providerCommands := make(map[string]string)

	for _, r := range setup.DetectProviders() {
		cmd, err := p.promptProvider(r)
		if err != nil {
			return err
		}
		if cmd != "" {
			providerCommands[r.Brand] = cmd
		}
	}
	fmt.Fprintln(out)

	// ── Step 3: State location ────────────────────────────────────────────
	fmt.Fprintf(out, "State location: %s\n", opts.StateDir)
	if !opts.Yes {
		if override, err := p.ask("Override state location? [Enter to keep, or type a new path]"); err != nil {
			return err
		} else if strings.TrimSpace(override) != "" {
			opts.StateDir = config.Expand(strings.TrimSpace(override))
			catalogRoot = filepath.Join(opts.StateDir, "catalog")
			fmt.Fprintf(out, "→ Using %s\n", opts.StateDir)
		}
	}
	fmt.Fprintln(out)

	// ── Step 4: AI provider keys ──────────────────────────────────────────
	fmt.Fprintln(out, "AI provider keys: set later in Settings → AI or by editing")
	fmt.Fprintf(out, "  %s\n\n", filepath.Join(catalogRoot, "global.yaml"))

	// ── Step 5: Review ────────────────────────────────────────────────────
	fmt.Fprintln(out, "Review:")
	fmt.Fprintf(out, "  Catalog dir : %s\n", catalogRoot)
	for brand, cmd := range providerCommands {
		fmt.Fprintf(out, "  %-12s: command = %s\n", brand, cmd)
	}
	for _, r := range setup.DetectProviders() {
		if _, set := providerCommands[r.Brand]; !set {
			fmt.Fprintf(out, "  %-12s: command = (empty — auto-detect)\n", r.Brand)
		}
	}
	fmt.Fprintln(out)

	if opts.PrintPlan {
		fmt.Fprintln(out, "(--print-plan: nothing written)")
		return nil
	}

	if !opts.Yes {
		confirmed, err := p.confirm("Write catalog?")
		if err != nil {
			return err
		}
		if !confirmed {
			fmt.Fprintln(out, "Aborted — nothing written.")
			return nil
		}
	}

	// ── Step 6: Write catalog ─────────────────────────────────────────────
	rep, err := setup.WriteCatalog(opts.StateDir, setup.WriteOpts{
		Force:            opts.Force,
		ProviderCommands: providerCommands,
		StateRoot:        opts.StateDir,
	})
	if err != nil {
		return fmt.Errorf("write catalog: %w", err)
	}

	fmt.Fprintf(out, "✓ Wrote %d file(s) to %s\n", len(rep.Written), catalogRoot)
	if len(rep.Skipped) > 0 {
		fmt.Fprintf(out, "  Skipped %d existing file(s) (use --force to overwrite)\n", len(rep.Skipped))
	}
	if len(rep.Backed) > 0 {
		fmt.Fprintf(out, "  Backed up %d file(s)\n", len(rep.Backed))
	}

	// ── Step 7: Apply migrations ──────────────────────────────────────────
	cat, err := config.LoadLayered(catalogRoot)
	if err != nil {
		return fmt.Errorf("load catalog: %w", err)
	}
	dbPath := config.ResolveStateDB(cat.Global.Catalog.Defaults, cat.Paths)
	if dbPath == "" {
		return fmt.Errorf("could not resolve state_db path")
	}
	db, err := store.Open(dbPath) // Open runs migrations automatically.
	if err != nil {
		return fmt.Errorf("apply migrations: %w", err)
	}
	_ = db.Close()
	fmt.Fprintln(out, "✓ Applied database migrations")

	// ── Next steps ────────────────────────────────────────────────────────
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Next steps:")
	fmt.Fprintln(out, "  mux daemon start          — start the daemon")
	fmt.Fprintln(out, "  mux doctor                — verify your install")
	fmt.Fprintln(out, "  open http://localhost:8947 — open the sysop dashboard")
	fmt.Fprintln(out)

	return nil
}

// ── prompter ──────────────────────────────────────────────────────────────────

// prompter drives the interactive prompts. When yes is true all prompts
// auto-resolve to the default answer so non-TTY stdin never blocks.
type prompter struct {
	r   *bufio.Reader
	w   io.Writer
	yes bool
}

// promptProvider asks the user what to do with a detected provider.
// Returns the resolved command string, or "" to leave it blank (skip / auto-detect).
func (p *prompter) promptProvider(r setup.DetectResult) (string, error) {
	if r.Found {
		if p.yes {
			fmt.Fprintf(p.w, "  %-10s ✓ %s (auto-accepted)\n", r.Brand, r.Path)
			return r.Path, nil
		}
		fmt.Fprintf(p.w, "  %-10s ✓ found: %s\n", r.Brand, r.Path)
		ans, err := p.ask("    [Enter to accept / paste path / 'skip']")
		if err != nil {
			return "", err
		}
		ans = strings.TrimSpace(ans)
		switch {
		case ans == "":
			return r.Path, nil
		case strings.EqualFold(ans, "skip"):
			fmt.Fprintf(p.w, "    → %s skipped — set later in Settings → Providers\n", r.Brand)
			return "", nil
		default:
			return config.Expand(ans), nil
		}
	}

	// Not found.
	if p.yes {
		fmt.Fprintf(p.w, "  %-10s ✗ not found (set later in Settings → Providers)\n", r.Brand)
		return "", nil
	}
	fmt.Fprintf(p.w, "  %-10s ✗ not found\n", r.Brand)
	ans, err := p.ask("    [paste path / 'skip']")
	if err != nil {
		return "", err
	}
	ans = strings.TrimSpace(ans)
	if ans == "" || strings.EqualFold(ans, "skip") {
		fmt.Fprintf(p.w, "    → %s skipped — set later in Settings → Providers\n", r.Brand)
		return "", nil
	}
	return config.Expand(ans), nil
}

// ask prints a prompt and reads one line. In --yes mode returns "" immediately.
func (p *prompter) ask(prompt string) (string, error) {
	if p.yes {
		return "", nil
	}
	fmt.Fprintf(p.w, "%s: ", prompt)
	line, err := p.r.ReadString('\n')
	if err != nil && err != io.EOF {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

// confirm asks a yes/no question. In --yes mode returns true.
func (p *prompter) confirm(question string) (bool, error) {
	if p.yes {
		return true, nil
	}
	ans, err := p.ask(question + " [Y/n]")
	if err != nil {
		return false, err
	}
	ans = strings.TrimSpace(strings.ToLower(ans))
	return ans == "" || ans == "y" || ans == "yes", nil
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
