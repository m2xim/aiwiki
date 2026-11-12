package server

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
)

// Agent projects: the model of PLAN-projects.md sections 2-4 on PostgreSQL.

type apEnv struct {
	*movesEnv
}

func newAPEnv(t *testing.T) *apEnv {
	t.Helper()
	m := newMovesEnv(t)
	m.root.objects = newMemStore()
	return &apEnv{movesEnv: m}
}

// call returns status and body of an API request as a user.
func (e *apEnv) call(user, method, path, body string) (int, string) {
	r := corporateHTTP(e.as(user), method, path, body)
	return r.Code, r.Body.String()
}

func (e *apEnv) project(user, name, visibility string, spaces ...[2]string) AgentProjectForTest {
	e.t.Helper()
	rows := make([]string, 0, len(spaces))
	for _, s := range spaces {
		rows = append(rows, fmt.Sprintf(`{"space_id":%q,"mode":%q}`, s[0], s[1]))
	}
	code, body := e.call(user, "POST", "/api/agent-projects", fmt.Sprintf(`{"name":%q,"visibility":%q,"spaces":[%s]}`, name, visibility, strings.Join(rows, ",")))
	if code != 201 {
		e.t.Fatalf("create project as %s: %d %s", user, code, body)
	}
	var p AgentProjectForTest
	if err := json.Unmarshal([]byte(body), &p); err != nil {
		e.t.Fatal(err)
	}
	return p
}

func (e *apEnv) get(user, id string) (int, AgentProjectForTest, string) {
	code, body := e.call(user, "GET", "/api/agent-projects/"+id, "")
	var p AgentProjectForTest
	_ = json.Unmarshal([]byte(body), &p)
	return code, p, body
}

func (e *apEnv) list(user string) map[string]AgentProjectForTest {
	e.t.Helper()
	code, body := e.call(user, "GET", "/api/agent-projects", "")
	if code != 200 {
		e.t.Fatalf("list %s: %d %s", user, code, body)
	}
	var out struct {
		Projects []AgentProjectForTest `json:"projects"`
		General  string                `json:"general_space_id"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		e.t.Fatal(err)
	}
	if out.General == "" {
		e.t.Fatalf("general_space_id missing: %s", body)
	}
	m := map[string]AgentProjectForTest{}
	for _, p := range out.Projects {
		m[p.ID] = p
	}
	return m
}

// keyAs mints a key for the user and returns its token. The auth service is the persistent harness's.
type apKeyEnv struct {
	*persistentEnv
}

func (e *apKeyEnv) rpc(token, project, method string, params any) (int, string) {
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	r := httptest.NewRequest("POST", "http://127.0.0.1:8090/mcp"+project, strings.NewReader(string(b)))
	r.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	e.root.Handler().ServeHTTP(w, r)
	return w.Code, w.Body.String()
}

func (e *apKeyEnv) tool(token, project, name string, args map[string]any) (int, bool, string) {
	code, body := e.rpc(token, project, "tools/call", map[string]any{"name": name, "arguments": args})
	var out struct {
		Result struct {
			IsError bool `json:"isError"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	_ = json.Unmarshal([]byte(body), &out)
	text := body
	if len(out.Result.Content) > 0 {
		text = out.Result.Content[0].Text
	}
	return code, out.Result.IsError, text
}

// Spaces, visibility and the "inaccessible space" rows (PLAN section 3).
func TestAgentProjectsVisibilityAndInaccessibleRows(t *testing.T) {
	e := newAPEnv(t)
	a := e.space("alice") // alice owns
	b := e.space("bob")
	e.grant("space", a, "user", "u1", "reader")
	e.grant("space", a, "user", "u2", "commenter")
	e.grant("space", a, "user", "u3", "editor")

	// Alice cannot add a space she does not see; bob's space is invisible to her.
	if code, body := e.call("alice", "POST", "/api/agent-projects", fmt.Sprintf(`{"name":"x","visibility":"private","spaces":[{"space_id":%q,"mode":"read"}]}`, b)); code != 400 || !strings.Contains(body, "space not visible") || strings.Contains(body, "Private") {
		t.Fatalf("invisible space: %d %s", code, body)
	}
	// But write mode is allowed where she only reads (PLAN section 3).
	e.grant("space", b, "user", "alice", "reader")
	pub := e.project("alice", "Shared", "public", [2]string{a, "write"}, [2]string{b, "write"})
	priv := e.project("alice", "Mine", "private", [2]string{a, "read"})

	// Other users see public projects only; a foreign private project is 404 everywhere.
	if code, _, body := e.get("u1", priv.ID); code != 404 {
		t.Fatalf("foreign private project: %d %s", code, body)
	}
	if _, ok := e.list("u1")[priv.ID]; ok {
		t.Fatal("foreign private project listed")
	}
	if _, ok := e.list("u1")[pub.ID]; !ok {
		t.Fatal("public project not listed")
	}
	if len(e.list("alice")) != 2 {
		t.Fatalf("owner sees %d", len(e.list("alice")))
	}

	// Rows the viewer cannot read are "inaccessible": no id, name or role.
	code, view, raw := e.get("u1", pub.ID)
	if code != 200 || len(view.Spaces) != 2 {
		t.Fatalf("view: %d %s", code, raw)
	}
	if view.CanEdit {
		t.Fatal("a reader may not edit")
	}
	var open, closed int
	for _, r := range view.Spaces {
		if r.Accessible {
			open++
			if r.SpaceID != a || r.MyRole != "reader" || r.Effective != "read" || r.Mode != "write" {
				t.Fatalf("accessible row: %+v", r)
			}
		} else {
			closed++
			if r.SpaceID != "" || r.Name != "" || r.MyRole != "" || r.Effective != "" || r.RowID == "" {
				t.Fatalf("inaccessible row leaks: %+v", r)
			}
		}
	}
	if open != 1 || closed != 1 || strings.Contains(raw, b) {
		t.Fatalf("rows: %d/%d %s", open, closed, raw)
	}
	if strings.Contains(raw, b) {
		t.Fatalf("hidden space id in response: %s", raw)
	}

	// effective = min(project mode, own role) (PLAN section 4).
	for user, want := range map[string]string{"u1": "read", "u2": "comment", "u3": "write", "alice": "write"} {
		_, p, body := e.get(user, pub.ID)
		got := ""
		for _, r := range p.Spaces {
			if r.SpaceID == a {
				got = r.Effective
			}
		}
		if got != want {
			t.Errorf("%s: effective %q want %q (%s)", user, got, want, body)
		}
	}
	// A read row never gives more than read, even to an editor.
	_, p, _ := e.get("alice", priv.ID)
	if p.Spaces[0].Effective != "read" {
		t.Fatalf("read row effective %q", p.Spaces[0].Effective)
	}
	// The administrator without a role sees neither space names nor private projects.
	if code, body := e.call("admin", "GET", "/api/agent-projects/"+priv.ID, ""); code != 404 {
		t.Fatalf("admin on a private project: %d %s", code, body)
	}
	_, ap, abody := e.get("admin", pub.ID)
	for _, r := range ap.Spaces {
		if r.Accessible || r.SpaceID != "" || r.Name != "" {
			t.Fatalf("admin without a role sees a space: %+v", r)
		}
	}
	if strings.Contains(abody, "Private Alice") {
		t.Fatalf("space name leaked to the admin: %s", abody)
	}
}

// Create, edit, delete: who may, validation, row ids, audit.
func TestAgentProjectsWritesValidationAndIDOR(t *testing.T) {
	e := newAPEnv(t)
	a := e.space("alice")
	b := e.space("bob")
	c := e.space("u1")
	e.grant("space", a, "user", "bob", "reader")

	pub := e.project("alice", "Public", "public", [2]string{a, "read"})
	priv := e.project("alice", "Private", "private", [2]string{a, "write"})
	bobs := e.project("bob", "Bobs", "private", [2]string{b, "write"})

	bad := []string{
		`{"name":"","visibility":"private"}`,
		`{"name":"` + strings.Repeat("x", 121) + `","visibility":"private"}`,
		`{"name":"n","visibility":"secret"}`,
		`{"name":"n","visibility":"private","description":"` + strings.Repeat("d", 2001) + `"}`,
		`{"name":"n","visibility":"private","spaces":[{"space_id":"` + a + `","mode":"admin"}]}`,
		`{"name":"n","visibility":"private","spaces":[{"space_id":"` + a + `","mode":"read"},{"space_id":"` + a + `","mode":"write"}]}`,
		`{"name":"n","visibility":"private","spaces":[{"row_id":"r","mode":"read"}]}`,
		`{"name":"n","visibility":"private","owner_id":"bob"}`,
		`{"name":"n","visibility":"private","spaces":[{"space_id":"../x","mode":"read"}]}`,
		`not json`,
	}
	for _, body := range bad {
		if code, resp := e.call("alice", "POST", "/api/agent-projects", body); code != 400 {
			t.Errorf("%s: %d %s", body, code, resp)
		}
	}
	// 21 rows are too many, 20 are fine.
	many := make([]string, 0, 21)
	for i := 0; i < 21; i++ {
		sp := e.space("alice")
		many = append(many, fmt.Sprintf(`{"space_id":%q,"mode":"read"}`, sp))
	}
	if code, body := e.call("alice", "POST", "/api/agent-projects", `{"name":"big","visibility":"private","spaces":[`+strings.Join(many, ",")+`]}`); code != 400 || !strings.Contains(body, "too many spaces") {
		t.Fatalf("21 rows: %d %s", code, body)
	}
	if code, body := e.call("alice", "POST", "/api/agent-projects", `{"name":"big","visibility":"private","spaces":[`+strings.Join(many[:20], ",")+`]}`); code != 201 {
		t.Fatalf("20 rows: %d %s", code, body)
	}

	// Editing: the owner; an administrator only for a public project; nobody else.
	patch := func(user, id, body string) int {
		code, _ := e.call(user, "PATCH", "/api/agent-projects/"+id, body)
		return code
	}
	if patch("alice", priv.ID, `{"name":"Renamed","description":"d"}`) != 200 {
		t.Fatal("owner rename")
	}
	if patch("bob", pub.ID, `{"name":"Hijack"}`) != 403 || patch("bob", priv.ID, `{"name":"Hijack"}`) != 404 {
		t.Fatal("foreign edit")
	}
	if patch("admin", pub.ID, `{"name":"By admin"}`) != 200 {
		t.Fatal("admin edits a public project")
	}
	if patch("admin", pub.ID, `{"visibility":"private"}`) != 403 {
		t.Fatal("admin changed the visibility")
	}
	if patch("admin", priv.ID, `{"name":"x"}`) != 404 {
		t.Fatal("admin edits a private project")
	}
	if code, p, _ := e.get("alice", pub.ID); code != 200 || p.Name != "By admin" || !p.CanEdit {
		t.Fatalf("after admin edit: %+v", p)
	}
	if code, p, _ := e.get("admin", pub.ID); code != 200 || !p.CanEdit {
		t.Fatalf("admin can_edit on public: %+v", p)
	}
	if code, p, _ := e.get("bob", pub.ID); code != 200 || p.CanEdit {
		t.Fatalf("bob can_edit: %+v", p)
	}
	if patch("alice", pub.ID, `{"visibility":"private"}`) != 200 || patch("alice", pub.ID, `{"visibility":"public"}`) != 200 {
		t.Fatal("owner visibility")
	}
	if patch("alice", pub.ID, `{}`) != 400 || patch("alice", pub.ID, `{"owner_id":"bob"}`) != 400 {
		t.Fatal("empty or unknown patch")
	}

	// spaces: {row_id,mode} keeps a row (even one the editor cannot read), {space_id,mode} adds one,
	// anything not listed is removed. A row id from another project is refused.
	_, view, _ := e.get("alice", priv.ID)
	row := view.Spaces[0].RowID
	_, bview, _ := e.get("bob", bobs.ID)
	foreignRow := bview.Spaces[0].RowID
	if patch("alice", priv.ID, fmt.Sprintf(`{"spaces":[{"row_id":%q,"mode":"read"}]}`, foreignRow)) != 400 {
		t.Fatal("row id of another project accepted")
	}
	if patch("alice", priv.ID, fmt.Sprintf(`{"spaces":[{"row_id":%q,"space_id":%q,"mode":"read"}]}`, row, a)) != 400 {
		t.Fatal("row and space together accepted")
	}
	if patch("alice", priv.ID, fmt.Sprintf(`{"spaces":[{"space_id":%q,"mode":"read"}]}`, b)) != 400 {
		t.Fatal("added a space alice cannot see")
	}
	if patch("alice", priv.ID, fmt.Sprintf(`{"spaces":[{"row_id":%q,"mode":"read"},{"space_id":%q,"mode":"write"}]}`, row, c)) != 400 {
		t.Fatal("added another stranger's space")
	}
	e.grant("space", c, "user", "alice", "editor")
	if patch("alice", priv.ID, fmt.Sprintf(`{"spaces":[{"row_id":%q,"mode":"read"},{"space_id":%q,"mode":"write"}]}`, row, c)) != 200 {
		t.Fatal("valid replacement")
	}
	// An editor who loses sight of a row keeps it when its row id is passed back, and cannot learn more.
	e.exec(`DELETE FROM memberships WHERE resource_id=? AND subject_id='alice'`, c)
	_, v2, raw := e.get("alice", priv.ID)
	if strings.Contains(raw, c) {
		t.Fatalf("lost space still visible: %s", raw)
	}
	var hidden string
	for _, r := range v2.Spaces {
		if !r.Accessible {
			hidden = r.RowID
		}
	}
	if hidden == "" || patch("alice", priv.ID, fmt.Sprintf(`{"spaces":[{"row_id":%q,"mode":"read"},{"row_id":%q,"mode":"read"}]}`, row, hidden)) != 200 {
		t.Fatalf("keeping an inaccessible row: %q", hidden)
	}
	var kept int
	if err := e.owner.DB.QueryRow(`SELECT count(*) FROM agent_project_spaces WHERE project_id=? AND space_id=? AND mode='read'`, priv.ID, c).Scan(&kept); err != nil || kept != 1 {
		t.Fatalf("hidden row lost or changed: %d %v", kept, err)
	}
	if patch("alice", priv.ID, fmt.Sprintf(`{"spaces":[{"row_id":%q,"mode":"read"}]}`, row)) != 200 {
		t.Fatal("dropping a row")
	}
	if err := e.owner.DB.QueryRow(`SELECT count(*) FROM agent_project_spaces WHERE project_id=?`, priv.ID).Scan(&kept); err != nil || kept != 1 {
		t.Fatalf("rows after drop: %d", kept)
	}

	// Delete: owner (admin for public); 404 for strangers; keys lose the project.
	if code, _ := e.call("bob", "DELETE", "/api/agent-projects/"+priv.ID, ""); code != 404 {
		t.Fatalf("foreign delete %d", code)
	}
	if code, _ := e.call("bob", "DELETE", "/api/agent-projects/"+pub.ID, ""); code != 403 {
		t.Fatalf("non-owner delete of a public project %d", code)
	}
	if code, _ := e.call("alice", "DELETE", "/api/agent-projects/"+priv.ID, ""); code != 204 {
		t.Fatalf("delete %d", code)
	}
	if code, _ := e.call("alice", "DELETE", "/api/agent-projects/"+priv.ID, ""); code != 404 {
		t.Fatalf("second delete %d", code)
	}
	if code, _ := e.call("admin", "DELETE", "/api/agent-projects/"+pub.ID, ""); code != 204 {
		t.Fatalf("admin delete of a public project %d", code)
	}
	for _, path := range []string{"/api/agent-projects/..%2Fx", "/api/agent-projects/%00"} {
		if code, _ := e.call("alice", "GET", path, ""); code != 404 && code != 400 {
			t.Fatalf("%s: %d", path, code)
		}
	}
	for _, action := range []string{"agent_project.create", "agent_project.update", "agent_project.delete"} {
		if e.auditCount(action, priv.ID) == 0 && e.auditCount(action, pub.ID) == 0 {
			t.Errorf("no audit row %s", action)
		}
	}
	// Method not allowed on the collection.
	if code, _ := e.call("alice", "DELETE", "/api/agent-projects", ""); code != 405 {
		t.Fatalf("collection delete %d", code)
	}
}

// Direct SQL under the runtime role honours the same rules (the API is not the only guard).
func TestAgentProjectsRowLevelSecurityDirectSQL(t *testing.T) {
	e := newAPEnv(t)
	a := e.space("alice")
	pub := e.project("alice", "Public", "public", [2]string{a, "write"})
	priv := e.project("alice", "Private", "private", [2]string{a, "write"})
	count := func(user, q string, args ...any) int {
		var n int
		if err := e.as(user).DB.QueryRow(q, args...).Scan(&n); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		return n
	}
	if count("bob", `SELECT count(*) FROM agent_projects`) != 1 || count("bob", `SELECT count(*) FROM agent_project_spaces`) != 1 || count("u1", `SELECT count(*) FROM agent_projects WHERE id=?`, priv.ID) != 0 {
		t.Fatal("bob sees more than the public project")
	}
	if count("alice", `SELECT count(*) FROM agent_projects`) != 2 || count("admin", `SELECT count(*) FROM agent_projects WHERE id=?`, priv.ID) != 0 {
		t.Fatal("owner or admin visibility")
	}
	denied := func(name, user, q string, args ...any) {
		t.Helper()
		res, err := e.as(user).DB.Exec(q, args...)
		if err == nil {
			if n, _ := res.RowsAffected(); n != 0 {
				t.Errorf("%s: succeeded (%d rows)", name, n)
			}
		}
	}
	denied("stranger rename", "bob", `UPDATE agent_projects SET name='x' WHERE id=?`, pub.ID)
	denied("stranger delete", "bob", `DELETE FROM agent_projects WHERE id=?`, pub.ID)
	denied("insert as another owner", "bob", `INSERT INTO agent_projects(id,name,owner_id,visibility) VALUES('x1','n','alice','private')`)
	denied("stranger adds a row", "bob", `INSERT INTO agent_project_spaces(project_id,row_id,space_id,mode) VALUES(?, 'rr', ?, 'read')`, pub.ID, a)
	denied("stranger changes a row", "bob", `UPDATE agent_project_spaces SET mode='read' WHERE project_id=?`, pub.ID)
	denied("stranger deletes a row", "bob", `DELETE FROM agent_project_spaces WHERE project_id=?`, pub.ID)
	denied("owner changes the owner", "alice", `UPDATE agent_projects SET owner_id='bob' WHERE id=?`, pub.ID)
	denied("owner moves a row between projects", "alice", `UPDATE agent_project_spaces SET project_id=? WHERE project_id=?`, priv.ID, pub.ID)
	denied("admin changes visibility", "admin", `UPDATE agent_projects SET visibility='private' WHERE id=?`, pub.ID)
	// A space the writer cannot read cannot be added, even by the owner of the project.
	bsp := e.space("bob")
	denied("owner adds a hidden space", "alice", `INSERT INTO agent_project_spaces(project_id,row_id,space_id,mode) VALUES(?, 'rr2', ?, 'read')`, pub.ID, bsp)
	if n := count("alice", `SELECT count(*) FROM agent_project_spaces WHERE space_id=?`, bsp); n != 0 {
		t.Fatalf("hidden space was added: %d", n)
	}
	// The creation of a project is limited to the live caller.
	if _, err := e.as("alice").DB.Exec(`INSERT INTO agent_projects(id,name,owner_id,visibility) VALUES('okp','n','alice','private')`); err != nil {
		t.Fatalf("own insert: %v", err)
	}
	// Unauthenticated session sees nothing.
	var n int
	if err := e.root.DB.QueryRow(`SELECT count(*) FROM agent_projects`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("anonymous session: %d %v", n, err)
	}
	// The SECURITY DEFINER views refuse an unauthenticated session too.
	if err := e.root.DB.QueryRow(`SELECT count(*) FROM corporate_agent_projects_visible(NULL)`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("anonymous visible(): %d %v", n, err)
	}
	if err := e.root.DB.QueryRow(`SELECT count(*) FROM corporate_agent_project_rows(NULL)`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("anonymous rows(): %d %v", n, err)
	}
	// corporate_agent_scope_rows is closed for the runtime role in a plain session.
	if err := e.as("alice").DB.QueryRow(`SELECT count(*) FROM corporate_agent_scope_rows('alice',?,false)`, pub.ID).Scan(&n); err == nil {
		t.Fatal("scope rows callable")
	}
	// A disabled user sees nothing and cannot write.
	e.exec(`UPDATE users SET disabled=1 WHERE id='alice'`)
	if count("alice", `SELECT count(*) FROM agent_projects`) != 0 {
		t.Fatal("disabled owner sees projects")
	}
	denied("disabled owner edits", "alice", `UPDATE agent_projects SET name='x' WHERE id=?`, priv.ID)
}

// Personal spaces, the administrator and the limited catalog (PLAN section 2).
func TestPersonalSpacesAndAdministrator(t *testing.T) {
	e := newAPEnv(t)
	mine := e.space("alice") // personal while alone
	type spaceView struct {
		ID            string `json:"id"`
		Name          string `json:"name"`
		Description   string `json:"description"`
		Personal      bool   `json:"personal"`
		Limited       bool   `json:"limited"`
		OwnerName     string `json:"owner_name"`
		EffectiveRole string `json:"effective_role"`
		ArticleCount  int    `json:"article_count"`
		Tags          []string
	}
	catalog := func(user string) map[string]spaceView {
		code, body := e.call(user, "GET", "/api/catalog", "")
		if code != 200 {
			t.Fatalf("catalog %s: %d %s", user, code, body)
		}
		var list []spaceView
		if err := json.Unmarshal([]byte(body), &list); err != nil {
			t.Fatal(err)
		}
		out := map[string]spaceView{}
		for _, s := range list {
			out[s.ID] = s
		}
		return out
	}
	e.article("alice", mine, "", "", "Secret article")
	if !catalog("alice")[mine].Personal {
		t.Fatal("a space with its owner alone is personal")
	}
	// Another member ends the personal state; no flag is stored.
	e.grant("space", mine, "user", "bob", "reader")
	if catalog("alice")[mine].Personal {
		t.Fatal("a shared space is not personal")
	}
	e.exec(`DELETE FROM memberships WHERE resource_id=? AND subject_id='bob'`, mine)
	if !catalog("alice")[mine].Personal {
		t.Fatal("personal again")
	}
	// A team member also ends it.
	e.team("tt", "u1")
	e.grant("space", mine, "team", "tt", "reader")
	if catalog("alice")[mine].Personal {
		t.Fatal("a team grant ends the personal state")
	}
	e.exec(`DELETE FROM memberships WHERE resource_id=? AND subject_id='tt'`, mine)

	// Administrator without a role: a bare directory entry, never content.
	ad := catalog("admin")[mine]
	if !ad.Limited || ad.ID != mine || ad.Name != "Private Alice" || ad.OwnerName != "alice" || !ad.Personal {
		t.Fatalf("admin directory entry: %+v", ad)
	}
	_, raw := e.call("admin", "GET", "/api/catalog", "")
	for _, leak := range []string{"Secret article", `"description"`, `"tags"`, `"article_count"`, `"effective_role"`} {
		if strings.Contains(raw, leak) {
			t.Fatalf("limited entry leaks %s: %s", leak, raw)
		}
	}
	if _, raw = e.call("admin", "GET", "/api/spaces", ""); strings.Contains(raw, mine) || strings.Contains(raw, "Private Alice") {
		t.Fatalf("/api/spaces lists a space without a role: %s", raw)
	}
	// Non-admins never get limited entries.
	if _, ok := catalog("bob")[mine]; ok {
		t.Fatal("bob sees alice's space")
	}
	for _, path := range []string{"/api/articles", "/api/search?q=Secret", "/api/bootstrap", "/api/tags?space_id=" + mine, "/api/spaces/" + mine + "/audit", "/api/spaces/" + mine + "/storage"} {
		code, body := e.call("admin", "GET", path, "")
		if strings.Contains(body, "Secret article") || (code == 200 && strings.Contains(body, `"Private Alice"`)) {
			t.Fatalf("admin reads content through %s: %d %s", path, code, body)
		}
	}
	if code, _ := e.call("admin", "GET", "/api/articles?space_id="+mine, ""); code != 200 {
		t.Fatalf("articles list %d", code)
	}
	if _, body := e.call("admin", "GET", "/api/articles?space_id="+mine, ""); strings.Contains(body, "Secret") {
		t.Fatal("admin lists articles of a space without a role")
	}

	// The administrator manages the roles of the space, audited, without gaining content access.
	access := "/api/access?resource_type=space&resource_id=" + mine
	if code, body := e.call("admin", "GET", access, ""); code != 200 || !strings.Contains(body, `"alice"`) || strings.Contains(body, "Secret") {
		t.Fatalf("admin reads grants: %d %s", code, body)
	}
	if code, body := e.call("bob", "GET", access, ""); code != 404 {
		t.Fatalf("stranger reads grants: %d %s", code, body)
	}
	if code, body := e.call("admin", "PUT", access, `{"grant":{"subject_type":"user","subject_id":"admin","role":"owner"}}`); code != 200 {
		t.Fatalf("admin grants themselves: %d %s", code, body)
	}
	if e.auditCount("access.upsert", mine) != 1 {
		t.Fatal("grant not audited")
	}
	var detail string
	if err := e.owner.DB.QueryRow(`SELECT details::text FROM security_audit WHERE action='access.upsert' AND resource_id=?`, mine).Scan(&detail); err != nil || !strings.Contains(detail, "admin") || !strings.Contains(detail, "via_admin") {
		t.Fatalf("audit details %s %v", detail, err)
	}
	// Now the admin has a role: the space is no longer limited and is not personal (two owners).
	got := catalog("admin")[mine]
	if got.Limited || got.EffectiveRole != "owner" || got.Personal {
		t.Fatalf("after self-grant: %+v", got)
	}
	// The admin can also remove roles, including the last other owner? the last-owner rule still protects.
	if code, _ := e.call("admin", "DELETE", access+"&subject_type=user&subject_id=alice", ""); code != 204 {
		t.Fatalf("admin removes the previous owner: %d", code)
	}
	if code, body := e.call("admin", "DELETE", access+"&subject_type=user&subject_id=admin", ""); code != 409 {
		t.Fatalf("last owner removal: %d %s", code, body)
	}
	// A non-admin without a role cannot do any of it; a plain manager cannot touch owners.
	e.grant("space", mine, "user", "bob", "manager")
	if code, _ := e.call("bob", "PUT", access, `{"grant":{"subject_type":"user","subject_id":"bob","role":"owner"}}`); code != 403 {
		t.Fatalf("manager self-promotion %d", code)
	}
	// Not under MCP: the same endpoint refuses a scoped session even for an admin.
	adminKey := *e.as("admin")
	adminKey.principal = &Principal{ID: "admin", Admin: true, MCPScope: spaceScope("write", mine), MCPDelegationID: "pat_x"}
	if code := corporateHTTP(&adminKey, "GET", access, "").Code; code != 403 && code != 404 {
		t.Fatalf("scoped admin: %d", code)
	}
	if code := corporateHTTP(&adminKey, "GET", "/api/catalog", "").Code; code == 200 {
		body := corporateHTTP(&adminKey, "GET", "/api/catalog", "").Body.String()
		if strings.Contains(body, `"limited"`) {
			t.Fatalf("MCP session got directory entries: %s", body)
		}
	}
	// Disabled admin loses it.
	e.exec(`UPDATE users SET disabled=1 WHERE id='admin'`)
	if code, _ := e.call("admin", "GET", "/api/catalog", ""); code != 403 {
		t.Fatalf("disabled admin %d", code)
	}
}

// The general space may be any live space, but never a personal one.
func TestGeneralSpaceCannotBePersonal(t *testing.T) {
	e := newAPEnv(t)
	solo := e.space("alice")
	shared := e.space("bob")
	e.grant("space", shared, "user", "alice", "reader")
	put := func(id string) int {
		code, _ := e.call("admin", "PUT", "/api/admin/settings", fmt.Sprintf(`{"general_space_id":%q}`, id))
		return code
	}
	if put(solo) != 400 {
		t.Fatal("a personal space was made general")
	}
	if put("no-such-space") != 400 {
		t.Fatal("unknown space made general")
	}
	if put(shared) != 200 {
		t.Fatal("a shared space refused as general")
	}
	// Once general, its single-owner shape no longer counts as personal.
	e.exec(`DELETE FROM memberships WHERE resource_id=? AND subject_id='alice'`, shared)
	_, raw := e.call("bob", "GET", "/api/catalog", "")
	var list []struct {
		ID       string `json:"id"`
		Personal bool   `json:"personal"`
	}
	_ = json.Unmarshal([]byte(raw), &list)
	for _, s := range list {
		if s.ID == shared && s.Personal {
			t.Fatal("the general space reported as personal")
		}
	}
}

// Old folders are gone from every API.
func TestFoldersInsideSpacesAreRemoved(t *testing.T) {
	e := newAPEnv(t)
	a := e.space("alice")
	for _, c := range []struct{ method, path, body string }{
		{"GET", "/api/projects", ""},
		{"POST", "/api/projects", `{"space_id":"` + a + `","name":"x"}`},
		{"PATCH", "/api/projects/x", `{"name":"x"}`},
		{"GET", "/api/projects/x", ""},
	} {
		if code, body := e.call("alice", c.method, c.path, c.body); code != 410 || !strings.Contains(body, "projects inside spaces were removed") {
			t.Errorf("%s %s: %d %s", c.method, c.path, code, body)
		}
	}
	for _, body := range []string{
		`{"space_id":"` + a + `","project_id":"p","title":"t"}`,
	} {
		if code, resp := e.call("alice", "POST", "/api/articles", body); code != 400 || !strings.Contains(resp, "projects inside spaces were removed") {
			t.Errorf("article with project_id: %d %s", code, resp)
		}
	}
	art := e.article("alice", a, "", "", "Doc")
	if code, resp := e.call("alice", "PUT", "/api/articles/"+art.ID, `{"base_revision":1,"project_id":"p"}`); code != 400 || !strings.Contains(resp, "projects inside spaces were removed") {
		t.Errorf("update with project_id: %d %s", code, resp)
	}
	if code, resp := e.call("alice", "POST", "/api/artifacts", `{"space_id":"`+a+`","project_id":"p","title":"t","files":[{"path":"index.html","content":"x"}]}`); code != 400 {
		t.Errorf("artifact with project_id: %d %s", code, resp)
	}
	if code, resp := e.call("alice", "PUT", "/api/access?resource_type=project&resource_id=p", `{"grant":{"subject_type":"user","subject_id":"bob","role":"reader"}}`); code != 410 {
		t.Errorf("grant on project: %d %s", code, resp)
	}
	if code, _ := e.call("alice", "GET", "/api/access/effective?resource_type=project&resource_id=p", ""); code != 410 {
		t.Errorf("effective on project: %d", code)
	}
	for _, path := range []string{"/api/articles/" + art.ID, "/api/articles?space_id=" + a, "/api/search?q=Doc", "/api/bootstrap", "/api/catalog"} {
		if _, body := e.call("alice", "GET", path, ""); strings.Contains(body, "project_id") || strings.Contains(body, `"projects"`) {
			t.Errorf("%s mentions projects: %s", path, body)
		}
	}
	// The database refuses folders and project grants too.
	if _, err := e.owner.DB.Exec(`INSERT INTO projects(id,space_id,name) VALUES('zz',?, 'z')`, a); err == nil {
		t.Error("project row inserted")
	}
	if _, err := e.owner.DB.Exec(`INSERT INTO memberships(resource_type,resource_id,subject_type,subject_id,role) VALUES('project','zz','user','bob','reader')`); err == nil {
		t.Error("project grant inserted")
	}
	if _, err := e.as("alice").DB.Exec(`UPDATE articles SET project_id='zz' WHERE id=?`, art.ID); err == nil {
		t.Error("article moved into a folder")
	}
}
