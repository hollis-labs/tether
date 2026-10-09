package environment

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
)

func TestConcurrentInitializationAndRecovery(t *testing.T) {
	dir := t.TempDir()
	const initializers = 16
	ids := make(chan string, initializers)
	errs := make(chan error, initializers)
	var wg sync.WaitGroup
	for range initializers {
		wg.Add(1)
		go func() { defer wg.Done(); id, err := EnsureID(dir); ids <- id; errs <- err }()
	}
	wg.Wait()
	close(ids)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	winner := ""
	for id := range ids {
		if winner == "" {
			winner = id
		}
		if id != winner {
			t.Fatal("initializers disagree")
		}
	}
	for _, name := range []string{"environment-id", "environment-id.recovery"} {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatalf("private durable identity %s: %v", name, err)
		}
	}
	// Simulate the final-link publication gap. Recovery remains the winner.
	if err := os.Remove(filepath.Join(dir, "environment-id")); err != nil {
		t.Fatal(err)
	}
	got, err := EnsureID(dir)
	if err != nil || got != winner {
		t.Fatalf("recovery changed identity: %v", err)
	}
	if err := BindAuthority(dir, winner, "worker-1"); err != nil {
		t.Fatal(err)
	}
	if err := BindAuthority(dir, winner, "worker-1"); err != nil {
		t.Fatal(err)
	}
	if err := BindAuthority(dir, winner, "other-worker"); err == nil {
		t.Fatal("silently renamed persisted authority")
	}
}

func TestInvalidIdentityNeverRegenerates(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "environment-id")
	if err := os.WriteFile(path, []byte("broken identity"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := EnsureID(dir); err == nil {
		t.Fatal("silently replaced corrupt identity")
	}
	raw, err := os.ReadFile(path)
	if err != nil || string(raw) != "broken identity" {
		t.Fatal("corrupt state was overwritten")
	}
}

func TestDescriptorCachedMinimalAndForwardCompatible(t *testing.T) {
	id, err := EnsureID(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	groups := map[string]map[string]any{"sessions": {"version": 1}}
	h, err := NewDescriptor(Descriptor{EnvironmentID: id, Label: "worker", ServerVersion: "0.8.0", Capabilities: groups, UpdateCapability: "foreground"})
	if err != nil {
		t.Fatal(err)
	}
	groups["sessions"]["credential"] = "must not appear in frozen descriptor"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, DescriptorPath, nil))
	if w.Code != 200 || w.Header().Get("ETag") == "" {
		t.Fatal("descriptor not cacheable")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &fields); err != nil {
		t.Fatal(err)
	}
	// Required public shape; the explicit list is a wire contract, not a
	// mutable-file-content assertion. Additive fields are allowed below.
	for _, name := range []string{"environmentId", "label", "platform", "serverVersion", "protocol", "capabilities", "updateCapability"} {
		if _, ok := fields[name]; !ok {
			t.Fatalf("missing required public field %s", name)
		}
	}
	var got Descriptor
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Capabilities["sessions"]["credential"] != nil {
		t.Fatal("descriptor aliases mutable caller state")
	}
	fields["futureField"] = json.RawMessage(`{"future":true}`)
	fields["updateCapability"] = json.RawMessage(`"future-lifecycle"`)
	additive, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(additive, &got); err != nil || got.UpdateCapability != "future-lifecycle" {
		t.Fatal("unknown wire field/variant rejected")
	}
	r := httptest.NewRequest(http.MethodGet, DescriptorPath, nil)
	r.Header.Set("If-None-Match", w.Header().Get("ETag"))
	cached := httptest.NewRecorder()
	h.ServeHTTP(cached, r)
	if cached.Code != 304 || cached.Body.Len() != 0 {
		t.Fatal("conditional descriptor read failed")
	}
}

func TestProtocolGate(t *testing.T) {
	for _, tc := range []struct {
		name, path, header string
		require            bool
		want               int
	}{
		{"legacy", "/sessions", "", false, 204},
		{"required", "/environment/events", "", true, 409},
		{"match-header", "/environment/events", "1", true, 204},
		{"match-query", "/environment/snapshot?protocol=1", "", true, 204},
		{"conflict", "/sessions?protocol=2", "1", false, 409},
		{"wrong", "/sessions", "2", false, 409},
		{"duplicate", "/sessions?protocol=1&protocol=1", "", false, 409},
		{"health", "/health", "2", true, 204},
		{"descriptor", DescriptorPath, "2", true, 204},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, tc.path, nil)
			if tc.header != "" {
				r.Header.Set(ProtocolHeader, tc.header)
			}
			w := httptest.NewRecorder()
			ProtocolGate(tc.require, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) })).ServeHTTP(w, r)
			if w.Code != tc.want {
				t.Fatalf("status %d", w.Code)
			}
			if tc.want == 409 {
				var e struct {
					Error ProtocolError `json:"error"`
				}
				if json.Unmarshal(w.Body.Bytes(), &e) != nil || e.Error.Code != "protocol_mismatch" || e.Error.RequiredProtocol != Protocol || e.Error.UpdateHint == "" {
					t.Fatal("missing actionable typed mismatch")
				}
			}
		})
	}
}

// Protocol-1 structural conformance preserves types and JSON names of
// established fields. New fields/groups are allowed; breaking an established
// field requires a new protocol baseline and an explicit version bump.
func TestProtocol1WireConformance(t *testing.T) {
	if Protocol != 1 {
		t.Fatal("add the next version's conformance baseline")
	}
	for _, tc := range []struct {
		value  any
		fields map[string]reflect.Type
	}{
		{Descriptor{}, map[string]reflect.Type{"environmentId": reflect.TypeFor[string](), "label": reflect.TypeFor[string](), "platform": reflect.TypeFor[Platform](), "serverVersion": reflect.TypeFor[string](), "protocol": reflect.TypeFor[int](), "capabilities": reflect.TypeFor[map[string]map[string]any](), "updateCapability": reflect.TypeFor[string]()}},
		{Platform{}, map[string]reflect.Type{"os": reflect.TypeFor[string](), "arch": reflect.TypeFor[string]()}},
		{ProtocolError{}, map[string]reflect.Type{"code": reflect.TypeFor[string](), "message": reflect.TypeFor[string](), "required_protocol": reflect.TypeFor[int](), "update_hint": reflect.TypeFor[string]()}},
	} {
		typ := reflect.TypeOf(tc.value)
		for name, want := range tc.fields {
			found := false
			for i := 0; i < typ.NumField(); i++ {
				field := typ.Field(i)
				if field.Tag.Get("json") == name {
					found = true
					if field.Type != want {
						t.Fatalf("protocol1 %s type changed without bump", name)
					}
				}
			}
			if !found {
				t.Fatalf("protocol1 %s removed/renamed without bump", name)
			}
		}
	}
}
