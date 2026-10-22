package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// PostgreSQL integration of the artifact feature. These tests use the
// disposable database of corporateFixture and skip without it.

type artEnv struct {
	*movesEnv
	space string
	proj  string
	other string
}

func newArtEnv(t *testing.T) *artEnv {
	t.Helper()
	m := newMovesEnv(t)
	m.root.objects = newMemStore()
	e := &artEnv{movesEnv: m}
	e.space = m.space("alice") // alice owns it
	m.grant("space", e.space, "user", "bob", "manager")
	m.grant("space", e.space, "user", "editor", "editor")
	m.grant("space", e.space, "user", "reader", "reader")
	m.grant("space", e.space, "user", "commenter", "commenter")
	e.proj = m.project(e.space, "", "P1", true)
	e.other = m.project(e.space, "", "P2", true)
	return e
}

func (e *artEnv) do(user, method, path string, body any) (int, []byte) {
	e.t.Helper()
	var raw string
	switch v := body.(type) {
	case nil:
	case string:
		raw = v
	default:
		b, _ := json.Marshal(v)
		raw = string(b)
	}
	r := corporateHTTP(e.as(user), method, path, raw)
	return r.Code, r.Body.Bytes()
}

func (e *artEnv) create(user string, extra map[string]any) artifactDetail {
	e.t.Helper()
	body := map[string]any{"space_id": e.space, "title": "Артефакт", "files": basicFiles()}
	for k, v := range extra {
		body[k] = v
	}
	code, raw := e.do(user, "POST", "/api/artifacts", body)
	if code != 201 {
		e.t.Fatalf("create as %s: %d %s", user, code, raw)
	}
	var d artifactDetail
	decodeJSON(e.t, raw, &d)
	return d
}

func (e *artEnv) status(user, method, path string, body any) int {
	e.t.Helper()
	code, _ := e.do(user, method, path, body)
	return code
}

func (e *artEnv) count(user, query string, args ...any) int {
	e.t.Helper()
	var n int
	if err := e.as(user).DB.QueryRow(query, args...).Scan(&n); err != nil {
		e.t.Fatalf("%s: %v", query, err)
	}
	return n
}

func TestArtifactsPostgresRights(t *testing.T) {
	e := newArtEnv(t)
	a := e.create("alice", nil)
	if a.OwnerID != "alice" || !a.CanEdit || !a.CanShare || a.Author != "alice" {
		t.Fatalf("created: %+v", a)
	}
	path := "/api/artifacts/" + a.ID
	// readers read, never write
	code, raw := e.do("reader", "GET", path, nil)
	var seen artifactDetail
	decodeJSON(t, raw, &seen)
	if code != 200 || seen.CanEdit || seen.CanShare || seen.CanManage {
		t.Fatalf("reader view: %d %s", code, raw)
	}
	if e.status("reader", "PUT", path, map[string]any{"base_revision": 1, "title": "x"}) != 403 {
		t.Error("reader updated")
	}
	if e.status("reader", "DELETE", path, map[string]any{"base_revision": 1}) != 403 {
		t.Error("reader deleted")
	}
	if e.status("reader", "POST", "/api/artifacts", map[string]any{"space_id": e.space, "title": "t", "files": basicFiles()}) != 403 {
		t.Error("reader created")
	}
	if e.status("commenter", "PUT", path, map[string]any{"base_revision": 1, "title": "x"}) != 403 {
		t.Error("commenter updated")
	}
	if e.status("reader", "POST", path+"/view", nil) != 200 {
		t.Error("reader cannot view")
	}
	for _, m := range [][2]string{{"GET", "/shares"}, {"POST", "/shares"}} {
		if e.status("reader", m[0], path+m[1], map[string]any{}) != 403 {
			t.Errorf("reader %s %s", m[0], m[1])
		}
	}
	if e.status("alice", "GET", "/api/artifacts/nonexistent", nil) != 404 {
		t.Error("missing artifact")
	}
	// outsiders see nothing at all
	for _, u := range []string{"u1", "admin"} {
		if e.status(u, "GET", path, nil) != 404 || e.status(u, "POST", path+"/view", nil) != 404 || e.status(u, "GET", path+"/history", nil) != 404 {
			t.Errorf("%s reads", u)
		}
		if _, raw := e.do(u, "GET", "/api/artifacts", nil); strings.Contains(string(raw), a.ID) {
			t.Errorf("%s lists", u)
		}
		if e.status(u, "POST", "/api/artifacts", map[string]any{"space_id": e.space, "title": "t", "files": basicFiles()}) != 403 {
			t.Errorf("%s created", u)
		}
	}
	// editors write and delete; stale revisions conflict
	code, raw = e.do("editor", "PUT", path, map[string]any{"base_revision": 1, "title": "Правка редактора", "files": []fileSpec{{Path: "app.js", Content: "2"}}})
	if code != 200 {
		t.Fatalf("editor update: %d %s", code, raw)
	}
	var u artifactDetail
	decodeJSON(t, raw, &u)
	if u.Revision != 2 || u.Author != "editor" || u.OwnerID != "alice" {
		t.Fatalf("editor update: %+v", u)
	}
	if e.status("alice", "PUT", path, map[string]any{"base_revision": 1, "title": "stale"}) != 409 {
		t.Error("stale update")
	}
	// the editor cannot change visibility or manage links
	if e.status("editor", "PUT", path, map[string]any{"base_revision": 2, "visibility": "private"}) != 403 {
		t.Error("editor changed visibility")
	}
	if e.status("editor", "POST", path+"/shares", map[string]any{}) != 403 {
		t.Error("editor created link")
	}
	// a container manager creates links
	shareCode, shareRaw := e.do("bob", "POST", path+"/shares", map[string]any{})
	if shareCode != 201 {
		t.Fatalf("manager link: %d %s", shareCode, shareRaw)
	}
	var share struct{ ID, URL string }
	decodeJSON(t, shareRaw, &share)
	if e.status("editor", "DELETE", path+"/shares/"+share.ID, nil) != 403 {
		t.Error("editor revoked link")
	}
	// the editor's delete revokes every link, though the editor cannot manage them
	code, raw = e.do("editor", "DELETE", path, map[string]any{"base_revision": 2})
	if code != 200 {
		t.Fatalf("editor delete: %d %s", code, raw)
	}
	if c := corporateHTTP(e.root, "GET", share.URL, ""); c.Code != 404 {
		t.Errorf("link of a deleted artifact: %d", c.Code)
	}
	if e.status("editor", "POST", path+"/restore", map[string]any{"base_revision": 3}) != 200 {
		t.Error("editor restore")
	}
	if c := corporateHTTP(e.root, "GET", share.URL, ""); c.Code != 404 {
		t.Errorf("link revived by restore: %d", c.Code)
	}
	_, raw = e.do("bob", "GET", path+"/shares?include_revoked=true", nil)
	var shares []map[string]any
	decodeJSON(t, raw, &shares)
	if len(shares) != 1 || shares[0]["revoked_at"] == "" || shares[0]["revoked_at"] == nil {
		t.Errorf("shares after delete: %s", raw)
	}

	// private: only the owner, whatever the role
	p := e.create("editor", map[string]any{"visibility": "private", "title": "Личный"})
	pp := "/api/artifacts/" + p.ID
	for _, u := range []string{"alice", "bob", "reader", "admin", "commenter"} {
		if e.status(u, "GET", pp, nil) != 404 || e.status(u, "PUT", pp, map[string]any{"base_revision": 1, "title": "x"}) != 404 {
			t.Errorf("%s reaches a private artifact", u)
		}
		if _, raw := e.do(u, "GET", "/api/artifacts", nil); strings.Contains(string(raw), p.ID) {
			t.Errorf("%s lists a private artifact", u)
		}
		if _, raw := e.do(u, "GET", "/api/artifacts?q=%D0%BB%D0%B8%D1%87%D0%BD%D1%8B%D0%B9", nil); strings.Contains(string(raw), p.ID) {
			t.Errorf("%s finds a private artifact", u)
		}
		if e.status(u, "POST", pp+"/shares", map[string]any{}) != 404 {
			t.Errorf("%s shares a private artifact", u)
		}
		if e.status(u, "GET", pp+"/files/index.html", nil) != 404 {
			t.Errorf("%s reads a private file", u)
		}
	}
	if e.status("editor", "PUT", pp, map[string]any{"base_revision": 1, "title": "Личный 2"}) != 200 {
		t.Error("owner cannot update own private artifact")
	}
	if e.status("editor", "POST", pp+"/shares", map[string]any{}) != 201 {
		t.Error("owner cannot share")
	}
	// the owner losing access loses the private artifact too
	e.exec(`DELETE FROM memberships WHERE subject_id='editor' AND resource_id=?`, e.space)
	if e.status("editor", "GET", pp, nil) != 404 {
		t.Error("a former member still reads a private artifact")
	}
	e.grant("space", e.space, "user", "editor", "editor")

	// visibility: only the owner changes it (a manager would lose sight of the artifact)
	if c, r := e.do("bob", "PUT", path, map[string]any{"base_revision": 4, "visibility": "private"}); c != 403 {
		t.Fatalf("manager changed visibility: %d %s", c, r)
	}
	if e.status("alice", "PUT", path, map[string]any{"base_revision": 4, "visibility": "private"}) != 200 {
		t.Fatal("owner could not hide her artifact")
	}
	if e.status("editor", "GET", path, nil) != 404 || e.status("bob", "GET", path, nil) != 404 {
		t.Error("hidden artifact still visible")
	}
	if e.status("alice", "PUT", path, map[string]any{"base_revision": 5, "visibility": "inherit"}) != 200 {
		t.Error("owner could not restore visibility")
	}
	if e.status("editor", "GET", path, nil) != 200 {
		t.Error("restored artifact invisible")
	}

	// audit: events without content
	rows, err := e.owner.DB.Query(`SELECT action,actor_id,details::text FROM security_audit WHERE action LIKE 'artifact.%' ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	actions := map[string]int{}
	for rows.Next() {
		var action, actor, details string
		if err := rows.Scan(&action, &actor, &details); err != nil {
			t.Fatal(err)
		}
		actions[action]++
		if strings.Contains(details, "Привет") || strings.Contains(details, "window.x") || actor == "" {
			t.Errorf("audit leaks content: %s %s", action, details)
		}
	}
	for _, want := range []string{"artifact.create", "artifact.update", "artifact.delete", "artifact.restore", "artifact.visibility", "artifact.share.create"} {
		if actions[want] == 0 {
			t.Errorf("no %s audit event: %v", want, actions)
		}
	}
	if e.status("bob", "DELETE", path+"/shares/"+share.ID, nil) != 204 {
		t.Error("revoke of an already revoked link")
	}
}

func TestArtifactsPostgresLinksAndSettings(t *testing.T) {
	e := newArtEnv(t)
	a := e.create("alice", map[string]any{"title": "Публичный"})
	path := "/api/artifacts/" + a.ID
	code, raw := e.do("alice", "POST", path+"/shares", map[string]any{})
	if code != 201 {
		t.Fatalf("share: %d %s", code, raw)
	}
	var share struct{ ID, URL string }
	decodeJSON(t, raw, &share)
	page := corporateHTTP(e.root, "GET", share.URL, "")
	if page.Code != 200 || !strings.Contains(page.Body.String(), "Публичный") {
		t.Fatalf("public page: %d %s", page.Code, page.Body.String())
	}
	frame := publicFrame(t, e.root, page.Body.Bytes())
	fr := corporateHTTP(e.root, "GET", frame, "")
	if fr.Code != 200 || !strings.Contains(fr.Body.String(), "Привет") || fr.Header().Get("Content-Security-Policy") != exactFrameCSP {
		t.Fatalf("public frame: %d", fr.Code)
	}
	userFrame := func() string {
		code, raw := e.do("reader", "POST", path+"/view", nil)
		var v struct {
			FrameURL string `json:"frame_url"`
		}
		decodeJSON(t, raw, &v)
		if code != 200 {
			t.Fatalf("view: %d %s", code, raw)
		}
		return v.FrameURL
	}
	uf := userFrame()
	if c := corporateHTTP(e.root, "GET", uf, ""); c.Code != 200 {
		t.Fatalf("user frame without a session: %d", c.Code)
	}

	// only system administrators read and write the setting
	if e.status("admin", "GET", "/api/admin/settings", nil) != 200 {
		t.Error("admin cannot read settings")
	}
	for _, u := range []string{"alice", "bob"} {
		if e.status(u, "GET", "/api/admin/settings", nil) != 403 || e.status(u, "PUT", "/api/admin/settings", map[string]any{"public_artifact_links": false}) != 403 {
			t.Errorf("%s reached the settings", u)
		}
	}
	if _, err := e.as("alice").DB.Exec(`INSERT INTO app_settings(key,value) VALUES('public_artifact_links','false')`); err == nil {
		t.Error("a non-admin wrote app_settings directly")
	}
	code, raw = e.do("admin", "GET", "/api/admin/settings", nil)
	if !strings.Contains(string(raw), `"public_artifact_links":true`) {
		t.Errorf("default setting: %d %s", code, raw)
	}
	if e.status("admin", "PUT", "/api/admin/settings", `{"public_artifact_links":"no"}`) != 400 || e.status("admin", "PUT", "/api/admin/settings", `{}`) != 400 {
		t.Error("invalid setting accepted")
	}
	if e.status("admin", "PUT", "/api/admin/settings", map[string]any{"public_artifact_links": false}) != 200 {
		t.Fatal("admin could not disable links")
	}
	// disabled: page and the already issued capability stop, nothing is deleted
	if c := corporateHTTP(e.root, "GET", share.URL, ""); c.Code != 404 {
		t.Errorf("page while disabled: %d", c.Code)
	}
	if c := corporateHTTP(e.root, "GET", frame, ""); c.Code != 404 {
		t.Errorf("public capability while disabled: %d", c.Code)
	}
	if c := corporateHTTP(e.root, "GET", uf, ""); c.Code != 200 {
		t.Errorf("user capability must not depend on the setting: %d", c.Code)
	}
	if e.status("alice", "POST", path+"/shares", map[string]any{}) != 409 {
		t.Error("link created while disabled")
	}
	if _, raw := e.do("alice", "GET", path+"/shares", nil); !strings.Contains(string(raw), share.ID) {
		t.Error("link deleted by disabling")
	}
	if e.status("admin", "PUT", "/api/admin/settings", map[string]any{"public_artifact_links": true}) != 200 {
		t.Fatal("admin could not enable links")
	}
	if c := corporateHTTP(e.root, "GET", share.URL, ""); c.Code != 200 {
		t.Errorf("page after enabling: %d", c.Code)
	}
	if c := corporateHTTP(e.root, "GET", frame, ""); c.Code != 200 {
		t.Errorf("capability after enabling: %d", c.Code)
	}
	if e.status("alice", "DELETE", path+"/shares/"+share.ID, nil) != 204 {
		t.Fatal("revoke")
	}
	if c := corporateHTTP(e.root, "GET", frame, ""); c.Code != 404 {
		t.Errorf("revoked capability: %d", c.Code)
	}
	n := e.count("admin", `SELECT count(*) FROM security_audit WHERE action IN ('settings.update','artifact.share.revoke')`)
	if n != 3 {
		t.Errorf("audit rows %d", n)
	}
	// the user capability dies with the artifact
	if e.status("alice", "DELETE", path, map[string]any{"base_revision": 1}) != 200 {
		t.Fatal("delete")
	}
	if c := corporateHTTP(e.root, "GET", uf, ""); c.Code != 404 {
		t.Errorf("user capability of a deleted artifact: %d", c.Code)
	}
}

func TestArtifactsPostgresRowLevelSecurity(t *testing.T) {
	e := newArtEnv(t)
	pub := e.create("alice", map[string]any{"title": "Общий"})
	priv := e.create("editor", map[string]any{"title": "Личный редактора", "visibility": "private"})
	// a project closed to everybody but editor
	closed := e.project(e.space, "", "Closed", false)
	e.grant("project", closed, "user", "editor", "owner")
	hidden := e.create("editor", map[string]any{"project_id": closed, "title": "В закрытом проекте"})
	if e.status("alice", "GET", "/api/artifacts/"+hidden.ID, nil) != 404 {
		t.Error("space owner reads a closed project's artifact")
	}
	// direct SELECTs under the runtime role honour the policies
	visible := func(user string) map[string]bool {
		rows, err := e.as(user).DB.Query(`SELECT id FROM artifacts`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		out := map[string]bool{}
		for rows.Next() {
			var id string
			rows.Scan(&id)
			out[id] = true
		}
		return out
	}
	for user, want := range map[string][]string{
		"alice":  {pub.ID},
		"bob":    {pub.ID},
		"reader": {pub.ID},
		"editor": {pub.ID, priv.ID, hidden.ID},
		"admin":  {},
		"u1":     {},
	} {
		got := visible(user)
		if len(got) != len(want) {
			t.Errorf("%s sees %v, want %v", user, got, want)
		}
		for _, id := range want {
			if !got[id] {
				t.Errorf("%s misses %s", user, id)
			}
		}
	}
	for _, q := range []string{`SELECT count(*) FROM artifact_revisions`, `SELECT count(*) FROM artifact_shares`} {
		if e.count("u1", q) != 0 || e.count("admin", q) != 0 {
			t.Errorf("%s leaks to outsiders", q)
		}
	}
	if e.count("reader", `SELECT count(*) FROM artifact_revisions WHERE artifact_id=?`, priv.ID) != 0 || e.count("bob", `SELECT count(*) FROM artifact_revisions WHERE artifact_id=?`, priv.ID) != 0 {
		t.Error("revisions of a private artifact leak")
	}
	if e.count("reader", `SELECT count(*) FROM artifact_revisions WHERE artifact_id=?`, pub.ID) != 1 {
		t.Error("revisions of a visible artifact missing")
	}
	// metadata of blobs is not readable by anybody
	if _, err := e.as("alice").DB.Exec(`INSERT INTO artifact_blobs(sha256,size,mime,stored_name,created_at) VALUES(?,1,'text/plain','x','t') `, strings.Repeat("ab", 32)); err != nil {
		t.Errorf("blob insert: %v", err)
	}
	if e.count("alice", `SELECT count(*) FROM artifact_blobs`) != 0 || e.count("editor", `SELECT count(*) FROM artifact_blobs`) != 0 {
		t.Error("artifact_blobs is readable")
	}
	var ownerBlobs int
	if err := e.owner.DB.QueryRow(`SELECT count(*) FROM artifact_blobs`).Scan(&ownerBlobs); err != nil || ownerBlobs != 0 {
		t.Errorf("owner reads blobs: %d %v", ownerBlobs, err)
	}
	// direct writes are refused
	deny := func(name, user, q string, args ...any) {
		t.Helper()
		res, err := e.as(user).DB.Exec(q, args...)
		if err == nil {
			if n, _ := res.RowsAffected(); n != 0 {
				t.Errorf("%s: %s succeeded (%d rows)", name, user, n)
			}
		}
	}
	deny("reader insert", "reader", `INSERT INTO artifacts(id,space_id,project_id,title,owner_id,revision,author,created_at,updated_at) VALUES('x1',?,'','t','reader',1,'reader','t','t')`, e.space)
	deny("impersonated owner", "editor", `INSERT INTO artifacts(id,space_id,project_id,title,owner_id,revision,author,created_at,updated_at) VALUES('x2',?,'','t','alice',1,'editor','t','t')`, e.space)
	deny("owner change", "editor", `UPDATE artifacts SET owner_id='editor' WHERE id=?`, pub.ID)
	deny("move", "editor", `UPDATE artifacts SET space_id='other' WHERE id=?`, pub.ID)
	deny("visibility by editor", "editor", `UPDATE artifacts SET visibility='private',revision=revision+1 WHERE id=?`, pub.ID)
	deny("revision jump", "editor", `UPDATE artifacts SET revision=revision+5 WHERE id=?`, pub.ID)
	deny("reader update", "reader", `UPDATE artifacts SET title='x',revision=revision+1 WHERE id=?`, pub.ID)
	deny("manager reads private", "bob", `UPDATE artifacts SET title='x',revision=revision+1 WHERE id=?`, priv.ID)
	deny("hard delete", "alice", `DELETE FROM artifacts WHERE id=?`, pub.ID)
	deny("hard delete by owner", "editor", `DELETE FROM artifacts WHERE id=?`, priv.ID)
	deny("revision rewrite", "alice", `UPDATE artifact_revisions SET message='x' WHERE artifact_id=?`, pub.ID)
	deny("revision delete", "alice", `DELETE FROM artifact_revisions WHERE artifact_id=?`, pub.ID)
	deny("revision insert by reader", "reader", `INSERT INTO artifact_revisions(artifact_id,revision,author,message,created_at,manifest) VALUES(?,2,'reader','','t','[]')`, pub.ID)
	deny("share insert by reader", "reader", `INSERT INTO artifact_shares(id,artifact_id,token_hash,created_by,created_at) VALUES('s1',?,?, 'reader','t')`, pub.ID, strings.Repeat("cd", 32))
	deny("share insert by editor", "editor", `INSERT INTO artifact_shares(id,artifact_id,token_hash,created_by,created_at) VALUES('s2',?,?, 'editor','t')`, pub.ID, strings.Repeat("ce", 32))
	deny("settings insert", "reader", `INSERT INTO app_settings(key,value) VALUES('x','y')`)
	deny("settings delete", "admin", `DELETE FROM app_settings`)
	// a legitimate owner still reaches only revoke through shares
	if e.status("alice", "POST", "/api/artifacts/"+pub.ID+"/shares", map[string]any{}) != 201 {
		t.Fatal("share")
	}
	if _, err := e.as("alice").DB.Exec(`UPDATE artifact_shares SET revision=1 WHERE artifact_id=?`, pub.ID); err == nil {
		t.Error("share rewritten")
	}
	if _, err := e.as("alice").DB.Exec(`UPDATE artifact_shares SET created_by='x' WHERE artifact_id=?`, pub.ID); err == nil {
		t.Error("share author rewritten")
	}
	// the unauthenticated runtime session sees nothing; a view session sees exactly its artifact
	ctx := context.Background()
	for name, settings := range map[string]map[string]string{"anonymous": {}, "view": {"app.artifact_view": pub.ID}} {
		view, done, err := e.root.DB.scopedView(ctx, settings)
		if err != nil {
			t.Fatal(err)
		}
		var n, withRevs, deletedSeen int
		view.QueryRow(`SELECT count(*) FROM artifacts`).Scan(&n)
		view.QueryRow(`SELECT count(*) FROM artifact_revisions`).Scan(&withRevs)
		view.QueryRow(`SELECT count(*) FROM artifact_shares`).Scan(&deletedSeen)
		done()
		wantN := map[string]int{"anonymous": 0, "view": 1}[name]
		if n != wantN || withRevs != wantN || deletedSeen != 0 {
			t.Errorf("%s session sees %d artifacts, %d revisions, %d shares", name, n, withRevs, deletedSeen)
		}
	}
	// the scope does not survive on the pooled connection
	view, done, _ := e.root.DB.scopedView(ctx, map[string]string{"app.artifact_view": pub.ID})
	done()
	var n int
	if err := e.root.DB.QueryRow(`SELECT count(*) FROM artifacts`).Scan(&n); err != nil || n != 0 {
		t.Errorf("view scope leaked to the pool: %d %v", n, err)
	}
	_ = view
	// a deleted artifact leaves the view scope
	e.do("alice", "DELETE", "/api/artifacts/"+pub.ID, map[string]any{"base_revision": 1})
	view, done, _ = e.root.DB.scopedView(ctx, map[string]string{"app.artifact_view": pub.ID})
	view.QueryRow(`SELECT count(*) FROM artifacts`).Scan(&n)
	done()
	if n != 0 {
		t.Error("deleted artifact visible in a view scope")
	}
}

func TestArtifactsPostgresProjectMoveGuard(t *testing.T) {
	e := newArtEnv(t)
	b := e.movesEnv.space("alice")
	top := e.project(e.space, "", "Top", true)
	empty := e.project(e.space, "", "Empty", true)
	d := e.create("alice", map[string]any{"project_id": top})
	_ = d
	// a project without artifacts still moves
	pv := e.call("alice", "/api/moves/preview", projectMove(empty, b, ""))
	if pv.Code != 200 {
		t.Fatalf("preview: %d %s", pv.Code, pv.Raw)
	}
	if r := e.call("alice", "/api/moves/apply", withFingerprint(projectMove(empty, b, ""), pv.preview(t).Fingerprint)); r.Code != 200 {
		t.Fatalf("empty project move: %d %s", r.Code, r.Raw)
	}
	// a project holding an artifact (even a private one the mover cannot see) does not
	e.create("editor", map[string]any{"project_id": e.other, "visibility": "private"})
	for _, proj := range []string{top, e.other} {
		pv = e.call("alice", "/api/moves/preview", projectMove(proj, b, ""))
		if pv.Code != 200 {
			t.Fatalf("preview: %d %s", pv.Code, pv.Raw)
		}
		if r := e.call("alice", "/api/moves/apply", withFingerprint(projectMove(proj, b, ""), pv.preview(t).Fingerprint)); r.Code == 200 {
			t.Errorf("project %s with artifacts moved", proj)
		}
	}
	if e.status("alice", "GET", "/api/artifacts/"+d.ID, nil) != 200 {
		t.Error("artifact lost after refused move")
	}
}

func TestArtifactsPostgresSchemaGuard(t *testing.T) {
	e := newArtEnv(t)
	if err := e.root.DB.validateRuntimeSchema(); err != nil {
		t.Fatalf("schema: %v", err)
	}
	broken := []string{
		`ALTER TABLE artifacts NO FORCE ROW LEVEL SECURITY`,
		`DROP POLICY artifacts_update ON artifacts`,
		`CREATE POLICY extra_delete ON artifacts FOR DELETE USING (true)`,
		`ALTER TABLE artifact_shares DISABLE TRIGGER artifact_shares_guard`,
		`DROP POLICY app_settings_update ON app_settings`,
	}
	for _, stmt := range broken {
		e.exec(stmt)
		if err := e.root.DB.validateRuntimeSchema(); err == nil {
			t.Errorf("guard missed: %s", stmt)
		}
		e.exec(map[string]string{
			`ALTER TABLE artifacts NO FORCE ROW LEVEL SECURITY`:                 `ALTER TABLE artifacts FORCE ROW LEVEL SECURITY`,
			`DROP POLICY artifacts_update ON artifacts`:                         `CREATE POLICY artifacts_update ON artifacts FOR UPDATE USING (public.artifact_can_write(space_id,project_id,visibility,owner_id)) WITH CHECK (public.artifact_role(space_id,project_id) >= 3)`,
			`CREATE POLICY extra_delete ON artifacts FOR DELETE USING (true)`:   `DROP POLICY extra_delete ON artifacts`,
			`ALTER TABLE artifact_shares DISABLE TRIGGER artifact_shares_guard`: `ALTER TABLE artifact_shares ENABLE TRIGGER artifact_shares_guard`,
			`DROP POLICY app_settings_update ON app_settings`:                   `CREATE POLICY app_settings_update ON app_settings FOR UPDATE USING (public.artifact_is_admin(current_setting('app.user_id',true)) AND NOT public.corporate_mcp_scope_active()) WITH CHECK (public.artifact_is_admin(current_setting('app.user_id',true)) AND NOT public.corporate_mcp_scope_active())`,
		}[stmt])
	}
	if err := e.root.DB.validateRuntimeSchema(); err != nil {
		t.Fatalf("schema after restoring: %v", err)
	}
}

// mcpAs builds a request server for a persistent-key style scope.
func (e *artEnv) mcpAs(user, mode string, projects ...string) *Server {
	e.t.Helper()
	db, cleanup, err := e.root.DB.scopedMCP(context.Background(), user, projects, mode)
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(cleanup)
	cp := *e.root
	cp.DB = db
	cp.principal = &Principal{ID: user, Name: user, MCPProjects: projects, MCPMode: mode, MCPDelegationID: "pat_test"}
	return &cp
}

func mcpCall(s *Server, tool string, args map[string]any) (bool, string) {
	raw, _ := json.Marshal(args)
	params := map[string]json.RawMessage{"name": json.RawMessage(`"` + tool + `"`), "arguments": raw}
	res := s.callTool(params).(map[string]any)
	text := res["content"].([]any)[0].(map[string]string)["text"]
	return res["isError"] == true, text
}

func TestArtifactsPostgresMCPCeilings(t *testing.T) {
	e := newArtEnv(t)
	a1 := e.create("alice", map[string]any{"project_id": e.proj, "title": "В проекте один"})
	a2 := e.create("alice", map[string]any{"project_id": e.other, "title": "В проекте два"})
	a0 := e.create("alice", map[string]any{"title": "На уровне пространства"})
	ro := func() *Server { return e.mcpAs("alice", "read", e.proj) }
	rw := func() *Server { return e.mcpAs("alice", "write", e.proj) }

	// tool gating
	if !mcpScopedTool(ro().principal, "wiki_artifact_list") || !mcpScopedTool(ro().principal, "wiki_artifact_read") || mcpScopedTool(ro().principal, "wiki_artifact_write") {
		t.Error("read-only key tool set")
	}
	if !mcpScopedTool(rw().principal, "wiki_artifact_write") {
		t.Error("write key lacks wiki_artifact_write")
	}
	// scoped keys see only their project
	isErr, text := mcpCall(ro(), "wiki_artifact_list", map[string]any{})
	if isErr || !strings.Contains(text, a1.ID) || strings.Contains(text, a2.ID) || strings.Contains(text, a0.ID) {
		t.Fatalf("scoped list: %v %s", isErr, text)
	}
	for _, id := range []string{a2.ID, a0.ID} {
		if isErr, _ := mcpCall(ro(), "wiki_artifact_read", map[string]any{"id": id}); !isErr {
			t.Errorf("scoped key read %s", id)
		}
	}
	if isErr, text = mcpCall(ro(), "wiki_artifact_read", map[string]any{"id": a1.ID, "path": "index.html", "max_chars": 10}); isErr || !strings.Contains(text, `"has_more":true`) {
		t.Fatalf("scoped read: %v %s", isErr, text)
	}
	// a read-only key cannot write even by calling the function directly
	if isErr, _ = mcpCall(ro(), "wiki_artifact_write", map[string]any{"id": a1.ID, "base_revision": 1, "title": "x"}); !isErr {
		t.Error("read-only key wrote through the tool function")
	}
	if code, raw := e.do("alice", "GET", "/api/artifacts/"+a1.ID, nil); code != 200 || strings.Contains(string(raw), `"revision":2`) {
		t.Fatalf("read-only key changed the artifact: %s", raw)
	}
	// write key: own project only, current revisions only, no visibility
	isErr, text = mcpCall(rw(), "wiki_artifact_write", map[string]any{"id": a1.ID, "base_revision": 1, "files": []map[string]any{{"path": "extra.txt", "content": "новый файл"}}})
	if isErr {
		t.Fatalf("write key update: %s", text)
	}
	if isErr, text = mcpCall(rw(), "wiki_artifact_write", map[string]any{"id": a1.ID, "base_revision": 1, "title": "stale"}); !isErr || !strings.Contains(text, "revision conflict") {
		t.Errorf("stale revision: %v %s", isErr, text)
	}
	for _, id := range []string{a2.ID, a0.ID} {
		if isErr, _ = mcpCall(rw(), "wiki_artifact_write", map[string]any{"id": id, "base_revision": 1, "title": "x"}); !isErr {
			t.Errorf("write key changed %s outside its scope", id)
		}
	}
	if isErr, _ = mcpCall(rw(), "wiki_artifact_write", map[string]any{"space_id": e.space, "title": "в пространстве", "files": []map[string]any{{"path": "index.html", "content": "x"}}}); !isErr {
		t.Error("write key created at space level")
	}
	if isErr, _ = mcpCall(rw(), "wiki_artifact_write", map[string]any{"space_id": e.space, "project_id": e.other, "title": "чужой проект", "files": []map[string]any{{"path": "index.html", "content": "x"}}}); !isErr {
		t.Error("write key created in another project")
	}
	isErr, text = mcpCall(rw(), "wiki_artifact_write", map[string]any{"space_id": e.space, "project_id": e.proj, "title": "Новый из MCP", "files": []map[string]any{{"path": "index.html", "content": "<p>из mcp</p>"}}})
	if isErr {
		t.Fatalf("create in scope: %s", text)
	}
	if isErr, _ = mcpCall(rw(), "wiki_artifact_write", map[string]any{"id": a1.ID, "base_revision": 2, "visibility": "private"}); !isErr {
		t.Error("MCP changed visibility")
	}
	if code, _ := e.do("alice", "GET", "/api/artifacts/"+a1.ID, nil); code != 200 {
		t.Fatal("alice lost her artifact")
	}
	// HTTP-level ceilings for the same principal: no links, no delete, no restore
	for _, c := range []struct{ method, path, body string }{{"POST", "/shares", `{}`}, {"DELETE", "", `{"base_revision":2}`}} {
		rr := corporateHTTP(rw(), c.method, "/api/artifacts/"+a1.ID+c.path, c.body)
		if rr.Code < 400 {
			t.Errorf("MCP principal %s %s: %d", c.method, c.path, rr.Code)
		}
	}
	// the other persistent mode for a private artifact of the sponsor behaves as the sponsor
	priv := e.create("alice", map[string]any{"project_id": e.proj, "title": "Приватный в проекте", "visibility": "private"})
	if isErr, text = mcpCall(ro(), "wiki_artifact_list", map[string]any{}); !strings.Contains(text, priv.ID) {
		t.Errorf("sponsor's private artifact hidden from the sponsor's own key: %s", text)
	}
	other := e.mcpAs("bob", "read", e.proj)
	if _, text = mcpCall(other, "wiki_artifact_list", map[string]any{}); strings.Contains(text, priv.ID) {
		t.Errorf("another user's key sees a private artifact: %s", text)
	}
}

func TestArtifactsPostgresConcurrentRevision(t *testing.T) {
	e := newArtEnv(t)
	a := e.create("alice", nil)
	codes := make(chan int, 4)
	for i := 0; i < 4; i++ {
		go func(i int) {
			u := []string{"alice", "editor"}[i%2]
			cp := corporateAs(t, e.root, u)
			r := corporateHTTP(cp, "PUT", "/api/artifacts/"+a.ID, `{"base_revision":1,"files":[{"path":"app.js","content":"w`+string(rune('0'+i))+`"}]}`)
			codes <- r.Code
		}(i)
	}
	ok, conflict := 0, 0
	for i := 0; i < 4; i++ {
		switch <-codes {
		case 200:
			ok++
		case 409:
			conflict++
		}
	}
	if ok != 1 || conflict != 3 {
		t.Fatalf("want one success and three conflicts, got %d/%d", ok, conflict)
	}
}

// The full middleware: session cookie and CSRF for the API, none for the frame.
func TestArtifactsPostgresOIDCEndToEnd(t *testing.T) {
	_, s := corporateFixture(t)
	s.objects = newMemStore()
	x := newAuthHarness(t, nil)
	x.a.users = authSQLUsers{db: s.DB}
	s.auth = x.a
	cookie := authCookie(x.login(authUserGrant("employee", "aiwiki-access")), x.a.sessionCookie)
	call := func(method, path, body, csrf string, c *http.Cookie) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, "http://127.0.0.1:8090"+path, strings.NewReader(body))
		if c != nil {
			req.AddCookie(c)
		}
		if csrf != "" {
			req.Header.Set("X-CSRF-Token", csrf)
		}
		rr := httptest.NewRecorder()
		s.Handler().ServeHTTP(rr, req)
		return rr
	}
	var me struct {
		CSRF string `json:"csrf_token"`
	}
	json.Unmarshal(call("GET", "/api/me", "", "", cookie).Body.Bytes(), &me)
	var space Space
	json.Unmarshal(call("POST", "/api/corporate/spaces", `{"name":"Mine","personal":true}`, me.CSRF, cookie).Body.Bytes(), &space)
	body := `{"space_id":"` + space.ID + `","title":"E2E","files":[{"path":"index.html","content":"<p>e2e</p>"}]}`
	if rr := call("GET", "/api/artifacts", "", "", nil); rr.Code != 401 {
		t.Fatalf("anonymous list: %d", rr.Code)
	}
	if rr := call("POST", "/api/artifacts", body, "", cookie); rr.Code != 403 {
		t.Fatalf("create without CSRF: %d", rr.Code)
	}
	rr := call("POST", "/api/artifacts", body, me.CSRF, cookie)
	if rr.Code != 201 {
		t.Fatalf("create: %d %s", rr.Code, rr.Body.String())
	}
	var d artifactDetail
	json.Unmarshal(rr.Body.Bytes(), &d)
	if rr := call("POST", "/api/artifacts/"+d.ID+"/view", "", "", cookie); rr.Code != 403 {
		t.Fatalf("view without CSRF: %d", rr.Code)
	}
	rr = call("POST", "/api/artifacts/"+d.ID+"/view", "", me.CSRF, cookie)
	var v struct {
		FrameURL string `json:"frame_url"`
	}
	json.Unmarshal(rr.Body.Bytes(), &v)
	if rr.Code != 200 || v.FrameURL == "" {
		t.Fatalf("view: %d %s", rr.Code, rr.Body.String())
	}
	// the frame needs neither cookie nor CSRF, and gets none
	fr := call("GET", v.FrameURL, "", "", nil)
	if fr.Code != 200 || !strings.Contains(fr.Body.String(), "e2e") || fr.Header().Get("Set-Cookie") != "" {
		t.Fatalf("frame: %d", fr.Code)
	}
	// an artifact of another user is invisible; their frame URL is not guessable from ours
	other := authCookie(x.login(authUserGrant("stranger", "aiwiki-access")), x.a.sessionCookie)
	if rr := call("GET", "/api/artifacts/"+d.ID, "", "", other); rr.Code != 404 {
		t.Fatalf("stranger reads: %d", rr.Code)
	}
	// the public link works without a session
	shareRR := call("POST", "/api/artifacts/"+d.ID+"/shares", `{}`, me.CSRF, cookie)
	var sh struct{ URL string }
	json.Unmarshal(shareRR.Body.Bytes(), &sh)
	if shareRR.Code != 201 {
		t.Fatalf("share: %d %s", shareRR.Code, shareRR.Body.String())
	}
	page := call("GET", sh.URL, "", "", nil)
	if page.Code != 200 || page.Header().Get("Set-Cookie") != "" {
		t.Fatalf("public page: %d", page.Code)
	}
	if call("GET", publicFrame(t, s, page.Body.Bytes()), "", "", nil).Code != 200 {
		t.Fatal("public frame")
	}
}
