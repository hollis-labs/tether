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
		if err := validateLaunchInjection(id, l.Injection); err != nil {
			return err
		}
	}
	for id, p := range c.Providers {
		typ := p.Type
		if typ == "" {
			typ = "cli"
		}
		switch p.EffectiveRuntimeKind() {
		case RuntimeKindPTY, RuntimeKindStreamingStdio, RuntimeKindJSONRPCStdio, RuntimeKindSubprocess, RuntimeKindAPI:
		default:
			return fmt.Errorf("provider %q has unsupported runtime_kind %q", id, p.EffectiveRuntimeKind())
		}
		switch typ {
		case "cli":
			if p.Command == "" {
				return fmt.Errorf("provider %q has empty command", id)
			}
		case "cli-goprovider":
			if p.Adapter == "" {
				return fmt.Errorf("provider %q (cli-goprovider) missing adapter field", id)
			}
			switch p.Adapter {
			case "claude", "codex":
			default:
				return fmt.Errorf("provider %q (cli-goprovider) has unsupported adapter %q", id, p.Adapter)
			}
		case "api":
			// "api" type (api-stub, future API-backed) has no command requirement.
		default:
			return fmt.Errorf("provider %q has unsupported type %q", id, typ)
		}
	}
	for id, a := range c.Agents {
		if name := a.Permissions.DefaultSandbox; name != "" {
			if _, ok := c.SandboxProfiles[name]; !ok {
				return fmt.Errorf("agent %q references unknown sandbox profile %q", id, name)
			}
		}
	}
	return nil
}

func validateLaunchInjection(launchID string, in LaunchInjection) error {
	for i, f := range in.NativeFiles {
		if f.Content != "" && f.Source != "" {
			return fmt.Errorf("launch %q injection.native_files[%d] sets both content and source", launchID, i)
		}
		kind := f.Kind
		if kind == "" {
			kind = "raw"
		}
		switch kind {
		case "raw":
			if f.RelPath == "" {
				return fmt.Errorf("launch %q injection.native_files[%d] missing rel_path", launchID, i)
			}
		case "skill":
			if f.ID == "" {
				return fmt.Errorf("launch %q injection.native_files[%d] missing id", launchID, i)
			}
		default:
			return fmt.Errorf("launch %q injection.native_files[%d] has unsupported kind %q", launchID, i, f.Kind)
		}
	}
	for i, f := range in.BootDirOverlay {
		if f.RelPath == "" {
			return fmt.Errorf("launch %q injection.boot_dir_overlay[%d] missing rel_path", launchID, i)
		}
		if f.Content != "" && f.Source != "" {
			return fmt.Errorf("launch %q injection.boot_dir_overlay[%d] sets both content and source", launchID, i)
		}
	}
	return nil
}
