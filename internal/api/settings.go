package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/hollis-labs/tether/internal/settings"
)

// SettingsService is the narrow interface api handlers need from internal/settings.
type SettingsService interface {
	GetEffectiveOnboarding(ctx context.Context, projectID, userID string) (settings.OnboardingSettings, error)
	GetOnboarding(ctx context.Context, scope settings.Scope, scopeID string) (*settings.OnboardingSettings, error)
	SetOnboarding(ctx context.Context, scope settings.Scope, scopeID string, ob settings.OnboardingSettings) error
}

func (s *Server) registerSettingsRoutes(mux *http.ServeMux) {
	if s.Settings == nil {
		return
	}
	mux.HandleFunc("/settings/onboarding", s.handleEffectiveOnboarding)
	mux.HandleFunc("/settings/onboarding/", s.handleScopedOnboarding)
}

// handleEffectiveOnboarding handles GET /settings/onboarding?project={p}&user={u}.
// Resolves the effective onboarding settings using the closest-wins cascade.
func (s *Server) handleEffectiveOnboarding(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, CodeMethodNotAllowed, "method not allowed")
		return
	}
	projectID := r.URL.Query().Get("project")
	userID := r.URL.Query().Get("user")

	eff, err := s.Settings.GetEffectiveOnboarding(r.Context(), projectID, userID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, CodeInternalError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, eff)
}

// handleScopedOnboarding handles GET and PUT /settings/onboarding/{scope}?scope_id={id}.
func (s *Server) handleScopedOnboarding(w http.ResponseWriter, r *http.Request) {
	scopeStr := strings.TrimPrefix(r.URL.Path, "/settings/onboarding/")
	if scopeStr == "" || strings.Contains(scopeStr, "/") {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest, "invalid settings scope path")
		return
	}

	scope := settings.Scope(scopeStr)
	if !settings.ValidScope(scope) {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest, "invalid settings scope")
		return
	}

	scopeID := r.URL.Query().Get("scope_id")
	if scope == settings.ScopeGlobal && scopeID != "" {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest, "global scope requires empty scope_id")
		return
	}
	if (scope == settings.ScopeProject || scope == settings.ScopeUser) && scopeID == "" {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest, string(scope)+" scope requires scope_id query param")
		return
	}

	switch r.Method {
	case http.MethodGet:
		ob, err := s.Settings.GetOnboarding(r.Context(), scope, scopeID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, CodeInternalError, err.Error())
			return
		}
		if ob == nil {
			writeError(w, http.StatusNotFound, CodeNotFound, "no onboarding settings configured for scope")
			return
		}
		writeJSON(w, http.StatusOK, ob)

	case http.MethodPut, http.MethodPost:
		var ob settings.OnboardingSettings
		if err := json.NewDecoder(r.Body).Decode(&ob); err != nil {
			writeError(w, http.StatusBadRequest, CodeInvalidRequest, "malformed JSON body")
			return
		}
		if err := s.Settings.SetOnboarding(r.Context(), scope, scopeID, ob); err != nil {
			writeError(w, http.StatusInternalServerError, CodeInternalError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})

	default:
		writeError(w, http.StatusMethodNotAllowed, CodeMethodNotAllowed, "method not allowed")
	}
}
