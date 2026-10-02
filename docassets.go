// Package tether embeds the authored documentation shipped with this binary.
package tether

import (
	"embed"
	"io/fs"
)

//go:embed docs/agents/mcp/*.md docs/mcp.md docs/secrets.md docs/dev-setup.md SECURITY.md
var agentDocs embed.FS

// AgentDocs returns the immutable source files; internal/app owns their service.
func AgentDocs() fs.FS { return agentDocs }

// DocMetadata is the first tier: no instructions or reference content.
type DocMetadata struct {
	ID      string `json:"id"`
	Title   string `json:"title"`
	Trigger string `json:"trigger"`
	URI     string `json:"uri"`
}

// DocFile describes one embedded reference reachable from the selected guide.
type DocFile struct {
	Path     string `json:"path"`
	URI      string `json:"uri"`
	MIMEType string `json:"mime_type"`
	Size     int    `json:"size"`
	SHA256   string `json:"sha256"`
}

// DocBody is the selected guide and its reference manifest, without file bodies.
type DocBody struct {
	DocMetadata
	Body  string    `json:"body"`
	Files []DocFile `json:"files"`
}

// DocFileBody is the final tier: one declared reference and its contents.
type DocFileBody struct {
	DocFile
	Body string `json:"body"`
}
