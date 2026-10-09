package main

import (
	"errors"
	"net/http"
	"strings"

	"github.com/hollis-labs/tether/internal/store"
)

func (s *appServer) handleMessageAliases(w http.ResponseWriter, r *http.Request) {
	s.userProfileMu.Lock()
	defer s.userProfileMu.Unlock()
	profile, err := s.loadUserProfile()
	if err != nil {
		writeProfileError(w, http.StatusServiceUnavailable, err)
		return
	}
	if r.Method == http.MethodGet {
		var aliases []store.MessageAlias
		err := s.withStateReader(func(db *store.Store) error {
			var err error
			aliases, err = db.ListMessageAliases()
			return err
		})
		if err != nil && !errors.Is(err, errStateDBUnset) {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error(), "message": err.Error()})
			return
		}
		for _, local := range profile.Aliases {
			for _, existing := range aliases {
				if strings.EqualFold(local.Alias, existing.Alias) && local.URN != existing.URN {
					writeProfileError(w, http.StatusServiceUnavailable, errors.New("ambiguous user profile alias"))
					return
				}
			}
			// The local record supplies the display alias for its addresses.
			filtered := aliases[:0]
			for _, existing := range aliases {
				if existing.URN != local.URN {
					filtered = append(filtered, existing)
				}
			}
			aliases = append(filtered, store.MessageAlias{URN: local.URN, Alias: local.Alias})
		}
		writeJSON(w, http.StatusOK, map[string]any{"aliases": aliases})
		return
	}
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	var a store.MessageAlias
	if err := decodeJSONBody(r, &a, false); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error(), "message": err.Error()})
		return
	}
	for _, local := range profile.Aliases {
		if local.URN == a.URN || strings.EqualFold(local.Alias, a.Alias) {
			writeProfileError(w, http.StatusBadRequest, errors.New("this address or alias is configured in the local user profile"))
			return
		}
	}
	db, err := s.openStateDB()
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error(), "message": err.Error()})
		return
	}
	defer func() { _ = db.Close() }()
	if err := db.SetMessageAlias(a.URN, a.Alias); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error(), "message": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, a)
}
