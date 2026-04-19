package config

import "fmt"

func (c *Catalog) Validate() error {
	for id, l := range c.Launches {
		if _, ok := c.Projects[l.Project]; !ok {
			return fmt.Errorf("launch %q references unknown project %q", id, l.Project)
		}
		if _, ok := c.Agents[l.Agent]; !ok {
			return fmt.Errorf("launch %q references unknown agent %q", id, l.Agent)
		}
		if _, ok := c.Providers[l.Provider]; !ok {
			return fmt.Errorf("launch %q references unknown provider %q", id, l.Provider)
		}
	}
	for id, p := range c.Providers {
		typ := p.Type
		if typ == "" {
			typ = "cli"
		}
		// Only CLI providers require a concrete Command — API-backed
		// providers (e.g. streaming SDKs) do not spawn a process.
		if typ == "cli" && p.Command == "" {
			return fmt.Errorf("provider %q has empty command", id)
		}
	}
	return nil
}
