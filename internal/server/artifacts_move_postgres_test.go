package server

import (
	"testing"
)

// Artifacts move together with their project (docs/MOVES.md, docs/ARTIFACTS.md).

type artRow struct {
	Space    string
	Revision int
	Updated  string
	Title    string
	Files    int
	Revs     int
}

func (e *artEnv) rowOf(id string) artRow {
	e.t.Helper()
	// The table owner is subject to FORCE RLS too, so the rows are read through a
	// test-only SECURITY DEFINER helper that uses the same probe predicate as the
	// production code (it only works when called by the runtime role).
	e.exec(`CREATE OR REPLACE FUNCTION test_artifact_row(art text, OUT sp text, OUT rev integer, OUT upd text, OUT ttl text, OUT fc integer, OUT revs bigint)
	LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,public,pg_temp AS $fn$
	BEGIN
		PERFORM set_config('app.artifact_probe','1',true);
		SELECT a.space_id,a.revision,a.updated_at,a.title,a.file_count,(SELECT count(*) FROM public.artifact_revisions v WHERE v.artifact_id=a.id) INTO sp,rev,upd,ttl,fc,revs FROM public.artifacts a WHERE a.id=art;
		PERFORM set_config('app.artifact_probe','',true);
	END
	$fn$`)
	e.exec(`GRANT EXECUTE ON FUNCTION test_artifact_row(text) TO PUBLIC`)
	var r artRow
	if err := e.root.DB.QueryRow(`SELECT sp,rev,upd,ttl,fc,revs FROM test_artifact_row(?)`, id).Scan(&r.Space, &r.Revision, &r.Updated, &r.Title, &r.Files, &r.Revs); err != nil {
		e.t.Fatal(err)
	}
	return r
}

func TestArtifactsPostgresSchemaV3Guard(t *testing.T) {
	e := newArtEnv(t)
	for _, c := range []struct{ break_, fix string }{
		{`ALTER TABLE projects DISABLE TRIGGER artifact_projects_follow_check`, `ALTER TABLE projects ENABLE TRIGGER artifact_projects_follow_check`},
		{`GRANT EXECUTE ON FUNCTION artifact_follow_project_move(text) TO PUBLIC`, `REVOKE ALL ON FUNCTION artifact_follow_project_move(text) FROM PUBLIC`},
		{`ALTER FUNCTION artifact_follow_project_move(text) SECURITY INVOKER`, `ALTER FUNCTION artifact_follow_project_move(text) SECURITY DEFINER`},
		{`ALTER FUNCTION artifact_follow_project_move(text) RESET search_path`, `ALTER FUNCTION artifact_follow_project_move(text) SET search_path=pg_catalog,public,pg_temp`},
		{`DROP POLICY artifacts_probe_update ON artifacts`, `CREATE POLICY artifacts_probe_update ON artifacts FOR UPDATE USING (current_user<>session_user AND current_setting('app.artifact_probe',true)='1') WITH CHECK (current_user<>session_user AND current_setting('app.artifact_probe',true)='1')`},
	} {
		e.exec(c.break_)
		if err := e.root.DB.validateRuntimeSchema(); err == nil {
			t.Errorf("guard missed: %s", c.break_)
		}
		e.exec(c.fix)
	}
	if err := e.root.DB.validateRuntimeSchema(); err != nil {
		t.Fatalf("after restoring: %v", err)
	}
}
