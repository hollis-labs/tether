package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/hollis-labs/go-messaging"
	"github.com/hollis-labs/tether/internal/store"
	"github.com/hollis-labs/tether/internal/userprofile"
)

func (s *appServer) profilePath() (string, error) {
	if s.userProfilePath != "" {
		return s.userProfilePath, nil
	}
	return userprofile.Path()
}

func (s *appServer) loadUserProfile() (userprofile.Profile, error) {
	path, err := s.profilePath()
	if err != nil {
		return userprofile.Profile{}, err
	}
	return userprofile.Load(path)
}

func resolveProfileAddress(p userprofile.Profile, db *store.Store, value string) (messaging.Address, error) {
	a, err := p.Resolve(value)
	if err != nil {
		return db.ResolveMessageAddress(value)
	}
	if !strings.HasPrefix(strings.TrimSpace(value), "msg://") {
		other, dbErr := db.ResolveMessageAddress(value)
		if dbErr == nil && other.URN() != a.URN() {
			return messaging.Address{}, fmt.Errorf("ambiguous alias %q", value)
		}
	}
	return a, nil
}

// The client supplies only a value. Identity and the messaging.from_default
// preference key come from the local profile, never request headers or keys.
func (s *appServer) handleMessageProfile(w http.ResponseWriter, r *http.Request) {
	s.userProfileMu.Lock()
	defer s.userProfileMu.Unlock()
	p, err := s.loadUserProfile()
	if err != nil {
		writeProfileError(w, http.StatusServiceUnavailable, err)
		return
	}
	if r.Method == http.MethodGet {
		writeJSON(w, http.StatusOK, p)
		return
	}
	if r.Method != http.MethodPost {
		writeProfileError(w, http.StatusMethodNotAllowed, errors.New("method not allowed"))
		return
	}
	var req struct {
		FromDefault *string `json:"from_default"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeProfileError(w, http.StatusBadRequest, err)
		return
	}
	if err := dec.Decode(new(any)); !errors.Is(err, io.EOF) {
		writeProfileError(w, http.StatusBadRequest, errors.New("expected one preference value"))
		return
	}
	if req.FromDefault == nil {
		writeProfileError(w, http.StatusBadRequest, errors.New("from_default is required"))
		return
	}
	value := strings.TrimSpace(*req.FromDefault)
	if value != "" {
		a, resolveErr := p.Resolve(value)
		if !strings.HasPrefix(value, "msg://") {
			resolveErr = s.withStateReader(func(db *store.Store) error {
				var err error
				a, err = resolveProfileAddress(p, db, value)
				return err
			})
			if errors.Is(resolveErr, errStateDBUnset) {
				a, resolveErr = p.Resolve(value)
			}
		}
		if resolveErr != nil {
			writeProfileError(w, http.StatusBadRequest, resolveErr)
			return
		}
		value = a.URN()
	}
	p.Messaging.FromDefault = value
	path, err := s.profilePath()
	if err == nil {
		err = userprofile.Save(path, p)
	}
	if err != nil {
		writeProfileError(w, http.StatusServiceUnavailable, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func writeProfileError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error(), "message": err.Error()})
}
