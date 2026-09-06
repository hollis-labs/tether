package app

import (
	"testing"

	"github.com/hollis-labs/agentkit/agentruntime/checkpoint"

	tethercheckpoint "github.com/hollis-labs/tether/internal/checkpoint"
	"github.com/hollis-labs/tether/internal/launch"
)

func TestResumeHintForCheckpoint_ProviderSessionID(t *testing.T) {
	hint := resumeHintForCheckpoint(&tethercheckpoint.Checkpoint{
		ProviderHintsJSON: `{"provider_session_id":"provider-123"}`,
	}, &launch.Plan{ProviderBrand: "claude", RuntimeKind: "streaming-stdio"})

	if hint.Support != checkpoint.ResumeNative {
		t.Fatalf("Support = %q, want %q", hint.Support, checkpoint.ResumeNative)
	}
	if hint.ProviderSessionID != "provider-123" {
		t.Fatalf("ProviderSessionID = %q", hint.ProviderSessionID)
	}
	if !hint.FallbackFreshBoot {
		t.Fatal("FallbackFreshBoot = false, want true")
	}
}

func TestResumeHintForCheckpoint_LegacySessionID(t *testing.T) {
	hint := resumeHintForCheckpoint(&tethercheckpoint.Checkpoint{
		ProviderHintsJSON: `{"session_id":"legacy-456"}`,
	}, &launch.Plan{ProviderBrand: "claude", RuntimeKind: "streaming-stdio"})

	if hint.ProviderSessionID != "legacy-456" {
		t.Fatalf("ProviderSessionID = %q", hint.ProviderSessionID)
	}
	if !hint.CanResumeNatively() {
		t.Fatal("CanResumeNatively = false, want true")
	}
}

func TestResumeParentSessionID_SetWhenCheckpointHasSource(t *testing.T) {
	got := resumeParentSessionID(&tethercheckpoint.Checkpoint{SourceSessionID: "sess-parent-1"})
	if !got.Valid || got.String != "sess-parent-1" {
		t.Fatalf("expected valid parent session id 'sess-parent-1', got %+v", got)
	}
}

func TestResumeParentSessionID_EmptyWhenCheckpointHasNoSource(t *testing.T) {
	got := resumeParentSessionID(&tethercheckpoint.Checkpoint{})
	if got.Valid {
		t.Fatalf("expected no parent session id for a checkpoint with no source session, got %+v", got)
	}
}

func TestResumeParentSessionID_NilCheckpoint(t *testing.T) {
	got := resumeParentSessionID(nil)
	if got.Valid {
		t.Fatalf("expected no parent session id for a nil checkpoint, got %+v", got)
	}
}

func TestResumeHintForCheckpoint_FreshBootFallback(t *testing.T) {
	hint := resumeHintForCheckpoint(&tethercheckpoint.Checkpoint{}, &launch.Plan{
		ProviderBrand: "codex",
		RuntimeKind:   "jsonrpc-stdio",
	})

	if hint.Support != checkpoint.ResumeFreshBoot {
		t.Fatalf("Support = %q, want %q", hint.Support, checkpoint.ResumeFreshBoot)
	}
	if !hint.ShouldFreshBoot() {
		t.Fatal("ShouldFreshBoot = false, want true")
	}
}
