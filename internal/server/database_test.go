package server

import (
	"context"
	"os"
	"testing"
)

func TestRebindPostgres(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"plain", "SELECT ? , ?", "SELECT $1 , $2"},
		{"none", "SELECT 1", "SELECT 1"},
		{"string literal", "SELECT '?', ?", "SELECT '?', $1"},
		{"escaped quote", "SELECT 'it''s ?', ?", "SELECT 'it''s ?', $1"},
		{"E string backslash quote", `SELECT E'a\'?', ?`, `SELECT E'a\'?', $1`},
		{"standard string backslash", `SELECT 'a\', ?`, `SELECT 'a\', $1`},
		{"quoted identifier", `SELECT "a?b", ?`, `SELECT "a?b", $1`},
		{"line comment", "SELECT ? -- why ?\n, ?", "SELECT $1 -- why ?\n, $2"},
		{"block comment", "SELECT /* ? /* nested ? */ ? */ ?", "SELECT /* ? /* nested ? */ ? */ $1"},
		{"dollar string", "SELECT $$a ? b$$, ?", "SELECT $$a ? b$$, $1"},
		{"tagged dollar string", "SELECT $f$ $$ ? $f$, ?", "SELECT $f$ $$ ? $f$, $1"},
		{"function body", "CREATE FUNCTION f() RETURNS int AS $body$ SELECT ?; $body$ LANGUAGE sql; SELECT ?", "CREATE FUNCTION f() RETURNS int AS $body$ SELECT ?; $body$ LANGUAGE sql; SELECT $1"},
		{"ident with dollar", "SELECT a$b, ?", "SELECT a$b, $1"},
		{"unterminated string", "SELECT 'abc ?", "SELECT 'abc ?"},
		{"like arg text", "SELECT x FROM t WHERE y LIKE ? AND z=?", "SELECT x FROM t WHERE y LIKE $1 AND z=$2"},
	}
	for _, c := range cases {
		if got := rebindPostgres(c.in); got != c.want {
			t.Errorf("%s: rebind(%q) = %q, want %q", c.name, c.in, got, c.want)
		}
	}
}

func TestDatabaseSQLiteWrapperAndHelpers(t *testing.T) {
	s := testServer(t)
	if s.DB.pg {
		t.Fatal("sqlite server must not be pg")
	}
	tx, err := s.DB.Begin()
	if err != nil {
		t.Fatal(err)
	}
	a := Article{ID: "db-helper", SpaceID: "general", Title: "Helper", Kind: "article", Status: "active", Tags: []string{"Alpha", "beta"}, Properties: map[string]string{"k": "v"}, Revision: 1, CreatedAt: stamp(), UpdatedAt: stamp(), Author: "t"}
	if err := insertArticle(tx, a, "fixture"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := s.DB.QueryRow(`SELECT count(*) FROM articles a WHERE a.id=? AND `+s.DB.jsonArrayHasSQL("a.tags")+` AND `+s.DB.jsonObjectHasSQL("a.properties"), a.ID, "beta", "k", "v").Scan(&n); err != nil || n != 1 {
		t.Fatalf("json helpers: n=%d err=%v", n, err)
	}
	var space string
	if err := s.DB.QueryRow(`SELECT `+s.DB.jsonFieldSQL("snapshot", "space_id")+` FROM revisions WHERE article_id=?`, a.ID).Scan(&space); err != nil || space != "general" {
		t.Fatalf("json field: %q %v", space, err)
	}
	scoped, cleanup, err := s.DB.scoped(context.Background(), "u1")
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if err := scoped.QueryRow(`SELECT count(*) FROM articles`).Scan(&n); err != nil || n == 0 {
		t.Fatalf("scoped sqlite query: n=%d err=%v", n, err)
	}
	cleanup()
	cleanup()
}

func TestSQLIdentRejectsUnsafe(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic")
		}
	}()
	sqlIdent("a.tags; DROP TABLE x")
}

// TestPostgresIntegration needs AIWIKI_TEST_POSTGRES_DSN for an unprivileged role
// on an empty database.
func TestPostgresIntegration(t *testing.T) {
	dsn := os.Getenv("AIWIKI_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("AIWIKI_TEST_POSTGRES_DSN not set")
	}
	ctx := context.Background()
	db, err := openPostgres(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := &Server{DB: db, DataDir: t.TempDir()}
	for i := 0; i < 2; i++ { // second run must be a no-op
		if err := s.migrate(); err != nil {
			t.Fatalf("migrate #%d: %v", i, err)
		}
	}
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`INSERT INTO spaces(id,name) VALUES(?,?) ON CONFLICT DO NOTHING`, "pg-space", "PG"); err != nil {
		t.Fatal(err)
	}
	a := Article{ID: "pg-art", SpaceID: "pg-space", Title: "T", Kind: "article", Status: "active", Tags: []string{"x"}, Properties: map[string]string{"k": "v"}, Revision: 1, CreatedAt: stamp(), UpdatedAt: stamp(), Author: "t"}
	if err := insertArticle(tx, a, "fixture"); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	if err := indexArticle(tx, a); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM articles a WHERE a.id=? AND `+db.jsonArrayHasSQL("a.tags")+` AND `+db.jsonObjectHasSQL("a.properties"), a.ID, "x", "k", "v").Scan(&n); err != nil || n != 1 {
		t.Fatalf("pg json helpers: n=%d err=%v", n, err)
	}
	scoped, cleanup, err := db.scoped(ctx, "user-1")
	if err != nil {
		t.Fatal(err)
	}
	var got string
	if err := scoped.QueryRow(`SELECT current_setting('app.user_id')`).Scan(&got); err != nil || got != "user-1" {
		t.Fatalf("scoped user: %q %v", got, err)
	}
	cleanup()
	if err := scoped.QueryRow(`SELECT 1`).Scan(&n); err == nil {
		t.Fatal("scoped database must be unusable after cleanup")
	}
}
