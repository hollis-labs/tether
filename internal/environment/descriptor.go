package environment

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"runtime"
)

const DescriptorPath = "/.well-known/tether/environment"

// Descriptor decodes additively: unknown JSON fields, capability groups and
// string variants are preserved/ignored by ordinary encoding/json decoding.
// Missing capability groups mean unsupported, never a guessed default.
type Descriptor struct {
	EnvironmentID    string                    `json:"environmentId"`
	Label            string                    `json:"label"`
	Platform         Platform                  `json:"platform"`
	ServerVersion    string                    `json:"serverVersion"`
	Protocol         int                       `json:"protocol"`
	Capabilities     map[string]map[string]any `json:"capabilities"`
	UpdateCapability string                    `json:"updateCapability"`
}

type Platform struct {
	OS   string `json:"os"`
	Arch string `json:"arch"`
}

// DescriptorHandler freezes the public descriptor at daemon composition;
// requests never read catalog files, provider credentials or local paths.
type DescriptorHandler struct {
	descriptor Descriptor
	body       []byte
	etag       string
}

func NewDescriptor(d Descriptor) (*DescriptorHandler, error) {
	if d.EnvironmentID == "" {
		return nil, fmt.Errorf("environment ID is required")
	}
	if d.Label == "" {
		host, err := os.Hostname()
		if err != nil {
			return nil, fmt.Errorf("environment label: %w", err)
		}
		d.Label = host
	}
	d.Platform = Platform{OS: runtime.GOOS, Arch: runtime.GOARCH}
	d.Protocol = Protocol
	if d.ServerVersion == "" {
		d.ServerVersion = "dev"
	}
	if d.Capabilities == nil {
		d.Capabilities = map[string]map[string]any{}
	}
	if d.UpdateCapability == "" {
		d.UpdateCapability = "foreground"
	}
	switch d.UpdateCapability {
	case "service", "foreground", "none":
	default:
		return nil, fmt.Errorf("invalid update capability")
	}
	body, err := json.Marshal(d)
	if err != nil {
		return nil, err
	}
	// Detach nested capability maps from the caller; serving is immutable.
	var frozen Descriptor
	if err := json.Unmarshal(body, &frozen); err != nil {
		return nil, err
	}
	return &DescriptorHandler{descriptor: frozen, body: body, etag: fmt.Sprintf(`"%x"`, sha256.Sum256(body))}, nil
}

func (h *DescriptorHandler) Identity() (id, version string, protocol int) {
	return h.descriptor.EnvironmentID, h.descriptor.ServerVersion, h.descriptor.Protocol
}

func (h *DescriptorHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "public, max-age=60")
	w.Header().Set("ETag", h.etag)
	if r.Header.Get("If-None-Match") == h.etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(h.body)
}
