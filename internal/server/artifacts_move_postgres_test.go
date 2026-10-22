package server

import (
	"encoding/json"
	"strings"
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
	LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,public AS $fn$
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

func TestArtifactsPostgresProjectMoveCarriesArtifacts(t *testing.T) {
	e := newArtEnv(t)
	b := e.movesEnv.space("alice")
	top := e.project(e.space, "", "Top", true)
	sub := e.project(e.space, top, "Sub", true)
	stay := e.project(e.space, "", "Stays", true)
	art1 := e.create("alice", map[string]any{"project_id": top, "title": "Общий алисы"})
	art2 := e.create("editor", map[string]any{"project_id": top, "title": "Общий редактора"})
	art3 := e.create("editor", map[string]any{"project_id": sub, "visibility": "private", "title": "Личный"})
	art4 := e.create("alice", map[string]any{"project_id": sub, "title": "Удалённый"})
	if e.status("alice", "DELETE", "/api/artifacts/"+art4.ID, map[string]any{"base_revision": 1}) != 200 {
		t.Fatal("delete")
	}
	outside := e.create("alice", map[string]any{"project_id": stay, "title": "Вне проекта"})
	_, link1 := e.shareOf("alice", art1.ID)
	_, link2 := e.shareOf("editor", art2.ID)
	_, link3 := e.shareOf("editor", art3.ID)
	for _, l := range []string{link1, link2, link3} {
		if !e.linkWorks(l) {
			t.Fatal("link before the move")
		}
	}

	// preview: only what the actor sees (the private artifact of another user is not counted)
	req := projectMove(top, b, "")
	pv := e.call("alice", "/api/moves/preview", req)
	if pv.Code != 200 {
		t.Fatalf("preview %d %s", pv.Code, pv.Raw)
	}
	p := pv.preview(t)
	if p.Artifacts != 3 || p.Projects != 2 {
		t.Fatalf("preview counts %+v", p)
	}
	warned := false
	for _, w := range p.Warnings {
		warned = warned || strings.HasPrefix(w, "Артефакты проекта переедут вместе с ним.")
	}
	if !warned {
		t.Fatalf("no artifact warning: %v", p.Warnings)
	}
	if q := e.call("alice", "/api/moves/preview", projectMove(stay, b, "")).preview(t); q.Artifacts != 1 {
		t.Fatalf("stay preview %+v", q)
	}
	// moving inside one space does not touch artifacts and does not warn
	inSpace := e.call("alice", "/api/moves/preview", projectMove(sub, e.space, "")).preview(t)
	if inSpace.Artifacts != 0 {
		t.Fatalf("same-space preview counts artifacts: %+v", inSpace)
	}
	for _, w := range inSpace.Warnings {
		if strings.Contains(w, "Артефакты") {
			t.Fatalf("same-space warning %q", w)
		}
	}

	// a change of a visible artifact between preview and apply gives 409
	if code, raw := e.do("editor", "PUT", "/api/artifacts/"+art2.ID, map[string]any{"base_revision": 1, "title": "Правка"}); code != 200 {
		t.Fatalf("update %d %s", code, raw)
	}
	stale := e.call("alice", "/api/moves/apply", withFingerprint(req, p.Fingerprint))
	if stale.Code != 409 {
		t.Fatalf("apply with a stale preview: %d %s", stale.Code, stale.Raw)
	}
	if e.rowOf(art1.ID).Space != e.space {
		t.Fatal("a refused move changed an artifact")
	}
	p = e.call("alice", "/api/moves/preview", req).preview(t)
	ids := []string{art1.ID, art2.ID, art3.ID, art4.ID}
	before := map[string]artRow{}
	for _, id := range ids {
		before[id] = e.rowOf(id)
	}
	outsideBefore := e.rowOf(outside.ID)

	done := e.call("alice", "/api/moves/apply", withFingerprint(req, p.Fingerprint))
	if done.Code != 200 {
		t.Fatalf("apply %d %s", done.Code, done.Raw)
	}
	var res struct{ Artifacts, Projects int }
	if err := json.Unmarshal([]byte(done.Raw), &res); err != nil || res.Artifacts != 3 || res.Projects != 2 {
		t.Fatalf("result %s %v", done.Raw, err)
	}
	for _, id := range ids {
		after := e.rowOf(id)
		want := before[id]
		want.Space = b
		if after != want {
			t.Errorf("artifact %s: %+v, want %+v (only the space changes)", id, after, want)
		}
	}
	if e.rowOf(outside.ID) != outsideBefore {
		t.Error("an artifact outside the moved subtree changed")
	}
	// the audit event carries the real number, including the hidden private artifact
	var n int
	if err := e.owner.DB.QueryRow(`SELECT (details::jsonb->>'artifacts')::int FROM security_audit WHERE action='move.project' AND resource_id=?`, top).Scan(&n); err != nil || n != 4 {
		t.Fatalf("audit artifacts %d %v", n, err)
	}
	var raw string
	e.owner.DB.QueryRow(`SELECT details::text FROM security_audit WHERE action='move.project' AND resource_id=?`, top).Scan(&raw)
	for _, id := range ids {
		if strings.Contains(raw, id) {
			t.Error("audit names an artifact")
		}
	}

	// access follows the new container
	if e.status("alice", "GET", "/api/artifacts/"+art1.ID, nil) != 200 || e.status("alice", "GET", "/api/artifacts/"+art4.ID, nil) != 200 {
		t.Error("the owner of the new space does not see moved artifacts")
	}
	if e.status("editor", "GET", "/api/artifacts/"+art2.ID, nil) != 404 {
		t.Error("a user without rights in the new space sees the artifact")
	}
	if e.status("editor", "GET", "/api/artifacts/"+art3.ID, nil) != 404 {
		t.Error("the owner of a private artifact without rights in the new space sees it")
	}
	if !e.linkWorks(link1) {
		t.Error("the link of a creator who may share in the new place stopped working")
	}
	if e.linkWorks(link2) || e.linkWorks(link3) {
		t.Error("a link of a creator without rights in the new space works")
	}
	e.grant("space", b, "user", "editor", "reader")
	if e.status("editor", "GET", "/api/artifacts/"+art3.ID, nil) != 200 {
		t.Error("the owner of a private artifact lost it after getting access")
	}
	if !e.linkWorks(link3) {
		t.Error("a private link must work for an owner with read access")
	}
	if e.linkWorks(link2) {
		t.Error("an inherit link of a mere reader works")
	}
	e.grant("space", b, "user", "editor", "editor")
	if !e.linkWorks(link2) {
		t.Error("an inherit link must work again once the creator may share")
	}

	// the container of an artifact cannot be changed directly, whoever asks
	for _, user := range []string{"alice", "editor"} {
		tx, err := e.as(user).DB.Begin()
		if err != nil {
			t.Fatal(err)
		}
		var v string
		tx.QueryRow(`SELECT set_config('app.move','project',true)`).Scan(&v)
		tx.QueryRow(`SELECT set_config('app.move_root',?,true)`, top).Scan(&v)
		tx.QueryRow(`SELECT set_config('app.artifact_probe','1',true)`).Scan(&v)
		res, err := tx.Exec(`UPDATE artifacts SET space_id=? WHERE id=?`, e.space, art1.ID)
		if err == nil {
			if k, _ := res.RowsAffected(); k != 0 {
				t.Errorf("%s moved an artifact directly", user)
			}
		}
		tx.Rollback()
		if _, err := e.as(user).DB.Exec(`UPDATE artifacts SET space_id=? WHERE id=?`, e.space, art2.ID); err == nil {
			if e.rowOf(art2.ID).Space != b {
				t.Errorf("%s moved an artifact directly", user)
			}
		}
	}
	if r, err := e.owner.DB.Exec(`UPDATE artifacts SET space_id=? WHERE id=?`, e.space, art1.ID); err == nil {
		if k, _ := r.RowsAffected(); k != 0 {
			t.Error("the table owner moved an artifact outside the follow function")
		}
	}
	// the follow function refuses to run outside a project move
	if err := e.as("alice").DB.QueryRow(`SELECT artifact_follow_project_move(?)`, top).Scan(&n); err == nil {
		t.Error("follow function ran without the marker")
	}
	for _, id := range ids {
		if e.rowOf(id).Space != b {
			t.Errorf("artifact %s drifted", id)
		}
	}
}

func TestArtifactsPostgresProjectMoveBypassLeavesNoStrays(t *testing.T) {
	e := newArtEnv(t)
	b := e.movesEnv.space("alice")
	proj := e.project(e.space, "", "Bypass", true)
	a := e.create("alice", map[string]any{"project_id": proj})
	bypass := func(follow bool) error {
		tx, err := e.as("alice").DB.Begin()
		if err != nil {
			t.Fatal(err)
		}
		var v string
		tx.QueryRow(`SELECT set_config('app.move','project',true)`).Scan(&v)
		tx.QueryRow(`SELECT set_config('app.move_root',?,true)`, proj).Scan(&v)
		if _, err = tx.Exec(`UPDATE projects SET space_id=? WHERE id=?`, b, proj); err != nil {
			tx.Rollback()
			return err
		}
		if follow {
			var n int
			if err = tx.QueryRow(`SELECT artifact_follow_project_move(?)`, proj).Scan(&n); err != nil || n != 1 {
				tx.Rollback()
				t.Fatalf("follow: %d %v", n, err)
			}
		}
		return tx.Commit()
	}
	// only the project row: the commit is refused and nothing changes
	if err := bypass(false); err == nil {
		t.Fatal("a project moved away from its artifacts")
	}
	var space string
	e.owner.DB.QueryRow(`SELECT space_id FROM projects WHERE id=?`, proj).Scan(&space)
	if space != e.space || e.rowOf(a.ID).Space != e.space {
		t.Fatalf("state after the refused bypass: project %q artifact %q", space, e.rowOf(a.ID).Space)
	}
	// the same sequence with the follow function (what apply does) commits
	if err := bypass(true); err != nil {
		t.Fatalf("project and artifacts together: %v", err)
	}
	if e.rowOf(a.ID).Space != b {
		t.Error("artifact did not follow")
	}
}

func TestArtifactsPostgresSchemaV3Guard(t *testing.T) {
	e := newArtEnv(t)
	for _, c := range []struct{ break_, fix string }{
		{`ALTER TABLE projects DISABLE TRIGGER artifact_projects_follow_check`, `ALTER TABLE projects ENABLE TRIGGER artifact_projects_follow_check`},
		{`GRANT EXECUTE ON FUNCTION artifact_follow_project_move(text) TO PUBLIC`, `REVOKE ALL ON FUNCTION artifact_follow_project_move(text) FROM PUBLIC`},
		{`ALTER FUNCTION artifact_follow_project_move(text) SECURITY INVOKER`, `ALTER FUNCTION artifact_follow_project_move(text) SECURITY DEFINER`},
		{`ALTER FUNCTION artifact_follow_project_move(text) RESET search_path`, `ALTER FUNCTION artifact_follow_project_move(text) SET search_path=pg_catalog,public`},
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
