package store

// joint is a transaction that writes more than one class. It stays atomic while its classes share a
// store; the slice it names redesigns it so that no transaction spans two classes (plan section 13.7)
// before either class moves.
type joint struct {
	classes []Class
	slice   string // the slice that redesigns it, and the plan 13.7 pattern
	old     string // where the old store runs it (main f6c37bfa), for the port
}

// joints is the allowlist for Stores.Joint: the cross-class transactions of the per-table classification
// (CW-20261114-0149), named after the old function. A joint not listed here is refused. The sysop
// tool's deleteSessionsByID writes the same tables but is not hub code, so it is not listed.
var joints = map[string]joint{
	// B-1: event identity in A, events in B.
	"BackfillEventIDs": {classes: []Class{A, B}, slice: "S8 (B-1: high-water mark reserved in A)", old: "internal/store/event_identity.go:99"},

	// B-4: the device-use audit row goes through the bounded event batcher; auth never waits on B.
	"RecordDeviceUse": {classes: []Class{A, B}, slice: "S3 (B-4)", old: "internal/identity/devices.go:132"},

	// B-5: session state (C) revokes session credentials (A); revocation becomes an explicit, idempotent
	// A write after the C write, retried from the C state on start.
	"CloseGhostSession":             {classes: []Class{A, B, C}, slice: "S9 (B-5, B-6)", old: "internal/store/ghosts.go:54"},
	"RecordReaperOrphan":            {classes: []Class{A, B, C}, slice: "S9 (B-5, B-6)", old: "internal/store/session_reaper.go:151"},
	"markSessionRecovery":           {classes: []Class{A, C}, slice: "S9 (B-5)", old: "internal/store/session_recovery.go:40"},
	"CompleteSessionShim":           {classes: []Class{A, C}, slice: "S9 (B-5)", old: "internal/store/session_shims.go:189"},
	"ReconcileRetirement":           {classes: []Class{A, C}, slice: "S9 (B-5)", old: "internal/store/shim_retirement.go:139"},
	"PrepareTeamReplacementTx":      {classes: []Class{A, B, C}, slice: "S9 (B-5, B-6)", old: "internal/app/team_session_replacement.go:55"},
	"PrepareCodexTeamReplacementTx": {classes: []Class{A, B, C}, slice: "S9 (B-5, B-6)", old: "internal/app/team_codex_replacement.go:133"},

	// B-6: the events outbox is written inside domain transactions; outbox rows stay in the domain's
	// class and event fan-out reads the outbox.
	"AttachChannelMessage":             {classes: []Class{B, C}, slice: "S8 (B-6)", old: "internal/store/channels_attach.go:66"},
	"insertEnvironmentRequestEnvelope": {classes: []Class{B, C}, slice: "S8 (B-6)", old: "internal/store/environment_requests.go:41"},
	"insertWorkStateEnvelope":          {classes: []Class{B, C}, slice: "S8 (B-6)", old: "internal/store/environment_work_state.go:22"},
	"RecordWorkerOutput":               {classes: []Class{B, C}, slice: "S8 (B-6)", old: "internal/store/session_reaper.go:228"},
	"DrainMessageEvents":               {classes: []Class{B, C}, slice: "S8 (B-6)", old: "internal/store/message_events.go:132"},

	// The retention sweep: one batch delete, then its receipt in retention_audit (B).
	"DeleteEventHistoryBefore": {classes: []Class{B, C}, slice: "S8 (retention)", old: "internal/store/event_retention.go:59"},

	// B-7: the fabric repository writes A and C in one transaction; the A rows are written first, then
	// C, idempotently.
	"ApplyImport":  {classes: []Class{A, C}, slice: "S5 (B-7)", old: "internal/fabricenrollment/import.go:239"},
	"EnrollActor":  {classes: []Class{A, C}, slice: "S5 (B-7)", old: "internal/fabricenrollment/service.go:163"},
	"RebindAgent":  {classes: []Class{A, C}, slice: "S5 (B-7)", old: "internal/fabricenrollment/service.go:197"},
	"RetireActor":  {classes: []Class{A, C}, slice: "S5 (B-7)", old: "internal/fabricenrollment/service.go:248"},
}
