package registry

import (
	"bytes"
	"errors"
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

var errURNMismatch = errors.New("registry: write-back: existing registry_urn differs")

// WriteURNBack writes registry_urn: <urn> into path if absent. Re-running is a
// no-op when the same URN is already present.
func WriteURNBack(path string, urn string) error {
	if path == "" || urn == "" {
		return fmt.Errorf("registry: write-back: path + urn required")
	}
	b, err := os.ReadFile(path) //nolint:gosec // operator-owned local config file
	if err != nil {
		return fmt.Errorf("registry: write-back read %s: %w", path, err)
	}

	var doc yaml.Node
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return appendURNLine(path, b, urn)
	}
	if len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return appendURNLine(path, b, urn)
	}
	root := doc.Content[0]
	for i := 0; i+1 < len(root.Content); i += 2 {
		key := root.Content[i]
		val := root.Content[i+1]
		if key.Value != "registry_urn" {
			continue
		}
		if val.Value == urn {
			return nil
		}
		return errURNMismatch
	}

	root.Content = append(root.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "registry_urn"},
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: urn},
	)

	var out bytes.Buffer
	enc := yaml.NewEncoder(&out)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		_ = enc.Close()
		return appendURNLine(path, b, urn)
	}
	if err := enc.Close(); err != nil {
		return appendURNLine(path, b, urn)
	}
	if err := os.WriteFile(path, out.Bytes(), 0o600); err != nil {
		return fmt.Errorf("registry: write-back write %s: %w", path, err)
	}
	return nil
}

func appendURNLine(path string, src []byte, urn string) error {
	if bytes.Contains(src, []byte("registry_urn: "+urn)) {
		return nil
	}
	if len(src) > 0 && src[len(src)-1] != '\n' {
		src = append(src, '\n')
	}
	src = append(src, []byte("# registry_urn added by mux registry bootstrap\nregistry_urn: "+urn+"\n")...)
	if err := os.WriteFile(path, src, 0o600); err != nil {
		return fmt.Errorf("registry: write-back append %s: %w", path, err)
	}
	return nil
}
