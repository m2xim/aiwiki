-- mcp_access v8: keys name agent projects instead of per-row scopes.
--  * mcp_credentials.agent_project_ids (1..20 project ids) and read_only are the authority of a key.
--    They change only through corporate_mcp_credential_set_projects(); the immutability trigger lets
--    the table owner (that SECURITY DEFINER function, a migration) change them, nobody else.
--  * mcp_credentials.scope / project_ids / mode stay as history (NULL on new keys, no longer authority).
--  * The session scope (app.mcp_scope) is computed per request from the project
--    (corporate_mcp_project_scope, defined with the agent_projects tables) and re-verified by
--    corporate_mcp_credential_audit on every request.
ALTER TABLE mcp_credentials ADD COLUMN agent_project_ids jsonb
    CHECK (agent_project_ids IS NULL OR (jsonb_typeof(agent_project_ids)='array' AND jsonb_array_length(agent_project_ids) BETWEEN 1 AND 20));
ALTER TABLE mcp_credentials ADD COLUMN read_only boolean NOT NULL DEFAULT false;
ALTER TABLE mcp_credentials ALTER COLUMN scope DROP NOT NULL;
ALTER TABLE mcp_credentials DROP CONSTRAINT mcp_credentials_scope_valid;
ALTER TABLE mcp_credentials ADD CONSTRAINT mcp_credentials_scope_valid CHECK (scope IS NULL OR corporate_mcp_scope_valid(scope));

CREATE OR REPLACE FUNCTION corporate_mcp_credential_immutable() RETURNS trigger
LANGUAGE plpgsql SET search_path=pg_catalog,public,pg_temp AS $fn$
BEGIN
 IF ROW(NEW.id,NEW.owner_id,NEW.service_account_id,NEW.name,NEW.secret_hash,NEW.created_at,NEW.expires_at)
  IS DISTINCT FROM ROW(OLD.id,OLD.owner_id,OLD.service_account_id,OLD.name,OLD.secret_hash,OLD.created_at,OLD.expires_at)
  OR (OLD.revoked_at IS NOT NULL AND NEW.revoked_at IS DISTINCT FROM OLD.revoked_at)
  OR (ROW(NEW.scope,NEW.project_ids,NEW.mode,NEW.agent_project_ids,NEW.read_only)
        IS DISTINCT FROM ROW(OLD.scope,OLD.project_ids,OLD.mode,OLD.agent_project_ids,OLD.read_only)
      AND (OLD.revoked_at IS NOT NULL
           OR current_user IS DISTINCT FROM (SELECT tableowner FROM pg_catalog.pg_tables
                                              WHERE schemaname='public' AND tablename='mcp_credentials'))) THEN
  RAISE EXCEPTION 'MCP credential authority is immutable';
 END IF;
 RETURN NEW;
END
$fn$;

-- The old per-row editor is gone: its rows now live in agent projects.
DROP FUNCTION corporate_mcp_credential_set_scope(text,jsonb);

-- The only way to change the projects or the read-only switch of a live key. The owner must see every
-- project that is new to the key (own or public); projects the key already lists may stay.
CREATE FUNCTION corporate_mcp_credential_set_projects(token_key text, new_projects jsonb, new_read_only boolean)
RETURNS void LANGUAGE plpgsql VOLATILE SECURITY DEFINER
SET search_path=pg_catalog,public,pg_temp AS $fn$
DECLARE owner_key text; grant_row public.mcp_credentials%ROWTYPE; pid text; n integer;
BEGIN
 owner_key:=nullif(current_setting('app.user_id',true),'');
 IF owner_key IS NULL OR public.corporate_mcp_scope_active() THEN
  RAISE EXCEPTION 'MCP key change requires a browser session';
 END IF;
 IF NOT EXISTS(SELECT 1 FROM public.users WHERE id=owner_key AND disabled=0) THEN
  RAISE EXCEPTION 'inactive owner';
 END IF;
 IF new_read_only IS NULL OR new_projects IS NULL OR jsonb_typeof(new_projects)<>'array' THEN
  RAISE EXCEPTION 'invalid MCP key projects';
 END IF;
 n:=jsonb_array_length(new_projects);
 IF n<1 OR n>20 OR EXISTS(SELECT 1 FROM jsonb_array_elements(new_projects) e WHERE jsonb_typeof(e)<>'string' OR e #>> '{}'='')
  OR (SELECT count(DISTINCT x) FROM jsonb_array_elements_text(new_projects) x)<>n THEN
  RAISE EXCEPTION 'invalid MCP key projects';
 END IF;
 SELECT * INTO grant_row FROM public.mcp_credentials
  WHERE id=token_key AND owner_id=owner_key AND revoked_at IS NULL AND expires_at>clock_timestamp()
  FOR UPDATE;
 IF NOT FOUND THEN
  RAISE EXCEPTION 'MCP credential not found';
 END IF;
 FOR pid IN SELECT jsonb_array_elements_text(new_projects) LOOP
  CONTINUE WHEN grant_row.agent_project_ids IS NOT NULL AND jsonb_exists(grant_row.agent_project_ids, pid);
  IF NOT EXISTS(SELECT 1 FROM public.agent_projects p WHERE p.id=pid AND (p.owner_id=owner_key OR p.visibility='public')) THEN
   RAISE EXCEPTION 'project is not visible to the key owner';
  END IF;
 END LOOP;
 UPDATE public.mcp_credentials SET agent_project_ids=new_projects, read_only=new_read_only WHERE id=token_key;
 INSERT INTO public.security_audit(actor_id,action,resource_type,resource_id,details,created_at)
 VALUES(owner_key,'mcp_credential.update','mcp_credential',token_key,
  jsonb_build_object('name',grant_row.name,'old_projects',grant_row.agent_project_ids,'projects',new_projects,
   'old_read_only',grant_row.read_only,'read_only',new_read_only),
  to_char(clock_timestamp() AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.MS"Z"'));
END
$fn$;
REVOKE ALL ON FUNCTION corporate_mcp_credential_set_projects(text,jsonb,boolean) FROM PUBLIC;

-- Per-request audit. Besides the key, the owner and the service account it re-derives the session scope
-- in SQL from the project the request named (app.mcp_project; empty = introspection) and refuses the request
-- unless the session carries exactly that scope: the project must be in the key's list, live and visible.
CREATE OR REPLACE FUNCTION corporate_mcp_credential_audit(token_key text, method_name text, tool_name text)
RETURNS void LANGUAGE plpgsql VOLATILE SECURITY DEFINER
SET search_path=pg_catalog,public,pg_temp AS $fn$
DECLARE owner_key text; actor_key text; grant_row public.mcp_credentials%ROWTYPE; project_key text; expected jsonb;
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
 IF NOT FOUND OR NOT EXISTS(SELECT 1 FROM public.users WHERE id=owner_key AND disabled=0) THEN
  RAISE EXCEPTION 'inactive MCP credential';
 END IF;
 project_key:=coalesce(current_setting('app.mcp_project',true),'');
 expected:=public.corporate_mcp_project_scope(token_key,project_key);
 IF expected IS NULL OR public.corporate_mcp_scope_json() IS NULL
  OR expected IS DISTINCT FROM public.corporate_mcp_scope_json()
  OR coalesce(current_setting('app.mcp_credential',true),'')<>token_key THEN
  RAISE EXCEPTION 'MCP project scope does not match the key';
 END IF;
 actor_key:=owner_key;
 IF grant_row.service_account_id<>'' THEN
  SELECT id INTO actor_key FROM public.mcp_service_accounts
   WHERE id=grant_row.service_account_id AND owner_id=owner_key AND disabled=0;
  IF NOT FOUND THEN RAISE EXCEPTION 'inactive MCP service account'; END IF;
 END IF;
 INSERT INTO public.security_audit(actor_id,action,resource_type,resource_id,details,created_at)
 VALUES(actor_key,'mcp.credential.request','mcp_credential',token_key,
  jsonb_build_object('owner_id',owner_key,'method',method_name,'tool',tool_name,'project',project_key),
  to_char(clock_timestamp() AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.MS"Z"'));
END
$fn$;
REVOKE ALL ON FUNCTION corporate_mcp_credential_audit(text,text,text) FROM PUBLIC;
