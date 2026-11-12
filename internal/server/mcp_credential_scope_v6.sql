-- Per-element key scope. mcp_credentials.scope is a list of 1..20 elements
-- {"type":"space"|"project","id":"...","mode":"read"|"write"} and is the only authority;
-- the legacy project_ids/mode columns stay for history (NULL on new keys). The session
-- carries the same array in app.mcp_scope (non-empty = scoped key); app.mcp_projects and
-- app.mcp_mode are no longer read.

-- Shape of a stored or session scope. Elements may repeat ids across types, never a pair.
CREATE OR REPLACE FUNCTION corporate_mcp_scope_valid(scope jsonb) RETURNS boolean
LANGUAGE sql IMMUTABLE SET search_path=pg_catalog,public AS $fn$
 SELECT scope IS NOT NULL AND jsonb_typeof(scope)='array'
    AND jsonb_array_length(scope) BETWEEN 1 AND 20
    AND NOT EXISTS (SELECT 1 FROM jsonb_array_elements(scope) AS e(value)
         WHERE jsonb_typeof(e.value)<>'object'
            OR jsonb_typeof(e.value->'id')<>'string' OR e.value->>'id'=''
            OR e.value->>'type' IS NULL OR e.value->>'type' NOT IN ('space','project')
            OR e.value->>'mode' IS NULL OR e.value->>'mode' NOT IN ('read','write'))
$fn$;

-- The session scope as validated jsonb, or NULL when absent or malformed (no access).
CREATE OR REPLACE FUNCTION corporate_mcp_scope_json() RETURNS jsonb
LANGUAGE plpgsql STABLE SET search_path=pg_catalog,public AS $fn$
DECLARE scope_text text; scope_json jsonb;
BEGIN
 scope_text := current_setting('app.mcp_scope',true);
 IF scope_text IS NULL OR scope_text='' THEN RETURN NULL; END IF;
 BEGIN scope_json := scope_text::jsonb;
 EXCEPTION WHEN invalid_text_representation THEN RETURN NULL; END;
 IF jsonb_typeof(scope_json)<>'array' OR jsonb_array_length(scope_json)<1 THEN RETURN NULL; END IF;
 RETURN scope_json;
END
$fn$;

CREATE OR REPLACE FUNCTION corporate_mcp_scope_active() RETURNS boolean
LANGUAGE sql STABLE SET search_path=pg_catalog AS $fn$
    -- app.mcp_projects is the pre-scope setting: a stale process that still sets it gets
    -- a scoped session with no valid scope (no access), never an unrestricted one.
    SELECT coalesce(current_setting('app.mcp_scope',true),'')<>'' OR coalesce(current_setting('app.mcp_projects',true),'')<>''
$fn$;

CREATE OR REPLACE FUNCTION corporate_mcp_content_write_allowed() RETURNS boolean
LANGUAGE sql STABLE SET search_path=pg_catalog,public AS $fn$
 SELECT NOT public.corporate_mcp_scope_active() OR EXISTS (
   SELECT 1 FROM jsonb_array_elements(coalesce(public.corporate_mcp_scope_json(),'[]'::jsonb)) AS e(value)
    WHERE jsonb_typeof(e.value)='object' AND e.value->>'mode'='write'
      AND e.value->>'type' IN ('space','project') AND jsonb_typeof(e.value->'id')='string')
$fn$;

ALTER TABLE mcp_credentials ADD COLUMN scope jsonb;
-- The immutability trigger of v5 only guards the legacy columns, so the owner role can backfill.
UPDATE mcp_credentials c SET scope=(
  SELECT jsonb_agg(jsonb_build_object('type','project','id',x.v,'mode',c.mode) ORDER BY x.o)
    FROM jsonb_array_elements_text(c.project_ids) WITH ORDINALITY AS x(v,o));
ALTER TABLE mcp_credentials ADD CONSTRAINT mcp_credentials_scope_valid CHECK (corporate_mcp_scope_valid(scope));
ALTER TABLE mcp_credentials ALTER COLUMN scope SET NOT NULL;
ALTER TABLE mcp_credentials ALTER COLUMN project_ids DROP NOT NULL;
ALTER TABLE mcp_credentials ALTER COLUMN mode DROP NOT NULL;

CREATE OR REPLACE FUNCTION corporate_mcp_credential_immutable() RETURNS trigger
LANGUAGE plpgsql SET search_path=pg_catalog,public AS $fn$
BEGIN
 IF ROW(NEW.id,NEW.owner_id,NEW.service_account_id,NEW.name,NEW.secret_hash,NEW.created_at,NEW.expires_at)
  IS DISTINCT FROM ROW(OLD.id,OLD.owner_id,OLD.service_account_id,OLD.name,OLD.secret_hash,OLD.created_at,OLD.expires_at)
  OR (OLD.revoked_at IS NOT NULL AND NEW.revoked_at IS DISTINCT FROM OLD.revoked_at)
  OR (ROW(NEW.scope,NEW.project_ids,NEW.mode) IS DISTINCT FROM ROW(OLD.scope,OLD.project_ids,OLD.mode)
      AND (OLD.revoked_at IS NOT NULL
           OR current_user IS DISTINCT FROM (SELECT tableowner FROM pg_catalog.pg_tables
                                              WHERE schemaname='public' AND tablename='mcp_credentials'))) THEN
  RAISE EXCEPTION 'MCP credential authority is immutable';
 END IF;
 RETURN NEW;
END
$fn$;

DROP FUNCTION corporate_mcp_credential_set_scope(text,jsonb,text);
CREATE FUNCTION corporate_mcp_credential_set_scope(token_key text, new_scope jsonb)
RETURNS void LANGUAGE plpgsql VOLATILE SECURITY DEFINER
SET search_path=pg_catalog,public AS $fn$
DECLARE owner_key text; grant_row public.mcp_credentials%ROWTYPE; el jsonb; space_key text; need integer;
BEGIN
 owner_key:=current_setting('app.user_id',true);
 IF owner_key IS NULL OR owner_key='' OR public.corporate_mcp_scope_active() THEN
  RAISE EXCEPTION 'MCP scope change requires a browser session';
 END IF;
 IF NOT EXISTS(SELECT 1 FROM public.users WHERE id=owner_key AND disabled=0) THEN
  RAISE EXCEPTION 'inactive owner';
 END IF;
 IF NOT public.corporate_mcp_scope_valid(new_scope)
  OR (SELECT count(DISTINCT (e.value->>'type', e.value->>'id')) FROM jsonb_array_elements(new_scope) e)<>jsonb_array_length(new_scope) THEN
  RAISE EXCEPTION 'invalid MCP scope';
 END IF;
 SELECT * INTO grant_row FROM public.mcp_credentials
  WHERE id=token_key AND owner_id=owner_key AND revoked_at IS NULL AND expires_at>clock_timestamp()
  FOR UPDATE;
 IF NOT FOUND THEN
  RAISE EXCEPTION 'MCP credential not found';
 END IF;
 FOR el IN SELECT value FROM jsonb_array_elements(new_scope) LOOP
  -- Everyone reads the general space: no check for a read element on it.
  CONTINUE WHEN el->>'type'='space' AND el->>'mode'='read' AND el->>'id'=public.corporate_general_space();
  need:=CASE el->>'mode' WHEN 'write' THEN 3 ELSE 1 END;
  IF el->>'type'='space' THEN
   IF NOT EXISTS (SELECT 1 FROM public.corporate_space_state st WHERE st.space_id=el->>'id' AND NOT st.archived)
    OR coalesce(public.corporate_role_rank(public.corporate_owner_effective_role(owner_key,el->>'id','')),0)<need THEN
    RAISE EXCEPTION 'insufficient rights for MCP scope';
   END IF;
  ELSE
   SELECT p.space_id INTO space_key FROM public.projects p
    WHERE p.id=el->>'id' AND public.corporate_container_writable(p.space_id,p.id);
   IF NOT FOUND OR coalesce(public.corporate_role_rank(
        public.corporate_owner_effective_role(owner_key,space_key,el->>'id')),0)<need THEN
    RAISE EXCEPTION 'insufficient rights for MCP scope';
   END IF;
  END IF;
 END LOOP;
 UPDATE public.mcp_credentials SET scope=new_scope, project_ids=NULL, mode=NULL WHERE id=token_key;
 INSERT INTO public.security_audit(actor_id,action,resource_type,resource_id,details,created_at)
 VALUES(owner_key,'mcp_credential.update','mcp_credential',token_key,
  jsonb_build_object('name',grant_row.name,'old_scope',grant_row.scope,'scope',new_scope),
  to_char(clock_timestamp() AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.MS"Z"'));
END
$fn$;
REVOKE ALL ON FUNCTION corporate_mcp_credential_set_scope(text,jsonb) FROM PUBLIC;

CREATE OR REPLACE FUNCTION corporate_mcp_credential_audit(token_key text, method_name text, tool_name text)
RETURNS void LANGUAGE plpgsql VOLATILE SECURITY DEFINER
SET search_path=pg_catalog,public AS $fn$
DECLARE owner_key text; actor_key text; grant_row public.mcp_credentials%ROWTYPE;
BEGIN
 owner_key:=current_setting('app.user_id',true);
 IF NOT public.corporate_mcp_scope_active() OR owner_key IS NULL OR owner_key='' THEN
  RAISE EXCEPTION 'missing MCP scope';
 END IF;
 IF method_name IS NULL OR char_length(method_name)>100 OR tool_name IS NULL OR char_length(tool_name)>100 THEN
  RAISE EXCEPTION 'invalid MCP audit metadata';
 END IF;
 SELECT * INTO grant_row FROM public.mcp_credentials
  WHERE id=token_key AND owner_id=owner_key AND revoked_at IS NULL AND expires_at>clock_timestamp();
 IF NOT FOUND OR public.corporate_mcp_scope_json() IS NULL
  OR grant_row.scope IS DISTINCT FROM public.corporate_mcp_scope_json()
  OR NOT EXISTS(SELECT 1 FROM public.users WHERE id=owner_key AND disabled=0) THEN
  RAISE EXCEPTION 'inactive MCP credential';
 END IF;
 actor_key:=owner_key;
 IF grant_row.service_account_id<>'' THEN
  SELECT id INTO actor_key FROM public.mcp_service_accounts
   WHERE id=grant_row.service_account_id AND owner_id=owner_key AND disabled=0;
  IF NOT FOUND THEN RAISE EXCEPTION 'inactive MCP service account'; END IF;
 END IF;
 INSERT INTO public.security_audit(actor_id,action,resource_type,resource_id,details,created_at)
 VALUES(actor_key,'mcp.credential.request','mcp_credential',token_key,
  jsonb_build_object('owner_id',owner_key,'method',method_name,'tool',tool_name),
  to_char(clock_timestamp() AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.MS"Z"'));
END
$fn$;
REVOKE ALL ON FUNCTION corporate_mcp_credential_audit(text,text,text) FROM PUBLIC;
REVOKE ALL ON FUNCTION corporate_mcp_scope_valid(jsonb) FROM PUBLIC;
REVOKE ALL ON FUNCTION corporate_mcp_scope_json() FROM PUBLIC;
