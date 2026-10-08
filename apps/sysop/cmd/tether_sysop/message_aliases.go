package main

import (
	"github.com/hollis-labs/tether/internal/store"
	"net/http"
)

func (s *appServer) handleMessageAliases(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		var aliases []store.MessageAlias
		err := s.withStateReader(func(db *store.Store) error {
			var err error
			aliases, err = db.ListMessageAliases()
			return err
		})
		if err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error(), "message": err.Error()})
			return
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
