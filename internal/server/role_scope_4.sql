-- role_scope v4: finish v3. Every function of the application owner (the owner of the tables) in public
-- that pins a search_path gets pg_temp last, whatever role runs the migration and however the path was
-- written (an empty path becomes pg_temp alone). Functions of other roles are left alone and reported by
-- the startup guard.
DO $fix$
DECLARE f record; cfg text;
BEGIN
    FOR f IN SELECT p.oid::regprocedure AS sig, c AS cfg
               FROM pg_catalog.pg_proc p
               JOIN pg_catalog.pg_namespace n ON n.oid=p.pronamespace
               CROSS JOIN LATERAL unnest(p.proconfig) AS c
              WHERE n.nspname='public' AND c LIKE 'search_path=%'
                AND c !~ '(=|,\s*)pg_temp\s*$'
                AND p.proowner=(SELECT t.relowner FROM pg_catalog.pg_class t WHERE t.oid='public.mcp_credentials'::regclass) LOOP
        cfg := btrim(substr(f.cfg, length('search_path=')+1));
        IF cfg='' OR cfg='""' THEN
            EXECUTE format('ALTER FUNCTION %s SET search_path=pg_temp', f.sig);
        ELSE
            EXECUTE format('ALTER FUNCTION %s SET search_path=%s, pg_temp', f.sig, cfg);
        END IF;
    END LOOP;
END
$fix$;
