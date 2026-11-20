package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
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
  tether catalog approve <plan_id> [<plan_id>...]  approve by digest, then the agent can apply
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
	Use:   "approve <plan_id> [<plan_id>...]",
	Short: "Approve planned catalog changes so the agent can apply them",
	Args:  cobra.MinimumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
		if err != nil || !term.IsTerminal(int(tty.Fd())) {
			return errors.New("approval needs a terminal: run `tether catalog approve` yourself, from a shell you are typing in")
		}
		defer func() { _ = tty.Close() }() // /dev/tty is never buffered; nothing to flush
		h, err := localOnlyClient(catalogPath)
		if err != nil {
			return err
		}
		if len(args) > 64 {
			return errors.New("approve accepts at most 64 plans")
		}
		var plans []*catalogapply.Plan
		seen := map[string]bool{}
		for _, id := range args {
			if seen[id] {
				return fmt.Errorf("duplicate plan id %q", id)
			}
			seen[id] = true
			plan, err := h.CatalogApply().Get(cmd.Context(), id)
			if err != nil {
				return fmt.Errorf("plan %s unavailable: %w; re-plan: call tether_catalog_plan with the original bundle (name and files, or rollback_of)", id, err)
			}
			plans = append(plans, plan)
		}
		now := time.Now()
		if err := printApprovalReview(tty, plans, now, catalogApproveAckPrivileged, catalogApproveAllowRunning); err != nil {
			return err
		}
		token := approvalFingerprint(plans)
		pass, err := readApproval(tty, tty, token, func() ([]byte, error) { return term.ReadPassword(int(tty.Fd())) })
		if err != nil {
			return err
		}
		defer clear(pass)

		if len(plans) == 1 {
			plan := plans[0]
			approved, err := h.CatalogApply().Approve(cmd.Context(), plan.ID, api.CatalogApproveRequest{
				Digest: plan.Digest, Passphrase: string(pass), AckPrivileged: catalogApproveAckPrivileged, AllowRunning: catalogApproveAllowRunning,
			})
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "approved plan %s (digest %s); the agent can now apply it\n", approved.ID, shortDigest(approved.Digest))
		} else {
			req := api.CatalogApproveBatchRequest{Passphrase: string(pass), AckPrivileged: catalogApproveAckPrivileged, AllowRunning: catalogApproveAllowRunning}
			for _, p := range plans {
				req.Plans = append(req.Plans, catalogapply.ApprovalItem{ID: p.ID, Digest: p.Digest})
			}
			result, err := h.CatalogApply().ApproveBatch(cmd.Context(), req)
			if err != nil {
				return err
			}
			failed := false
			for _, r := range result.Results {
				if r.Error != "" {
					failed = true
					fmt.Fprintf(cmd.OutOrStdout(), "plan %s: NOT approved: %s\n", r.ID, r.Error)
				} else {
					fmt.Fprintf(cmd.OutOrStdout(), "approved plan %s (digest %s); the agent can now apply it\n", r.ID, shortDigest(r.Plan.Digest))
				}
			}
			if failed {
				return errors.New("some plans were not approved; review the per-plan results and re-plan failures with tether_catalog_plan using the original bundle")
			}
		}
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
		defer func() { _ = tty.Close() }() // /dev/tty is never buffered; nothing to flush
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
	now := time.Now()
	printPlanAt(w, p, withDiff, now)
	if err := planApprovalError(p, now); err != nil {
		fmt.Fprintln(w, err)
	}
}

func printPlanAt(w interface{ Write([]byte) (int, error) }, p *catalogapply.Plan, withDiff bool, now time.Time) {
	fmt.Fprintf(w, "plan %s  %q  state=%s\n", p.ID, p.Name, p.State)
	remaining := p.ExpiresAt.Sub(now).Truncate(time.Second).String()
	if !now.Before(p.ExpiresAt) {
		remaining = "expired"
	}
	fmt.Fprintf(w, "expires %s (remaining %s; ttl %ds); survives restart: %t\n", p.ExpiresAt.UTC().Format(time.RFC3339), remaining, p.TTLSeconds, p.SurvivesRestart)
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

func shortDigest(digest string) string {
	if len(digest) > 12 {
		return digest[:12]
	}
	return digest
}

// approvalFingerprint hashes sorted full digests separated by a single newline,
// with no trailing newline. Single-plan approval retains the original token.
func approvalFingerprint(plans []*catalogapply.Plan) string {
	if len(plans) == 1 {
		return shortDigest(plans[0].Digest)
	}
	digests := make([]string, 0, len(plans))
	for _, p := range plans {
		digests = append(digests, p.Digest)
	}
	sort.Strings(digests)
	sum := sha256.Sum256([]byte(strings.Join(digests, "\n")))
	return hex.EncodeToString(sum[:])[:12]
}

func printApprovalConfirmation(w interface{ Write([]byte) (int, error) }, token string) {
	fmt.Fprintf(w, "\nConfirmation token:\n%s\nType the confirmation token: ", token)
}

func replanGuidance(p *catalogapply.Plan) string {
	if p.RollbackOf != "" {
		return fmt.Sprintf("re-plan: call tether_catalog_plan with rollback_of=%q", p.RollbackOf)
	}
	return fmt.Sprintf("re-plan: call tether_catalog_plan with name=%q and the original files bundle", p.Name)
}

func planApprovalError(p *catalogapply.Plan, now time.Time) error {
	switch {
	case !now.Before(p.ExpiresAt):
		return fmt.Errorf("plan %s expired; %s", p.ID, replanGuidance(p))
	case p.State == catalogapply.StateInvalid:
		return fmt.Errorf("plan %s invalid: %s; %s", p.ID, strings.Join(strings.Fields(p.ValidationError), " "), replanGuidance(p))
	case p.State == catalogapply.StateApplied:
		return fmt.Errorf("plan %s already applied", p.ID)
	case len(p.Digest) != 64:
		return fmt.Errorf("plan %s has an invalid digest; %s", p.ID, replanGuidance(p))
	}
	return nil
}

func printApprovalReview(w interface{ Write([]byte) (int, error) }, plans []*catalogapply.Plan, now time.Time, ackPrivileged, allowRunning bool) error {
	for _, p := range plans {
		printPlanAt(w, p, len(plans) == 1, now)
		if err := planApprovalError(p, now); err != nil {
			return err
		}
		if len(p.Privileged) > 0 && !ackPrivileged {
			return fmt.Errorf("plan %s makes privileged changes (listed above); re-run with --ack-privileged to confirm them", p.ID)
		}
		if len(p.RunningSessions) > 0 && !allowRunning {
			return fmt.Errorf("running sessions use plan %s touched definitions (listed above); re-run with --allow-running to approve anyway", p.ID)
		}
	}
	return nil
}

// The caller establishes a real local terminal before entering this interaction.
func readApproval(w io.Writer, r io.Reader, token string, password func() ([]byte, error)) ([]byte, error) {
	printApprovalConfirmation(w, token)
	line, err := bufio.NewReader(r).ReadString('\n')
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(line) != token {
		return nil, errors.New("digest not confirmed; nothing approved")
	}
	fmt.Fprint(w, "Approval passphrase: ")
	pass, err := password()
	fmt.Fprintln(w)
	return pass, err
}
