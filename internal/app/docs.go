package app

import (
	"crypto/sha256"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"sort"
	"strings"

	assets "github.com/hollis-labs/tether"
)

const docsRoot = "docs/agents/mcp"

type DocMetadata = assets.DocMetadata
type DocFile = assets.DocFile
type DocBody = assets.DocBody
type DocFileBody = assets.DocFileBody

// DocsService owns the binary's docs. It never reads the catalog, cwd or disk.
type DocsService struct{}

// Docs is usable in lightweight Service compositions without catalog or state.
func (*Service) Docs() *DocsService { return &DocsService{} }

var docTriggers = map[string]string{
	"README":           "Find the MCP guide for your task.",
	"connect":          "Connect an MCP client to Tether.",
	"discovery":        "Choose a discovery mode, find tools and call them.",
	"limit-tools":      "Restrict upstreams and grant tools to launched agents.",
	"protection":       "Understand protected proxies and their security limits.",
	"add-upstream":     "Register an upstream app and its credential reference.",
	"budgets":          "Inspect AI budgets, MCP policy plans and event retention.",
	"troubleshoot":     "Diagnose missing tools or failed upstream connections.",
	"daemon-ownership": "Choose daemon-owned MCP upstreams for launched sessions.",
}

func (*DocsService) List() []DocMetadata {
	names, _ := fs.Glob(assets.AgentDocs(), docsRoot+"/*.md") // Constant valid pattern.
	items := make([]DocMetadata, 0, len(names))
	for _, name := range names {
		id := strings.TrimSuffix(path.Base(name), ".md")
		body, err := fs.ReadFile(assets.AgentDocs(), name)
		if err != nil {
			continue
		} // Only embedded documents are advertised.
		title := strings.TrimPrefix(strings.SplitN(string(body), "\n", 2)[0], "# ")
		trigger := docTriggers[id]
		if trigger == "" {
			trigger = "When you need to " + strings.ToLower(title) + "."
		}
		items = append(items, DocMetadata{ID: id, Title: title, Trigger: trigger, URI: "tether://docs/mcp/" + id})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].ID < items[j].ID })
	return items
}

var docLinks = regexp.MustCompile(`\]\(([^)\s]+)\)`)

func (s *DocsService) Get(id string) (DocBody, error) {
	var meta DocMetadata
	for _, item := range s.List() {
		if item.ID == id {
			meta = item
			break
		}
	}
	if meta.ID == "" {
		return DocBody{}, fmt.Errorf("documentation %q: %w", id, fs.ErrNotExist)
	}
	body, err := fs.ReadFile(assets.AgentDocs(), path.Join(docsRoot, id+".md"))
	if err != nil {
		return DocBody{}, err
	}
	result := DocBody{DocMetadata: meta, Body: string(body), Files: []DocFile{}}
	seen := map[string]bool{}
	for _, match := range docLinks.FindAllStringSubmatch(result.Body, -1) {
		link := strings.SplitN(match[1], "#", 2)[0]
		if link == "" || strings.Contains(link, ":") || strings.HasPrefix(link, "/") {
			continue
		}
		name := path.Join(docsRoot, link)
		if seen[name] {
			continue
		}
		content, err := fs.ReadFile(assets.AgentDocs(), name)
		if err != nil {
			continue
		} // External/nonembedded links are not file-fetch grants.
		seen[name] = true
		result.Files = append(result.Files, docFile(name, content))
	}
	sort.Slice(result.Files, func(i, j int) bool { return result.Files[i].Path < result.Files[j].Path })
	return result, nil
}

func docFile(name string, content []byte) DocFile {
	return DocFile{Path: name, URI: "tether://docs/files/" + name, MIMEType: "text/markdown", Size: len(content), SHA256: fmt.Sprintf("%x", sha256.Sum256(content))}
}

// GetFile only accepts an exact path in this guide's manifest, never a host path.
func (s *DocsService) GetFile(id, name string) (DocFileBody, error) {
	doc, err := s.Get(id)
	if err != nil {
		return DocFileBody{}, err
	}
	for _, file := range doc.Files {
		if file.Path != name {
			continue
		}
		body, err := fs.ReadFile(assets.AgentDocs(), name)
		if err != nil {
			return DocFileBody{}, err
		}
		return DocFileBody{DocFile: file, Body: string(body)}, nil
	}
	return DocFileBody{}, fmt.Errorf("reference %q in documentation %q: %w", name, id, fs.ErrNotExist)
}
