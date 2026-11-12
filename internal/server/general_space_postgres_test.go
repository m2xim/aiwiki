package server

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

// gpEnv: a general space "gp_general" (root article + project article) and another
// space "gp_other" with the same layout. Users: alice (no memberships), bob
// (editor in general), admin. Setting general_space_id points at gp_general.
type gpEnv struct {
	t           *testing.T
	owner, root *Server
}

func newGPEnv(t *testing.T) *gpEnv {
	owner, root := corporateFixture(t)
	e := &gpEnv{t: t, owner: owner, root: root}
	tx, err := owner.DB.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	x := func(q string, a ...any) {
		t.Helper()
		if _, err := tx.Exec(q, a...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	for _, tbl := range []string{"spaces", "articles", "revisions", "article_fts", "app_settings"} {
		x(`ALTER TABLE ` + tbl + ` NO FORCE ROW LEVEL SECURITY`)
	}
	for _, sp := range []string{"gp_general", "gp_other"} {
		x(`INSERT INTO spaces(id,name,description,owner_id,personal) VALUES(?,?, '', '',0)`, sp, sp)
		pr := sp + "_proj"
		x(`INSERT INTO projects(id,space_id,parent_id,name,description,inherit_acl) VALUES(?,?, '', ?, '',1)`, pr, sp, pr)
		for _, a := range [][2]string{{sp + "_root_art", ""}, {sp + "_proj_art", pr}} {
			x(`INSERT INTO articles(id,space_id,parent_id,project_id,title,summary,body,kind,status,pinned,tags,properties,revision,created_at,updated_at,author,deleted_at) VALUES(?,?, '', ?, ?, '', 'body', 'article','active',1,'[]','{}',1,'now','now','admin',NULL)`, a[0], sp, a[1], a[0])
			x(`INSERT INTO article_fts(article_id,title) VALUES(?,?)`, a[0], a[0])
		}
	}
	x(`INSERT INTO memberships(resource_type,resource_id,subject_type,subject_id,role) VALUES('space','gp_general','user','bob','editor')`)
	x(`INSERT INTO app_settings(key,value) VALUES('general_space_id','gp_general')`)
	for _, tbl := range []string{"spaces", "articles", "revisions", "article_fts", "app_settings"} {
		x(`ALTER TABLE ` + tbl + ` FORCE ROW LEVEL SECURITY`)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return e
}

// ownerExecNoRLS runs statements as the table owner with FORCE RLS lifted on the content tables.
func ownerExecNoRLS(t *testing.T, owner *Server, stmts ...string) {
	t.Helper()
	tables := []string{"spaces", "articles"}
	for _, tbl := range tables {
		if _, err := owner.DB.Exec(`ALTER TABLE ` + tbl + ` NO FORCE ROW LEVEL SECURITY`); err != nil {
			t.Fatal(err)
		}
	}
	defer func() {
		for _, tbl := range tables {
			if _, err := owner.DB.Exec(`ALTER TABLE ` + tbl + ` FORCE ROW LEVEL SECURITY`); err != nil {
				t.Fatal(err)
			}
		}
	}()
	for _, q := range stmts {
		if _, err := owner.DB.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
}

func (e *gpEnv) setGeneral(v string) error {
	_, err := e.owner.DB.Exec(`ALTER TABLE app_settings NO FORCE ROW LEVEL SECURITY; UPDATE app_settings SET value='` + v + `' WHERE key='general_space_id'; ALTER TABLE app_settings FORCE ROW LEVEL SECURITY`)
	return err
}

func (e *gpEnv) asScope(user string, scope ...mcpScopeElement) *Server {
	e.t.Helper()
	db, cleanup, err := e.root.DB.scopedMCP(context.Background(), user, scope)
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(cleanup)
	cp := *e.root
	cp.DB = db
	cp.principal = &Principal{ID: user, Name: user, MCPScope: scope, MCPDelegationID: "pat_test"}
	return &cp
}

func (e *gpEnv) as(user, mode string, projects ...string) *Server {
	e.t.Helper()
	var db *database
	var cleanup func()
	var err error
	if mode == "" {
		db, cleanup, err = e.root.DB.scoped(context.Background(), user)
	} else {
		db, cleanup, err = e.root.DB.scopedMCP(context.Background(), user, projScope(mode, projects...))
	}
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(cleanup)
	cp := *e.root
	cp.DB = db
	cp.principal = &Principal{ID: user, Name: user, Admin: user == "admin"}
	if mode != "" {
		cp.principal.MCPScope, cp.principal.MCPDelegationID = projScope(mode, projects...), "pat_test"
	}
	return &cp
}

func (s *Server) gpReadable(id string) bool {
	var n int
	return s.DB.QueryRow(`SELECT count(*) FROM articles WHERE id=?`, id).Scan(&n) == nil && n == 1
}

func (s *Server) gpCanWrite(space, project string) bool {
	_, err := s.DB.Exec(`INSERT INTO articles(id,space_id,parent_id,project_id,title,summary,body,kind,status,pinned,tags,properties,revision,created_at,updated_at,author,deleted_at) VALUES(?,?, '', ?, 't', '', '', 'article','active',0,'[]','{}',1,'now','now','x',NULL)`, "gpw_"+randomSuffix(), space, project)
	return err == nil
}

func TestGeneralSpaceImplicitReadPostgres(t *testing.T) {
	e := newGPEnv(t)
	alice := e.as("alice", "")
	for _, id := range []string{"gp_general_root_art", "gp_general_proj_art"} {
		if !alice.gpReadable(id) {
			t.Errorf("alice cannot read %s", id)
		}
	}
	for _, id := range []string{"gp_other_root_art", "gp_other_proj_art"} {
		if alice.gpReadable(id) {
			t.Errorf("alice reads non-general %s", id)
		}
	}
	var n int
	if err := alice.DB.QueryRow(`SELECT count(*) FROM spaces WHERE id='gp_general'`).Scan(&n); err != nil || n != 1 {
		t.Errorf("general space row n=%d err=%v", n, err)
	}
	if alice.gpCanWrite("gp_general", "") || alice.gpCanWrite("gp_general", "gp_general_proj") {
		t.Error("implicit reader can write general")
	}
	bob := e.as("bob", "")
	if !bob.gpCanWrite("gp_general", "gp_general_proj") || bob.gpCanWrite("gp_other", "gp_other_proj") {
		t.Error("explicit editor writes wrong")
	}
	// disabled users get nothing
	if _, err := e.owner.DB.Exec(`UPDATE users SET disabled=1 WHERE id='alice'`); err != nil {
		t.Fatal(err)
	}
	if e.as("alice", "").gpReadable("gp_general_root_art") {
		t.Error("disabled user reads general")
	}
	if _, err := e.owner.DB.Exec(`UPDATE users SET disabled=0 WHERE id='alice'`); err != nil {
		t.Fatal(err)
	}
	// moving the layer
	if err := e.setGeneral("gp_other"); err != nil {
		t.Fatal(err)
	}
	alice = e.as("alice", "")
	if alice.gpReadable("gp_general_root_art") || !alice.gpReadable("gp_other_proj_art") {
		t.Error("setting change did not move the layer")
	}
	// empty value falls back to 'general'
	if err := e.setGeneral(""); err != nil {
		t.Fatal(err)
	}
	var g string
	if err := e.root.DB.QueryRow(`SELECT corporate_general_space()`).Scan(&g); err != nil || g != "general" {
		t.Errorf("default general=%q err=%v", g, err)
	}
}

func TestGeneralSpaceMCPPostgres(t *testing.T) {
	e := newGPEnv(t)
	listed := "gp_other_proj"
	// read key not listing general: reads general, not write, still sees listed, not others
	ro := e.as("bob", "read", listed)
	for id, want := range map[string]bool{"gp_general_root_art": true, "gp_general_proj_art": true, "gp_other_proj_art": false, "gp_other_root_art": false} {
		if got := ro.gpReadable(id); got != want {
			t.Errorf("read key readable(%s)=%v want %v", id, got, want)
		}
	}
	if ro.gpCanWrite("gp_general", "gp_general_proj") {
		t.Error("read key writes general")
	}
	// bob has no grant in gp_other => key lists it but sees nothing there
	roAlice := e.as("alice", "read", listed)
	if !roAlice.gpReadable("gp_general_proj_art") || roAlice.gpReadable("gp_other_proj_art") {
		t.Error("alice read key scope wrong")
	}
	// a write key that does not list general as a write element still only reads it
	rw := e.as("bob", "write", listed)
	if rw.gpCanWrite("gp_general", "gp_general_proj") || !rw.gpReadable("gp_general_proj_art") {
		t.Error("implicit general must be read-only even for a write key")
	}
	// explicit space element with write + owner editor in general
	rw = e.asScope("bob", mcpScopeElement{"project", listed, "write"}, mcpScopeElement{"space", "gp_general", "write"})
	if !rw.gpCanWrite("gp_general", "gp_general_proj") || !rw.gpCanWrite("gp_general", "") {
		t.Error("explicit general write with editor owner cannot write general")
	}
	// ... but a key never exceeds its owner (alice is only an implicit reader)
	if e.asScope("alice", mcpScopeElement{"space", "gp_general", "write"}).gpCanWrite("gp_general", "gp_general_proj") {
		t.Error("write key with implicit-reader owner writes general")
	}
	// reference function role values
	var role string
	if err := rw.DB.QueryRow(`SELECT coalesce(corporate_effective_role('bob','gp_general','gp_general_proj'),'')`).Scan(&role); err != nil || role != "editor" {
		t.Errorf("role=%q err=%v", role, err)
	}
	if err := ro.DB.QueryRow(`SELECT coalesce(corporate_effective_role('bob','gp_other','gp_other_root'),'')`).Scan(&role); err != nil || role != "" {
		t.Errorf("unlisted role=%q err=%v", role, err)
	}
	// no admin/space administration via MCP
	if _, err := rw.DB.Exec(`INSERT INTO memberships(resource_type,resource_id,subject_type,subject_id,role) VALUES('space','gp_general','user','alice','editor')`); err == nil {
		t.Error("membership insert allowed under MCP")
	}
}

func TestGeneralSpaceAdminSettingsPostgres(t *testing.T) {
	e := newGPEnv(t)
	admin := e.as("admin", "")
	put := func(body string) (int, string) {
		rr := httptest.NewRecorder()
		db, done, err := admin.DB.resumeAfterTransfer(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		defer done()
		cp := *admin
		cp.DB = db
		cp.routes().ServeHTTP(rr, httptest.NewRequest("PUT", "/api/admin/settings", strings.NewReader(body)))
		return rr.Code, rr.Body.String()
	}
	if code, _ := put(`{}`); code != 400 {
		t.Errorf("empty put=%d", code)
	}
	if code, body := put(`{"general_space_id":"nope"}`); code != 400 || !strings.Contains(body, "unknown space") {
		t.Errorf("unknown space put=%d %s", code, body)
	}
	if code, body := put(`{"general_space_id":"gp_other"}`); code != 200 || !strings.Contains(body, `"general_space_id":"gp_other"`) || !strings.Contains(body, "public_artifact_links") {
		t.Errorf("put=%d %s", code, body)
	}
	if code, body := put(`{"public_artifact_links":false}`); code != 200 || !strings.Contains(body, `"general_space_id":"gp_other"`) || !strings.Contains(body, `"public_artifact_links":false`) {
		t.Errorf("links-only put=%d %s", code, body)
	}
	if code, body := put(`{"public_artifact_links":true,"general_space_id":"gp_general"}`); code != 200 {
		t.Errorf("both put=%d %s", code, body)
	}
	rr := corporateHTTP(admin, "GET", "/api/admin/settings", "")
	var got map[string]any
	json.Unmarshal(rr.Body.Bytes(), &got)
	if got["general_space_id"] != "gp_general" || got["public_artifact_links"] != true {
		t.Errorf("get=%s", rr.Body.String())
	}
	var audits int
	if err := e.owner.DB.QueryRow(`SELECT count(*) FROM security_audit WHERE action='settings.update' AND resource_id='general_space_id'`).Scan(&audits); err != nil || audits != 2 {
		t.Errorf("audits=%d err=%v", audits, err)
	}
	if rr := corporateHTTP(e.as("alice", ""), "PUT", "/api/admin/settings", `{"general_space_id":"gp_other"}`); rr.Code != 403 {
		t.Errorf("non-admin put=%d", rr.Code)
	}
}

func TestGeneralSpaceBootstrapPostgres(t *testing.T) {
	e := newGPEnv(t)
	// a second pinned article in the listed project's space, titled to sort before general's
	call := func(s *Server, args map[string]any) map[string]any {
		isErr, text := mcpCall(s, "wiki_bootstrap", args)
		if isErr {
			t.Fatalf("bootstrap: %s", text)
		}
		var out map[string]any
		if err := json.Unmarshal([]byte(text), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	ids := func(v any) []string {
		var r []string
		for _, a := range v.([]any) {
			r = append(r, a.(map[string]any)["id"].(string))
		}
		return r
	}
	scoped := e.as("bob", "read", "gp_other_proj")
	out := call(scoped, map[string]any{})
	if out["general_space_id"] != "gp_general" {
		t.Errorf("general_space_id=%v", out["general_space_id"])
	}
	got := strings.Join(ids(out["instructions"]), ",")
	// bob sees nothing in gp_other (no grant) so only general articles are there, general first
	if !strings.HasPrefix(got, "gp_general_") || strings.Contains(got, "gp_other") {
		t.Errorf("scoped instructions=%s", got)
	}
	// owner with a grant in gp_other: both layers, general first
	if _, err := e.owner.DB.Exec(`INSERT INTO memberships(resource_type,resource_id,subject_type,subject_id,role) VALUES('space','gp_other','user','bob','reader')`); err != nil {
		t.Fatal(err)
	}
	out = call(e.as("bob", "read", "gp_other_proj"), map[string]any{})
	list := ids(out["instructions"])
	if len(list) != 3 || !strings.HasPrefix(list[0], "gp_general_") || !strings.HasPrefix(list[1], "gp_general_") || list[2] != "gp_other_proj_art" {
		t.Errorf("layered instructions=%v", list)
	}
	// other_pinned must not repeat full ones
	for _, id := range ids(out["other_pinned"]) {
		for _, f := range list {
			if id == f {
				t.Errorf("%s in both", id)
			}
		}
	}
	// plain user without space_id: only general in full, other pinned compact
	out = call(e.as("bob", ""), map[string]any{})
	if l := ids(out["instructions"]); len(l) != 2 {
		t.Errorf("plain instructions=%v", l)
	}
	if l := ids(out["other_pinned"]); len(l) != 2 {
		t.Errorf("plain other_pinned=%v", l)
	}
	// with space_id
	out = call(e.as("bob", ""), map[string]any{"space_id": "gp_other"})
	if l := ids(out["instructions"]); len(l) != 4 || !strings.HasPrefix(l[0], "gp_general_") {
		t.Errorf("space_id instructions=%v", l)
	}
	// REST
	rr := corporateHTTP(e.as("bob", ""), "GET", "/api/bootstrap", "")
	var rest map[string]any
	json.Unmarshal(rr.Body.Bytes(), &rest)
	if l := ids(rest["instructions"]); rest["general_space_id"] != "gp_general" || len(l) != 4 || !strings.HasPrefix(l[0], "gp_general_") || !strings.HasPrefix(l[1], "gp_general_") {
		t.Errorf("rest bootstrap=%d %s", rr.Code, rr.Body.String())
	}
}
