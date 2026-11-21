package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"

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
// The one exception is Chrispian (@archie's ruling): his forms, the operator
// address and every user address, are protected. No agent row ever counts as
// him: an address that is not protected never extends to a protected form
// through an alias, and only the operator credential or a user principal may
// hold a protected form or act on a mailbox that has one.
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
//
// Alias rows hold seat addresses in their catalog urn form (handleAddressAliases
// stores them so), so urn is mapped to its seat before the lookup and the answer
// after it: a caller using a seat's logical name resolves like any other.
func (s *Server) canonicalAlias(ctx context.Context, urn string) string {
	if s.Aliases == nil || urn == "" {
		return urn
	}
	seat := s.canonicalSeat(ctx, urn)
	c, err := s.Aliases.CanonicalAddress(ctx, seat)
	if err != nil {
		slog.Warn("address aliases: resolve failed", "err", err)
		return urn
	}
	if c == seat {
		return urn // not an alias: leave the caller's form to the seat helpers
	}
	return s.canonicalSeat(ctx, c)
}

// protectedForm reports whether urn is one of Chrispian's forms: the operator
// address or any user address.
func protectedForm(urn string) bool {
	return urn == identity.OperatorID || strings.HasPrefix(urn, "msg://user/")
}

// mayHoldProtected reports whether p may hold Chrispian's forms: the operator
// credential or a user principal. Never an agent, session, device or token.
func mayHoldProtected(p identity.Principal) bool {
	return identity.IsOperatorCredential(p) || (p.Verified && p.TokenID == "" && strings.HasPrefix(p.ID, "msg://user/"))
}

// reachableForms is aliasForms seen from urn: an address that is not protected
// does not reach a protected form through an alias (protectedForm).
func (s *Server) reachableForms(ctx context.Context, urn string) []string {
	forms := s.aliasForms(ctx, urn)
	if protectedForm(urn) {
		return forms
	}
	out := forms[:0:0]
	for _, f := range forms {
		if !protectedForm(f) {
			out = append(out, f)
		}
	}
	if len(out) == 0 {
		return []string{urn}
	}
	return out
}

// aliasForms is every address of urn's mailbox, the canonical one first.
func (s *Server) aliasForms(ctx context.Context, urn string) []string {
	if s.Aliases == nil || urn == "" {
		return []string{urn}
	}
	forms, err := s.Aliases.AddressForms(ctx, s.canonicalSeat(ctx, urn))
	if err != nil {
		slog.Warn("address aliases: resolve failed", "err", err)
	}
	if len(forms) < 2 {
		return []string{urn} // not aliased: exactly the address asked about
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

// actsFor reports whether actor, acting under its own address, acts for
// target's mailbox: sameAddress, except that an address that is not protected
// never acts for Chrispian's (protectedForm), whatever alias joins them. One
// way: his forms act for the agent addresses aliased into his mailbox.
func (s *Server) actsFor(ctx context.Context, actor, target string) bool {
	if !s.sameAddress(ctx, actor, target) {
		return false
	}
	if protectedForm(s.canonicalSeat(ctx, actor)) {
		return true
	}
	return !protectedForm(s.canonicalSeat(ctx, target)) && !protectedForm(s.canonicalAlias(ctx, target))
}

// ownsAliasOf reports whether p may act as another form of addr's mailbox. An
// alias is an identity statement made by the operator: whoever owns one form
// owns the mailbox, except that only a principal that may hold Chrispian's
// forms (mayHoldProtected) acts as one of them.
func (s *Server) ownsAliasOf(ctx context.Context, p identity.Principal, addr string) bool {
	if protectedForm(addr) && !mayHoldProtected(p) {
		return false
	}
	for _, form := range s.aliasForms(ctx, addr) {
		if form == addr || (protectedForm(form) && !mayHoldProtected(p)) {
			continue
		}
		if p.MayActAs(form) {
			return true
		}
	}
	return false
}

// protectedMailbox reports whether a verified principal that may not hold
// Chrispian's forms is acting on a mailbox that has one: acting on any form of
// it would read or settle his mail.
func (s *Server) protectedMailbox(r *http.Request, as string) bool {
	p, ok := verifiedPrincipal(r)
	if !ok || mayHoldProtected(p) {
		return false
	}
	return s.hasProtectedForm(r.Context(), as)
}

// hasProtectedForm reports whether as's mailbox has one of Chrispian's forms.
func (s *Server) hasProtectedForm(ctx context.Context, as string) bool {
	if as == "" {
		return false
	}
	for _, form := range s.aliasForms(ctx, as) {
		if protectedForm(form) {
			return true
		}
	}
	return false
}

// guardMailbox is guardAs for the mailbox routes: after the claim is verified,
// as and to are also resolved from an alias to the canonical address, so a
// caller using an old address reads and acts on the one mailbox.
func (s *Server) guardMailbox(next http.HandlerFunc) http.HandlerFunc {
	canonical := func(w http.ResponseWriter, r *http.Request) {
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
	}
	guarded := s.guardAs(func(w http.ResponseWriter, r *http.Request) {
		if s.protectedMailbox(r, r.URL.Query().Get("as")) {
			writeError(w, http.StatusForbidden, CodeForbidden, "this mailbox is the operator's; only the operator or a user principal may act on it")
			return
		}
		canonical(w, r)
	})
	return func(w http.ResponseWriter, r *http.Request) {
		if identity.ServiceEventsRequest(r.Context()) && serviceBoundMailboxRoute(r) {
			// The remote admission already checked the literal bound mailbox.
			// Do not expand its authority via mutable seat or address aliases.
			p, ok := verifiedPrincipal(r)
			as := r.URL.Query().Get("as")
			if !ok || !slices.Contains(p.Addresses, as) || s.protectedMailbox(r, as) {
				writeError(w, http.StatusForbidden, CodeForbidden, "mailbox is not a bound agent address")
				return
			}
			next(w, r.WithContext(store.WithExactMailbox(r.Context())))
			return
		}
		// Chrispian's own console reads his mailbox (@archie's ruling on #554): a device he paired, holding
		// read, may GET it (list, watch, thread, item). Never GET /messages/inbox, the destructive pull (it
		// marks delivery and settles read mail), never a send, read mark, archive, ack or consume (all POST),
		// and never an agent principal or an issued token.
		if r.Method == http.MethodGet && deviceReadRoute(r.URL.Path) && operatorReadDevice(r) && s.hasProtectedForm(r.Context(), r.URL.Query().Get("as")) {
			s.canonicalizeSeatParams(r)
			canonical(w, r)
			return
		}
		guarded(w, r)
	}
}

// serviceBoundMailboxRoute reports whether r is one of the mailbox routes that
// remote service JWTs may reach with a bound ?as= agent address (list, single
// message fetch, mark-read).
func serviceBoundMailboxRoute(r *http.Request) bool {
	if r.Method == http.MethodGet {
		if r.URL.Path == "/messages/list" {
			return true
		}
		id, ok := strings.CutPrefix(r.URL.Path, "/messages/")
		return ok && id != "" && !strings.Contains(id, "/") &&
			id != "inbox" && id != "subscribe" && id != "watch" && id != "notify" && id != "request" && id != "post" && id != "aliases" && id != "pending-replies" && id != "thread" && id != "retention"
	}
	return r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/read")
}

// deviceReadRoute is the allowlist of the device read exception: list, watch, thread and a single item.
// Everything else (the inbox pull, subscribe, the collection, every POST) goes through the normal guard.
func deviceReadRoute(path string) bool {
	switch {
	case path == "/messages/list", path == "/messages/watch", strings.HasPrefix(path, "/messages/thread/"):
		return true
	}
	id, ok := strings.CutPrefix(path, "/messages/")
	return ok && id != "" && !strings.Contains(id, "/") && id != "inbox" && id != "subscribe" && id != "notify" && id != "request" && id != "aliases"
}

// operatorReadDevice reports whether the request's verified principal is a device the operator paired that
// holds the read scope: Chrispian's own console (Tachyon). A worker-only device has no read scope.
func operatorReadDevice(r *http.Request) bool {
	p, ok := verifiedPrincipal(r)
	if !ok || p.Kind != "device" || p.TokenID != "" || p.CreatedBy != identity.OperatorID {
		return false
	}
	return slices.Contains(p.Scopes, identity.ScopeRead) || slices.Contains(p.Scopes, "*")
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
			canonical = s.canonicalSeat(ctx, s.canonicalAlias(ctx, canonical))
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
		// A seat is stored by its catalog urn, the address its mail is under
		// (seat_identity.go): a logical name on either side would point the alias
		// at, or from, a mailbox nobody reads.
		row, merged, err := s.Aliases.AddAddressAlias(ctx, s.canonicalSeat(ctx, req.Alias), s.canonicalSeat(ctx, req.Canonical), by)
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
		if err := s.Aliases.RemoveAddressAlias(ctx, s.canonicalSeat(ctx, alias)); err != nil {
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
