package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	tether "github.com/hollis-labs/substrate/mesh/tetherclient"
	directory "github.com/hollis-labs/tether/internal/environmentdirectory"
)

// EnvironmentDirectory is daemon-owned: no HTTP/MCP caller opens its DB directly.
type EnvironmentDirectory interface {
	List(context.Context) ([]directory.Record, error)
	Get(context.Context, string) (directory.Record, error)
	Register(context.Context, directory.Registration) (directory.Record, error)
	Rename(context.Context, string, string) (directory.Record, error)
	Retire(context.Context, string) (directory.Record, error)
}

func (s *Server) registerEnvironmentDirectoryRoutes(router *http.ServeMux) {
	if s.Directory == nil {
		return
	}
	router.HandleFunc("/environments", s.environmentDirectoryCollection)
	router.HandleFunc("/environments/", s.environmentDirectoryItem)
}

func directoryBody(w http.ResponseWriter, r *http.Request, out any) bool {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 256<<10))
	decoder.DisallowUnknownFields()
	if decoder.Decode(out) != nil {
		writeError(w, 400, CodeInvalidRequest, "invalid environment directory body")
		return false
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		writeError(w, 400, CodeInvalidRequest, "expected one JSON object")
		return false
	}
	return true
}

func directoryError(w http.ResponseWriter, err error) {
	var mismatch *tether.EnvironmentIdentityError
	var protocol *tether.ProtocolMismatchError
	var auth *tether.EnvironmentAuthenticationError
	switch {
	case errors.Is(err, directory.ErrInvalid):
		writeError(w, 400, CodeInvalidRequest, "invalid directory declaration")
	case errors.Is(err, directory.ErrNotFound):
		writeError(w, 404, CodeNotFound, "environment not found")
	case errors.Is(err, directory.ErrConflict), errors.Is(err, directory.ErrRetired), errors.As(err, &mismatch):
		writeError(w, 409, CodeConflict, "environment identity or binding conflict")
	case errors.As(err, &protocol):
		writeError(w, 409, CodeConflict, "environment protocol incompatible")
	case errors.As(err, &auth):
		writeError(w, 403, CodeForbidden, "environment credential refused")
	default:
		writeError(w, 503, CodeInternalError, "environment directory operation unavailable")
	}
}

func directoryOperator(w http.ResponseWriter, r *http.Request) bool {
	if _, ok := localOperator(r); !ok {
		writeError(w, 403, CodeForbidden, "directory changes require the local Unix socket and verified operator")
		return false
	}
	return true
}

func (s *Server) environmentDirectoryCollection(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		records, err := s.Directory.List(r.Context())
		if err != nil {
			directoryError(w, err)
			return
		}
		for i := range records {
			records[i] = records[i].Public()
		}
		writeJSON(w, 200, map[string]any{"environments": records})
	case http.MethodPost:
		if !directoryOperator(w, r) {
			return
		}
		var in directory.Registration
		if !directoryBody(w, r, &in) {
			return
		}
		record, err := s.Directory.Register(r.Context(), in)
		if err != nil {
			directoryError(w, err)
			return
		}
		writeJSON(w, 201, record.Public())
	default:
		writeError(w, 405, CodeMethodNotAllowed, "method not allowed")
	}
}

func (s *Server) environmentDirectoryItem(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/environments/")
	if id == "" || strings.Contains(id, "/") {
		writeError(w, 404, CodeNotFound, "environment not found")
		return
	}
	var record directory.Record
	var err error
	switch r.Method {
	case http.MethodGet:
		record, err = s.Directory.Get(r.Context(), id)
	case http.MethodPatch:
		if !directoryOperator(w, r) {
			return
		}
		var in struct {
			Label string `json:"label"`
		}
		if !directoryBody(w, r, &in) {
			return
		}
		record, err = s.Directory.Rename(r.Context(), id, in.Label)
	case http.MethodDelete:
		if !directoryOperator(w, r) {
			return
		}
		record, err = s.Directory.Retire(r.Context(), id)
	default:
		writeError(w, 405, CodeMethodNotAllowed, "method not allowed")
		return
	}
	if err != nil {
		directoryError(w, err)
		return
	}
	writeJSON(w, 200, record.Public())
}
