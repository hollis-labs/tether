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

func TestMultiChoiceContent_DefaultValue(t *testing.T) {
	opts := []string{"Alpha", "Beta", "Gamma"}
	c := panel.NewMultiChoiceContent("Pick one", opts, 1, "toolu_02")
	if c.DefaultValue() != "Beta" {
		t.Errorf("want Beta, got %q", c.DefaultValue())
	}
	if c.Kind() != panel.KindMultiChoice {
		t.Errorf("want KindMultiChoice, got %v", c.Kind())
	}
}

func TestMultiChoiceContent_View_ContainsOptions(t *testing.T) {
	opts := []string{"Alpha", "Beta", "Gamma"}
	c := panel.NewMultiChoiceContent("Pick one", opts, 0, "toolu_02")
	v := c.View(40, 10)
	for _, opt := range opts {
		if !strings.Contains(v, opt) {
			t.Errorf("View must contain option %q", opt)
		}
	}
}

func TestTextInputContent_DefaultValue(t *testing.T) {
	c := panel.NewTextInputContent("Branch name:", "feat/my-branch", "toolu_03")
	if c.DefaultValue() != "feat/my-branch" {
		t.Errorf("want feat/my-branch, got %q", c.DefaultValue())
	}
	if c.Kind() != panel.KindTextInput {
		t.Errorf("want KindTextInput, got %v", c.Kind())
	}
}

func TestViewportContent_NilToolUseID(t *testing.T) {
	c := panel.NewViewportContent("## Sprint Plan\n\n- [ ] Task 1\n")
	if c.ToolUseID() != "" {
		t.Errorf("pinned file has no tool_use ID, want empty, got %q", c.ToolUseID())
	}
	if c.DefaultValue() != "" {
		t.Errorf("viewport has no default, want empty, got %q", c.DefaultValue())
	}
	if c.Kind() != panel.KindViewport {
		t.Errorf("want KindViewport, got %v", c.Kind())
	}
}

func TestPanel_InitialState(t *testing.T) {
	p := panel.New()
	if p.IsOpen() {
		t.Error("panel must start closed")
	}
	if p.IsFocused() {
		t.Error("panel must start unfocused")
	}
	if p.HasEphemeral() {
		t.Error("panel must start with no ephemeral content")
	}
}

func TestPanel_PushOpensPanel(t *testing.T) {
	p := panel.New()
	content := panel.NewYesNoContent("Continue?", "yes", "t01")
	newM, _ := p.Update(panel.PanelPushMsg{Content: content})
	pp := newM.(panel.Model)
	if !pp.IsOpen() {
		t.Error("panel must open after PanelPushMsg")
	}
	if !pp.HasEphemeral() {
		t.Error("panel must have ephemeral content after push")
	}
}

func TestPanel_PinKeepsOpen(t *testing.T) {
	p := panel.New()
	content := panel.NewYesNoContent("Ok?", "yes", "t02")
	// Without pin: dismiss should close
	p2, _ := p.Update(panel.PanelPushMsg{Content: content})
	p3, _ := p2.(panel.Model).Update(panel.PanelDismissMsg{})
	if p3.(panel.Model).IsOpen() {
		t.Error("unpinned panel must close when ephemeral clears")
	}
	// With pin: dismiss should stay open
	p4, _ := p.Update(panel.PanelPushMsg{Content: content})
	p5, _ := p4.(panel.Model).Update(panel.PanelTogglePinMsg{})
	p6, _ := p5.(panel.Model).Update(panel.PanelDismissMsg{})
	if !p6.(panel.Model).IsOpen() {
		t.Error("pinned panel must stay open when ephemeral clears")
	}
}

func TestPanel_AcceptAll_DrainQueue(t *testing.T) {
	p := panel.New()
	c1 := panel.NewYesNoContent("Q1?", "yes", "t03")
	c2 := panel.NewYesNoContent("Q2?", "no", "t04")
	p2, _ := p.Update(panel.PanelPushMsg{Content: c1})
	p3, _ := p2.(panel.Model).Update(panel.PanelPushMsg{Content: c2})
	_, cmd := p3.(panel.Model).Update(panel.PanelAcceptAllMsg{})
	if cmd == nil {
		t.Error("AcceptAll must return a cmd (PanelResponseMsgs)")
	}
}
