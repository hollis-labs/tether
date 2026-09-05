package main

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"

	"github.com/hollis-labs/tether/internal/llm/secrets"
	"github.com/zalando/go-keyring"
)

const serviceName = "tether"

var (
	version      = "dev"
	commit       = "unknown"
	buildDate    = "unknown"
	readSecret   = defaultReadSecret
	writeSecret  = defaultWriteSecret
	deleteSecret = defaultDeleteSecret
)

func main() {
	if err := run(os.Args[1:], os.Stdin, os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "mux-apikey-helper: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string, stdin io.Reader, stdout io.Writer) error {
	if len(args) == 0 {
		return usageError()
	}
	switch args[0] {
	case "--version", "version":
		_, err := fmt.Fprintf(stdout, "mux-apikey-helper %s (commit %s, built %s)\n", version, commit, buildDate)
		return err
	case "resolve":
		if len(args) != 2 {
			return fmt.Errorf("resolve expects exactly 1 argument")
		}
		secret, err := resolveRef(args[1])
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(stdout, secret)
		return err
	case "set":
		if len(args) < 2 || len(args) > 3 {
			return fmt.Errorf("set expects <ref> [secret]")
		}
		secret := ""
		if len(args) == 3 {
			secret = args[2]
		} else {
			raw, err := io.ReadAll(stdin)
			if err != nil {
				return fmt.Errorf("read secret from stdin: %w", err)
			}
			secret = strings.TrimSpace(string(raw))
		}
		return storeRef(args[1], secret)
	case "delete":
		if len(args) != 2 {
			return fmt.Errorf("delete expects exactly 1 argument")
		}
		return deleteRef(args[1])
	default:
		return usageError()
	}
}

func usageError() error {
	return errors.New("usage: mux-apikey-helper <resolve|set|delete> <ref> [secret]")
}

func resolveRef(raw string) (string, error) {
	ref, err := parseKeychainRef(raw)
	if err != nil {
		return "", err
	}
	for _, key := range envKeysForRef(ref) {
		if value := strings.TrimSpace(os.Getenv(key)); value != "" {
			return value, nil
		}
	}
	secret, err := readSecret(accountName(ref))
	if err != nil {
		return "", fmt.Errorf("resolve %s from keychain service %q: %w", ref.Raw, serviceName, err)
	}
	secret = strings.TrimSpace(secret)
	if secret == "" {
		return "", fmt.Errorf("resolve %s from keychain service %q: empty secret", ref.Raw, serviceName)
	}
	return secret, nil
}

func storeRef(raw, secret string) error {
	ref, err := parseKeychainRef(raw)
	if err != nil {
		return err
	}
	secret = strings.TrimSpace(secret)
	if secret == "" {
		return fmt.Errorf("secret is empty")
	}
	if err := writeSecret(accountName(ref), secret); err != nil {
		return fmt.Errorf("store %s in keychain service %q: %w", ref.Raw, serviceName, err)
	}
	return nil
}

func deleteRef(raw string) error {
	ref, err := parseKeychainRef(raw)
	if err != nil {
		return err
	}
	if err := deleteSecret(accountName(ref)); err != nil && !errors.Is(err, keyring.ErrNotFound) {
		return fmt.Errorf("delete %s from keychain service %q: %w", ref.Raw, serviceName, err)
	}
	return nil
}

func parseKeychainRef(raw string) (secrets.Ref, error) {
	ref, err := secrets.ParseRef(raw)
	if err != nil {
		return secrets.Ref{}, err
	}
	if ref.Scheme != "keychain" {
		return secrets.Ref{}, fmt.Errorf("unsupported secret scheme %q: only keychain:// refs can be managed locally", ref.Scheme)
	}
	return ref, nil
}

func accountName(ref secrets.Ref) string {
	return "provider-api-key:" + ref.Authority + "/" + strings.Join(ref.Path, "/")
}

func envKeysForRef(ref secrets.Ref) []string {
	parts := append([]string{ref.Authority}, ref.Path...)
	keys := []string{"MUX_APIKEY_" + normalizeEnvSuffix(strings.Join(parts, "_"))}
	switch ref.Authority {
	case "openai":
		keys = append(keys, "OPENAI_API_KEY")
	case "anthropic":
		keys = append(keys, "ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN")
	}
	return keys
}

func normalizeEnvSuffix(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteRune(r - ('a' - 'A'))
		case r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

func defaultReadSecret(account string) (string, error) {
	if runtime.GOOS == "darwin" {
		return readSecretDarwin(account)
	}
	return keyring.Get(serviceName, account)
}

func defaultWriteSecret(account, secret string) error {
	if runtime.GOOS == "darwin" {
		return writeSecretDarwin(account, secret)
	}
	return keyring.Set(serviceName, account, secret)
}

func defaultDeleteSecret(account string) error {
	if runtime.GOOS == "darwin" {
		return deleteSecretDarwin(account)
	}
	return keyring.Delete(serviceName, account)
}

func readSecretDarwin(account string) (string, error) {
	cmd := exec.Command("security", "find-generic-password", "-s", serviceName, "-a", account, "-w") //nolint:gosec // fixed binary + helper-owned args
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		text := strings.TrimSpace(stderr.String())
		if strings.Contains(text, "could not be found in the keychain") {
			return "", keyring.ErrNotFound
		}
		if text != "" {
			return "", fmt.Errorf("%w (stderr: %s)", err, text)
		}
		return "", err
	}
	return decodeKeyringValue(strings.TrimSpace(stdout.String()))
}

// keyringBase64Prefix is the marker zalando/go-keyring writes ahead of a
// base64-encoded secret on macOS (keyring_darwin.go, base64EncodingPrefix).
const keyringBase64Prefix = "go-keyring-base64:"

// decodeKeyringValue reverses go-keyring's macOS storage encoding.
//
// This helper reads the keychain through the `security` CLI rather than
// go-keyring, and the two halves of the portfolio do not agree: Cerberus and
// Nanite write entries with go-keyring, which stores
// "go-keyring-base64:<base64>", while go-keyring's own reader decodes that
// prefix transparently. A raw `security -w` read does not — it returns the
// marker string verbatim, which is the length and shape of a real credential
// and fails only later, at the API call, as a 401.
//
// Implementing the same read contract here makes the two interoperate in both
// directions: go-keyring already reads plain values written by `security`, and
// after this `security`-based reads understand values written by go-keyring.
//
// A value carrying the marker but not decoding as base64 is corrupt, and is
// reported rather than passed through as if it were a secret.
func decodeKeyringValue(value string) (string, error) {
	if !strings.HasPrefix(value, keyringBase64Prefix) {
		return value, nil
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(value, keyringBase64Prefix))
	if err != nil {
		return "", fmt.Errorf("decode go-keyring-encoded secret: %w", err)
	}
	return string(decoded), nil
}

func writeSecretDarwin(account, secret string) error {
	cmd := exec.Command("security", "add-generic-password", "-U", "-s", serviceName, "-a", account, "-w", secret) //nolint:gosec // fixed binary + helper-owned args
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		text := strings.TrimSpace(stderr.String())
		if text != "" {
			return fmt.Errorf("%w (stderr: %s)", err, text)
		}
		return err
	}
	return nil
}

func deleteSecretDarwin(account string) error {
	cmd := exec.Command("security", "delete-generic-password", "-s", serviceName, "-a", account) //nolint:gosec // fixed binary + helper-owned args
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		text := strings.TrimSpace(stderr.String())
		if strings.Contains(text, "could not be found in the keychain") {
			return keyring.ErrNotFound
		}
		if text != "" {
			return fmt.Errorf("%w (stderr: %s)", err, text)
		}
		return err
	}
	return nil
}
