-- Interactive artifacts, component "artifacts" v1 (PostgreSQL, corporate mode).
-- Append-only on top of corporate_acl, mcp_access and corporate_moves.
-- See docs/ARTIFACTS.md ("Implementation notes") for the design.
CREATE TABLE artifacts (
    id text PRIMARY KEY,
    space_id text NOT NULL REFERENCES spaces(id),
    project_id text NOT NULL DEFAULT '',
    title text NOT NULL,
    description text NOT NULL DEFAULT '',
    visibility text NOT NULL DEFAULT 'inherit' CHECK (visibility IN ('inherit','private')),
    owner_id text NOT NULL,
    revision integer NOT NULL CHECK (revision >= 1),
    author text NOT NULL,
    created_at text NOT NULL,
    updated_at text NOT NULL,
    deleted_at text,
    file_count integer NOT NULL DEFAULT 0,
    projection text NOT NULL DEFAULT '{}',
    search_text text NOT NULL DEFAULT ''
);
CREATE INDEX artifacts_space_updated_idx ON artifacts(space_id,project_id,updated_at DESC);
CREATE INDEX artifacts_project_idx ON artifacts(project_id);
CREATE TABLE artifact_revisions (
    artifact_id text NOT NULL REFERENCES artifacts(id),
    revision integer NOT NULL CHECK (revision >= 1),
    author text NOT NULL,
    message text NOT NULL DEFAULT '',
    created_at text NOT NULL,
    manifest text NOT NULL,
    file_count integer NOT NULL DEFAULT 0,
    PRIMARY KEY (artifact_id,revision)
);
-- Content-addressed object metadata only (no user content): runtime can insert
-- but never read, update or delete it (no SELECT policy).
CREATE TABLE artifact_blobs (
    sha256 text PRIMARY KEY CHECK (sha256 ~ '^[0-9a-f]{64}$'),
    size bigint NOT NULL CHECK (size >= 0),
    mime text NOT NULL,
    stored_name text NOT NULL,
    created_at text NOT NULL
);
CREATE TABLE artifact_shares (
    id text PRIMARY KEY,
    artifact_id text NOT NULL REFERENCES artifacts(id),
    token_hash text NOT NULL UNIQUE CHECK (token_hash ~ '^[0-9a-f]{64}$'),
    revision integer,
    created_by text NOT NULL,
    created_at text NOT NULL,
    revoked_at text
);
CREATE INDEX artifact_shares_artifact_idx ON artifact_shares(artifact_id);
CREATE TABLE app_settings (
    key text PRIMARY KEY,
    value text NOT NULL
);

-- Administrator check as the table owner, so the policy never needs to read the
-- identity table as the runtime role (users has a separate MCP-aware policy).
CREATE FUNCTION artifact_is_admin(who text) RETURNS boolean
LANGUAGE sql STABLE SECURITY DEFINER SET search_path=pg_catalog,public AS $fn$
    SELECT EXISTS (SELECT 1 FROM public.users u WHERE u.id=who AND who IS NOT NULL AND who<>'' AND u.admin=1 AND u.disabled=0)
$fn$;

-- Role rank of the session user in a container (0 = none), evaluated with the
-- same corporate_effective_role chain as articles (MCP ceilings included).
CREATE FUNCTION artifact_role(space_key text, project_key text) RETURNS integer
LANGUAGE sql STABLE SET search_path=pg_catalog,public AS $fn$
    SELECT coalesce(public.corporate_role_rank(public.corporate_effective_role(
        nullif(current_setting('app.user_id',true),''),space_key,project_key)),0)
$fn$;

-- private: only the owner, and only while the owner still has container access;
-- container managers and administrators get no automatic access.
CREATE FUNCTION artifact_can_read(space_key text, project_key text, vis text, owner text) RETURNS boolean
LANGUAGE sql STABLE SET search_path=pg_catalog,public AS $fn$
    SELECT public.artifact_role(space_key,project_key) >= 1
       AND (vis='inherit' OR (owner<>'' AND owner=current_setting('app.user_id',true)))
$fn$;
CREATE FUNCTION artifact_can_write(space_key text, project_key text, vis text, owner text) RETURNS boolean
LANGUAGE sql STABLE SET search_path=pg_catalog,public AS $fn$
    SELECT public.artifact_role(space_key,project_key) >= 3
       AND public.corporate_container_writable(space_key,project_key)
       AND (vis='inherit' OR (owner<>'' AND owner=current_setting('app.user_id',true)))
$fn$;
CREATE FUNCTION artifact_can_share(space_key text, project_key text, vis text, owner text) RETURNS boolean
LANGUAGE sql STABLE SET search_path=pg_catalog,public AS $fn$
    SELECT public.artifact_can_write(space_key,project_key,vis,owner)
       AND ((owner<>'' AND owner=current_setting('app.user_id',true))
            OR (vis='inherit' AND public.artifact_role(space_key,project_key) >= 4))
$fn$;

ALTER TABLE artifacts ENABLE ROW LEVEL SECURITY;
ALTER TABLE artifacts FORCE ROW LEVEL SECURITY;
CREATE POLICY artifacts_read ON artifacts FOR SELECT
USING (public.artifact_can_read(space_id,project_id,visibility,owner_id));
-- A request that carries a verified render capability (set by the server only
-- after HMAC verification) may read exactly that live artifact.
CREATE POLICY artifacts_view_scope ON artifacts FOR SELECT
USING (deleted_at IS NULL AND id=nullif(current_setting('app.artifact_view',true),''));
-- Visible only to SECURITY DEFINER code (current_user differs from session_user)
-- that explicitly asks for it: the project-move guard below.
CREATE POLICY artifacts_probe ON artifacts FOR SELECT
USING (current_user<>session_user AND current_setting('app.artifact_probe',true)='1');
CREATE POLICY artifacts_insert ON artifacts FOR INSERT
WITH CHECK (public.artifact_role(space_id,project_id) >= 3
  AND public.corporate_container_writable(space_id,project_id)
  AND owner_id=current_setting('app.user_id',true)
  AND revision=1 AND deleted_at IS NULL);
CREATE POLICY artifacts_update ON artifacts FOR UPDATE
USING (public.artifact_can_write(space_id,project_id,visibility,owner_id))
WITH CHECK (public.artifact_role(space_id,project_id) >= 3
  AND public.corporate_container_writable(space_id,project_id)
  AND (visibility='inherit' OR owner_id=current_setting('app.user_id',true)));
CREATE POLICY mcp_scope_artifacts_insert ON artifacts AS RESTRICTIVE FOR INSERT
WITH CHECK (public.corporate_mcp_content_write_allowed());
CREATE POLICY mcp_scope_artifacts_update ON artifacts AS RESTRICTIVE FOR UPDATE
USING (public.corporate_mcp_content_write_allowed()) WITH CHECK (public.corporate_mcp_content_write_allowed());

CREATE FUNCTION artifact_guard() RETURNS trigger
LANGUAGE plpgsql SET search_path=pg_catalog,public AS $fn$
BEGIN
    IF NEW.id IS DISTINCT FROM OLD.id OR NEW.space_id IS DISTINCT FROM OLD.space_id
       OR NEW.project_id IS DISTINCT FROM OLD.project_id OR NEW.owner_id IS DISTINCT FROM OLD.owner_id
       OR NEW.created_at IS DISTINCT FROM OLD.created_at THEN
        RAISE EXCEPTION 'artifact identity, container and owner are immutable' USING ERRCODE='42501';
    END IF;
    IF NEW.revision IS DISTINCT FROM OLD.revision+1 THEN
        RAISE EXCEPTION 'artifact revision must advance by one' USING ERRCODE='23514';
    END IF;
    IF NEW.visibility IS DISTINCT FROM OLD.visibility THEN
        IF public.corporate_mcp_scope_active() THEN
            RAISE EXCEPTION 'MCP cannot change artifact visibility' USING ERRCODE='42501';
        END IF;
        -- A manager could not follow through anyway: the row would turn invisible to
        -- them (private artifacts are the owner's alone).
        IF OLD.owner_id IS DISTINCT FROM current_setting('app.user_id',true) THEN
            RAISE EXCEPTION 'only the owner can change visibility' USING ERRCODE='42501';
        END IF;
    END IF;
    IF NEW.deleted_at IS DISTINCT FROM OLD.deleted_at AND public.corporate_mcp_scope_active() THEN
        RAISE EXCEPTION 'MCP cannot delete or restore artifacts' USING ERRCODE='42501';
    END IF;
    RETURN NEW;
END
$fn$;
CREATE TRIGGER artifacts_guard BEFORE UPDATE ON artifacts
FOR EACH ROW EXECUTE FUNCTION artifact_guard();

ALTER TABLE artifact_revisions ENABLE ROW LEVEL SECURITY;
ALTER TABLE artifact_revisions FORCE ROW LEVEL SECURITY;
-- The container current for the artifact decides (artifacts are not moved).
CREATE POLICY artifact_revisions_read ON artifact_revisions FOR SELECT
USING (EXISTS (SELECT 1 FROM public.artifacts a WHERE a.id=artifact_revisions.artifact_id));
-- A revision row is written before the artifact row advances (so a visibility
-- change that hides the artifact from its manager can still be recorded), or
-- right after creation: revision is the artifact's current one or the next.
CREATE POLICY artifact_revisions_insert ON artifact_revisions FOR INSERT
WITH CHECK (EXISTS (SELECT 1 FROM public.artifacts a WHERE a.id=artifact_revisions.artifact_id
  AND artifact_revisions.revision IN (a.revision,a.revision+1)
  AND public.artifact_can_write(a.space_id,a.project_id,a.visibility,a.owner_id)));
CREATE POLICY mcp_scope_artifact_revisions_insert ON artifact_revisions AS RESTRICTIVE FOR INSERT
WITH CHECK (public.corporate_mcp_content_write_allowed());

ALTER TABLE artifact_blobs ENABLE ROW LEVEL SECURITY;
ALTER TABLE artifact_blobs FORCE ROW LEVEL SECURITY;
CREATE POLICY artifact_blobs_insert ON artifact_blobs FOR INSERT
WITH CHECK (nullif(current_setting('app.user_id',true),'') IS NOT NULL);
CREATE POLICY mcp_scope_artifact_blobs_insert ON artifact_blobs AS RESTRICTIVE FOR INSERT
WITH CHECK (public.corporate_mcp_content_write_allowed());

ALTER TABLE artifact_shares ENABLE ROW LEVEL SECURITY;
ALTER TABLE artifact_shares FORCE ROW LEVEL SECURITY;
CREATE POLICY artifact_shares_read ON artifact_shares FOR SELECT
USING (EXISTS (SELECT 1 FROM public.artifacts a WHERE a.id=artifact_shares.artifact_id
         AND public.artifact_can_share(a.space_id,a.project_id,a.visibility,a.owner_id)));
-- Public-link lookup: only by the hash (or id) the server derived from a token
-- or a verified capability; the hash is a SHA-256 of 32 random bytes.
CREATE POLICY artifact_shares_view_scope ON artifact_shares FOR SELECT
USING (token_hash=nullif(current_setting('app.share_token_hash',true),'')
    OR id=nullif(current_setting('app.share_id',true),''));
CREATE POLICY artifact_shares_insert ON artifact_shares FOR INSERT
WITH CHECK (created_by=current_setting('app.user_id',true) AND revoked_at IS NULL
  AND EXISTS (SELECT 1 FROM public.artifacts a WHERE a.id=artifact_shares.artifact_id
         AND public.artifact_can_share(a.space_id,a.project_id,a.visibility,a.owner_id)));
CREATE POLICY artifact_shares_update ON artifact_shares FOR UPDATE
USING (EXISTS (SELECT 1 FROM public.artifacts a WHERE a.id=artifact_shares.artifact_id
         AND public.artifact_can_share(a.space_id,a.project_id,a.visibility,a.owner_id)))
WITH CHECK (EXISTS (SELECT 1 FROM public.artifacts a WHERE a.id=artifact_shares.artifact_id
         AND public.artifact_can_share(a.space_id,a.project_id,a.visibility,a.owner_id)));
-- Soft-deleting an artifact revokes its links (see artifact_revoke_shares), which
-- an editor who cannot manage links must still trigger: only that trigger,
-- running as the table owner, passes this probe predicate.
CREATE POLICY artifact_shares_probe_read ON artifact_shares FOR SELECT
USING (current_user<>session_user AND current_setting('app.artifact_probe',true)='1');
CREATE POLICY artifact_shares_probe_update ON artifact_shares FOR UPDATE
USING (current_user<>session_user AND current_setting('app.artifact_probe',true)='1')
WITH CHECK (current_user<>session_user AND current_setting('app.artifact_probe',true)='1');
CREATE POLICY mcp_scope_artifact_shares_insert ON artifact_shares AS RESTRICTIVE FOR INSERT
WITH CHECK (NOT public.corporate_mcp_scope_active());
CREATE POLICY mcp_scope_artifact_shares_update ON artifact_shares AS RESTRICTIVE FOR UPDATE
USING (NOT public.corporate_mcp_scope_active()) WITH CHECK (NOT public.corporate_mcp_scope_active());

CREATE FUNCTION artifact_share_guard() RETURNS trigger
LANGUAGE plpgsql SET search_path=pg_catalog,public AS $fn$
BEGIN
    IF ROW(NEW.id,NEW.artifact_id,NEW.token_hash,NEW.revision,NEW.created_by,NEW.created_at)
       IS DISTINCT FROM ROW(OLD.id,OLD.artifact_id,OLD.token_hash,OLD.revision,OLD.created_by,OLD.created_at)
       OR (OLD.revoked_at IS NOT NULL AND NEW.revoked_at IS DISTINCT FROM OLD.revoked_at) THEN
        RAISE EXCEPTION 'a public link can only be revoked' USING ERRCODE='42501';
    END IF;
    RETURN NEW;
END
$fn$;
CREATE TRIGGER artifact_shares_guard BEFORE UPDATE ON artifact_shares
FOR EACH ROW EXECUTE FUNCTION artifact_share_guard();

CREATE FUNCTION artifact_revoke_shares() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,public AS $fn$
BEGIN
    PERFORM set_config('app.artifact_probe','1',true);
    UPDATE public.artifact_shares SET revoked_at=NEW.deleted_at WHERE artifact_id=NEW.id AND revoked_at IS NULL;
    PERFORM set_config('app.artifact_probe','',true);
    RETURN NULL;
END
$fn$;
CREATE TRIGGER artifacts_revoke_shares AFTER UPDATE OF deleted_at ON artifacts
FOR EACH ROW WHEN (OLD.deleted_at IS NULL AND NEW.deleted_at IS NOT NULL)
EXECUTE FUNCTION artifact_revoke_shares();

ALTER TABLE app_settings ENABLE ROW LEVEL SECURITY;
ALTER TABLE app_settings FORCE ROW LEVEL SECURITY;
CREATE POLICY app_settings_read ON app_settings FOR SELECT USING (true);
CREATE POLICY app_settings_insert ON app_settings FOR INSERT
WITH CHECK (public.artifact_is_admin(current_setting('app.user_id',true)) AND NOT public.corporate_mcp_scope_active());
CREATE POLICY app_settings_update ON app_settings FOR UPDATE
USING (public.artifact_is_admin(current_setting('app.user_id',true)) AND NOT public.corporate_mcp_scope_active())
WITH CHECK (public.artifact_is_admin(current_setting('app.user_id',true)) AND NOT public.corporate_mcp_scope_active());

-- Artifacts are not moved in this stage: a project holding artifacts (even
-- deleted or private ones the mover cannot see) cannot change its space.
CREATE FUNCTION artifact_guard_project_move() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,public AS $fn$
BEGIN
    IF NEW.space_id IS DISTINCT FROM OLD.space_id THEN
        PERFORM set_config('app.artifact_probe','1',true);
        IF EXISTS (SELECT 1 FROM public.artifacts WHERE project_id=OLD.id) THEN
            PERFORM set_config('app.artifact_probe','',true);
            RAISE EXCEPTION 'a project that contains artifacts cannot be moved to another space' USING ERRCODE='42501';
        END IF;
        PERFORM set_config('app.artifact_probe','',true);
    END IF;
    RETURN NEW;
END
$fn$;
CREATE TRIGGER artifact_projects_no_move BEFORE UPDATE OF space_id ON projects
FOR EACH ROW EXECUTE FUNCTION artifact_guard_project_move();

REVOKE ALL ON FUNCTION artifact_is_admin(text) FROM PUBLIC;
REVOKE ALL ON FUNCTION artifact_role(text,text) FROM PUBLIC;
REVOKE ALL ON FUNCTION artifact_can_read(text,text,text,text) FROM PUBLIC;
REVOKE ALL ON FUNCTION artifact_can_write(text,text,text,text) FROM PUBLIC;
REVOKE ALL ON FUNCTION artifact_can_share(text,text,text,text) FROM PUBLIC;
REVOKE ALL ON FUNCTION artifact_guard() FROM PUBLIC;
REVOKE ALL ON FUNCTION artifact_share_guard() FROM PUBLIC;
REVOKE ALL ON FUNCTION artifact_revoke_shares() FROM PUBLIC;
REVOKE ALL ON FUNCTION artifact_guard_project_move() FROM PUBLIC
