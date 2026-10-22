package server

import (
	"errors"
	"sort"
	"strings"
)

// roleScopeBodyMD5 is md5 of corporate_role_scope()'s body (prosrc, CR removed so a
// CRLF checkout of role_scope.sql hashes the same). prosrc is stored verbatim, so the
// value is stable across PostgreSQL versions; change it together with the function.
const roleScopeBodyMD5 = "051ab117ae7b7fd91566e763ddc34adc"

// roleScopePolicies are the SELECT policies that must call corporate_role_scope(), as
// pg_get_expr deparses them with whitespace collapsed (default search_path). A later
// migration that recreates one of them must keep exactly this predicate.
var roleScopePolicies = map[string]string{
	"article_fts/corporate_fts_read":         `(EXISTS ( SELECT 1 FROM articles a WHERE ((a.id = article_fts.article_id) AND ((a.space_id, a.project_id) IN ( SELECT s.space_id, s.project_id FROM corporate_role_scope() s(space_id, project_id, role_rank))))))`,
	"articles/corporate_articles_read":       `((space_id, project_id) IN ( SELECT s.space_id, s.project_id FROM corporate_role_scope() s(space_id, project_id, role_rank)))`,
	"attachments/corporate_attachments_read": `(EXISTS ( SELECT 1 FROM articles a WHERE ((a.id = attachments.article_id) AND ((a.space_id, a.project_id) IN ( SELECT s.space_id, s.project_id FROM corporate_role_scope() s(space_id, project_id, role_rank))))))`,
	"comments/corporate_comments_read":       `(EXISTS ( SELECT 1 FROM articles a WHERE ((a.id = comments.article_id) AND ((a.space_id, a.project_id) IN ( SELECT s.space_id, s.project_id FROM corporate_role_scope() s(space_id, project_id, role_rank))))))`,
	"favorites/corporate_favorites_read":     `((user_id = current_setting('app.user_id'::text, true)) AND (EXISTS ( SELECT 1 FROM articles a WHERE ((a.id = favorites.article_id) AND ((a.space_id, a.project_id) IN ( SELECT s.space_id, s.project_id FROM corporate_role_scope() s(space_id, project_id, role_rank)))))))`,
	"projects/mcp_scope_projects_read":       `((space_id, id) IN ( SELECT s.space_id, s.project_id FROM corporate_role_scope() s(space_id, project_id, role_rank)))`,
	"revisions/corporate_revisions_read":     `((EXISTS ( SELECT 1 FROM articles current_article WHERE ((current_article.id = revisions.article_id) AND ((current_article.space_id, current_article.project_id) IN ( SELECT s.space_id, s.project_id FROM corporate_role_scope() s(space_id, project_id, role_rank)))))) AND ((space_id, project_id) IN ( SELECT s.space_id, s.project_id FROM corporate_role_scope() s(space_id, project_id, role_rank))))`,
	"spaces/corporate_spaces_read":           `((id, ''::text) IN ( SELECT s.space_id, s.project_id FROM corporate_role_scope() s(space_id, project_id, role_rank)))`,
}

// roleScopeReadPolicies is every permissive policy that can grant SELECT on those
// tables (command r or *). Policies are ORed, so an extra one would bypass the scope.
var roleScopeReadPolicies = []string{
	"article_fts/corporate_fts_read",
	"articles/corporate_articles_mismatch_probe",
	"articles/corporate_articles_project_move_read",
	"articles/corporate_articles_read",
	"articles/storage_quota_probe_articles",
	"attachments/corporate_attachments_read",
	"attachments/storage_quota_probe_attachments",
	"comments/corporate_comments_read",
	"favorites/corporate_favorites_read",
	"projects/mcp_scope_projects_default",
	"projects/mcp_scope_projects_read",
	"revisions/corporate_revisions_read",
	"spaces/corporate_spaces_read",
	"spaces/storage_quota_probe_spaces",
}

var errRoleScope = errors.New("role scope function or read policies are missing, altered, not SECURITY DEFINER or executable by PUBLIC")

func (d *database) validateRoleScope() error {
	var ok bool
	var bodyMD5 string
	if err := d.QueryRow(`SELECT coalesce((SELECT p.prosecdef AND p.proconfig @> ARRAY['search_path=pg_catalog, public, pg_temp'] AND NOT pg_has_role(current_user,p.proowner,'MEMBER') AND has_function_privilege(current_user,p.oid,'EXECUTE') AND NOT EXISTS(SELECT 1 FROM aclexplode(COALESCE(p.proacl,acldefault('f',p.proowner))) a WHERE a.grantee=0) FROM pg_proc p WHERE p.oid=to_regprocedure('public.corporate_role_scope()')),false), coalesce((SELECT md5(replace(prosrc,E'\r','')) FROM pg_proc WHERE oid=to_regprocedure('public.corporate_role_scope()')),'')`).Scan(&ok, &bodyMD5); err != nil || !ok || bodyMD5 != roleScopeBodyMD5 {
		return errRoleScope
	}
	rows, err := d.Query(`SELECT c.relname||'/'||p.polname, p.polpermissive, p.polcmd::text, regexp_replace(coalesce(pg_get_expr(p.polqual,p.polrelid),''),'\s+',' ','g') FROM pg_policy p JOIN pg_class c ON c.oid=p.polrelid JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='public' AND c.relname IN ('articles','revisions','article_fts','attachments','comments','favorites','projects','spaces') AND p.polcmd IN ('r','*') AND p.polpermissive`)
	if err != nil {
		return errRoleScope
	}
	defer rows.Close()
	var found []string
	for rows.Next() {
		var name, cmd, expr string
		var permissive bool
		if err := rows.Scan(&name, &permissive, &cmd, &expr); err != nil {
			return errRoleScope
		}
		found = append(found, name)
		if want, ok := roleScopePolicies[name]; ok && (cmd != "r" || expr != want) {
			return errRoleScope
		}
	}
	if rows.Err() != nil {
		return errRoleScope
	}
	sort.Strings(found)
	if strings.Join(found, ",") != strings.Join(roleScopeReadPolicies, ",") {
		return errRoleScope
	}
	return nil
}
