package server

import (
	"errors"
	"strconv"
)

var errAgentProjects = errors.New("agent project tables, policies, guards or functions are missing, altered or executable by PUBLIC")

// agentProjectFunctions are the SECURITY DEFINER functions of the agent project layer. All must pin
// search_path and be closed to PUBLIC; the runtime role needs EXECUTE on them.
var agentProjectFunctions = []string{
	"corporate_ap_live_user()", "corporate_ap_can_read(text,text,text)", "corporate_ap_can_write(text,text)",
	"corporate_ap_space_addable(text)", "corporate_admin_space_exists(text)", "corporate_admin_space_directory()",
	"corporate_agent_projects_visible(text[])", "corporate_agent_project_rows(text[])",
	"corporate_mcp_project_scope(text,text)", "corporate_user_project_scope(text,text)", "corporate_space_assignable_v2(text)",
	"corporate_mcp_credential_set_projects(text,jsonb,boolean)", "corporate_mcp_credential_audit(text,text,text)",
}

// validateAgentProjectsSchema checks that the new access model is intact: RLS on both project tables with
// exactly the expected policies, the immutability guards, the CHECKs that keep content out of folders,
// the closed SECURITY DEFINER surface, and a credential audit that re-derives the project scope.
func (d *database) validateAgentProjectsSchema() error {
	var ok bool
	var count int
	if err := d.QueryRow(`SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='public' AND c.relname IN ('agent_projects','agent_project_spaces') AND c.relrowsecurity AND NOT c.relforcerowsecurity AND NOT pg_has_role(current_user,c.relowner,'MEMBER')`).Scan(&count); err != nil || count != 2 {
		return errAgentProjects
	}
	if err := d.QueryRow(`SELECT count(*) FROM pg_policy p WHERE p.polrelid IN ('public.agent_projects'::regclass,'public.agent_project_spaces'::regclass) AND p.polpermissive AND p.polname IN ('agent_projects_read','agent_projects_insert','agent_projects_update','agent_projects_delete','agent_project_spaces_read','agent_project_spaces_insert','agent_project_spaces_update','agent_project_spaces_delete')`).Scan(&count); err != nil || count != 8 {
		return errAgentProjects
	}
	if err := d.QueryRow(`SELECT count(*) FROM pg_policy WHERE polrelid IN ('public.agent_projects'::regclass,'public.agent_project_spaces'::regclass)`).Scan(&count); err != nil || count != 8 {
		return errAgentProjects
	}
	// The read policy of projects must keep the owner/public rule and the MCP restriction (a function body check).
	if err := d.QueryRow(`SELECT (SELECT count(*) FROM pg_policy p WHERE p.polrelid='public.agent_projects'::regclass AND p.polname='agent_projects_read' AND pg_get_expr(p.polqual,p.polrelid) LIKE '%corporate_ap_can_read(id, owner_id, visibility)%')=1
 AND (SELECT count(*) FROM pg_policy p WHERE p.polrelid='public.agent_projects'::regclass AND p.polname IN ('agent_projects_update','agent_projects_delete') AND pg_get_expr(p.polqual,p.polrelid) LIKE '%corporate_ap_can_write(owner_id, visibility)%')=2
 AND EXISTS(SELECT 1 FROM pg_proc WHERE oid=to_regprocedure('public.corporate_ap_can_write(text,text)') AND prosrc LIKE '%corporate_mcp_scope_active%')
 AND EXISTS(SELECT 1 FROM pg_proc WHERE oid=to_regprocedure('public.corporate_ap_can_read(text,text,text)') AND prosrc LIKE '%app.mcp_project%' AND prosrc LIKE '%corporate_mcp_scope_active%')`).Scan(&ok); err != nil || !ok {
		return errAgentProjects
	}
	if err := d.QueryRow(`SELECT count(*) FROM pg_trigger WHERE NOT tgisinternal AND tgenabled IN ('O','A') AND ((tgrelid='public.agent_projects'::regclass AND tgname='agent_projects_guard' AND tgfoid=to_regprocedure('public.corporate_ap_guard()')) OR (tgrelid='public.agent_project_spaces'::regclass AND tgname IN ('agent_project_spaces_guard_update','agent_project_spaces_guard_insert') AND tgfoid=to_regprocedure('public.corporate_aps_guard()')) OR (tgrelid='public.projects'::regclass AND tgname='corporate_projects_removed'))`).Scan(&count); err != nil || count != 4 {
		return errAgentProjects
	}
	if err := d.QueryRow(`SELECT count(*) FROM pg_constraint WHERE conname IN ('articles_no_project','revisions_no_project','artifacts_no_project','memberships_space_only','mcp_credentials_projects_required') AND convalidated`).Scan(&count); err != nil || count != 5 {
		return errAgentProjects
	}
	list := agentProjectOIDs()
	if err := d.QueryRow(`SELECT (SELECT count(*) FROM pg_proc p WHERE p.oid=ANY(ARRAY[` + list + `]) AND p.prosecdef AND p.proconfig @> ARRAY['search_path=pg_catalog, public'])=` + strconv.Itoa(len(agentProjectFunctions)) + `
 AND NOT EXISTS(SELECT 1 FROM pg_proc p, aclexplode(COALESCE(p.proacl,acldefault('f',p.proowner))) a WHERE p.oid=ANY(ARRAY[` + list + `,to_regprocedure('public.corporate_agent_scope_rows(text,text,boolean)'),to_regprocedure('public.corporate_ap_guard()'),to_regprocedure('public.corporate_aps_guard()')]) AND a.grantee=0)
 AND NOT EXISTS(SELECT 1 FROM unnest(ARRAY[` + list + `]) AS f(oid) WHERE f.oid IS NULL OR NOT has_function_privilege(current_user,f.oid,'EXECUTE'))`).Scan(&ok); err != nil || !ok {
		return errAgentProjects
	}
	// The per-request audit must recompute the scope from the project (not merely compare a stored value).
	if err := d.QueryRow(`SELECT EXISTS(SELECT 1 FROM pg_proc WHERE oid=to_regprocedure('public.corporate_mcp_credential_audit(text,text,text)') AND prosrc LIKE '%corporate_mcp_project_scope%' AND prosrc LIKE '%app.mcp_project%' AND prosrc LIKE '%app.mcp_credential%')`).Scan(&ok); err != nil || !ok {
		return errAgentProjects
	}
	return nil
}

func agentProjectOIDs() string {
	out := ""
	for i, f := range agentProjectFunctions {
		if i > 0 {
			out += ","
		}
		out += "to_regprocedure('public." + f + "')"
	}
	return out
}
