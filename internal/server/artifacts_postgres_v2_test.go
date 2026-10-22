package server

import (
	"strings"
	"testing"
)

// shareOf creates a public link as user and returns its id and URL.
func (e *artEnv) shareOf(user, artifactID string) (string, string) {
	e.t.Helper()
	code, raw := e.do(user, "POST", "/api/artifacts/"+artifactID+"/shares", map[string]any{})
	if code != 201 {
		e.t.Fatalf("share as %s: %d %s", user, code, raw)
	}
	var v struct{ ID, URL string }
	decodeJSON(e.t, raw, &v)
	return v.ID, v.URL
}

// linkWorks reports whether the public page and the public capability of its
// frame are served (they must always agree).
func (e *artEnv) linkWorks(url string) bool {
	e.t.Helper()
	page := corporateHTTP(e.root, "GET", url, "")
	if page.Code != 200 {
		return false
	}
	frame := publicFrame(e.t, e.root, page.Body.Bytes())
	return corporateHTTP(e.root, "GET", frame, "").Code == 200
}

// pageThenFrame returns the frame URL of a working link.
func (e *artEnv) frameOf(url string) string {
	e.t.Helper()
	page := corporateHTTP(e.root, "GET", url, "")
	if page.Code != 200 {
		e.t.Fatalf("page %d", page.Code)
	}
	return publicFrame(e.t, e.root, page.Body.Bytes())
}

func TestArtifactsPostgresLinkOutlivesNeitherCreatorNorRights(t *testing.T) {
	e := newArtEnv(t)

	// (a) private: the owner is demoted, loses access, or is disabled
	p := e.create("editor", map[string]any{"visibility": "private", "title": "Личный"})
	pp := "/api/artifacts/" + p.ID
	shareID, url := e.shareOf("editor", p.ID)
	frame := e.frameOf(url)
	if !e.linkWorks(url) {
		t.Fatal("fresh link does not work")
	}
	e.exec(`DELETE FROM memberships WHERE subject_id='editor' AND resource_id=?`, e.space)
	if e.linkWorks(url) || corporateHTTP(e.root, "GET", frame, "").Code != 404 {
		t.Error("link of an owner without container access still works")
	}
	if e.status("editor", "GET", pp+"/shares", nil) != 404 {
		t.Error("an owner without access lists links")
	}
	// reader access is enough for the owner of a private artifact: the link is valid
	// again and the owner can list and revoke it, though the container is read-only for her
	e.grant("space", e.space, "user", "editor", "reader")
	if !e.linkWorks(url) {
		t.Error("link of a private artifact must work while the owner can read the container")
	}
	if code, raw := e.do("editor", "GET", pp+"/shares", nil); code != 200 || !strings.Contains(string(raw), shareID) {
		t.Errorf("owner as reader lists links: %d %s", code, raw)
	}
	if e.status("editor", "POST", pp+"/shares", map[string]any{}) != 403 {
		t.Error("a reader created a new link")
	}
	if e.status("editor", "DELETE", pp+"/shares/"+shareID, nil) != 204 {
		t.Error("owner as reader could not revoke")
	}
	if e.linkWorks(url) {
		t.Error("revoked link works")
	}
	// disabled owner
	e.grant("space", e.space, "user", "editor", "editor")
	_, url2 := e.shareOf("editor", p.ID)
	frame2 := e.frameOf(url2)
	e.exec(`UPDATE users SET disabled=1 WHERE id='editor'`)
	if corporateHTTP(e.root, "GET", url2, "").Code != 404 || corporateHTTP(e.root, "GET", frame2, "").Code != 404 {
		t.Error("link of a disabled user works")
	}
	e.exec(`UPDATE users SET disabled=0 WHERE id='editor'`)
	if !e.linkWorks(url2) {
		t.Error("link must work again for a re-enabled owner")
	}

	// (c) inherit: the creator drops below the right to share
	a := e.create("editor", map[string]any{"title": "Общий редактора"})
	_, ownerLink := e.shareOf("editor", a.ID)
	b := e.create("alice", map[string]any{"title": "Общий"})
	_, managerLink := e.shareOf("bob", b.ID)
	if !e.linkWorks(ownerLink) || !e.linkWorks(managerLink) {
		t.Fatal("links do not work")
	}
	e.grant("space", e.space, "user", "editor", "reader")
	if e.linkWorks(ownerLink) {
		t.Error("link of an owner demoted to reader works")
	}
	e.grant("space", e.space, "user", "bob", "editor")
	if e.linkWorks(managerLink) {
		t.Error("link of a manager demoted to editor works")
	}
	e.grant("space", e.space, "user", "bob", "manager")
	if !e.linkWorks(managerLink) {
		t.Error("link must work again for a restored manager")
	}
	e.exec(`UPDATE users SET disabled=1 WHERE id='bob'`)
	if e.linkWorks(managerLink) {
		t.Error("link of a disabled manager works")
	}
	e.exec(`UPDATE users SET disabled=0 WHERE id='bob'`)
	// the ordinary owner of the space manages the link of the demoted manager
	_, raw := e.do("alice", "GET", "/api/artifacts/"+b.ID+"/shares", nil)
	var shares []map[string]any
	decodeJSON(t, raw, &shares)
	if len(shares) != 1 {
		t.Fatalf("shares: %s", raw)
	}
	if e.status("alice", "DELETE", "/api/artifacts/"+b.ID+"/shares/"+shares[0]["id"].(string), nil) != 204 {
		t.Error("revoke by the space owner")
	}
	if e.linkWorks(managerLink) {
		t.Error("revoked link works")
	}
}

func TestArtifactsPostgresLinksInArchivedContainer(t *testing.T) {
	e := newArtEnv(t)
	a := e.create("alice", map[string]any{"title": "В архиве"})
	path := "/api/artifacts/" + a.ID
	shareID, url := e.shareOf("alice", a.ID)
	p := e.create("editor", map[string]any{"visibility": "private"})
	pid, purl := e.shareOf("editor", p.ID)
	if e.status("alice", "PATCH", "/api/spaces/"+e.space, map[string]any{"archived": true}) != 200 {
		t.Fatal("archive")
	}
	// archive = read only for content, links stay manageable
	if code, raw := e.do("bob", "GET", path+"/shares", nil); code != 200 || !strings.Contains(string(raw), shareID) {
		t.Errorf("list in an archived container: %d %s", code, raw)
	}
	if e.status("alice", "POST", path+"/shares", map[string]any{}) != 403 {
		t.Error("new link in an archived container")
	}
	if e.status("alice", "PUT", path, map[string]any{"base_revision": 1, "title": "x"}) != 403 {
		t.Error("update in an archived container")
	}
	if e.status("alice", "DELETE", path, map[string]any{"base_revision": 1}) != 403 {
		t.Error("delete in an archived container must stay forbidden")
	}
	if e.status("editor", "DELETE", path+"/shares/"+shareID, nil) != 403 {
		t.Error("an editor revoked a link")
	}
	if e.status("bob", "DELETE", path+"/shares/"+shareID, nil) != 204 {
		t.Error("revoke in an archived container")
	}
	if e.linkWorks(url) {
		t.Error("revoked link works")
	}
	// a private artifact in an archived container: its owner revokes too
	if e.status("editor", "DELETE", "/api/artifacts/"+p.ID+"/shares/"+pid, nil) != 204 || e.linkWorks(purl) {
		t.Error("owner could not revoke in an archived container")
	}
}

func TestArtifactsPostgresRightsBeforeBodyParsing(t *testing.T) {
	e := newArtEnv(t)
	a := e.create("alice", nil)
	huge := strings.Repeat("a", 25<<20)
	// invalid on purpose: parsing it would give 400/413, rights give 403
	put := `{"base_revision":1,"files":[{"path":"../bad","content":"` + huge + `"}]}`
	for _, u := range []string{"reader", "commenter"} {
		if code, raw := e.do(u, "PUT", "/api/artifacts/"+a.ID, put); code != 403 || strings.Contains(string(raw), "path") {
			t.Errorf("%s PUT: %d %.120s", u, code, raw)
		}
	}
	if code, _ := e.do("u1", "PUT", "/api/artifacts/"+a.ID, put); code != 404 {
		t.Errorf("outsider PUT: %d", code)
	}
	post := `{"space_id":"` + e.space + `","title":"t","files":[{"path":"../bad","content":"` + huge + `"}]}`
	for _, u := range []string{"reader", "commenter", "u1"} {
		if code, raw := e.do(u, "POST", "/api/artifacts", post); code != 403 || strings.Contains(string(raw), "path") {
			t.Errorf("%s POST: %d %.120s", u, code, raw)
		}
	}
	// the same body from an editor is parsed and rejected on its content
	if code, raw := e.do("editor", "POST", "/api/artifacts", post); code != 400 || !strings.Contains(string(raw), "path") {
		t.Errorf("editor POST: %d %.120s", code, raw)
	}
	if code, raw := e.do("editor", "PUT", "/api/artifacts/"+a.ID, put); code != 400 || !strings.Contains(string(raw), "path") {
		t.Errorf("editor PUT: %d %.120s", code, raw)
	}
}

func TestArtifactsPostgresDeletedArtifactsOnlyForRestorers(t *testing.T) {
	e := newArtEnv(t)
	a := e.create("alice", nil)
	path := "/api/artifacts/" + a.ID
	if e.status("editor", "DELETE", path, map[string]any{"base_revision": 1}) != 200 {
		t.Fatal("delete")
	}
	reads := []string{path, path + "/history", path + "/files/index.html", path + "/shares"}
	for _, u := range []string{"reader", "commenter", "u1", "admin"} {
		for _, p := range reads {
			if code := e.status(u, "GET", p, nil); code != 404 {
				t.Errorf("%s GET %s of a deleted artifact: %d", u, p, code)
			}
		}
		if e.count(u, `SELECT count(*) FROM artifacts WHERE deleted_at IS NOT NULL`) != 0 || e.count(u, `SELECT count(*) FROM artifact_revisions WHERE artifact_id=?`, a.ID) != 0 {
			t.Errorf("%s sees rows of a deleted artifact", u)
		}
	}
	for _, u := range []string{"editor", "bob", "alice"} {
		for _, p := range reads[:3] {
			if code := e.status(u, "GET", p, nil); code != 200 {
				t.Errorf("%s GET %s of a deleted artifact: %d", u, p, code)
			}
		}
	}
	if e.status("reader", "POST", path+"/restore", map[string]any{"base_revision": 2}) != 404 {
		t.Error("reader restored")
	}
	if e.status("editor", "POST", path+"/restore", map[string]any{"base_revision": 2}) != 200 || e.status("reader", "GET", path, nil) != 200 {
		t.Error("restore did not bring the artifact back")
	}
	// private: only the owner restores
	p := e.create("editor", map[string]any{"visibility": "private"})
	pp := "/api/artifacts/" + p.ID
	e.status("editor", "DELETE", pp, map[string]any{"base_revision": 1})
	for _, u := range []string{"alice", "bob", "reader"} {
		if e.status(u, "GET", pp, nil) != 404 {
			t.Errorf("%s reads a deleted private artifact", u)
		}
	}
	if e.status("editor", "GET", pp, nil) != 200 || e.status("editor", "POST", pp+"/restore", map[string]any{"base_revision": 2}) != 200 {
		t.Error("owner of a private artifact could not restore")
	}
}

func TestArtifactsPostgresSchemaV2Guard(t *testing.T) {
	e := newArtEnv(t)
	for _, c := range []struct{ break_, fix string }{
		{`ALTER POLICY artifact_shares_read ON artifact_shares USING (true)`, `ALTER POLICY artifact_shares_read ON artifact_shares USING (EXISTS (SELECT 1 FROM public.artifacts a WHERE a.id=artifact_shares.artifact_id AND public.artifact_can_manage_links(a.space_id,a.project_id,a.visibility,a.owner_id)))`},
		{`ALTER POLICY artifacts_read ON artifacts USING (public.artifact_can_read(space_id,project_id,visibility,owner_id))`, `ALTER POLICY artifacts_read ON artifacts USING (public.artifact_can_view(space_id,project_id,visibility,owner_id,deleted_at))`},
		{`GRANT EXECUTE ON FUNCTION artifact_link_valid(text,text) TO PUBLIC`, `REVOKE ALL ON FUNCTION artifact_link_valid(text,text) FROM PUBLIC`},
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
	// the function answers a bare boolean for anybody and discloses nothing else
	a := e.create("alice", nil)
	var ok bool
	if err := e.root.DB.QueryRow(`SELECT artifact_link_valid(?,?)`, a.ID, "alice").Scan(&ok); err != nil || !ok {
		t.Errorf("valid creator: %v %v", ok, err)
	}
	for _, args := range [][2]string{{a.ID, "reader"}, {a.ID, "nobody"}, {a.ID, ""}, {"missing", "alice"}} {
		if err := e.root.DB.QueryRow(`SELECT artifact_link_valid(?,?)`, args[0], args[1]).Scan(&ok); err != nil || ok {
			t.Errorf("%v: %v %v", args, ok, err)
		}
	}
}
