-- Interactive artifacts, component "artifacts" v4 (append-only; v1-v3 are published).
-- Closes the remaining ways to leave an artifact in a space different from its
-- project's (there is no foreign key artifacts.project_id -> projects):
--  * a deferred constraint trigger on artifacts (insert, space/project change)
--    covers an artifact created concurrently with a project move (MCP sessions do
--    not share-lock the project): whichever transaction commits second is rolled
--    back with 42501;
--  * the project check also runs after INSERT of a project row (DELETE+INSERT
--    with another space), and the runtime role cannot delete projects at all
--    (the application never does);
--  * the container change allowed in artifact_guard additionally requires that the
--    new space is the space of the artifact's project.
CREATE OR REPLACE FUNCTION artifact_guard() RETURNS trigger
LANGUAGE plpgsql SET search_path=pg_catalog,public AS $fn$
BEGIN
    IF NEW.space_id IS DISTINCT FROM OLD.space_id THEN
        IF current_user<>session_user AND current_setting('app.artifact_probe',true)='1'
           AND coalesce(current_setting('app.move',true),'')='project'
           AND NEW.project_id<>'' AND NEW.space_id=(SELECT p.space_id FROM public.projects p WHERE p.id=NEW.project_id)
           AND ROW(NEW.id,NEW.project_id,NEW.title,NEW.description,NEW.visibility,NEW.owner_id,NEW.revision,NEW.author,NEW.created_at,NEW.updated_at,NEW.deleted_at,NEW.file_count,NEW.projection,NEW.search_text)
               IS NOT DISTINCT FROM
               ROW(OLD.id,OLD.project_id,OLD.title,OLD.description,OLD.visibility,OLD.owner_id,OLD.revision,OLD.author,OLD.created_at,OLD.updated_at,OLD.deleted_at,OLD.file_count,OLD.projection,OLD.search_text) THEN
            RETURN NEW;
        END IF;
        RAISE EXCEPTION 'artifact identity, container and owner are immutable' USING ERRCODE='42501';
    END IF;
    IF NEW.id IS DISTINCT FROM OLD.id
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

-- Deferred check of one artifact row, judged by its state at commit.
CREATE FUNCTION artifact_check_container() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,public AS $fn$
DECLARE
    stray boolean;
BEGIN
    PERFORM set_config('app.artifact_probe','1',true);
    SELECT EXISTS (SELECT 1 FROM public.artifacts a WHERE a.id=NEW.id AND a.project_id<>''
                    AND NOT EXISTS (SELECT 1 FROM public.projects p WHERE p.id=a.project_id AND p.space_id=a.space_id)) INTO stray;
    PERFORM set_config('app.artifact_probe','',true);
    IF stray THEN
        RAISE EXCEPTION 'an artifact must live in the space of its project' USING ERRCODE='42501';
    END IF;
    RETURN NULL;
END
$fn$;
CREATE CONSTRAINT TRIGGER artifact_rows_container_check AFTER INSERT OR UPDATE OF space_id, project_id ON artifacts
DEFERRABLE INITIALLY DEFERRED
FOR EACH ROW EXECUTE FUNCTION artifact_check_container();

-- A project row inserted (after a delete) with another space is checked as well.
CREATE CONSTRAINT TRIGGER artifact_projects_insert_check AFTER INSERT ON projects
DEFERRABLE INITIALLY DEFERRED
FOR EACH ROW EXECUTE FUNCTION artifact_guard_project_move();

-- Projects are never deleted by the application; only the owner role may.
CREATE FUNCTION artifact_reject_project_delete() RETURNS trigger
LANGUAGE plpgsql SET search_path=pg_catalog,public AS $fn$
BEGIN
    IF NOT pg_has_role(session_user,(SELECT c.relowner FROM pg_class c WHERE c.oid='public.projects'::regclass),'MEMBER') THEN
        RAISE EXCEPTION 'projects cannot be deleted' USING ERRCODE='42501';
    END IF;
    RETURN OLD;
END
$fn$;
CREATE TRIGGER artifact_projects_no_delete BEFORE DELETE ON projects
FOR EACH ROW EXECUTE FUNCTION artifact_reject_project_delete();

-- Count-only probe for the startup warning (artifacts outside their project's space).
CREATE FUNCTION artifact_stray_count() RETURNS bigint
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,public AS $fn$
DECLARE
    n bigint;
BEGIN
    PERFORM set_config('app.artifact_probe','1',true);
    SELECT count(*) INTO n FROM public.artifacts a WHERE a.project_id<>''
       AND NOT EXISTS (SELECT 1 FROM public.projects p WHERE p.id=a.project_id AND p.space_id=a.space_id);
    PERFORM set_config('app.artifact_probe','',true);
    RETURN n;
END
$fn$;

REVOKE ALL ON FUNCTION artifact_check_container() FROM PUBLIC;
REVOKE ALL ON FUNCTION artifact_reject_project_delete() FROM PUBLIC;
REVOKE ALL ON FUNCTION artifact_stray_count() FROM PUBLIC
