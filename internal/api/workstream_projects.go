package api

import (
	"bytes"
	"encoding/json"
	"errors"
)

// Project lists distinguish omitted from [] on PATCH. Explicit null is invalid
// on every write, rather than silently clearing or ignoring an association.
func decodeWorkstreamProjects(b []byte, out any) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(b, &fields); err != nil {
		return err
	}
	if raw, ok := fields["project_urns"]; ok && bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return errors.New("project_urns must be an array, not null")
	}
	return json.Unmarshal(b, out)
}
func (r *WorkstreamCreateRequest) UnmarshalJSON(b []byte) error {
	type plain WorkstreamCreateRequest
	return decodeWorkstreamProjects(b, (*plain)(r))
}
func (r *WorkstreamUpdateRequest) UnmarshalJSON(b []byte) error {
	type plain WorkstreamUpdateRequest
	return decodeWorkstreamProjects(b, (*plain)(r))
}
func (r *SessionWorkstreamRequest) UnmarshalJSON(b []byte) error {
	type plain SessionWorkstreamRequest
	return decodeWorkstreamProjects(b, (*plain)(r))
}
