-- Per-space storage quotas, component "storage_quotas" v1 (append-only).
-- See docs/OPERATIONS.md section 5.
--
--  * spaces.storage_quota_bytes: NULL = server default (AIWIKI_SPACE_QUOTA_BYTES),
--    0 = unlimited. Only a system administrator changes it (trigger below; the
--    runtime role has no other way: managers could otherwise UPDATE their space).
--  * space_storage_objects(space) lists the distinct stored objects of a space:
--    attachments of its articles (deleted ones included), and for artifacts every
--    file hash once PER ARTIFACT across all its revisions (deleted artifacts
--    included). A hash is deliberately not shared between artifacts: crediting
--    content stored by another (possibly private) artifact would let an editor
--    test whether an exact file exists there. The function refuses to run unless
--    it is called from SECURITY DEFINER code (current_user <> session_user).
--  * space_storage_used / space_storage_override / space_storage_admit /
--    space_set_storage_quota are SECURITY DEFINER with a fixed search_path
--    (pg_temp explicitly last, catalog objects schema-qualified). They see rows
--    hidden by RLS through probe policies that only pass inside them (they set
--    app.quota_probe / app.artifact_probe for their own duration, and the
--    policies also require current_user <> session_user).
--  * usage is returned only for spaces where the caller is manager/owner, or to
--    a system administrator (and then only for an existing space);
--    space_storage_admit answers one boolean and needs an editor role in the
--    container; both return nothing / refuse otherwise.
ALTER TABLE spaces ADD COLUMN storage_quota_bytes bigint CHECK (storage_quota_bytes IS NULL OR storage_quota_bytes >= 0);

CREATE POLICY storage_quota_probe_spaces ON spaces FOR SELECT
USING (current_user<>session_user AND current_setting('app.quota_probe',true)='1');
CREATE POLICY storage_quota_probe_spaces_update ON spaces FOR UPDATE
USING (current_user<>session_user AND current_setting('app.quota_probe',true)='1')
WITH CHECK (current_user<>session_user AND current_setting('app.quota_probe',true)='1');
CREATE POLICY storage_quota_probe_articles ON articles FOR SELECT
USING (current_user<>session_user AND current_setting('app.quota_probe',true)='1');
CREATE POLICY storage_quota_probe_attachments ON attachments FOR SELECT
USING (current_user<>session_user AND current_setting('app.quota_probe',true)='1');

-- The quota changes only through space_set_storage_quota (administrators) or by
-- the table owner role (maintenance).
CREATE FUNCTION storage_quota_guard() RETURNS trigger
LANGUAGE plpgsql SET search_path=pg_catalog,public,pg_temp AS $fn$
BEGIN
    IF TG_OP='INSERT' THEN
        IF NEW.storage_quota_bytes IS NULL THEN
            RETURN NEW;
        END IF;
    ELSIF NEW.storage_quota_bytes IS NOT DISTINCT FROM OLD.storage_quota_bytes THEN
        RETURN NEW;
    END IF;
    IF current_user<>session_user AND pg_catalog.current_setting('app.quota_probe',true)='1' THEN
        RETURN NEW;
    END IF;
    IF pg_catalog.pg_has_role(session_user,(SELECT c.relowner FROM pg_catalog.pg_class c WHERE c.oid='public.spaces'::regclass),'MEMBER') THEN
        RETURN NEW;
    END IF;
    RAISE EXCEPTION 'only a system administrator changes the storage quota' USING ERRCODE='42501';
END
$fn$;
CREATE TRIGGER spaces_storage_quota_guard BEFORE INSERT OR UPDATE ON spaces
FOR EACH ROW EXECUTE FUNCTION storage_quota_guard();

CREATE FUNCTION space_storage_objects(space_key text) RETURNS TABLE(k text, sz bigint)
LANGUAGE plpgsql STABLE SET search_path=pg_catalog,public,pg_temp AS $fn$
BEGIN
    IF current_user=session_user THEN
        RAISE EXCEPTION 'access denied' USING ERRCODE='42501';
    END IF;
    RETURN QUERY
    SELECT 'a:'||at.stored_name, pg_catalog.max(at.size)::bigint
      FROM public.attachments at JOIN public.articles ar ON ar.id=at.article_id
     WHERE ar.space_id=space_key GROUP BY at.stored_name
    UNION ALL
    SELECT 'b:'||a.id||':'||(f.value->>'sha256'), pg_catalog.max((f.value->>'size')::bigint)
      FROM public.artifacts a
      JOIN public.artifact_revisions r ON r.artifact_id=a.id
      CROSS JOIN LATERAL pg_catalog.jsonb_array_elements(r.manifest::jsonb) AS f(value)
     WHERE a.space_id=space_key GROUP BY a.id, f.value->>'sha256';
END
$fn$;

CREATE FUNCTION space_storage_used(space_key text) RETURNS bigint
LANGUAGE plpgsql VOLATILE SECURITY DEFINER
SET search_path=pg_catalog,public,pg_temp AS $fn$
DECLARE
    actor text := nullif(pg_catalog.current_setting('app.user_id',true),'');
    total numeric;
BEGIN
    IF actor IS NULL OR space_key IS NULL OR space_key='' THEN
        RETURN NULL;
    END IF;
    IF NOT (public.corporate_role_rank(public.corporate_effective_role(actor,space_key,''))>=4
            OR (public.artifact_is_admin(actor) AND NOT public.corporate_mcp_scope_active())) THEN
        RETURN NULL;
    END IF;
    PERFORM pg_catalog.set_config('app.quota_probe','1',true);
    PERFORM pg_catalog.set_config('app.artifact_probe','1',true);
    IF NOT EXISTS (SELECT 1 FROM public.spaces s WHERE s.id=space_key) THEN
        PERFORM pg_catalog.set_config('app.quota_probe','',true);
        PERFORM pg_catalog.set_config('app.artifact_probe','',true);
        RETURN NULL;
    END IF;
    SELECT coalesce(pg_catalog.sum(o.sz),0) INTO total FROM public.space_storage_objects(space_key) o;
    PERFORM pg_catalog.set_config('app.quota_probe','',true);
    PERFORM pg_catalog.set_config('app.artifact_probe','',true);
    RETURN total;
END
$fn$;

-- The stored override (NULL = default) for the same audience as space_storage_used.
CREATE FUNCTION space_storage_override(space_key text) RETURNS bigint
LANGUAGE plpgsql VOLATILE SECURITY DEFINER
SET search_path=pg_catalog,public,pg_temp AS $fn$
DECLARE
    actor text := nullif(pg_catalog.current_setting('app.user_id',true),'');
    v bigint;
BEGIN
    IF actor IS NULL OR space_key IS NULL OR space_key='' THEN
        RETURN NULL;
    END IF;
    IF NOT (public.corporate_role_rank(public.corporate_effective_role(actor,space_key,''))>=4
            OR (public.artifact_is_admin(actor) AND NOT public.corporate_mcp_scope_active())) THEN
        RETURN NULL;
    END IF;
    PERFORM pg_catalog.set_config('app.quota_probe','1',true);
    SELECT s.storage_quota_bytes INTO v FROM public.spaces s WHERE s.id=space_key;
    PERFORM pg_catalog.set_config('app.quota_probe','',true);
    RETURN v;
END
$fn$;

-- True when adding the given objects keeps the space within its limit. The
-- advisory lock is keyed on the space and held until the end of the caller's
-- transaction, so the caller's insert, done in the same transaction, is seen by
-- the next admission. Only the objects not yet stored count.
-- add_keys: 'a:'||stored_name for an attachment (always new);
--           'b:'||artifact_id||':'||sha256 for an artifact file. Only a key already
--           stored in that same artifact is credited; a new artifact gets no credit.
CREATE FUNCTION space_storage_admit(space_key text, project_key text, default_limit bigint, add_keys text[], add_sizes bigint[]) RETURNS boolean
LANGUAGE plpgsql VOLATILE SECURITY DEFINER
SET search_path=pg_catalog,public,pg_temp AS $fn$
DECLARE
    actor text := nullif(pg_catalog.current_setting('app.user_id',true),'');
    lim bigint;
    used numeric;
    extra numeric;
BEGIN
    IF actor IS NULL OR space_key IS NULL OR space_key='' OR project_key IS NULL
       OR public.corporate_role_rank(public.corporate_effective_role(actor,space_key,project_key))<3 THEN
        RAISE EXCEPTION 'access denied' USING ERRCODE='42501';
    END IF;
    IF add_keys IS NULL OR add_sizes IS NULL OR pg_catalog.cardinality(add_keys)<>pg_catalog.cardinality(add_sizes)
       OR EXISTS (SELECT 1 FROM pg_catalog.unnest(add_sizes) AS z(v) WHERE z.v IS NULL OR z.v<0) THEN
        RAISE EXCEPTION 'invalid storage request' USING ERRCODE='22023';
    END IF;
    PERFORM pg_catalog.set_config('app.quota_probe','1',true);
    PERFORM pg_catalog.set_config('app.artifact_probe','1',true);
    SELECT s.storage_quota_bytes INTO lim FROM public.spaces s WHERE s.id=space_key;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'access denied' USING ERRCODE='42501';
    END IF;
    lim := coalesce(lim,default_limit,0);
    IF lim<=0 THEN
        PERFORM pg_catalog.set_config('app.quota_probe','',true);
        PERFORM pg_catalog.set_config('app.artifact_probe','',true);
        RETURN true;
    END IF;
    PERFORM pg_catalog.pg_advisory_xact_lock(pg_catalog.hashtextextended('aiwiki.space_quota:'||space_key,0));
    WITH o AS MATERIALIZED (SELECT x.k,x.sz FROM public.space_storage_objects(space_key) x),
         n AS (SELECT t.k,pg_catalog.max(t.sz) AS sz FROM unnest(add_keys,add_sizes) AS t(k,sz) GROUP BY t.k)
    SELECT (SELECT coalesce(pg_catalog.sum(o.sz),0) FROM o),
           (SELECT coalesce(pg_catalog.sum(n.sz),0) FROM n WHERE NOT EXISTS (SELECT 1 FROM o WHERE o.k=n.k))
      INTO used,extra;
    PERFORM pg_catalog.set_config('app.quota_probe','',true);
    PERFORM pg_catalog.set_config('app.artifact_probe','',true);
    RETURN used+extra<=lim;
END
$fn$;

-- Administrators only; a quota-only change needs no access to the space itself.
-- NULL restores the server default, 0 means unlimited.
CREATE FUNCTION space_set_storage_quota(space_key text, quota bigint) RETURNS boolean
LANGUAGE plpgsql VOLATILE SECURITY DEFINER
SET search_path=pg_catalog,public,pg_temp AS $fn$
DECLARE
    actor text := nullif(pg_catalog.current_setting('app.user_id',true),'');
    found_row boolean;
BEGIN
    IF actor IS NULL OR NOT public.artifact_is_admin(actor) OR public.corporate_mcp_scope_active() THEN
        RAISE EXCEPTION 'access denied' USING ERRCODE='42501';
    END IF;
    IF space_key IS NULL OR space_key='' OR (quota IS NOT NULL AND quota<0) THEN
        RAISE EXCEPTION 'invalid storage request' USING ERRCODE='22023';
    END IF;
    PERFORM pg_catalog.set_config('app.quota_probe','1',true);
    UPDATE public.spaces SET storage_quota_bytes=quota WHERE id=space_key;
    found_row := FOUND;
    PERFORM pg_catalog.set_config('app.quota_probe','',true);
    RETURN found_row;
END
$fn$;

REVOKE ALL ON FUNCTION storage_quota_guard() FROM PUBLIC;
REVOKE ALL ON FUNCTION space_storage_objects(text) FROM PUBLIC;
REVOKE ALL ON FUNCTION space_storage_used(text) FROM PUBLIC;
REVOKE ALL ON FUNCTION space_storage_override(text) FROM PUBLIC;
REVOKE ALL ON FUNCTION space_storage_admit(text,text,bigint,text[],bigint[]) FROM PUBLIC;
REVOKE ALL ON FUNCTION space_set_storage_quota(text,bigint) FROM PUBLIC
