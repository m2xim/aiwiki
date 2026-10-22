package main

import (
	"aiwiki/internal/server"
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
)

// runBackupObjects implements "aiwiki backup-objects" and "aiwiki restore-objects"
// (restore=true); it returns the exit code: 0 ok, 1 problems found, 2 could not run.
func runBackupObjects(name string, restore bool, args []string) int {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	dir := fs.String("dir", "", "directory that holds the object files")
	verify := fs.Bool("verify-hashes", false, "re-read artifact objects and compare their sha256 with the database")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *dir == "" {
		fmt.Fprintf(os.Stderr, "%s: -dir is required\n", name)
		return 2
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	cfg := server.BackupObjectsConfig{DatabaseURL: os.Getenv("AIWIKI_BACKUP_DATABASE_URL"), S3: s3ConfigFromEnv(), Dir: *dir, VerifyHashes: *verify, Out: os.Stdout}
	run := server.BackupObjects
	if restore {
		run = server.RestoreObjects
	}
	rep, err := run(ctx, cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", name, err)
		return 2
	}
	if rep.Failed() {
		return 1
	}
	return 0
}
