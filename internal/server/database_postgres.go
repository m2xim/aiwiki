package server

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

// openPostgres connects to PostgreSQL and refuses to run as a role that could
// bypass row-level security: superusers, BYPASSRLS roles (checked for both the
// session and the current user) and sessions with row_security off.
func openPostgres(ctx context.Context, dsn string) (*database, error) {
	config, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, errors.New("invalid PostgreSQL connection configuration")
	}
	config.RuntimeParams["statement_timeout"] = "30000"
	config.RuntimeParams["lock_timeout"] = "5000"
	config.RuntimeParams["idle_in_transaction_session_timeout"] = "30000"
	config.RuntimeParams["application_name"] = "aiwiki"
	pool := stdlib.OpenDB(*config)
	pool.SetMaxOpenConns(20)
	pool.SetMaxIdleConns(5)
	pool.SetConnMaxLifetime(30 * time.Minute)
	fail := func(err error) (*database, error) {
		pool.Close()
		return nil, err
	}
	pctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := pool.PingContext(pctx); err != nil {
		return fail(errors.New("PostgreSQL is not reachable; check connection and role configuration"))
	}
	var privileged bool
	var roles int
	var rowSecurity string
	if err := pool.QueryRowContext(pctx, `SELECT coalesce(bool_or(rolsuper OR rolbypassrls),false),count(*),current_setting('row_security') FROM pg_roles WHERE rolname IN (current_user,session_user)`).Scan(&privileged, &roles, &rowSecurity); err != nil {
		return fail(fmt.Errorf("cannot verify database role: %w", err))
	}
	if roles == 0 {
		return fail(errors.New("cannot verify database role: role not found"))
	}
	if privileged {
		return fail(errors.New("database role must not be a superuser or have BYPASSRLS"))
	}
	if rowSecurity != "on" {
		return fail(errors.New("database session must have row_security=on"))
	}
	return &database{pool: pool, pg: true}, nil
}

// pgMigration is one ordered, transactional schema step of a component.
type pgMigration struct {
	Version    int
	Name       string
	Statements []string
}

// migrationLockKey serializes all schema migrations of all components.
const migrationLockKey int64 = 0x41495749_4b494d47

// runMigrations applies the missing migrations of a component, all in one
// transaction guarded by a PostgreSQL advisory lock, and records them in
// schema_migrations(component,version). Concurrent servers wait for the lock and
// then see the recorded versions. Other components (security) may reuse it.
func (d *database) runMigrations(ctx context.Context, component string, migrations []pgMigration) error {
	if !d.pg {
		return errors.New("migrations require PostgreSQL")
	}
	ordered := append([]pgMigration(nil), migrations...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Version < ordered[j].Version })
	for i, m := range ordered {
		if m.Version < 1 || (i > 0 && ordered[i-1].Version == m.Version) {
			return fmt.Errorf("invalid migration version %d for %s", m.Version, component)
		}
	}
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(?)`, migrationLockKey); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations(component TEXT NOT NULL,version INTEGER NOT NULL,name TEXT NOT NULL,applied_at TEXT NOT NULL,PRIMARY KEY(component,version))`); err != nil {
		return err
	}
	applied := map[int]bool{}
	rows, err := tx.QueryContext(ctx, `SELECT version FROM schema_migrations WHERE component=?`, component)
	if err != nil {
		return err
	}
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			rows.Close()
			return err
		}
		applied[v] = true
	}
	err = rows.Err()
	if cerr := rows.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	for _, m := range ordered {
		if applied[m.Version] {
			continue
		}
		for _, stmt := range m.Statements {
			if _, err := tx.ExecContext(ctx, stmt); err != nil {
				return fmt.Errorf("migration %s %d (%s): %w", component, m.Version, m.Name, err)
			}
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations(component,version,name,applied_at) VALUES(?,?,?,?)`, component, m.Version, m.Name, stamp()); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// migratePostgres creates the base article schema (the SQLite schema with text
// JSON columns, integer flags and an ordinary article_fts table; the legacy
// ranker scans rows and does not use full-text search).
func (s *Server) migratePostgres() error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	return s.DB.runMigrations(ctx, "base", baseMigrations)
}

var baseMigrations = []pgMigration{{Version: 1, Name: "base article schema", Statements: []string{
	`CREATE OR REPLACE FUNCTION aiwiki_json_array(t text) RETURNS jsonb LANGUAGE plpgsql IMMUTABLE STRICT SET search_path = pg_catalog AS $$
BEGIN
  IF jsonb_typeof(t::jsonb) = 'array' THEN RETURN t::jsonb; END IF;
  RETURN '[]'::jsonb;
EXCEPTION WHEN others THEN
  RETURN '[]'::jsonb;
END $$`,
	`CREATE OR REPLACE FUNCTION aiwiki_json_object(t text) RETURNS jsonb LANGUAGE plpgsql IMMUTABLE STRICT SET search_path = pg_catalog AS $$
BEGIN
  IF jsonb_typeof(t::jsonb) = 'object' THEN RETURN t::jsonb; END IF;
  RETURN '{}'::jsonb;
EXCEPTION WHEN others THEN
  RETURN '{}'::jsonb;
END $$`,
	`CREATE TABLE spaces(id TEXT PRIMARY KEY,name TEXT NOT NULL,description TEXT NOT NULL DEFAULT '',archived_at TEXT)`,
	`CREATE TABLE articles(id TEXT PRIMARY KEY,space_id TEXT NOT NULL REFERENCES spaces(id),parent_id TEXT NOT NULL DEFAULT '',project_id TEXT NOT NULL DEFAULT '',title TEXT NOT NULL,summary TEXT NOT NULL DEFAULT '',body TEXT NOT NULL DEFAULT '',kind TEXT NOT NULL,status TEXT NOT NULL,pinned INTEGER NOT NULL DEFAULT 0,tags TEXT NOT NULL DEFAULT '[]',properties TEXT NOT NULL DEFAULT '{}',revision INTEGER NOT NULL,created_at TEXT NOT NULL,updated_at TEXT NOT NULL,author TEXT NOT NULL,deleted_at TEXT)`,
	`CREATE INDEX articles_space_updated ON articles(space_id,updated_at DESC)`,
	`CREATE INDEX articles_parent ON articles(parent_id)`,
	`CREATE TABLE revisions(id BIGSERIAL PRIMARY KEY,article_id TEXT NOT NULL REFERENCES articles(id),revision INTEGER NOT NULL,author TEXT NOT NULL,message TEXT NOT NULL,created_at TEXT NOT NULL,snapshot TEXT NOT NULL,UNIQUE(article_id,revision))`,
	`CREATE INDEX revisions_snapshot_space ON revisions ((aiwiki_json_object(snapshot)->>'space_id'))`,
	`CREATE TABLE comments(id TEXT PRIMARY KEY,article_id TEXT NOT NULL REFERENCES articles(id),anchor TEXT NOT NULL,body TEXT NOT NULL,author TEXT NOT NULL,created_at TEXT NOT NULL,resolved INTEGER NOT NULL DEFAULT 0)`,
	`CREATE INDEX comments_article ON comments(article_id)`,
	`CREATE TABLE attachments(id TEXT PRIMARY KEY,article_id TEXT NOT NULL REFERENCES articles(id),name TEXT NOT NULL,mime TEXT NOT NULL,size BIGINT NOT NULL,stored_name TEXT NOT NULL,created_at TEXT NOT NULL,deleted_at TEXT)`,
	`CREATE INDEX attachments_article ON attachments(article_id)`,
	// Per-article favorites; the security migration rebuilds this per user.
	`CREATE TABLE favorites(article_id TEXT PRIMARY KEY REFERENCES articles(id),created_at TEXT NOT NULL)`,
	// Ordinary table mirroring the SQLite FTS5 columns (one row per article).
	`CREATE TABLE article_fts(article_id TEXT PRIMARY KEY,title TEXT NOT NULL DEFAULT '',summary TEXT NOT NULL DEFAULT '',body TEXT NOT NULL DEFAULT '',tags TEXT NOT NULL DEFAULT '')`,
}}}
