package main

// sessions_tasks.go — fleet view of which sessions work which Torque tasks
// (CW-20261013-0075 item 7). `tether launch --torque-task` records a
// torque_task session ref per task; this command reads them back.

import (
	"context"
	"fmt"
	"os"
	"sort"

	"github.com/spf13/cobra"

	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/client"
	"github.com/hollis-labs/tether/internal/session"
)

const torqueTaskRefKind = "torque_task"

type sessionTaskRow struct {
	TaskID    string `json:"task_id"`
	SessionID string `json:"session_id"`
	State     string `json:"state"`
	Agent     string `json:"agent"`
	Stalled   bool   `json:"stalled,omitempty"`
}

var (
	sessionsTasksAll  bool
	sessionsTasksJSON bool
)

// recordLaunchTaskRefs attaches a torque_task ref per task to the launched
// session. Best effort: the launch already succeeded, so failures only warn.
func recordLaunchTaskRefs(ctx context.Context, c *client.Client, sessionID string, taskIDs []string) {
	for _, id := range taskIDs {
		if id == "" {
			continue
		}
		if _, err := c.AttachSessionRef(ctx, sessionID, api.SessionRefAttachRequest{Kind: torqueTaskRefKind, RefID: id, Relation: "referenced"}); err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not record task ref %s on session %s: %v\n", id, sessionID, err)
		}
	}
}

var sessionsTasksCmd = &cobra.Command{
	Use:   "tasks",
	Short: "Show which sessions are working which Torque tasks",
	Long: `Lists Torque task -> session pairs from torque_task session refs (recorded by
tether launch --torque-task, or attached with sessions refs attach). Only live
sessions are shown unless --all is set.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		c, err := newDaemonClient(catalogPath)
		if err != nil {
			return err
		}
		res, err := c.ListSessions(cmd.Context(), client.ListOptions{Limit: 500})
		if err != nil {
			return err
		}
		var rows []sessionTaskRow
		for _, s := range res.Sessions {
			if !sessionsTasksAll && session.State(s.State).Terminal() {
				continue
			}
			refs, err := c.ListSessionRefs(cmd.Context(), s.ID, torqueTaskRefKind, "", "")
			if err != nil {
				return classifyErr(err)
			}
			for _, r := range refs {
				rows = append(rows, sessionTaskRow{TaskID: r.RefID, SessionID: s.ID, State: s.State, Agent: s.LogicalAgentID, Stalled: s.Stalled})
			}
		}
		sort.SliceStable(rows, func(i, j int) bool { return rows[i].TaskID < rows[j].TaskID })
		if sessionsTasksJSON {
			if rows == nil {
				rows = []sessionTaskRow{}
			}
			return printJSON(rows)
		}
		if len(rows) == 0 {
			fmt.Println("(no sessions with Torque task refs)")
			return nil
		}
		fmt.Printf("%-20s  %-36s  %-10s  %s\n", "TASK", "SESSION", "STATE", "AGENT")
		for _, r := range rows {
			state := r.State
			if r.Stalled {
				state += "*"
			}
			fmt.Printf("%-20s  %-36s  %-10s  %s\n", r.TaskID, r.SessionID, state, r.Agent)
		}
		return nil
	},
}

func init() {
	sessionsTasksCmd.Flags().BoolVar(&sessionsTasksAll, "all", false, "include ended sessions")
	sessionsTasksCmd.Flags().BoolVar(&sessionsTasksJSON, "json", false, "print JSON")
	sessionsCmd.AddCommand(sessionsTasksCmd)
}
