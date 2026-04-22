package tui

import (
	"fmt"
	"os"
	"path/filepath"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/chrispian/agent-mux/internal/tui/client"
)

// Options configures how a TUI program is constructed.
type Options struct {
	ListenAddr  string
	CatalogRoot string // enables in-process boot profile loading and boot-launch TUI flow
	LogPath     string
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

	c := client.NewWithCatalog(opts.ListenAddr, opts.CatalogRoot)
	prog := tea.NewProgram(NewWithOptions(c, opts), tea.WithAltScreen())
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
