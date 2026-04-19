package tui

import (
	"fmt"
	"os"
	"path/filepath"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/chrispian/agent-mux/internal/tui/client"
)

// Options configures how a TUI program is constructed. Fields are
// captured here even when the scaffold doesn't yet consume them so the
// cmd-layer call-site stays stable as T-03+ wires data flow.
type Options struct {
	// ListenAddr is the resolved muxd listen address (e.g.
	// "unix:/Users/you/.agent-mux/run/muxd.sock"). T-03 passes this to
	// the TUI client package; the scaffold accepts but does not use it.
	ListenAddr string

	// LogPath overrides the default Bubble Tea debug-log destination.
	// Empty value defaults to ~/.agent-mux/logs/tui.log so the alt
	// screen never sees debug writes.
	LogPath string
}

// Run constructs a Bubble Tea program with the scaffold root model and
// runs it until the user quits.
func Run(opts Options) error {
	logPath, err := resolveLogPath(opts.LogPath)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(logPath), 0o750); err != nil {
		return fmt.Errorf("create tui log dir: %w", err)
	}
	logFile, err := tea.LogToFile(logPath, "tui")
	if err != nil {
		return fmt.Errorf("open tui log: %w", err)
	}
	defer func() { _ = logFile.Close() }()

	c := client.New(opts.ListenAddr)
	prog := tea.NewProgram(New(c), tea.WithAltScreen())
	if _, err := prog.Run(); err != nil {
		return fmt.Errorf("run tui: %w", err)
	}
	return nil
}

func resolveLogPath(override string) (string, error) {
	if override != "" {
		return override, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home dir: %w", err)
	}
	return filepath.Join(home, ".agent-mux", "logs", "tui.log"), nil
}
