// Command tether-apikey-helper is a thin front for go-secrets, kept so existing
// helper:// and keychain:// workflows keep working. It owns no keychain or
// resolution logic of its own.
//
//	tether-apikey-helper resolve <ref>   print the value (helper protocol v0)
//	tether-apikey-helper set <ref>       store the value read from stdin
//	tether-apikey-helper delete <ref>    remove the entry
//
// The value is read from stdin only, on every platform. It is never an
// argument: argv is visible to every process of the user.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	secrets "github.com/hollis-labs/libs/security/secrets"
	"github.com/hollis-labs/tether/internal/secretsetup"
)

var (
	version   = "dev"
	commit    = "unknown"
	buildDate = "unknown"

	// store is the credential store. Tests replace it with an in-memory one.
	store secrets.Store = secrets.NewKeyringStore()
)

// Helper protocol v0 exit codes (see go-secrets README).
const (
	exitFailure  = 1
	exitNotFound = 2
	exitDenied   = 3
)

func main() {
	err := run(context.Background(), os.Args[1:], os.Stdin, os.Stdout)
	if err == nil {
		return
	}
	fmt.Fprintf(os.Stderr, "tether-apikey-helper: %s\n", describe(err))
	os.Exit(exitCode(err))
}

func resolver() *secrets.Resolver { return secretsetup.New(secrets.WithStore(store)) }

func run(ctx context.Context, args []string, stdin io.Reader, stdout io.Writer) error {
	if len(args) == 0 {
		return usageError()
	}
	switch args[0] {
	case "--version", "version":
		_, err := fmt.Fprintf(stdout, "tether-apikey-helper %s (commit %s, built %s)\n", version, commit, buildDate)
		return err
	case "resolve":
		if len(args) != 2 {
			return errors.New("resolve expects exactly 1 argument")
		}
		if err := managedRef(args[1]); err != nil {
			return err
		}
		res, err := resolver().Resolve(ctx, args[1])
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(stdout, res.Secret.Reveal())
		return err
	case "set":
		if len(args) != 2 {
			return errors.New("set expects exactly 1 argument, <ref>; the secret is read from stdin and is never accepted as an argument")
		}
		if err := managedRef(args[1]); err != nil {
			return err
		}
		if isTerminal(stdin) {
			return errors.New("set: stdin is a terminal; pipe or redirect the value (it is never taken from arguments)")
		}
		return resolver().Set(ctx, args[1], stdin)
	case "delete":
		if len(args) != 2 {
			return errors.New("delete expects exactly 1 argument")
		}
		if err := managedRef(args[1]); err != nil {
			return err
		}
		return deleteRef(ctx, args[1])
	default:
		return usageError()
	}
}

func usageError() error {
	return errors.New("usage: tether-apikey-helper <resolve|set|delete> <ref>   (set reads the secret from stdin)")
}

// managedRef accepts only keychain:// references: this binary manages the
// local keychain, it does not forward to other helpers or backends.
func managedRef(raw string) error {
	ref, err := secrets.ParseRef(raw)
	if err != nil {
		return err
	}
	switch ref.Scheme {
	case "keychain", "keyring":
		return nil
	}
	return fmt.Errorf("unsupported secret scheme %q: only keychain:// refs can be managed locally", ref.Scheme)
}

// deleteRef removes the entry from the shared service and from the legacy
// "tether" service, so a deleted reference does not keep resolving through the
// legacy read-through.
func deleteRef(ctx context.Context, raw string) error {
	if err := resolver().Delete(ctx, raw); err != nil {
		return err
	}
	ref, err := secrets.ParseRef(raw)
	if err != nil {
		return err
	}
	if account := secretsetup.LegacyAccount(ref); account != "" {
		if err := store.Delete(ctx, secretsetup.LegacyService, account); err != nil && !errors.Is(err, secrets.ErrNotFound) {
			return err
		}
	}
	return nil
}

func describe(err error) string {
	var se *secrets.Error
	if errors.As(err, &se) {
		if g := se.Guidance(); g != "" {
			return se.Error() + " (" + g + ")"
		}
	}
	return err.Error()
}

func exitCode(err error) int {
	switch {
	case errors.Is(err, secrets.ErrNotFound):
		return exitNotFound
	case errors.Is(err, secrets.ErrDenied):
		return exitDenied
	}
	return exitFailure
}

func isTerminal(r io.Reader) bool {
	f, ok := r.(*os.File)
	if !ok {
		return false
	}
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}
