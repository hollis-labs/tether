package bootgen

// defaultTemplate is the canonical 7-section boot prompt shape from
// agent-ops-alignment/examples/README.md. It renders using the named
// slots defined in the profile; sections are omitted when their slot
// is empty so a minimal profile still produces a clean output.
const defaultTemplate = `# Boot Prompt — {{if .Profile.DisplayName}}{{ .Profile.DisplayName }}{{else}}{{ .Profile.ID }}{{end}}

> **Compiled:** {{ .CompiledAt }}
> **Profile:** {{ .Profile.ID }}

---

{{- if hasSlot "agent" }}
## 1. Identity

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
