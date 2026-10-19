package events

import (
	"context"

	"github.com/hollis-labs/libs/harness/core/adapters/runtimeevents/catalog"
	"github.com/hollis-labs/libs/harness/core/eventstage"
	"github.com/hollis-labs/libs/security/redact"
)

// AdmissionClass is a source compatibility contract, not a classification
// inferred from a payload or a producer's sanitized flag.
type AdmissionClass string

const (
	Metadata      AdmissionClass = "metadata"
	Compatibility AdmissionClass = "compatibility"
	Unregistered  AdmissionClass = "unregistered"
)

type admissionPolicy struct {
	class     AdmissionClass
	sanitizer *redact.Sanitizer
}

func pipelinePolicies() map[string]admissionPolicy {
	policies := map[string]admissionPolicy{}
	add := func(class AdmissionClass, kind string, paths ...string) {
		mode := redact.Allowlist
		if class == Compatibility {
			mode = redact.Denylist
		}
		s, err := redact.NewSanitizer(redact.Policy{Mode: mode, AllowedPaths: paths, Sensitivity: redact.Private})
		if err != nil {
			panic("events: invalid built-in sanitize policy")
		}
		policies[kind] = admissionPolicy{class: class, sanitizer: s}
	}
	add(Metadata, KindSessionUsage, "/input_tokens", "/output_tokens", "/cache_read_tokens", "/cache_write_tokens", "/reasoning_tokens", "/turns", "/model", "/final", "/source", "/turn_id")
	add(Metadata, KindSessionHealthChanged, "/from", "/to", "/reasons/*", "/since")
	add(Metadata, catalog.KindToolCallStarted, "/tool", "/server", "/args_bytes", "/args_schema_fp")
	add(Metadata, catalog.KindToolCallFinished, "/tool", "/server", "/args_bytes", "/args_schema_fp", "/ok", "/error_class", "/duration_ms", "/gateway_ms", "/forward_ms", "/result_bytes")
	add(Metadata, KindSessionStalled, "/reason", "/denied")
	add(Metadata, EventTypeMachineAuthorityDecision, "/server", "/tool", "/class", "/mode", "/allowed", "/session_id", "/timestamp")
	// Conservative compatibility entries retain every local-consumer byte. Their
	// denylist sanitizer runs only on a diagnostic copy, even in enforce mode.
	for _, kind := range []string{
		KindRoutingReplyDelivered, KindRoutingReplyUndeliverable, KindSessionTurnInterruptRequested, KindSessionTurnInterruptCompleted,
		catalog.KindMessageSent, catalog.KindMessageQueued, catalog.KindMessageRedriven, catalog.KindMessageFailed, catalog.KindMessageDeadLettered, catalog.KindMessageConsumed,
		"session.request_status", "session.force_closed", "sandbox_rule_unenforced",
		KindStreamGap, KindSessionTurnRouted, KindSessionShimRetired, KindSessionTurnOutput, KindSessionStateChanged,
		KindBrokerEnvelopeCreated, KindBrokerEnvelopeReplied, KindDaemonStarted, KindDaemonShutdownStarted, KindDaemonShutdownCompleted,
		KindDaemonShutdownSessionsEnded, KindDaemonSessionsSwept, KindDaemonExecutionPolicyChanged, KindAIBudgetRejected,
		KindSessionBootDirPlanted, KindProviderSessionLost, KindProviderPermissionDenied, KindSessionShimStatus,
		EventTypeToolCallStart, EventTypeToolCallEnd,
		"session.reaper", "session.activity", "session.replaced_by", "session.native_resumed", "session.boot_recovery",
	} {
		add(Compatibility, kind)
	}
	return policies
}

type payloadPolicies map[string]admissionPolicy

func (p payloadPolicies) SanitizePayload(_ context.Context, kind string, payload []byte) ([]byte, eventstage.PayloadReport, error) {
	policy, ok := p[kind]
	if !ok {
		return nil, eventstage.PayloadReport{ErrorClass: "unknown_kind"}, nil
	}
	out, report := policy.sanitizer.Sanitize(payload)
	return out, eventstage.PayloadReport{Redactions: report.Redactions, Sensitivity: string(report.Sensitivity), PII: report.PII, ErrorClass: string(report.ErrorClass)}, nil
}
