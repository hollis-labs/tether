package workerwire

import (
	"github.com/hollis-labs/substrate/harness/sandbox"
	"github.com/hollis-labs/tether/internal/launchprofile"
)

type Workspace struct {
	Kind   string `json:"kind"`
	URL    string `json:"url,omitempty"`
	Ref    string `json:"ref,omitempty"`
	Subdir string `json:"subdir,omitempty"`
}
type LaunchSpec struct {
	Session  string `json:"session"`
	Actor    string `json:"actor"`
	Provider string `json:"provider"`
	// PermissionMode is the resolved agent permission posture ("bypass" or "default"). A worker has no
	// human to answer tool-permission prompts, so "bypass" must reach the provider as its own flag.
	PermissionMode string                     `json:"permission_mode,omitempty"`
	RuntimeKind    string                     `json:"runtime_kind"`
	Args           []string                   `json:"args"`
	Env            map[string]string          `json:"env"`
	BootPrompt     string                     `json:"boot_prompt"`
	BootMode       string                     `json:"boot_mode"`
	BootFiles      map[string]string          `json:"boot_files,omitempty"`
	NativeFiles    []launchprofile.NativeFile `json:"native_files,omitempty"`
	Workspace      Workspace                  `json:"workspace"`
	Sandbox        *sandbox.Profile           `json:"sandbox,omitempty"`
}
