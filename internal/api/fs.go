package api

import (
	"encoding/json"
	"net/http"
	"os"

	"github.com/hollis-labs/tether/internal/setup"
)

// FSValidateRequest is the JSON body for POST /fs/validate.
type FSValidateRequest struct {
	Path string `json:"path"`
	Kind string `json:"kind"` // "file" | "dir" | "executable"
}

// FSValidateResponse is the JSON response for POST /fs/validate.
type FSValidateResponse struct {
	Exists     bool   `json:"exists"`
	Executable *bool  `json:"executable,omitempty"` // only when kind=executable
	Resolved   string `json:"resolved,omitempty"`
	Note       string `json:"note,omitempty"`
}

// FSDetectResponse is the JSON response for GET /fs/detect?brand=<name>.
type FSDetectResponse struct {
	Brand  string `json:"brand"`
	Found  bool   `json:"found"`
	Path   string `json:"path,omitempty"`
	Source string `json:"source"`
}

func (s *Server) registerFSRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/fs/validate", s.handleFSValidate)
	mux.HandleFunc("/fs/detect", s.handleFSDetect)
}

func (s *Server) handleFSValidate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req FSValidateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	if req.Path == "" {
		writeJSON(w, http.StatusOK, FSValidateResponse{
			Exists: false,
			Note:   "path is empty",
		})
		return
	}

	expanded := expandPath(req.Path)
	info, err := os.Stat(expanded) //nolint:gosec // G304: user-provided path, read-only stat check
	if err != nil {
		writeJSON(w, http.StatusOK, FSValidateResponse{
			Exists: false,
			Note:   "not found",
		})
		return
	}

	resp := FSValidateResponse{
		Exists:   true,
		Resolved: expanded,
	}

	switch req.Kind {
	case "dir":
		if !info.IsDir() {
			resp.Exists = false
			resp.Note = "exists but is not a directory"
		}
	case "executable":
		executable := !info.IsDir() && info.Mode()&0o111 != 0
		resp.Executable = &executable
		if !executable {
			resp.Note = "file is not executable"
		}
	case "file":
		if info.IsDir() {
			resp.Exists = false
			resp.Note = "exists but is a directory"
		}
	}

	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleFSDetect(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	brand := r.URL.Query().Get("brand")
	if brand == "" {
		http.Error(w, "brand query param required", http.StatusBadRequest)
		return
	}

	results := setup.DetectProviders()
	for _, d := range results {
		if d.Brand == brand {
			writeJSON(w, http.StatusOK, FSDetectResponse{
				Brand:  d.Brand,
				Found:  d.Found,
				Path:   d.Path,
				Source: d.Source,
			})
			return
		}
	}

	writeJSON(w, http.StatusOK, FSDetectResponse{
		Brand:  brand,
		Found:  false,
		Source: "unset",
	})
}

// expandPath expands a leading "~" to the user's home directory.
func expandPath(p string) string {
	if len(p) > 1 && p[0] == '~' && p[1] == '/' {
		home, err := os.UserHomeDir()
		if err == nil {
			return home + p[1:]
		}
	}
	return p
}
