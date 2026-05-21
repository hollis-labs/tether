package registry_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/hollis-labs/tether/internal/registry"
)

// TestArrayPatch_UnmarshalJSON covers both wire shapes (shorthand + explicit)
// plus the leading-whitespace case the v060-01 PR review caught: a payload
// like `  ["a"]` (with leading spaces) was being mis-detected as the object
// form because the type checked data[0] without trimming.
func TestArrayPatch_UnmarshalJSON(t *testing.T) {
	tests := []struct {
		name     string
		in       string
		wantMode registry.ArrayMode
		wantVals []string
		wantErr  string
	}{
		{
			name:     "shorthand array",
			in:       `["a","b"]`,
			wantMode: registry.ArrayModeReplace,
			wantVals: []string{"a", "b"},
		},
		{
			name:     "shorthand array with leading whitespace",
			in:       "  \n\t[\"a\"]",
			wantMode: registry.ArrayModeReplace,
			wantVals: []string{"a"},
		},
		{
			name:     "shorthand empty array",
			in:       `[]`,
			wantMode: registry.ArrayModeReplace,
			wantVals: nil,
		},
		{
			name:     "explicit replace",
			in:       `{"mode":"replace","value":["x"]}`,
			wantMode: registry.ArrayModeReplace,
			wantVals: []string{"x"},
		},
		{
			name:     "explicit append",
			in:       `{"mode":"append","value":["x","y"]}`,
			wantMode: registry.ArrayModeAppend,
			wantVals: []string{"x", "y"},
		},
		{
			name:     "explicit remove",
			in:       `{"mode":"remove","value":["x"]}`,
			wantMode: registry.ArrayModeRemove,
			wantVals: []string{"x"},
		},
		{
			name:    "explicit with bad mode",
			in:      `{"mode":"upsert","value":["x"]}`,
			wantErr: "invalid array patch mode",
		},
		{
			name:    "empty payload",
			in:      ``,
			wantErr: "empty array patch",
		},
		{
			name:    "whitespace-only payload",
			in:      "   \n  ",
			wantErr: "empty array patch",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var p registry.ArrayPatch[string]
			err := p.UnmarshalJSON([]byte(tc.in))
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("UnmarshalJSON(%q): err = nil; want substring %q", tc.in, tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("UnmarshalJSON(%q): err = %v; want substring %q", tc.in, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("UnmarshalJSON(%q): %v", tc.in, err)
			}
			if p.Mode != tc.wantMode {
				t.Errorf("Mode = %q; want %q", p.Mode, tc.wantMode)
			}
			if !slicesEqual(p.Value, tc.wantVals) {
				t.Errorf("Value = %v; want %v", p.Value, tc.wantVals)
			}
		})
	}
}

// TestArrayPatch_NestedWhitespace covers the more realistic case the
// reviewer flagged: encoding/json passing a field value to UnmarshalJSON
// with surrounding whitespace when the parent struct is decoded.
func TestArrayPatch_NestedWhitespace(t *testing.T) {
	// Pretty-printed JSON: the inner array token is "  [\n  \"x\"\n]"
	// (with whitespace at both ends after the colon).
	parent := `{
  "caps":   [
    "x"
  ]
}`
	var got struct {
		Caps registry.ArrayPatch[string] `json:"caps"`
	}
	if err := json.Unmarshal([]byte(parent), &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got.Caps.Mode != registry.ArrayModeReplace {
		t.Errorf("Mode = %q; want %q", got.Caps.Mode, registry.ArrayModeReplace)
	}
	if len(got.Caps.Value) != 1 || got.Caps.Value[0] != "x" {
		t.Errorf("Value = %v; want [x]", got.Caps.Value)
	}
}

func slicesEqual[T comparable](a, b []T) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
