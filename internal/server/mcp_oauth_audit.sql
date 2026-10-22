-- Per-request audit for OAuth/bearer tokens issued by the identity provider.
-- The token itself never reaches the database: only the client id, method and
-- tool name are recorded. Runtime access is granted by the migration step that
-- grants EXECUTE on all functions; PUBLIC gets nothing.
CREATE OR REPLACE FUNCTION corporate_mcp_oauth_audit(client_id text, method_name text, tool_name text)
RETURNS void LANGUAGE plpgsql VOLATILE SECURITY DEFINER
SET search_path=pg_catalog,public AS $fn$
DECLARE actor_key text;
BEGIN
 actor_key:=current_setting('app.user_id',true);
 IF actor_key IS NULL OR actor_key='' THEN
  RAISE EXCEPTION 'missing MCP OAuth identity';
 END IF;
 IF client_id IS NULL OR char_length(client_id)<1 OR char_length(client_id)>100
  OR method_name IS NULL OR char_length(method_name)>100
  OR tool_name IS NULL OR char_length(tool_name)>100 THEN
  RAISE EXCEPTION 'invalid MCP OAuth audit metadata';
 END IF;
 IF NOT EXISTS(SELECT 1 FROM public.users WHERE id=actor_key AND disabled=0) THEN
  RAISE EXCEPTION 'inactive MCP OAuth user';
 END IF;
 INSERT INTO public.security_audit(actor_id,action,resource_type,resource_id,details,created_at)
 VALUES(actor_key,'mcp.oauth.request','mcp_oauth_client',client_id,
  jsonb_build_object('method',method_name,'tool',tool_name),
  to_char(clock_timestamp() AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.MS"Z"'));
END
$fn$;
REVOKE ALL ON FUNCTION corporate_mcp_oauth_audit(text,text,text) FROM PUBLIC;
