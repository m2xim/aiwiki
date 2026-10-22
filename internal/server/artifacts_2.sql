-- Interactive artifacts, component "artifacts" v2 (append-only; v1 is published).
--  * deleted artifacts are visible only to those who may restore them;
--  * listing and revoking public links no longer needs a writable container (an
--    archived container made links unmanageable) and works for the owner while
--    the owner can still read the container;
--  * a public link works only while its creator is an active user who still has
--    the right to share that artifact (checked by the server on every request).
CREATE FUNCTION artifact_can_view(space_key text, project_key text, vis text, owner text, deleted text) RETURNS boolean
LANGUAGE sql STABLE SET search_path=pg_catalog,public AS $fn$
    SELECT public.artifact_can_read(space_key,project_key,vis,owner)
       AND (deleted IS NULL OR public.artifact_role(space_key,project_key) >= 3)
$fn$;
DROP POLICY artifacts_read ON artifacts;
CREATE POLICY artifacts_read ON artifacts FOR SELECT
USING (public.artifact_can_view(space_id,project_id,visibility,owner_id,deleted_at));

-- Link management: the owner while the owner can read the container, and
-- managers of the container for inherit artifacts. No container-writable test.
CREATE FUNCTION artifact_can_manage_links(space_key text, project_key text, vis text, owner text) RETURNS boolean
LANGUAGE sql STABLE SET search_path=pg_catalog,public AS $fn$
    SELECT (owner<>'' AND owner=current_setting('app.user_id',true) AND public.artifact_role(space_key,project_key) >= 1)
        OR (vis='inherit' AND public.artifact_role(space_key,project_key) >= 4)
$fn$;
DROP POLICY artifact_shares_read ON artifact_shares;
CREATE POLICY artifact_shares_read ON artifact_shares FOR SELECT
USING (EXISTS (SELECT 1 FROM public.artifacts a WHERE a.id=artifact_shares.artifact_id
         AND public.artifact_can_manage_links(a.space_id,a.project_id,a.visibility,a.owner_id)));
DROP POLICY artifact_shares_update ON artifact_shares;
CREATE POLICY artifact_shares_update ON artifact_shares FOR UPDATE
USING (EXISTS (SELECT 1 FROM public.artifacts a WHERE a.id=artifact_shares.artifact_id
         AND public.artifact_can_manage_links(a.space_id,a.project_id,a.visibility,a.owner_id)))
WITH CHECK (EXISTS (SELECT 1 FROM public.artifacts a WHERE a.id=artifact_shares.artifact_id
         AND public.artifact_can_manage_links(a.space_id,a.project_id,a.visibility,a.owner_id)));

-- Does the creator of a link still have the right to share the artifact? True
-- or false only: no detail about why leaves the function.
CREATE FUNCTION artifact_link_valid(art text, creator text) RETURNS boolean
LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path=pg_catalog,public AS $fn$
DECLARE
    sp text; pr text; vis text; own text; rank integer;
BEGIN
    IF art IS NULL OR creator IS NULL OR creator='' THEN
        RETURN false;
    END IF;
    PERFORM set_config('app.artifact_probe','1',true);
    SELECT a.space_id,a.project_id,a.visibility,a.owner_id INTO sp,pr,vis,own FROM public.artifacts a WHERE a.id=art;
    PERFORM set_config('app.artifact_probe','',true);
    IF sp IS NULL THEN
        RETURN false;
    END IF;
    rank := public.corporate_role_rank(public.corporate_owner_effective_role(creator,sp,pr));
    IF vis='private' THEN
        RETURN creator=own AND rank >= 1;
    END IF;
    RETURN (creator=own AND rank >= 3) OR rank >= 4;
END
$fn$;

REVOKE ALL ON FUNCTION artifact_can_view(text,text,text,text,text) FROM PUBLIC;
REVOKE ALL ON FUNCTION artifact_can_manage_links(text,text,text,text) FROM PUBLIC;
REVOKE ALL ON FUNCTION artifact_link_valid(text,text) FROM PUBLIC
