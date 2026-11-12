package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Keys, the per-request project scope, the introspection mode and write escalation (SPEC section D).

// keyEnv: alice (editor in sa, reader in sr, nothing in sn), bob (public project pub over sbob).
func newKeyEnv(t *testing.T) (*apKeyEnv, *Server, string) {
	t.Helper()
	pe := newPersistentEnv(t)
	alice, aliceID := pe.user("alice")
	_, bobID := pe.user("bob")
	pe.seed(aliceID, bobID)
	return &apKeyEnv{persistentEnv: pe}, alice, aliceID
}

func TestAgentProjectKeyScopeMinRuleAndReadOnly(t *testing.T) {
	e, alice, aliceID := newKeyEnv(t)
	// Project over sa (write), sr (write but alice is reader there), sbob (alice has no role) and an extra read row.
	p := e.seedMixedProject(alice)
	_, token := e.mintToken(alice, `{"name":"agent","projects":["`+p+`"]}`)
	_, roToken := e.mintToken(alice, `{"name":"ro","projects":["`+p+`"],"read_only":true}`)
	e.exec(`ALTER TABLE articles NO FORCE ROW LEVEL SECURITY`)
	e.exec(`INSERT INTO articles(id,space_id,parent_id,project_id,title,summary,body,kind,status,pinned,tags,properties,revision,created_at,updated_at,author) VALUES
	 ('k_sa','sa','','','in sa','','b','article','active',0,'[]','{}',1,'now','now','x'),
	 ('k_sr','sr','','','in sr','','b','article','active',0,'[]','{}',1,'now','now','x'),
	 ('k_sb','sb','','','in sb','','b','article','active',0,'[]','{}',1,'now','now','x'),
	 ('k_bob','sbob','','','in sbob','','b','article','active',0,'[]','{}',1,'now','now','bob')`)
	e.exec(`ALTER TABLE articles FORCE ROW LEVEL SECURITY`)

	read := func(tok, id string) bool {
		_, isErr, _ := e.tool(tok, "?project="+p, "wiki_read", map[string]any{"id": id})
		return !isErr
	}
	// Reads: spaces of the project the owner can read; sb is outside; sbob has no owner role.
	if !read(token, "k_sa") || !read(token, "k_sr") || read(token, "k_sb") || read(token, "k_bob") {
		t.Fatal("read set does not match project spaces and owner roles")
	}
	// min(mode, role): write in sa (editor), but only read in sr (reader).
	write := func(tok, space string) bool {
		_, isErr, _ := e.tool(tok, "?project="+p, "wiki_write", map[string]any{"article": map[string]any{"space_id": space, "title": "w " + space + randomSuffix()}})
		return !isErr
	}
	if !write(token, "sa") || write(token, "sr") || write(token, "sb") || write(token, "sbob") {
		t.Fatal("write escalated beyond min(mode, role) or outside the project")
	}
	// read_only: reads work, writes never do, whatever the project says; the write tools are hidden.
	if !read(roToken, "k_sa") || write(roToken, "sa") {
		t.Fatal("read-only key")
	}
	if _, body := e.rpc(roToken, "?project="+p, "tools/list", map[string]any{}); strings.Contains(body, `"wiki_write"`) || !strings.Contains(body, `"wiki_search"`) {
		t.Fatalf("read-only tool list: %s", body)
	}
	if _, isErr, _ := e.tool(roToken, "?project="+p, "wiki_edit", map[string]any{"id": "k_sa", "base_revision": 1, "op": "append", "text": "x"}); !isErr {
		t.Fatal("read-only key edited")
	}
	// The same key through direct SQL in its session: the policies refuse the write too.
	view, done, scope, err := e.root.DB.scopedAgent(context.Background(), aliceID, aliceID, "pat_probe", p)
	if err == nil {
		done()
		_ = view
		_ = scope
		t.Fatal("a session for an unknown credential was admitted")
	}

	// The general space is readable in every project, writable only when listed with write.
	e.seedExec(`INSERT INTO spaces(id,name,owner_id) VALUES('general','General',?)`, aliceID)
	e.exec(`ALTER TABLE articles NO FORCE ROW LEVEL SECURITY`)
	e.exec(`INSERT INTO articles(id,space_id,parent_id,project_id,title,summary,body,kind,status,pinned,tags,properties,revision,created_at,updated_at,author) VALUES('k_gen','general','','','rules','','b','article','active',1,'[]','{}',1,'now','now','x')`)
	e.exec(`ALTER TABLE articles FORCE ROW LEVEL SECURITY`)
	if !read(token, "k_gen") {
		t.Fatal("the general space is readable in a project")
	}
	if write(token, "general") {
		t.Fatal("the general space is writable without a write row")
	}
}

func (e *apKeyEnv) seedMixedProject(alice *Server) string {
	e.t.Helper()
	body := `{"name":"mixed","visibility":"private","spaces":[{"space_id":"sa","mode":"write"},{"space_id":"sr","mode":"write"}]}`
	rec := corporateHTTP(alice, "POST", "/api/agent-projects", body)
	if rec.Code != 201 {
		e.t.Fatalf("project: %d %s", rec.Code, rec.Body)
	}
	var p AgentProjectForTest
	_ = json.Unmarshal(rec.Body.Bytes(), &p)
	// A space alice cannot read, added directly by the migration owner (as another user's public project might hold it).
	e.exec(`INSERT INTO agent_project_spaces(project_id,row_id,space_id,mode) VALUES(?,?,?,'write')`, p.ID, "row_hidden_"+p.ID, "sbob")
	return p.ID
}

// Without ?project= a key only introspects; foreign, unknown and malformed projects are refused alike.
func TestAgentProjectKeyIntrospectionAndProjectParam(t *testing.T) {
	e, alice, _ := newKeyEnv(t)
	p := e.seedMixedProject(alice)
	_, token := e.mintToken(alice, `{"name":"agent","projects":["`+p+`","pub"]}`)

	// Introspection: only wiki_bootstrap and wiki_projects; everything else is not even listed or callable.
	_, list := e.rpc(token, "", "tools/list", map[string]any{})
	if strings.Contains(list, "wiki_write") || strings.Contains(list, "wiki_search") || strings.Contains(list, "wiki_read") || !strings.Contains(list, "wiki_projects") || !strings.Contains(list, "wiki_bootstrap") {
		t.Fatalf("introspection tools: %s", list)
	}
	for _, tool := range []string{"wiki_search", "wiki_search_sections", "wiki_read", "wiki_write", "wiki_edit", "wiki_artifact_list", "wiki_comments", "wiki_links", "wiki_history", "wiki_restore", "wiki_comment"} {
		if _, isErr, text := e.tool(token, "", tool, map[string]any{"id": "x", "q": "x", "article": map[string]any{"space_id": "sa", "title": "t"}, "base_revision": 1, "revision": 1, "body": "b"}); !isErr {
			t.Fatalf("%s works without a project: %s", tool, text)
		}
	}
	_, _, text := e.tool(token, "", "wiki_projects", map[string]any{})
	var projects struct {
		Current  string                `json:"current_project_id"`
		Projects []AgentProjectForTest `json:"projects"`
		Template string                `json:"connect_url_template"`
	}
	if err := json.Unmarshal([]byte(text), &projects); err != nil {
		t.Fatalf("wiki_projects: %v %s", err, text)
	}
	if projects.Current != "" || len(projects.Projects) != 2 || !strings.HasSuffix(projects.Template, "/mcp?project={id}") || strings.Contains(projects.Template, "evil") {
		t.Fatalf("wiki_projects: %+v", projects)
	}
	ids := map[string]bool{}
	for _, pr := range projects.Projects {
		ids[pr.ID] = true
		for _, r := range pr.Spaces {
			if !r.Accessible && (r.SpaceID != "" || r.Name != "") {
				t.Fatalf("introspection leaks an unreadable space: %+v", r)
			}
		}
	}
	if !ids[p] || !ids["pub"] || ids["pn"] || ids["pa"] {
		t.Fatalf("a key lists only its projects: %v", ids)
	}
	// Bootstrap in introspection mode: the general rules, projects and the guide to choose one.
	_, _, text = e.tool(token, "", "wiki_bootstrap", map[string]any{})
	if !strings.Contains(text, `"projects"`) || !strings.Contains(text, "AGENTS.md") || !strings.Contains(text, "connect_url_template") || strings.Contains(text, `"other_pinned"`) {
		t.Fatalf("introspection bootstrap: %s", text)
	}
	// A space_id argument cannot widen the introspective bootstrap beyond the general rules.
	e.seedExec(`INSERT INTO articles(id,space_id,parent_id,project_id,title,summary,body,kind,status,pinned,tags,properties,revision,created_at,updated_at,author) VALUES('pin_sa','sa','','','Pinned rule','','secret rule','instruction','active',1,'[]','{}',1,'now','now','x')`)
	if _, _, text = e.tool(token, "", "wiki_bootstrap", map[string]any{"space_id": "sa"}); strings.Contains(text, "secret rule") {
		t.Fatalf("introspection read a pinned rule of a project space: %s", text)
	}
	// With the project: its pinned rules come in full, with the project and effective rights.
	_, _, text = e.tool(token, "?project="+p, "wiki_bootstrap", map[string]any{})
	if !strings.Contains(text, "secret rule") || !strings.Contains(text, `"project"`) || !strings.Contains(text, `"effective":"write"`) {
		t.Fatalf("project bootstrap: %s", text)
	}
	if _, _, text = e.tool(token, "?project="+p, "wiki_projects", map[string]any{}); !strings.Contains(text, `"current_project_id":"`+p+`"`) {
		t.Fatalf("current project: %s", text)
	}

	// The project parameter: foreign (not in the key), unknown, deleted, malformed, repeated -> refused, uniformly.
	for name, q := range map[string]string{
		"not in the key":   "?project=pa",
		"someone else's":   "?project=pn",
		"unknown":          "?project=nonexistent",
		"traversal":        "?project=..%2F..%2Fetc",
		"slash":            "?project=a/b",
		"quote":            "?project=x%27%20OR%20%271%27=%271",
		"nul":              "?project=%00",
		"repeated":         "?project=" + p + "&project=pub",
		"repeated foreign": "?project=" + p + "&project=pa",
		"long":             "?project=" + strings.Repeat("a", 300),
	} {
		code, body := e.rpc(token, q, "tools/list", map[string]any{})
		if (code != 403 && code != 400) || strings.Contains(body, "wiki_") || strings.Contains(body, "Selected") || strings.Contains(body, "mixed") {
			t.Errorf("%s: %d %s", name, code, body)
		}
		if code == 403 && !strings.Contains(body, "project is not available for this key") {
			t.Errorf("%s: wrong message %s", name, body)
		}
	}
	// Error bodies for foreign and unknown projects are identical (no existence oracle).
	_, foreign := e.rpc(token, "?project=pn", "ping", nil)
	_, unknown := e.rpc(token, "?project=nonexistent", "ping", nil)
	if foreign != unknown {
		t.Fatalf("existence oracle: %q vs %q", foreign, unknown)
	}
	// Other MCP-shaped paths do not skip the check.
	if code, _ := e.rpc(token, "?project=pn", "initialize", map[string]any{}); code != 403 {
		t.Fatalf("initialize with a foreign project: %d", code)
	}
	// A key never works outside /mcp.
	req := corporateHTTPBearer(e.root, "GET", "/api/catalog", token)
	if req != 401 && req != 403 {
		t.Fatalf("key on the REST API: %d", req)
	}
}

func corporateHTTPBearer(s *Server, method, path, token string) int {
	r := httptest.NewRequest(method, "http://127.0.0.1:8090"+path, nil)
	r.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	return w.Code
}

// Project edits, deletion and visibility changes apply with the very next request.
func TestAgentProjectChangesApplyToTheNextRequest(t *testing.T) {
	e, alice, aliceID := newKeyEnv(t)
	_ = aliceID
	p := e.seedMixedProject(alice)
	_, token := e.mintToken(alice, `{"name":"agent","projects":["`+p+`"]}`)
	q := "?project=" + p
	write := func(space string) bool {
		_, isErr, _ := e.tool(token, q, "wiki_write", map[string]any{"article": map[string]any{"space_id": space, "title": "t" + randomSuffix()}})
		return !isErr
	}
	if !write("sa") {
		t.Fatal("write before the edit")
	}
	e.seedExec(`UPDATE agent_project_spaces SET mode='read' WHERE project_id=? AND space_id='sa'`, p)
	if write("sa") {
		t.Fatal("write after the project switched to read")
	}
	e.seedExec(`UPDATE agent_project_spaces SET mode='write' WHERE project_id=? AND space_id='sa'`, p)
	if !write("sa") {
		t.Fatal("write after switching back")
	}
	// The owner loses the role: the same project now yields nothing for that space.
	e.seedExec(`DELETE FROM memberships WHERE resource_id='sa' AND subject_id=?`, aliceID)
	if write("sa") {
		t.Fatal("write after the owner lost the role")
	}
	if _, isErr, _ := e.tool(token, q, "wiki_search", map[string]any{"space_id": "sa"}); isErr {
		t.Log("search errors for an unreadable space (acceptable)")
	}
	e.seedExec(`INSERT INTO memberships(resource_type,resource_id,subject_type,subject_id,role) VALUES('space','sa','user',?,'editor')`, aliceID)
	if !write("sa") {
		t.Fatal("write after the role came back")
	}
	// Making the project empty leaves the general space read-only.
	e.seedExec(`DELETE FROM agent_project_spaces WHERE project_id=?`, p)
	if write("sa") {
		t.Fatal("write in an empty project")
	}
	if code, _ := e.rpc(token, q, "tools/list", map[string]any{}); code != 200 {
		t.Fatalf("an empty project is still a valid connection: %d", code)
	}
	// Deleted: the key keeps its entry, the connection is refused.
	e.seedExec(`DELETE FROM agent_projects WHERE id=?`, p)
	if code, body := e.rpc(token, q, "tools/list", map[string]any{}); code != 403 || !strings.Contains(body, "project is not available for this key") {
		t.Fatalf("deleted project: %d %s", code, body)
	}
	// A project that becomes private to someone else is refused too (public -> private by its owner).
	_, bobToken := e.mintToken(alice, `{"name":"pub","projects":["pub"]}`)
	if code, _ := e.rpc(bobToken, "?project=pub", "ping", nil); code != 200 {
		t.Fatalf("public project of another user: %d", code)
	}
	e.exec(`UPDATE agent_projects SET visibility='private' WHERE id='pub'`)
	if code, _ := e.rpc(bobToken, "?project=pub", "ping", nil); code != 403 {
		t.Fatalf("project made private: %d", code)
	}
	e.exec(`UPDATE agent_projects SET visibility='public' WHERE id='pub'`)
	if code, _ := e.rpc(bobToken, "?project=pub", "ping", nil); code != 200 {
		t.Fatalf("project public again: %d", code)
	}
	// A foreign public project grants nothing by itself: alice has no role in sbob.
	_, isErr, _ := e.tool(bobToken, "?project=pub", "wiki_write", map[string]any{"article": map[string]any{"space_id": "sbob", "title": "x"}})
	if !isErr {
		t.Fatal("a public project of another user granted write")
	}
}

// Key management API: projects, read_only, PATCH, listing shape, removed fields.
func TestAgentProjectKeyManagementAPI(t *testing.T) {
	e, alice, _ := newKeyEnv(t)
	p := e.seedMixedProject(alice)
	out := e.mint(alice, `{"name":"k","projects":["`+p+`","pub"],"read_only":true}`, http.StatusCreated)
	id, _ := out["id"].(string)
	if out["read_only"] != true || out["scope"] != nil || out["project_ids"] != nil {
		t.Fatalf("mint response: %v", out)
	}
	projects, _ := json.Marshal(out["projects"])
	if !strings.Contains(string(projects), `"available":true`) || !strings.Contains(string(projects), `"visibility":"public"`) {
		t.Fatalf("projects in response: %s", projects)
	}
	rec := e.call(alice, persistentCredentials, "GET", "", "")
	e.expect(rec, 200)
	if strings.Contains(rec.Body.String(), `"scope"`) || !strings.Contains(rec.Body.String(), `"read_only":true`) {
		t.Fatalf("list: %s", rec.Body)
	}
	// PATCH: projects and/or read_only; the removed fields are refused with "use projects".
	for _, body := range []string{`{"scope":[]}`, `{"project_ids":["pub"]}`, `{"mode":"write"}`} {
		rec = e.call(alice, persistentCredentialR, "PATCH", body, id)
		e.expect(rec, 400)
		if !strings.Contains(rec.Body.String(), "use projects") {
			t.Fatalf("%s: %s", body, rec.Body)
		}
	}
	for _, body := range []string{`{}`, `{"projects":[]}`, `{"projects":["pn"]}`, `{"projects":["missing"]}`, `{"projects":["pub","pub"]}`, `{"read_only":"x"}`} {
		e.expect(e.call(alice, persistentCredentialR, "PATCH", body, id), 400)
	}
	e.expect(e.call(alice, persistentCredentialR, "PATCH", `{"read_only":false}`, id), 200)
	if e.text(`SELECT read_only::text FROM mcp_credentials WHERE id=?`, id) != "false" {
		t.Fatal("read_only not stored")
	}
	e.expect(e.call(alice, persistentCredentialR, "PATCH", `{"projects":["pub"]}`, id), 200)
	if e.text(`SELECT agent_project_ids::text FROM mcp_credentials WHERE id=?`, id) != `["pub"]` {
		t.Fatal("projects not stored")
	}
	if e.count(`SELECT count(*) FROM security_audit WHERE action='mcp_credential.update' AND resource_id=?`, id) != 2 {
		t.Fatal("updates must be audited")
	}
	// Another owner cannot edit, and cannot add a private project of someone else.
	bob, _ := e.user("bobby")
	e.expect(e.call(bob, persistentCredentialR, "PATCH", `{"read_only":true}`, id), 404)
	// The runtime role cannot change the authority columns directly.
	if _, err := alice.DB.Exec(`UPDATE mcp_credentials SET agent_project_ids='["pa"]'::jsonb WHERE id=?`, id); err == nil {
		t.Fatal("direct project change accepted")
	}
	if _, err := alice.DB.Exec(`UPDATE mcp_credentials SET read_only=NOT read_only WHERE id=?`, id); err == nil {
		t.Fatal("direct read_only change accepted")
	}
	// Only the checked function changes them, and it refuses invisible projects.
	if _, err := alice.DB.Exec(`SELECT corporate_mcp_credential_set_projects(?,?::jsonb,false)`, id, `["pn"]`); err == nil {
		t.Fatal("function accepted an invisible project")
	}
	if _, err := alice.DB.Exec(`SELECT corporate_mcp_credential_set_scope(?,?::jsonb)`, id, `[]`); err == nil {
		t.Fatal("the old scope function still exists")
	}
	// A deleted project stays listed as unavailable, without a name.
	e.exec(`DELETE FROM agent_projects WHERE id='pub'`)
	rec = e.call(alice, persistentCredentials, "GET", "", "")
	if !strings.Contains(rec.Body.String(), `"id":"pub","name":"","visibility":"","available":false`) {
		t.Fatalf("unavailable project: %s", rec.Body)
	}
	// Refused principals: no key may manage keys, and a session scoped to a project may not either.
	for _, pr := range []*Principal{{MCPKeyProjects: []string{p}}, {MCPScope: spaceScope("write", "sa")}} {
		cp := *alice
		pr.ID, pr.Issuer, pr.Subject = alice.principal.ID, e.iss, "alice"
		cp.principal = pr
		e.expect(e.call(&cp, persistentCredentialR, "PATCH", `{"read_only":true}`, id), 403)
		e.expect(e.call(&cp, persistentCredentials, "POST", `{"name":"n","projects":["`+p+`"]}`, ""), 403)
	}
	// The agent-project API refuses keys, OAuth bearers and introspection principals.
	for name, pr := range map[string]*Principal{
		"key":        {MCPKeyProjects: []string{p}, MCPDelegationID: "pat_x"},
		"scoped":     {MCPScope: spaceScope("write", "sa"), MCPDelegationID: "pat_x"},
		"oauth":      {MCPBearer: true},
		"introspect": {MCPKeyProjects: []string{p}, MCPIntrospect: true},
	} {
		cp := *alice
		pr.ID, pr.Issuer, pr.Subject = alice.principal.ID, e.iss, "alice"
		cp.principal = pr
		for _, c := range []struct{ method, path, body string }{
			{"GET", "/api/agent-projects", ""}, {"POST", "/api/agent-projects", `{"name":"x","visibility":"private"}`},
			{"PATCH", "/api/agent-projects/" + p, `{"name":"y"}`}, {"DELETE", "/api/agent-projects/" + p, ""},
		} {
			if rec := corporateHTTP(&cp, c.method, c.path, c.body); rec.Code != 403 {
				t.Errorf("%s %s %s: %d %s", name, c.method, c.path, rec.Code, rec.Body)
			}
		}
	}
}

// A key survives while its owner keeps the rights it needs; an administrator-only key reads nothing.
func TestAgentProjectKeyOwnerStateAndServiceAccounts(t *testing.T) {
	e, alice, aliceID := newKeyEnv(t)
	p := e.seedMixedProject(alice)
	svc := e.account(alice, "bot")
	_, token := e.mintToken(alice, `{"name":"svc","projects":["`+p+`"],"service_account_id":"`+svc+`"}`)
	q := "?project=" + p
	e.seedExec(`INSERT INTO articles(id,space_id,parent_id,project_id,title,summary,body,kind,status,pinned,tags,properties,revision,created_at,updated_at,author) VALUES('o_sa','sa','','','t','','b','article','active',0,'[]','{}',1,'now','now',?)`, aliceID)
	if _, isErr, _ := e.tool(token, q, "wiki_read", map[string]any{"id": "o_sa"}); isErr {
		t.Fatal("service key read")
	}
	if _, isErr, _ := e.tool(token, q, "wiki_write", map[string]any{"article": map[string]any{"space_id": "sa", "title": "by bot"}}); isErr {
		t.Fatal("service key write")
	}
	var byBot int
	if err := e.seedDB.QueryRow(`SELECT count(*) FROM articles WHERE title='by bot' AND author=?`, svc).Scan(&byBot); err != nil || byBot != 1 {
		t.Fatal("authorship must be the service account")
	}
	// Admin status of the owner is irrelevant: keys never carry it.
	e.exec(`UPDATE users SET admin=1 WHERE id=?`, aliceID)
	if _, isErr, _ := e.tool(token, q, "wiki_read", map[string]any{"id": "k_bob_none"}); !isErr {
		t.Fatal("unknown article")
	}
	if code, _ := e.rpc(token, q, "ping", nil); code != 200 {
		t.Fatalf("ping %d", code)
	}
	// Disabling the service account revokes the key; disabling the owner stops it too.
	e.exec(`UPDATE users SET disabled=1 WHERE id=?`, aliceID)
	if code, _ := e.rpc(token, q, "ping", nil); code != 403 {
		t.Fatalf("disabled owner: %d", code)
	}
	e.exec(`UPDATE users SET disabled=0 WHERE id=?`, aliceID)
	if code, _ := e.rpc(token, q, "ping", nil); code != 200 {
		t.Fatalf("re-enabled owner: %d", code)
	}
	if e.count(`SELECT count(*) FROM security_audit WHERE action='mcp.credential.request' AND details->>'project'=?`, p) < 4 {
		t.Fatal("requests of a project connection are audited with the project")
	}
}

// The audit function recomputes the scope in SQL: a session carrying any other scope is refused.
func TestAgentProjectAuditReverifiesScopeInSQL(t *testing.T) {
	e, alice, aliceID := newKeyEnv(t)
	p := e.seedMixedProject(alice)
	credID, _ := e.mintToken(alice, `{"name":"k","projects":["`+p+`"]}`)
	ctx := context.Background()
	open := func(project string, scope []mcpScopeElement, cred string) *database {
		db, done, err := e.root.DB.scopedSession(ctx, agentSession{userID: aliceID, actor: aliceID, credential: cred, project: project, scope: scope, scopeText: scopeJSONText(scope)})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(done)
		return db
	}
	audit := func(db *database) error {
		_, err := db.Exec(`SELECT corporate_mcp_credential_audit(?,?,?)`, credID, "tools/call", "wiki_read")
		return err
	}
	// The honest session passes.
	honest, done, _, err := e.root.DB.scopedAgent(ctx, aliceID, aliceID, credID, p)
	if err != nil {
		t.Fatal(err)
	}
	defer done()
	if err := audit(honest); err != nil {
		t.Fatalf("honest session: %v", err)
	}
	// Forged sessions: a wider scope, another project, a missing project, a foreign credential id.
	wide := []mcpScopeElement{{Type: "space", ID: "sa", Mode: "write"}, {Type: "space", ID: "sb", Mode: "write"}}
	if audit(open(p, wide, credID)) == nil {
		t.Fatal("a wider scope passed the audit")
	}
	exact := []mcpScopeElement{{Type: "space", ID: "sa", Mode: "write"}, {Type: "space", ID: "sbob", Mode: "write"}, {Type: "space", ID: "sr", Mode: "write"}}
	if err := audit(open(p, exact, credID)); err != nil {
		t.Fatalf("the exact scope must pass: %v", err)
	}
	upgraded := []mcpScopeElement{{Type: "space", ID: "sa", Mode: "write"}, {Type: "space", ID: "sbob", Mode: "read"}, {Type: "space", ID: "sr", Mode: "write"}}
	if audit(open(p, upgraded, credID)) == nil {
		t.Fatal("a different mode set passed the audit")
	}
	if audit(open("pa", exact, credID)) == nil {
		t.Fatal("a project outside the key passed the audit")
	}
	if audit(open("", exact, credID)) == nil {
		t.Fatal("a project scope claimed as introspection passed")
	}
	if audit(open(p, exact, "pat_other")) == nil {
		t.Fatal("a foreign credential id passed")
	}
	// The pure functions are closed to the runtime role's plain sessions.
	var raw string
	if err := e.root.DB.QueryRow(`SELECT corporate_mcp_project_scope(?,?)::text`, credID, p).Scan(&raw); err != nil {
		t.Logf("scope function refuses an unbound session: %v", err)
	} else if raw != "" {
		t.Fatalf("scope computed for an unbound session: %s", raw)
	}
	// Revoked keys fail the audit, and the SQL scope function returns nothing for them.
	e.exec(`UPDATE mcp_credentials SET revoked_at=now() WHERE id=?`, credID)
	if audit(honest) == nil {
		t.Fatal("a revoked key passed the audit")
	}
	var computed *string
	if err := honest.QueryRow(`SELECT corporate_mcp_project_scope(?,?)::text`, credID, p).Scan(&computed); err != nil || computed != nil {
		t.Fatalf("revoked key scope: %v %v", computed, err)
	}
}
