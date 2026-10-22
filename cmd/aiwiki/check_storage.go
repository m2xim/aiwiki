package main

import (
	"aiwiki/internal/server"
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"time"
)

// runCheckStorage implements "aiwiki check-storage"; it returns the exit code.
func runCheckStorage(args []string) int {
	fs := flag.NewFlagSet("check-storage", flag.ContinueOnError)
	verify := fs.Bool("verify-hashes", false, "re-read artifact objects and compare their sha256 with the database (reads every artifact object)")
	del := fs.Bool("delete-orphans", false, "delete bucket objects that no database row references (needs -yes; objects younger than -orphan-min-age are kept)")
	yes := fs.Bool("yes", false, "confirm -delete-orphans")
	allowEmpty := fs.Bool("allow-empty-database", false, "let -delete-orphans proceed although the database references no objects at all (normally a sign of the wrong database)")
	minAge := fs.Duration("orphan-min-age", 24*time.Hour, "orphans younger than this may belong to an upload in progress and are never deleted")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *minAge < 24*time.Hour {
		fmt.Fprintln(os.Stderr, "check-storage: -orphan-min-age must be at least 24h")
		return 2
	}
	url := os.Getenv("AIWIKI_BACKUP_DATABASE_URL")
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	rep, err := server.CheckStorage(ctx, server.StorageCheckConfig{
		DatabaseURL:  url,
		S3:           s3ConfigFromEnv(),
		VerifyHashes: *verify, DeleteOrphans: *del, Yes: *yes, AllowEmptyDatabase: *allowEmpty, OrphanMinAge: *minAge,
		Out: os.Stdout,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "check-storage:", err)
		return 2
	}
	if rep.Failed() {
		return 1
	}
	return 0
}

func s3ConfigFromEnv() server.S3Config {
	return server.S3Config{Bucket: os.Getenv("AIWIKI_S3_BUCKET"), Region: os.Getenv("AIWIKI_S3_REGION"), Endpoint: os.Getenv("AIWIKI_S3_ENDPOINT"), Prefix: os.Getenv("AIWIKI_S3_PREFIX"), PathStyle: os.Getenv("AIWIKI_S3_PATH_STYLE") == "1", AllowInsecureLoopback: os.Getenv("AIWIKI_ALLOW_INSECURE_LOOPBACK") == "1", AllowHTTPHosts: strings.FieldsFunc(os.Getenv("AIWIKI_S3_ALLOW_HTTP_HOSTS"), func(r rune) bool { return r == ',' || r == ' ' })}
}
