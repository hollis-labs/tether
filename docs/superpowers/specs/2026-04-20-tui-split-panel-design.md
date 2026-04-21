# TUI Split Panel — Design Spec

**Date:** 2026-04-20  
**Status:** Approved — ready for planning  
**Scope:** v0.0.4 or v0.1 (TBD at sprint planning)

---

## Summary

Add a right-side panel to the agent-mux TUI that lives at the root model level and is available on every screen. The panel has two content slots — ephemeral (agent-pushed interactive forms and review cards) and persistent (user-pinned files and documents). The agent drives the ephemeral slot via a new `ui_prompt` tool intercept in `pkg/claudestream/`; users manage the persistent slot via the command palette. The panel auto shows/hides and can be pinned open.

---

## Architecture

### Root-level composition

The panel is a sibling of the screen stack in `model.go`, following the same pattern as `toast` and `palette`. It is not owned by any individual screen.

```
RootModel
├── screenStack     // internal/tui/screen — unchanged
├── panel           // NEW — internal/tui/panel/
├── toast           // unchanged
└── palette         // unchanged
```

`View()` at the root:

```go
lipgloss.JoinVertical(
    lipgloss.JoinHorizontal(
        screenStack.View(),  // left, takes remaining width
        panel.View(),        // right, ~30% width when open
    ),
    toast.View(),
)
```

When the panel is closed, `panel.View()` returns an empty string and the screen stack takes full width.

### New package: `internal/tui/panel/`

```
internal/tui/panel/
├── panel.go        // Model, Update, View, Init
├── content.go      // Content interface + ContentKind enum
├── yesno.go        // YesNoContent
├── multichoice.go  // MultiChoiceContent  (bubbles/list)
├── textinput.go    // TextInputContent    (bubbles/textinput)
├── viewport.go     // ViewportContent     (bubbles/viewport — review, diff, doc, pinned file)
└── panel_test.go
```

### Panel model

```go
type Model struct {
    open      bool    // panel visible?
    pinned    bool    // stay open when ephemeral clears
    active    Slot    // SlotEphemeral | SlotPersistent
    ephemeral Content // agent-pushed; nil when empty
    persistent Content // user-pinned; nil when empty
    focused   bool    // keyboard focus on panel vs screen stack
    width     int     // ~30% terminal width; calculated on WindowSizeMsg
}
```

### Messages (root routes these to/from panel)

| Message | Direction | Meaning |
|---|---|---|
| `PanelPushMsg` | → panel | Agent pushed ephemeral content; auto-open |
| `PanelPinMsg` | → panel | User pinned a file path; set persistent slot |
| `PanelToggleOpenMsg` | → panel | User pressed Ctrl+\ |
| `PanelToggleSlotMsg` | → panel | User pressed Tab (flip ephemeral ↔ persistent) |
| `PanelTogglePinMsg` | → panel | User pressed `p` |
| `PanelAcceptAllMsg` | → panel | User pressed `A`; confirm all queued prompts with defaults |
| `PanelDismissMsg` | → panel | Active ephemeral prompt confirmed/rejected |
| `PanelFocusMsg` | → panel | Move keyboard focus to panel |
| `PanelResponseMsg` | panel → root → ChatScreen | User confirmed a prompt; carry tool_result payload |

---

## Content Model

### Content interface

```go
type Content interface {
    View(width, height int) string
    Update(msg tea.Msg) (Content, tea.Cmd)
    Kind() ContentKind
}
```

`ViewportContent` is reused for all read-only slot types (ReviewCard, Diff, Document, pinned file). Interactive types each have their own implementation.

### Ephemeral content types

All types carry a `Default` field. The panel pre-selects / pre-fills the default on render so the user can press Enter immediately without navigating.

| Kind | Rendered with | Default field |
|---|---|---|
| `YesNo` | 2-option list | `"yes"` or `"no"` |
| `MultiChoice` | `bubbles/list` | index of pre-selected option |
| `TextInput` | `bubbles/textinput` | pre-filled string value |
| `ReviewCard` | `ViewportContent` + Approve/Reject actions | `"approve"` |
| `Diff` | `ViewportContent` (ANSI-colored via `go-diff`) + Approve/Reject | `"approve"` |
| `Document` | `ViewportContent` (read-only, no confirm needed) | n/a |

### Persistent content

User pins any file path via command palette verb `"pin file"` or the `PanelPinMsg`. Content is read from disk at pin time and rendered as `ViewportContent`. Pressing `r` re-reads the file. No fsnotify in v1 — explicit refresh keeps it simple.

Typical use: pinned sprint plan, todo list, or context doc stays visible across the full working session.

### Prompt queue

The ephemeral slot maintains a queue of pending `UIPromptEvent`s. The panel renders one at a time (front of queue). On confirm/reject, the front item is dequeued and the next is shown. Accept-all drains the entire queue using each item's default.

---

## Agent Integration

### Signal path

```
Agent emits tool_use { name: "ui_prompt", input: UIPromptDescriptor }
    │
    ▼
claudestream adapter (pkg/claudestream/)
    intercepts tool_name == "ui_prompt"
    emits UIPromptEvent instead of ToolUseEvent
    tool_use is NOT forwarded to chat stream (panel handles it cleanly)
    │
    ▼
ChatScreen receives UIPromptEvent
    sends PanelPushMsg{ content: buildContent(event) } to root
    │
    ▼
Root routes PanelPushMsg → panel.Update()
    panel opens, queues content, switches to ephemeral slot
    │
    ▼
User confirms (Enter, Y/N, or Accept-all)
    panel sends PanelResponseMsg{ toolUseID, result } to root
    root routes to ChatScreen
    │
    ▼
ChatScreen injects tool_result into session via existing input injection
Agent receives result and continues
```

### UIPromptDescriptor (JSON, carried in tool_use input)

```json
{
  "kind": "multi_choice",
  "title": "Which ADR approach?",
  "body": "Optional detail text shown above options.",
  "options": ["File-based", "In-memory", "Defer"],
  "default": 1
}
```

`default` is always set by the agent when a reasonable default exists. The TUI uses it for pre-selection and accept-all.

---

## Accept-All

Two paths; both resolve pending prompts without requiring panel focus:

**Key binding (`A`)** — global, active whenever the panel has queued ephemeral prompts. Drains the queue by sending each item's `default` as a tool_result. Panel closes (if not pinned) after the queue clears.

**Natural language** — user types in the chat input ("proceed", "use defaults", "yes to all", etc.). The chat message is sent to the agent as normal. Because the agent set defaults on its prompts, it interprets the NL response and resolves them. The TUI does not parse NL itself — it keeps the chat input live while prompts are pending so this path always works.

---

## Auto Show/Hide + Pin Rules

| Event | `pinned=false` | `pinned=true` |
|---|---|---|
| Agent pushes ephemeral content | open → true, switch to ephemeral slot | open stays true, switch to ephemeral slot |
| Ephemeral queue clears | open → false (hide) | open stays true, switch back to persistent |
| User pins a file | open → true, switch to persistent slot | open stays true |
| `Ctrl+\` | toggle open | toggle open |
| `p` (pin key) | pinned → true | pinned → false |

---

## Keyboard Model

### Global (always active)

| Key | Action |
|---|---|
| `Ctrl+\` | Toggle panel open/closed |
| `Ctrl+→` or `F3` | Move focus to panel (only when open) |
| `A` | Accept all queued prompts with defaults (only when prompts pending) |

### Panel-focused

| Key | Action |
|---|---|
| `Esc` | Return focus to left pane (panel stays open) |
| `↑` `↓` | Navigate list / scroll viewport |
| `Enter` | Confirm selection or submit text input |
| `Tab` | Toggle active slot (only when both slots have content) |
| `p` | Toggle pin |
| `r` | Refresh persistent slot (re-read file from disk) |
| `?` | Show inline key hints |

### Focus routing

Root `Update()` checks `panelFocused` bool. When false, all key events route to the screen stack unchanged — zero regression risk to existing bindings.

### Panel header (always visible when open)

Shows current slot label (`▸ AGENT REQUEST` or `📌 filename`), pin indicator, and context-sensitive key hints. Border color distinguishes focused (accent) vs unfocused (dim).

---

## Sizing

- Panel width: ~30% of terminal width, calculated from `WindowSizeMsg`
- Minimum useful width: 40 columns (panel suppresses itself below this)
- Screen stack gets remaining width; recalculates on every `WindowSizeMsg`

---

## Out of Scope (v1)

- fsnotify / live file watch for persistent slot (explicit `r` refresh instead)
- Multiple persistent pins (one slot, one file at a time)
- Mermaid chart rendering (deferred — would need iTerm2/Kitty inline-image protocol or ASCII-art library)
- Panel width drag-to-resize
- Panel history / scroll back through past ephemeral prompts

---

## Files Touched

| File | Change |
|---|---|
| `internal/tui/panel/` | New package — Model, Content interface, 4 content types |
| `pkg/claudestream/event.go` | Add `UIPromptEvent` kind + `UIPromptDescriptor` struct |
| `pkg/claudestream/scanner.go` | Intercept `tool_use` with `name == "ui_prompt"` → emit `UIPromptEvent` |
| `internal/tui/model.go` | Add `panel panel.Model`; compose in `View()`; route panel messages |
| `internal/tui/keys.go` | Add `Ctrl+\`, `Ctrl+→`/`F3`, `A` global bindings |
| `internal/tui/detail/chat.go` | Receive `UIPromptEvent` → send `PanelPushMsg`; receive `PanelResponseMsg` → inject tool_result |
| `internal/tui/commands.go` | Add `"pin file"` palette verb → `PanelPinMsg` |

---

## Acceptance Criteria

- [ ] Panel opens automatically when agent emits `ui_prompt` tool_use
- [ ] `ui_prompt` tool_use does not appear in the chat stream
- [ ] Agent-set default is pre-selected/pre-filled on render
- [ ] Enter alone confirms the default without navigating
- [ ] `A` key accepts all queued prompts with defaults and closes panel (if not pinned)
- [ ] Chat input remains live while prompts are pending (NL path works)
- [ ] `Ctrl+\` toggles panel from any screen
- [ ] User can pin a file via command palette; content renders in persistent slot
- [ ] `p` toggles pin; pinned panel does not auto-hide when ephemeral clears
- [ ] `Tab` flips between slots when both have content
- [ ] Panel suppresses itself below 40-column terminal width
- [ ] `make check` green; existing TUI tests unaffected
