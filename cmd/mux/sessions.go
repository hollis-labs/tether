package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

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
			fmt.Printf("%-36s  %-12s  %-12s  %-12s  %s\n", d.ID, d.State, d.ProjectID, d.LogicalAgentID, d.CreatedAt)
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
			d.ID, d.State, d.LaunchID, d.ProjectID, d.LogicalAgentID, d.ProviderID, d.Workspace, pid, exitCode, d.CreatedAt, d.UpdatedAt)
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

var sessionsTailFollow bool

var sessionsTailCmd = &cobra.Command{
	Use:   "tail <id>",
	Short: "Follow the session's live output (alias for attach; --follow=false for snapshot)",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if sessionsTailFollow {
			return runAttach(cmd.Context(), args[0], true)
		}
		return runTailSnapshot(args[0])
	},
}

var sessionsAttachCmd = &cobra.Command{
	Use:   "attach <id>",
	Short: "Live-follow the session's output until Ctrl-C",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runAttach(cmd.Context(), args[0], false)
	},
}

var sessionsInputCmd = &cobra.Command{
	Use:   "input <id> [text]",
	Short: "Send input to a running session; with no text, pipes stdin until EOF",
	Args:  cobra.RangeArgs(1, 2),
	RunE: func(cmd *cobra.Command, args []string) error {
		c, err := newDaemonClient(catalogPath)
		if err != nil {
			return err
		}
		id := args[0]
		var data []byte
		if len(args) == 2 {
			// Convenience form: append newline.
			data = []byte(args[1] + "\n")
		} else {
			data, err = io.ReadAll(os.Stdin)
			if err != nil {
				return fmt.Errorf("read stdin: %w", err)
			}
		}
		if err := c.SendInput(cmd.Context(), id, data); err != nil {
			if errors.Is(err, client.ErrDaemonUnreachable) {
				return fmt.Errorf("agent-mux daemon is not running; run `mux daemon start` first")
			}
			return err
		}
		return nil
	},
}

// runAttach opens a live attach stream to the daemon and copies PTY bytes
// to stdout until the session exits or ctx is cancelled. Falls back to a
// log-file read if the daemon is unreachable OR the session has already
// exited (server returns 404 from attach in that case).
func runAttach(ctx context.Context, id string, tailFallbackOnNotRunning bool) error {
	c, err := newDaemonClient(catalogPath)
	if err != nil {
		return err
	}
	err = c.AttachSession(ctx, id, os.Stdout)
	if err == nil {
		return nil
	}
	if errors.Is(err, client.ErrDaemonUnreachable) {
		// Daemon down — fall back to a snapshot of the on-disk log.
		return runTailSnapshot(id)
	}
	// 404 means the session isn't currently registered in the runtime (e.g.,
	// already exited). `tail --follow` should degrade to snapshot; explicit
	// `attach` surfaces the error so the user knows there's nothing to follow.
	if tailFallbackOnNotRunning && strings.Contains(err.Error(), "session not running") {
		return runTailSnapshot(id)
	}
	return err
}

func runTailSnapshot(id string) error {
	d, err := getSessionDaemonOrStore(context.Background(), catalogPath, id)
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
	sessionsTailCmd.Flags().BoolVar(&sessionsTailFollow, "follow", true,
		"follow live output (default); use --follow=false for a one-shot snapshot")
	sessionsCmd.AddCommand(
		sessionsListCmd, sessionsGetCmd, sessionsStopCmd,
		sessionsTailCmd, sessionsAttachCmd, sessionsInputCmd,
	)
}
