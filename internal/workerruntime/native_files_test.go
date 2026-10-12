package workerruntime

import (
	"testing"

	"github.com/hollis-labs/tether/internal/launchprofile"
)

// The hub compiles an agent's skills into kind "skill" native files that carry an ID and no RelPath. The worker
// used to plant every native file at RelPath, so any agent with skills failed to launch on a worker with
// "invalid boot file path".
func TestNativeFilePathResolvesSkillsLikeTheHubPlanter(t *testing.T) {
	got, err := nativeFilePath("claude", "streaming-stdio", launchprofile.NativeFile{Kind: "skill", ID: "mailbox", Content: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if got != ".claude/skills/mailbox/SKILL.md" {
		t.Fatalf("claude skill path = %q", got)
	}
}

func TestNativeFilePathRawKeepsItsRelPath(t *testing.T) {
	got, err := nativeFilePath("claude", "streaming-stdio", launchprofile.NativeFile{Kind: "raw", RelPath: "notes/a.md"})
	if err != nil || got != "notes/a.md" {
		t.Fatalf("raw path = %q, %v", got, err)
	}
	got, err = nativeFilePath("claude", "streaming-stdio", launchprofile.NativeFile{RelPath: ".tether/context.md"})
	if err != nil || got != ".tether/context.md" {
		t.Fatalf("kindless path = %q, %v", got, err)
	}
}

func TestNativeFilePathRefusesUnsafeSkillIDs(t *testing.T) {
	for _, id := range []string{"", ".", "..", "../x", "a/b", `a\b`} {
		if p, err := nativeFilePath("claude", "streaming-stdio", launchprofile.NativeFile{Kind: "skill", ID: id}); err == nil {
			t.Fatalf("skill id %q must be refused, got path %q", id, p)
		}
	}
}

func TestNativeFilePathUnknownProviderIsAnError(t *testing.T) {
	if p, err := nativeFilePath("no-such-provider", "streaming-stdio", launchprofile.NativeFile{Kind: "skill", ID: "mailbox"}); err == nil {
		t.Fatalf("unknown provider must fail, got %q", p)
	}
}
