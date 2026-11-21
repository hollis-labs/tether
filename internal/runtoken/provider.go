package runtoken

import (
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/hollis-labs/libs/security/secrets/tokenfile"
)

const FileEnv = "TETHER_RUN_TOKEN_FILE"
const EnvFallback = "TETHER_RUN_TOKEN_ALLOW_ENV"

// ProviderEnvironment strips raw proof values from the provider environment.
// Launchers call it before sandbox construction; shims call it after restoring
// the sealed handoff to validate the file binding. Keep release until exit.
func ProviderEnvironment(env []string) ([]string, func(), error) {
	out := make([]string, 0, len(env)+1)
	token, file, fallback := "", "", false
	for _, entry := range env {
		k, v, _ := strings.Cut(entry, "=")
		switch k {
		case TokenEnv:
			if token != "" && token != v {
				return nil, nil, ErrHandoff
			}
			token = v
		case FileEnv:
			if file != "" {
				return nil, nil, ErrHandoff
			}
			file = v
		case EnvFallback:
			fallback = v == "1"
			out = append(out, entry)
		default:
			out = append(out, entry)
		}
	}
	if token == "" {
		if file != "" {
			if _, err := ReadProviderToken(file, "", false); err != nil {
				return nil, nil, ErrHandoff
			}
			out = append(out, FileEnv+"="+file)
		}
		return out, managedFileRelease(file), nil
	}
	if file != "" {
		existing, err := ReadProviderToken(file, "", false)
		if err != nil || existing != token {
			return nil, nil, ErrHandoff
		}
		return append(out, FileEnv+"="+file), managedFileRelease(file), nil
	}
	if fallback {
		return append(out, TokenEnv+"="+token), func() {}, nil
	}
	if len(token) > MaxSize {
		return nil, nil, ErrHandoff
	}
	dir, err := os.MkdirTemp("", "tether-run-token-")
	if err != nil {
		return nil, nil, ErrHandoff
	}
	path := filepath.Join(dir, "run.token")
	release := managedFileRelease(path)
	if err := tokenfile.Write(path, token); err != nil {
		release()
		return nil, nil, ErrHandoff
	}
	return append(out, FileEnv+"="+path), release, nil
}

// ReadEnvironment prefers an explicitly supplied file and fails closed if that
// file is unreadable. Raw environment credentials require an explicit opt-in.
func ReadEnvironment() (string, error) {
	return ReadProviderToken(os.Getenv(FileEnv), os.Getenv(TokenEnv), os.Getenv(EnvFallback) == "1")
}
func ReadProviderToken(path, raw string, allowEnvironment bool) (string, error) {
	if path != "" {
		token, err := tokenfile.Read(path)
		if err != nil || token == "" || len(token) > MaxSize {
			return "", errors.New("run token file unavailable")
		}
		return token, nil
	}
	if raw != "" {
		if !allowEnvironment || len(raw) > MaxSize {
			return "", errors.New("run token environment fallback disabled")
		}
		return raw, nil
	}
	return "", nil
}

// The shim owns the generated file through controller/outpost restarts. Only
// the launcher naming convention is eligible for cleanup; no recursive delete.
func managedFileRelease(path string) func() {
	dir := filepath.Dir(path)
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || filepath.Base(path) != "run.token" || !strings.HasPrefix(filepath.Base(dir), "tether-run-token-") {
		return func() {}
	}
	return func() { _ = os.Remove(path); _ = os.Remove(dir) }
}
