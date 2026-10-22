-- The owner session limits statements to 30 s; the backfill parses every snapshot, so lift it for this transaction only.
SET LOCAL statement_timeout = 0;
SET LOCAL lock_timeout = '60s';

-- Real columns for the space/project of a revision snapshot: the read policy and the
-- audit query no longer parse every snapshot (whole articles) to filter by space.
ALTER TABLE revisions ADD COLUMN IF NOT EXISTS space_id text;
ALTER TABLE revisions ADD COLUMN IF NOT EXISTS project_id text NOT NULL DEFAULT '';

-- FORCE RLS plus the UPDATE policy USING (false) make a plain UPDATE touch 0 rows even
-- for the owner, so the backfill lifts FORCE for its own transaction only.
ALTER TABLE revisions NO FORCE ROW LEVEL SECURITY;
UPDATE revisions SET space_id = snapshot::jsonb->>'space_id', project_id = coalesce(snapshot::jsonb->>'project_id','');

-- Checked before FORCE returns: with FORCE on the owner sees no revisions and the check would pass vacuously.
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM revisions WHERE space_id IS NULL) THEN
    RAISE EXCEPTION 'revision_columns backfill left revisions without space_id';
  END IF;
END
$$;

ALTER TABLE revisions FORCE ROW LEVEL SECURITY;

-- Loud failure if the fill trigger ever goes missing (BEFORE triggers run before this check).
ALTER TABLE revisions ALTER COLUMN space_id SET NOT NULL;

-- Inserts stay unchanged in Go (and in the SQLite importer): the columns follow the snapshot.
CREATE OR REPLACE FUNCTION public.revisions_fill_scope() RETURNS trigger
LANGUAGE plpgsql
SET search_path = pg_catalog, public, pg_temp
AS $$
BEGIN
  NEW.space_id := NEW.snapshot::jsonb->>'space_id';
  NEW.project_id := coalesce(NEW.snapshot::jsonb->>'project_id','');
  RETURN NEW;
END
$$;

DROP TRIGGER IF EXISTS revisions_fill_scope ON revisions;
CREATE TRIGGER revisions_fill_scope BEFORE INSERT OR UPDATE OF snapshot ON revisions
FOR EACH ROW EXECUTE FUNCTION public.revisions_fill_scope();

CREATE INDEX IF NOT EXISTS revisions_space_idx ON revisions(space_id, id DESC);
DROP INDEX IF EXISTS revisions_snapshot_space;

-- Same rule as before (snapshot space/project, project defaulting to ''), read from the columns.
DROP POLICY IF EXISTS corporate_revisions_read ON revisions;
CREATE POLICY corporate_revisions_read ON revisions FOR SELECT USING (
    EXISTS (SELECT 1 FROM public.articles current_article
      WHERE current_article.id=revisions.article_id
        AND public.corporate_article_read_policy(current_article.space_id,current_article.project_id))
    AND public.corporate_article_read_policy(revisions.space_id, revisions.project_id)
);
