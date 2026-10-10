// Package environment owns machine-local identity and the additive remote
// protocol contract. It does not enroll agents or move session authority.
package environment

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/google/uuid"
)

var authorityName = regexp.MustCompile(`^[a-z0-9-]{1,32}$`)

func ValidateAuthority(name string) error {
	if name != "" && !authorityName.MatchString(name) {
		return errors.New("environment authority must contain 1–32 lowercase letters, digits or hyphens")
	}
	return nil
}

// EnsureID selects one UUIDv4 for the selected state directory. Both links
// survive initialization: recovery is NOT a disposable temporary file. A
// delayed initializer or a crash between links still observes the same winner.
func EnsureID(stateDir string) (string, error) {
	raw, err := firstWriter(stateDir, "environment-id", func() ([]byte, error) {
		id, err := uuid.NewRandom()
		if err != nil {
			return nil, err
		}
		return []byte(id.String() + "\n"), nil
	})
	if err != nil {
		return "", err
	}
	id := strings.TrimSpace(string(raw))
	parsed, err := uuid.Parse(id)
	if err != nil || parsed.Version() != 4 || parsed.Variant() != uuid.RFC4122 || parsed.String() != id {
		return "", errors.New("invalid persisted environment UUIDv4; restore state identity rather than generating a replacement")
	}
	return id, nil
}

// BindAuthority pins the configured name to this UUID. Directory collision
// checks and enrollment belong to the hub directory, not this local binding.
func BindAuthority(stateDir, id, name string) error {
	if err := ValidateAuthority(name); err != nil {
		return err
	}
	if name == "" {
		return nil
	}
	want := struct {
		ID        string `json:"environmentId"`
		Authority string `json:"authority"`
	}{id, name}
	raw, err := firstWriter(stateDir, "environment-authority", func() ([]byte, error) { return json.Marshal(want) })
	if err != nil {
		return err
	}
	var got struct {
		ID        string `json:"environmentId"`
		Authority string `json:"authority"`
	}
	if json.Unmarshal(raw, &got) != nil || got.ID != id || got.Authority != name {
		return errors.New("configured environment authority differs from its persisted UUID binding; explicit rename/enrollment is required")
	}
	return nil
}

func firstWriter(dir, name string, generate func() ([]byte, error)) ([]byte, error) {
	if dir == "" {
		return nil, errors.New("environment state directory is required")
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, fmt.Errorf("create environment state directory: %w", err)
	}
	path, recovery := filepath.Join(dir, name), filepath.Join(dir, name+".recovery")
	if raw, err := readIdentityFile(path); err == nil {
		if err := os.Link(path, recovery); err != nil && !errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("retain environment recovery: %w", err)
		}
		other, err := readIdentityFile(recovery)
		if err != nil || !bytes.Equal(raw, other) {
			return nil, errors.New("environment identity and recovery disagree; restore the original state identity")
		}
		return raw, syncDirectory(dir)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if _, err := readIdentityFile(recovery); errors.Is(err, os.ErrNotExist) {
		raw, err := generate()
		if err != nil {
			return nil, err
		}
		candidate, err := os.CreateTemp(dir, "."+name+"-candidate-")
		if err != nil {
			return nil, err
		}
		candidatePath := candidate.Name()
		// Only our unlinked candidate is disposable; the durable recovery link
		// remains even if the final link fails or another initializer wins.
		defer func() { _ = os.Remove(candidatePath) }()
		if _, err = candidate.Write(raw); err == nil {
			err = candidate.Sync()
		}
		err = errors.Join(err, candidate.Close())
		if err != nil {
			return nil, err
		}
		if err = os.Link(candidatePath, recovery); err != nil && !errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("publish environment recovery: %w", err)
		}
	} else if err != nil {
		return nil, err
	}
	if err := syncDirectory(dir); err != nil {
		return nil, err
	}
	if err := os.Link(recovery, path); err != nil && !errors.Is(err, os.ErrExist) {
		return nil, fmt.Errorf("publish environment identity: %w", err)
	}
	if err := syncDirectory(dir); err != nil {
		return nil, err
	}
	raw, err := readIdentityFile(path)
	if err != nil {
		return nil, err
	}
	other, err := readIdentityFile(recovery)
	if err != nil || !bytes.Equal(raw, other) {
		return nil, errors.New("environment identity and recovery disagree")
	}
	return raw, nil
}

func readIdentityFile(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("environment identity must be a private regular file")
	}
	// #nosec G304 -- path is a fixed identity filename under the selected state directory.
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	opened, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !os.SameFile(info, opened) || !opened.Mode().IsRegular() || opened.Mode().Perm()&0077 != 0 {
		return nil, errors.New("environment identity changed while opening")
	}
	raw, err := io.ReadAll(io.LimitReader(f, 513))
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 || len(raw) > 512 {
		return nil, errors.New("invalid environment identity file size")
	}
	return raw, nil
}

func syncDirectory(dir string) error {
	// #nosec G304 -- fsync the selected state directory used for atomic identity publication.
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	return f.Sync()
}
