-- Local admission for persistent MCP credentials: the time of the owner's last
-- successful interactive (browser) login. Credentials stop working when it is
-- older than the login window or missing. Existing users start with NULL, so
-- their credentials resume after their next login.
ALTER TABLE public.users ADD COLUMN IF NOT EXISTS last_login_at timestamptz;

-- Maximum credential lifetime 30 -> 180 days. The original CHECK is unnamed;
-- drop it by its definition, then add a named replacement.
DO $do$
DECLARE c record;
BEGIN
 FOR c IN SELECT conname FROM pg_constraint
  WHERE conrelid='public.mcp_credentials'::regclass AND contype='c'
   AND pg_get_constraintdef(oid) LIKE '%expires_at > created_at%'
 LOOP
  EXECUTE format('ALTER TABLE public.mcp_credentials DROP CONSTRAINT %I', c.conname);
 END LOOP;
END
$do$;
ALTER TABLE public.mcp_credentials ADD CONSTRAINT mcp_credentials_lifetime
 CHECK(expires_at>created_at AND expires_at<=created_at+interval '4320 hours');
