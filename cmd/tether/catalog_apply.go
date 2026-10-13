package main

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/catalogapply"
	"github.com/hollis-labs/tether/internal/client"
	"github.com/hollis-labs/tether/internal/config"
)

// catalogCmd groups the operator's side of the approval-gated catalog change
// flow (CW-20261012-0124). The agent plans and applies through MCP; only the
// operator, at a terminal, approves.
var catalogCmd = &cobra.Command{
	Use:   "catalog",
	Short: "Review, approve and restore catalog changes",
	Long: `Review, approve and restore catalog changes an agent has planned.

An agent plans a change with the tether_catalog_plan MCP tool and applies it
with tether_catalog_apply, but only after the operator has approved the plan
here, on a terminal, with the approval passphrase:

  tether catalog approver set       set the passphrase (once)
  tether catalog show <plan_id>     read the diff, validation and privileged changes
  tether catalog approve <plan_id>  approve by digest, then the agent can apply
  tether catalog backups            list the backups an apply left

The passphrase is the guarantee: an agent that does not know it cannot approve
a plan, even running as the same OS user with the same socket and token files.
It does not protect against someone who can type as you, or who can write the
catalog files directly.`,
}

func init() {
	catalogCmd.AddCommand(catalogShowCmd, catalogApproveCmd, catalogBackupsCmd, catalogApproverCmd)
}

func localOnlyClient(catalogRoot string) (*client.Client, error) {
	if remoteTargetSelected() {
		return nil, errors.New("approving a catalog change is done on the daemon's own host; a remote target is not accepted")
	}
	c, err := newDaemonClient(catalogRoot)
	if err != nil {
		return nil, err
	}
	return c, nil
}

var catalogShowCmd = &cobra.Command{
	Use:   "show <plan_id>",
	Short: "Show a planned catalog change",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		h, err := localOnlyClient(catalogPath)
		if err != nil {
			return err
		}
		plan, err := h.CatalogApply().Get(cmd.Context(), args[0])
		if err != nil {
			return err
		}
		printPlan(cmd.OutOrStdout(), plan, true)
		return nil
	},
}

var catalogBackupsCmd = &cobra.Command{
	Use:   "backups",
	Short: "List catalog backups",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		h, err := localOnlyClient(catalogPath)
		if err != nil {
			return err
		}
		backups, err := h.CatalogApply().Backups(cmd.Context())
		if err != nil {
			return err
		}
		for _, b := range backups {
			fmt.Fprintf(cmd.OutOrStdout(), "%s  %s\n", b.CreatedAt.Format(time.RFC3339), b.ID)
		}
		return nil
	},
}

var (
	catalogApproveAckPrivileged bool
	catalogApproveAllowRunning  bool
)

func init() {
	catalogApproveCmd.Flags().BoolVar(&catalogApproveAckPrivileged, "ack-privileged", false, "confirm the plan's privileged changes (permissions, network, scopes, daemon defaults)")
	catalogApproveCmd.Flags().BoolVar(&catalogApproveAllowRunning, "allow-running", false, "allow changing definitions that running sessions use")
}

var catalogApproveCmd = &cobra.Command{
	Use:   "approve <plan_id>",
	Short: "Approve a planned catalog change so the agent can apply it",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
		if err != nil || !term.IsTerminal(int(tty.Fd())) {
			return errors.New("approval needs a terminal: run `tether catalog approve` yourself, from a shell you are typing in")
		}
		defer tty.Close()
		h, err := localOnlyClient(catalogPath)
		if err != nil {
			return err
		}
		plan, err := h.CatalogApply().Get(cmd.Context(), args[0])
		if err != nil {
			return err
		}
		printPlan(tty, plan, true)
		if plan.State == catalogapply.StateInvalid {
			return fmt.Errorf("plan is invalid and cannot be approved: %s", plan.ValidationError)
		}
		if len(plan.Privileged) > 0 && !catalogApproveAckPrivileged {
			return errors.New("this plan makes privileged changes (listed above); re-run with --ack-privileged to confirm them")
		}
		if len(plan.RunningSessions) > 0 && !catalogApproveAllowRunning {
			return errors.New("running sessions use the touched definitions (listed above); re-run with --allow-running to approve anyway")
		}
		fmt.Fprintf(tty, "\nTo approve, type the first 12 characters of the digest (%s): ", plan.Digest[:12])
		line, err := bufio.NewReader(tty).ReadString('\n')
		if err != nil {
			return err
		}
		if strings.TrimSpace(line) != plan.Digest[:12] {
			return errors.New("digest not confirmed; nothing approved")
		}
		fmt.Fprint(tty, "Approval passphrase: ")
		pass, err := term.ReadPassword(int(tty.Fd()))
		fmt.Fprintln(tty)
		if err != nil {
			return err
		}
		approved, err := h.CatalogApply().Approve(cmd.Context(), plan.ID, api.CatalogApproveRequest{
			Digest: plan.Digest, Passphrase: string(pass),
			AckPrivileged: catalogApproveAckPrivileged, AllowRunning: catalogApproveAllowRunning,
		})
		if err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "approved plan %s (digest %s); the agent can now apply it\n", approved.ID, approved.Digest[:12])
		return nil
	},
}

var catalogApproverCmd = &cobra.Command{
	Use:   "approver",
	Short: "Manage the approval passphrase",
}

func init() { catalogApproverCmd.AddCommand(catalogApproverSetCmd) }

var catalogApproverSetCmd = &cobra.Command{
	Use:   "set",
	Short: "Set or change the approval passphrase",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
		if err != nil || !term.IsTerminal(int(tty.Fd())) {
			return errors.New("setting the passphrase needs a terminal")
		}
		defer tty.Close()
		cat, err := config.LoadLayered(catalogPath)
		if err != nil {
			return err
		}
		file, _ := catalogApplyPaths(cat)
		ap := catalogapply.NewApprover(file)
		var current string
		if ap.Configured() {
			fmt.Fprint(tty, "Current passphrase: ")
			b, err := term.ReadPassword(int(tty.Fd()))
			fmt.Fprintln(tty)
			if err != nil {
				return err
			}
			current = string(b)
		}
		fmt.Fprint(tty, "New passphrase (12+ characters; do not store it where an agent can read it): ")
		n1, err := term.ReadPassword(int(tty.Fd()))
		fmt.Fprintln(tty)
		if err != nil {
			return err
		}
		fmt.Fprint(tty, "Repeat: ")
		n2, err := term.ReadPassword(int(tty.Fd()))
		fmt.Fprintln(tty)
		if err != nil {
			return err
		}
		if string(n1) != string(n2) {
			return errors.New("the two entries differ; nothing changed")
		}
		if err := ap.Set(current, string(n1)); err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "approval passphrase set (hash in %s)\n", file)
		return nil
	},
}

func printPlan(w interface{ Write([]byte) (int, error) }, p *catalogapply.Plan, withDiff bool) {
	fmt.Fprintf(w, "plan %s  %q  state=%s  expires %s\n", p.ID, p.Name, p.State, p.ExpiresAt.Local().Format("15:04:05"))
	fmt.Fprintf(w, "digest %s\n", p.Digest)
	for _, f := range p.Files {
		mark := ""
		if f.Privileged {
			mark = "  [privileged]"
		}
		fmt.Fprintf(w, "  %-9s %s%s\n", f.Action, f.Path, mark)
	}
	if p.ValidationOK {
		fmt.Fprintln(w, "validation: ok on a scratch copy")
	} else {
		fmt.Fprintf(w, "validation: FAILED: %s\n", p.ValidationError)
	}
	if p.BaselineError != "" {
		fmt.Fprintf(w, "note: the live catalog already reports: %s\n", p.BaselineError)
	}
	for _, r := range p.Privileged {
		fmt.Fprintf(w, "PRIVILEGED %s\n", r)
	}
	if len(p.RunningSessions) > 0 {
		fmt.Fprintf(w, "RUNNING SESSIONS use these definitions: %s\n", strings.Join(p.RunningSessions, ", "))
	}
	if p.RollbackOf != "" {
		fmt.Fprintf(w, "restores backup %s\n", p.RollbackOf)
	}
	if withDiff && p.Diff != "" {
		fmt.Fprintf(w, "\n%s", p.Diff)
	}
}
