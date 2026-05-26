package main

import (
	"errors"
	"fmt"
	"os"
)

var (
	version   = "dev"
	commit    = "unknown"
	buildDate = "unknown"
)

// exitCoder is satisfied by errors that want to override the default
// "any error → exit 1" mapping. The registry subcommand uses this to
// classify ErrNotFound (1), validation (2), daemon-unreachable (3),
// and unexpected errors (4). Anything that doesn't implement the
// interface keeps the legacy behavior.
type exitCoder interface {
	error
	ExitCode() int
}

func main() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		var ec exitCoder
		if errors.As(err, &ec) {
			os.Exit(ec.ExitCode())
		}
		os.Exit(1)
	}
}
