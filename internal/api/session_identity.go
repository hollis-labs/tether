package api

import "github.com/hollis-labs/tether/internal/store"

// SessionIdentityReader optionally enriches safe reads with recorded mappings.
// Implementations must not infer native identity from names or workspace paths.
type SessionIdentityReader interface {
	SessionProviderMappings(ids []string) (map[string][]store.SessionProviderMapping, error)
}

func (s *Server) populateSessionMappings(dtos []SessionDTO) error {
	reader, ok := s.Service.(SessionIdentityReader)
	if !ok {
		reader, ok = s.GroupStore.(SessionIdentityReader)
	}
	if !ok {
		return nil
	}
	ids := make([]string, len(dtos))
	for i := range dtos {
		ids[i] = dtos[i].ID
	}
	mappings, err := reader.SessionProviderMappings(ids)
	if err != nil {
		return err
	}
	for i := range dtos {
		session := &dtos[i]
		projected := make([]SessionProviderMappingDTO, 0, len(mappings[session.ID]))
		for _, m := range mappings[session.ID] {
			dto := SessionProviderMappingDTO{Owner: m.Owner, Provider: m.Provider, CreatedAt: m.CreatedAt, UpdatedAt: m.UpdatedAt}
			if m.NativeSessionID.Valid && m.NativeSessionID.String != "" {
				id := m.NativeSessionID.String
				dto.NativeSessionID = &id
			}
			projected = append(projected, dto)
		}
		session.ProviderMappings = projected
	}
	return nil
}
