package panel_test

import (
	"strings"
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

func TestYesNoContent_DefaultValue(t *testing.T) {
	c := panel.NewYesNoContent("Continue?", "yes", "toolu_01")
	if c.DefaultValue() != "yes" {
		t.Errorf("want yes, got %q", c.DefaultValue())
	}
	if c.ToolUseID() != "toolu_01" {
		t.Errorf("want toolu_01, got %q", c.ToolUseID())
	}
	if c.Kind() != panel.KindYesNo {
		t.Errorf("want KindYesNo, got %v", c.Kind())
	}
}

func TestYesNoContent_View_NotEmpty(t *testing.T) {
	c := panel.NewYesNoContent("Continue?", "yes", "toolu_01")
	v := c.View(40, 10)
	if v == "" {
		t.Error("View must not be empty")
	}
	if !strings.Contains(v, "Continue?") {
		t.Error("View must contain title")
	}
}
