package server

import (
	"context"
	_ "embed"
	"errors"
)

//go:embed semantic_index.sql
var semanticIndexSQL string

// migrateSemantic adds the chunk vector table with forced RLS (component
// "semantic_index"). The table exists even when semantic search is off, so
// enabling it later needs no migration.
func (s *Server) migrateSemantic(ctx context.Context) error {
	if s == nil || s.DB == nil || !s.DB.pg {
		return errors.New("semantic index migration requires PostgreSQL")
	}
	return s.DB.runMigrations(ctx, "semantic_index", []pgMigration{{
		Version: 1, Name: "article chunk vectors with article visibility", Statements: splitCorporateSQL(semanticIndexSQL),
	}})
}

// migrateSemanticSQLite creates the trusted-mode table (no row-level security in
// single-user mode).
func (s *Server) migrateSemanticSQLite() error {
	_, err := s.DB.Exec(`CREATE TABLE IF NOT EXISTS article_embeddings(article_id TEXT NOT NULL REFERENCES articles(id),model TEXT NOT NULL,text_key TEXT NOT NULL,dims INTEGER NOT NULL,vector BLOB NOT NULL,created_at TEXT NOT NULL,PRIMARY KEY(article_id,model,text_key))`)
	return err
}

// validateSemanticSchema is the startup protection check: forced RLS, a separate
// owner, exactly the three policies delegating to article visibility, and no
// UPDATE or ALL policy.
func (d *database) validateSemanticSchema() error {
	var count int
	if err := d.QueryRow(`SELECT count(*) FROM pg_class c WHERE c.relnamespace='public'::regnamespace AND c.relname='article_embeddings' AND c.relrowsecurity AND c.relforcerowsecurity AND NOT pg_has_role(current_user,c.relowner,'MEMBER')`).Scan(&count); err != nil || count != 1 {
		return errors.New("article_embeddings requires FORCE RLS and a separate owner")
	}
	if err := d.QueryRow(`SELECT count(*) FROM pg_policy p WHERE p.polrelid='public.article_embeddings'::regclass AND p.polpermissive AND p.polcmd NOT IN ('r','a','d')`).Scan(&count); err != nil || count != 0 {
		return errors.New("article_embeddings must not be updatable by the runtime role")
	}
	const visible = `(EXISTS ( SELECT 1
   FROM articles a
  WHERE (a.id = article_embeddings.article_id)))`
	if err := d.QueryRow(`SELECT count(*) FROM pg_policy p WHERE p.polrelid='public.article_embeddings'::regclass AND p.polpermissive AND (
 (p.polname='article_embeddings_read' AND p.polcmd='r' AND replace(pg_get_expr(p.polqual,p.polrelid),'public.articles','articles')=? AND p.polwithcheck IS NULL) OR
 (p.polname='article_embeddings_insert' AND p.polcmd='a' AND replace(pg_get_expr(p.polwithcheck,p.polrelid),'public.articles','articles')=? AND p.polqual IS NULL) OR
 (p.polname='article_embeddings_delete' AND p.polcmd='d' AND replace(pg_get_expr(p.polqual,p.polrelid),'public.articles','articles')=? AND p.polwithcheck IS NULL))`, visible, visible, visible).Scan(&count); err != nil || count != 3 {
		return errors.New("article_embeddings row-level security policies are missing or changed")
	}
	// The printed text does not say which schema "articles" is in; the stored
	// dependency does.
	if err := d.QueryRow(`SELECT count(*) FROM pg_policy p WHERE p.polrelid='public.article_embeddings'::regclass AND p.polpermissive AND EXISTS (SELECT 1 FROM pg_depend d WHERE d.classid='pg_policy'::regclass AND d.objid=p.oid AND d.refclassid='pg_class'::regclass AND d.refobjid='public.articles'::regclass)`).Scan(&count); err != nil || count != 3 {
		return errors.New("article_embeddings policies must refer to public.articles")
	}
	if err := d.QueryRow(`SELECT count(*) FROM pg_policy p WHERE p.polrelid='public.article_embeddings'::regclass AND p.polpermissive`).Scan(&count); err != nil || count != 3 {
		return errors.New("article_embeddings has unexpected row-level security policies")
	}
	return nil
}
