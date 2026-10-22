package main

import (
	"aiwiki/internal/server"
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
)

// runMigrateFromSQLite imports a trusted-mode data directory into a
// personal-mode PostgreSQL database and bucket (docs/PERSONAL.md). Exit codes:
// 0 imported and verified (or dry run finished), 1 the verification found a
// mismatch, 2 refused or failed with nothing committed.
func runMigrateFromSQLite(args []string) int {
	fs := flag.NewFlagSet("migrate-from-sqlite", flag.ContinueOnError)
	from := fs.String("from", "", "trusted-mode data directory (contains wiki.db and files/)")
	dry := fs.Bool("dry-run", false, "do everything in a transaction that is rolled back; upload nothing")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintln(os.Stderr, "migrate-from-sqlite: unexpected argument", fs.Arg(0))
		return 2
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	rep, err := server.ImportSQLite(ctx, server.SQLiteImportConfig{
		From:                 *from,
		DryRun:               *dry,
		S3:                   s3ConfigFromEnv(),
		DatabaseURL:          os.Getenv("AIWIKI_DATABASE_URL"),
		MigrationDatabaseURL: os.Getenv("AIWIKI_MIGRATION_DATABASE_URL"),
		RuntimeRole:          os.Getenv("AIWIKI_DATABASE_ROLE"),
		ImportDatabaseURL:    os.Getenv("AIWIKI_IMPORT_DATABASE_URL"),
		OwnerName:            os.Getenv("AIWIKI_PERSONAL_NAME"),
		Out:                  os.Stdout,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "migrate-from-sqlite:", err)
		return 2
	}
	if rep.Failed() {
		return 1
	}
	return 0
}
