package store

import "path/filepath"

// RunSigningKeyPath identifies separate, checked private custody. The signing
// key must never enter the ordinary database or its WAL/SHM sidecars.
func (s *Store) RunSigningKeyPath() string {
	if s == nil || s.stateDBPath == "" || s.stateDBPath == ":memory:" {
		return ""
	}
	path, err := filepath.Abs(s.stateDBPath)
	if err != nil {
		return ""
	}
	return filepath.Join(path+".run-keys", "signing.key")
}
