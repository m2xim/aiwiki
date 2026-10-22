package server

import (
	"strings"
	"testing"
	"time"
)

// strayInsert opens a transaction as user and inserts, without committing, an
// artifact into project (the way a concurrent MCP creation does: no lock on the project).
func (e *artEnv) strayInsert(user, space, project string) (*transaction, string) {
	e.t.Helper()
	tx, err := e.as(user).DB.Begin()
	if err != nil {
		e.t.Fatal(err)
	}
	id := newID()
	if _, err = tx.Exec(`INSERT INTO artifacts(id,space_id,project_id,title,owner_id,revision,author,created_at,updated_at) VALUES(?,?,?,'Гонка',?,1,?,'t','t')`, id, space, project, user, user); err != nil {
		e.t.Fatalf("stray insert: %v", err)
	}
	return tx, id
}

func (e *artEnv) artifactExists(id string) bool {
	e.t.Helper()
	var missing bool
	if err := e.root.DB.QueryRow(`SELECT sp IS NULL FROM test_artifact_row(?)`, id).Scan(&missing); err != nil {
		e.t.Fatal(err)
	}
	return !missing
}

func TestArtifactsPostgresCreateRacingWithProjectMove(t *testing.T) {
	e := newArtEnv(t)
	b := e.movesEnv.space("alice")
	e.exec(`CREATE OR REPLACE FUNCTION test_artifact_row(art text, OUT sp text, OUT rev integer, OUT upd text, OUT ttl text, OUT fc integer, OUT revs bigint)
	LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,public AS $fn$
	BEGIN
		PERFORM set_config('app.artifact_probe','1',true);
		SELECT a.space_id,a.revision,a.updated_at,a.title,a.file_count,(SELECT count(*) FROM public.artifact_revisions v WHERE v.artifact_id=a.id) INTO sp,rev,upd,ttl,fc,revs FROM public.artifacts a WHERE a.id=art;
		PERFORM set_config('app.artifact_probe','',true);
	END
	$fn$`)
	e.exec(`GRANT EXECUTE ON FUNCTION test_artifact_row(text) TO PUBLIC`)

	// Order 1: the creating transaction is open, the move commits first, the creation then fails.
	p1 := e.project(e.space, "", "Race1", true)
	tx, id := e.strayInsert("editor", e.space, p1)
	req := projectMove(p1, b, "")
	pv := e.call("alice", "/api/moves/preview", req)
	if r := e.call("alice", "/api/moves/apply", withFingerprint(req, pv.preview(t).Fingerprint)); r.Code != 200 {
		t.Fatalf("apply %d %s", r.Code, r.Raw)
	}
	if err := tx.Commit(); err == nil {
		t.Fatal("an artifact created in a project that moved meanwhile committed")
	}
	if e.artifactExists(id) {
		t.Fatal("stray artifact survived")
	}

	// Order 2: the creation commits while the move is still open (held by a lock on the
	// audit table, which apply writes after the artifacts were handled); the deferred
	// check then rolls the move back at commit and apply answers 409.
	p2 := e.project(e.space, "", "Race2", true)
	tx2, id2 := e.strayInsert("editor", e.space, p2)
	req2 := projectMove(p2, b, "")
	fp := e.call("alice", "/api/moves/preview", req2).preview(t).Fingerprint
	lock, err := e.owner.DB.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = lock.Exec(`LOCK TABLE security_audit IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}
	result := make(chan movesCall, 1)
	go func() { result <- e.call("alice", "/api/moves/apply", withFingerprint(req2, fp)) }()
	time.Sleep(800 * time.Millisecond)
	if err = tx2.Commit(); err != nil {
		t.Fatalf("creation before the move commit: %v", err)
	}
	lock.Rollback()
	r := <-result
	if r.Code != 409 || !strings.Contains(r.Raw, "concurrent change, retry") {
		t.Fatalf("apply after a concurrent creation: %d %s", r.Code, r.Raw)
	}
	var space string
	e.owner.DB.QueryRow(`SELECT space_id FROM projects WHERE id=?`, p2).Scan(&space)
	if space != e.space || e.rowOf(id2).Space != e.space {
		t.Fatalf("rolled back move left project %q artifact %q", space, e.rowOf(id2).Space)
	}
	// a retry moves the artifact too
	pv = e.call("alice", "/api/moves/preview", req2)
	if r := e.call("alice", "/api/moves/apply", withFingerprint(req2, pv.preview(t).Fingerprint)); r.Code != 200 || e.rowOf(id2).Space != b {
		t.Fatalf("retry %d %s", r.Code, r.Raw)
	}
}

func TestArtifactsPostgresProjectRowBypasses(t *testing.T) {
	e := newArtEnv(t)
	b := e.movesEnv.space("alice")
	proj := e.project(e.space, "", "Rewrite", true)
	a := e.create("alice", map[string]any{"project_id": proj})
	// the runtime role cannot delete projects at all
	if _, err := e.as("alice").DB.Exec(`DELETE FROM projects WHERE id=?`, proj); err == nil {
		t.Fatal("runtime role deleted a project")
	}
	// the owner role (trusted, not subject to the probe) deletes the row; the runtime role then
	// inserts it again in another space: the deferred check on INSERT refuses the commit
	e.exec(`DELETE FROM projects WHERE id=?`, proj)
	tx, err := e.as("alice").DB.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(`INSERT INTO projects(id,space_id,parent_id,name,inherit_acl) VALUES(?,?,'','Rewrite',1)`, proj, b); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err == nil {
		t.Fatal("project re-created in another space away from its artifacts")
	}
	e.exec(`INSERT INTO projects(id,space_id,parent_id,name,inherit_acl) VALUES(?,?,'','Rewrite',1)`, proj, e.space)
	var space string
	e.owner.DB.QueryRow(`SELECT space_id FROM projects WHERE id=?`, proj).Scan(&space)
	if space != e.space || e.rowOf(a.ID).Space != e.space {
		t.Fatalf("state after the refused rewrite: %q %q", space, e.rowOf(a.ID).Space)
	}
}

func TestArtifactsPostgresGuardRequiresProjectSpace(t *testing.T) {
	e := newArtEnv(t)
	b := e.movesEnv.space("alice")
	proj := e.project(e.space, "", "Guard", true)
	a := e.create("alice", map[string]any{"project_id": proj})
	e.exec(`CREATE OR REPLACE FUNCTION test_force_space(art text, sp text) RETURNS void
	LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,public AS $fn$
	BEGIN
		PERFORM set_config('app.artifact_probe','1',true);
		UPDATE public.artifacts SET space_id=sp WHERE id=art;
		PERFORM set_config('app.artifact_probe','',true);
	END
	$fn$`)
	e.exec(`GRANT EXECUTE ON FUNCTION test_force_space(text,text) TO PUBLIC`)
	tx, err := e.as("alice").DB.Begin()
	if err != nil {
		t.Fatal(err)
	}
	var v string
	tx.QueryRow(`SELECT set_config('app.move','project',true)`).Scan(&v)
	// probe and marker are satisfied, but the project still lives in the old space
	if _, err = tx.Exec(`SELECT test_force_space(?,?)`, a.ID, b); err == nil {
		t.Error("the guard allowed a container change away from the project's space")
	}
	tx.Rollback()
	if e.rowOf(a.ID).Space != e.space {
		t.Error("artifact moved")
	}
}

func TestArtifactsPostgresStrayWarningAndDepthBoundary(t *testing.T) {
	e := newArtEnv(t)
	b := e.movesEnv.space("alice")
	// a chain of exactly 64 projects: the deepest artifact moves with the root
	parent := ""
	var root, deepest string
	for i := 0; i < 64; i++ {
		parent = e.project(e.space, parent, "L", true)
		if i == 0 {
			root = parent
		}
		deepest = parent
	}
	a := e.create("alice", map[string]any{"project_id": deepest})
	req := projectMove(root, b, "")
	pv := e.call("alice", "/api/moves/preview", req)
	if pv.Code != 200 {
		t.Fatalf("preview %d %s", pv.Code, pv.Raw)
	}
	p := pv.preview(t)
	if p.Projects != 64 || p.Artifacts != 1 {
		t.Fatalf("preview %+v", p)
	}
	if r := e.call("alice", "/api/moves/apply", withFingerprint(req, p.Fingerprint)); r.Code != 200 {
		t.Fatalf("apply %d %s", r.Code, r.Raw)
	}
	if e.rowOf(a.ID).Space != b {
		t.Error("the artifact at the depth limit did not follow")
	}
	// the count-only startup probe sees an artifact planted outside its project's space
	proj := e.project(e.space, "", "Planted", true)
	e.create("alice", map[string]any{"project_id": proj})
	// the database owner (trusted) moves the project row behind the guards
	e.exec(`ALTER TABLE projects DISABLE TRIGGER USER`)
	e.exec(`UPDATE projects SET space_id=? WHERE id=?`, b, proj)
	e.exec(`ALTER TABLE projects ENABLE TRIGGER USER`)
	var n int
	if err := e.root.DB.QueryRow(`SELECT artifact_stray_count()`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("stray count %d %v", n, err)
	}
	e.root.DB.warnStrayArtifacts()
	e.exec(`ALTER TABLE projects DISABLE TRIGGER USER`)
	e.exec(`UPDATE projects SET space_id=? WHERE id=?`, e.space, proj)
	e.exec(`ALTER TABLE projects ENABLE TRIGGER USER`)
}

func TestArtifactsPostgresSchemaV4Guard(t *testing.T) {
	e := newArtEnv(t)
	var guardDef string
	if err := e.owner.DB.QueryRow(`SELECT pg_get_functiondef('public.artifact_guard()'::regprocedure)`).Scan(&guardDef); err != nil {
		t.Fatal(err)
	}
	probeCond := `current_user<>session_user AND current_setting('app.artifact_probe',true)='1'`
	follow := `CREATE CONSTRAINT TRIGGER artifact_projects_follow_check AFTER UPDATE OF space_id ON projects DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION artifact_guard_project_move()`
	rows := `CREATE CONSTRAINT TRIGGER artifact_rows_container_check AFTER INSERT OR UPDATE OF space_id, project_id ON artifacts DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION artifact_check_container()`
	for _, c := range []struct{ break_, fix string }{
		{`DROP TRIGGER artifact_projects_follow_check ON projects; CREATE CONSTRAINT TRIGGER artifact_projects_follow_check AFTER UPDATE OF space_id ON projects NOT DEFERRABLE FOR EACH ROW EXECUTE FUNCTION artifact_guard_project_move()`,
			`DROP TRIGGER artifact_projects_follow_check ON projects; ` + follow},
		{`DROP TRIGGER artifact_projects_follow_check ON projects; CREATE CONSTRAINT TRIGGER artifact_projects_follow_check AFTER UPDATE ON projects DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION artifact_guard_project_move()`,
			`DROP TRIGGER artifact_projects_follow_check ON projects; ` + follow},
		{`DROP TRIGGER artifact_rows_container_check ON artifacts; CREATE CONSTRAINT TRIGGER artifact_rows_container_check AFTER INSERT OR UPDATE OF space_id, project_id ON artifacts DEFERRABLE INITIALLY IMMEDIATE FOR EACH ROW EXECUTE FUNCTION artifact_check_container()`,
			`DROP TRIGGER artifact_rows_container_check ON artifacts; ` + rows},
		{`DROP TRIGGER artifact_rows_container_check ON artifacts; CREATE CONSTRAINT TRIGGER artifact_rows_container_check AFTER INSERT ON artifacts DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION artifact_check_container()`,
			`DROP TRIGGER artifact_rows_container_check ON artifacts; ` + rows},
		{`DROP TRIGGER artifact_projects_insert_check ON projects`,
			`CREATE CONSTRAINT TRIGGER artifact_projects_insert_check AFTER INSERT ON projects DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION artifact_guard_project_move()`},
		{`DROP TRIGGER artifact_projects_no_delete ON projects`,
			`CREATE TRIGGER artifact_projects_no_delete BEFORE DELETE ON projects FOR EACH ROW EXECUTE FUNCTION artifact_reject_project_delete()`},
		{`ALTER POLICY artifacts_probe_update ON artifacts USING (true)`,
			`ALTER POLICY artifacts_probe_update ON artifacts USING (` + probeCond + `)`},
		{`ALTER POLICY artifacts_probe_update ON artifacts WITH CHECK (true)`,
			`ALTER POLICY artifacts_probe_update ON artifacts WITH CHECK (` + probeCond + `)`},
		{`CREATE OR REPLACE FUNCTION artifact_guard() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog,public AS $f$ BEGIN RETURN NEW; END $f$`, guardDef},
	} {
		e.exec(c.break_)
		if err := e.root.DB.validateRuntimeSchema(); err == nil {
			t.Errorf("guard missed: %s", c.break_)
		}
		e.exec(c.fix)
		if err := e.root.DB.validateRuntimeSchema(); err != nil {
			t.Fatalf("after restoring %q: %v", c.break_, err)
		}
	}
	// the runtime role must not be a member of other roles
	var role string
	e.root.DB.QueryRow(`SELECT current_user`).Scan(&role)
	if _, err := e.owner.DB.Exec(`GRANT pg_read_all_stats TO ` + role); err == nil {
		if err := e.root.DB.validateRuntimeSchema(); err == nil {
			t.Error("guard missed a role membership of the runtime role")
		}
		e.exec(`REVOKE pg_read_all_stats FROM ` + role)
	} else {
		t.Logf("role membership case skipped: %v", err)
	}
	if err := e.root.DB.validateRuntimeSchema(); err != nil {
		t.Fatalf("final: %v", err)
	}
}
