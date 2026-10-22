-- Interactive artifacts, component "artifacts" v3 (append-only; v1 and v2 are published).
-- Artifacts follow their project to another space.
--  * artifact_follow_project_move(root) moves, inside the project-move transaction,
--    the artifacts (inherit and private, deleted too) of the moved project subtree
--    to the space the projects already live in. It is the only code path allowed to
--    change the container of an artifact: SECURITY DEFINER, only with the
--    transaction marker app.move='project' for that root, target space read from
--    projects.space_id (never from an argument).
--  * artifact_guard still rejects every container change except the one made by
--    that function (probe predicate: current_user<>session_user AND
--    app.artifact_probe='1', plus app.move='project'). Such a change touches
--    nothing but space_id and keeps the revision: the content, the manifest and
--    the history do not change, so there is no new revision (the "revision+1 on
--    every change" rule is for changes of the artifact itself).
--  * the old refusal trigger is replaced by a deferred check at commit: a project
--    whose space_id changed must not leave an artifact in another space, however
--    the project row was updated.
CREATE POLICY artifacts_probe_update ON artifacts FOR UPDATE
USING (current_user<>session_user AND current_setting('app.artifact_probe',true)='1')
WITH CHECK (current_user<>session_user AND current_setting('app.artifact_probe',true)='1');

CREATE OR REPLACE FUNCTION artifact_guard() RETURNS trigger
LANGUAGE plpgsql SET search_path=pg_catalog,public AS $fn$
BEGIN
    IF NEW.space_id IS DISTINCT FROM OLD.space_id THEN
        IF current_user<>session_user AND current_setting('app.artifact_probe',true)='1'
           AND coalesce(current_setting('app.move',true),'')='project'
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

CREATE FUNCTION artifact_follow_project_move(root text) RETURNS integer
LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path=pg_catalog,public AS $fn$
DECLARE
    actor text := nullif(current_setting('app.user_id',true),'');
    moved integer;
    sp text;
BEGIN
    IF root IS NULL OR root='' OR actor IS NULL
       OR coalesce(current_setting('app.move',true),'')<>'project'
       OR coalesce(current_setting('app.move_root',true),'')<>root THEN
        RAISE EXCEPTION 'artifacts follow a project only inside a project move' USING ERRCODE='42501';
    END IF;
    SELECT p.space_id INTO sp FROM public.projects p WHERE p.id=root;
    IF NOT FOUND OR public.corporate_role_rank(public.corporate_effective_role(actor,sp,root))<3 THEN
        RAISE EXCEPTION 'artifacts follow a project only inside a project move' USING ERRCODE='42501';
    END IF;
    PERFORM set_config('app.artifact_probe','1',true);
    WITH RECURSIVE sub(id,path) AS (
        SELECT root,ARRAY[root]::text[]
        UNION ALL
        SELECT c.id,sub.path||c.id FROM public.projects c JOIN sub ON c.parent_id=sub.id
         WHERE NOT c.id=ANY(sub.path) AND cardinality(sub.path)<64
    )
    UPDATE public.artifacts a SET space_id=p.space_id
      FROM sub JOIN public.projects p ON p.id=sub.id
     WHERE a.project_id=sub.id AND a.space_id<>p.space_id;
    GET DIAGNOSTICS moved = ROW_COUNT;
    PERFORM set_config('app.artifact_probe','',true);
    RETURN moved;
END
$fn$;

-- Deferred consistency check (replaces the v1 refusal): after the transaction no
-- artifact may sit in a space different from its project's.
CREATE OR REPLACE FUNCTION artifact_guard_project_move() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,public AS $fn$
DECLARE
    stray boolean;
BEGIN
    PERFORM set_config('app.artifact_probe','1',true);
    SELECT EXISTS (SELECT 1 FROM public.artifacts a JOIN public.projects p ON p.id=a.project_id
                    WHERE a.project_id=NEW.id AND a.space_id<>p.space_id) INTO stray;
    PERFORM set_config('app.artifact_probe','',true);
    IF stray THEN
        RAISE EXCEPTION 'artifacts must move together with their project' USING ERRCODE='42501';
    END IF;
    RETURN NULL;
END
$fn$;
DROP TRIGGER artifact_projects_no_move ON projects;
CREATE CONSTRAINT TRIGGER artifact_projects_follow_check AFTER UPDATE OF space_id ON projects
DEFERRABLE INITIALLY DEFERRED
FOR EACH ROW EXECUTE FUNCTION artifact_guard_project_move();

REVOKE ALL ON FUNCTION artifact_follow_project_move(text) FROM PUBLIC;
REVOKE ALL ON FUNCTION artifact_guard_project_move() FROM PUBLIC
