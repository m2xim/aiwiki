-- Display name of a stored author id for read-time rendering. Authorship is stored as
-- the principal id (u_... for users, svc_... for MCP service accounts); the owner-only
-- RLS on mcp_service_accounts and the per-user view of users would hide other people's
-- names, so this SECURITY DEFINER function returns the name and nothing else. Unknown
-- ids give ''. Runtime EXECUTE comes from the grant that runs at every start.
CREATE OR REPLACE FUNCTION public.corporate_actor_name(actor text)
RETURNS text LANGUAGE sql STABLE SECURITY DEFINER
SET search_path=pg_catalog,public AS $fn$
 SELECT coalesce(CASE
   WHEN actor LIKE 'svc\_%' THEN (SELECT s.name FROM public.mcp_service_accounts s WHERE s.id=actor)
   WHEN actor LIKE 'u\_%' THEN (SELECT u.name FROM public.users u WHERE u.id=actor)
 END,'')
$fn$;
REVOKE ALL ON FUNCTION public.corporate_actor_name(text) FROM PUBLIC;
