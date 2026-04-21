package panel_test

import (
	"testing"

	"github.com/chrispian/agent-mux/internal/tui/panel"
)

func TestContentKinds_Defined(t *testing.T) {
	kinds := []panel.ContentKind{
		panel.KindYesNo,
		panel.KindMultiChoice,
		panel.KindTextInput,
		panel.KindViewport,
	}
	for _, k := range kinds {
		if k == "" {
			t.Error("ContentKind must not be empty string")
		}
	}
}

func TestSlots_Defined(t *testing.T) {
	if panel.SlotEphemeral == panel.SlotPersistent {
		t.Error("SlotEphemeral and SlotPersistent must be distinct")
	}
}
