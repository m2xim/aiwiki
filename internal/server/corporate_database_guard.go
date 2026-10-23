package server

import "errors"

func (d *database) validateRuntimeSchema() error {
	var count int
	if err := d.QueryRow(`SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='public' AND c.relname IN ('spaces','articles','revisions','comments','attachments','favorites','article_fts') AND (NOT c.relrowsecurity OR NOT c.relforcerowsecurity OR pg_has_role(current_user,c.relowner,'MEMBER'))`).Scan(&count); err != nil || count != 0 {
		return errors.New("runtime role must not own content tables and FORCE RLS must be enabled")
	}
	if err := d.QueryRow(`SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='public' AND c.relname IN ('spaces','articles','revisions','comments','attachments','favorites','article_fts')`).Scan(&count); err != nil || count != 7 {
		return errors.New("corporate schema is not initialized")
	}
	var memberOf int
	if err := d.QueryRow(`SELECT count(*) FROM pg_auth_members m WHERE m.member=(SELECT oid FROM pg_roles WHERE rolname=current_user)`).Scan(&memberOf); err != nil || memberOf != 0 {
		return errors.New("runtime role must not be a member of any role")
	}
	var mcpReady bool
	if err := d.QueryRow(`SELECT to_regprocedure('public.corporate_mcp_scope_active()') IS NOT NULL AND to_regprocedure('public.corporate_owner_effective_role(text,text,text)') IS NOT NULL`).Scan(&mcpReady); err != nil || !mcpReady {
		return errors.New("MCP access schema migration is required")
	}
	if err := d.QueryRow(`SELECT EXISTS(SELECT 1 FROM pg_proc p JOIN pg_language l ON l.oid=p.prolang WHERE p.oid=to_regprocedure('public.corporate_effective_role(text,text,text)') AND l.lanname='plpgsql' AND p.prosrc LIKE '%app.mcp_projects%') AND NOT has_function_privilege(current_user,'public.corporate_owner_effective_role(text,text,text)','EXECUTE')`).Scan(&mcpReady); err != nil || !mcpReady {
		return errors.New("MCP role restriction is missing or can be bypassed")
	}
	if err := d.QueryRow(`SELECT has_function_privilege(current_user,'public.corporate_effective_role(text,text,text)','EXECUTE') AND has_function_privilege(current_user,'public.corporate_mcp_request_audit(text,text,text)','EXECUTE')`).Scan(&mcpReady); err != nil || !mcpReady {
		return errors.New("MCP authorization/audit function grants are missing")
	}
	if err := d.QueryRow(`SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='public' AND c.relname IN ('users','teams','team_members','projects','memberships','security_audit','corporate_space_state') AND c.relrowsecurity AND NOT c.relforcerowsecurity`).Scan(&count); err != nil || count != 7 {
		return errors.New("MCP metadata protection is not initialized")
	}
	if err := d.QueryRow(`SELECT to_regprocedure('public.corporate_mcp_content_write_allowed()') IS NOT NULL AND has_function_privilege(current_user,'public.corporate_mcp_credential_audit(text,text,text)','EXECUTE') AND EXISTS(SELECT 1 FROM pg_proc WHERE oid=to_regprocedure('public.corporate_effective_role(text,text,text)') AND prosrc LIKE '%app.mcp_mode%')`).Scan(&mcpReady); err != nil || !mcpReady {
		return errors.New("persistent MCP scope migration/grants are required")
	}
	if err := d.QueryRow(`SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='public' AND c.relname IN ('mcp_credentials','mcp_service_accounts') AND c.relrowsecurity AND NOT c.relforcerowsecurity AND NOT pg_has_role(current_user,c.relowner,'MEMBER')`).Scan(&count); err != nil || count != 2 {
		return errors.New("persistent MCP tables require metadata RLS and a separate owner")
	}
	if err := d.QueryRow(`SELECT count(*) FROM pg_policy p JOIN pg_class c ON c.oid=p.polrelid JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='public' AND c.relname IN ('mcp_credentials','mcp_service_accounts') AND p.polname='mcp_scope_'||c.relname||'_default' AND p.polpermissive AND p.polcmd='*'`).Scan(&count); err != nil || count != 2 {
		return errors.New("persistent MCP metadata policies are missing")
	}
	credentialPolicy := `((NOT corporate_mcp_scope_active()) AND ((COALESCE(current_setting('app.user_id'::text, true), ''::text) = ''::text) OR (owner_id = current_setting('app.user_id'::text, true))))`
	if err := d.QueryRow(`SELECT count(*) FROM pg_policy WHERE polrelid IN ('public.mcp_credentials'::regclass,'public.mcp_service_accounts'::regclass)`).Scan(&count); err != nil || count != 4 {
		return errors.New("unexpected persistent MCP metadata policies")
	}
	if err := d.QueryRow(`SELECT count(*) FROM pg_policy p WHERE p.polrelid IN ('public.mcp_credentials'::regclass,'public.mcp_service_accounts'::regclass) AND p.polcmd='*' AND p.polpermissive AND pg_get_expr(p.polqual,p.polrelid)=? AND pg_get_expr(p.polwithcheck,p.polrelid)=?`, credentialPolicy, credentialPolicy).Scan(&count); err != nil || count != 2 {
		return errors.New("persistent MCP metadata owner isolation is missing")
	}
	if err := d.QueryRow(`SELECT count(*) FROM pg_policy p WHERE p.polrelid IN ('public.mcp_credentials'::regclass,'public.mcp_service_accounts'::regclass) AND p.polcmd='d' AND NOT p.polpermissive AND pg_get_expr(p.polqual,p.polrelid)='false'`).Scan(&count); err != nil || count != 2 {
		return errors.New("persistent MCP records must not be deletable by runtime")
	}
	if err := d.QueryRow(`SELECT count(*) FROM pg_trigger WHERE NOT tgisinternal AND tgenabled IN ('O','A') AND tgtype=19 AND ((tgrelid='public.mcp_credentials'::regclass AND tgname='mcp_credential_immutable' AND tgfoid=to_regprocedure('public.corporate_mcp_credential_immutable()')) OR (tgrelid='public.mcp_service_accounts'::regclass AND tgname='mcp_service_immutable' AND tgfoid=to_regprocedure('public.corporate_mcp_service_immutable()')))`).Scan(&count); err != nil || count != 2 {
		return errors.New("persistent MCP immutable authority guards are missing")
	}
	if err := d.QueryRow(`SELECT count(*) FROM pg_policy p JOIN pg_class c ON c.oid=p.polrelid JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='public' AND c.relname IN ('users','teams','team_members','projects','memberships','security_audit','corporate_space_state') AND p.polpermissive AND (p.polname='mcp_scope_'||c.relname||'_default' OR c.relname='projects' AND p.polname='mcp_scope_projects_read')`).Scan(&count); err != nil || count != 8 {
		return errors.New("MCP metadata policies are missing")
	}
	if err := d.QueryRow(`SELECT count(*) FROM pg_policy p JOIN pg_class c ON c.oid=p.polrelid JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='public' AND c.relname IN ('spaces','articles','revisions','comments','attachments','favorites','article_fts') AND p.polname LIKE 'mcp_scope_%' AND NOT p.polpermissive AND p.polcmd IN ('a','w','d')`).Scan(&count); err != nil || count != 21 {
		return errors.New("MCP access write protection is not initialized")
	}
	if err := d.QueryRow(`WITH guards AS (
	 SELECT p.*,c.relname, CASE WHEN (c.relname,p.polcmd) IN (('articles','a'),('articles','w'),('revisions','a'),('comments','a'),('article_fts','a'),('article_fts','d')) THEN 'corporate_mcp_content_write_allowed()' ELSE '(NOT corporate_mcp_scope_active())' END AS expected
	 FROM pg_policy p JOIN pg_class c ON c.oid=p.polrelid JOIN pg_namespace n ON n.oid=c.relnamespace
	 WHERE n.nspname='public' AND c.relname IN ('spaces','articles','revisions','comments','attachments','favorites','article_fts') AND p.polname LIKE 'mcp_scope_%' AND NOT p.polpermissive AND p.polcmd IN ('a','w','d'))
	 SELECT count(*) FROM guards WHERE pg_get_expr(polqual,polrelid) IS NOT DISTINCT FROM CASE WHEN polcmd='a' THEN NULL ELSE expected END AND pg_get_expr(polwithcheck,polrelid) IS NOT DISTINCT FROM CASE WHEN polcmd='d' THEN NULL ELSE expected END`).Scan(&count); err != nil || count != 21 {
		return errors.New("MCP content write policies do not match the supported tool operations")
	}
	if err := d.QueryRow(`SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='public' AND c.relname IN ('users','teams','team_members','projects','memberships','security_audit','corporate_space_state','schema_migrations') AND pg_has_role(current_user,c.relowner,'MEMBER')`).Scan(&count); err != nil || count != 0 {
		return errors.New("runtime role must not own ACL, identity or migration tables")
	}
	if err := d.QueryRow(`SELECT EXISTS(SELECT 1 FROM pg_proc WHERE oid=to_regprocedure('public.corporate_mcp_oauth_audit(text,text,text)') AND prosecdef AND prosrc LIKE '%mcp.oauth.request%') AND has_function_privilege(current_user,'public.corporate_mcp_oauth_audit(text,text,text)','EXECUTE') AND NOT EXISTS(SELECT 1 FROM pg_proc p, aclexplode(COALESCE(p.proacl,acldefault('f',p.proowner))) a WHERE p.oid=to_regprocedure('public.corporate_mcp_oauth_audit(text,text,text)') AND a.grantee=0)`).Scan(&mcpReady); err != nil || !mcpReady {
		return errors.New("MCP OAuth audit function is missing, not SECURITY DEFINER or executable by PUBLIC")
	}
	if err := d.QueryRow(`SELECT EXISTS(SELECT 1 FROM pg_proc WHERE oid=to_regprocedure('public.corporate_move_impact(text,text,text,text)') AND prosecdef AND proconfig @> ARRAY['search_path=pg_catalog, public']) AND has_function_privilege(current_user,'public.corporate_move_impact(text,text,text,text)','EXECUTE') AND NOT EXISTS(SELECT 1 FROM pg_proc p, aclexplode(COALESCE(p.proacl,acldefault('f',p.proowner))) a WHERE p.oid IN (to_regprocedure('public.corporate_move_impact(text,text,text,text)'),to_regprocedure('public.corporate_guard_project_move()'),to_regprocedure('public.corporate_reject_article_move()')) AND a.grantee=0) AND EXISTS(SELECT 1 FROM pg_proc WHERE oid=to_regprocedure('public.corporate_guard_project_move()') AND prosecdef)`).Scan(&mcpReady); err != nil || !mcpReady {
		return errors.New("safe-move functions are missing, not SECURITY DEFINER or executable by PUBLIC")
	}
	if err := d.QueryRow(`SELECT count(*) FROM pg_trigger WHERE NOT tgisinternal AND tgenabled IN ('O','A') AND ((tgrelid='public.articles'::regclass AND tgname='corporate_articles_no_move' AND tgfoid=to_regprocedure('public.corporate_reject_article_move()')) OR (tgrelid='public.projects'::regclass AND tgname='corporate_projects_no_move' AND tgfoid=to_regprocedure('public.corporate_guard_project_move()')))`).Scan(&count); err != nil || count != 2 {
		return errors.New("article/project move guard triggers are missing or disabled")
	}
	if err := d.QueryRow(`SELECT count(*) FROM pg_trigger WHERE NOT tgisinternal AND tgenabled IN ('O','A') AND tgtype=23 AND tgrelid='public.revisions'::regclass AND tgname='revisions_fill_scope' AND tgfoid=to_regprocedure('public.revisions_fill_scope()')`).Scan(&count); err != nil || count != 1 {
		return errors.New("revision scope trigger is missing or disabled")
	}
	if err := d.validateRoleScope(); err != nil {
		return err
	}
	var unsafe bool
	if err := d.QueryRow(`SELECT has_schema_privilege(current_user,'public','CREATE') OR has_table_privilege(current_user,'security_audit','UPDATE,DELETE') OR EXISTS(SELECT 1 FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='public' AND c.relkind IN ('r','p') AND has_table_privilege(current_user,c.oid,'TRUNCATE,TRIGGER')) OR EXISTS(SELECT 1 FROM pg_roles WHERE rolname=current_user AND (rolsuper OR rolbypassrls OR rolcreaterole))`).Scan(&unsafe); err != nil || unsafe {
		return errors.New("runtime role has unsafe schema, audit or role privileges")
	}
	if err := d.validateArtifactSchema(); err != nil {
		return err
	}
	if err := d.validateStorageQuotaSchema(); err != nil {
		return err
	}
	return d.validateSemanticSchema()
}
