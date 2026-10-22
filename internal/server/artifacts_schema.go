package server

import (
	"context"
	_ "embed"
	"errors"
)

//go:embed artifacts.sql
var artifactsSchemaSQL string

//go:embed artifacts_2.sql
var artifactsSchema2SQL string

// migrateArtifacts adds the artifact tables, forced RLS and guards (component
// "artifacts", append-only on top of corporate_acl, mcp_access, corporate_moves).
func (s *Server) migrateArtifacts(ctx context.Context) error {
	if s == nil || s.DB == nil || !s.DB.pg {
		return errors.New("artifact migration requires PostgreSQL")
	}
	return s.DB.runMigrations(ctx, "artifacts", []pgMigration{{
		Version: 1, Name: "interactive artifacts, forced RLS and settings", Statements: splitCorporateSQL(artifactsSchemaSQL),
	}, {
		Version: 2, Name: "deleted visibility, link management and creator validity", Statements: splitCorporateSQL(artifactsSchema2SQL),
	}})
}

// migrateArtifactsSQLite creates the trusted-mode tables. Trusted mode has no
// row-level security and no users; the owner is always empty.
func (s *Server) migrateArtifactsSQLite() error {
	_, err := s.DB.Exec(`CREATE TABLE IF NOT EXISTS artifacts(id TEXT PRIMARY KEY,space_id TEXT NOT NULL REFERENCES spaces(id),project_id TEXT NOT NULL DEFAULT '',title TEXT NOT NULL,description TEXT NOT NULL DEFAULT '',visibility TEXT NOT NULL DEFAULT 'inherit' CHECK(visibility IN ('inherit','private')),owner_id TEXT NOT NULL DEFAULT '',revision INTEGER NOT NULL,author TEXT NOT NULL,created_at TEXT NOT NULL,updated_at TEXT NOT NULL,deleted_at TEXT,file_count INTEGER NOT NULL DEFAULT 0,projection TEXT NOT NULL DEFAULT '{}',search_text TEXT NOT NULL DEFAULT '');
CREATE INDEX IF NOT EXISTS artifacts_space_updated_idx ON artifacts(space_id,project_id,updated_at DESC);
CREATE INDEX IF NOT EXISTS artifacts_project_idx ON artifacts(project_id);
CREATE TABLE IF NOT EXISTS artifact_revisions(artifact_id TEXT NOT NULL REFERENCES artifacts(id),revision INTEGER NOT NULL,author TEXT NOT NULL,message TEXT NOT NULL DEFAULT '',created_at TEXT NOT NULL,manifest TEXT NOT NULL,file_count INTEGER NOT NULL DEFAULT 0,PRIMARY KEY(artifact_id,revision));
CREATE TABLE IF NOT EXISTS artifact_blobs(sha256 TEXT PRIMARY KEY,size INTEGER NOT NULL,mime TEXT NOT NULL,stored_name TEXT NOT NULL,created_at TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS artifact_shares(id TEXT PRIMARY KEY,artifact_id TEXT NOT NULL REFERENCES artifacts(id),token_hash TEXT NOT NULL UNIQUE,revision INTEGER,created_by TEXT NOT NULL,created_at TEXT NOT NULL,revoked_at TEXT);
CREATE INDEX IF NOT EXISTS artifact_shares_artifact_idx ON artifact_shares(artifact_id);
CREATE TABLE IF NOT EXISTS app_settings(key TEXT PRIMARY KEY,value TEXT NOT NULL);`)
	return err
}

// validateArtifactSchema is the startup protection check of the artifact
// objects: forced RLS, the expected policies and guards, no delete path for the
// runtime role, a separate owner, and a non-public SECURITY DEFINER surface.
func (d *database) validateArtifactSchema() error {
	var count int
	if err := d.QueryRow(`SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='public' AND c.relname IN ('artifacts','artifact_revisions','artifact_blobs','artifact_shares','app_settings')`).Scan(&count); err != nil || count != 5 {
		return errors.New("artifact schema migration is required")
	}
	if err := d.QueryRow(`SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='public' AND c.relname IN ('artifacts','artifact_revisions','artifact_blobs','artifact_shares','app_settings') AND (NOT c.relrowsecurity OR NOT c.relforcerowsecurity OR pg_has_role(current_user,c.relowner,'MEMBER'))`).Scan(&count); err != nil || count != 0 {
		return errors.New("artifact tables require FORCE RLS and a separate owner")
	}
	// Exactly the supported operations have a permissive policy: no DELETE or ALL
	// policy anywhere, no UPDATE policy on revisions/blobs, no SELECT on blobs.
	if err := d.QueryRow(`SELECT count(*) FROM pg_policy p JOIN pg_class c ON c.oid=p.polrelid WHERE c.relnamespace='public'::regnamespace AND c.relname IN ('artifacts','artifact_revisions','artifact_blobs','artifact_shares','app_settings') AND p.polpermissive AND (p.polcmd IN ('d','*') OR (c.relname IN ('artifact_revisions','artifact_blobs') AND p.polcmd='w') OR (c.relname='artifact_blobs' AND p.polcmd='r') OR (c.relname='app_settings' AND p.polcmd='d'))`).Scan(&count); err != nil || count != 0 {
		return errors.New("artifact tables must not be deletable or rewritable by the runtime role")
	}
	if err := d.QueryRow(`SELECT count(*) FROM pg_policy p JOIN pg_class c ON c.oid=p.polrelid WHERE c.relnamespace='public'::regnamespace AND ((c.relname='artifacts' AND p.polname IN ('artifacts_read','artifacts_view_scope','artifacts_probe','artifacts_insert','artifacts_update','mcp_scope_artifacts_insert','mcp_scope_artifacts_update')) OR (c.relname='artifact_revisions' AND p.polname IN ('artifact_revisions_read','artifact_revisions_insert','mcp_scope_artifact_revisions_insert')) OR (c.relname='artifact_blobs' AND p.polname IN ('artifact_blobs_insert','mcp_scope_artifact_blobs_insert')) OR (c.relname='artifact_shares' AND p.polname IN ('artifact_shares_read','artifact_shares_view_scope','artifact_shares_insert','artifact_shares_update','artifact_shares_probe_read','artifact_shares_probe_update','mcp_scope_artifact_shares_insert','mcp_scope_artifact_shares_update')) OR (c.relname='app_settings' AND p.polname IN ('app_settings_read','app_settings_insert','app_settings_update')))`).Scan(&count); err != nil || count != 23 {
		return errors.New("artifact row-level security policies are missing")
	}
	// The settings table is writable by system administrators only.
	if err := d.QueryRow(`SELECT count(*) FROM pg_policy p WHERE p.polrelid='public.app_settings'::regclass AND p.polname IN ('app_settings_insert','app_settings_update') AND pg_get_expr(p.polwithcheck,p.polrelid) LIKE '%artifact_is_admin%' AND pg_get_expr(p.polwithcheck,p.polrelid) LIKE '%corporate_mcp_scope_active%'`).Scan(&count); err != nil || count != 2 {
		return errors.New("application settings must be writable by administrators only")
	}
	if err := d.QueryRow(`SELECT count(*) FROM pg_trigger WHERE NOT tgisinternal AND tgenabled IN ('O','A') AND ((tgrelid='public.artifacts'::regclass AND tgname IN ('artifacts_guard','artifacts_revoke_shares')) OR (tgrelid='public.artifact_shares'::regclass AND tgname='artifact_shares_guard') OR (tgrelid='public.projects'::regclass AND tgname='artifact_projects_no_move'))`).Scan(&count); err != nil || count != 4 {
		return errors.New("artifact guard triggers are missing or disabled")
	}
	var ok bool
	if err := d.QueryRow(`SELECT (SELECT count(*) FROM pg_proc p WHERE p.oid IN (to_regprocedure('public.artifact_is_admin(text)'),to_regprocedure('public.artifact_revoke_shares()'),to_regprocedure('public.artifact_guard_project_move()')) AND p.prosecdef AND p.proconfig @> ARRAY['search_path=pg_catalog, public'])=3
 AND NOT EXISTS (SELECT 1 FROM pg_proc p, aclexplode(COALESCE(p.proacl,acldefault('f',p.proowner))) a WHERE p.oid IN (to_regprocedure('public.artifact_is_admin(text)'),to_regprocedure('public.artifact_role(text,text)'),to_regprocedure('public.artifact_can_read(text,text,text,text)'),to_regprocedure('public.artifact_can_write(text,text,text,text)'),to_regprocedure('public.artifact_can_share(text,text,text,text)'),to_regprocedure('public.artifact_guard()'),to_regprocedure('public.artifact_share_guard()'),to_regprocedure('public.artifact_revoke_shares()'),to_regprocedure('public.artifact_guard_project_move()')) AND a.grantee=0)
 AND has_function_privilege(current_user,'public.artifact_role(text,text)','EXECUTE')`).Scan(&ok); err != nil || !ok {
		return errors.New("artifact functions are missing, not SECURITY DEFINER where required or executable by PUBLIC")
	}
	// v2: deleted artifacts only for restorers, link management without the
	// writable-container test, link validity as a non-PUBLIC SECURITY DEFINER.
	if err := d.QueryRow(`SELECT (SELECT count(*) FROM pg_policy p WHERE (p.polrelid='public.artifacts'::regclass AND p.polname='artifacts_read' AND pg_get_expr(p.polqual,p.polrelid) LIKE '%artifact_can_view%') OR (p.polrelid='public.artifact_shares'::regclass AND p.polname IN ('artifact_shares_read','artifact_shares_update') AND pg_get_expr(p.polqual,p.polrelid) LIKE '%artifact_can_manage_links%' AND pg_get_expr(p.polqual,p.polrelid) NOT LIKE '%writable%'))=3
 AND EXISTS (SELECT 1 FROM pg_proc p WHERE p.oid=to_regprocedure('public.artifact_link_valid(text,text)') AND p.prosecdef AND p.proconfig @> ARRAY['search_path=pg_catalog, public'])
 AND has_function_privilege(current_user,'public.artifact_link_valid(text,text)','EXECUTE')
 AND NOT EXISTS (SELECT 1 FROM pg_proc p, aclexplode(COALESCE(p.proacl,acldefault('f',p.proowner))) a WHERE p.oid IN (to_regprocedure('public.artifact_link_valid(text,text)'),to_regprocedure('public.artifact_can_view(text,text,text,text,text)'),to_regprocedure('public.artifact_can_manage_links(text,text,text,text)')) AND a.grantee=0)`).Scan(&ok); err != nil || !ok {
		return errors.New("artifact link and deletion policies (schema v2) are missing or executable by PUBLIC")
	}
	return nil
}
