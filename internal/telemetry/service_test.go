package telemetry

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/hollis-labs/tether/internal/callcontext"
	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/redact"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

type capture struct{ rows []events.Event }

func (c *capture) Publish(_ context.Context, ev events.Event) error {
	c.rows = append(c.rows, ev)
	return nil
}

func TestServiceKeepsResolvedCredentialIdentityWithoutSessionClaims(t *testing.T) {
	for _, kind := range []string{"operator", "service"} {
		t.Run(kind, func(t *testing.T) {
			s := Service{}
			ctx := callcontext.WithSnapshot(context.Background(), callcontext.Snapshot{Source: "daemon", PrincipalID: kind, PrincipalKind: kind, SessionID: "claimed", AgentURN: "claimed", WorkstreamID: "claimed"})
			ctx, o := s.Start(ctx, Call{Name: "read", ClaimedSessionID: "claimed"})
			end := s.End(ctx, o, Outcome{OK: true})
			want := callcontext.Snapshot{Source: "daemon", PrincipalID: kind, PrincipalKind: kind}
			if end.Attribution != want || end.ClaimedSessionID != "claimed" {
				t.Fatalf("credential/claim separation: %+v", end)
			}
		})
	}
}

func TestServiceBoundsLargeToolNamesInBothDurablePhases(t *testing.T) {
	for _, size := range []int{1 << 20, 8 << 20} {
		publisher := &capture{}
		s := Service{Publisher: publisher}
		name := strings.Repeat("€", size/3) + strings.Repeat("x", size%3)
		ctx, observation := s.Start(context.Background(), Call{Name: name})
		end := s.End(ctx, observation, Outcome{Class: events.ToolErrorDenied, Error: "denied"})
		if len(end.ToolName) > MaxToolNameBytes || !utf8.ValidString(end.ToolName) || !strings.Contains(end.ToolName, "[sha256:") {
			t.Fatalf("recorded name: %q", end.ToolName)
		}
		for _, row := range publisher.rows {
			var call events.ToolCallEvent
			if err := json.Unmarshal([]byte(row.PayloadJSON), &call); err != nil {
				t.Fatal(err)
			}
			if call.ToolName != end.ToolName || len(row.PayloadJSON) > 1024 {
				t.Fatal("large name retained or phase mismatch")
			}
		}
	}
}
func TestServiceRecordsVerifiedMetadataAndTypedOutcome(t *testing.T) {
	previous := otel.GetTracerProvider()
	provider := sdktrace.NewTracerProvider()
	otel.SetTracerProvider(provider)
	t.Cleanup(func() { otel.SetTracerProvider(previous); _ = provider.Shutdown(context.Background()) })
	publisher := &capture{}
	secrets := &redact.Set{}
	secrets.Add("canary-secret")
	s := Service{Publisher: publisher, Secrets: secrets}
	ctx := callcontext.WithSnapshot(context.Background(), callcontext.Snapshot{Verified: true, PrincipalID: "principal", AgentURN: "agent", WorkstreamID: "work"})
	ctx, o := s.Start(ctx, Call{Name: "read", Server: "upstream", ArgsBytes: 42, Profile: "reader", Mode: "search"})
	finish := Forward(ctx)
	finish()
	SetErrorClass(ctx, events.ToolErrorDenied)
	end := s.End(ctx, o, Outcome{Error: "canary-secret " + strings.Repeat("€", 5000), ResultBytes: 123})
	if end.Attribution.PrincipalID != "principal" || end.Attribution.WorkstreamID != "work" || end.ArgsBytes != 42 || end.ResultBytes != 123 || end.Profile != "reader" || end.DiscoveryMode != "search" {
		t.Fatalf("metadata: %+v", end)
	}
	if end.ErrorClass != events.ToolErrorDenied || !end.ErrorTruncated || len(end.Error) > events.MaxToolCallErrorBytes || strings.Contains(end.Error, "canary-secret") {
		t.Fatalf("error: %+v", end)
	}
	if end.TraceID == "" || end.SpanID == "" || end.GatewayMs < 0 || end.ForwardMs < 0 {
		t.Fatalf("trace/timing: %+v", end)
	}
	if len(publisher.rows) != 2 || publisher.rows[1].Kind != events.EventTypeToolCallEnd {
		t.Fatalf("published: %+v", publisher.rows)
	}
	var roundtrip events.ToolCallEvent
	if err := json.Unmarshal([]byte(publisher.rows[1].PayloadJSON), &roundtrip); err != nil {
		t.Fatal(err)
	}
	if roundtrip.ErrorClass != events.ToolErrorDenied {
		t.Fatal(roundtrip)
	}
}
func TestServiceRejectsUnverifiedAttributionAndClassifiesTimeout(t *testing.T) {
	s := Service{}
	ctx := callcontext.WithSnapshot(context.Background(), callcontext.Snapshot{PrincipalID: "forged", AgentURN: "forged", WorkstreamID: "forged"})
	ctx, o := s.Start(ctx, Call{Name: "read"})
	end := s.End(ctx, o, Outcome{Class: ErrorClass(errors.Join(errors.New("upstream"), context.DeadlineExceeded)), Error: "deadline"})
	if end.Attribution.PrincipalID != "" || end.ErrorClass != events.ToolErrorTimeout {
		t.Fatal(end)
	}
	ctx, o = s.Start(ctx, Call{Name: "ok"})
	end = s.End(ctx, o, Outcome{OK: true})
	if end.ErrorClass != "" || end.Error != "" {
		t.Fatal(end)
	}
}
