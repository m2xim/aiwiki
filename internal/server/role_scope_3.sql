-- role_scope v3: pg_temp goes last in the search_path of every function that pins one.
--
-- Without an explicit pg_temp, PostgreSQL searches the session's temporary schema first for
-- relations and types. A runtime role with TEMP on the database could create pg_temp.text (a domain
-- whose CHECK calls its own function) and have it run as the owner inside any SECURITY DEFINER
-- function, or inside an invoker function called from one. Listing pg_temp last closes that.
-- Functions whose path is already safe are left alone; the startup guard refuses a function in
-- public that pins a search_path without pg_temp last.
DO $fix$
DECLARE f record; cfg text;
BEGIN
    FOR f IN SELECT p.oid::regprocedure AS sig, c AS cfg
               FROM pg_catalog.pg_proc p
               JOIN pg_catalog.pg_namespace n ON n.oid=p.pronamespace
               CROSS JOIN LATERAL unnest(p.proconfig) AS c
              WHERE n.nspname='public' AND c LIKE 'search_path=%'
                AND pg_catalog.pg_has_role(current_user,p.proowner,'MEMBER') LOOP
        cfg := substr(f.cfg, length('search_path=')+1);
        CONTINUE WHEN cfg ~ '(^|,\s*)pg_temp\s*$';
        EXECUTE format('ALTER FUNCTION %s SET search_path=%s, pg_temp', f.sig, cfg);
    END LOOP;
END
$fix$;
