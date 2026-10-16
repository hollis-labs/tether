package app

import "errors"

var errEventBusUnavailable = errors.New("event bus is unavailable")

// requireEventBus prevents mutation paths from bypassing the shared event
// stages when a partial service is accidentally used for runtime operations.
func (s *Service) requireEventBus() error {
	if s.Bus == nil {
		return errEventBusUnavailable
	}
	return nil
}
