// Package redact scrubs known secret values out of text before Tether
// persists it (CW-20260930-0009).
//
// It is the complete-string counterpart of go-mcp's supervise.Redact, which
// scrubs upstream stderr. supervise.Redact also blanks a secret's leading or
// trailing fragment at the edges of the text, because a stderr tail can be
// cut mid-value. Error text is whole, so this package replaces exact
// occurrences only and leaves an ordinary word that happens to start or end
// like a secret intact. Both use the same "[redacted]" marker.
package redact

import (
	"context"
	"sort"
	"strings"
	"sync"
)

// Marker replaces each redacted value, matching supervise.Redact.
const Marker = "[redacted]"

// minLen is the shortest value treated as a secret. Shorter values (an env
// flag such as "1" or "on") are never credentials, and replacing them would
// mangle every number or word that contains them.
const minLen = 4

// Text returns text with every occurrence of each secret replaced by Marker.
// Longer secrets are replaced first, so one secret that contains another is
// never left half-scrubbed.
func Text(text string, secrets []string) string {
	if text == "" || len(secrets) == 0 {
		return text
	}
	ordered := make([]string, 0, len(secrets))
	for _, s := range secrets {
		if len(s) >= minLen {
			ordered = append(ordered, s)
		}
	}
	sort.Slice(ordered, func(i, j int) bool { return len(ordered[i]) > len(ordered[j]) })
	for _, s := range ordered {
		text = strings.ReplaceAll(text, s, Marker)
	}
	return text
}

// Set is a concurrency-safe set of secret values. The zero value is ready
// to use; a nil *Set redacts nothing.
type Set struct {
	mu     sync.RWMutex
	values map[string]struct{}
}

// Add records values as secrets. Values shorter than minLen are ignored.
func (s *Set) Add(values ...string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, v := range values {
		if len(v) < minLen {
			continue
		}
		if s.values == nil {
			s.values = map[string]struct{}{}
		}
		s.values[v] = struct{}{}
	}
}

// Redact returns text with every value in the set replaced by Marker.
func (s *Set) Redact(text string) string {
	if s == nil || text == "" {
		return text
	}
	s.mu.RLock()
	secrets := make([]string, 0, len(s.values))
	for v := range s.values {
		secrets = append(secrets, v)
	}
	s.mu.RUnlock()
	return Text(text, secrets)
}

// Remember wraps a secret resolver so that every value it returns is added
// to the set before the caller can use it. A credential resolved for a call
// is then known by the time that call's error is recorded.
func (s *Set) Remember(resolve func(context.Context) (string, error)) func(context.Context) (string, error) {
	if resolve == nil {
		return nil
	}
	return func(ctx context.Context) (string, error) {
		v, err := resolve(ctx)
		if err == nil {
			s.Add(v)
		}
		return v, err
	}
}
