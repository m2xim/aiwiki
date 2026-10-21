-- Separate MCP scope layer. Do not change corporate_acl v1: its policies depend
-- on the original corporate_effective_role function OID.
CREATE OR REPLACE FUNCTION corporate_owner_effective_role(who text, space_key text, project_key text)
RETURNS text LANGUAGE sql STABLE SECURITY DEFINER
SET search_path=pg_catalog,public AS $fn$
WITH RECURSIVE live_user AS (
    SELECT u.id FROM public.users u WHERE u.id=who AND u.disabled=0
), project_chain(id,space_id,parent_id,inherit_acl,path,depth) AS (
    SELECT p.id,p.space_id,p.parent_id,p.inherit_acl,ARRAY[p.id]::text[],0
      FROM public.projects p
     WHERE project_key IS NOT NULL AND project_key<>'' AND p.id=project_key AND p.space_id=space_key
    UNION ALL
    SELECT parent.id,parent.space_id,parent.parent_id,parent.inherit_acl,
           chain.path || parent.id,chain.depth+1
      FROM project_chain chain
      JOIN public.projects child ON child.id=chain.id
      JOIN public.projects parent ON parent.id=child.parent_id AND parent.space_id=child.space_id
     WHERE chain.inherit_acl=1 AND chain.parent_id<>'' AND chain.depth<64
       AND NOT parent.id=ANY(chain.path)
), scopes(resource_type,resource_id) AS (
    SELECT 'project'::text,id FROM project_chain
    UNION
    SELECT 'space'::text,space_key
     WHERE space_key IS NOT NULL AND space_key<>''
       AND (project_key IS NULL OR project_key='' OR EXISTS (
            SELECT 1 FROM project_chain terminal WHERE terminal.parent_id='' AND terminal.inherit_acl=1
       ))
), principals(subject_type,subject_id) AS (
    SELECT 'user'::text,live_user.id FROM live_user
    UNION
    SELECT 'team'::text,tm.team_id FROM public.team_members tm
      JOIN public.teams t ON t.id=tm.team_id AND t.archived_at IS NULL
      JOIN live_user lu ON lu.id=tm.user_id
), candidates(role) AS (
    SELECT m.role FROM public.memberships m
      JOIN scopes s ON s.resource_type=m.resource_type AND s.resource_id=m.resource_id
      JOIN principals p ON p.subject_type=m.subject_type AND p.subject_id=m.subject_id
)
SELECT CASE max(public.corporate_role_rank(role)) WHEN 5 THEN 'owner' WHEN 4 THEN 'manager'
       WHEN 3 THEN 'editor' WHEN 2 THEN 'commenter' WHEN 1 THEN 'reader' ELSE NULL END
FROM candidates
$fn$;

CREATE OR REPLACE FUNCTION corporate_effective_role(who text, space_key text, project_key text)
RETURNS text LANGUAGE plpgsql STABLE SECURITY DEFINER
SET search_path=pg_catalog,public AS $fn$
DECLARE scope_text text;
DECLARE scope_json jsonb;
DECLARE base_role text;
BEGIN
    scope_text := current_setting('app.mcp_projects',true);
    IF scope_text IS NULL OR scope_text='' THEN
        RETURN public.corporate_owner_effective_role(who,space_key,project_key);
    END IF;
    IF who IS DISTINCT FROM current_setting('app.user_id',true)
       OR project_key IS NULL OR project_key='' THEN
        RETURN NULL;
    END IF;
    BEGIN
        scope_json := scope_text::jsonb;
    EXCEPTION WHEN invalid_text_representation THEN
        RETURN NULL;
    END;
    IF jsonb_typeof(scope_json)<>'array' THEN
        RETURN NULL;
    END IF;
    IF NOT EXISTS (
        SELECT 1 FROM jsonb_array_elements(scope_json) AS p(value)
         WHERE jsonb_typeof(p.value)='string' AND p.value #>> '{}'=project_key AND project_key<>''
    ) THEN
        RETURN NULL;
    END IF;
    base_role := public.corporate_owner_effective_role(who,space_key,project_key);
    IF public.corporate_role_rank(base_role)>=1 THEN
        RETURN 'reader';
    END IF;
    RETURN NULL;
END
$fn$;

CREATE OR REPLACE FUNCTION corporate_mcp_scope_active() RETURNS boolean
LANGUAGE sql STABLE SET search_path=pg_catalog AS $fn$
    SELECT coalesce(current_setting('app.mcp_projects',true),'')<>''
$fn$;

CREATE OR REPLACE FUNCTION corporate_mcp_request_audit(grant_key text, method_name text, tool_name text)
RETURNS void LANGUAGE plpgsql VOLATILE SECURITY DEFINER
SET search_path=pg_catalog,public AS $fn$
DECLARE request_actor text;
BEGIN
    IF NOT public.corporate_mcp_scope_active() THEN
        RETURN;
    END IF;
    request_actor := current_setting('app.user_id',true);
    IF request_actor IS NULL OR request_actor='' THEN
        RETURN;
    END IF;
    IF grant_key IS NULL OR left(grant_key,4)<>'dlg_' OR char_length(grant_key)>128
       OR method_name IS NULL OR char_length(method_name)>100
       OR tool_name IS NULL OR char_length(tool_name)>100 THEN
        RAISE EXCEPTION 'invalid MCP delegation audit metadata';
    END IF;
    INSERT INTO public.security_audit(actor_id,action,resource_type,resource_id,details,created_at)
    VALUES (
        request_actor,
        'mcp.delegation.request',
        'mcp_delegation',
        grant_key,
        jsonb_build_object('method',method_name,'tool',tool_name),
        to_char(clock_timestamp() AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.MS"Z"')
    );
END
$fn$;

REVOKE ALL ON FUNCTION corporate_owner_effective_role(text,text,text) FROM PUBLIC;
REVOKE ALL ON FUNCTION corporate_effective_role(text,text,text) FROM PUBLIC;
REVOKE ALL ON FUNCTION corporate_mcp_request_audit(text,text,text) FROM PUBLIC;

-- Runtime EXECUTE grants are provisioned by the lead after migration; the base
-- role function remains private to the SECURITY DEFINER wrapper.
-- Metadata deliberately uses NO FORCE: the function/table owner must read ACLs
-- and append constrained audit records while scoped runtime sessions cannot.
ALTER TABLE public.users ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.users NO FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS mcp_scope_users_default ON public.users;
CREATE POLICY mcp_scope_users_default ON public.users AS PERMISSIVE FOR ALL
USING (NOT public.corporate_mcp_scope_active())
WITH CHECK (NOT public.corporate_mcp_scope_active());

ALTER TABLE public.projects ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.projects NO FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS mcp_scope_projects_default ON public.projects;
CREATE POLICY mcp_scope_projects_default ON public.projects AS PERMISSIVE FOR ALL
USING (NOT public.corporate_mcp_scope_active())
WITH CHECK (NOT public.corporate_mcp_scope_active());
DROP POLICY IF EXISTS mcp_scope_projects_read ON public.projects;
CREATE POLICY mcp_scope_projects_read ON public.projects AS PERMISSIVE FOR SELECT
USING (public.corporate_role_rank(public.corporate_effective_role(
    current_setting('app.user_id',true),space_id,id)) >= 1);

ALTER TABLE public.memberships ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.memberships NO FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS mcp_scope_memberships_default ON public.memberships;
CREATE POLICY mcp_scope_memberships_default ON public.memberships AS PERMISSIVE FOR ALL
USING (NOT public.corporate_mcp_scope_active())
WITH CHECK (NOT public.corporate_mcp_scope_active());

ALTER TABLE public.teams ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.teams NO FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS mcp_scope_teams_default ON public.teams;
CREATE POLICY mcp_scope_teams_default ON public.teams AS PERMISSIVE FOR ALL
USING (NOT public.corporate_mcp_scope_active())
WITH CHECK (NOT public.corporate_mcp_scope_active());

ALTER TABLE public.team_members ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.team_members NO FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS mcp_scope_team_members_default ON public.team_members;
CREATE POLICY mcp_scope_team_members_default ON public.team_members AS PERMISSIVE FOR ALL
USING (NOT public.corporate_mcp_scope_active())
WITH CHECK (NOT public.corporate_mcp_scope_active());

ALTER TABLE public.corporate_space_state ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.corporate_space_state NO FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS mcp_scope_corporate_space_state_default ON public.corporate_space_state;
CREATE POLICY mcp_scope_corporate_space_state_default ON public.corporate_space_state AS PERMISSIVE FOR ALL
USING (NOT public.corporate_mcp_scope_active())
WITH CHECK (NOT public.corporate_mcp_scope_active());

ALTER TABLE public.security_audit ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.security_audit NO FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS mcp_scope_security_audit_default ON public.security_audit;
CREATE POLICY mcp_scope_security_audit_default ON public.security_audit AS PERMISSIVE FOR ALL
USING (NOT public.corporate_mcp_scope_active())
WITH CHECK (NOT public.corporate_mcp_scope_active());

DROP POLICY IF EXISTS mcp_scope_spaces_insert ON spaces;
CREATE POLICY mcp_scope_spaces_insert ON spaces AS RESTRICTIVE FOR INSERT WITH CHECK (NOT public.corporate_mcp_scope_active());
DROP POLICY IF EXISTS mcp_scope_spaces_update ON spaces;
CREATE POLICY mcp_scope_spaces_update ON spaces AS RESTRICTIVE FOR UPDATE USING (NOT public.corporate_mcp_scope_active()) WITH CHECK (NOT public.corporate_mcp_scope_active());
DROP POLICY IF EXISTS mcp_scope_spaces_delete ON spaces;
CREATE POLICY mcp_scope_spaces_delete ON spaces AS RESTRICTIVE FOR DELETE USING (NOT public.corporate_mcp_scope_active());

DROP POLICY IF EXISTS mcp_scope_articles_insert ON articles;
CREATE POLICY mcp_scope_articles_insert ON articles AS RESTRICTIVE FOR INSERT WITH CHECK (NOT public.corporate_mcp_scope_active());
DROP POLICY IF EXISTS mcp_scope_articles_update ON articles;
CREATE POLICY mcp_scope_articles_update ON articles AS RESTRICTIVE FOR UPDATE USING (NOT public.corporate_mcp_scope_active()) WITH CHECK (NOT public.corporate_mcp_scope_active());
DROP POLICY IF EXISTS mcp_scope_articles_delete ON articles;
CREATE POLICY mcp_scope_articles_delete ON articles AS RESTRICTIVE FOR DELETE USING (NOT public.corporate_mcp_scope_active());

DROP POLICY IF EXISTS mcp_scope_revisions_insert ON revisions;
CREATE POLICY mcp_scope_revisions_insert ON revisions AS RESTRICTIVE FOR INSERT WITH CHECK (NOT public.corporate_mcp_scope_active());
DROP POLICY IF EXISTS mcp_scope_revisions_update ON revisions;
CREATE POLICY mcp_scope_revisions_update ON revisions AS RESTRICTIVE FOR UPDATE USING (NOT public.corporate_mcp_scope_active()) WITH CHECK (NOT public.corporate_mcp_scope_active());
DROP POLICY IF EXISTS mcp_scope_revisions_delete ON revisions;
CREATE POLICY mcp_scope_revisions_delete ON revisions AS RESTRICTIVE FOR DELETE USING (NOT public.corporate_mcp_scope_active());

DROP POLICY IF EXISTS mcp_scope_comments_insert ON comments;
CREATE POLICY mcp_scope_comments_insert ON comments AS RESTRICTIVE FOR INSERT WITH CHECK (NOT public.corporate_mcp_scope_active());
DROP POLICY IF EXISTS mcp_scope_comments_update ON comments;
CREATE POLICY mcp_scope_comments_update ON comments AS RESTRICTIVE FOR UPDATE USING (NOT public.corporate_mcp_scope_active()) WITH CHECK (NOT public.corporate_mcp_scope_active());
DROP POLICY IF EXISTS mcp_scope_comments_delete ON comments;
CREATE POLICY mcp_scope_comments_delete ON comments AS RESTRICTIVE FOR DELETE USING (NOT public.corporate_mcp_scope_active());

DROP POLICY IF EXISTS mcp_scope_attachments_insert ON attachments;
CREATE POLICY mcp_scope_attachments_insert ON attachments AS RESTRICTIVE FOR INSERT WITH CHECK (NOT public.corporate_mcp_scope_active());
DROP POLICY IF EXISTS mcp_scope_attachments_update ON attachments;
CREATE POLICY mcp_scope_attachments_update ON attachments AS RESTRICTIVE FOR UPDATE USING (NOT public.corporate_mcp_scope_active()) WITH CHECK (NOT public.corporate_mcp_scope_active());
DROP POLICY IF EXISTS mcp_scope_attachments_delete ON attachments;
CREATE POLICY mcp_scope_attachments_delete ON attachments AS RESTRICTIVE FOR DELETE USING (NOT public.corporate_mcp_scope_active());

DROP POLICY IF EXISTS mcp_scope_favorites_insert ON favorites;
CREATE POLICY mcp_scope_favorites_insert ON favorites AS RESTRICTIVE FOR INSERT WITH CHECK (NOT public.corporate_mcp_scope_active());
DROP POLICY IF EXISTS mcp_scope_favorites_update ON favorites;
CREATE POLICY mcp_scope_favorites_update ON favorites AS RESTRICTIVE FOR UPDATE USING (NOT public.corporate_mcp_scope_active()) WITH CHECK (NOT public.corporate_mcp_scope_active());
DROP POLICY IF EXISTS mcp_scope_favorites_delete ON favorites;
CREATE POLICY mcp_scope_favorites_delete ON favorites AS RESTRICTIVE FOR DELETE USING (NOT public.corporate_mcp_scope_active());

DO $policies$
BEGIN
    IF to_regclass('public.article_fts') IS NOT NULL THEN
        EXECUTE 'DROP POLICY IF EXISTS mcp_scope_article_fts_insert ON public.article_fts';
        EXECUTE 'CREATE POLICY mcp_scope_article_fts_insert ON public.article_fts AS RESTRICTIVE FOR INSERT WITH CHECK (NOT public.corporate_mcp_scope_active())';
        EXECUTE 'DROP POLICY IF EXISTS mcp_scope_article_fts_update ON public.article_fts';
        EXECUTE 'CREATE POLICY mcp_scope_article_fts_update ON public.article_fts AS RESTRICTIVE FOR UPDATE USING (NOT public.corporate_mcp_scope_active()) WITH CHECK (NOT public.corporate_mcp_scope_active())';
        EXECUTE 'DROP POLICY IF EXISTS mcp_scope_article_fts_delete ON public.article_fts';
        EXECUTE 'CREATE POLICY mcp_scope_article_fts_delete ON public.article_fts AS RESTRICTIVE FOR DELETE USING (NOT public.corporate_mcp_scope_active())';
    END IF;
END
$policies$;
