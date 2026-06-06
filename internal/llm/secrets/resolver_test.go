package secrets

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestParseRef(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		raw     string
		want    Ref
		wantErr error
	}{
		{
			name: "keychain",
			raw:  "keychain://openai/personal",
			want: Ref{
				Raw:       "keychain://openai/personal",
				Scheme:    "keychain",
				Authority: "openai",
				Path:      []string{"personal"},
			},
		},
		{
			name: "helper",
			raw:  "helper://mux-apikey-helper/openai/default",
			want: Ref{
				Raw:       "helper://mux-apikey-helper/openai/default",
				Scheme:    "helper",
				Authority: "mux-apikey-helper",
				Path:      []string{"openai", "default"},
			},
		},
		{
			name:    "empty",
			raw:     "  ",
			wantErr: ErrEmptyRef,
		},
		{
			name:    "unsupported scheme",
			raw:     "env://OPENAI_API_KEY/default",
			wantErr: ErrUnsupportedScheme,
		},
		{
			name:    "missing path",
			raw:     "keychain://openai",
			wantErr: errAny,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := ParseRef(tc.raw)
			if tc.wantErr != nil {
				if err == nil {
					t.Fatalf("ParseRef(%q) error = nil, want non-nil", tc.raw)
				}
				if errors.Is(tc.wantErr, errAny) {
					return
				}
				if errors.Is(tc.wantErr, ErrUnsupportedScheme) || errors.Is(tc.wantErr, ErrEmptyRef) {
					if !errors.Is(err, tc.wantErr) {
						t.Fatalf("ParseRef(%q) error = %v, want errors.Is(..., %v)", tc.raw, err, tc.wantErr)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseRef(%q) error = %v", tc.raw, err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("ParseRef(%q) = %+v, want %+v", tc.raw, got, tc.want)
			}
		})
	}
}

func TestResolverResolveKeychainRef(t *testing.T) {
	helperPath, argsPath := writeHelperScript(t, `#!/bin/sh
printf '%s\n' "$@" > "$ARGS_PATH"
printf 'sk-test-123\n'
`)
	t.Setenv("ARGS_PATH", argsPath)

	r := NewResolver(WithDefaultHelperPath(helperPath))
	secret, err := r.Resolve(context.Background(), "keychain://openai/personal")
	if err != nil {
		t.Fatalf("Resolve returned err: %v", err)
	}
	if secret != "sk-test-123" {
		t.Fatalf("secret = %q, want sk-test-123", secret)
	}

	gotArgs, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatalf("ReadFile(args): %v", err)
	}
	if string(gotArgs) != "resolve\nkeychain://openai/personal\n" {
		t.Fatalf("helper args = %q", string(gotArgs))
	}
}

func TestResolverResolveNamedHelperRef(t *testing.T) {
	helperPath, argsPath := writeHelperScript(t, `#!/bin/sh
printf '%s\n' "$@" > "$ARGS_PATH"
printf 'sk-helper-456'
`)
	t.Setenv("ARGS_PATH", argsPath)

	r := NewResolver(
		WithNamedHelperResolver(func(name string) string {
			if name == "custom-helper" {
				return helperPath
			}
			return ""
		}),
	)
	secret, err := r.Resolve(context.Background(), "helper://custom-helper/openai/default")
	if err != nil {
		t.Fatalf("Resolve returned err: %v", err)
	}
	if secret != "sk-helper-456" {
		t.Fatalf("secret = %q, want sk-helper-456", secret)
	}
}

func TestResolverHelperNotFound(t *testing.T) {
	t.Parallel()

	r := NewResolver(
		WithDefaultHelperPath(""),
		WithNamedHelperResolver(func(string) string { return "" }),
	)

	_, err := r.Resolve(context.Background(), "keychain://anthropic/work")
	if !errors.Is(err, ErrHelperNotFound) {
		t.Fatalf("Resolve error = %v, want ErrHelperNotFound", err)
	}
}

func TestResolverPropagatesHelperFailure(t *testing.T) {
	helperPath, _ := writeHelperScript(t, `#!/bin/sh
echo 'lookup failed' >&2
exit 17
`)
	r := NewResolver(WithDefaultHelperPath(helperPath))

	_, err := r.Resolve(context.Background(), "keychain://openai/personal")
	if err == nil {
		t.Fatal("Resolve error = nil, want non-nil")
	}
	if !strings.Contains(err.Error(), "lookup failed") {
		t.Fatalf("Resolve error = %v, want helper stderr", err)
	}
}

func TestResolverRejectsEmptySecret(t *testing.T) {
	helperPath, _ := writeHelperScript(t, `#!/bin/sh
printf '\n'
`)
	r := NewResolver(WithDefaultHelperPath(helperPath))

	_, err := r.Resolve(context.Background(), "keychain://openai/personal")
	if !errors.Is(err, ErrEmptySecret) {
		t.Fatalf("Resolve error = %v, want ErrEmptySecret", err)
	}
}

func writeHelperScript(t *testing.T, body string) (string, string) {
	t.Helper()

	dir := t.TempDir()
	path := filepath.Join(dir, "helper.sh")
	argsPath := filepath.Join(dir, "args.txt")
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatalf("WriteFile(helper): %v", err)
	}
	return path, argsPath
}

var errAny = errors.New("any error")
