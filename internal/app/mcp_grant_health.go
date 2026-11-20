package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/hollis-labs/tether/internal/store"
	"log/slog"
	"sync"

	messaging "github.com/hollis-labs/libs/mesh/core/messaging"
	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/mcpgateway"
	"github.com/hollis-labs/tether/internal/registry"
)

const MCPGrantResumeHint = "Unavailable servers were dropped; remaining servers and Tether stay available. Resume or relaunch after correcting the catalog to refresh the saved grant."

// MCPGrantImpacts checks every running session against the candidate catalog,
// before it is installed. Saved snapshots remain untouched.
func (s *Service) MCPGrantImpacts(ctx context.Context, candidate *config.Catalog) ([]config.SessionMCPGrantImpact, error) {
	if s.Store == nil {
		return nil, nil
	}
	rows, err := s.Store.DB().QueryContext(ctx, `SELECT id FROM sessions WHERE state='running' ORDER BY id`)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return nil, err
	}
	var impacts []config.SessionMCPGrantImpact
	for _, id := range ids {
		policy, err := s.Store.SessionMCPPolicy(ctx, id)
		if err != nil {
			if errors.Is(err, store.ErrSessionMCPPolicyUnavailable) || errors.Is(err, mcpgateway.ErrInvalidSessionMCPPolicy) || errors.Is(err, mcpgateway.ErrSessionMCPIdentity) {
				continue
			}
			return nil, fmt.Errorf("session %s MCP preflight: %w", id, err)
		} // Invalid snapshots cannot acquire a narrowed grant.
		_, issues := candidate.NarrowMCPGrant(policy.Servers)
		if len(issues) > 0 {
			impacts = append(impacts, config.SessionMCPGrantImpact{SessionID: id, Servers: issues, Hint: MCPGrantResumeHint})
		}
	}
	return impacts, nil
}

// MCPGrantNotifier emits one agent notice per saved policy and narrowed set in
// this daemon lifetime. Failed deliveries are retried on a subsequent request.
func (s *Service) MCPGrantNotifier() func(context.Context, mcpgateway.SessionPolicy, []config.MCPGrantIssue) {
	var mu sync.Mutex
	seen := map[string]bool{}
	return func(ctx context.Context, policy mcpgateway.SessionPolicy, issues []config.MCPGrantIssue) {
		raw, _ := json.Marshal(issues)
		key := policy.SessionID + ":" + policy.Digest + ":" + string(raw)
		mu.Lock()
		defer mu.Unlock()
		if seen[key] || s.Store == nil {
			return
		}
		target := registry.LogicalAgentBindingTarget(policy.AgentID)
		to, err := messaging.ParseURN(target)
		if err != nil {
			slog.WarnContext(ctx, "MCP grant notice unavailable", "session", policy.SessionID, "reason", err.Error())
			return
		}
		payload, _ := json.Marshal(map[string]any{"subject": "MCP grant narrowed", "body": fmt.Sprintf("Session %s MCP grant narrowed: %s. %s", policy.SessionID, raw, MCPGrantResumeHint), "code": "mcp_grant_stale", "session_id": policy.SessionID, "servers": issues})
		_, err = s.Store.MessagingStore().Send(ctx, messaging.Envelope{Kind: messaging.MsgKindNotice, From: messaging.Address{Kind: messaging.KindService, Authority: "local", ID: "tether"}, To: to, Payload: payload})
		if err != nil {
			slog.WarnContext(ctx, "MCP grant notice failed", "session", policy.SessionID, "reason", err.Error())
			return
		}
		seen[key] = true
	}
}

// MCPGrantHealth describes only the authenticated caller's saved grant.
func (s *Service) MCPGrantHealth(ctx context.Context, id string) *config.SessionMCPGrantImpact {
	if s.Store == nil {
		return nil
	}
	policy, err := s.Store.SessionMCPPolicy(ctx, id)
	if err != nil {
		return nil
	}
	cat := s.CurrentCatalog()
	if s.CatalogRoot != "" {
		if fresh, err := config.LoadLayered(s.CatalogRoot); err == nil {
			cat = fresh
		}
	}
	if cat == nil {
		return nil
	}
	_, issues := cat.NarrowMCPGrant(policy.Servers)
	if len(issues) == 0 {
		return nil
	}
	return &config.SessionMCPGrantImpact{SessionID: id, Servers: issues, Hint: MCPGrantResumeHint}
}
