-- DROP/CREATE POLICY below take ACCESS EXCLUSIVE on 8 tables; the owner session default wait is 5 s.
SET LOCAL lock_timeout = '60s';

-- Read permission as a set: corporate_role_scope() returns every (space, project)
-- the current user may read, with the effective role rank, computed once per
-- statement in one pass over memberships, teams and the project tree. Policies
-- test "(space, project) IN (that set)": an uncorrelated subquery, so PostgreSQL
-- evaluates it once and hashes it, instead of calling corporate_effective_role
-- (a recursive CTE, ~0.6 ms) for every row. corporate_effective_role stays the
-- reference definition; the equivalence test compares the two.
CREATE OR REPLACE FUNCTION public.corporate_role_scope()
RETURNS TABLE(space_id text, project_id text, role_rank integer)
LANGUAGE plpgsql STABLE SECURITY DEFINER
SET search_path = pg_catalog, public, pg_temp
AS $fn$
-- app.user_id, app.mcp_projects and app.mcp_mode are read once per statement (they do not
-- change inside a statement); the per-row reference read them for every row.
DECLARE scope_text text; scope_json jsonb; mode_text text;
BEGIN
 scope_text := current_setting('app.mcp_projects', true);
 mode_text := current_setting('app.mcp_mode', true);
 IF scope_text IS NOT NULL AND scope_text <> '' THEN
   IF mode_text IS NULL OR mode_text NOT IN ('read','write') THEN RETURN; END IF;
   BEGIN scope_json := scope_text::jsonb;
   EXCEPTION WHEN invalid_text_representation THEN RETURN; END;
   IF jsonb_typeof(scope_json) <> 'array' THEN RETURN; END IF;
 END IF;
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
 scopes(start_id, start_space, resource_type, resource_id) AS (
   SELECT c.start_id, c.start_space, 'project'::text, c.id FROM chain c
   UNION
   SELECT c.start_id, c.start_space, 'space'::text, c.start_space FROM chain c
    WHERE c.parent_id = '' AND c.inherit_acl = 1 AND c.start_space IS NOT NULL AND c.start_space <> ''),
 base(space_id, project_id, rank) AS (
   SELECT g.resource_id, ''::text, g.rank FROM grants g
    WHERE g.resource_type = 'space' AND g.resource_id IS NOT NULL AND g.resource_id <> ''
   UNION ALL
   SELECT s.start_space, s.start_id, max(g.rank) FROM scopes s
     JOIN grants g ON g.resource_type = s.resource_type AND g.resource_id = s.resource_id
    GROUP BY s.start_space, s.start_id)
 SELECT b.space_id, b.project_id,
        CASE WHEN scope_json IS NULL THEN b.rank
             WHEN mode_text = 'write' AND b.rank >= 3 THEN 3
             WHEN mode_text = 'write' AND b.rank = 2 THEN 2
             WHEN b.rank >= 1 THEN 1 END
   FROM base b
  WHERE b.rank >= 1
    AND (scope_json IS NULL OR (b.project_id <> '' AND EXISTS (
         SELECT 1 FROM jsonb_array_elements(scope_json) AS e(value)
          WHERE jsonb_typeof(e.value) = 'string' AND e.value #>> '{}' = b.project_id)));
END
$fn$;

DROP POLICY IF EXISTS corporate_articles_read ON public.articles;
CREATE POLICY corporate_articles_read ON public.articles FOR SELECT USING (
  (space_id, project_id) IN (SELECT s.space_id, s.project_id FROM public.corporate_role_scope() s));

-- The explicit check inside EXISTS stays: other permissive SELECT policies on articles
-- (move and probe modes) must not widen what revisions show.
DROP POLICY IF EXISTS corporate_revisions_read ON public.revisions;
CREATE POLICY corporate_revisions_read ON public.revisions FOR SELECT USING (
  EXISTS (SELECT 1 FROM public.articles current_article
    WHERE current_article.id = revisions.article_id
      AND (current_article.space_id, current_article.project_id) IN (SELECT s.space_id, s.project_id FROM public.corporate_role_scope() s))
  AND (revisions.space_id, revisions.project_id) IN (SELECT s.space_id, s.project_id FROM public.corporate_role_scope() s));

DROP POLICY IF EXISTS corporate_fts_read ON public.article_fts;
CREATE POLICY corporate_fts_read ON public.article_fts FOR SELECT USING (
  EXISTS (SELECT 1 FROM public.articles a
    WHERE a.id = article_fts.article_id AND (a.space_id, a.project_id) IN (SELECT s.space_id, s.project_id FROM public.corporate_role_scope() s)));

DROP POLICY IF EXISTS corporate_attachments_read ON public.attachments;
CREATE POLICY corporate_attachments_read ON public.attachments FOR SELECT USING (
  EXISTS (SELECT 1 FROM public.articles a
    WHERE a.id = attachments.article_id AND (a.space_id, a.project_id) IN (SELECT s.space_id, s.project_id FROM public.corporate_role_scope() s)));

DROP POLICY IF EXISTS corporate_comments_read ON public.comments;
CREATE POLICY corporate_comments_read ON public.comments FOR SELECT USING (
  EXISTS (SELECT 1 FROM public.articles a
    WHERE a.id = comments.article_id AND (a.space_id, a.project_id) IN (SELECT s.space_id, s.project_id FROM public.corporate_role_scope() s)));

DROP POLICY IF EXISTS corporate_favorites_read ON public.favorites;
CREATE POLICY corporate_favorites_read ON public.favorites FOR SELECT USING (
  user_id = current_setting('app.user_id', true)
  AND EXISTS (SELECT 1 FROM public.articles a
    WHERE a.id = favorites.article_id AND (a.space_id, a.project_id) IN (SELECT s.space_id, s.project_id FROM public.corporate_role_scope() s)));

DROP POLICY IF EXISTS mcp_scope_projects_read ON public.projects;
CREATE POLICY mcp_scope_projects_read ON public.projects FOR SELECT USING (
  (space_id, id) IN (SELECT s.space_id, s.project_id FROM public.corporate_role_scope() s));

DROP POLICY IF EXISTS corporate_spaces_read ON public.spaces;
CREATE POLICY corporate_spaces_read ON public.spaces FOR SELECT USING (
  (id, ''::text) IN (SELECT s.space_id, s.project_id FROM public.corporate_role_scope() s));

-- New functions are executable by PUBLIC by default; the runtime role gets EXECUTE
-- from the grant that runs at every start (GRANT EXECUTE ON ALL FUNCTIONS).
REVOKE ALL ON FUNCTION public.corporate_role_scope() FROM PUBLIC;
