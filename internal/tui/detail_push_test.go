package tui

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/chrispian/agent-mux/internal/tui/detail"
	"github.com/chrispian/agent-mux/internal/tui/screen"
)

// TestRightArrowPushesDetailForEachRowType asserts that pressing the
// Right arrow on each of the five row types emits a PushScreenMsg
// carrying the type-appropriate detail screen.
func TestRightArrowPushesDetailForEachRowType(t *testing.T) {
	m := newSeededModel(t)

	// Drive selection through each row type; assert push emits the
	// correct detail-screen variant.
	cases := []struct {
		wantType  string
		screenFor any
	}{
		// The seeded model has alphabetical ordering; we'll just
		// iterate m.visible and dispatch per concrete type.
	}
	_ = cases

	for i, row := range m.visible {
		m.selectedIdx = i
		next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRight})
		m = next.(MainScreen)
		if cmd == nil {
			t.Fatalf("idx %d row [%s] %s: expected push cmd, got nil", i, row.Type(), row.ID())
		}
		msg := cmd()
		pushMsg, ok := msg.(screen.PushScreenMsg)
		if !ok {
			t.Fatalf("idx %d: expected PushScreenMsg, got %T", i, msg)
		}
		switch row.(type) {
		case ProjectRow:
			if _, ok := pushMsg.Screen.(detail.ProjectScreen); !ok {
				t.Fatalf("idx %d: expected ProjectScreen, got %T", i, pushMsg.Screen)
			}
		case AgentRow:
			if _, ok := pushMsg.Screen.(detail.AgentScreen); !ok {
				t.Fatalf("idx %d: expected AgentScreen, got %T", i, pushMsg.Screen)
			}
		case ProviderRow:
			if _, ok := pushMsg.Screen.(detail.ProviderScreen); !ok {
				t.Fatalf("idx %d: expected ProviderScreen, got %T", i, pushMsg.Screen)
			}
		case LaunchRow:
			if _, ok := pushMsg.Screen.(detail.LaunchScreen); !ok {
				t.Fatalf("idx %d: expected LaunchScreen, got %T", i, pushMsg.Screen)
			}
		case SessionRow:
			if _, ok := pushMsg.Screen.(detail.SessionScreen); !ok {
				t.Fatalf("idx %d: expected SessionScreen, got %T", i, pushMsg.Screen)
			}
		default:
			t.Fatalf("idx %d: unhandled row type %T", i, row)
		}
	}
}
