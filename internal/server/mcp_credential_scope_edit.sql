-- Projects and mode of a live credential may change, but only through
-- corporate_mcp_credential_set_scope(): a SECURITY DEFINER function that checks
-- the owner's current rights on the new scope itself. The trigger lets these
-- two columns change only when the update runs as the table owner (i.e. inside
-- that function, or a migration); the runtime role can never widen a scope
-- directly. Identity, secret, service account and times stay immutable for
-- everyone, and revocation stays final.
CREATE OR REPLACE FUNCTION corporate_mcp_credential_immutable() RETURNS trigger
LANGUAGE plpgsql SET search_path=pg_catalog,public AS $fn$
BEGIN
 IF ROW(NEW.id,NEW.owner_id,NEW.service_account_id,NEW.name,NEW.secret_hash,NEW.created_at,NEW.expires_at)
  IS DISTINCT FROM ROW(OLD.id,OLD.owner_id,OLD.service_account_id,OLD.name,OLD.secret_hash,OLD.created_at,OLD.expires_at)
  OR (OLD.revoked_at IS NOT NULL AND NEW.revoked_at IS DISTINCT FROM OLD.revoked_at)
  OR (ROW(NEW.project_ids,NEW.mode) IS DISTINCT FROM ROW(OLD.project_ids,OLD.mode)
      AND (OLD.revoked_at IS NOT NULL
           OR current_user IS DISTINCT FROM (SELECT tableowner FROM pg_catalog.pg_tables
                                              WHERE schemaname='public' AND tablename='mcp_credentials'))) THEN
  RAISE EXCEPTION 'MCP credential authority is immutable';
 END IF;
 RETURN NEW;
END
$fn$;

CREATE OR REPLACE FUNCTION corporate_mcp_credential_set_scope(token_key text, new_projects jsonb, new_mode text)
RETURNS void LANGUAGE plpgsql VOLATILE SECURITY DEFINER
SET search_path=pg_catalog,public AS $fn$
DECLARE owner_key text; grant_row public.mcp_credentials%ROWTYPE; pid text; space_key text; need integer; n integer;
BEGIN
 owner_key:=current_setting('app.user_id',true);
 IF owner_key IS NULL OR owner_key='' OR public.corporate_mcp_scope_active() THEN
  RAISE EXCEPTION 'MCP scope change requires a browser session';
 END IF;
 IF NOT EXISTS(SELECT 1 FROM public.users WHERE id=owner_key AND disabled=0) THEN
  RAISE EXCEPTION 'inactive owner';
 END IF;
 IF new_mode IS NULL OR new_mode NOT IN ('read','write') OR new_projects IS NULL
  OR jsonb_typeof(new_projects)<>'array' THEN
  RAISE EXCEPTION 'invalid MCP scope';
 END IF;
 n:=jsonb_array_length(new_projects);
 IF n<1 OR n>20 OR (SELECT count(DISTINCT x) FROM jsonb_array_elements_text(new_projects) x)<>n
  OR EXISTS(SELECT 1 FROM jsonb_array_elements(new_projects) e WHERE jsonb_typeof(e)<>'string') THEN
  RAISE EXCEPTION 'invalid MCP scope';
 END IF;
 SELECT * INTO grant_row FROM public.mcp_credentials
  WHERE id=token_key AND owner_id=owner_key AND revoked_at IS NULL AND expires_at>clock_timestamp()
  FOR UPDATE;
 IF NOT FOUND THEN
  RAISE EXCEPTION 'MCP credential not found';
 END IF;
 need:=CASE new_mode WHEN 'write' THEN 3 ELSE 1 END;
 FOR pid IN SELECT jsonb_array_elements_text(new_projects) LOOP
  SELECT p.space_id INTO space_key FROM public.projects p
   WHERE p.id=pid AND public.corporate_container_writable(p.space_id,p.id);
  IF NOT FOUND OR coalesce(public.corporate_role_rank(
       public.corporate_owner_effective_role(owner_key,space_key,pid)),0)<need THEN
   RAISE EXCEPTION 'insufficient rights for MCP scope';
  END IF;
 END LOOP;
 UPDATE public.mcp_credentials SET project_ids=new_projects, mode=new_mode WHERE id=token_key;
 INSERT INTO public.security_audit(actor_id,action,resource_type,resource_id,details,created_at)
 VALUES(owner_key,'mcp_credential.update','mcp_credential',token_key,
  jsonb_build_object('name',grant_row.name,'old_mode',grant_row.mode,'mode',new_mode,
   'old_project_ids',grant_row.project_ids,'project_ids',new_projects),
  to_char(clock_timestamp() AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.MS"Z"'));
END
$fn$;
REVOKE ALL ON FUNCTION corporate_mcp_credential_set_scope(text,jsonb,text) FROM PUBLIC;
