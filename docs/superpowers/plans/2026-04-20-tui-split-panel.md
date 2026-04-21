# TUI Split Panel Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a root-level right-side panel to the agent-mux TUI that displays agent-pushed interactive forms (ephemeral slot) and user-pinned documents (persistent slot), with auto show/hide, pin control, slot toggle, and accept-all shortcut.

**Architecture:** A new `internal/tui/panel/` package provides `panel.Model` composed at the root `internal/tui/model.go` level as a sibling of the screen stack — same pattern as `toast` and `palette`. `pkg/claudestream/parse.go` intercepts `tool_use` blocks named `ui_prompt` and emits `KindUIPrompt` instead of `KindToolUse`; ChatScreen converts them to `PanelPushMsg`; user responses are sent back to the agent via the existing `SendInput` mechanism as the next user turn. `lipgloss.JoinHorizontal` at the root splits terminal width between stack (~70%) and panel (~30%) when open.

**Tech Stack:** Go 1.26, Bubble Tea v1.3.10, Bubbles v1.0.0 (`bubbles/textinput`, `bubbles/viewport`), Lipgloss v1.1.0. Module path: `github.com/chrispian/agent-mux`.

---

## File Map

**New files:**
- `internal/tui/panel/content.go` — `Content` interface, `ContentKind`, `Slot` type
- `internal/tui/panel/yesno.go` — `YesNoContent` (2-option list)
- `internal/tui/panel/multichoice.go` — `MultiChoiceContent` (custom arrow-key list, no bubbles/list dependency)
- `internal/tui/panel/textinput_content.go` — `TextInputContent` (wraps `bubbles/textinput`)
- `internal/tui/panel/viewport_content.go` — `ViewportContent` (wraps `bubbles/viewport`; used for ReviewCard, Diff, Document, pinned file)
- `internal/tui/panel/panel.go` — `Model`, messages, `Init`/`Update`/`View`
- `internal/tui/panel/panel_test.go` — panel model tests

**Modified files:**
- `pkg/claudestream/events.go` — add `KindUIPrompt`, `UIPromptDescriptor` struct, `UIPrompt` field on `Event`
- `pkg/claudestream/parse.go` — intercept `tool_use` with `name == "ui_prompt"` → `KindUIPrompt`
- `pkg/claudestream/events_test.go` — add UIPrompt parse test
- `internal/tui/model.go` — add `panel` field + global panel keys + width-split + message routing + updated `View`
- `internal/tui/detail/chat.go` — handle `KindUIPrompt` → `PanelPushMsg`; handle `PanelResponseMsg` → `SendInput`
- `internal/tui/verbs.go` — add `"pin file"` verb → `PanelPinMsg`

---

## Task 1: claudestream — UIPromptDescriptor event type

**Files:**
- Modify: `pkg/claudestream/events.go`
- Modify: `pkg/claudestream/parse.go`
- Modify: `pkg/claudestream/events_test.go`

- [ ] **Step 1: Add KindUIPrompt and UIPromptDescriptor to events.go**

In `pkg/claudestream/events.go`, add after `KindError`:

```go
// KindUIPrompt is emitted when the agent invokes the reserved
// "ui_prompt" tool. The TUI renders a form in the side panel;
// the agent receives the user's response as the next user turn.
KindUIPrompt Kind = "ui_prompt"
```

Add a new struct before the wire-format section:

```go
// UIPromptDescriptor carries the form specification embedded in a
// "ui_prompt" tool_use block. Kind drives which Content type the
// panel instantiates.
type UIPromptDescriptor struct {
	// Kind selects the content renderer.
	// Valid values: "yes_no", "multi_choice", "text_input", "review_card", "diff", "document"
	Kind string `json:"kind"`
	// Title is the short heading shown above the form.
	Title string `json:"title"`
	// Body is optional detail text rendered below the title.
	Body string `json:"body,omitempty"`
	// Options lists the selectable items for multi_choice.
	Options []string `json:"options,omitempty"`
	// Default is the pre-selected value. For yes_no: "yes"/"no".
	// For multi_choice: index (float64 from JSON). For text_input: string.
	// For review_card/diff/document: unused.
	Default any `json:"default,omitempty"`
	// ToolUseID is the claude tool_use block ID, used to correlate the
	// user's response back to the agent as a tool_result marker.
	ToolUseID string `json:"-"` // set by parse.go from the block ID, not from JSON
}
```

Add `UIPrompt` field to the `Event` struct after `SessionID`:

```go
UIPrompt *UIPromptDescriptor // populated for KindUIPrompt
```

- [ ] **Step 2: Intercept ui_prompt in parse.go**

In `pkg/claudestream/parse.go`, in `parseAssistant`, replace the `"tool_use"` case:

```go
case "tool_use":
	if block.Name == "ui_prompt" {
		// Intercept: parse the input as a UIPromptDescriptor and emit
		// KindUIPrompt. The tool_use is NOT forwarded to consumers as
		// KindToolUse — the panel owns the interaction.
		var desc UIPromptDescriptor
		if len(block.Input) > 0 {
			_ = json.Unmarshal(block.Input, &desc)
		}
		desc.ToolUseID = block.ID
		out = append(out, Event{
			Kind:     KindUIPrompt,
			UIPrompt: &desc,
		})
		continue
	}
	input := make(map[string]any)
	if len(block.Input) > 0 {
		_ = json.Unmarshal(block.Input, &input)
	}
	out = append(out, Event{
		Kind: KindToolUse,
		ToolUse: &ToolUseBlock{
			ID:    block.ID,
			Name:  block.Name,
			Input: input,
		},
	})
```

- [ ] **Step 3: Write the failing parse test**

In `pkg/claudestream/events_test.go`, add:

```go
func TestParse_UIPrompt(t *testing.T) {
	line := []byte(`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_01","name":"ui_prompt","input":{"kind":"yes_no","title":"Continue?","default":"yes"}}]}}`)
	events, err := Parse(line)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("want 1 event, got %d", len(events))
	}
	ev := events[0]
	if ev.Kind != KindUIPrompt {
		t.Errorf("want KindUIPrompt, got %q", ev.Kind)
	}
	if ev.UIPrompt == nil {
		t.Fatal("UIPrompt is nil")
	}
	if ev.UIPrompt.Kind != "yes_no" {
		t.Errorf("want kind yes_no, got %q", ev.UIPrompt.Kind)
	}
	if ev.UIPrompt.Title != "Continue?" {
		t.Errorf("want title Continue?, got %q", ev.UIPrompt.Title)
	}
	if ev.UIPrompt.ToolUseID != "toolu_01" {
		t.Errorf("want ToolUseID toolu_01, got %q", ev.UIPrompt.ToolUseID)
	}
}

func TestParse_UIPrompt_NotForwardedAsToolUse(t *testing.T) {
	line := []byte(`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_02","name":"ui_prompt","input":{"kind":"yes_no","title":"Ok?"}}]}}`)
	events, err := Parse(line)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, ev := range events {
		if ev.Kind == KindToolUse {
			t.Error("ui_prompt must not be emitted as KindToolUse")
		}
	}
}
```

- [ ] **Step 4: Run the test to verify it fails**

```bash
cd ~/Projects-apps/agent-mux && go test ./pkg/claudestream/... -v -run TestParse_UIPrompt
```
Expected: `FAIL — KindUIPrompt undefined`

- [ ] **Step 5: Verify tests pass after changes**

```bash
cd ~/Projects-apps/agent-mux && go test ./pkg/claudestream/... -v -run TestParse_UIPrompt
```
Expected: both tests PASS

- [ ] **Step 6: Run full gate and commit**

```bash
cd ~/Projects-apps/agent-mux && make check
git add pkg/claudestream/events.go pkg/claudestream/parse.go pkg/claudestream/events_test.go
git commit -m "feat(claudestream): add KindUIPrompt + UIPromptDescriptor; intercept ui_prompt tool_use"
```

---

## Task 2: panel — Content interface and kinds

**Files:**
- Create: `internal/tui/panel/content.go`

- [ ] **Step 1: Write a failing test for ContentKind constants**

Create `internal/tui/panel/panel_test.go`:

```go
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
```

- [ ] **Step 2: Run to verify it fails**

```bash
cd ~/Projects-apps/agent-mux && go test ./internal/tui/panel/... -v -run TestContent
```
Expected: FAIL — package not found

- [ ] **Step 3: Create content.go**

Create `internal/tui/panel/content.go`:

```go
// Package panel provides the side-panel Model and its Content types.
// The panel lives at the TUI root level — a sibling of the screen
// stack — so every screen can drive it without importing detail/*.
package panel

import tea "github.com/charmbracelet/bubbletea"

// ContentKind identifies which renderer a Content value uses.
type ContentKind string

const (
	KindYesNo      ContentKind = "yes_no"
	KindMultiChoice ContentKind = "multi_choice"
	KindTextInput  ContentKind = "text_input"
	KindViewport   ContentKind = "viewport"
)

// Slot distinguishes the two panel content slots.
type Slot int

const (
	SlotEphemeral  Slot = iota // agent-pushed forms and review cards
	SlotPersistent             // user-pinned files and documents
)

// Content is anything the panel can render and optionally interact with.
// Both the ephemeral and persistent slots hold one Content at a time.
type Content interface {
	// View renders the content into width×height cells.
	View(width, height int) string
	// Update handles a key or window-size message routed from the panel.
	// Returns the updated Content and any command to run.
	Update(msg tea.Msg) (Content, tea.Cmd)
	// Kind returns the content's variant; used by the panel header.
	Kind() ContentKind
	// DefaultValue returns the pre-set response string.
	// Used by accept-all to confirm without user interaction.
	// Returns "" for content types that have no meaningful default
	// (e.g., KindViewport).
	DefaultValue() string
	// ToolUseID returns the claude tool_use block ID this content is
	// responding to, or "" if the content is not tied to a tool call
	// (e.g., pinned files).
	ToolUseID() string
}
```

- [ ] **Step 4: Verify test passes**

```bash
cd ~/Projects-apps/agent-mux && go test ./internal/tui/panel/... -v -run TestContent
```
Expected: PASS

- [ ] **Step 5: Commit**

```bash
cd ~/Projects-apps/agent-mux && git add internal/tui/panel/
git commit -m "feat(panel): Content interface, ContentKind, Slot types"
```

---

## Task 3: panel — YesNoContent

**Files:**
- Create: `internal/tui/panel/yesno.go`
- Modify: `internal/tui/panel/panel_test.go`

- [ ] **Step 1: Write failing test**

Add to `internal/tui/panel/panel_test.go`:

```go
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
```

Add `"strings"` to the import block in `panel_test.go`.

- [ ] **Step 2: Run to verify it fails**

```bash
cd ~/Projects-apps/agent-mux && go test ./internal/tui/panel/... -v -run TestYesNo
```
Expected: FAIL — NewYesNoContent undefined

- [ ] **Step 3: Create yesno.go**

Create `internal/tui/panel/yesno.go`:

```go
package panel

import (
	"strings"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/chrispian/agent-mux/internal/tui/theme"
)

// YesNoContent renders a two-option yes/no prompt with pre-selected default.
type YesNoContent struct {
	title      string
	toolUseID  string
	options    [2]string // always ["yes", "no"]
	cursor     int       // 0=yes, 1=no
	defaultVal string
	th         theme.Theme
	keyUp      key.Binding
	keyDown    key.Binding
	keyYes     key.Binding
	keyNo      key.Binding
}

// NewYesNoContent constructs a YesNoContent. defaultChoice must be "yes" or "no".
func NewYesNoContent(title, defaultChoice, toolUseID string) *YesNoContent {
	cursor := 0
	if defaultChoice == "no" {
		cursor = 1
	}
	return &YesNoContent{
		title:      title,
		toolUseID:  toolUseID,
		options:    [2]string{"yes", "no"},
		cursor:     cursor,
		defaultVal: defaultChoice,
		th:         theme.Default(),
		keyUp:      key.NewBinding(key.WithKeys("up", "k")),
		keyDown:    key.NewBinding(key.WithKeys("down", "j")),
		keyYes:     key.NewBinding(key.WithKeys("y", "Y")),
		keyNo:      key.NewBinding(key.WithKeys("n", "N")),
	}
}

func (c *YesNoContent) Kind() ContentKind  { return KindYesNo }
func (c *YesNoContent) DefaultValue() string { return c.defaultVal }
func (c *YesNoContent) ToolUseID() string   { return c.toolUseID }

func (c *YesNoContent) Update(msg tea.Msg) (Content, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch {
		case key.Matches(msg, c.keyUp):
			if c.cursor > 0 {
				c.cursor--
			}
		case key.Matches(msg, c.keyDown):
			if c.cursor < 1 {
				c.cursor++
			}
		case key.Matches(msg, c.keyYes):
			c.cursor = 0
		case key.Matches(msg, c.keyNo):
			c.cursor = 1
		}
	}
	return c, nil
}

// SelectedValue returns the currently highlighted option.
func (c *YesNoContent) SelectedValue() string {
	return c.options[c.cursor]
}

func (c *YesNoContent) View(width, height int) string {
	var sb strings.Builder
	sb.WriteString(c.th.FieldValue().Render(c.title))
	sb.WriteString("\n\n")
	for i, opt := range c.options {
		label := strings.ToUpper(opt[:1]) + opt[1:]
		if i == c.cursor {
			row := c.th.ResultSelected().Width(width - 2).Render("● " + label)
			sb.WriteString(row)
		} else {
			row := c.th.Footer().Width(width - 2).Render("○ " + label)
			sb.WriteString(row)
		}
		sb.WriteString("\n")
	}
	sb.WriteString("\n")
	hint := c.th.Footer().Render("↑↓ navigate · Enter confirm · Y/N shortcut")
	sb.WriteString(lipgloss.NewStyle().Width(width).Render(hint))
	return sb.String()
}
```

- [ ] **Step 4: Verify tests pass**

```bash
cd ~/Projects-apps/agent-mux && go test ./internal/tui/panel/... -v -run TestYesNo
```
Expected: PASS

- [ ] **Step 5: Commit**

```bash
cd ~/Projects-apps/agent-mux && git add internal/tui/panel/yesno.go internal/tui/panel/panel_test.go
git commit -m "feat(panel): YesNoContent"
```

---

## Task 4: panel — MultiChoiceContent

**Files:**
- Create: `internal/tui/panel/multichoice.go`
- Modify: `internal/tui/panel/panel_test.go`

- [ ] **Step 1: Write failing test**

Add to `internal/tui/panel/panel_test.go`:

```go
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
```

- [ ] **Step 2: Run to verify it fails**

```bash
cd ~/Projects-apps/agent-mux && go test ./internal/tui/panel/... -v -run TestMultiChoice
```
Expected: FAIL — NewMultiChoiceContent undefined

- [ ] **Step 3: Create multichoice.go**

Create `internal/tui/panel/multichoice.go`:

```go
package panel

import (
	"strings"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/chrispian/agent-mux/internal/tui/theme"
)

// MultiChoiceContent renders a list of options with arrow-key navigation.
// No bubbles/list dependency — a custom renderer keeps the code small
// and gives full control over selection rendering.
type MultiChoiceContent struct {
	title      string
	toolUseID  string
	options    []string
	cursor     int
	defaultIdx int
	th         theme.Theme
	keyUp      key.Binding
	keyDown    key.Binding
}

// NewMultiChoiceContent constructs a MultiChoiceContent.
// defaultIdx is the 0-based index of the pre-selected option.
func NewMultiChoiceContent(title string, options []string, defaultIdx int, toolUseID string) *MultiChoiceContent {
	if defaultIdx < 0 || defaultIdx >= len(options) {
		defaultIdx = 0
	}
	return &MultiChoiceContent{
		title:      title,
		toolUseID:  toolUseID,
		options:    options,
		cursor:     defaultIdx,
		defaultIdx: defaultIdx,
		th:         theme.Default(),
		keyUp:      key.NewBinding(key.WithKeys("up", "k")),
		keyDown:    key.NewBinding(key.WithKeys("down", "j")),
	}
}

func (c *MultiChoiceContent) Kind() ContentKind { return KindMultiChoice }
func (c *MultiChoiceContent) ToolUseID() string  { return c.toolUseID }
func (c *MultiChoiceContent) DefaultValue() string {
	if c.defaultIdx >= 0 && c.defaultIdx < len(c.options) {
		return c.options[c.defaultIdx]
	}
	return ""
}

// SelectedValue returns the currently highlighted option string.
func (c *MultiChoiceContent) SelectedValue() string {
	if c.cursor >= 0 && c.cursor < len(c.options) {
		return c.options[c.cursor]
	}
	return ""
}

func (c *MultiChoiceContent) Update(msg tea.Msg) (Content, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch {
		case key.Matches(msg, c.keyUp):
			if c.cursor > 0 {
				c.cursor--
			}
		case key.Matches(msg, c.keyDown):
			if c.cursor < len(c.options)-1 {
				c.cursor++
			}
		}
	}
	return c, nil
}

func (c *MultiChoiceContent) View(width, height int) string {
	var sb strings.Builder
	sb.WriteString(c.th.FieldValue().Render(c.title))
	sb.WriteString("\n\n")
	for i, opt := range c.options {
		if i == c.cursor {
			row := c.th.ResultSelected().Width(width - 2).Render("● " + opt)
			sb.WriteString(row)
		} else {
			row := c.th.Footer().Width(width - 2).Render("○ " + opt)
			sb.WriteString(row)
		}
		sb.WriteString("\n")
	}
	sb.WriteString("\n")
	hint := c.th.Footer().Render("↑↓ navigate · Enter confirm")
	sb.WriteString(lipgloss.NewStyle().Width(width).Render(hint))
	return sb.String()
}
```

- [ ] **Step 4: Verify tests pass**

```bash
cd ~/Projects-apps/agent-mux && go test ./internal/tui/panel/... -v -run TestMultiChoice
```
Expected: PASS

- [ ] **Step 5: Commit**

```bash
cd ~/Projects-apps/agent-mux && git add internal/tui/panel/multichoice.go internal/tui/panel/panel_test.go
git commit -m "feat(panel): MultiChoiceContent"
```

---

## Task 5: panel — TextInputContent and ViewportContent

**Files:**
- Create: `internal/tui/panel/textinput_content.go`
- Create: `internal/tui/panel/viewport_content.go`
- Modify: `internal/tui/panel/panel_test.go`

- [ ] **Step 1: Write failing tests**

Add to `internal/tui/panel/panel_test.go`:

```go
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
```

- [ ] **Step 2: Run to verify it fails**

```bash
cd ~/Projects-apps/agent-mux && go test ./internal/tui/panel/... -v -run "TestTextInput|TestViewport"
```
Expected: FAIL

- [ ] **Step 3: Create textinput_content.go**

Create `internal/tui/panel/textinput_content.go`:

```go
package panel

import (
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/chrispian/agent-mux/internal/tui/theme"
)

// TextInputContent wraps bubbles/textinput for single-line agent prompts.
type TextInputContent struct {
	title     string
	toolUseID string
	input     textinput.Model
	th        theme.Theme
}

// NewTextInputContent constructs a TextInputContent with an optional
// prefilled default value.
func NewTextInputContent(title, defaultValue, toolUseID string) *TextInputContent {
	ti := textinput.New()
	ti.Placeholder = "type here…"
	ti.SetValue(defaultValue)
	ti.Focus()
	return &TextInputContent{
		title:     title,
		toolUseID: toolUseID,
		input:     ti,
		th:        theme.Default(),
	}
}

func (c *TextInputContent) Kind() ContentKind    { return KindTextInput }
func (c *TextInputContent) DefaultValue() string { return c.input.Value() }
func (c *TextInputContent) ToolUseID() string    { return c.toolUseID }

// CurrentValue returns whatever the user has typed (may differ from
// the initial default).
func (c *TextInputContent) CurrentValue() string { return c.input.Value() }

func (c *TextInputContent) Update(msg tea.Msg) (Content, tea.Cmd) {
	var cmd tea.Cmd
	c.input, cmd = c.input.Update(msg)
	return c, cmd
}

func (c *TextInputContent) View(width, height int) string {
	c.input.Width = width - 4
	title := c.th.FieldValue().Render(c.title)
	input := c.th.Search().Width(width - 2).Render(c.input.View())
	hint := c.th.Footer().Render("Enter to confirm")
	return title + "\n\n" + input + "\n\n" + hint
}
```

- [ ] **Step 4: Create viewport_content.go**

Create `internal/tui/panel/viewport_content.go`:

```go
package panel

import (
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/chrispian/agent-mux/internal/tui/theme"
)

// ViewportContent wraps bubbles/viewport for read-only content: review
// cards, diffs, documents, and user-pinned files.
type ViewportContent struct {
	body      string // raw text/ANSI content
	toolUseID string // empty for pinned files; set for review_card/diff
	vp        viewport.Model
	th        theme.Theme
	sized     bool
}

// NewViewportContent constructs a ViewportContent for passive display.
// For agent-pushed review cards or diffs, pass the tool_use ID.
// For user-pinned files, pass toolUseID="".
func NewViewportContent(body string) *ViewportContent {
	vp := viewport.New(0, 0)
	vp.SetContent(body)
	return &ViewportContent{
		body: body,
		vp:   vp,
		th:   theme.Default(),
	}
}

// NewViewportContentWithToolUse constructs a ViewportContent tied to
// a specific agent tool_use block (review card, diff).
func NewViewportContentWithToolUse(body, toolUseID string) *ViewportContent {
	c := NewViewportContent(body)
	c.toolUseID = toolUseID
	return c
}

func (c *ViewportContent) Kind() ContentKind    { return KindViewport }
func (c *ViewportContent) DefaultValue() string { return "" }
func (c *ViewportContent) ToolUseID() string    { return c.toolUseID }

// SetBody replaces the content (used by "r refresh" in the panel).
func (c *ViewportContent) SetBody(body string) {
	c.body = body
	c.vp.SetContent(body)
}

func (c *ViewportContent) Update(msg tea.Msg) (Content, tea.Cmd) {
	var cmd tea.Cmd
	c.vp, cmd = c.vp.Update(msg)
	return c, cmd
}

func (c *ViewportContent) View(width, height int) string {
	// Resize viewport on every View call; cheap and avoids a separate
	// WindowSizeMsg path inside the panel.
	c.vp.Width = width - 2
	if height > 2 {
		c.vp.Height = height - 2
	}
	return c.th.Body().Width(width).Render(c.vp.View())
}
```

- [ ] **Step 5: Verify tests pass**

```bash
cd ~/Projects-apps/agent-mux && go test ./internal/tui/panel/... -v -run "TestTextInput|TestViewport"
```
Expected: PASS

- [ ] **Step 6: Run full check and commit**

```bash
cd ~/Projects-apps/agent-mux && make check
git add internal/tui/panel/textinput_content.go internal/tui/panel/viewport_content.go internal/tui/panel/panel_test.go
git commit -m "feat(panel): TextInputContent and ViewportContent"
```

---

## Task 6: panel — Panel model

**Files:**
- Create: `internal/tui/panel/panel.go`
- Modify: `internal/tui/panel/panel_test.go`

- [ ] **Step 1: Write failing panel model tests**

Add to `internal/tui/panel/panel_test.go`:

```go
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
	newP, _ := p.Update(panel.PanelPushMsg{Content: content})
	pp := newP.(panel.Model)
	if !pp.IsOpen() {
		t.Error("panel must open after PanelPushMsg")
	}
	if !pp.HasEphemeral() {
		t.Error("panel must have ephemeral content after push")
	}
}

func TestPanel_PinKeepsOpen(t *testing.T) {
	p := panel.New()
	// Push then dismiss — without pin, panel should close.
	content := panel.NewYesNoContent("Ok?", "yes", "t02")
	p2, _ := p.Update(panel.PanelPushMsg{Content: content})
	p3, _ := p2.(panel.Model).Update(panel.PanelDismissMsg{})
	if p3.(panel.Model).IsOpen() {
		t.Error("unpinned panel must close when ephemeral clears")
	}
	// With pin set, panel stays open.
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
	// AcceptAll should drain the queue and return a cmd batch.
	p4, cmd := p3.(panel.Model).Update(panel.PanelAcceptAllMsg{})
	_ = p4
	if cmd == nil {
		t.Error("AcceptAll must return a cmd (PanelResponseMsgs)")
	}
}
```

- [ ] **Step 2: Run to verify it fails**

```bash
cd ~/Projects-apps/agent-mux && go test ./internal/tui/panel/... -v -run TestPanel
```
Expected: FAIL — panel.New undefined

- [ ] **Step 3: Create panel.go**

Create `internal/tui/panel/panel.go`:

```go
package panel

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/chrispian/agent-mux/internal/tui/theme"
)

// ---- messages -------------------------------------------------------

// PanelPushMsg pushes content onto the ephemeral queue and opens the panel.
type PanelPushMsg struct{ Content Content }

// PanelPinMsg sets a file's text as the persistent slot content.
type PanelPinMsg struct {
	Path string
	Body string
}

// PanelToggleOpenMsg opens or closes the panel (Ctrl+\).
type PanelToggleOpenMsg struct{}

// PanelToggleSlotMsg flips the active slot when both slots have content (Tab).
type PanelToggleSlotMsg struct{}

// PanelTogglePinMsg toggles the pinned flag (p).
type PanelTogglePinMsg struct{}

// PanelAcceptAllMsg accepts all queued ephemeral prompts with their defaults (A).
type PanelAcceptAllMsg struct{}

// PanelFocusMsg moves keyboard focus into the panel.
type PanelFocusMsg struct{}

// PanelDismissMsg dequeues the front ephemeral item; closes the panel if
// unpinned and the queue is now empty.
type PanelDismissMsg struct{}

// PanelResponseMsg carries a confirmed prompt value back to ChatScreen.
// Root model forwards this to the top screen.
type PanelResponseMsg struct {
	ToolUseID string
	Value     string
}

// ---- keys -----------------------------------------------------------

type panelKeys struct {
	Up          key.Binding
	Down        key.Binding
	Confirm     key.Binding
	ToggleSlot  key.Binding
	TogglePin   key.Binding
	Refresh     key.Binding
	Hints       key.Binding
	BlurPanel   key.Binding
}

func defaultPanelKeys() panelKeys {
	return panelKeys{
		Up:         key.NewBinding(key.WithKeys("up", "k"), key.WithHelp("↑", "up")),
		Down:       key.NewBinding(key.WithKeys("down", "j"), key.WithHelp("↓", "down")),
		Confirm:    key.NewBinding(key.WithKeys("enter"), key.WithHelp("⏎", "confirm")),
		ToggleSlot: key.NewBinding(key.WithKeys("tab"), key.WithHelp("tab", "switch slot")),
		TogglePin:  key.NewBinding(key.WithKeys("p"), key.WithHelp("p", "pin")),
		Refresh:    key.NewBinding(key.WithKeys("r"), key.WithHelp("r", "refresh")),
		Hints:      key.NewBinding(key.WithKeys("?"), key.WithHelp("?", "hints")),
		BlurPanel:  key.NewBinding(key.WithKeys("esc"), key.WithHelp("esc", "back")),
	}
}

// ---- model ----------------------------------------------------------

// Model is the panel side-pane. Held at the root model level, not on the
// screen stack. Satisfies tea.Model for convenience but is driven by the
// root's Update routing, not by Bubble Tea directly.
type Model struct {
	open       bool
	pinned     bool
	active     Slot
	queue      []Content // pending ephemeral prompts; front is active
	persistent Content   // user-pinned; nil when empty
	pinnedPath string    // path of the pinned file, for refresh
	focused    bool
	width      int
	height     int
	th         theme.Theme
	keys       panelKeys
	showHints  bool
}

// New returns a closed, empty panel.
func New() Model {
	return Model{
		th:   theme.Default(),
		keys: defaultPanelKeys(),
	}
}

// IsOpen reports whether the panel is currently visible.
func (m Model) IsOpen() bool { return m.open }

// IsFocused reports whether keyboard focus is on the panel.
func (m Model) IsFocused() bool { return m.focused }

// HasEphemeral reports whether the ephemeral queue has at least one item.
func (m Model) HasEphemeral() bool { return len(m.queue) > 0 }

// HasPersistent reports whether a pinned file is loaded.
func (m Model) HasPersistent() bool { return m.persistent != nil }

// ActiveContent returns the content currently rendered in the panel, or nil.
func (m Model) ActiveContent() Content {
	if m.active == SlotEphemeral && len(m.queue) > 0 {
		return m.queue[0]
	}
	if m.active == SlotPersistent {
		return m.persistent
	}
	return nil
}

// Width returns the panel's allocated width.
func (m Model) Width() int { return m.width }

// SetSize updates the panel dimensions (called from root on WindowSizeMsg).
func (m *Model) SetSize(width, height int) {
	m.width = width
	m.height = height
}

func (m Model) Init() tea.Cmd { return nil }

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case PanelPushMsg:
		m.queue = append(m.queue, msg.Content)
		m.open = true
		m.active = SlotEphemeral
		return m, nil

	case PanelPinMsg:
		m.persistent = NewViewportContent(msg.Body)
		m.pinnedPath = msg.Path
		m.open = true
		if len(m.queue) == 0 {
			m.active = SlotPersistent
		}
		return m, nil

	case PanelToggleOpenMsg:
		m.open = !m.open
		if !m.open {
			m.focused = false
		}
		return m, nil

	case PanelToggleSlotMsg:
		if len(m.queue) > 0 && m.persistent != nil {
			if m.active == SlotEphemeral {
				m.active = SlotPersistent
			} else {
				m.active = SlotEphemeral
			}
		}
		return m, nil

	case PanelTogglePinMsg:
		m.pinned = !m.pinned
		return m, nil

	case PanelFocusMsg:
		if m.open {
			m.focused = true
		}
		return m, nil

	case PanelDismissMsg:
		return m.dismissFront()

	case PanelAcceptAllMsg:
		return m.acceptAll()

	case tea.KeyMsg:
		if !m.focused {
			return m, nil
		}
		return m.handleKey(msg)
	}
	// Forward to active content for viewport scroll, textinput edits, etc.
	return m.forwardToContent(msg)
}

func (m Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch {
	case key.Matches(msg, m.keys.BlurPanel):
		m.focused = false
		return m, nil

	case key.Matches(msg, m.keys.TogglePin):
		m.pinned = !m.pinned
		return m, nil

	case key.Matches(msg, m.keys.ToggleSlot):
		if len(m.queue) > 0 && m.persistent != nil {
			if m.active == SlotEphemeral {
				m.active = SlotPersistent
			} else {
				m.active = SlotEphemeral
			}
		}
		return m, nil

	case key.Matches(msg, m.keys.Refresh):
		// Refresh persistent slot if it has a path. Caller must handle
		// actual file re-read (we don't import os here); for now reset.
		return m, nil

	case key.Matches(msg, m.keys.Hints):
		m.showHints = !m.showHints
		return m, nil

	case key.Matches(msg, m.keys.Confirm):
		return m.confirmActive()
	}
	return m.forwardToContent(msg)
}

func (m Model) confirmActive() (tea.Model, tea.Cmd) {
	if m.active != SlotEphemeral || len(m.queue) == 0 {
		return m, nil
	}
	front := m.queue[0]
	var value string
	switch c := front.(type) {
	case *YesNoContent:
		value = c.SelectedValue()
	case *MultiChoiceContent:
		value = c.SelectedValue()
	case *TextInputContent:
		value = c.CurrentValue()
	default:
		value = front.DefaultValue()
	}
	resp := panelResponseCmd(front.ToolUseID(), value)
	newM, _ := m.dismissFront()
	return newM, resp
}

func (m Model) dismissFront() (tea.Model, tea.Cmd) {
	if len(m.queue) > 0 {
		m.queue = m.queue[1:]
	}
	if len(m.queue) == 0 {
		if !m.pinned {
			m.open = false
			m.focused = false
		} else if m.persistent != nil {
			m.active = SlotPersistent
		}
	}
	return m, nil
}

func (m Model) acceptAll() (tea.Model, tea.Cmd) {
	if len(m.queue) == 0 {
		return m, nil
	}
	cmds := make([]tea.Cmd, 0, len(m.queue))
	for _, c := range m.queue {
		cmds = append(cmds, panelResponseCmd(c.ToolUseID(), c.DefaultValue()))
	}
	m.queue = nil
	if !m.pinned {
		m.open = false
		m.focused = false
	} else if m.persistent != nil {
		m.active = SlotPersistent
	}
	return m, tea.Batch(cmds...)
}

func (m Model) forwardToContent(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch m.active {
	case SlotEphemeral:
		if len(m.queue) > 0 {
			updated, cmd := m.queue[0].Update(msg)
			m.queue[0] = updated
			return m, cmd
		}
	case SlotPersistent:
		if m.persistent != nil {
			updated, cmd := m.persistent.Update(msg)
			m.persistent = updated
			return m, cmd
		}
	}
	return m, nil
}

func panelResponseCmd(toolUseID, value string) tea.Cmd {
	return func() tea.Msg {
		return PanelResponseMsg{ToolUseID: toolUseID, Value: value}
	}
}

// ---- view -----------------------------------------------------------

func (m Model) View() string {
	if !m.open || m.width < 40 {
		return ""
	}
	header := m.renderHeader()
	body := m.renderBody()
	return lipgloss.JoinVertical(lipgloss.Left, header, body)
}

func (m Model) renderHeader() string {
	var label string
	pinMark := ""
	if m.pinned {
		pinMark = "📌 "
	}
	switch m.active {
	case SlotEphemeral:
		if len(m.queue) > 0 {
			label = fmt.Sprintf("%s▸ AGENT  [%d]", pinMark, len(m.queue))
		} else {
			label = pinMark + "▸ AGENT"
		}
	case SlotPersistent:
		path := m.pinnedPath
		if path == "" {
			path = "doc"
		}
		label = pinMark + path
	}
	borderColor := m.th.Border()
	if m.focused {
		borderColor = m.th.Accent()
	}
	hints := m.renderHints()
	left := m.th.Header().Render(label)
	right := m.th.Footer().Render(hints)
	gap := m.width - lipgloss.Width(left) - lipgloss.Width(right) - 4
	if gap < 1 {
		gap = 1
	}
	row := left + strings.Repeat(" ", gap) + right
	return lipgloss.NewStyle().
		Width(m.width).
		BorderBottom(true).
		BorderStyle(lipgloss.NormalBorder()).
		BorderForeground(borderColor).
		Render(row)
}

func (m Model) renderHints() string {
	parts := []string{}
	if m.focused {
		parts = append(parts, "⏎ confirm")
		if len(m.queue) > 0 && m.persistent != nil {
			parts = append(parts, "tab: switch")
		}
		parts = append(parts, "p: pin", "esc: back")
	} else {
		parts = append(parts, "Ctrl+\\ toggle")
	}
	return strings.Join(parts, " · ")
}

func (m Model) renderBody() string {
	bodyHeight := m.height - 2 // header row
	if bodyHeight < 1 {
		bodyHeight = 1
	}
	content := m.ActiveContent()
	if content == nil {
		return m.th.Footer().Width(m.width).Render("(empty)")
	}
	return lipgloss.NewStyle().
		Width(m.width).
		Height(bodyHeight).
		MaxHeight(bodyHeight).
		Render(content.View(m.width, bodyHeight))
}
```

- [ ] **Step 4: Verify tests pass**

```bash
cd ~/Projects-apps/agent-mux && go test ./internal/tui/panel/... -v -run TestPanel
```
Expected: all TestPanel_* tests PASS

- [ ] **Step 5: Run full gate and commit**

```bash
cd ~/Projects-apps/agent-mux && make check
git add internal/tui/panel/panel.go internal/tui/panel/panel_test.go
git commit -m "feat(panel): panel.Model — messages, Update, View, accept-all"
```

---

## Task 7: model.go — Wire panel at root

**Files:**
- Modify: `internal/tui/model.go`

The root model must: hold a `panel.Model`, expose global key bindings (`Ctrl+\`, `Ctrl+→`/`F3`, `A`), split `WindowSizeMsg` width between stack and panel, route panel messages, route keys to panel when focused, and compose `View` with `JoinHorizontal`.

- [ ] **Step 1: Read current model.go before editing**

```bash
cat ~/Projects-apps/agent-mux/internal/tui/model.go
```

- [ ] **Step 2: Write the updated model.go**

Replace `internal/tui/model.go` with:

```go
package tui

import (
	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/chrispian/agent-mux/internal/tui/client"
	"github.com/chrispian/agent-mux/internal/tui/palette"
	"github.com/chrispian/agent-mux/internal/tui/panel"
	"github.com/chrispian/agent-mux/internal/tui/screen"
)

// globalKeys are root-level bindings that apply regardless of which
// screen is on top or whether the panel is focused.
type globalKeys struct {
	PanelToggle key.Binding // Ctrl+\  — show/hide panel
	PanelFocus  key.Binding // Ctrl+→ or F3 — focus panel
	AcceptAll   key.Binding // A — accept all queued prompts
}

func defaultGlobalKeys() globalKeys {
	return globalKeys{
		PanelToggle: key.NewBinding(
			key.WithKeys("ctrl+\\"),
			key.WithHelp("ctrl+\\", "panel"),
		),
		PanelFocus: key.NewBinding(
			key.WithKeys("ctrl+right", "f3"),
			key.WithHelp("ctrl+→", "focus panel"),
		),
		AcceptAll: key.NewBinding(
			key.WithKeys("A"),
			key.WithHelp("A", "accept all"),
		),
	}
}

// Model is the Bubble Tea root for the mux TUI.
type Model struct {
	stack      *screen.Stack
	registry   *palette.Registry
	sidePanel  panel.Model
	globalKeys globalKeys
	width      int
	height     int
}

// New constructs the root Model.
func New(c *client.Client) Model {
	reg := palette.NewRegistry()
	registerDefaultVerbs(reg)
	return Model{
		stack:      screen.NewStack(NewMainScreen(c)),
		registry:   reg,
		sidePanel:  panel.New(),
		globalKeys: defaultGlobalKeys(),
	}
}

func (m Model) Init() tea.Cmd {
	return m.stack.Top().Init()
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	// ---- panel messages (route to panel, not stack) ----------------
	case panel.PanelPushMsg, panel.PanelPinMsg, panel.PanelToggleOpenMsg,
		panel.PanelToggleSlotMsg, panel.PanelTogglePinMsg,
		panel.PanelAcceptAllMsg, panel.PanelFocusMsg, panel.PanelDismissMsg:
		newPanel, cmd := m.sidePanel.Update(msg)
		m.sidePanel = newPanel.(panel.Model)
		if m.width > 0 {
			m.sidePanel.SetSize(m.panelWidth(), m.height)
		}
		return m, cmd

	// ---- panel response routes to the top screen -------------------
	case panel.PanelResponseMsg:
		top := m.stack.Top()
		newTop, cmd := top.Update(msg)
		m.stack.Replace(newTop)
		return m, cmd

	// ---- screen stack management -----------------------------------
	case openPaletteMsg:
		overlay := palette.NewOverlay(m.registry)
		return m.Update(screen.PushScreenMsg{Screen: overlay})

	case screen.PushScreenMsg:
		m.stack.Push(msg.Screen)
		initCmd := msg.Screen.Init()
		if m.width > 0 && m.height > 0 {
			sized, sizeCmd := m.stack.Top().Update(tea.WindowSizeMsg{
				Width:  m.stackWidth(),
				Height: m.height,
			})
			m.stack.Replace(sized)
			return m, tea.Batch(initCmd, sizeCmd)
		}
		return m, initCmd

	case screen.PopScreenMsg:
		if m.stack.Pop() == nil {
			return m, nil
		}
		if m.width > 0 && m.height > 0 {
			sized, sizeCmd := m.stack.Top().Update(tea.WindowSizeMsg{
				Width:  m.stackWidth(),
				Height: m.height,
			})
			m.stack.Replace(sized)
			return m, sizeCmd
		}
		return m, nil

	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.sidePanel.SetSize(m.panelWidth(), m.height)
		// Send stack-width-adjusted size to the top screen.
		stackMsg := tea.WindowSizeMsg{Width: m.stackWidth(), Height: m.height}
		top := m.stack.Top()
		newTop, cmd := top.Update(stackMsg)
		m.stack.Replace(newTop)
		return m, cmd

	case tea.KeyMsg:
		return m.handleKey(msg)

	default:
		top := m.stack.Top()
		newTop, cmd := top.Update(msg)
		m.stack.Replace(newTop)
		return m, cmd
	}
}

func (m Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// Global panel toggle — always fires.
	if key.Matches(msg, m.globalKeys.PanelToggle) {
		newPanel, cmd := m.sidePanel.Update(panel.PanelToggleOpenMsg{})
		m.sidePanel = newPanel.(panel.Model)
		m.sidePanel.SetSize(m.panelWidth(), m.height)
		// Re-send adjusted width to stack.
		if m.width > 0 {
			top := m.stack.Top()
			newTop, sizeCmd := top.Update(tea.WindowSizeMsg{
				Width:  m.stackWidth(),
				Height: m.height,
			})
			m.stack.Replace(newTop)
			return m, tea.Batch(cmd, sizeCmd)
		}
		return m, cmd
	}

	// Move focus to panel.
	if key.Matches(msg, m.globalKeys.PanelFocus) && m.sidePanel.IsOpen() {
		newPanel, cmd := m.sidePanel.Update(panel.PanelFocusMsg{})
		m.sidePanel = newPanel.(panel.Model)
		return m, cmd
	}

	// Accept all queued prompts.
	if key.Matches(msg, m.globalKeys.AcceptAll) && m.sidePanel.HasEphemeral() {
		newPanel, cmd := m.sidePanel.Update(panel.PanelAcceptAllMsg{})
		m.sidePanel = newPanel.(panel.Model)
		return m, cmd
	}

	// Route to panel when focused; otherwise to screen stack.
	if m.sidePanel.IsFocused() {
		newPanel, cmd := m.sidePanel.Update(msg)
		m.sidePanel = newPanel.(panel.Model)
		return m, cmd
	}

	top := m.stack.Top()
	newTop, cmd := top.Update(msg)
	m.stack.Replace(newTop)
	return m, cmd
}

func (m Model) View() string {
	stackView := m.stack.Top().View()
	panelView := m.sidePanel.View()
	if panelView == "" {
		return stackView
	}
	return lipgloss.JoinHorizontal(lipgloss.Top, stackView, panelView)
}

// stackWidth returns the width the screen stack should use.
// When the panel is closed or too small, the stack takes full width.
func (m Model) stackWidth() int {
	pw := m.panelWidth()
	if pw == 0 {
		return m.width
	}
	return m.width - pw
}

// panelWidth returns the width allocated to the side panel.
// Returns 0 if the terminal is too narrow to show the panel usefully.
func (m Model) panelWidth() int {
	if !m.sidePanel.IsOpen() {
		return 0
	}
	pw := m.width * 30 / 100
	if pw < 40 {
		return 0 // terminal too narrow; suppress panel rather than squash the stack
	}
	return pw
}
```

- [ ] **Step 3: Compile and run the gate**

```bash
cd ~/Projects-apps/agent-mux && make check
```
Expected: green. Fix any import or type errors before continuing.

- [ ] **Step 4: Commit**

```bash
cd ~/Projects-apps/agent-mux && git add internal/tui/model.go
git commit -m "feat(tui): wire panel.Model at root — split width, global keys, JoinHorizontal"
```

---

## Task 8: chat.go — UIPromptEvent → panel + PanelResponseMsg → SendInput

**Files:**
- Modify: `internal/tui/detail/chat.go`

ChatScreen must: (1) convert `KindUIPrompt` events to `PanelPushMsg` commands, and (2) handle `PanelResponseMsg` by sending the user's answer via `SendInput` and entering the awaiting state.

- [ ] **Step 1: Read the current chat.go**

```bash
cat ~/Projects-apps/agent-mux/internal/tui/detail/chat.go
```

- [ ] **Step 2: Add UIPromptEvent handling to applyEvent**

In `applyEvent`, in the `switch ev.Kind` block, add a case for `claudestream.KindUIPrompt`. Also return a `tea.Cmd` from `applyEvent` (the method signature must change slightly, or we can store the pending cmd and return it from `Update`).

The cleanest approach: have `applyEvent` return a `tea.Cmd` and thread it through `Update`:

Change the signature:
```go
func (s *ChatScreen) applyEvent(ev claudestream.Event) tea.Cmd {
```

In `Update`, where `applyEvent` is called:
```go
case chatStreamMsg:
    if msg.done {
        // ... unchanged
        return s, nil
    }
    cmd := s.applyEvent(msg.ev)
    s.refresh()
    return s, tea.Batch(drainChatCmd(s.ch), cmd)
```

Add the KindUIPrompt case inside `applyEvent`:
```go
case claudestream.KindUIPrompt:
    if msg.UIPrompt == nil {
        return nil
    }
    content := buildPanelContent(msg)
    if content == nil {
        return nil
    }
    return func() tea.Msg {
        return panel.PanelPushMsg{Content: content}
    }
```

All other `applyEvent` cases return `nil` (no cmd). Add `return nil` at the bottom of the function.

- [ ] **Step 3: Add buildPanelContent helper**

Add to `internal/tui/detail/chat.go`:

```go
// buildPanelContent converts a KindUIPrompt event into the appropriate
// panel.Content type. Returns nil for unknown kinds (panel stays closed).
func buildPanelContent(ev claudestream.Event) panel.Content {
	if ev.UIPrompt == nil {
		return nil
	}
	d := ev.UIPrompt
	switch d.Kind {
	case "yes_no":
		def := "yes"
		if s, ok := d.Default.(string); ok {
			def = s
		}
		return panel.NewYesNoContent(d.Title, def, d.ToolUseID)
	case "multi_choice":
		idx := 0
		if f, ok := d.Default.(float64); ok { // JSON numbers unmarshal as float64
			idx = int(f)
		}
		return panel.NewMultiChoiceContent(d.Title, d.Options, idx, d.ToolUseID)
	case "text_input":
		def := ""
		if s, ok := d.Default.(string); ok {
			def = s
		}
		return panel.NewTextInputContent(d.Title, def, d.ToolUseID)
	case "review_card", "diff", "document":
		body := d.Body
		if body == "" {
			body = d.Title
		}
		return panel.NewViewportContentWithToolUse(body, d.ToolUseID)
	default:
		return nil
	}
}
```

Add the import:
```go
"github.com/chrispian/agent-mux/internal/tui/panel"
```

- [ ] **Step 4: Handle PanelResponseMsg in Update**

In `ChatScreen.Update`, add a case before `tea.KeyMsg`:

```go
case panel.PanelResponseMsg:
    // The user confirmed a ui_prompt panel form. Send the response as
    // the next user turn. The agent receives it as a user message and
    // resolves its pending ui_prompt call.
    if msg.Value == "" {
        return s, nil
    }
    s.history = append(s.history, chatTurn{user: msg.Value})
    s.awaiting = true
    s.status = ""
    s.refresh()
    return s, s.sendTurnCmd(msg.Value)
```

- [ ] **Step 5: Compile and run make check**

```bash
cd ~/Projects-apps/agent-mux && make check
```
Expected: green. Fix any signature mismatches (the `applyEvent` return type change touches every call site — there is only one in `Update`).

- [ ] **Step 6: Commit**

```bash
cd ~/Projects-apps/agent-mux && git add internal/tui/detail/chat.go
git commit -m "feat(chat): handle KindUIPrompt → PanelPushMsg; PanelResponseMsg → SendInput"
```

---

## Task 9: verbs.go — "pin file" verb

**Files:**
- Modify: `internal/tui/verbs.go`

The `"pin file"` verb asks the user for a path (via a second palette prompt in v1 — simplest approach: open a text-input modal or re-use the palette input), reads the file, and emits `PanelPinMsg`. For v1, the verb emits a `pinFileRequestMsg` that the root or MainScreen handles by prompting for a path; for simplicity, implement it as a palette verb that uses a hard-coded demo path and documents that the file-picker is a follow-up.

Actually, for v1 keep it simple: the verb reads the path from a palette input. Since the palette currently only shows filtered verbs, the simplest path is: add a `"pin file <path>"` free-text verb where the extra text after `"pin file "` is treated as the path.

- [ ] **Step 1: Add panelPinFileMsg and the verb**

In `internal/tui/verbs.go`, add after the existing `type applyFilterMsg`:

```go
// panelPinFileMsg is emitted when the user runs "pin file <path>" from
// the palette. The root model reads the file and sends PanelPinMsg.
type panelPinFileMsg struct{ path string }
```

In `registerDefaultVerbs`, add after the "quit" verb:

```go
reg.Register(palette.Verb{
    Name:        "pin file",
    Description: "Pin a file to the side panel persistent slot (usage: pin file <path>)",
    Handler: func() tea.Cmd {
        // Path is empty here; the palette's free-text will be handled by
        // a prefix-match extension in a follow-up. For now register the
        // verb so it appears in the palette autocomplete.
        return nil
    },
})
```

- [ ] **Step 2: Check compile and run gate**

```bash
cd ~/Projects-apps/agent-mux && make check
```
Expected: green.

- [ ] **Step 3: Commit**

```bash
cd ~/Projects-apps/agent-mux && git add internal/tui/verbs.go
git commit -m "feat(tui): register 'pin file' palette verb stub (path arg follow-up)"
```

---

## Task 10: Acceptance verification

Run each acceptance criterion from the spec against the running binary.

- [ ] **Step 1: Build the binary**

```bash
cd ~/Projects-apps/agent-mux && go build ./cmd/mux/...
```
Expected: clean build

- [ ] **Step 2: Run full test suite with race detector**

```bash
cd ~/Projects-apps/agent-mux && make check
```
Expected: green, no race conditions

- [ ] **Step 3: Verify panel package test coverage**

```bash
cd ~/Projects-apps/agent-mux && go test ./internal/tui/panel/... -v -count=1
```
Expected: all tests pass

- [ ] **Step 4: Verify claudestream package tests**

```bash
cd ~/Projects-apps/agent-mux && go test ./pkg/claudestream/... -v -count=1
```
Expected: all tests pass including TestParse_UIPrompt*

- [ ] **Step 5: Smoke the panel in the TUI**

```bash
~/go/bin/mux tui
```
- Press `Ctrl+\` — panel should appear/disappear
- Press `Ctrl+→` or `F3` when panel is open — focus moves to panel
- Press `Esc` in panel — focus returns to left
- Press `?` in panel — inline hints appear

- [ ] **Step 6: Tag acceptance and note smoke gaps**

Any live-claude smoke (actual ui_prompt tool_use round-trip) follows the existing `[SMOKE-TEST GAP]` backlog pattern from the boot prompt — add a note to the backlog if the live path couldn't be verified.

- [ ] **Step 7: Final commit**

```bash
cd ~/Projects-apps/agent-mux && git add .
git commit -m "chore(tui): v0.0.4 split-panel acceptance verification complete"
```

---

## Known follow-ups (out of scope for this plan)

- `"pin file <path>"` palette verb full implementation (path argument parsing + file read)
- Live `ui_prompt` end-to-end smoke (requires `claude` CLI on PATH)
- Panel width drag-to-resize
- fsnotify live-refresh for pinned persistent slot
- Multiple pinned files (currently one slot)
- Mermaid chart rendering (iTerm2/Kitty inline-image protocol)
- `panelWidth()` user-configurable percentage (currently hard-coded 30%)
