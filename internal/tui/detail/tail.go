package detail

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/chrispian/agent-mux/internal/api"
	"github.com/chrispian/agent-mux/internal/tui/screen"
)

// tailMaxBytes caps how much of session.log we read on `t` snapshot.
// 64 KiB keeps the viewport responsive for large logs; users who
// need full history can use `mux sessions tail --follow=false`.
const tailMaxBytes = 64 * 1024

// tailLoadedMsg carries the result of a log-file read into Update.
type tailLoadedMsg struct {
	content string
	err     error
}

// TailScreen renders a one-shot snapshot of a session's log file.
// The log path comes from the session DTO's Workspace field
// (workspace/logs/session.log) — the same filesystem read the CLI's
// `mux sessions tail --follow=false` does. A dedicated daemon
// `/sessions/{id}/log` endpoint isn't needed for v0.0.3 (local-only
// operation); flag as follow-up if a multi-host use case appears.
type TailScreen struct {
	base
	s    api.SessionDTO
	err  error
	keyR key.Binding
}

func NewTailScreen(s api.SessionDTO) *TailScreen {
	short := s.ID
	if len(short) > 8 {
		short = short[:8]
	}
	keyR := key.NewBinding(key.WithKeys("r"), key.WithHelp("r", "refresh"))
	b := newBase("Tail " + short)
	b.extra = append(b.extra, keyR)
	return &TailScreen{base: b, s: s, keyR: keyR}
}

func (s *TailScreen) Init() tea.Cmd { return loadTailCmd(s.s.Workspace) }

func (s *TailScreen) Update(msg tea.Msg) (screen.Screen, tea.Cmd) {
	switch m := msg.(type) {
	case tailLoadedMsg:
		s.err = m.err
		if m.err != nil {
			s.setContent("log read error: " + m.err.Error())
		} else {
			s.setContent(m.content)
		}
		return s, nil

	case tea.KeyMsg:
		if key.Matches(m, s.keyR) {
			return s, loadTailCmd(s.s.Workspace)
		}
	}
	cmd, handled := s.updateCommon(msg)
	if handled {
		return s, cmd
	}
	return s, nil
}

func (s *TailScreen) View() string { return s.render() }

func (s *TailScreen) KeyBindings() []key.Binding { return s.bindings() }

func (s *TailScreen) Title() string { return s.title }

// loadTailCmd reads the session's log file (last tailMaxBytes) off
// disk. Runs in a goroutine via tea.Cmd so file I/O never blocks
// Update.
func loadTailCmd(workspace string) tea.Cmd {
	return func() tea.Msg {
		if workspace == "" {
			return tailLoadedMsg{err: fmt.Errorf("session has no workspace path")}
		}
		path := filepath.Join(workspace, "logs", "session.log")
		f, err := os.Open(path) //nolint:gosec // G304: workspace is daemon-controlled, safe
		if err != nil {
			return tailLoadedMsg{err: err}
		}
		defer func() { _ = f.Close() }()

		stat, err := f.Stat()
		if err != nil {
			return tailLoadedMsg{err: err}
		}
		size := stat.Size()
		off := int64(0)
		if size > tailMaxBytes {
			off = size - tailMaxBytes
		}
		buf := make([]byte, size-off)
		if _, err := f.ReadAt(buf, off); err != nil {
			return tailLoadedMsg{err: err}
		}
		return tailLoadedMsg{content: string(buf)}
	}
}
