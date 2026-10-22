package server

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// Fixtures for the safe-move tests. They use the disposable PostgreSQL database
// of corporateFixture; ACL rows are inserted through the owner connection and
// content through the HTTP API as ordinary users.

type movesEnv struct {
	t     *testing.T
	owner *Server
	root  *Server
	users map[string]*Server
}

func newMovesEnv(t *testing.T) *movesEnv {
	t.Helper()
	owner, root := corporateFixture(t)
	for _, id := range []string{"u1", "u2", "u3", "u4", "u5"} {
		if _, err := owner.DB.Exec(`INSERT INTO users(id,issuer,subject,name,admin) VALUES(?,?,?,?,0)`, id, "https://identity.example.org", id, "User "+id); err != nil {
			t.Fatal(err)
		}
	}
	return &movesEnv{t: t, owner: owner, root: root, users: map[string]*Server{}}
}

func (e *movesEnv) as(id string) *Server {
	if s := e.users[id]; s != nil {
		return s
	}
	e.users[id] = corporateAs(e.t, e.root, id)
	return e.users[id]
}

func (e *movesEnv) exec(q string, args ...any) {
	e.t.Helper()
	if _, err := e.owner.DB.Exec(q, args...); err != nil {
		e.t.Fatal(err)
	}
}

func (e *movesEnv) grant(kind, id, subjectType, subject, role string) {
	e.t.Helper()
	e.exec(`INSERT INTO memberships(resource_type,resource_id,subject_type,subject_id,role) VALUES(?,?,?,?,?) ON CONFLICT (resource_type,resource_id,subject_type,subject_id) DO UPDATE SET role=EXCLUDED.role`, kind, id, subjectType, subject, role)
}

func (e *movesEnv) space(user string) string {
	e.t.Helper()
	return corporateCreateSpace(e.t, e.as(user))
}

func (e *movesEnv) project(space, parent, name string, inherit bool) string {
	e.t.Helper()
	id := newID()
	e.exec(`INSERT INTO projects(id,space_id,parent_id,name,inherit_acl) VALUES(?,?,?,?,?)`, id, space, parent, name, boolInt(inherit))
	return id
}

func (e *movesEnv) team(id string, members ...string) {
	e.t.Helper()
	e.exec(`INSERT INTO teams(id,name) VALUES(?,?)`, id, "Team "+id)
	for _, m := range members {
		e.exec(`INSERT INTO team_members(team_id,user_id) VALUES(?,?)`, id, m)
	}
}

func (e *movesEnv) article(user, space, project, parent, title string) Article {
	e.t.Helper()
	body, _ := json.Marshal(map[string]string{"space_id": space, "project_id": project, "parent_id": parent, "title": title, "body": "needle " + title})
	r := corporateHTTP(e.as(user), "POST", "/api/articles", string(body))
	if r.Code != 201 {
		e.t.Fatalf("create article %d %s", r.Code, r.Body.String())
	}
	var a Article
	if err := json.Unmarshal(r.Body.Bytes(), &a); err != nil {
		e.t.Fatal(err)
	}
	return a
}

func (e *movesEnv) get(user, id string) (Article, int) {
	e.t.Helper()
	r := corporateHTTP(e.as(user), "GET", "/api/articles/"+id, "")
	var a Article
	_ = json.Unmarshal(r.Body.Bytes(), &a)
	return a, r.Code
}

type movesCall struct {
	Code int
	Body map[string]json.RawMessage
	Raw  string
}

func (e *movesEnv) call(user, path string, req map[string]any) movesCall {
	e.t.Helper()
	raw, _ := json.Marshal(req)
	r := corporateHTTP(e.as(user), "POST", path, string(raw))
	out := movesCall{Code: r.Code, Raw: r.Body.String()}
	_ = json.Unmarshal(r.Body.Bytes(), &out.Body)
	return out
}

func (c movesCall) preview(t *testing.T) movePreview {
	t.Helper()
	var p movePreview
	raw := c.Raw
	if c.Body["preview"] != nil {
		raw = string(c.Body["preview"])
	}
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		t.Fatalf("preview json: %v %s", err, c.Raw)
	}
	return p
}

func articleMove(id string, rev int, space, project, parent string) map[string]any {
	return map[string]any{"kind": "article", "id": id, "base_revision": rev, "target": map[string]string{"space_id": space, "project_id": project, "parent_id": parent}}
}

func projectMove(id, space, parent string) map[string]any {
	return map[string]any{"kind": "project", "id": id, "target": map[string]string{"space_id": space, "parent_id": parent}}
}

func withFingerprint(req map[string]any, fp string) map[string]any {
	out := map[string]any{"fingerprint": fp}
	for k, v := range req {
		out[k] = v
	}
	return out
}

func (e *movesEnv) accessRow(p movePreview, subjectType, subject string) *moveAccess {
	for i := range p.Access {
		if p.Access[i].SubjectType == subjectType && p.Access[i].SubjectID == subject {
			return &p.Access[i]
		}
	}
	return nil
}

func roleIs(v *string, want string) bool {
	if want == "" {
		return v == nil
	}
	return v != nil && *v == want
}

func (e *movesEnv) historyMessages(user, id string) []string {
	e.t.Helper()
	r := corporateHTTP(e.as(user), "GET", "/api/articles/"+id+"/history", "")
	if r.Code != 200 {
		e.t.Fatalf("history %d %s", r.Code, r.Body.String())
	}
	var rows []Revision
	if err := json.Unmarshal(r.Body.Bytes(), &rows); err != nil {
		e.t.Fatal(err)
	}
	msgs := make([]string, 0, len(rows))
	for _, row := range rows {
		msgs = append(msgs, row.Message)
	}
	return msgs
}

func (e *movesEnv) auditCount(action, resource string) int {
	e.t.Helper()
	var n int
	if err := e.owner.DB.QueryRow(`SELECT count(*) FROM security_audit WHERE action=? AND resource_id=?`, action, resource).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

// scenario: alice owns A, bob owns B and alice manages B.
func (e *movesEnv) twoSpaces() (a, b string) {
	a, b = e.space("alice"), e.space("bob")
	e.grant("space", b, "user", "alice", "manager")
	return a, b
}

func TestMovesArticleBetweenSpacesWithAccessDiff(t *testing.T) {
	e := newMovesEnv(t)
	a, b := e.twoSpaces()
	e.team("t1", "u1", "u2")
	e.grant("space", a, "team", "t1", "reader")
	e.grant("space", a, "user", "u3", "reader")
	e.grant("space", a, "user", "u4", "reader")
	e.grant("space", b, "user", "u3", "editor")
	e.grant("space", b, "user", "u5", "reader")

	root := e.article("alice", a, "", "", "Root")
	child := e.article("alice", a, "", root.ID, "Child")
	gone := e.article("alice", a, "", root.ID, "Gone")
	grand := e.article("alice", a, "", child.ID, "Grandchild")
	if r := corporateHTTP(e.as("alice"), "DELETE", "/api/articles/"+gone.ID, `{"base_revision":1}`); r.Code != 200 && r.Code != 204 {
		t.Fatalf("delete %d %s", r.Code, r.Body.String())
	}
	target := e.article("bob", b, "", "", "Landing parent")

	pv := e.call("alice", "/api/moves/preview", articleMove(root.ID, 1, b, "", target.ID))
	if pv.Code != 200 {
		t.Fatalf("preview %d %s", pv.Code, pv.Raw)
	}
	p := pv.preview(t)
	if p.Articles != 4 || p.Projects != 0 || p.Fingerprint == "" {
		t.Fatalf("counts %+v", p)
	}
	if p.Source.SpaceID != a || p.Target.SpaceID != b || p.Source.SpaceName == "" || p.Target.SpaceName == "" {
		t.Fatalf("containers %+v %+v", p.Source, p.Target)
	}
	if row := e.accessRow(p, "team", "t1"); row == nil || !roleIs(row.Before, "reader") || !roleIs(row.After, "") || row.Users != 2 || row.Name != "Team t1" {
		t.Fatalf("team row %+v", row)
	}
	if row := e.accessRow(p, "user", "u3"); row == nil || !roleIs(row.Before, "reader") || !roleIs(row.After, "editor") {
		t.Fatalf("u3 row %+v", row)
	}
	if row := e.accessRow(p, "user", "u4"); row == nil || !roleIs(row.Before, "reader") || !roleIs(row.After, "") {
		t.Fatalf("u4 row %+v", row)
	}
	if row := e.accessRow(p, "user", "u5"); row == nil || !roleIs(row.Before, "") || !roleIs(row.After, "reader") {
		t.Fatalf("u5 row %+v", row)
	}
	if row := e.accessRow(p, "user", "alice"); row == nil || !roleIs(row.Before, "owner") || !roleIs(row.After, "manager") {
		t.Fatalf("alice row %+v", row)
	}
	if row := e.accessRow(p, "user", "bob"); row == nil || !roleIs(row.Before, "") || !roleIs(row.After, "owner") {
		t.Fatalf("bob row %+v", row)
	}
	// gaining: u3, u5, bob. losing: u1, u2 (via team), u4, alice (owner -> manager).
	if p.UsersGaining != 3 || p.UsersLosing != 4 {
		t.Fatalf("users gaining=%d losing=%d", p.UsersGaining, p.UsersLosing)
	}
	if len(p.Warnings) == 0 {
		t.Fatal("expected warnings")
	}
	again := e.call("alice", "/api/moves/preview", articleMove(root.ID, 1, b, "", target.ID)).preview(t)
	if again.Fingerprint != p.Fingerprint {
		t.Fatal("fingerprint is not stable")
	}

	if r := e.call("alice", "/api/moves/apply", articleMove(root.ID, 1, b, "", target.ID)); r.Code != 400 {
		t.Fatalf("apply without fingerprint %d %s", r.Code, r.Raw)
	}
	if r := e.call("alice", "/api/moves/apply", withFingerprint(articleMove(root.ID, 1, b, "", target.ID), "deadbeef")); r.Code != 409 || !strings.Contains(r.Raw, "access changed") {
		t.Fatalf("wrong fingerprint %d %s", r.Code, r.Raw)
	}
	done := e.call("alice", "/api/moves/apply", withFingerprint(articleMove(root.ID, 1, b, "", target.ID), p.Fingerprint))
	if done.Code != 200 {
		t.Fatalf("apply %d %s", done.Code, done.Raw)
	}
	var counts struct {
		Articles int     `json:"articles"`
		Projects int     `json:"projects"`
		Root     Article `json:"root"`
	}
	if err := json.Unmarshal([]byte(done.Raw), &counts); err != nil {
		t.Fatal(err)
	}
	if counts.Articles != 4 || counts.Projects != 0 || counts.Root.ID != root.ID || counts.Root.SpaceID != b || counts.Root.ParentID != target.ID || counts.Root.Revision != 2 {
		t.Fatalf("result %+v", counts)
	}
	for _, id := range []string{root.ID, child.ID, grand.ID, gone.ID} {
		moved, code := e.get("alice", id)
		if code != 200 || moved.SpaceID != b || moved.ProjectID != "" || moved.Revision < 2 {
			t.Fatalf("article %s after move: code=%d %+v", id, code, moved)
		}
	}
	if got, _ := e.get("alice", child.ID); got.ParentID != root.ID {
		t.Fatalf("descendant parent changed: %q", got.ParentID)
	}
	if msgs := e.historyMessages("alice", root.ID); msgs[0] != "Перенос" {
		t.Fatalf("root history %v", msgs)
	}
	if msgs := e.historyMessages("alice", child.ID); msgs[0] != "Перенос вместе с родительской статьёй" {
		t.Fatalf("child history %v", msgs)
	}
	if msgs := e.historyMessages("alice", gone.ID); msgs[0] != "Перенос вместе с родительской статьёй" {
		t.Fatalf("deleted child history %v", msgs)
	}
	// Search index: live articles are found in the new space, the deleted one is not indexed.
	var fts int
	if err := e.owner.DB.QueryRow(`SELECT count(*) FROM article_fts WHERE article_id=?`, gone.ID).Scan(&fts); err != nil || fts != 0 {
		t.Fatalf("deleted article indexed: %d %v", fts, err)
	}
	// Old readers lost the article, new ones can read it.
	if _, code := e.get("u4", root.ID); code != 404 {
		t.Fatalf("u4 still reads: %d", code)
	}
	if _, code := e.get("u5", root.ID); code != 200 {
		t.Fatalf("u5 cannot read: %d", code)
	}
	if n := e.auditCount("move.article", root.ID); n != 1 {
		t.Fatalf("audit events %d", n)
	}
	var details string
	if err := e.owner.DB.QueryRow(`SELECT details::text FROM security_audit WHERE action='move.article' AND resource_id=?`, root.ID).Scan(&details); err != nil || !strings.Contains(details, "t1") || strings.Contains(details, "needle") {
		t.Fatalf("audit details %s %v", details, err)
	}
	// A second apply with the old base revision is a conflict.
	if r := e.call("alice", "/api/moves/apply", withFingerprint(articleMove(root.ID, 1, a, "", ""), p.Fingerprint)); r.Code != 409 {
		t.Fatalf("replay %d %s", r.Code, r.Raw)
	}
}

func TestMovesArticleIntoProject(t *testing.T) {
	e := newMovesEnv(t)
	a, b := e.twoSpaces()
	proj := e.project(b, "", "Docs", false)
	e.grant("project", proj, "user", "alice", "manager")
	e.grant("project", proj, "user", "u1", "editor")
	root := e.article("alice", a, "", "", "Root")
	child := e.article("alice", a, "", root.ID, "Child")

	p := e.call("alice", "/api/moves/preview", articleMove(root.ID, 1, b, proj, "")).preview(t)
	if p.Target.ProjectID != proj || p.Target.ProjectName != "Docs" {
		t.Fatalf("target %+v", p.Target)
	}
	// The project does not inherit the space: bob (space owner) gets nothing, u1 gets editor.
	if row := e.accessRow(p, "user", "bob"); row != nil {
		t.Fatalf("bob must be unaffected: %+v", row)
	}
	if row := e.accessRow(p, "user", "u1"); row == nil || !roleIs(row.After, "editor") {
		t.Fatalf("u1 row %+v", row)
	}
	if row := e.accessRow(p, "user", "alice"); row == nil || !roleIs(row.Before, "owner") || !roleIs(row.After, "manager") {
		t.Fatalf("alice row %+v", row)
	}
	if r := e.call("alice", "/api/moves/apply", withFingerprint(articleMove(root.ID, 1, b, proj, ""), p.Fingerprint)); r.Code != 200 {
		t.Fatalf("apply %d %s", r.Code, r.Raw)
	}
	for _, id := range []string{root.ID, child.ID} {
		got, code := e.get("alice", id)
		if code != 200 || got.SpaceID != b || got.ProjectID != proj {
			t.Fatalf("after move %+v %d", got, code)
		}
	}
	// Moving between projects of one space works the same way and back to the space.
	back := e.call("alice", "/api/moves/preview", articleMove(root.ID, 2, b, "", ""))
	if back.Code != 200 {
		t.Fatalf("back preview %d %s", back.Code, back.Raw)
	}
	if r := e.call("alice", "/api/moves/apply", withFingerprint(articleMove(root.ID, 2, b, "", ""), back.preview(t).Fingerprint)); r.Code != 200 {
		t.Fatalf("back apply %d %s", r.Code, r.Raw)
	}
	if got, _ := e.get("alice", child.ID); got.ProjectID != "" || got.SpaceID != b {
		t.Fatalf("child after return %+v", got)
	}
}

func TestMovesDeniedByRolesAndArchive(t *testing.T) {
	e := newMovesEnv(t)
	a, b := e.twoSpaces()
	e.grant("space", a, "user", "editor", "editor")
	e.grant("space", b, "user", "editor", "manager")
	e.grant("space", a, "user", "bob", "manager")
	e.grant("space", b, "user", "reader", "reader")
	art := e.article("alice", a, "", "", "Doc")

	// editor on the source is not enough even if manager on the target.
	if r := e.call("editor", "/api/moves/preview", articleMove(art.ID, 1, b, "", "")); r.Code != 403 {
		t.Fatalf("editor source %d %s", r.Code, r.Raw)
	}
	// manager on the source but only editor on the target.
	e.grant("space", a, "user", "editor", "manager")
	e.grant("space", b, "user", "editor", "editor")
	if r := e.call("editor", "/api/moves/preview", articleMove(art.ID, 1, b, "", "")); r.Code != 403 {
		t.Fatalf("editor target %d %s", r.Code, r.Raw)
	}
	// a reader of the target cannot see it is there at all for a non-reader; apply is also refused.
	if r := e.call("u1", "/api/moves/preview", articleMove(art.ID, 1, b, "", "")); r.Code != 404 {
		t.Fatalf("stranger %d %s", r.Code, r.Raw)
	}
	if r := e.call("reader", "/api/moves/apply", withFingerprint(articleMove(art.ID, 1, b, "", ""), "x")); r.Code != 404 {
		t.Fatalf("reader apply %d %s", r.Code, r.Raw)
	}
	// Archived target space.
	e.exec(`UPDATE corporate_space_state SET archived=true WHERE space_id=?`, b)
	if r := e.call("alice", "/api/moves/preview", articleMove(art.ID, 1, b, "", "")); r.Code != 409 {
		t.Fatalf("archived target %d %s", r.Code, r.Raw)
	}
	e.exec(`UPDATE corporate_space_state SET archived=false WHERE space_id=?`, b)
	// Archived target project.
	proj := e.project(b, "", "Frozen", true)
	e.exec(`UPDATE projects SET archived_at='2026-01-01T00:00:00Z' WHERE id=?`, proj)
	if r := e.call("alice", "/api/moves/preview", articleMove(art.ID, 1, b, proj, "")); r.Code != 409 {
		t.Fatalf("archived project %d %s", r.Code, r.Raw)
	}
	// Archived source project.
	srcProj := e.project(a, "", "Old", true)
	inProj := e.article("alice", a, srcProj, "", "In project")
	e.exec(`UPDATE projects SET archived_at='2026-01-01T00:00:00Z' WHERE id=?`, srcProj)
	if r := e.call("alice", "/api/moves/preview", articleMove(inProj.ID, 1, b, "", "")); r.Code != 409 {
		t.Fatalf("archived source %d %s", r.Code, r.Raw)
	}
	// Same container is not a move; invalid input shapes.
	if r := e.call("alice", "/api/moves/preview", articleMove(art.ID, 1, a, "", "")); r.Code != 400 {
		t.Fatalf("same container %d %s", r.Code, r.Raw)
	}
	if r := e.call("alice", "/api/moves/preview", map[string]any{"kind": "other", "id": art.ID}); r.Code != 400 {
		t.Fatalf("kind %d", r.Code)
	}
	if r := e.call("alice", "/api/moves/preview", map[string]any{"kind": "article", "id": art.ID, "extra": 1}); r.Code != 400 {
		t.Fatalf("unknown field %d", r.Code)
	}
}

func TestMovesApplyRejectsChangedAccessOrRevision(t *testing.T) {
	e := newMovesEnv(t)
	a, b := e.twoSpaces()
	root := e.article("alice", a, "", "", "Root")
	child := e.article("alice", a, "", root.ID, "Child")
	req := articleMove(root.ID, 1, b, "", "")
	p := e.call("alice", "/api/moves/preview", req).preview(t)

	// A grant appears on the target between preview and apply.
	e.grant("space", b, "user", "u2", "reader")
	r := e.call("alice", "/api/moves/apply", withFingerprint(req, p.Fingerprint))
	if r.Code != 409 || !strings.Contains(r.Raw, `"error":"access changed"`) {
		t.Fatalf("grant change %d %s", r.Code, r.Raw)
	}
	fresh := r.preview(t)
	if fresh.Fingerprint == p.Fingerprint || e.accessRow(fresh, "user", "u2") == nil {
		t.Fatalf("fresh preview %+v", fresh)
	}
	if got, _ := e.get("alice", root.ID); got.SpaceID != a {
		t.Fatal("article moved despite 409")
	}
	// The fresh fingerprint is accepted... unless a descendant is edited first.
	if rr := corporateHTTP(e.as("alice"), "PUT", "/api/articles/"+child.ID, `{"base_revision":1,"body":"edited"}`); rr.Code != 200 {
		t.Fatalf("edit %d", rr.Code)
	}
	if rr := e.call("alice", "/api/moves/apply", withFingerprint(req, fresh.Fingerprint)); rr.Code != 409 {
		t.Fatalf("descendant edit %d %s", rr.Code, rr.Raw)
	}
	// The root changes: the supplied base_revision is stale.
	if rr := corporateHTTP(e.as("alice"), "PUT", "/api/articles/"+root.ID, `{"base_revision":1,"body":"edited"}`); rr.Code != 200 {
		t.Fatalf("edit root %d", rr.Code)
	}
	if rr := e.call("alice", "/api/moves/apply", withFingerprint(req, fresh.Fingerprint)); rr.Code != 409 || !strings.Contains(rr.Raw, "revision conflict") {
		t.Fatalf("stale revision %d %s", rr.Code, rr.Raw)
	}
	// With current revisions a new preview applies.
	req = articleMove(root.ID, 2, b, "", "")
	p = e.call("alice", "/api/moves/preview", req).preview(t)
	if rr := e.call("alice", "/api/moves/apply", withFingerprint(req, p.Fingerprint)); rr.Code != 200 {
		t.Fatalf("final apply %d %s", rr.Code, rr.Raw)
	}
}

func TestMovesProjectSubtreeBetweenSpaces(t *testing.T) {
	e := newMovesEnv(t)
	a, b := e.twoSpaces()
	e.team("t1", "u1", "u2")
	e.grant("space", a, "team", "t1", "editor")
	e.grant("space", b, "user", "u3", "reader")
	top := e.project(a, "", "Top", true)
	sub := e.project(a, top, "Sub", true)
	subsub := e.project(a, sub, "SubSub", true)
	stay := e.project(a, "", "Stays", true)
	rootArticle := e.article("alice", a, top, "", "Top article")
	subArticle := e.article("alice", a, sub, "", "Sub article")
	deepArticle := e.article("alice", a, subsub, "", "Deep article")
	stayArticle := e.article("alice", a, stay, "", "Stay article")
	spaceArticle := e.article("alice", a, "", "", "Space article")

	pv := e.call("alice", "/api/moves/preview", projectMove(top, b, ""))
	if pv.Code != 200 {
		t.Fatalf("preview %d %s", pv.Code, pv.Raw)
	}
	p := pv.preview(t)
	if p.Projects != 3 || p.Articles != 3 {
		t.Fatalf("counts %+v", p)
	}
	if row := e.accessRow(p, "team", "t1"); row == nil || !roleIs(row.Before, "editor") || !roleIs(row.After, "") || row.Users != 2 {
		t.Fatalf("t1 %+v", row)
	}
	if row := e.accessRow(p, "user", "u3"); row == nil || !roleIs(row.Before, "") || !roleIs(row.After, "reader") {
		t.Fatalf("u3 %+v", row)
	}
	if p.UsersLosing < 3 || p.UsersGaining < 2 {
		t.Fatalf("users %d/%d", p.UsersGaining, p.UsersLosing)
	}
	done := e.call("alice", "/api/moves/apply", withFingerprint(projectMove(top, b, ""), p.Fingerprint))
	if done.Code != 200 {
		t.Fatalf("apply %d %s", done.Code, done.Raw)
	}
	var res struct {
		Articles int              `json:"articles"`
		Projects int              `json:"projects"`
		Root     corporateProject `json:"root"`
	}
	if err := json.Unmarshal([]byte(done.Raw), &res); err != nil || res.Projects != 3 || res.Articles != 3 || res.Root.SpaceID != b || res.Root.ParentID != "" {
		t.Fatalf("result %+v %v", res, err)
	}
	for _, id := range []string{top, sub, subsub} {
		var space string
		if err := e.owner.DB.QueryRow(`SELECT space_id FROM projects WHERE id=?`, id).Scan(&space); err != nil || space != b {
			t.Fatalf("project %s space %q %v", id, space, err)
		}
	}
	var parent string
	if err := e.owner.DB.QueryRow(`SELECT parent_id FROM projects WHERE id=?`, sub).Scan(&parent); err != nil || parent != top {
		t.Fatalf("sub parent %q", parent)
	}
	for _, id := range []string{rootArticle.ID, subArticle.ID, deepArticle.ID} {
		got, code := e.get("alice", id)
		if code != 200 || got.SpaceID != b || got.Revision != 2 {
			t.Fatalf("article %s: %d %+v", id, code, got)
		}
		if msgs := e.historyMessages("alice", id); msgs[0] != "Перенос вместе с проектом" {
			t.Fatalf("history %v", msgs)
		}
	}
	for _, id := range []string{stayArticle.ID, spaceArticle.ID} {
		if got, _ := e.get("alice", id); got.SpaceID != a || got.Revision != 1 {
			t.Fatalf("unrelated article moved %+v", got)
		}
	}
	if n := e.auditCount("move.project", top); n != 1 {
		t.Fatalf("audit %d", n)
	}
	// After the move alice is only manager in B, so she may not move a project there...
	if r := e.call("alice", "/api/moves/preview", projectMove(subsub, b, "")); r.Code != 403 {
		t.Fatalf("manager moves project %d %s", r.Code, r.Raw)
	}
	// ...until she is made an owner; then a sub-project moves to the root of the same space (parent change only).
	e.grant("space", b, "user", "alice", "owner")
	p2 := e.call("alice", "/api/moves/preview", projectMove(subsub, b, ""))
	if p2.Code != 200 {
		t.Fatalf("second preview %d %s", p2.Code, p2.Raw)
	}
	if r := e.call("alice", "/api/moves/apply", withFingerprint(projectMove(subsub, b, ""), p2.preview(t).Fingerprint)); r.Code != 200 {
		t.Fatalf("second apply %d %s", r.Code, r.Raw)
	}
}

func TestMovesProjectRulesOwnerCycleAndLimits(t *testing.T) {
	e := newMovesEnv(t)
	a, b := e.twoSpaces()
	top := e.project(a, "", "Top", true)
	sub := e.project(a, top, "Sub", true)
	e.grant("project", top, "user", "editor", "manager")
	e.grant("space", b, "user", "editor", "manager")
	// Manager (not owner) of the project cannot move it.
	if r := e.call("editor", "/api/moves/preview", projectMove(top, b, "")); r.Code != 403 {
		t.Fatalf("manager %d %s", r.Code, r.Raw)
	}
	// Owner of the project but only editor on the target.
	e.grant("project", top, "user", "u1", "owner")
	e.grant("space", b, "user", "u1", "editor")
	if r := e.call("u1", "/api/moves/preview", projectMove(top, b, "")); r.Code != 403 {
		t.Fatalf("target editor %d %s", r.Code, r.Raw)
	}
	// Cycles: under itself and under a descendant.
	if r := e.call("alice", "/api/moves/preview", projectMove(top, a, top)); r.Code != 400 {
		t.Fatalf("self cycle %d %s", r.Code, r.Raw)
	}
	if r := e.call("alice", "/api/moves/preview", projectMove(top, a, sub)); r.Code != 400 {
		t.Fatalf("descendant cycle %d %s", r.Code, r.Raw)
	}
	if r := e.call("alice", "/api/moves/preview", projectMove(top, a, "")); r.Code != 400 {
		t.Fatalf("no-op %d %s", r.Code, r.Raw)
	}
	// The impact function itself refuses a cycle, regardless of the Go checks.
	alice := e.as("alice")
	var n int
	if err := alice.DB.QueryRow(`SELECT count(*) FROM corporate_move_impact('project',?,?,?)`, top, a, sub).Scan(&n); err == nil {
		t.Fatal("impact function accepted a cycle")
	}
	// Owner role on the project is required even where the sub-project has an independent ACL.
	locked := e.project(a, top, "Locked", false)
	if r := e.call("alice", "/api/moves/preview", projectMove(top, b, "")); r.Code != 403 {
		t.Fatalf("sub-project without access %d %s", r.Code, r.Raw)
	}
	e.grant("project", locked, "user", "alice", "manager")
	if r := e.call("alice", "/api/moves/preview", projectMove(top, b, "")); r.Code != 200 {
		t.Fatalf("with sub-project access %d %s", r.Code, r.Raw)
	}
	// Archived sub-project.
	e.exec(`UPDATE projects SET archived_at='2026-01-01T00:00:00Z' WHERE id=?`, locked)
	if r := e.call("alice", "/api/moves/preview", projectMove(top, b, "")); r.Code != 409 {
		t.Fatalf("archived sub-project %d %s", r.Code, r.Raw)
	}
	e.exec(`UPDATE projects SET archived_at=NULL WHERE id=?`, locked)
	// Project limit: 100 projects per move.
	for i := 0; i < moveMaxProjects; i++ {
		e.project(a, sub, fmt.Sprintf("Bulk %d", i), true)
	}
	if r := e.call("alice", "/api/moves/preview", projectMove(top, b, "")); r.Code != 400 || !strings.Contains(r.Raw, "move too large") {
		t.Fatalf("project limit %d %s", r.Code, r.Raw)
	}
}

func TestMovesArticleLimit(t *testing.T) {
	e := newMovesEnv(t)
	a, b := e.twoSpaces()
	root := e.article("alice", a, "", "", "Root")
	alice := e.as("alice")
	tx, err := alice.DB.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(`INSERT INTO articles(id,space_id,parent_id,project_id,title,kind,status,revision,created_at,updated_at,author)
		SELECT 'bulk'||g,?,?,'','Bulk '||g,'article','draft',1,'2026-01-01T00:00:00Z','2026-01-01T00:00:00Z','alice' FROM generate_series(1,?) g`, a, root.ID, moveMaxArticles); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	// 1 root + 500 children = 501 > 500.
	if r := e.call("alice", "/api/moves/preview", articleMove(root.ID, 1, b, "", "")); r.Code != 400 || !strings.Contains(r.Raw, "move too large") {
		t.Fatalf("article limit %d %s", r.Code, r.Raw)
	}
}

func TestMovesSameContainerReparentThroughPut(t *testing.T) {
	e := newMovesEnv(t)
	a, b := e.twoSpaces()
	e.grant("space", a, "user", "editor", "editor")
	e.grant("space", a, "user", "reader", "reader")
	e.grant("space", b, "user", "editor", "editor")
	proj := e.project(a, "", "P", true)
	first := e.article("alice", a, "", "", "First")
	second := e.article("alice", a, "", "", "Second")
	pfirst := e.article("alice", a, proj, "", "P first")
	psecond := e.article("alice", a, proj, "", "P second")

	put := func(user, id, body string) int {
		return corporateHTTP(e.as(user), "PUT", "/api/articles/"+id, body).Code
	}
	if c := put("editor", second.ID, fmt.Sprintf(`{"base_revision":1,"parent_id":%q}`, first.ID)); c != 200 {
		t.Fatalf("editor reparent in space %d", c)
	}
	if c := put("editor", psecond.ID, fmt.Sprintf(`{"base_revision":1,"parent_id":%q}`, pfirst.ID)); c != 200 {
		t.Fatalf("editor reparent in project %d", c)
	}
	if got, _ := e.get("editor", second.ID); got.ParentID != first.ID {
		t.Fatalf("parent %q", got.ParentID)
	}
	if c := put("reader", first.ID, fmt.Sprintf(`{"base_revision":1,"parent_id":%q}`, second.ID)); c < 400 {
		t.Fatalf("reader reparent %d", c)
	}
	// Parent from another container is refused by the existing parent rule.
	other := e.article("bob", b, "", "", "Other")
	if c := put("editor", first.ID, fmt.Sprintf(`{"base_revision":1,"parent_id":%q}`, other.ID)); c != 400 {
		t.Fatalf("foreign parent %d", c)
	}
	// Cross-container PUT is refused with a hint about the preview endpoint.
	r := corporateHTTP(e.as("alice"), "PUT", "/api/articles/"+first.ID, fmt.Sprintf(`{"base_revision":1,"space_id":%q}`, b))
	if r.Code != 400 || !strings.Contains(r.Body.String(), "/api/moves/preview") {
		t.Fatalf("cross-space PUT %d %s", r.Code, r.Body.String())
	}
	r = corporateHTTP(e.as("alice"), "PUT", "/api/articles/"+first.ID, fmt.Sprintf(`{"base_revision":1,"project_id":%q}`, proj))
	if r.Code != 400 || !strings.Contains(r.Body.String(), "/api/moves/preview") {
		t.Fatalf("cross-project PUT %d %s", r.Code, r.Body.String())
	}
	if got, _ := e.get("alice", first.ID); got.SpaceID != a || got.ProjectID != "" {
		t.Fatalf("article moved by PUT: %+v", got)
	}
}

func TestMovesDatabaseTriggersRejectBypass(t *testing.T) {
	e := newMovesEnv(t)
	a, b := e.twoSpaces()
	e.grant("space", a, "user", "editor", "editor")
	e.grant("space", b, "user", "editor", "editor")
	art := e.article("alice", a, "", "", "Doc")
	proj := e.project(a, "", "P", true)
	inProj := e.article("alice", a, proj, "", "In project")

	exec := func(user, marker, q string, args ...any) error {
		db := e.as(user).DB
		tx, err := db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		if marker != "" {
			var v string
			if err = tx.QueryRow(`SELECT set_config('app.move',?,true)`, marker).Scan(&v); err != nil {
				t.Fatal(err)
			}
			if err = tx.QueryRow(`SELECT set_config('app.move_root',?,true)`, "x").Scan(&v); err != nil {
				t.Fatal(err)
			}
		}
		_, err = tx.Exec(q, args...)
		return err
	}
	// Even the owner of both containers cannot move without the transaction marker.
	if err := exec("alice", "", `UPDATE articles SET space_id=? WHERE id=?`, b, art.ID); err == nil {
		t.Fatal("direct space update accepted without marker")
	}
	if err := exec("alice", "", `UPDATE articles SET project_id='' , space_id=? WHERE id=?`, b, inProj.ID); err == nil {
		t.Fatal("direct container update accepted without marker")
	}
	// An editor who sets the marker themselves is stopped by the role check in the trigger.
	if err := exec("editor", "article", `UPDATE articles SET space_id=? WHERE id=?`, b, art.ID); err == nil {
		t.Fatal("editor moved an article with a self-set marker")
	}
	if err := exec("editor", "project", `UPDATE articles SET space_id=? WHERE id=?`, b, inProj.ID); err == nil {
		t.Fatal("editor moved a project article with a self-set marker")
	}
	// Projects: no marker, and a marker without owner/manager rights.
	if err := exec("alice", "", `UPDATE projects SET space_id=? WHERE id=?`, b, proj); err == nil {
		t.Fatal("direct project move accepted without marker")
	}
	if err := exec("alice", "", `UPDATE projects SET parent_id='' , space_id=? WHERE id=?`, a, proj); err != nil {
		t.Fatalf("unchanged parent/space must stay allowed: %v", err)
	}
	e.grant("project", proj, "user", "editor", "owner")
	if err := exec("editor", "project", `UPDATE projects SET space_id=? WHERE id=?`, b, proj); err == nil {
		t.Fatal("project owner without manager on the target moved the project")
	}
	// A marker for another root does not open sub-project updates.
	child := e.project(a, proj, "C", true)
	if err := exec("alice", "project", `UPDATE projects SET space_id=? WHERE id=?`, b, child); err == nil {
		t.Fatal("sub-project moved away from its parent")
	}
	if got, _ := e.get("alice", art.ID); got.SpaceID != a {
		t.Fatal("article moved by a rejected statement")
	}
}

func TestMovesImpactFunctionDoesNotLeak(t *testing.T) {
	e := newMovesEnv(t)
	a, b := e.twoSpaces()
	e.grant("space", a, "user", "editor", "manager")
	e.grant("space", a, "user", "u1", "reader")
	art := e.article("alice", a, "", "", "Doc")
	impact := func(user, kind, id, space, container string) (int, error) {
		var n int
		err := e.as(user).DB.QueryRow(`SELECT count(*) FROM corporate_move_impact(?,?,?,?)`, kind, id, space, container).Scan(&n)
		return n, err
	}
	if n, err := impact("alice", "article", art.ID, b, ""); err != nil || n == 0 {
		t.Fatalf("authorized call: %d %v", n, err)
	}
	// Manager of the source only (no manager on the target b).
	if _, err := impact("editor", "article", art.ID, b, ""); err == nil {
		t.Fatal("manager of the source only may read the diff")
	}
	// Reader of the source, stranger to the target.
	if _, err := impact("u1", "article", art.ID, b, ""); err == nil {
		t.Fatal("reader may read the diff")
	}
	// A user who cannot read the article at all.
	if _, err := impact("u4", "article", art.ID, b, ""); err == nil {
		t.Fatal("stranger may read the diff")
	}
	// Manager on both containers, but a project move needs the owner role.
	proj := e.project(a, "", "P", true)
	e.grant("space", b, "user", "editor", "manager")
	if _, err := impact("editor", "project", proj, b, ""); err == nil {
		t.Fatal("non-owner may read a project move diff")
	}
	if _, err := impact("alice", "bogus", art.ID, b, ""); err == nil {
		t.Fatal("unknown kind accepted")
	}
	// Without a user context the function refuses everything.
	var n int
	if err := e.root.DB.QueryRow(`SELECT count(*) FROM corporate_move_impact('article',?,?,'')`, art.ID, b).Scan(&n); err == nil {
		t.Fatal("anonymous call accepted")
	}
}

func TestMovesRouteNotRegisteredInTrustedMode(t *testing.T) {
	s := testServer(t)
	for _, path := range []string{"/api/moves/preview", "/api/moves/apply"} {
		r := corporateHTTP(s, "POST", path, `{}`)
		if r.Code == 200 || strings.Contains(r.Body.String(), "fingerprint") {
			t.Fatalf("%s served in trusted mode: %d", path, r.Code)
		}
	}
}
