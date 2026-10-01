package main

import "testing"

func TestRootCommandsBootNameIsUnique(t *testing.T) {
	seen := map[string]int{}
	for _, cmd := range rootCmd.Commands() {
		seen[cmd.Name()]++
	}
	if got := seen["boot"]; got != 1 {
		t.Fatalf("root command name %q appears %d times, want 1", "boot", got)
	}
	if got := seen["boot-prompts"]; got != 1 {
		t.Fatalf("root command name %q appears %d times, want 1", "boot-prompts", got)
	}
	if got := seen["boot-exec"]; got != 1 {
		t.Fatalf("root command name %q appears %d times, want 1", "boot-exec", got)
	}
}

func TestBootPromptUtilitiesLiveUnderBootPrompts(t *testing.T) {
	if child, _, err := rootCmd.Find([]string{"boot-prompts", "generate-boot"}); err != nil {
		t.Fatalf("find boot-prompts generate-boot: %v", err)
	} else if child != generateBootCmd {
		t.Fatalf("boot-prompts generate-boot resolved to %q, want generateBootCmd", child.CommandPath())
	}

	if child, _, err := rootCmd.Find([]string{"boot", "example.profile"}); err != nil {
		t.Fatalf("find boot example.profile: %v", err)
	} else if child != bootLaunchCmd {
		t.Fatalf("boot example.profile resolved to %q, want bootLaunchCmd", child.CommandPath())
	}

	if child, _, err := rootCmd.Find([]string{"boot-exec", "example.profile"}); err != nil {
		t.Fatalf("find boot-exec example.profile: %v", err)
	} else if child != bootExecCmd {
		t.Fatalf("boot-exec example.profile resolved to %q, want bootExecCmd", child.CommandPath())
	}
}
