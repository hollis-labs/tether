package bootgen

// defaultTemplate is the canonical 7-section boot prompt shape per
// agent-ops-alignment/examples/README.md and the Agent Identity Model
// (chatgpt-research-01-2026-04-21.md).
// Sections are omitted when their slot is empty.
const defaultTemplate = `# Boot Prompt — {{if .Profile.DisplayName}}{{ .Profile.DisplayName }}{{else}}{{ .Profile.ID }}{{end}}

> **Memory + knowledge:** Vanta-primary. Recall Vanta first (` + "`" + `memory_recall` + "`" + ` / ` + "`" + `conduit_lookup` + "`" + `), file-based is legacy fallback. Writes → Vanta only via ` + "`" + `capture-to-vanta` + "`" + `.

---

## 1. Identity & Freshness

` + "```yaml" + `
compiled_at:     {{ .CompiledAt }}
lineage_alias:   {{ .Profile.Identity.LineageAlias }}
lineage_id:      {{ if .Profile.Identity.LineageID }}{{ .Profile.Identity.LineageID }}{{ else }}(pending — Agent Mux profile registry){{ end }}
profile_id:      {{ .Profile.Identity.ProfileID }}
profile_version: {{ .Profile.Identity.ProfileVersion }}
role:            {{ .Profile.Identity.Role }}
project:         {{ .Profile.Identity.Project }}
work_root:       {{ .Profile.Identity.WorkRoot }}
tracking_root:   {{ .Profile.Identity.TrackingRoot }}
vanta_primary:   {{ .Profile.Identity.VantaPrimary }}
` + "```" + `
{{- if hasSlot "agent" }}

{{ slot "agent" }}
{{- end }}

{{- if hasSlot "recap" }}
## 2. Resume Now

{{ slot "recap" }}
{{- end }}

{{- if hasSlot "delta" }}
## 3. What Changed Since Last Boot

{{ slot "delta" }}
{{- end }}

{{- if or (hasSlot "history") (hasSlot "tasks") (hasSlot "status") }}
## 4. Active State

{{- if hasSlot "history" }}

### Git history
{{ slot "history" }}
{{- end }}
{{- if hasSlot "tasks" }}

### Tasks
{{ slot "tasks" }}
{{- end }}
{{- if hasSlot "status" }}

### Status
{{ slot "status" }}
{{- end }}
{{- end }}

{{- if or (hasSlot "memory") (hasSlot "knowledge") (hasSlot "skills") (hasSlot "context") }}
## 5. Durable Context

{{- if hasSlot "memory" }}

### Memory
{{ slot "memory" }}
{{- end }}
{{- if hasSlot "knowledge" }}

### Knowledge
{{ slot "knowledge" }}
{{- end }}
{{- if hasSlot "skills" }}

### Skills
{{ slot "skills" }}
{{- end }}
{{- if hasSlot "context" }}

### Context
{{ slot "context" }}
{{- end }}
{{- end }}

{{- if hasSlot "candidates" }}
## 6. Promotion / Defer / Archive

{{ slot "candidates" }}
{{- end }}

{{- if hasSlot "narrative" }}
## 7. Session Narrative

{{ slot "narrative" }}
{{- end }}
`
