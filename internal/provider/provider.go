package provider

import (
	"os/exec"

	"github.com/chrispian/agent-mux/internal/launch"
)

// Adapter converts a launch plan into an os/exec.Cmd ready for PTY start.
type Adapter interface {
	ID() string
	Build(plan *launch.Plan, workdir string) (*exec.Cmd, error)
}

// Registry holds adapters by provider ID.
type Registry struct {
	byID map[string]Adapter
}

func NewRegistry() *Registry {
	return &Registry{byID: map[string]Adapter{}}
}

func (r *Registry) Register(a Adapter) { r.byID[a.ID()] = a }

func (r *Registry) Get(id string) (Adapter, bool) {
	a, ok := r.byID[id]
	return a, ok
}
