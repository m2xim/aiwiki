package server

import (
	"context"
	"os"
	"strings"
	"testing"
)

func TestCorporateStartupAndScopeCleanup(t *testing.T) {
	_, root := corporateFixture(t)
	if err := root.DB.validateRuntimeSchema(); err != nil {
		t.Fatal(err)
	}
	root.DB.pool.SetMaxOpenConns(1)
	scoped, done, err := root.DB.scoped(context.Background(), "alice")
	if err != nil {
		t.Fatal(err)
	}
	tx, err := scoped.Begin()
	if err != nil {
		t.Fatal(err)
	}
	// Deliberately forget Rollback: cleanup must own even this failure path.
	if _, err = tx.Exec("SELECT 1"); err != nil {
		t.Fatal(err)
	}
	done()
	if tx.Commit() == nil {
		t.Fatal("unfinished transaction survived scope cleanup")
	}
	next, closeNext, err := root.DB.scoped(context.Background(), "bob")
	if err != nil {
		t.Fatal(err)
	}
	var who string
	if err = next.QueryRow("SELECT current_setting('app.user_id',true)").Scan(&who); err != nil || who != "bob" {
		t.Fatalf("identity leakage %q %v", who, err)
	}
	closeNext()
	if err = root.DB.QueryRow("SELECT current_setting('app.user_id',true)").Scan(&who); err != nil || who != "" {
		t.Fatalf("poisoned pool %q %v", who, err)
	}
	s3Env(t)
	_, endpoint := newFakeS3(t)
	auth := newAuthHarness(t, nil)
	cfg := auth.a.cfg
	cfg.DatabaseURL = os.Getenv("AIWIKI_TEST_POSTGRES")
	cfg.MigrationDatabaseURL = os.Getenv("AIWIKI_TEST_POSTGRES_OWNER")
	cfg.RuntimeRole = "aiwiki_app"
	cfg.S3 = S3Config{Bucket: "wiki", Region: "us-east-1", Endpoint: endpoint.URL, PathStyle: true, AllowInsecureLoopback: true}
	started, err := OpenCorporate(context.Background(), cfg, t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	if started.authDB == nil || started.authDB == started.DB || started.objects == nil {
		t.Fatal("startup lost storage or auth pool isolation")
	}
	started.Close()
	cfg.DatabaseURL = cfg.MigrationDatabaseURL
	cfg.MigrationDatabaseURL = ""
	if _, err = OpenCorporate(context.Background(), cfg, t.TempDir(), ""); err == nil || !strings.Contains(err.Error(), "runtime role") {
		t.Fatalf("owner runtime not rejected: %v", err)
	}
}
