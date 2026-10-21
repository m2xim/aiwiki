-- Corporate ACL schema, version 1. Applied transactionally via schema_migrations.
CREATE TABLE IF NOT EXISTS users (
    id text PRIMARY KEY,
    issuer text NOT NULL,
    subject text NOT NULL,
    name text NOT NULL DEFAULT '',
    admin integer NOT NULL DEFAULT 0 CHECK (admin IN (0,1)),
    disabled integer NOT NULL DEFAULT 0 CHECK (disabled IN (0,1)),
    UNIQUE (issuer, subject)
);
CREATE TABLE IF NOT EXISTS teams (
    id text PRIMARY KEY,
    name text NOT NULL,
    description text NOT NULL DEFAULT '',
    archived_at text
);
CREATE TABLE IF NOT EXISTS team_members (
    team_id text NOT NULL REFERENCES teams(id) ON DELETE CASCADE,
    user_id text NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    PRIMARY KEY (team_id,user_id)
);
CREATE TABLE IF NOT EXISTS projects (
    id text PRIMARY KEY,
    space_id text NOT NULL REFERENCES spaces(id) ON DELETE CASCADE,
    parent_id text NOT NULL DEFAULT '',
    name text NOT NULL,
    description text NOT NULL DEFAULT '',
    inherit_acl integer NOT NULL DEFAULT 1 CHECK (inherit_acl IN (0,1)),
    archived_at text
);
CREATE TABLE IF NOT EXISTS memberships (
    resource_type text NOT NULL CHECK (resource_type IN ('space','project')),
    resource_id text NOT NULL,
    subject_type text NOT NULL CHECK (subject_type IN ('user','team')),
    subject_id text NOT NULL,
    role text NOT NULL CHECK (role IN ('owner','manager','editor','commenter','reader')),
    PRIMARY KEY (resource_type,resource_id,subject_type,subject_id)
);
CREATE TABLE IF NOT EXISTS security_audit (
    id bigserial PRIMARY KEY,
    actor_id text NOT NULL,
    action text NOT NULL,
    resource_type text NOT NULL,
    resource_id text NOT NULL,
    details jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at text NOT NULL
);
CREATE TABLE IF NOT EXISTS corporate_space_state (
    space_id text PRIMARY KEY,
    archived boolean NOT NULL DEFAULT false
);

ALTER TABLE spaces ADD COLUMN IF NOT EXISTS owner_id text NOT NULL DEFAULT '';
ALTER TABLE spaces ADD COLUMN IF NOT EXISTS personal integer NOT NULL DEFAULT 0 CHECK (personal IN (0,1));
ALTER TABLE articles ADD COLUMN IF NOT EXISTS project_id text NOT NULL DEFAULT '';

-- Favorites are per corporate identity. Backfill historic rows into the empty
-- legacy principal, which remains invisible to every authenticated user.
ALTER TABLE favorites ADD COLUMN IF NOT EXISTS user_id text;
UPDATE favorites SET user_id = '' WHERE user_id IS NULL;
ALTER TABLE favorites ALTER COLUMN user_id SET DEFAULT current_setting('app.user_id', true);
ALTER TABLE favorites ALTER COLUMN user_id SET NOT NULL;
DO $migration$
DECLARE pk_name text;
BEGIN
    SELECT c.conname INTO pk_name
      FROM pg_constraint c
     WHERE c.conrelid = 'public.favorites'::regclass AND c.contype = 'p';
    IF pk_name IS NOT NULL THEN
        EXECUTE format('ALTER TABLE public.favorites DROP CONSTRAINT %I', pk_name);
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conrelid='public.favorites'::regclass AND contype='p') THEN
        ALTER TABLE favorites ADD PRIMARY KEY (user_id,article_id);
    END IF;
END
$migration$;

CREATE INDEX IF NOT EXISTS memberships_subject_idx ON memberships(subject_type,subject_id,resource_type,resource_id);
CREATE INDEX IF NOT EXISTS memberships_resource_role_idx ON memberships(resource_type,resource_id,role);
CREATE INDEX IF NOT EXISTS team_members_user_idx ON team_members(user_id,team_id);
CREATE INDEX IF NOT EXISTS projects_space_parent_idx ON projects(space_id,parent_id);
CREATE INDEX IF NOT EXISTS articles_project_updated_idx ON articles(project_id,updated_at DESC);
CREATE INDEX IF NOT EXISTS articles_space_project_updated_idx ON articles(space_id,project_id,updated_at DESC);
CREATE INDEX IF NOT EXISTS revisions_article_revision_idx ON revisions(article_id,revision DESC);
CREATE INDEX IF NOT EXISTS comments_article_idx ON comments(article_id);
CREATE INDEX IF NOT EXISTS attachments_article_idx ON attachments(article_id);
CREATE INDEX IF NOT EXISTS security_audit_created_idx ON security_audit(created_at DESC,id DESC);

INSERT INTO corporate_space_state(space_id,archived)
SELECT id, archived_at IS NOT NULL FROM spaces
ON CONFLICT (space_id) DO UPDATE SET archived=EXCLUDED.archived;
INSERT INTO memberships(resource_type,resource_id,subject_type,subject_id,role)
SELECT 'space',id,'user',owner_id,'owner' FROM spaces
 WHERE personal=1 AND owner_id<>''
ON CONFLICT (resource_type,resource_id,subject_type,subject_id) DO NOTHING;

CREATE OR REPLACE FUNCTION corporate_sync_space_state() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,public AS $fn$
BEGIN
    INSERT INTO public.corporate_space_state(space_id,archived)
    VALUES (NEW.id,NEW.archived_at IS NOT NULL)
    ON CONFLICT (space_id) DO UPDATE SET archived=EXCLUDED.archived;
    RETURN NEW;
END
$fn$;
DROP TRIGGER IF EXISTS corporate_spaces_state_sync ON spaces;
CREATE TRIGGER corporate_spaces_state_sync
AFTER INSERT OR UPDATE OF archived_at ON spaces
FOR EACH ROW EXECUTE FUNCTION corporate_sync_space_state();

CREATE OR REPLACE FUNCTION corporate_role_rank(role_name text) RETURNS integer
LANGUAGE sql IMMUTABLE PARALLEL SAFE SET search_path=pg_catalog,public AS $fn$
    SELECT CASE role_name WHEN 'owner' THEN 5 WHEN 'manager' THEN 4
      WHEN 'editor' THEN 3 WHEN 'commenter' THEN 2 WHEN 'reader' THEN 1 ELSE 0 END
$fn$;

CREATE OR REPLACE FUNCTION corporate_effective_role(who text, space_key text, project_key text)
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

CREATE OR REPLACE FUNCTION corporate_can_read_article(who text, space_key text, project_key text)
RETURNS boolean LANGUAGE sql STABLE SECURITY DEFINER
SET search_path=pg_catalog,public AS $fn$
    SELECT public.corporate_role_rank(public.corporate_effective_role(who,space_key,project_key)) >= 1
$fn$;
CREATE OR REPLACE FUNCTION corporate_container_writable(space_key text, project_key text)
RETURNS boolean LANGUAGE sql STABLE SECURITY DEFINER
SET search_path=pg_catalog,public AS $fn$
WITH RECURSIVE chain(id,parent_id,archived,path,depth) AS (
    SELECT p.id,p.parent_id,p.archived_at IS NOT NULL,ARRAY[p.id]::text[],0
      FROM public.projects p WHERE project_key IS NOT NULL AND project_key<>'' AND p.id=project_key AND p.space_id=space_key
    UNION ALL
    SELECT p.id,p.parent_id,p.archived_at IS NOT NULL,chain.path || p.id,chain.depth+1
      FROM chain JOIN public.projects prior ON prior.id=chain.id
      JOIN public.projects p ON p.id=prior.parent_id AND p.space_id=space_key
     WHERE chain.depth<64 AND NOT p.id=ANY(chain.path)
)
SELECT coalesce((SELECT NOT archived FROM public.corporate_space_state WHERE space_id=space_key),false)
   AND NOT EXISTS (SELECT 1 FROM chain WHERE archived)
$fn$;

CREATE OR REPLACE FUNCTION corporate_article_read_policy(space_key text, project_key text) RETURNS boolean
LANGUAGE sql STABLE SET search_path=pg_catalog,public AS $fn$
    SELECT current_setting('app.user_id',true) IS NOT NULL
       AND public.corporate_can_read_article(current_setting('app.user_id',true),space_key,project_key)
$fn$;

CREATE OR REPLACE FUNCTION corporate_reject_article_move() RETURNS trigger
LANGUAGE plpgsql SET search_path=pg_catalog,public AS $fn$
BEGIN
    IF NEW.space_id IS DISTINCT FROM OLD.space_id
       OR NEW.project_id IS DISTINCT FROM OLD.project_id
       OR NEW.parent_id IS DISTINCT FROM OLD.parent_id THEN
        RAISE EXCEPTION 'article moves require an access-impact preview';
    END IF;
    RETURN NEW;
END
$fn$;
DROP TRIGGER IF EXISTS corporate_articles_no_move ON articles;
CREATE TRIGGER corporate_articles_no_move
BEFORE UPDATE OF space_id,project_id,parent_id ON articles
FOR EACH ROW EXECUTE FUNCTION corporate_reject_article_move();

-- These policies are intentionally limited to row identifiers and ACL metadata.
ALTER TABLE spaces ENABLE ROW LEVEL SECURITY;
ALTER TABLE spaces FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS corporate_spaces_read ON spaces;
CREATE POLICY corporate_spaces_read ON spaces FOR SELECT
USING (public.corporate_role_rank(public.corporate_effective_role(current_setting('app.user_id',true),id,'')) >= 1);
DROP POLICY IF EXISTS corporate_spaces_insert ON spaces;
CREATE POLICY corporate_spaces_insert ON spaces FOR INSERT
WITH CHECK (current_setting('app.user_id',true)=owner_id AND EXISTS (
    SELECT 1 FROM public.users u WHERE u.id=owner_id AND u.disabled=0));
DROP POLICY IF EXISTS corporate_spaces_update ON spaces;
CREATE POLICY corporate_spaces_update ON spaces FOR UPDATE
USING (public.corporate_role_rank(public.corporate_effective_role(current_setting('app.user_id',true),id,'')) >= 4)
WITH CHECK (public.corporate_role_rank(public.corporate_effective_role(current_setting('app.user_id',true),id,'')) >= 4);
DROP POLICY IF EXISTS corporate_spaces_delete ON spaces;
CREATE POLICY corporate_spaces_delete ON spaces FOR DELETE
USING (public.corporate_role_rank(public.corporate_effective_role(current_setting('app.user_id',true),id,'')) >= 5);

ALTER TABLE articles ENABLE ROW LEVEL SECURITY;
ALTER TABLE articles FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS corporate_articles_read ON articles;
CREATE POLICY corporate_articles_read ON articles FOR SELECT
USING (public.corporate_article_read_policy(space_id,project_id));
DROP POLICY IF EXISTS corporate_articles_insert ON articles;
CREATE POLICY corporate_articles_insert ON articles FOR INSERT
WITH CHECK (public.corporate_role_rank(public.corporate_effective_role(current_setting('app.user_id',true),space_id,project_id)) >= 3
  AND public.corporate_container_writable(space_id,project_id));
DROP POLICY IF EXISTS corporate_articles_update ON articles;
CREATE POLICY corporate_articles_update ON articles FOR UPDATE
USING (public.corporate_role_rank(public.corporate_effective_role(current_setting('app.user_id',true),space_id,project_id)) >= 3
  AND public.corporate_container_writable(space_id,project_id))
WITH CHECK (public.corporate_role_rank(public.corporate_effective_role(current_setting('app.user_id',true),space_id,project_id)) >= 3
  AND public.corporate_container_writable(space_id,project_id));
DROP POLICY IF EXISTS corporate_articles_delete ON articles;
CREATE POLICY corporate_articles_delete ON articles FOR DELETE
USING (public.corporate_role_rank(public.corporate_effective_role(current_setting('app.user_id',true),space_id,project_id)) >= 3
  AND public.corporate_container_writable(space_id,project_id));

ALTER TABLE revisions ENABLE ROW LEVEL SECURITY;
ALTER TABLE revisions FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS corporate_revisions_read ON revisions;
CREATE POLICY corporate_revisions_read ON revisions FOR SELECT USING (
    EXISTS (SELECT 1 FROM public.articles current_article
      WHERE current_article.id=revisions.article_id
        AND public.corporate_article_read_policy(current_article.space_id,current_article.project_id))
    AND public.corporate_article_read_policy(
      (revisions.snapshot::text::jsonb->>'space_id'),
      coalesce(revisions.snapshot::text::jsonb->>'project_id',''))
);
DROP POLICY IF EXISTS corporate_revisions_insert ON revisions;
CREATE POLICY corporate_revisions_insert ON revisions FOR INSERT WITH CHECK (
    EXISTS (SELECT 1 FROM public.articles current_article
      WHERE current_article.id=revisions.article_id
        AND public.corporate_role_rank(public.corporate_effective_role(current_setting('app.user_id',true),current_article.space_id,current_article.project_id)) >= 3
        AND public.corporate_container_writable(current_article.space_id,current_article.project_id))
);
DROP POLICY IF EXISTS corporate_revisions_update ON revisions;
CREATE POLICY corporate_revisions_update ON revisions FOR UPDATE USING (false) WITH CHECK (false);
DROP POLICY IF EXISTS corporate_revisions_delete ON revisions;
CREATE POLICY corporate_revisions_delete ON revisions FOR DELETE USING (false);

ALTER TABLE comments ENABLE ROW LEVEL SECURITY;
ALTER TABLE comments FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS corporate_comments_read ON comments;
CREATE POLICY corporate_comments_read ON comments FOR SELECT USING (
    EXISTS (SELECT 1 FROM public.articles a WHERE a.id=comments.article_id
      AND public.corporate_article_read_policy(a.space_id,a.project_id)));
DROP POLICY IF EXISTS corporate_comments_insert ON comments;
CREATE POLICY corporate_comments_insert ON comments FOR INSERT WITH CHECK (
    EXISTS (SELECT 1 FROM public.articles a WHERE a.id=comments.article_id
      AND public.corporate_role_rank(public.corporate_effective_role(current_setting('app.user_id',true),a.space_id,a.project_id)) >= 2
      AND (comments.author=current_setting('app.user_id',true)
        OR public.corporate_role_rank(public.corporate_effective_role(current_setting('app.user_id',true),a.space_id,a.project_id)) >= 3)
      AND public.corporate_container_writable(a.space_id,a.project_id)));
DROP POLICY IF EXISTS corporate_comments_update ON comments;
CREATE POLICY corporate_comments_update ON comments FOR UPDATE USING (
    EXISTS (SELECT 1 FROM public.articles a WHERE a.id=comments.article_id
      AND public.corporate_role_rank(public.corporate_effective_role(current_setting('app.user_id',true),a.space_id,a.project_id)) >= 2
      AND (comments.author=current_setting('app.user_id',true)
        OR public.corporate_role_rank(public.corporate_effective_role(current_setting('app.user_id',true),a.space_id,a.project_id)) >= 3)
      AND public.corporate_container_writable(a.space_id,a.project_id)))
WITH CHECK (EXISTS (SELECT 1 FROM public.articles a WHERE a.id=comments.article_id
      AND public.corporate_role_rank(public.corporate_effective_role(current_setting('app.user_id',true),a.space_id,a.project_id)) >= 2
      AND (comments.author=current_setting('app.user_id',true)
        OR public.corporate_role_rank(public.corporate_effective_role(current_setting('app.user_id',true),a.space_id,a.project_id)) >= 3)
      AND public.corporate_container_writable(a.space_id,a.project_id)));
DROP POLICY IF EXISTS corporate_comments_delete ON comments;
CREATE POLICY corporate_comments_delete ON comments FOR DELETE USING (
    EXISTS (SELECT 1 FROM public.articles a WHERE a.id=comments.article_id
      AND public.corporate_role_rank(public.corporate_effective_role(current_setting('app.user_id',true),a.space_id,a.project_id)) >= 2
      AND (comments.author=current_setting('app.user_id',true)
        OR public.corporate_role_rank(public.corporate_effective_role(current_setting('app.user_id',true),a.space_id,a.project_id)) >= 3)
      AND public.corporate_container_writable(a.space_id,a.project_id)));

ALTER TABLE attachments ENABLE ROW LEVEL SECURITY;
ALTER TABLE attachments FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS corporate_attachments_read ON attachments;
CREATE POLICY corporate_attachments_read ON attachments FOR SELECT USING (
    EXISTS (SELECT 1 FROM public.articles a WHERE a.id=attachments.article_id
      AND public.corporate_article_read_policy(a.space_id,a.project_id)));
DROP POLICY IF EXISTS corporate_attachments_insert ON attachments;
CREATE POLICY corporate_attachments_insert ON attachments FOR INSERT WITH CHECK (
    EXISTS (SELECT 1 FROM public.articles a WHERE a.id=attachments.article_id
      AND public.corporate_role_rank(public.corporate_effective_role(current_setting('app.user_id',true),a.space_id,a.project_id)) >= 3
      AND public.corporate_container_writable(a.space_id,a.project_id)));
DROP POLICY IF EXISTS corporate_attachments_update ON attachments;
CREATE POLICY corporate_attachments_update ON attachments FOR UPDATE USING (
    EXISTS (SELECT 1 FROM public.articles a WHERE a.id=attachments.article_id
      AND public.corporate_role_rank(public.corporate_effective_role(current_setting('app.user_id',true),a.space_id,a.project_id)) >= 3
      AND public.corporate_container_writable(a.space_id,a.project_id)))
WITH CHECK (EXISTS (SELECT 1 FROM public.articles a WHERE a.id=attachments.article_id
      AND public.corporate_role_rank(public.corporate_effective_role(current_setting('app.user_id',true),a.space_id,a.project_id)) >= 3
      AND public.corporate_container_writable(a.space_id,a.project_id)));
DROP POLICY IF EXISTS corporate_attachments_delete ON attachments;
CREATE POLICY corporate_attachments_delete ON attachments FOR DELETE USING (
    EXISTS (SELECT 1 FROM public.articles a WHERE a.id=attachments.article_id
      AND public.corporate_role_rank(public.corporate_effective_role(current_setting('app.user_id',true),a.space_id,a.project_id)) >= 3
      AND public.corporate_container_writable(a.space_id,a.project_id)));

ALTER TABLE favorites ENABLE ROW LEVEL SECURITY;
ALTER TABLE favorites FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS corporate_favorites_read ON favorites;
CREATE POLICY corporate_favorites_read ON favorites FOR SELECT USING (
    user_id=current_setting('app.user_id',true) AND EXISTS (SELECT 1 FROM public.articles a
      WHERE a.id=favorites.article_id AND public.corporate_article_read_policy(a.space_id,a.project_id)));
DROP POLICY IF EXISTS corporate_favorites_insert ON favorites;
CREATE POLICY corporate_favorites_insert ON favorites FOR INSERT WITH CHECK (
    user_id=current_setting('app.user_id',true) AND EXISTS (SELECT 1 FROM public.articles a
      WHERE a.id=favorites.article_id AND public.corporate_article_read_policy(a.space_id,a.project_id)));
DROP POLICY IF EXISTS corporate_favorites_delete ON favorites;
CREATE POLICY corporate_favorites_delete ON favorites FOR DELETE USING (user_id=current_setting('app.user_id',true));

DO $rls$
BEGIN
    IF to_regclass('public.article_fts') IS NOT NULL THEN
        EXECUTE 'ALTER TABLE public.article_fts ENABLE ROW LEVEL SECURITY';
        EXECUTE 'ALTER TABLE public.article_fts FORCE ROW LEVEL SECURITY';
        EXECUTE 'DROP POLICY IF EXISTS corporate_fts_read ON public.article_fts';
        EXECUTE 'CREATE POLICY corporate_fts_read ON public.article_fts FOR SELECT USING (EXISTS (SELECT 1 FROM public.articles a WHERE a.id=article_fts.article_id AND public.corporate_article_read_policy(a.space_id,a.project_id)))';
        EXECUTE 'DROP POLICY IF EXISTS corporate_fts_insert ON public.article_fts';
        EXECUTE 'CREATE POLICY corporate_fts_insert ON public.article_fts FOR INSERT WITH CHECK (EXISTS (SELECT 1 FROM public.articles a WHERE a.id=article_fts.article_id AND public.corporate_role_rank(public.corporate_effective_role(current_setting(''app.user_id'',true),a.space_id,a.project_id)) >= 3 AND public.corporate_container_writable(a.space_id,a.project_id)))';
        EXECUTE 'DROP POLICY IF EXISTS corporate_fts_update ON public.article_fts';
        EXECUTE 'CREATE POLICY corporate_fts_update ON public.article_fts FOR UPDATE USING (EXISTS (SELECT 1 FROM public.articles a WHERE a.id=article_fts.article_id AND public.corporate_role_rank(public.corporate_effective_role(current_setting(''app.user_id'',true),a.space_id,a.project_id)) >= 3 AND public.corporate_container_writable(a.space_id,a.project_id))) WITH CHECK (EXISTS (SELECT 1 FROM public.articles a WHERE a.id=article_fts.article_id AND public.corporate_role_rank(public.corporate_effective_role(current_setting(''app.user_id'',true),a.space_id,a.project_id)) >= 3 AND public.corporate_container_writable(a.space_id,a.project_id)))';
        EXECUTE 'DROP POLICY IF EXISTS corporate_fts_delete ON public.article_fts';
        EXECUTE 'CREATE POLICY corporate_fts_delete ON public.article_fts FOR DELETE USING (EXISTS (SELECT 1 FROM public.articles a WHERE a.id=article_fts.article_id AND public.corporate_role_rank(public.corporate_effective_role(current_setting(''app.user_id'',true),a.space_id,a.project_id)) >= 3 AND public.corporate_container_writable(a.space_id,a.project_id)))';
    END IF;
END
$rls$;

REVOKE ALL ON FUNCTION corporate_effective_role(text,text,text) FROM PUBLIC;
REVOKE ALL ON FUNCTION corporate_role_rank(text) FROM PUBLIC;
REVOKE ALL ON FUNCTION corporate_can_read_article(text,text,text) FROM PUBLIC;
REVOKE ALL ON FUNCTION corporate_container_writable(text,text) FROM PUBLIC;
REVOKE ALL ON FUNCTION corporate_article_read_policy(text,text) FROM PUBLIC;
REVOKE ALL ON FUNCTION corporate_sync_space_state() FROM PUBLIC;
REVOKE ALL ON FUNCTION corporate_reject_article_move() FROM PUBLIC;
