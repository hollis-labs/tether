package config

import (
	"fmt"
	"sort"
)

// MCPFileCheck reports whether a catalog-authored credential file is usable.
// It never carries the credential's contents.
type MCPFileCheck struct {
	ServerID string
	Field    string
	Err      error
}

// CheckMCPServerCredentialFiles checks enabled entries without resolving helper
// references or starting upstreams. It uses the same file reader as spawning.
func CheckMCPServerCredentialFiles(catalogDir string) ([]MCPFileCheck, error) {
	entries, err := LoadMCPServerCatalog(catalogDir)
	if err != nil {
		return nil, err
	}
	var checks []MCPFileCheck
	for _, entry := range entries {
		if !entry.IsEnabled() {
			continue
		}
		refs := map[string]string{"token": entry.Token, "url": entry.URL}
		for i, value := range entry.Args {
			refs[fmt.Sprintf("args[%d]", i)] = value
		}
		for key, value := range entry.Env {
			refs["env."+key] = value
		}
		fields := make([]string, 0, len(entry.fileRefs))
		for field := range entry.fileRefs {
			fields = append(fields, field)
		}
		sort.Strings(fields)
		for _, field := range fields {
			_, readErr := resolveFileRef(field, refs[field], entry.catalogDir)
			checks = append(checks, MCPFileCheck{ServerID: entry.ID, Field: field, Err: readErr})
		}
	}
	return checks, nil
}
