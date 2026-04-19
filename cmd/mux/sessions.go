package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/chrispian/agent-mux/internal/client"
	"github.com/chrispian/agent-mux/internal/daemon"
)

var sessionsCmd = &cobra.Command{
	Use:   "sessions",
	Short: "Session commands",
}

var sessionsListCmd = &cobra.Command{
	Use:   "list",
	Short: "List sessions",
	RunE: func(cmd *cobra.Command, args []string) error {
		dtos, err := listSessionsDaemonOrStore(cmd.Context(), catalogPath)
		if err != nil {
			return err
		}
		fmt.Printf("%-36s  %-12s  %-12s  %-12s  %s\n", "ID", "STATE", "PROJECT", "AGENT", "CREATED")
		for _, d := range dtos {
			fmt.Printf("%-36s  %-12s  %-12s  %-12s  %s\n", d.ID, d.State, d.ProjectID, d.AgentID, d.CreatedAt)
		}
		return nil
	},
}

var sessionsGetCmd = &cobra.Command{
	Use:   "get <id>",
	Short: "Show session detail",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		d, err := getSessionDaemonOrStore(cmd.Context(), catalogPath, args[0])
		if err != nil {
			return err
		}
		exitCode := ""
		if d.ExitCode != nil {
			exitCode = fmt.Sprintf("%d", *d.ExitCode)
		}
		pid := ""
		if d.PID != nil {
			pid = fmt.Sprintf("%d", *d.PID)
		}
		fmt.Printf("id:         %s\nstate:      %s\nlaunch:     %s\nproject:    %s\nagent:      %s\nprovider:   %s\nworkspace:  %s\npid:        %s\nexit:       %s\ncreated_at: %s\nupdated_at: %s\n",
			d.ID, d.State, d.LaunchID, d.ProjectID, d.AgentID, d.ProviderID, d.Workspace, pid, exitCode, d.CreatedAt, d.UpdatedAt)
		return nil
	},
}

var sessionsStopCmd = &cobra.Command{
	Use:   "stop <id>",
	Short: "Stop a running session",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		c, err := newDaemonClient(catalogPath)
		if err != nil {
			return err
		}
		if err := c.StopSession(cmd.Context(), args[0]); err != nil {
			if errors.Is(err, client.ErrDaemonUnreachable) {
				return fmt.Errorf("agent-mux daemon is not running; run `mux daemon start` first")
			}
			return err
		}
		fmt.Fprintf(os.Stderr, "stopped: %s\n", args[0])
		return nil
	},
}

var sessionsTailCmd = &cobra.Command{
	Use:   "tail <id>",
	Short: "Tail the session log",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		d, err := getSessionDaemonOrStore(cmd.Context(), catalogPath, args[0])
		if err != nil {
			return err
		}
		f, err := os.Open(filepath.Join(d.Workspace, "logs", "session.log"))
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.Copy(os.Stdout, f)
		return err
	},
}

// listSessionsDaemonOrStore tries the daemon first and falls back to a
// read-only SQLite lookup when the daemon is unreachable. Errors other than
// "daemon unreachable" are propagated verbatim.
func listSessionsDaemonOrStore(ctx context.Context, catalogRoot string) ([]daemon.SessionDTO, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	c, err := newDaemonClient(catalogRoot)
	if err != nil {
		return nil, err
	}
	dtos, err := c.ListSessions(ctx)
	if err == nil {
		return dtos, nil
	}
	if !errors.Is(err, client.ErrDaemonUnreachable) {
		return nil, err
	}
	return listSessionsFromStore(catalogRoot)
}

func listSessionsFromStore(catalogRoot string) ([]daemon.SessionDTO, error) {
	db, err := openStoreReadOnly(catalogRoot)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	rows, err := db.ListSessions()
	if err != nil {
		return nil, err
	}
	out := make([]daemon.SessionDTO, 0, len(rows))
	for _, r := range rows {
		out = append(out, daemon.SessionRowToDTO(r))
	}
	return out, nil
}

func getSessionDaemonOrStore(ctx context.Context, catalogRoot, id string) (daemon.SessionDTO, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	c, err := newDaemonClient(catalogRoot)
	if err != nil {
		return daemon.SessionDTO{}, err
	}
	dto, err := c.GetSession(ctx, id)
	if err == nil {
		return dto, nil
	}
	if !errors.Is(err, client.ErrDaemonUnreachable) {
		return daemon.SessionDTO{}, err
	}
	return getSessionFromStore(catalogRoot, id)
}

func getSessionFromStore(catalogRoot, id string) (daemon.SessionDTO, error) {
	db, err := openStoreReadOnly(catalogRoot)
	if err != nil {
		return daemon.SessionDTO{}, err
	}
	defer db.Close()
	row, err := db.GetSession(id)
	if err != nil {
		return daemon.SessionDTO{}, err
	}
	return daemon.SessionRowToDTO(*row), nil
}

func init() {
	sessionsCmd.AddCommand(sessionsListCmd, sessionsGetCmd, sessionsStopCmd, sessionsTailCmd)
}
