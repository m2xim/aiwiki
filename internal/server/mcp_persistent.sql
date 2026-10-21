-- Append-only migration: mcp_access v1 is already published.
CREATE TABLE mcp_service_accounts (
 id text PRIMARY KEY, owner_id text NOT NULL REFERENCES users(id),
 name text NOT NULL, disabled integer NOT NULL DEFAULT 0 CHECK(disabled IN (0,1)),
 created_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE mcp_credentials (
 id text PRIMARY KEY, owner_id text NOT NULL REFERENCES users(id),
 service_account_id text NOT NULL DEFAULT '', name text NOT NULL,
 secret_hash text NOT NULL UNIQUE CHECK(length(secret_hash)=64),
 project_ids jsonb NOT NULL CHECK(jsonb_typeof(project_ids)='array' AND jsonb_array_length(project_ids) BETWEEN 1 AND 20),
 mode text NOT NULL CHECK(mode IN ('read','write')),
 created_at timestamptz NOT NULL DEFAULT now(), expires_at timestamptz NOT NULL,
 revoked_at timestamptz, rate_window timestamptz, rate_requests integer NOT NULL DEFAULT 0,
 CHECK(expires_at>created_at AND expires_at<=created_at+interval '720 hours')
);
CREATE INDEX mcp_credentials_owner ON mcp_credentials(owner_id);
CREATE INDEX mcp_credentials_service ON mcp_credentials(service_account_id);
ALTER TABLE mcp_service_accounts ENABLE ROW LEVEL SECURITY;
ALTER TABLE mcp_service_accounts NO FORCE ROW LEVEL SECURITY;
CREATE POLICY mcp_scope_mcp_service_accounts_default ON mcp_service_accounts FOR ALL
 USING (NOT corporate_mcp_scope_active() AND (coalesce(current_setting('app.user_id',true),'')='' OR owner_id=current_setting('app.user_id',true)))
 WITH CHECK (NOT corporate_mcp_scope_active() AND (coalesce(current_setting('app.user_id',true),'')='' OR owner_id=current_setting('app.user_id',true)));
ALTER TABLE mcp_credentials ENABLE ROW LEVEL SECURITY;
ALTER TABLE mcp_credentials NO FORCE ROW LEVEL SECURITY;
CREATE POLICY mcp_scope_mcp_credentials_default ON mcp_credentials FOR ALL
 USING (NOT corporate_mcp_scope_active() AND (coalesce(current_setting('app.user_id',true),'')='' OR owner_id=current_setting('app.user_id',true)))
 WITH CHECK (NOT corporate_mcp_scope_active() AND (coalesce(current_setting('app.user_id',true),'')='' OR owner_id=current_setting('app.user_id',true)));
-- Retention is a migration-owner operation. Runtime cannot delete/reinsert a
-- revoked credential or disabled account to evade the monotonic update guards.
CREATE POLICY mcp_credentials_no_delete ON mcp_credentials AS RESTRICTIVE FOR DELETE USING(false);
CREATE POLICY mcp_service_accounts_no_delete ON mcp_service_accounts AS RESTRICTIVE FOR DELETE USING(false);

CREATE FUNCTION corporate_mcp_credential_immutable() RETURNS trigger
LANGUAGE plpgsql SET search_path=pg_catalog,public AS $fn$
BEGIN
 IF ROW(NEW.id,NEW.owner_id,NEW.service_account_id,NEW.name,NEW.secret_hash,NEW.project_ids,NEW.mode,NEW.created_at,NEW.expires_at)
  IS DISTINCT FROM ROW(OLD.id,OLD.owner_id,OLD.service_account_id,OLD.name,OLD.secret_hash,OLD.project_ids,OLD.mode,OLD.created_at,OLD.expires_at)
  OR (OLD.revoked_at IS NOT NULL AND NEW.revoked_at IS DISTINCT FROM OLD.revoked_at) THEN
  RAISE EXCEPTION 'MCP credential authority is immutable';
 END IF;
 RETURN NEW;
END
$fn$;
CREATE TRIGGER mcp_credential_immutable BEFORE UPDATE ON mcp_credentials
 FOR EACH ROW EXECUTE FUNCTION corporate_mcp_credential_immutable();
CREATE FUNCTION corporate_mcp_service_immutable() RETURNS trigger
LANGUAGE plpgsql SET search_path=pg_catalog,public AS $fn$
BEGIN
 IF ROW(NEW.id,NEW.owner_id,NEW.created_at) IS DISTINCT FROM ROW(OLD.id,OLD.owner_id,OLD.created_at)
  OR (OLD.disabled=1 AND NEW.disabled<>1) THEN
  RAISE EXCEPTION 'MCP service sponsor and disable are immutable';
 END IF;
 RETURN NEW;
END
$fn$;
CREATE TRIGGER mcp_service_immutable BEFORE UPDATE ON mcp_service_accounts
 FOR EACH ROW EXECUTE FUNCTION corporate_mcp_service_immutable();

CREATE OR REPLACE FUNCTION corporate_effective_role(who text, space_key text, project_key text)
RETURNS text LANGUAGE plpgsql STABLE SECURITY DEFINER
SET search_path=pg_catalog,public AS $fn$
DECLARE scope_text text; scope_json jsonb; base_role text; mode_text text;
BEGIN
 scope_text := current_setting('app.mcp_projects',true);
 IF scope_text IS NULL OR scope_text='' THEN
  RETURN public.corporate_owner_effective_role(who,space_key,project_key);
 END IF;
 mode_text := current_setting('app.mcp_mode',true);
 IF mode_text IS NULL OR mode_text NOT IN ('read','write')
    OR who IS DISTINCT FROM current_setting('app.user_id',true)
    OR project_key IS NULL OR project_key='' THEN RETURN NULL; END IF;
 BEGIN scope_json := scope_text::jsonb;
 EXCEPTION WHEN invalid_text_representation THEN RETURN NULL; END;
 IF jsonb_typeof(scope_json)<>'array' THEN RETURN NULL; END IF;
 IF NOT EXISTS (SELECT 1 FROM jsonb_array_elements(scope_json) AS p(value)
  WHERE jsonb_typeof(p.value)='string' AND p.value #>> '{}'=project_key) THEN RETURN NULL; END IF;
 base_role := public.corporate_owner_effective_role(who,space_key,project_key);
 IF mode_text='write' AND public.corporate_role_rank(base_role)>=3 THEN RETURN 'editor'; END IF;
 IF mode_text='write' AND public.corporate_role_rank(base_role)=2 THEN RETURN 'commenter'; END IF;
 IF public.corporate_role_rank(base_role)>=1 THEN RETURN 'reader'; END IF;
 RETURN NULL;
END
$fn$;

CREATE OR REPLACE FUNCTION corporate_mcp_content_write_allowed() RETURNS boolean
LANGUAGE sql STABLE SET search_path=pg_catalog,public AS $fn$
 SELECT NOT public.corporate_mcp_scope_active() OR current_setting('app.mcp_mode',true)='write'
$fn$;

-- Original per-container ACL/RLS policies still constrain each row. This layer
-- permits content operations only, never space, favorite or ACL administration.
DO $policies$
DECLARE target text; operation text;
BEGIN
 FOR target,operation IN SELECT * FROM (VALUES ('articles','insert'),('articles','update'),('revisions','insert'),('comments','insert'),('article_fts','insert'),('article_fts','delete')) AS needed(t,op) LOOP
  IF operation='insert' THEN
   EXECUTE format('ALTER POLICY %I ON public.%I WITH CHECK (public.corporate_mcp_content_write_allowed())','mcp_scope_'||target||'_insert',target);
  ELSIF operation='update' THEN
   EXECUTE format('ALTER POLICY %I ON public.%I USING (public.corporate_mcp_content_write_allowed()) WITH CHECK (public.corporate_mcp_content_write_allowed())','mcp_scope_'||target||'_update',target);
  ELSE
   EXECUTE format('ALTER POLICY %I ON public.%I USING (public.corporate_mcp_content_write_allowed())','mcp_scope_'||target||'_delete',target);
  END IF;
 END LOOP;
END
$policies$;

CREATE FUNCTION corporate_mcp_credential_audit(token_key text, method_name text, tool_name text)
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
 IF NOT FOUND OR grant_row.project_ids IS DISTINCT FROM current_setting('app.mcp_projects',true)::jsonb
  OR grant_row.mode IS DISTINCT FROM current_setting('app.mcp_mode',true)
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
