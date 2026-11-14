package registry

import (
	"context"
	"regexp"
	"strings"
)

// HandleSubstrate records a seat's @handle as an external id (CW-20261114-0013):
// typed messages address seats as @<handle>, and the registry keeps a handle
// unique among agents (AttachExternalID refuses a second owner).
const HandleSubstrate = "handle"

var handleNameRE = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)

// BackfillHandles gives every agent with a tether catalog id and no handle the
// handle <catalog id>, or overrides[<catalog id>] when the catalog sets
// handle:, when that is a valid handle and not taken. It never replaces a
// handle an agent already has. It returns how many it attached.
func BackfillHandles(ctx context.Context, svc *Service, overrides map[string]string) (int, error) {
	rows, err := svc.Search(ctx, KindAgent, Filter{Status: StatusAny})
	if err != nil {
		return 0, err
	}
	attached := 0
	for _, row := range rows {
		if row.MergedInto != "" {
			continue
		}
		if _, ok := row.ExternalIDFor(HandleSubstrate); ok {
			continue
		}
		ext, ok := row.ExternalIDFor("tether")
		if !ok {
			continue
		}
		name := strings.ToLower(ext.ExternalID)
		if h := overrides[ext.ExternalID]; h != "" {
			name = h
		}
		if !handleNameRE.MatchString(name) {
			continue
		}
		if err := svc.AttachExternalID(ctx, row.URN, HandleSubstrate, name); err != nil {
			// Taken by another agent: leave this one without a handle rather
			// than fail the whole pass.
			continue
		}
		attached++
	}
	return attached, nil
}

// BackfillHandles is the method form for the API seam.
func (s *Service) BackfillHandles(ctx context.Context, overrides map[string]string) (int, error) {
	return BackfillHandles(ctx, s, overrides)
}
