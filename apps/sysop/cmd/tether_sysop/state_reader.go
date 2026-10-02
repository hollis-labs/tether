package main

import (
	"os"
	"sync"

	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/store"
)

type stateReader struct {
	mu   sync.Mutex
	db   *store.Store
	path string
	file os.FileInfo
}

// withStateReader leases the shared reader for one inbox refresh. Holding the
// lock through the read prevents a catalog change or DB replacement from
// closing the handle beneath another request. Failed opens are retried on the
// next refresh, so daemon startup does not poison the reader permanently.
func (s *appServer) withStateReader(read func(*store.Store) error) error {
	s.reader.mu.Lock()
	defer s.reader.mu.Unlock()
	cat, err := config.Load(s.catalogRoot)
	if err != nil {
		return err
	}
	path := config.Expand(cat.Global.Catalog.Defaults.StateDB)
	if path == "" {
		return errStateDBUnset
	}
	file, err := os.Stat(path)
	if err != nil {
		return err
	}
	if s.reader.db == nil || s.reader.path != path || !os.SameFile(s.reader.file, file) {
		if s.reader.db != nil {
			_ = s.reader.db.Close()
			s.reader.db = nil
		}
		db, err := store.OpenReadOnly(path)
		if err != nil {
			return err
		}
		s.reader.db, s.reader.path, s.reader.file = db, path, file
	}
	return read(s.reader.db)
}

func (s *appServer) closeStateReader() {
	s.reader.mu.Lock()
	defer s.reader.mu.Unlock()
	if s.reader.db != nil {
		_ = s.reader.db.Close()
		s.reader.db = nil
	}
}
