package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/chrispian/agent-mux/internal/api"
	"github.com/chrispian/agent-mux/internal/client"
	"github.com/chrispian/agent-mux/internal/store"
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

var sessionsTurnCmd = &cobra.Command{
	Use:   "turn <id> <text>",
	Short: "Send a user turn with lifecycle-aware framing (streaming-stdio NDJSON, jsonrpc-stdio turn/start, or raw stdin fallback)",
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		c, err := newDaemonClient(catalogPath)
		if err != nil {
			return err
		}
		if err := c.SendTurn(cmd.Context(), args[0], args[1]); err != nil {
			if errors.Is(err, client.ErrDaemonUnreachable) {
				return fmt.Errorf("agent-mux daemon is not running; run `mux daemon start` first")
			}
			return err
		}
		return nil
	},
}

// detachByte is the keystroke that cleanly exits `mux sessions
// attach` while leaving the session running. \x1d is Ctrl-] —
// familiar to telnet users and rarely sent by any real program, so
// it's a safe "drop me out" escape.
const detachByte = 0x1d

// runAttach opens a bidirectional interactive attach:
//
//   - stdin is flipped to raw mode so keystrokes forward to the
//     session's PTY without line-buffering or local echo (the session
//     echoes what it wants).
//   - daemon → stdout via a streaming HTTP GET (same as before).
//   - stdin  → daemon via POST /sessions/{id}/input for each batch.
//   - SIGWINCH → POST /sessions/{id}/resize so the PTY tracks the
//     outer terminal size.
//   - Ctrl-] detaches cleanly; the session keeps running on the
//     daemon.
//
// When stdin isn't a TTY (pipes, CI, etc.) the function falls back to
// the old read-only stream. Daemon-down or session-exited errors map
// to the log-file snapshot path the tail subcommand used.
func runAttach(ctx context.Context, id string, tailFallbackOnNotRunning bool) error {
	c, err := newDaemonClient(catalogPath)
	if err != nil {
		return err
	}

	// fd uintptr → int conversion is safe in practice (stdin fd is 0).
	// Hoisted so the //nolint tag applies once.
	stdinFd := int(os.Stdin.Fd()) //nolint:gosec // G115: fd is a small non-negative int in practice

	// Degraded path: stdin is not a TTY → just stream output.
	if !term.IsTerminal(stdinFd) {
		err := c.AttachSession(ctx, id, os.Stdout, 0)
		return attachFallback(err, id, tailFallbackOnNotRunning)
	}
	old, err := term.MakeRaw(stdinFd)
	if err != nil {
		return fmt.Errorf("raw-mode stdin: %w", err)
	}
	defer func() { _ = term.Restore(stdinFd, old) }()

	fmt.Fprintf(os.Stderr, "[attached to %s — Ctrl-] to detach]\r\n", id)

	attachCtx, cancelAttach := context.WithCancel(ctx)
	defer cancelAttach()

	// Initial resize so the session starts at the right shape.
	if w, h, err := term.GetSize(stdinFd); err == nil {
		_ = c.ResizeSession(attachCtx, id, uint16(h), uint16(w)) //nolint:gosec // G115: GetSize returns sane int dims
	}

	// SIGWINCH → resize. Runs until attach exits.
	winchCh := make(chan os.Signal, 1)
	signal.Notify(winchCh, syscall.SIGWINCH)
	defer signal.Stop(winchCh)
	go func() {
		for {
			select {
			case <-winchCh:
				w, h, err := term.GetSize(stdinFd)
				if err == nil {
					_ = c.ResizeSession(attachCtx, id, uint16(h), uint16(w)) //nolint:gosec // G115: sane dims
				}
			case <-attachCtx.Done():
				return
			}
		}
	}()

	// stdin → daemon. Reads a byte at a time so the detach byte is
	// caught immediately (no line-buffering). Per-keystroke HTTP
	// overhead is tiny over UDS; we could batch later if it matters.
	inputDone := make(chan struct{})
	go func() {
		defer close(inputDone)
		buf := make([]byte, 1)
		for {
			n, err := os.Stdin.Read(buf)
			if err != nil {
				return
			}
			if n == 0 {
				continue
			}
			if buf[0] == detachByte {
				cancelAttach()
				return
			}
			if err := c.SendInput(attachCtx, id, buf[:n]); err != nil {
				if errors.Is(err, context.Canceled) {
					return
				}
				fmt.Fprintf(os.Stderr, "\r\n[send input: %v]\r\n", err)
				return
			}
		}
	}()

	// daemon → stdout (blocks until attach ends or ctx cancels).
	err = c.AttachSession(attachCtx, id, os.Stdout, 0)
	cancelAttach()
	<-inputDone

	// Final restore newline + message so the prompt after detach
	// doesn't land in the middle of a half-drawn line.
	fmt.Fprint(os.Stderr, "\r\n[detached]\r\n")

	return attachFallback(err, id, tailFallbackOnNotRunning)
}

// attachFallback maps benign attach errors (daemon down, session
// exited) to the log-snapshot path, and passes everything else
// through as-is.
func attachFallback(err error, id string, tailFallbackOnNotRunning bool) error {
	if err == nil || errors.Is(err, context.Canceled) {
		return nil
	}
	if errors.Is(err, client.ErrDaemonUnreachable) {
		return runTailSnapshot(id)
	}
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
func listSessionsDaemonOrStore(ctx context.Context, catalogRoot string) ([]api.SessionDTO, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	c, err := newDaemonClient(catalogRoot)
	if err != nil {
		return nil, err
	}
	// CLI currently has no --limit/--cursor flags; request the default
	// page. Pagination-aware flags can land in a follow-up.
	res, err := c.ListSessions(ctx, client.ListOptions{})
	if err == nil {
		return res.Sessions, nil
	}
	if !errors.Is(err, client.ErrDaemonUnreachable) {
		return nil, err
	}
	return listSessionsFromStore(catalogRoot)
}

func listSessionsFromStore(catalogRoot string) ([]api.SessionDTO, error) {
	db, err := openStoreReadOnly(catalogRoot)
	if err != nil {
		return nil, err
	}
	defer func() { _ = db.Close() }()
	rows, err := db.ListSessions(store.ListSessionsOptions{})
	if err != nil {
		return nil, err
	}
	out := make([]api.SessionDTO, 0, len(rows))
	for _, r := range rows {
		out = append(out, api.SessionRowToDTO(r))
	}
	return out, nil
}

func getSessionDaemonOrStore(ctx context.Context, catalogRoot, id string) (api.SessionDTO, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	c, err := newDaemonClient(catalogRoot)
	if err != nil {
		return api.SessionDTO{}, err
	}
	dto, err := c.GetSession(ctx, id)
	if err == nil {
		return dto, nil
	}
	if !errors.Is(err, client.ErrDaemonUnreachable) {
		return api.SessionDTO{}, err
	}
	return getSessionFromStore(catalogRoot, id)
}

func getSessionFromStore(catalogRoot, id string) (api.SessionDTO, error) {
	db, err := openStoreReadOnly(catalogRoot)
	if err != nil {
		return api.SessionDTO{}, err
	}
	defer func() { _ = db.Close() }()
	row, err := db.GetSession(id)
	if err != nil {
		return api.SessionDTO{}, err
	}
	return api.SessionRowToDTO(*row), nil
}

func init() {
	sessionsTailCmd.Flags().BoolVar(&sessionsTailFollow, "follow", true,
		"follow live output (default); use --follow=false for a one-shot snapshot")
	sessionsCmd.AddCommand(
		sessionsListCmd, sessionsGetCmd, sessionsStopCmd,
		sessionsTailCmd, sessionsAttachCmd, sessionsInputCmd, sessionsTurnCmd,
	)
}
