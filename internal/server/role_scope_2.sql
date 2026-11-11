-- Configurable "general" space: a shared rules layer.
--  * every live user has an implicit reader role on the whole general space (root and
--    all its projects), on top of explicit memberships (max of ranks);
--  * MCP credentials restricted to exact projects additionally always see the general
--    space, with the same ceiling as listed projects (read => reader; write => editor /
--    commenter only if the owner's own rank there is >=3 / =2).
-- The general space id is app_settings key general_space_id; missing or empty means 'general'.
CREATE OR REPLACE FUNCTION public.corporate_general_space()
RETURNS text LANGUAGE sql STABLE SECURITY DEFINER
SET search_path=pg_catalog,public AS $fn$
 SELECT coalesce(nullif((SELECT value FROM public.app_settings WHERE key='general_space_id'),''),'general')
$fn$;

-- Existence check for the administrator settings endpoint: an administrator is not a
-- member of every space, so RLS would hide the space they want to designate.
-- corporate_space_state (kept in sync with spaces by trigger) is not under forced RLS.
CREATE OR REPLACE FUNCTION public.corporate_space_assignable(space_key text)
RETURNS boolean LANGUAGE sql STABLE SECURITY DEFINER
SET search_path=pg_catalog,public AS $fn$
 SELECT public.artifact_is_admin(current_setting('app.user_id',true)) AND NOT public.corporate_mcp_scope_active()
    AND EXISTS (SELECT 1 FROM public.corporate_space_state st WHERE st.space_id=space_key AND NOT st.archived)
$fn$;

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
    UNION ALL
    SELECT 'reader'::text FROM live_user
     WHERE space_key IS NOT NULL AND space_key<>'' AND space_key=public.corporate_general_space()
       AND (project_key IS NULL OR project_key='' OR EXISTS (SELECT 1 FROM project_chain WHERE depth=0))
)
SELECT CASE max(public.corporate_role_rank(role)) WHEN 5 THEN 'owner' WHEN 4 THEN 'manager'
       WHEN 3 THEN 'editor' WHEN 2 THEN 'commenter' WHEN 1 THEN 'reader' ELSE NULL END
FROM candidates
$fn$;

-- Effective mode of a row under a key scope (0 none, 1 read, 2 write): the highest mode
-- of every matching element. A space element matches the root and every project of the
-- space; a project element matches that project and all its descendants (by parent_id,
-- regardless of inherit_acl) in the same space, never the space root. The general space
-- always matches with read.
CREATE OR REPLACE FUNCTION public.corporate_mcp_row_mode(scope jsonb, space_key text, project_key text)
RETURNS integer LANGUAGE sql STABLE SECURITY DEFINER
SET search_path=pg_catalog,public AS $fn$
WITH RECURSIVE elems(t,i,m) AS (
    SELECT e.value->>'type', e.value->>'id', e.value->>'mode'
      FROM jsonb_array_elements(scope) AS e(value)
     WHERE jsonb_typeof(e.value)='object' AND jsonb_typeof(e.value->'id')='string'
       AND e.value->>'type' IN ('space','project') AND e.value->>'mode' IN ('read','write')
), anc(id,parent_id,depth,path) AS (
    SELECT p.id,p.parent_id,0,ARRAY[p.id]::text[]
      FROM public.projects p
     WHERE project_key IS NOT NULL AND project_key<>'' AND p.id=project_key AND p.space_id=space_key
    UNION ALL
    SELECT par.id,par.parent_id,a.depth+1,a.path || par.id
      FROM anc a JOIN public.projects par ON par.id=a.parent_id AND par.space_id=space_key
     WHERE a.parent_id<>'' AND a.depth<64 AND NOT par.id=ANY(a.path)
)
SELECT greatest(
    coalesce((SELECT max(CASE e.m WHEN 'write' THEN 2 ELSE 1 END) FROM elems e
               WHERE (e.t='space' AND space_key IS NOT NULL AND space_key<>'' AND e.i=space_key)
                  OR (e.t='project' AND e.i IN (SELECT id FROM anc))),0),
    CASE WHEN space_key IS NOT NULL AND space_key<>'' AND space_key=public.corporate_general_space() THEN 1 ELSE 0 END)
$fn$;

CREATE OR REPLACE FUNCTION corporate_effective_role(who text, space_key text, project_key text)
RETURNS text LANGUAGE plpgsql STABLE SECURITY DEFINER
SET search_path=pg_catalog,public AS $fn$
DECLARE scope_json jsonb; base_role text; row_mode integer;
BEGIN
 IF NOT public.corporate_mcp_scope_active() THEN
  RETURN public.corporate_owner_effective_role(who,space_key,project_key);
 END IF;
 IF who IS DISTINCT FROM current_setting('app.user_id',true) THEN RETURN NULL; END IF;
 scope_json := public.corporate_mcp_scope_json();
 IF scope_json IS NULL THEN RETURN NULL; END IF;
 row_mode := public.corporate_mcp_row_mode(scope_json,space_key,project_key);
 IF row_mode=0 THEN RETURN NULL; END IF;
 base_role := public.corporate_owner_effective_role(who,space_key,project_key);
 IF row_mode=2 AND public.corporate_role_rank(base_role)>=3 THEN RETURN 'editor'; END IF;
 IF row_mode=2 AND public.corporate_role_rank(base_role)=2 THEN RETURN 'commenter'; END IF;
 IF public.corporate_role_rank(base_role)>=1 THEN RETURN 'reader'; END IF;
 RETURN NULL;
END
$fn$;

CREATE OR REPLACE FUNCTION public.corporate_role_scope()
RETURNS TABLE(space_id text, project_id text, role_rank integer)
LANGUAGE plpgsql STABLE SECURITY DEFINER
SET search_path = pg_catalog, public, pg_temp
AS $fn$
-- app.user_id, app.mcp_scope and the general space are read once per
-- statement (they do not change inside a statement); the per-row reference reads them for every row.
DECLARE scope_json jsonb; general_key text;
BEGIN
 IF public.corporate_mcp_scope_active() THEN
   scope_json := public.corporate_mcp_scope_json();
   IF scope_json IS NULL THEN RETURN; END IF;
 END IF;
 general_key := public.corporate_general_space();
 RETURN QUERY
 WITH RECURSIVE
 live_user AS (SELECT u.id FROM public.users u WHERE u.id = current_setting('app.user_id', true) AND u.disabled = 0),
 principals(subject_type, subject_id) AS (
   SELECT 'user'::text, live_user.id FROM live_user
   UNION
   SELECT 'team'::text, tm.team_id FROM public.team_members tm
     JOIN public.teams t ON t.id = tm.team_id AND t.archived_at IS NULL
     JOIN live_user lu ON lu.id = tm.user_id),
 grants(resource_type, resource_id, rank) AS (
   SELECT m.resource_type, m.resource_id, max(public.corporate_role_rank(m.role))
     FROM public.memberships m
     JOIN principals p ON p.subject_type = m.subject_type AND p.subject_id = m.subject_id
    GROUP BY m.resource_type, m.resource_id),
 chain(start_id, start_space, id, parent_id, inherit_acl, path, depth) AS (
   SELECT p.id, p.space_id, p.id, p.parent_id, p.inherit_acl, ARRAY[p.id]::text[], 0 FROM public.projects p WHERE p.id <> ''
   UNION ALL
   SELECT c.start_id, c.start_space, parent.id, parent.parent_id, parent.inherit_acl, c.path || parent.id, c.depth + 1
     FROM chain c
     JOIN public.projects child ON child.id = c.id
     JOIN public.projects parent ON parent.id = child.parent_id AND parent.space_id = child.space_id
    WHERE c.inherit_acl = 1 AND c.parent_id <> '' AND c.depth < 64 AND NOT parent.id = ANY(c.path)),
 elems(t, i, m) AS (
   SELECT e.value->>'type', e.value->>'id', e.value->>'mode'
     FROM jsonb_array_elements(coalesce(scope_json, '[]'::jsonb)) AS e(value)
    WHERE jsonb_typeof(e.value) = 'object' AND jsonb_typeof(e.value->'id') = 'string'
      AND e.value->>'type' IN ('space','project') AND e.value->>'mode' IN ('read','write')),
 anc(start_id, start_space, id, parent_id, path, depth) AS (
   SELECT p.id, p.space_id, p.id, p.parent_id, ARRAY[p.id]::text[], 0 FROM public.projects p
    WHERE scope_json IS NOT NULL AND p.id <> ''
   UNION ALL
   SELECT a.start_id, a.start_space, par.id, par.parent_id, a.path || par.id, a.depth + 1
     FROM anc a JOIN public.projects par ON par.id = a.parent_id AND par.space_id = a.start_space
    WHERE a.parent_id <> '' AND a.depth < 64 AND NOT par.id = ANY(a.path)),
 scopes(start_id, start_space, resource_type, resource_id) AS (
   SELECT c.start_id, c.start_space, 'project'::text, c.id FROM chain c
   UNION
   SELECT c.start_id, c.start_space, 'space'::text, c.start_space FROM chain c
    WHERE c.parent_id = '' AND c.inherit_acl = 1 AND c.start_space IS NOT NULL AND c.start_space <> ''),
 base0(space_id, project_id, rank) AS (
   SELECT g.resource_id, ''::text, g.rank FROM grants g
    WHERE g.resource_type = 'space' AND g.resource_id IS NOT NULL AND g.resource_id <> ''
   UNION ALL
   SELECT s.start_space, s.start_id, max(g.rank) FROM scopes s
     JOIN grants g ON g.resource_type = s.resource_type AND g.resource_id = s.resource_id
    GROUP BY s.start_space, s.start_id
   UNION ALL
   SELECT general_key, ''::text, 1 FROM live_user WHERE general_key <> ''
   UNION ALL
   SELECT p.space_id, p.id, 1 FROM public.projects p, live_user
    WHERE p.id <> '' AND p.space_id = general_key AND general_key <> ''),
 base(space_id, project_id, rank) AS (
   SELECT b0.space_id, b0.project_id, max(b0.rank) FROM base0 b0 GROUP BY b0.space_id, b0.project_id),
 moded(space_id, project_id, rank, mrank) AS (
   SELECT b.space_id, b.project_id, b.rank,
          greatest(
            coalesce((SELECT max(CASE e.m WHEN 'write' THEN 2 ELSE 1 END) FROM elems e
                       WHERE (e.t = 'space' AND e.i = b.space_id)
                          OR (e.t = 'project' AND b.project_id <> '' AND EXISTS (
                                SELECT 1 FROM anc a WHERE a.start_id = b.project_id AND a.start_space = b.space_id AND a.id = e.i))), 0),
            CASE WHEN b.space_id = general_key THEN 1 ELSE 0 END)
     FROM base b WHERE b.rank >= 1)
 SELECT m.space_id, m.project_id,
        CASE WHEN scope_json IS NULL THEN m.rank
             WHEN m.mrank = 2 AND m.rank >= 3 THEN 3
             WHEN m.mrank = 2 AND m.rank = 2 THEN 2
             ELSE 1 END
   FROM moded m
  WHERE scope_json IS NULL OR m.mrank >= 1;
END
$fn$;

REVOKE ALL ON FUNCTION public.corporate_general_space() FROM PUBLIC;
REVOKE ALL ON FUNCTION public.corporate_mcp_row_mode(jsonb,text,text) FROM PUBLIC;
REVOKE ALL ON FUNCTION public.corporate_space_assignable(text) FROM PUBLIC;
REVOKE ALL ON FUNCTION corporate_owner_effective_role(text,text,text) FROM PUBLIC;
REVOKE ALL ON FUNCTION corporate_effective_role(text,text,text) FROM PUBLIC;
REVOKE ALL ON FUNCTION public.corporate_role_scope() FROM PUBLIC;
