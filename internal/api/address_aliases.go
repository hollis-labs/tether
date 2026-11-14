package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"slices"

	"github.com/hollis-labs/libs/mesh/core/messaging"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/messaging/channels"
	"github.com/hollis-labs/tether/internal/store"
)

// Address aliases (CW-20261114-0067): an alias is another name for a canonical
// address's mailbox. Sends resolve to the canonical address, mailbox reads of
// any form return the mail stored under every form, and a principal that owns
// one form may act as the others. Stored envelopes are never rewritten.
//
//	GET    /messages/aliases[?canonical=<urn>]   list
//	POST   /messages/aliases {alias, canonical}  add (operator)
//	DELETE /messages/aliases?alias=<urn>         remove (operator)

// AddressAliasStore is the alias directory. *store.Store satisfies it.
type AddressAliasStore interface {
	AddAddressAlias(ctx context.Context, alias, canonical, by string) (store.AddressAlias, int, error)
	RemoveAddressAlias(ctx context.Context, alias string) error
	ListAddressAliases(ctx context.Context, canonical string) ([]store.AddressAlias, error)
	CanonicalAddress(ctx context.Context, urn string) (string, error)
	AddressForms(ctx context.Context, urn string) ([]string, error)
	CanonicalizeMail(ctx context.Context, env *messaging.Envelope) error
}

func (s *Server) registerAddressAliasRoutes(router *http.ServeMux) {
	if s.Aliases == nil {
		return
	}
	router.HandleFunc("/messages/aliases", s.handleAddressAliases)
}

// canonicalAlias is the canonical address of urn, or urn itself when it is not
// an alias. A failed alias read leaves urn unchanged: access checks then see no
// extra form (they fail closed), and the store resolves again on a send.
func (s *Server) canonicalAlias(ctx context.Context, urn string) string {
	if s.Aliases == nil || urn == "" {
		return urn
	}
	c, err := s.Aliases.CanonicalAddress(ctx, urn)
	if err != nil {
		slog.Warn("address aliases: resolve failed", "err", err)
		return urn
	}
	return c
}

// aliasForms is every address of urn's mailbox, the canonical one first.
func (s *Server) aliasForms(ctx context.Context, urn string) []string {
	if s.Aliases == nil || urn == "" {
		return []string{urn}
	}
	forms, err := s.Aliases.AddressForms(ctx, urn)
	if err != nil || len(forms) == 0 {
		if err != nil {
			slog.Warn("address aliases: resolve failed", "err", err)
		}
		return []string{urn}
	}
	return forms
}

// sameAddress reports whether a and b name one mailbox: the same seat
// (seat_identity.go) or forms of one aliased address.
func (s *Server) sameAddress(ctx context.Context, a, b string) bool {
	if s.sameSeat(ctx, a, b) {
		return true
	}
	return s.canonicalAlias(ctx, s.canonicalSeat(ctx, a)) == s.canonicalAlias(ctx, s.canonicalSeat(ctx, b))
}

// ownsAliasOf reports whether p may act as another form of addr's mailbox. An
// alias is an identity statement made by the operator: whoever owns one form
// owns the mailbox.
func (s *Server) ownsAliasOf(ctx context.Context, p identity.Principal, addr string) bool {
	for _, form := range s.aliasForms(ctx, addr) {
		if form != addr && p.MayActAs(form) {
			return true
		}
	}
	return false
}

// guardMailbox is guardAs for the mailbox routes: after the claim is verified,
// as and to are also resolved from an alias to the canonical address, so a
// caller using an old address reads and acts on the one mailbox.
func (s *Server) guardMailbox(next http.HandlerFunc) http.HandlerFunc {
	return s.guardAs(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		changed := false
		for _, key := range []string{"as", "to"} {
			if v := q.Get(key); v != "" {
				if c := s.canonicalAlias(r.Context(), v); c != v {
					q.Set(key, c)
					changed = true
				}
			}
		}
		if changed {
			r.URL.RawQuery = q.Encode()
		}
		next(w, r)
	})
}

// canonicalizeMail resolves a mailbox envelope's alias addresses before the
// recipient checks and federation routing see them. Channel publications are
// left alone: their From is checked against the publisher's own address set.
func (s *Server) canonicalizeMail(ctx context.Context, env *messaging.Envelope) error {
	if s.Aliases == nil {
		return nil
	}
	if _, ok := channels.AddressName(env.To); ok {
		return nil
	}
	return s.Aliases.CanonicalizeMail(ctx, env)
}

// mayManageAliases: the operator credential. An alias redirects an address's
// mail and lets its owner act as the canonical address, so no agent, device or
// issued token may declare one. Only a daemon that verifies no credentials at
// all (identity mode off) accepts an unauthenticated caller, and then only on
// the local socket.
func (s *Server) mayManageAliases(r *http.Request) bool {
	if p, ok := verifiedPrincipal(r); ok {
		return identity.IsOperatorCredential(p)
	}
	return (s.IdentityMode == "" || s.IdentityMode == identity.Off) && !identity.IsRemote(r.Context())
}

// AddressAliasRequest is the POST /messages/aliases body.
type AddressAliasRequest struct {
	Alias     string `json:"alias"`
	Canonical string `json:"canonical"`
}

// AddressAliasResponse is the POST /messages/aliases answer. MergedMessages is
// how many messages already stored under the alias are now also listed in the
// canonical mailbox (they are not moved or rewritten).
type AddressAliasResponse struct {
	store.AddressAlias
	MergedMessages int `json:"merged_messages"`
}

func (s *Server) handleAddressAliases(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	switch r.Method {
	case http.MethodGet:
		canonical := r.URL.Query().Get("canonical")
		if canonical != "" {
			canonical = s.canonicalAlias(ctx, canonical)
		}
		rows, err := s.Aliases.ListAddressAliases(ctx, canonical)
		if err != nil {
			writeError(w, http.StatusInternalServerError, CodeInternalError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"aliases": rows})
	case http.MethodPost:
		if !s.mayManageAliases(r) {
			writeError(w, http.StatusForbidden, CodeForbidden, "only the operator may declare address aliases")
			return
		}
		var req AddressAliasRequest
		dec := json.NewDecoder(io.LimitReader(r.Body, 16<<10))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, CodeInvalidRequest, "invalid body: "+err.Error())
			return
		}
		if req.Alias == "" || req.Canonical == "" {
			writeError(w, http.StatusBadRequest, CodeInvalidRequest, "alias and canonical are required")
			return
		}
		by := ""
		if p, ok := verifiedPrincipal(r); ok {
			by = p.ID
		}
		row, merged, err := s.Aliases.AddAddressAlias(ctx, req.Alias, req.Canonical, by)
		if err != nil {
			if errors.Is(err, store.ErrAddressAliasInvalid) {
				writeError(w, http.StatusConflict, CodeConflict, err.Error())
				return
			}
			writeError(w, http.StatusInternalServerError, CodeInternalError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, AddressAliasResponse{AddressAlias: row, MergedMessages: merged})
	case http.MethodDelete:
		if !s.mayManageAliases(r) {
			writeError(w, http.StatusForbidden, CodeForbidden, "only the operator may remove address aliases")
			return
		}
		alias := r.URL.Query().Get("alias")
		if alias == "" {
			writeError(w, http.StatusBadRequest, CodeInvalidRequest, "alias query param required")
			return
		}
		if err := s.Aliases.RemoveAddressAlias(ctx, alias); err != nil {
			switch {
			case errors.Is(err, store.ErrAddressAliasNotFound):
				writeError(w, http.StatusNotFound, CodeNotFound, "no such alias "+alias)
			case errors.Is(err, store.ErrAddressAliasInvalid):
				writeError(w, http.StatusBadRequest, CodeInvalidRequest, err.Error())
			default:
				writeError(w, http.StatusInternalServerError, CodeInternalError, err.Error())
			}
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"removed": alias})
	default:
		writeError(w, http.StatusMethodNotAllowed, CodeMethodNotAllowed, "method not allowed")
	}
}

// aliasResolution names the canonical address and aliases of urn's mailbox, or
// ok=false when urn has no alias rows at all.
func (s *Server) aliasResolution(ctx context.Context, urn string) (canonical string, aliases []string, ok bool) {
	forms := s.aliasForms(ctx, urn)
	if len(forms) < 2 {
		return "", nil, false
	}
	return forms[0], slices.Clone(forms[1:]), true
}
