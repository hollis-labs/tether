// Command tether is the Tether hub on Substrate (CW-20261112-0101). The shell (config, logging,
// health, the HTTP host, the database and the CLI skeleton) comes from the manifest; this file, its
// boot hooks and internal/ are the app. At slice S0 the hub serves only /healthz; each later slice
// adds its domain on top (docs/architecture.md).
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/hollis-labs/substrate/boot"
	"github.com/hollis-labs/substrate/modules/cli"
	"github.com/hollis-labs/substrate/modules/database"

	_ "github.com/hollis-labs/tether/next/internal/sqlitefk" // foreign keys off, as the old store ran
	"github.com/hollis-labs/tether/next/migrations"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Getenv, os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "tether:", err)
		stop()
		os.Exit(1)
	}
}

// run boots the modules, then executes the command line. args are the arguments after the program
// name; tests pass them directly.
func run(ctx context.Context, getenv func(string) string, args []string) error {
	fsys, root := migrations.FS()
	database.UseMigrationFS(fsys, root)

	started, err := boot.StartWith(ctx, substrateApp, substrateModules, substrateConfigJSON, bootOptions(getenv))
	if err != nil {
		return err
	}
	defer func() { _ = started.Stop(context.Background()) }()

	t := &tether{app: started.App}
	rootCmd := cli.Root(started.App)
	rootCmd.SetArgs(args)
	rootCmd.AddCommand(t.serveCmd(ctx))
	return rootCmd.ExecuteContext(ctx)
}
