package runtoken

import (
	"errors"
	"github.com/hollis-labs/libs/harness/core/sandbox"
	"path/filepath"
	"slices"
)

// ReadablePolicy adds only this execution's credential directory, preserving
// deny precedence. It never grants access over an explicit denied path.
func ReadablePolicy(p *sandbox.ResolvedAccessPolicy, path string) (*sandbox.ResolvedAccessPolicy, error) {
	if p == nil {
		return nil, nil
	}
	if p.AccessFor(path) == sandbox.AccessDenied {
		return nil, errors.New("run token file conflicts with sandbox deny rules")
	}
	out := *p
	out.Runtime = append(slices.Clone(p.Runtime), sandbox.ResolvedPath{Kind: sandbox.AccessRuntimeRead, Path: filepath.Dir(path), Source: "launcher run proof"})
	return &out, nil
}
