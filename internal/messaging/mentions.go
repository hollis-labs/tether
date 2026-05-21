// Package messaging hosts cross-component messaging helpers that don't
// fit naturally inside internal/store/messaging_store.go (the Store impl)
// or go-messaging (the library). v060-05 adds the @-mention parser that
// runs around SendToGroup commit — the daemon-side parsing surface
// defined by D6.
//
// Scope discipline (D6):
//
//   - This package owns ONLY @-parsing. ! and : are reserved namespace
//     for agent-side interpretation per D6 detail; the daemon transports
//     bytes verbatim and never inspects ! or :.
package messaging

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"

	gomsg "github.com/hollis-labs/go-messaging"

	"github.com/hollis-labs/tether/internal/registry"
)

// Tunables.
const (
	// MaxMentions caps how many @-tokens a single SendToGroup will
	// resolve + emit notices for. Per the sprint's review notes — guards
	// against malicious-blow-up payloads.
	MaxMentions = 32

	// SnippetMax is the max length of the body snippet included in each
	// emitted notice's payload (cuts off at SnippetMax bytes + ellipsis).
	SnippetMax = 240

	// SystemSenderURN is the from-field on emitted mention notices.
	// Reuses the agent kind so go-messaging.ParseURN at the messaging-
	// store boundary accepts it (the alternative `msg://system/...`
	// path is not in go-messaging v0.2.1's closed AddressKind enum).
	SystemSenderURN = "msg://agent/agent-mux/agt_mxsysmnt00"
)

// Pattern: `\@` (escaped, literal) OR `@<token>` where token is either
// a msg:// URN (with embedded slashes) or a short-form
// [a-z0-9_-]+ identifier. The leading `\@` group is captured + skipped
// to preserve the literal-escape semantic (D6 detail: `\@token` is sent
// verbatim, no resolution attempted).
var mentionPattern = regexp.MustCompile(`(\\@)|@(msg://[A-Za-z0-9._/+\-]+|[A-Za-z0-9_\-]+)`)

// Lookup is the subset of registry.Service this package needs. Defining
// a focused interface here lets tests pass a stub without spinning a
// full registry surface.
type Lookup interface {
	Lookup(ctx context.Context, urn string) (registry.Profile, error)
	// FindByDisplayName returns all profiles whose display_name == name
	// (excluding deprecated/archived). len(out)>1 → ambiguous.
	FindByDisplayName(ctx context.Context, name string) ([]registry.Profile, error)
}

// Sender is the subset of the messaging.Store contract this package
// needs to emit notice envelopes. Identical shape to go-messaging.Store
// .Send so the production wiring takes the real Store directly.
type Sender interface {
	Send(ctx context.Context, env gomsg.Envelope) (gomsg.Envelope, error)
}

// Parser implements registry.MentionParser. Holds a Lookup (for
// resolution) + a Sender (for notice emission) + an optional logger.
// Construct with NewParser and install on the registry Service via
// registry.WithMentionParser.
type Parser struct {
	lookup    Lookup
	sender    Sender
	logger    *slog.Logger
	systemURN string // override for tests; defaults to SystemSenderURN
}

// Option configures a Parser at construction time.
type Option func(*Parser)

// WithLogger sets the slog.Logger used for the warn-and-continue
// codepaths (unknown URN, dispatch failures). Default is slog.Default().
func WithLogger(l *slog.Logger) Option {
	return func(p *Parser) { p.logger = l }
}

// WithSystemSender overrides the from-URN on emitted notices. Tests use
// this to assert the dispatched envelope's from field without depending
// on the production system-sender constant.
func WithSystemSender(urn string) Option {
	return func(p *Parser) { p.systemURN = urn }
}

// NewParser constructs a Parser. lookup is the registry-side resolver;
// sender is the messaging-store the notices are written to.
func NewParser(lookup Lookup, sender Sender, opts ...Option) *Parser {
	p := &Parser{
		lookup:    lookup,
		sender:    sender,
		logger:    slog.Default(),
		systemURN: SystemSenderURN,
	}
	for _, opt := range opts {
		opt(p)
	}
	return p
}

// Parse scans payload for @-tokens, resolves them via the Lookup, and
// returns the unique resolved mentions. Implements registry.MentionParser.
//
// Semantics (sprint D6 detail + T-05 acceptance):
//   - `\@token` is escaped — the literal text passes through unchanged
//     and no mention is generated.
//   - `@<urn>` resolves directly. If the URN doesn't exist in the
//     registry, the mention is kept but flagged with empty
//     ResolvedURN — Dispatch will log + skip (sprint: "send anyway,
//     log warn — agent might intentionally reference something
//     deferred"). Wait — the sprint says "send anyway"; that means
//     send the GROUP MESSAGE anyway. The mention notice itself is
//     skipped (no recipient exists).
//   - `@<display_name>` short-form resolves via FindByDisplayName.
//     Zero matches → behave like unknown URN. >1 matches → return
//     *registry.ErrAmbiguousMention with the candidate URNs;
//     SendToGroup aborts the commit.
//   - Duplicate mentions (same URN twice) collapse to one Mention.
//   - Self-mention (a member mentioning their own URN) is emitted —
//     useful for "save for later" patterns.
//   - Bot-mention (mentioning the group's own URN, groupURN) is
//     silently dropped — no notice generated.
//   - Hard cap at MaxMentions resolved unique URNs; additional matches
//     are silently dropped (review-note hardening).
func (p *Parser) Parse(ctx context.Context, payload json.RawMessage, groupURN string) ([]registry.Mention, error) {
	if len(payload) == 0 {
		return nil, nil
	}
	matches := mentionPattern.FindAllStringSubmatch(string(payload), -1)
	if len(matches) == 0 {
		return nil, nil
	}
	seen := make(map[string]bool, len(matches)) // dedup key = resolved URN OR raw token if unresolved
	var out []registry.Mention
	for _, m := range matches {
		// m[1] is the escape capture (`\@`); if present, skip.
		if m[1] != "" {
			continue
		}
		token := m[2]
		if token == "" {
			continue
		}
		var mention registry.Mention
		mention.Token = token
		if strings.HasPrefix(token, "msg://") {
			// Trim any trailing JSON-string characters that the regex
			// over-eagerly grabbed (e.g. `,` or `}` in JSON wrappers).
			token = trimTrailingDelimiters(token)
			mention.Token = token
			mention.Source = "urn"
			if _, err := registry.ParseRegistryURN(token); err != nil {
				// Malformed URN — drop silently.
				continue
			}
			if token == groupURN {
				// Bot-mention — silent no-op.
				continue
			}
			if _, err := p.lookup.Lookup(ctx, token); err != nil {
				// Unknown URN — keep token, log + skip during Dispatch.
				// ResolvedURN stays empty so Dispatch can log "unknown mention".
				mention.ResolvedURN = ""
			} else {
				mention.ResolvedURN = token
			}
		} else {
			mention.Source = "display_name"
			profiles, err := p.lookup.FindByDisplayName(ctx, token)
			if err != nil {
				return nil, fmt.Errorf("mention lookup %q: %w", token, err)
			}
			switch len(profiles) {
			case 0:
				// Unknown short-form — drop silently (sprint: only URNs
				// produce a warn for unknown; short-forms that don't
				// resolve are just text that happens to start with @).
				continue
			case 1:
				if profiles[0].URN == groupURN {
					continue // bot-mention silently dropped
				}
				mention.ResolvedURN = profiles[0].URN
			default:
				candidates := make([]string, len(profiles))
				for i, pr := range profiles {
					candidates[i] = pr.URN
				}
				return nil, &registry.ErrAmbiguousMention{Token: token, Candidates: candidates}
			}
		}
		key := mention.ResolvedURN
		if key == "" {
			key = "TOKEN:" + mention.Token
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, mention)
		if len(out) >= MaxMentions {
			break
		}
	}
	return out, nil
}

// Dispatch emits one notice envelope per resolved mention with non-empty
// ResolvedURN. Errors (unknown URNs, send failures) are LOGGED and
// continue — per D11/D12 mention dispatch is fire-and-forget after the
// group message has already been committed. Implements
// registry.MentionParser.Dispatch.
func (p *Parser) Dispatch(ctx context.Context, gm registry.GroupMessage, mentions []registry.Mention) {
	snippet := bodySnippet(gm.Payload, SnippetMax)
	// Resolve the group's display_name once per Dispatch for the notice
	// subject ("Mention in <display_name>" per ADR-0042). Falls back to
	// the URN if Lookup fails — notices stay informative either way.
	groupLabel := gm.GroupURN
	if gp, err := p.lookup.Lookup(ctx, gm.GroupURN); err == nil && gp.DisplayName != "" {
		groupLabel = gp.DisplayName
	}
	for _, m := range mentions {
		if m.ResolvedURN == "" {
			p.logger.WarnContext(ctx, "mention skip: unresolved",
				"token", m.Token, "group", gm.GroupURN, "message_id", gm.ID)
			continue
		}
		payload, err := json.Marshal(map[string]any{
			"subject":      "Mention in " + groupLabel,
			"group":        gm.GroupURN,
			"message_id":   gm.ID,
			"group_seq":    gm.GroupSeq,
			"mentioned_by": gm.FromURN,
			"thread_id":    gm.ThreadID,
			"snippet":      snippet,
		})
		if err != nil {
			p.logger.WarnContext(ctx, "mention dispatch: marshal failed",
				"target", m.ResolvedURN, "err", err)
			continue
		}
		fromAddr, fromErr := gomsg.ParseURN(p.systemURN)
		toAddr, toErr := gomsg.ParseURN(m.ResolvedURN)
		if fromErr != nil || toErr != nil {
			p.logger.WarnContext(ctx, "mention dispatch: address parse failed",
				"from", p.systemURN, "fromErr", fromErr,
				"to", m.ResolvedURN, "toErr", toErr)
			continue
		}
		env := gomsg.Envelope{
			Kind:        "notice",
			From:        fromAddr,
			To:          toAddr,
			ThreadID:    gm.ThreadID,
			Payload:     payload,
			ContentType: "application/json",
			CreatedAt:   time.Now().UTC(),
		}
		if _, err := p.sender.Send(ctx, env); err != nil {
			p.logger.WarnContext(ctx, "mention dispatch: send failed",
				"target", m.ResolvedURN, "group", gm.GroupURN, "err", err)
		}
	}
}

// trimTrailingDelimiters strips JSON-string-context trailing chars
// (",}]\"`) from a URN candidate. The regex over-matches when a URN
// appears inside a JSON string ("`"@msg://...`"") and grabs the closing
// quote / comma / brace; this trims them off.
func trimTrailingDelimiters(s string) string {
	for len(s) > 0 {
		switch s[len(s)-1] {
		case '"', ',', '}', ']', '\\', '\'', '`':
			s = s[:len(s)-1]
		default:
			return s
		}
	}
	return s
}

// bodySnippet returns up to max bytes of the payload as a human-readable
// snippet for inclusion in mention notices. If payload is JSON with a
// `text` or `body` string field, that's used; otherwise the raw payload
// is truncated.
func bodySnippet(payload json.RawMessage, maxLen int) string {
	if len(payload) == 0 {
		return ""
	}
	// Try JSON object shape first.
	var obj map[string]any
	if err := json.Unmarshal(payload, &obj); err == nil {
		if v, ok := obj["text"].(string); ok {
			return truncateRunes(v, maxLen)
		}
		if v, ok := obj["body"].(string); ok {
			return truncateRunes(v, maxLen)
		}
	}
	return truncateRunes(string(payload), maxLen)
}

// truncateRunes returns s truncated so that the result is at most maxLen
// bytes of valid UTF-8 plus a trailing "…" ellipsis. Truncation happens
// on rune boundaries so multi-byte characters aren't split mid-sequence.
// maxLen is a byte budget, not a rune budget — keeps the caller's size
// cap stable regardless of input encoding density.
func truncateRunes(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	// Walk forward by rune until adding the next rune would exceed maxLen.
	end := 0
	for i := range s {
		if i > maxLen {
			break
		}
		end = i
	}
	return s[:end] + "…"
}
