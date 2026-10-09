//go:build !windows

package app

import (
	"encoding/json"
	"errors"
	"os"
	"time"

	"github.com/google/uuid"
	"github.com/hollis-labs/substrate/harness/adapters/agentsessions"
	gop "github.com/hollis-labs/substrate/harness/adapters/provider"
	pevents "github.com/hollis-labs/substrate/harness/adapters/provider/events"
	"github.com/hollis-labs/substrate/harness/adapters/runtimeevents"
	llmtypes "github.com/hollis-labs/substrate/llm-core/llmtypes"
	"github.com/hollis-labs/tether/internal/shimagy"
	"github.com/hollis-labs/tether/internal/shimhost"
)

func agyWorkerArgv(binary string, opts agentsessions.StartOptions, systemPrompt string, command []string) ([]string, error) {
	if opts.Launch == nil {
		return nil, errors.New("AGY launch template unavailable")
	}
	template := opts.Launch.Clone()
	// Environment belongs to the canonical shim Launch, never a duplicated
	// command argument. Only the turn argv projection is needed by the worker.
	template.Convention.Env = nil
	cfg := shimagy.Config{Binary: binary, Launch: *template, ExtraArgs: opts.ExtraArgs, SystemPrompt: systemPrompt, ResumeID: opts.SessionIDPreset}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(cfg)
	if err != nil || len(encoded) > shimagy.MaxConfig {
		return nil, errors.New("AGY worker launch exceeds its argument budget")
	}
	if len(command) == 0 {
		worker, err := os.Executable()
		if err != nil {
			return nil, err
		}
		command = []string{worker, "shim-agy"}
	}
	return append(append([]string(nil), command...), "--config", string(encoded)), nil
}

func (s *Service) shimHostedBridgeRuntime(sessionID, id, brand string, command []string, caps agentsessions.Capabilities) (agentsessions.Runtime, error) {
	if brand != "antigravity" {
		return s.shimBridgeRuntime(sessionID, id, command, caps)
	}
	caps.StreamingStdio = true
	adapter := &shimAGYAdapter{AntigravityAdapter: gop.NewAntigravityAdapter(), service: s, sessionID: sessionID}
	adapter.Binary = command[0]
	return agentsessions.NewFromAdapter(agentsessions.AdapterRuntimeConfig{ID: id, Kind: "cli", Adapter: adapter, Caps: caps})
}

func agyBridgeOptions(opts agentsessions.StartOptions, command []string, receipt shimhost.Receipt, attach bool) (agentsessions.StartOptions, error) {
	opts = shimBridgeOptions(opts, command, receipt, attach)
	if opts.BootMode == "stdin" && opts.BootPrompt != "" {
		frame, err := frameUserMessage(opts.BootPrompt)
		if err != nil {
			return opts, err
		}
		opts.BootPrompt = string(frame) + "\n"
	}
	if len(opts.FirstTurnPayload) > 0 {
		frame, err := frameUserMessage(string(opts.FirstTurnPayload))
		if err != nil {
			return opts, err
		}
		opts.FirstTurnPayload = frame
	}
	return opts, nil
}

// AGY's native result identifies a conversation, not a unique turn. The
// hosted worker adds a per-turn UUID before the canonical journal append.
// Only that exact durable published result suppresses a replayed completion.
type shimAGYAdapter struct {
	*gop.AntigravityAdapter
	service   *Service
	sessionID string
	duplicate bool
	turnID    string
	sequence  uint64
}

func (a *shimAGYAdapter) ParseLine(line []byte) ([]llmtypes.StreamEvent, error) {
	var result struct {
		Event string `json:"event"`
		UUID  string `json:"uuid"`
	}
	a.duplicate = false
	identity := ""
	if json.Unmarshal(line, &result) == nil && result.Event != "" {
		identity = result.UUID
		if identity == "" {
			a.duplicate = true // Also suppress the typed terminal on refusal.
			return nil, errors.New("hosted AGY result identity unavailable")
		}
	}
	if identity != "" {
		ctx, cancel := a.service.outputPersistenceContext()
		published, err := a.service.Store.HasPublishedProviderResult(ctx, a.sessionID, identity)
		cancel()
		if err == nil {
			a.duplicate = published
		}
		// An unavailable dedup read is not proof of prior publication. Keep
		// the exact result identity and let the existing durable output retry
		// journal handle persistence, rather than silently consume the result.
	}
	if a.duplicate {
		return nil, nil
	}
	if value, ok := a.service.turnOutputs.Load(a.sessionID); ok {
		output := value.(*sessionTurnOutput)
		if identity != "" && a.turnID != identity {
			// A real accepted worker turn opens an explicit reducer identity;
			// equal final text on separate turns is never a replay identity.
			a.sequence++
			output.observeRuntime(runtimeevents.Event{SchemaVersion: runtimeevents.SchemaVersion, ID: uuid.NewString(), Time: time.Now(), SessionID: a.sessionID, TurnID: identity, Sequence: a.sequence, Kind: runtimeevents.KindTurnStarted, Process: runtimeevents.Process{Provider: "antigravity", Runtime: "shim-agy"}, Source: runtimeevents.Source{Channel: runtimeevents.ChannelStdio, Confidence: runtimeevents.ConfidenceExact}})
			a.turnID = identity
		}
		output.mu.Lock()
		if result.Event == "result" {
			output.providerResultID = identity
		} else {
			output.providerResultID = ""
		}
		output.mu.Unlock()
	}
	return a.AntigravityAdapter.ParseLine(line)
}

func (a *shimAGYAdapter) ParseLineEvents(line []byte) ([]pevents.Event, error) {
	if a.duplicate {
		return nil, nil
	}
	return a.AntigravityAdapter.ParseLineEvents(line)
}
