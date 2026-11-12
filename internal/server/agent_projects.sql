-- Agent projects (component "agent_projects" v1, append-only on top of every other component).
--
--  A. Projects inside spaces (the "projects" folders) are removed from the model: grants on them are
--     deleted (narrowing access, confirmed by the product owner), content stays in its space with
--     project_id = '' and CHECK constraints keep it that way. The table, the columns and the
--     recursive role functions stay (policies and functions reference them); they only ever see
--     empty data now.
--  B. Personal spaces are derived from memberships; the administrator reads a limited directory.
--  C. agent_projects / agent_project_spaces: named sets of spaces with a read or write mode. A project
--     grants nothing; it only limits an agent.
--  D. MCP keys name agent projects (mcp_access v8 added the columns); each key of the previous
--     release gets a private project built from its scope.
SET LOCAL lock_timeout = '60s';
SET LOCAL statement_timeout = 0;

-- ---------------------------------------------------------------------------------------------
-- C. Tables
-- ---------------------------------------------------------------------------------------------
CREATE TABLE agent_projects (
    id text PRIMARY KEY,
    name text NOT NULL CHECK (char_length(name) BETWEEN 1 AND 120),
    description text NOT NULL DEFAULT '' CHECK (char_length(description) <= 2000),
    owner_id text NOT NULL REFERENCES users(id),
    visibility text NOT NULL CHECK (visibility IN ('private','public')),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX agent_projects_owner_idx ON agent_projects(owner_id);
CREATE TABLE agent_project_spaces (
    project_id text NOT NULL REFERENCES agent_projects(id) ON DELETE CASCADE,
    row_id text NOT NULL UNIQUE,
    space_id text NOT NULL REFERENCES spaces(id) ON DELETE CASCADE,
    mode text NOT NULL CHECK (mode IN ('read','write')),
    PRIMARY KEY (project_id, space_id)
);
CREATE INDEX agent_project_spaces_space_idx ON agent_project_spaces(space_id);

-- ---------------------------------------------------------------------------------------------
-- Helper functions. SECURITY DEFINER only where the caller cannot read the data itself; search_path
-- pinned; never executable by PUBLIC.
-- ---------------------------------------------------------------------------------------------
CREATE FUNCTION corporate_ap_live_user() RETURNS boolean
LANGUAGE sql STABLE SECURITY DEFINER SET search_path=pg_catalog,public AS $fn$
    SELECT EXISTS (SELECT 1 FROM public.users u WHERE u.id=nullif(current_setting('app.user_id',true),'') AND u.disabled=0)
$fn$;

-- Reading: the owner sees every own project, everybody else sees public ones. A scoped MCP session
-- sees only the project it is connected to.
CREATE FUNCTION corporate_ap_can_read(project_key text, owner_key text, vis text) RETURNS boolean
LANGUAGE sql STABLE SECURITY DEFINER SET search_path=pg_catalog,public AS $fn$
    SELECT public.corporate_ap_live_user()
       AND (owner_key=current_setting('app.user_id',true) OR vis='public')
       AND (NOT public.corporate_mcp_scope_active()
            OR (project_key=nullif(current_setting('app.mcp_project',true),'')))
$fn$;

-- Writing: the owner; the administrator only for public projects. Never from an MCP session.
CREATE FUNCTION corporate_ap_can_write(owner_key text, vis text) RETURNS boolean
LANGUAGE sql STABLE SECURITY DEFINER SET search_path=pg_catalog,public AS $fn$
    SELECT NOT public.corporate_mcp_scope_active() AND public.corporate_ap_live_user()
       AND (owner_key=current_setting('app.user_id',true)
            OR (vis='public' AND public.artifact_is_admin(current_setting('app.user_id',true))))
$fn$;

-- A space may be added to a project by someone who sees it (any role; the general space counts).
CREATE FUNCTION corporate_ap_space_addable(space_key text) RETURNS boolean
LANGUAGE sql STABLE SECURITY DEFINER SET search_path=pg_catalog,public AS $fn$
    SELECT public.corporate_ap_live_user()
       AND public.corporate_role_rank(public.corporate_owner_effective_role(current_setting('app.user_id',true),space_key,''))>=1
       AND coalesce((SELECT NOT st.archived FROM public.corporate_space_state st WHERE st.space_id=space_key),false)
$fn$;

CREATE FUNCTION corporate_ap_guard() RETURNS trigger
LANGUAGE plpgsql SET search_path=pg_catalog,public AS $fn$
BEGIN
    IF NEW.id IS DISTINCT FROM OLD.id OR NEW.owner_id IS DISTINCT FROM OLD.owner_id
       OR NEW.created_at IS DISTINCT FROM OLD.created_at THEN
        RAISE EXCEPTION 'agent project identity and owner are immutable' USING ERRCODE='42501';
    END IF;
    -- Visibility belongs to the owner alone (an administrator edits public projects otherwise).
    IF NEW.visibility IS DISTINCT FROM OLD.visibility
       AND OLD.owner_id IS DISTINCT FROM nullif(current_setting('app.user_id',true),'')
       AND NOT pg_has_role(session_user,(SELECT c.relowner FROM pg_class c WHERE c.oid='public.agent_projects'::regclass),'MEMBER') THEN
        RAISE EXCEPTION 'only the owner changes the visibility' USING ERRCODE='42501';
    END IF;
    RETURN NEW;
END
$fn$;
CREATE TRIGGER agent_projects_guard BEFORE UPDATE ON agent_projects
FOR EACH ROW EXECUTE FUNCTION corporate_ap_guard();

CREATE FUNCTION corporate_aps_guard() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,public AS $fn$
DECLARE n integer;
BEGIN
    IF TG_OP='UPDATE' THEN
        IF NEW.project_id IS DISTINCT FROM OLD.project_id OR NEW.space_id IS DISTINCT FROM OLD.space_id
           OR NEW.row_id IS DISTINCT FROM OLD.row_id THEN
            RAISE EXCEPTION 'only the mode of a project row changes' USING ERRCODE='42501';
        END IF;
        RETURN NEW;
    END IF;
    PERFORM 1 FROM public.agent_projects p WHERE p.id=NEW.project_id FOR UPDATE;
    SELECT count(*) INTO n FROM public.agent_project_spaces s WHERE s.project_id=NEW.project_id;
    IF n > 20 THEN
        RAISE EXCEPTION 'too many spaces in a project' USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END
$fn$;
CREATE TRIGGER agent_project_spaces_guard_update BEFORE UPDATE ON agent_project_spaces
FOR EACH ROW EXECUTE FUNCTION corporate_aps_guard();
CREATE CONSTRAINT TRIGGER agent_project_spaces_guard_insert AFTER INSERT ON agent_project_spaces
DEFERRABLE INITIALLY IMMEDIATE FOR EACH ROW EXECUTE FUNCTION corporate_aps_guard();

-- ---------------------------------------------------------------------------------------------
-- Row-level security. NO FORCE: the owner role (migrations, SECURITY DEFINER functions) reads the
-- tables to compute scopes; the runtime role is bound by the policies.
-- ---------------------------------------------------------------------------------------------
ALTER TABLE agent_projects ENABLE ROW LEVEL SECURITY;
ALTER TABLE agent_projects NO FORCE ROW LEVEL SECURITY;
CREATE POLICY agent_projects_read ON agent_projects FOR SELECT
USING (public.corporate_ap_can_read(id, owner_id, visibility));
CREATE POLICY agent_projects_insert ON agent_projects FOR INSERT
WITH CHECK (NOT public.corporate_mcp_scope_active() AND public.corporate_ap_live_user()
            AND owner_id=current_setting('app.user_id',true));
CREATE POLICY agent_projects_update ON agent_projects FOR UPDATE
USING (public.corporate_ap_can_write(owner_id, visibility))
WITH CHECK (public.corporate_ap_can_write(owner_id, visibility));
CREATE POLICY agent_projects_delete ON agent_projects FOR DELETE
USING (public.corporate_ap_can_write(owner_id, visibility));

ALTER TABLE agent_project_spaces ENABLE ROW LEVEL SECURITY;
ALTER TABLE agent_project_spaces NO FORCE ROW LEVEL SECURITY;
CREATE POLICY agent_project_spaces_read ON agent_project_spaces FOR SELECT
USING (EXISTS (SELECT 1 FROM public.agent_projects p WHERE p.id=agent_project_spaces.project_id));
CREATE POLICY agent_project_spaces_insert ON agent_project_spaces FOR INSERT
WITH CHECK (EXISTS (SELECT 1 FROM public.agent_projects p WHERE p.id=agent_project_spaces.project_id
                      AND public.corporate_ap_can_write(p.owner_id, p.visibility))
            AND public.corporate_ap_space_addable(space_id));
CREATE POLICY agent_project_spaces_update ON agent_project_spaces FOR UPDATE
USING (EXISTS (SELECT 1 FROM public.agent_projects p WHERE p.id=agent_project_spaces.project_id
                 AND public.corporate_ap_can_write(p.owner_id, p.visibility)))
WITH CHECK (EXISTS (SELECT 1 FROM public.agent_projects p WHERE p.id=agent_project_spaces.project_id
                 AND public.corporate_ap_can_write(p.owner_id, p.visibility)));
CREATE POLICY agent_project_spaces_delete ON agent_project_spaces FOR DELETE
USING (EXISTS (SELECT 1 FROM public.agent_projects p WHERE p.id=agent_project_spaces.project_id
                 AND public.corporate_ap_can_write(p.owner_id, p.visibility)));

-- ---------------------------------------------------------------------------------------------
-- Space names the viewer may not read through RLS are read by the SECURITY DEFINER functions below with the
-- existing storage_quota_probe_spaces policy (passes only for definer code that sets app.quota_probe).

-- The administrator may manage roles of any space, whether or not they have a role in it.
CREATE FUNCTION corporate_admin_space_exists(space_key text) RETURNS boolean
LANGUAGE sql STABLE SECURITY DEFINER SET search_path=pg_catalog,public AS $fn$
    SELECT public.artifact_is_admin(nullif(current_setting('app.user_id',true),''))
       AND NOT public.corporate_mcp_scope_active()
       AND EXISTS (SELECT 1 FROM public.corporate_space_state st WHERE st.space_id=space_key)
$fn$;

-- Limited directory for the administrator: spaces without a role of theirs, name and owner only.
CREATE FUNCTION corporate_admin_space_directory()
RETURNS TABLE(id text, name text, owner_name text, personal boolean)
LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path=pg_catalog,public AS $fn$
DECLARE who text := nullif(current_setting('app.user_id',true),'');
BEGIN
    IF who IS NULL OR NOT public.artifact_is_admin(who) OR public.corporate_mcp_scope_active() THEN RETURN; END IF;
    PERFORM set_config('app.quota_probe','1',true);
    RETURN QUERY
    SELECT s.id, s.name,
           coalesce((SELECT u.name FROM public.memberships m JOIN public.users u ON u.id=m.subject_id
                      WHERE m.resource_type='space' AND m.resource_id=s.id AND m.subject_type='user' AND m.role='owner'
                      ORDER BY u.name, u.id LIMIT 1),
                    (SELECT u.name FROM public.users u WHERE u.id=s.owner_id), ''),
           s.id<>public.corporate_general_space()
             AND (SELECT count(*) FROM public.memberships m WHERE m.resource_type='space' AND m.resource_id=s.id)=1
             AND EXISTS (SELECT 1 FROM public.memberships m WHERE m.resource_type='space' AND m.resource_id=s.id
                           AND m.subject_type='user' AND m.role='owner')
      FROM public.spaces s
      JOIN public.corporate_space_state st ON st.space_id=s.id AND NOT st.archived
     WHERE public.corporate_role_rank(public.corporate_owner_effective_role(who,s.id,''))=0
     ORDER BY lower(s.name), s.id;
    PERFORM set_config('app.quota_probe','',true);
END
$fn$;

-- ---------------------------------------------------------------------------------------------
-- Project views. The header rows and the space rows of the projects the caller may see; a scoped
-- session sees only the projects of its key. An inaccessible space row carries no space id,
-- name or role.
-- ---------------------------------------------------------------------------------------------
CREATE FUNCTION corporate_agent_projects_visible(ids text[])
RETURNS TABLE(id text, name text, description text, owner_id text, owner_name text, visibility text,
              can_edit boolean, created_at timestamptz, updated_at timestamptz)
LANGUAGE plpgsql STABLE SECURITY DEFINER SET search_path=pg_catalog,public AS $fn$
DECLARE who text := nullif(current_setting('app.user_id',true),'');
        scoped boolean := public.corporate_mcp_scope_active();
        cred text := nullif(current_setting('app.mcp_credential',true),'');
        keyset text[];
BEGIN
    IF who IS NULL OR NOT EXISTS (SELECT 1 FROM public.users u WHERE u.id=who AND u.disabled=0) THEN RETURN; END IF;
    -- A scoped session lists only what its key lists; a scoped session without a key (an OAuth session that
    -- named a project) sees what its user sees; a scoped session with neither sees nothing.
    IF scoped AND cred IS NULL AND nullif(current_setting('app.mcp_project',true),'') IS NULL THEN RETURN; END IF;
    IF scoped AND cred IS NOT NULL THEN
        SELECT ARRAY(SELECT jsonb_array_elements_text(c.agent_project_ids)) INTO keyset
          FROM public.mcp_credentials c
         WHERE c.id=cred AND c.owner_id=who AND c.revoked_at IS NULL AND c.expires_at>clock_timestamp()
           AND c.agent_project_ids IS NOT NULL;
        IF keyset IS NULL THEN RETURN; END IF;
    END IF;
    RETURN QUERY
    SELECT p.id, p.name, p.description, p.owner_id, coalesce(o.name,''), p.visibility,
           (NOT scoped AND (p.owner_id=who OR (p.visibility='public' AND public.artifact_is_admin(who)))),
           p.created_at, p.updated_at
      FROM public.agent_projects p LEFT JOIN public.users o ON o.id=p.owner_id
     WHERE (p.owner_id=who OR p.visibility='public')
       AND (ids IS NULL OR p.id=ANY(ids))
       AND (keyset IS NULL OR p.id=ANY(keyset))
     ORDER BY lower(p.name), p.id;
END
$fn$;

CREATE FUNCTION corporate_agent_project_rows(ids text[])
RETURNS TABLE(project_id text, row_id text, mode text, accessible boolean, space_id text, name text, my_role text)
LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path=pg_catalog,public AS $fn$
DECLARE who text := nullif(current_setting('app.user_id',true),'');
        allowed text[];
BEGIN
    IF who IS NULL THEN RETURN; END IF;
    SELECT ARRAY(SELECT v.id FROM public.corporate_agent_projects_visible(ids) v) INTO allowed;
    IF cardinality(allowed)=0 THEN RETURN; END IF;
    PERFORM set_config('app.quota_probe','1',true);
    RETURN QUERY
    SELECT r.project_id, r.row_id, r.mode, r.rank>=1,
           CASE WHEN r.rank>=1 THEN r.space_id END,
           CASE WHEN r.rank>=1 THEN sp.name END,
           CASE WHEN r.rank>=1 THEN r.role END
      FROM (SELECT s.project_id, s.row_id, s.space_id, s.mode,
                   public.corporate_owner_effective_role(who,s.space_id,'') AS role,
                   public.corporate_role_rank(public.corporate_owner_effective_role(who,s.space_id,'')) AS rank
              FROM public.agent_project_spaces s WHERE s.project_id=ANY(allowed)) r
      LEFT JOIN public.spaces sp ON sp.id=r.space_id
     ORDER BY r.project_id, r.rank>=1 DESC, sp.name, r.row_id;
    PERFORM set_config('app.quota_probe','',true);
END
$fn$;

-- ---------------------------------------------------------------------------------------------
-- Session scope of an agent project. Only callable from SECURITY DEFINER code (invoker function
-- guarded by current_user<>session_user).
-- ---------------------------------------------------------------------------------------------
CREATE FUNCTION corporate_agent_scope_rows(owner_key text, project_key text, force_read boolean)
RETURNS jsonb LANGUAGE plpgsql STABLE SET search_path=pg_catalog,public AS $fn$
DECLARE scope jsonb; general text;
BEGIN
    IF current_user=session_user THEN
        RAISE EXCEPTION 'access denied' USING ERRCODE='42501';
    END IF;
    general := public.corporate_general_space();
    IF project_key IS NOT NULL AND project_key<>'' THEN
        IF NOT EXISTS (SELECT 1 FROM public.agent_projects p WHERE p.id=project_key
                         AND (p.owner_id=owner_key OR p.visibility='public')) THEN
            RETURN NULL;
        END IF;
        SELECT jsonb_agg(jsonb_build_object('type','space','id',s.space_id,
                                            'mode',CASE WHEN force_read THEN 'read' ELSE s.mode END)
                         ORDER BY s.space_id)
          INTO scope FROM public.agent_project_spaces s WHERE s.project_id=project_key;
    END IF;
    IF scope IS NULL THEN
        scope := jsonb_build_array(jsonb_build_object('type','space','id',general,'mode','read'));
    END IF;
    RETURN scope;
END
$fn$;

-- The scope of a persistent key for one request: NULL when the key, its owner or the project are
-- not usable. Without a project: introspection (read of the general space only).
CREATE FUNCTION corporate_mcp_project_scope(cred text, project_key text) RETURNS jsonb
LANGUAGE plpgsql STABLE SECURITY DEFINER SET search_path=pg_catalog,public AS $fn$
DECLARE c public.mcp_credentials%ROWTYPE; who text := nullif(current_setting('app.user_id',true),'');
BEGIN
    SELECT * INTO c FROM public.mcp_credentials k
     WHERE k.id=cred AND k.revoked_at IS NULL AND k.expires_at>clock_timestamp()
       AND k.agent_project_ids IS NOT NULL;
    -- A session already bound to a user may only ask for its own key.
    IF NOT FOUND OR who IS NULL OR who<>c.owner_id
       OR NOT EXISTS (SELECT 1 FROM public.users u WHERE u.id=c.owner_id AND u.disabled=0) THEN
        RETURN NULL;
    END IF;
    IF project_key IS NULL OR project_key='' THEN
        RETURN public.corporate_agent_scope_rows(c.owner_id,'',true);
    END IF;
    IF NOT jsonb_exists(c.agent_project_ids, project_key) THEN RETURN NULL; END IF;
    RETURN public.corporate_agent_scope_rows(c.owner_id,project_key,c.read_only);
END
$fn$;

-- Same for a browser-less OAuth session that names a project.
CREATE FUNCTION corporate_user_project_scope(owner_key text, project_key text) RETURNS jsonb
LANGUAGE plpgsql STABLE SECURITY DEFINER SET search_path=pg_catalog,public AS $fn$
DECLARE who text := nullif(current_setting('app.user_id',true),'');
BEGIN
    IF who IS NULL OR who<>owner_key OR project_key IS NULL OR project_key='' OR NOT EXISTS (SELECT 1 FROM public.users u WHERE u.id=owner_key AND u.disabled=0) THEN
        RETURN NULL;
    END IF;
    RETURN public.corporate_agent_scope_rows(owner_key,project_key,false);
END
$fn$;

-- ---------------------------------------------------------------------------------------------
-- A. Removal of the projects inside spaces (everything in this migration's single transaction)
-- ---------------------------------------------------------------------------------------------
-- D. Keys of the previous release: one private project per live key, built from its scope. A project
--    element becomes a row of its space with the same mode; several elements of one space keep the
--    highest mode. This runs before the old projects are deleted.
DO $transfer$
DECLARE c record; pid text; moved integer := 0;
BEGIN
    FOR c IN SELECT k.id, k.owner_id, k.name, k.scope FROM public.mcp_credentials k
              WHERE k.revoked_at IS NULL AND k.agent_project_ids IS NULL
              ORDER BY k.created_at, k.id FOR UPDATE LOOP
        pid := replace(gen_random_uuid()::text,'-','');
        INSERT INTO public.agent_projects(id,name,description,owner_id,visibility)
        VALUES (pid, 'Ключ «' || left(c.name,80) || '»', 'Создан при переходе на проекты агентов из настроек ключа.', c.owner_id, 'private');
        INSERT INTO public.agent_project_spaces(project_id,row_id,space_id,mode)
        SELECT pid, replace(gen_random_uuid()::text,'-',''), t.space_id, t.mode
          FROM (SELECT x.space_id, CASE WHEN bool_or(x.mode='write') THEN 'write' ELSE 'read' END AS mode
                  FROM (SELECT CASE e.value->>'type' WHEN 'space' THEN e.value->>'id'
                                 ELSE (SELECT p.space_id FROM public.projects p WHERE p.id=e.value->>'id') END AS space_id,
                               e.value->>'mode' AS mode
                          FROM jsonb_array_elements(c.scope) AS e(value)
                         WHERE jsonb_typeof(e.value)='object') x
                 WHERE x.space_id IS NOT NULL AND x.mode IN ('read','write')
                   AND EXISTS (SELECT 1 FROM public.corporate_space_state st WHERE st.space_id=x.space_id)
                 GROUP BY x.space_id) t;
        UPDATE public.mcp_credentials SET agent_project_ids=jsonb_build_array(pid), read_only=false WHERE id=c.id;
        INSERT INTO public.security_audit(actor_id,action,resource_type,resource_id,details,created_at)
        VALUES (c.owner_id,'migration.key_project_created','mcp_credential',c.id,
                jsonb_build_object('project_id',pid,'scope',c.scope),
                to_char(clock_timestamp() AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.MS"Z"'));
        moved := moved + 1;
    END LOOP;
END
$transfer$;
ALTER TABLE mcp_credentials ADD CONSTRAINT mcp_credentials_projects_required
    CHECK (revoked_at IS NOT NULL OR agent_project_ids IS NOT NULL);

-- Grants on the folders are removed (not moved to the space): this narrows access. One audit row first.
INSERT INTO security_audit(actor_id,action,resource_type,resource_id,details,created_at)
SELECT 'migration','migration.project_grants_removed','migration','agent_projects_v1',
       jsonb_build_object('count',count(*),
                          'projects',(SELECT count(*) FROM projects),
                          'grants',coalesce(jsonb_agg(jsonb_build_object('resource_id',m.resource_id,'subject_type',m.subject_type,'subject_id',m.subject_id,'role',m.role)
                                                      ORDER BY m.resource_id,m.subject_type,m.subject_id),'[]'::jsonb)),
       to_char(clock_timestamp() AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.MS"Z"')
  FROM memberships m WHERE m.resource_type='project';
DELETE FROM memberships WHERE resource_type='project';

-- Content leaves its folders. FORCE RLS and the move guards are lifted for this transaction only.
ALTER TABLE articles NO FORCE ROW LEVEL SECURITY;
ALTER TABLE revisions NO FORCE ROW LEVEL SECURITY;
ALTER TABLE artifacts NO FORCE ROW LEVEL SECURITY;
ALTER TABLE articles DISABLE TRIGGER corporate_articles_no_move;
ALTER TABLE artifacts DISABLE TRIGGER artifacts_guard;
UPDATE articles SET project_id='' WHERE project_id<>'';
UPDATE revisions SET snapshot=((snapshot::jsonb) - 'project_id')::text, project_id='' WHERE project_id<>'' OR (snapshot::jsonb->'project_id') IS NOT NULL;
UPDATE artifacts SET project_id='' WHERE project_id<>'';
-- Deferred constraint triggers (artifact container checks) would block the ALTER TABLE statements below.
SET CONSTRAINTS ALL IMMEDIATE;
ALTER TABLE articles ENABLE TRIGGER corporate_articles_no_move;
ALTER TABLE artifacts ENABLE TRIGGER artifacts_guard;
ALTER TABLE articles FORCE ROW LEVEL SECURITY;
ALTER TABLE revisions FORCE ROW LEVEL SECURITY;
ALTER TABLE artifacts FORCE ROW LEVEL SECURITY;

DELETE FROM projects;
SET CONSTRAINTS ALL IMMEDIATE;

ALTER TABLE articles ADD CONSTRAINT articles_no_project CHECK (project_id = '');
ALTER TABLE revisions ADD CONSTRAINT revisions_no_project CHECK (project_id = '');
ALTER TABLE artifacts ADD CONSTRAINT artifacts_no_project CHECK (project_id = '');
ALTER TABLE memberships ADD CONSTRAINT memberships_space_only CHECK (resource_type = 'space');

CREATE FUNCTION corporate_reject_project_insert() RETURNS trigger
LANGUAGE plpgsql SET search_path=pg_catalog,public AS $fn$
BEGIN
    RAISE EXCEPTION 'projects inside spaces were removed' USING ERRCODE='42501';
END
$fn$;
CREATE TRIGGER corporate_projects_removed BEFORE INSERT ON projects
FOR EACH ROW EXECUTE FUNCTION corporate_reject_project_insert();

-- The move-time policies on articles only mattered for folder moves.
DROP POLICY IF EXISTS corporate_articles_project_move_read ON articles;
DROP POLICY IF EXISTS corporate_articles_project_move ON articles;

REVOKE ALL ON FUNCTION corporate_ap_live_user() FROM PUBLIC;
REVOKE ALL ON FUNCTION corporate_ap_can_read(text,text,text) FROM PUBLIC;
REVOKE ALL ON FUNCTION corporate_ap_can_write(text,text) FROM PUBLIC;
REVOKE ALL ON FUNCTION corporate_ap_space_addable(text) FROM PUBLIC;
REVOKE ALL ON FUNCTION corporate_ap_guard() FROM PUBLIC;
REVOKE ALL ON FUNCTION corporate_aps_guard() FROM PUBLIC;
REVOKE ALL ON FUNCTION corporate_admin_space_exists(text) FROM PUBLIC;
REVOKE ALL ON FUNCTION corporate_admin_space_directory() FROM PUBLIC;
REVOKE ALL ON FUNCTION corporate_agent_projects_visible(text[]) FROM PUBLIC;
REVOKE ALL ON FUNCTION corporate_agent_project_rows(text[]) FROM PUBLIC;
REVOKE ALL ON FUNCTION corporate_agent_scope_rows(text,text,boolean) FROM PUBLIC;
REVOKE ALL ON FUNCTION corporate_mcp_project_scope(text,text) FROM PUBLIC;
REVOKE ALL ON FUNCTION corporate_user_project_scope(text,text) FROM PUBLIC;
REVOKE ALL ON FUNCTION corporate_reject_project_insert() FROM PUBLIC;

-- Assignable as the general space: any live space except a personal one (a single membership: its owner).
-- plpgsql: corporate_space_assignable is defined by the role_scope component, which runs after this one.
CREATE FUNCTION corporate_space_assignable_v2(space_key text) RETURNS boolean
LANGUAGE plpgsql STABLE SECURITY DEFINER SET search_path=pg_catalog,public AS $fn$
BEGIN
    IF NOT public.corporate_space_assignable(space_key) THEN RETURN false; END IF;
    IF space_key=public.corporate_general_space() THEN RETURN true; END IF;
    RETURN NOT (
        (SELECT count(*) FROM public.memberships m WHERE m.resource_type='space' AND m.resource_id=space_key)=1
        AND EXISTS (SELECT 1 FROM public.memberships m WHERE m.resource_type='space' AND m.resource_id=space_key
                      AND m.subject_type='user' AND m.role='owner'));
END
$fn$;
REVOKE ALL ON FUNCTION corporate_space_assignable_v2(text) FROM PUBLIC;
