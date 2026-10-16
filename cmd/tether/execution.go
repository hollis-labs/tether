package main

import (
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/hollis-labs/tether/internal/execpolicy"
)

var executionCmd = &cobra.Command{
	Use:   "execution",
	Short: "Execution policy: global go/pause and session caps enforced at launch (daemon required)",
}

func printExecutionStatus(w io.Writer, st execpolicy.Status) {
	state := "running"
	if st.Policy.Paused {
		state = "paused"
		if st.Policy.Reason != "" {
			state += " (" + st.Policy.Reason + ")"
		}
	}
	fmt.Fprintf(w, "state: %s\n", state)
	fmt.Fprintf(w, "environment: %d live, cap %s\n", st.Usage.Total, capText(st.Policy.MaxSessions))
	resume := "off"
	if st.WorkerResume {
		resume = "on"
	}
	fmt.Fprintf(w, "worker resume: %s (catalog.defaults.worker_resume)\n", resume)
	for _, id := range st.Policy.SortedProjects() {
		fmt.Fprintf(w, "project %s: %d live, cap %s\n", id, st.Usage.ByProject[id], capText(st.Policy.ProjectCaps[id]))
	}
}

func capText(n int) string {
	if n <= 0 {
		return "none"
	}
	return strconv.Itoa(n)
}

// executionUpdate reads the policy, applies edit and writes it back.
func executionUpdate(cmd *cobra.Command, edit func(*execpolicy.Policy) error) error {
	c, err := newDaemonClient(catalogPath)
	if err != nil {
		return classifyErr(err)
	}
	st, err := c.ExecutionStatus(cmdCtx(cmd))
	if err != nil {
		return classifyErr(err)
	}
	p := st.Policy
	if err := edit(&p); err != nil {
		return err
	}
	st, err = c.SetExecutionPolicy(cmdCtx(cmd), p)
	if err != nil {
		return classifyErr(err)
	}
	printExecutionStatus(cmd.OutOrStdout(), st)
	return nil
}

func init() {
	var reason string
	var envCap int
	var projectCaps []string
	status := &cobra.Command{Use: "status", Short: "Show the execution policy and live session counts", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := newDaemonClient(catalogPath)
			if err != nil {
				return classifyErr(err)
			}
			st, err := c.ExecutionStatus(cmdCtx(cmd))
			if err != nil {
				return classifyErr(err)
			}
			printExecutionStatus(cmd.OutOrStdout(), st)
			return nil
		}}
	pause := &cobra.Command{Use: "pause", Short: "Refuse new session launches until resumed (running sessions are untouched)", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return executionUpdate(cmd, func(p *execpolicy.Policy) error { p.Paused, p.Reason = true, reason; return nil })
		}}
	pause.Flags().StringVar(&reason, "reason", "", "why launches are paused, shown in the refusal")
	resume := &cobra.Command{Use: "resume", Short: "Allow session launches again", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return executionUpdate(cmd, func(p *execpolicy.Policy) error { p.Paused, p.Reason = false, ""; return nil })
		}}
	capCmd := &cobra.Command{Use: "cap", Short: "Set session caps; 0 clears a cap", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return executionUpdate(cmd, func(p *execpolicy.Policy) error {
				if cmd.Flags().Changed("environment") {
					p.MaxSessions = envCap
				}
				for _, kv := range projectCaps {
					id, v, ok := strings.Cut(kv, "=")
					n, err := strconv.Atoi(v)
					if !ok || id == "" || err != nil {
						return fmt.Errorf("--project wants PROJECT=N, got %q", kv)
					}
					if p.ProjectCaps == nil {
						p.ProjectCaps = map[string]int{}
					}
					p.ProjectCaps[id] = n
				}
				return nil
			})
		}}
	capCmd.Flags().IntVar(&envCap, "environment", 0, "max live sessions on this daemon (0 = no cap)")
	capCmd.Flags().StringArrayVar(&projectCaps, "project", nil, "per-project cap as PROJECT=N (repeatable; 0 = no cap)")
	executionCmd.AddCommand(status, pause, resume, capCmd)
	rootCmd.AddCommand(executionCmd)
}
