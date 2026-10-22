-- Safe moves, corporate_moves v1. Append-only migration; corporate_acl v1 is not edited.
-- A move is permitted only in the transaction that performs it: the server sets
-- the transaction-local settings app.move ('article' | 'project') and
-- app.move_root, and the triggers below re-check the caller's roles on the old
-- and the new container. The settings are a switch, not a credential: the role
-- checks in the triggers are what authorizes.

CREATE OR REPLACE FUNCTION corporate_role_name(rank_value integer) RETURNS text
LANGUAGE sql IMMUTABLE PARALLEL SAFE SET search_path=pg_catalog,public AS $fn$
    SELECT CASE rank_value WHEN 5 THEN 'owner' WHEN 4 THEN 'manager' WHEN 3 THEN 'editor'
      WHEN 2 THEN 'commenter' WHEN 1 THEN 'reader' ELSE NULL END
$fn$;

-- Effective-grant diff of a prospective move. It reuses the recursive chain of
-- corporate_effective_role (project chain while inherit_acl=1, then the space
-- only when the chain ends at a root project with inheritance) for the old and
-- the new position, so it matches RLS. Rows:
--   'principal': a user or a team whose effective role on the moved content
--                changes (strongest change over the moved containers);
--   'user':      the same after expanding teams into active users.
-- Only changes are returned, never complete grant lists. The caller must be the
-- current app.user_id and hold the roles the move rules demand, otherwise the
-- function raises; MCP-scoped sessions get at most "reader" from
-- corporate_effective_role and are therefore refused.
CREATE OR REPLACE FUNCTION corporate_move_impact(p_kind text, p_id text, p_target_space text, p_target_container text)
RETURNS TABLE(r_kind text, r_subject_type text, r_subject_id text, r_name text, r_before text, r_after text, r_users integer)
LANGUAGE plpgsql STABLE SECURITY DEFINER
SET search_path=pg_catalog,public AS $fn$
#variable_conflict use_column
DECLARE
    actor text := nullif(current_setting('app.user_id',true),'');
    src_space text;
    src_project text;
    root_space text;
BEGIN
    IF actor IS NULL OR p_id IS NULL OR p_id='' OR p_target_space IS NULL OR p_target_space=''
       OR p_target_container IS NULL THEN
        RAISE EXCEPTION 'access denied' USING ERRCODE='42501';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM public.spaces s WHERE s.id=p_target_space) THEN
        RAISE EXCEPTION 'target not found' USING ERRCODE='42501';
    END IF;
    IF p_target_container<>'' AND NOT EXISTS (
        SELECT 1 FROM public.projects p WHERE p.id=p_target_container AND p.space_id=p_target_space) THEN
        RAISE EXCEPTION 'target not found' USING ERRCODE='42501';
    END IF;
    IF p_kind='article' THEN
        SELECT a.space_id,a.project_id INTO src_space,src_project FROM public.articles a WHERE a.id=p_id;
        IF NOT FOUND THEN
            RAISE EXCEPTION 'article not found' USING ERRCODE='42501';
        END IF;
        IF public.corporate_role_rank(public.corporate_effective_role(actor,src_space,src_project))<4
           OR public.corporate_role_rank(public.corporate_effective_role(actor,p_target_space,p_target_container))<4 THEN
            RAISE EXCEPTION 'access denied' USING ERRCODE='42501';
        END IF;
    ELSIF p_kind='project' THEN
        SELECT p.space_id INTO root_space FROM public.projects p WHERE p.id=p_id;
        IF NOT FOUND THEN
            RAISE EXCEPTION 'project not found' USING ERRCODE='42501';
        END IF;
        IF public.corporate_role_rank(public.corporate_effective_role(actor,root_space,p_id))<5
           OR public.corporate_role_rank(public.corporate_effective_role(actor,p_target_space,p_target_container))<4 THEN
            RAISE EXCEPTION 'access denied' USING ERRCODE='42501';
        END IF;
        IF p_target_container<>'' AND EXISTS (
            WITH RECURSIVE down(id,path) AS (
                SELECT p_id,ARRAY[p_id]::text[]
                UNION ALL
                SELECT c.id,down.path||c.id FROM public.projects c JOIN down ON c.parent_id=down.id
                 WHERE NOT c.id=ANY(down.path) AND cardinality(down.path)<64
            ) SELECT 1 FROM down WHERE down.id=p_target_container) THEN
            RAISE EXCEPTION 'project cycle' USING ERRCODE='42501';
        END IF;
        IF EXISTS (
            WITH RECURSIVE down(id,path) AS (
                SELECT p_id,ARRAY[p_id]::text[]
                UNION ALL
                SELECT c.id,down.path||c.id FROM public.projects c JOIN down ON c.parent_id=down.id
                 WHERE NOT c.id=ANY(down.path) AND cardinality(down.path)<64
            ) SELECT 1 FROM down JOIN public.projects p ON p.id=down.id
               WHERE public.corporate_role_rank(public.corporate_effective_role(actor,p.space_id,p.id))<4) THEN
            RAISE EXCEPTION 'access denied' USING ERRCODE='42501';
        END IF;
    ELSE
        RAISE EXCEPTION 'invalid move kind' USING ERRCODE='42501';
    END IF;

    RETURN QUERY
    WITH RECURSIVE sub(id,path) AS (
        SELECT p_id,ARRAY[p_id]::text[] WHERE p_kind='project'
        UNION ALL
        SELECT c.id,sub.path||c.id FROM public.projects c JOIN sub ON c.parent_id=sub.id
         WHERE NOT c.id=ANY(sub.path) AND cardinality(sub.path)<64
    ), subset(id) AS (
        SELECT DISTINCT sub.id FROM sub
    ), pview(side,id,space_id,parent_id,inherit_acl) AS (
        SELECT 'b'::text,p.id,p.space_id,p.parent_id,p.inherit_acl FROM public.projects p
        UNION ALL
        SELECT 'a'::text,p.id,
               CASE WHEN p.id IN (SELECT id FROM subset) THEN p_target_space ELSE p.space_id END,
               CASE WHEN p_kind='project' AND p.id=p_id THEN p_target_container ELSE p.parent_id END,
               p.inherit_acl
          FROM public.projects p
    ), starts(key,side,space_key,project_key) AS (
        SELECT 'x'::text,'b'::text,src_space,src_project WHERE p_kind='article'
        UNION ALL
        SELECT 'x'::text,'a'::text,p_target_space,p_target_container WHERE p_kind='article'
        UNION ALL
        SELECT p.id,'b'::text,p.space_id,p.id FROM public.projects p WHERE p.id IN (SELECT id FROM subset)
        UNION ALL
        SELECT p.id,'a'::text,p_target_space,p.id FROM public.projects p WHERE p.id IN (SELECT id FROM subset)
    ), chain(key,side,id,space_id,parent_id,inherit_acl,path,depth) AS (
        SELECT s.key,s.side,v.id,v.space_id,v.parent_id,v.inherit_acl,ARRAY[v.id]::text[],0
          FROM starts s JOIN pview v ON v.side=s.side AND v.id=s.project_key AND v.space_id=s.space_key
         WHERE s.project_key<>''
        UNION ALL
        SELECT c.key,c.side,par.id,par.space_id,par.parent_id,par.inherit_acl,c.path||par.id,c.depth+1
          FROM chain c
          JOIN pview par ON par.side=c.side AND par.id=c.parent_id AND par.space_id=c.space_id
         WHERE c.inherit_acl=1 AND c.parent_id<>'' AND c.depth<64 AND NOT par.id=ANY(c.path)
    ), scopes(key,side,resource_type,resource_id) AS (
        SELECT chain.key,chain.side,'project'::text,chain.id FROM chain
        UNION
        SELECT s.key,s.side,'space'::text,s.space_key FROM starts s
         WHERE s.space_key<>'' AND (s.project_key='' OR EXISTS (
               SELECT 1 FROM chain t WHERE t.key=s.key AND t.side=s.side AND t.parent_id='' AND t.inherit_acl=1))
    ), pgr(key,side,subject_type,subject_id,rk) AS (
        SELECT sc.key,sc.side,m.subject_type,m.subject_id,max(public.corporate_role_rank(m.role))::integer
          FROM public.memberships m
          JOIN scopes sc ON sc.resource_type=m.resource_type AND sc.resource_id=m.resource_id
         WHERE (m.subject_type='user' AND EXISTS (SELECT 1 FROM public.users u WHERE u.id=m.subject_id AND u.disabled=0))
            OR (m.subject_type='team' AND EXISTS (SELECT 1 FROM public.teams t WHERE t.id=m.subject_id AND t.archived_at IS NULL))
         GROUP BY sc.key,sc.side,m.subject_type,m.subject_id
    ), ug(key,side,user_id,rk) AS (
        SELECT pgr.key,pgr.side,pgr.subject_id,pgr.rk FROM pgr WHERE pgr.subject_type='user'
        UNION ALL
        SELECT pgr.key,pgr.side,tm.user_id,pgr.rk
          FROM pgr
          JOIN public.team_members tm ON pgr.subject_type='team' AND tm.team_id=pgr.subject_id
          JOIN public.users u ON u.id=tm.user_id AND u.disabled=0
    ), ugm(key,side,user_id,rk) AS (
        SELECT ug.key,ug.side,ug.user_id,max(ug.rk)::integer FROM ug GROUP BY ug.key,ug.side,ug.user_id
    ), pdiff AS (
        SELECT DISTINCT ON (x.subject_type,x.subject_id) x.subject_type,x.subject_id,x.rb,x.ra
          FROM (
            SELECT coalesce(b.key,a.key) AS key,
                   coalesce(b.subject_type,a.subject_type) AS subject_type,
                   coalesce(b.subject_id,a.subject_id) AS subject_id,
                   coalesce(b.rk,0) AS rb, coalesce(a.rk,0) AS ra
              FROM (SELECT * FROM pgr WHERE side='b') b
              FULL JOIN (SELECT * FROM pgr WHERE side='a') a
                ON a.key=b.key AND a.subject_type=b.subject_type AND a.subject_id=b.subject_id
          ) x
         WHERE x.rb<>x.ra
         ORDER BY x.subject_type,x.subject_id,abs(x.ra-x.rb) DESC,x.key
    ), udiff AS (
        SELECT DISTINCT ON (x.user_id) x.user_id,x.rb,x.ra
          FROM (
            SELECT coalesce(b.key,a.key) AS key,
                   coalesce(b.user_id,a.user_id) AS user_id,
                   coalesce(b.rk,0) AS rb, coalesce(a.rk,0) AS ra
              FROM (SELECT * FROM ugm WHERE side='b') b
              FULL JOIN (SELECT * FROM ugm WHERE side='a') a
                ON a.key=b.key AND a.user_id=b.user_id
          ) x
         WHERE x.rb<>x.ra
         ORDER BY x.user_id,abs(x.ra-x.rb) DESC,x.key
    )
    SELECT 'principal'::text, d.subject_type, d.subject_id,
           coalesce(CASE d.subject_type WHEN 'user' THEN (SELECT u.name FROM public.users u WHERE u.id=d.subject_id)
                                        ELSE (SELECT t.name FROM public.teams t WHERE t.id=d.subject_id) END,''),
           public.corporate_role_name(d.rb), public.corporate_role_name(d.ra),
           CASE d.subject_type WHEN 'team' THEN
               (SELECT count(*)::integer FROM public.team_members tm
                  JOIN public.users u ON u.id=tm.user_id AND u.disabled=0 WHERE tm.team_id=d.subject_id)
             ELSE 1 END
      FROM pdiff d
    UNION ALL
    SELECT 'user'::text,'user'::text,d.user_id,''::text,
           public.corporate_role_name(d.rb), public.corporate_role_name(d.ra), 1
      FROM udiff d;
END
$fn$;

CREATE OR REPLACE FUNCTION corporate_reject_article_move() RETURNS trigger
LANGUAGE plpgsql SET search_path=pg_catalog,public AS $fn$
DECLARE
    actor text := nullif(current_setting('app.user_id',true),'');
    move_mode text := coalesce(current_setting('app.move',true),'');
BEGIN
    IF NEW.space_id IS NOT DISTINCT FROM OLD.space_id
       AND NEW.project_id IS NOT DISTINCT FROM OLD.project_id THEN
        IF NEW.parent_id IS NOT DISTINCT FROM OLD.parent_id THEN
            RETURN NEW;
        END IF;
        -- Parent change inside one container: editor+ as for any edit.
        IF actor IS NOT NULL AND public.corporate_role_rank(
               public.corporate_effective_role(actor,OLD.space_id,OLD.project_id))>=3 THEN
            RETURN NEW;
        END IF;
        RAISE EXCEPTION 'article parent change requires editor access' USING ERRCODE='42501';
    END IF;
    IF actor IS NULL THEN
        RAISE EXCEPTION 'article moves require an access-impact preview' USING ERRCODE='42501';
    END IF;
    IF move_mode='article' THEN
        IF public.corporate_role_rank(public.corporate_effective_role(actor,OLD.space_id,OLD.project_id))>=4
           AND public.corporate_role_rank(public.corporate_effective_role(actor,NEW.space_id,NEW.project_id))>=4 THEN
            RETURN NEW;
        END IF;
    ELSIF move_mode='project' THEN
        -- Articles follow a project that was already moved by this transaction:
        -- same project, same parent, and the project now lives in the new space.
        IF NEW.project_id<>'' AND NEW.project_id=OLD.project_id
           AND NEW.parent_id IS NOT DISTINCT FROM OLD.parent_id
           AND NEW.space_id IS DISTINCT FROM OLD.space_id
           AND coalesce(current_setting('app.move_root',true),'')<>''
           AND EXISTS (SELECT 1 FROM public.projects p WHERE p.id=NEW.project_id AND p.space_id=NEW.space_id) THEN
            RETURN NEW;
        END IF;
    END IF;
    RAISE EXCEPTION 'article moves require an access-impact preview' USING ERRCODE='42501';
END
$fn$;

CREATE OR REPLACE FUNCTION corporate_guard_project_move() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,public AS $fn$
DECLARE
    actor text := nullif(current_setting('app.user_id',true),'');
    move_mode text := coalesce(current_setting('app.move',true),'');
    move_root text := coalesce(current_setting('app.move_root',true),'');
    parent_space text;
    walker text;
    steps integer := 0;
BEGIN
    IF NEW.parent_id IS NOT DISTINCT FROM OLD.parent_id AND NEW.space_id IS NOT DISTINCT FROM OLD.space_id THEN
        RETURN NEW;
    END IF;
    IF actor IS NULL OR move_mode<>'project' OR move_root='' THEN
        RAISE EXCEPTION 'project moves require an access-impact preview' USING ERRCODE='42501';
    END IF;
    IF NEW.id=move_root THEN
        IF public.corporate_role_rank(public.corporate_effective_role(actor,OLD.space_id,OLD.id))<5 THEN
            RAISE EXCEPTION 'moving a project requires the owner role' USING ERRCODE='42501';
        END IF;
        IF NOT EXISTS (SELECT 1 FROM public.spaces s WHERE s.id=NEW.space_id) THEN
            RAISE EXCEPTION 'target space not found' USING ERRCODE='42501';
        END IF;
        IF NEW.parent_id<>'' THEN
            SELECT p.space_id INTO parent_space FROM public.projects p WHERE p.id=NEW.parent_id;
            IF NOT FOUND OR parent_space<>NEW.space_id THEN
                RAISE EXCEPTION 'target parent not found' USING ERRCODE='42501';
            END IF;
        END IF;
        IF public.corporate_role_rank(public.corporate_effective_role(actor,NEW.space_id,NEW.parent_id))<4 THEN
            RAISE EXCEPTION 'target requires the manager role' USING ERRCODE='42501';
        END IF;
        IF NOT public.corporate_container_writable(OLD.space_id,OLD.id)
           OR NOT public.corporate_container_writable(NEW.space_id,NEW.parent_id) THEN
            RAISE EXCEPTION 'archived container' USING ERRCODE='42501';
        END IF;
        walker := NEW.parent_id;
        WHILE walker<>'' LOOP
            IF walker=NEW.id OR steps>=64 THEN
                RAISE EXCEPTION 'project cycle' USING ERRCODE='42501';
            END IF;
            steps := steps+1;
            SELECT p.parent_id INTO walker FROM public.projects p WHERE p.id=walker;
            IF NOT FOUND THEN
                walker := '';
            END IF;
        END LOOP;
        RETURN NEW;
    END IF;
    -- A sub-project only follows its parent into the parent's space.
    IF NEW.parent_id IS NOT DISTINCT FROM OLD.parent_id AND NEW.parent_id<>'' THEN
        SELECT p.space_id INTO parent_space FROM public.projects p WHERE p.id=NEW.parent_id;
        IF FOUND AND parent_space=NEW.space_id AND NEW.space_id IS DISTINCT FROM OLD.space_id THEN
            walker := NEW.parent_id;
            WHILE walker<>'' AND steps<64 LOOP
                IF walker=move_root THEN
                    RETURN NEW;
                END IF;
                steps := steps+1;
                SELECT p.parent_id INTO walker FROM public.projects p WHERE p.id=walker;
                IF NOT FOUND THEN
                    walker := '';
                END IF;
            END LOOP;
        END IF;
    END IF;
    RAISE EXCEPTION 'project moves require an access-impact preview' USING ERRCODE='42501';
END
$fn$;
DROP TRIGGER IF EXISTS corporate_projects_no_move ON projects;
CREATE TRIGGER corporate_projects_no_move
BEFORE UPDATE OF space_id,parent_id ON projects
FOR EACH ROW EXECUTE FUNCTION corporate_guard_project_move();

-- A project move leaves the articles of the moved projects with a stale space_id
-- until they are rewritten, a state in which the ordinary update policy sees no
-- role on either the old or the new row. This permissive policy covers exactly
-- that step (UPDATE needs SELECT visibility of the old row as well): only inside a project move, only for articles whose project already
-- lives elsewhere (old row) or whose space is being aligned to it (new row), and
-- only for an editor+ of the project at its current location. The articles'
-- trigger and the projects' trigger still apply.
DROP POLICY IF EXISTS corporate_articles_project_move_read ON articles;
CREATE POLICY corporate_articles_project_move_read ON articles AS PERMISSIVE FOR SELECT
USING (coalesce(current_setting('app.move',true),'')='project' AND project_id<>'' AND EXISTS (
    SELECT 1 FROM public.projects p WHERE p.id=articles.project_id AND p.space_id<>articles.space_id
      AND public.corporate_role_rank(public.corporate_effective_role(current_setting('app.user_id',true),p.space_id,p.id)) >= 3
      AND public.corporate_container_writable(p.space_id,p.id)));
DROP POLICY IF EXISTS corporate_articles_project_move ON articles;
CREATE POLICY corporate_articles_project_move ON articles AS PERMISSIVE FOR UPDATE
USING (coalesce(current_setting('app.move',true),'')='project' AND project_id<>'' AND EXISTS (
    SELECT 1 FROM public.projects p WHERE p.id=articles.project_id AND p.space_id<>articles.space_id
      AND public.corporate_role_rank(public.corporate_effective_role(current_setting('app.user_id',true),p.space_id,p.id)) >= 3
      AND public.corporate_container_writable(p.space_id,p.id)))
WITH CHECK (coalesce(current_setting('app.move',true),'')='project' AND project_id<>'' AND EXISTS (
    SELECT 1 FROM public.projects p WHERE p.id=articles.project_id AND p.space_id=articles.space_id
      AND public.corporate_role_rank(public.corporate_effective_role(current_setting('app.user_id',true),p.space_id,p.id)) >= 3
      AND public.corporate_container_writable(p.space_id,p.id)));

REVOKE ALL ON FUNCTION corporate_role_name(integer) FROM PUBLIC;
REVOKE ALL ON FUNCTION corporate_move_impact(text,text,text,text) FROM PUBLIC;
REVOKE ALL ON FUNCTION corporate_reject_article_move() FROM PUBLIC;
REVOKE ALL ON FUNCTION corporate_guard_project_move() FROM PUBLIC
