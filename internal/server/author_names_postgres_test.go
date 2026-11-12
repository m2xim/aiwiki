package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
)

func TestAuthorNamesAndServiceAccountRenamePostgres(t *testing.T) {
	e := newPersistentEnv(t)
	alice, aliceID := e.user("alice")
	bob, bobID := e.user("bob")
	e.seed(aliceID, bobID)
	e.member("sa", bobID, "reader")
	svc := e.account(alice, "Agent Smith")

	e.seedExec(`INSERT INTO articles(id,space_id,parent_id,project_id,title,summary,body,kind,status,pinned,tags,properties,revision,created_at,updated_at,author) VALUES('an_art','sa','','','T','','b','article','active',0,'[]','{}',2,'now','now',?)`, svc)
	for rev, author := range map[int]string{1: aliceID, 2: svc} {
		snap, _ := json.Marshal(Article{ID: "an_art", SpaceID: "sa", Title: "T", Revision: rev, Author: author})
		e.seedExec(`INSERT INTO revisions(article_id,revision,author,message,created_at,snapshot) VALUES('an_art',?,?,'m','now',?)`, rev, author, string(snap))
	}
	e.seedExec(`INSERT INTO comments(id,article_id,anchor,body,author,created_at,resolved) VALUES('c_u','an_art','','x',?,'now',0),('c_s','an_art','','y',?,'now',0),('c_g','an_art','','z','ghost_u_unknown',?, 0)`, aliceID, svc, "now")

	check := func(who *Server, label string) {
		t.Helper()
		var art map[string]any
		rr := corporateHTTP(who, "GET", "/api/articles/an_art", "")
		if rr.Code != 200 || json.Unmarshal(rr.Body.Bytes(), &art) != nil || art["author"] != svc || art["author_name"] != "Agent Smith" || art["author_kind"] != "service" {
			t.Fatalf("%s article: %d %s", label, rr.Code, rr.Body)
		}
		var revs []map[string]any
		rr = corporateHTTP(who, "GET", "/api/articles/an_art/history", "")
		if rr.Code != 200 || json.Unmarshal(rr.Body.Bytes(), &revs) != nil || len(revs) != 2 {
			t.Fatalf("%s revisions: %d %s", label, rr.Code, rr.Body)
		}
		if revs[0]["author_name"] != "Agent Smith" || revs[0]["author_kind"] != "service" || revs[1]["author_name"] != "alice" || revs[1]["author_kind"] != "user" {
			t.Fatalf("%s revision names: %s", label, rr.Body)
		}
		if snap := revs[0]["snapshot"].(map[string]any); snap["author_name"] != "Agent Smith" {
			t.Fatalf("%s snapshot name: %v", label, snap)
		}
		var comments []map[string]any
		rr = corporateHTTP(who, "GET", "/api/articles/an_art/comments", "")
		if json.Unmarshal(rr.Body.Bytes(), &comments) != nil || len(comments) != 3 {
			t.Fatalf("%s comments: %d %s", label, rr.Code, rr.Body)
		}
		got := map[string]string{}
		for _, c := range comments {
			n, _ := c["author_name"].(string) // empty names are omitted
			got[c["id"].(string)] = n
		}
		if got["c_u"] != "alice" || got["c_s"] != "Agent Smith" || got["c_g"] != "" {
			t.Fatalf("%s comment names: %v", label, got)
		}
		var list []map[string]any
		rr = corporateHTTP(who, "GET", "/api/articles?space_id=sa", "")
		if json.Unmarshal(rr.Body.Bytes(), &list) != nil || len(list) == 0 || list[0]["author_name"] != "Agent Smith" {
			t.Fatalf("%s list: %s", label, rr.Body)
		}
		// MCP tools
		isErr, text := mcpCall(who, "wiki_history", map[string]any{"id": "an_art"})
		if isErr || !strings.Contains(text, `"author_name":"Agent Smith"`) || !strings.Contains(text, `"author_kind":"service"`) {
			t.Fatalf("%s wiki_history: %v %s", label, isErr, text)
		}
		isErr, text = mcpCall(who, "wiki_read", map[string]any{"id": "an_art"})
		if isErr || !strings.Contains(text, `"author_name":"Agent Smith"`) {
			t.Fatalf("%s wiki_read: %v %s", label, isErr, text)
		}
		isErr, text = mcpCall(who, "wiki_comments", map[string]any{"id": "an_art"})
		if isErr || !strings.Contains(text, `"author_name":"Agent Smith"`) {
			t.Fatalf("%s wiki_comments: %v %s", label, isErr, text)
		}
	}
	check(alice, "sponsor")
	check(bob, "other user")

	// Rename: reflects in old revisions for everyone.
	rec := e.call(alice, persistentAccountR, "PATCH", `{"name":"  Agent Brown "}`, svc)
	e.expect(rec, http.StatusOK)
	out := persistentJSON(t, rec)
	if out["id"] != svc || out["name"] != "Agent Brown" || out["sponsor_id"] != aliceID || out["disabled"] != false {
		t.Fatalf("rename response: %v", out)
	}
	if e.count(`SELECT count(*) FROM security_audit WHERE action='mcp_service_account.rename' AND resource_id=? AND details->>'old_name'='Agent Smith' AND details->>'name'='Agent Brown'`, svc) != 1 {
		t.Fatal("rename audit row missing")
	}
	var names []map[string]any
	rr := corporateHTTP(bob, "GET", "/api/articles/an_art/history", "")
	json.Unmarshal(rr.Body.Bytes(), &names)
	if names[0]["author_name"] != "Agent Brown" {
		t.Fatalf("old revision keeps the old name: %s", rr.Body)
	}

	// Refusals: validation, non-owner, MCP key, disabled account, unknown account.
	e.expect(e.call(alice, persistentAccountR, "PATCH", `{"name":"   "}`, svc), http.StatusBadRequest)
	e.expect(e.call(alice, persistentAccountR, "PATCH", `{"name":"x","owner_id":"bob"}`, svc), http.StatusBadRequest)
	e.expect(e.call(bob, persistentAccountR, "PATCH", `{"name":"Hijack"}`, svc), http.StatusNotFound)
	e.expect(e.call(alice, persistentAccountR, "PATCH", `{"name":"n"}`, "svc_missing"), http.StatusNotFound)
	scoped := *alice
	scoped.principal = &Principal{ID: aliceID, Issuer: e.iss, Subject: "alice", MCPScope: spaceScope("write", "sa"), MCPDelegationID: "pat_x"}
	e.expect(e.call(&scoped, persistentAccountR, "PATCH", `{"name":"Hijack"}`, svc), http.StatusForbidden)
	if got := e.text(`SELECT name FROM mcp_service_accounts WHERE id=?`, svc); got != "Agent Brown" {
		t.Fatalf("name after refusals: %s", got)
	}
	e.expect(e.call(alice, persistentAccountR, "DELETE", "", svc), http.StatusNoContent)
	e.expect(e.call(alice, persistentAccountR, "PATCH", `{"name":"Zombie"}`, svc), http.StatusConflict)
	// A disabled account still resolves to its (last) name in history.
	rr = corporateHTTP(bob, "GET", "/api/articles/an_art", "")
	if !strings.Contains(rr.Body.String(), `"author_name":"Agent Brown"`) {
		t.Fatalf("disabled account name: %s", rr.Body)
	}
	// The function leaks nothing but a name and is not executable by PUBLIC.
	var public int
	if err := e.owner.DB.QueryRow(`SELECT count(*) FROM pg_proc p, aclexplode(COALESCE(p.proacl,acldefault('f',p.proowner))) a WHERE p.oid='public.corporate_actor_name(text)'::regprocedure AND a.grantee=0`).Scan(&public); err != nil || public != 0 {
		t.Fatalf("PUBLIC EXECUTE on corporate_actor_name: %d %v", public, err)
	}
	if err := e.root.DB.validateRuntimeSchema(); err != nil {
		t.Fatalf("guards: %v", err)
	}
	_ = context.Background
}

// v7 applies on top of a database that already has mcp_access v6 and role_scope v2.
func TestActorNameMigrationOnTopOfV6Postgres(t *testing.T) {
	ownerURL := os.Getenv("AIWIKI_TEST_POSTGRES_OWNER")
	if ownerURL == "" || os.Getenv("AIWIKI_TEST_POSTGRES") == "" {
		t.Skip("set AIWIKI_TEST_POSTGRES_OWNER and AIWIKI_TEST_POSTGRES to a disposable database")
	}
	mcpScopeVersionCap = 6
	defer func() { mcpScopeVersionCap = 0 }()
	owner, err := openPostgres(context.Background(), ownerURL)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	if _, err = owner.Exec(`DROP SCHEMA public CASCADE; CREATE SCHEMA public`); err != nil {
		t.Fatal(err)
	}
	s := &Server{DB: owner, DataDir: t.TempDir()}
	if err = s.migrate(); err != nil {
		t.Fatal(err)
	}
	if err = s.migrateCorporate(); err != nil {
		t.Fatal(err)
	}
	var n int
	if err = owner.QueryRow(`SELECT count(*) FROM pg_proc WHERE proname='corporate_actor_name'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("v6 database already has the function: %d %v", n, err)
	}
	mcpScopeVersionCap = 0
	if err = s.migrateCorporate(); err != nil {
		t.Fatal(err)
	}
	if err = owner.QueryRow(`SELECT count(*) FROM schema_migrations WHERE component='mcp_access' AND version=7`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("v7 row: %d %v", n, err)
	}
	if err = owner.QueryRow(`SELECT count(*) FROM schema_migrations WHERE component='role_scope' AND version=2`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("role_scope v2 must stay applied: %d %v", n, err)
	}
}

func TestMCPCommentResolvePostgres(t *testing.T) {
	e := newPersistentEnv(t)
	alice, aliceID := e.user("alice")
	bob, bobID := e.user("bob")
	e.seed(aliceID, bobID)
	svc := e.account(alice, "Agent")
	e.seedExec(`INSERT INTO articles(id,space_id,parent_id,project_id,title,summary,body,kind,status,pinned,tags,properties,revision,created_at,updated_at,author) VALUES('cr_art','sa','','','T','','b','article','active',0,'[]','{}',1,'now','now',?)`, aliceID)
	e.seedExec(`INSERT INTO comments(id,article_id,anchor,body,author,created_at,resolved) VALUES('c_human','cr_art','','h',?,'now',0),('c_agent','cr_art','','a',?,'now',0),('c_agent2','cr_art','','a2',?,'now',0)`, aliceID, svc, svc)

	asKey := func(mode, actor string) *Server {
		sc := spaceScope(mode, "sa")
		db, done, err := e.root.DB.scopedMCP(context.Background(), aliceID, sc, actor)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(done)
		cp := *e.root
		cp.DB = db
		cp.auth = e.x.a
		cp.principal = &Principal{ID: aliceID, Issuer: e.iss, Subject: "alice", MCPScope: sc, MCPDelegationID: "pat_x", MCPActorID: actor}
		return &cp
	}
	resolved := func(id string) string {
		var v int
		if err := e.seedDB.QueryRow(`SELECT resolved FROM comments WHERE id=?`, id).Scan(&v); err != nil {
			t.Fatal(err)
		}
		return fmt.Sprint(v)
	}
	// Service-account write key (editor owner): resolves its own and others'.
	agent := asKey("write", svc)
	isErr, text := mcpCall(agent, "wiki_comment_resolve", map[string]any{"id": "c_agent"})
	if isErr || !strings.Contains(text, `"resolved":true`) || !strings.Contains(text, `"author_name":"Agent"`) || !strings.Contains(text, `"author_kind":"service"`) {
		t.Fatalf("resolve own: %v %s", isErr, text)
	}
	if isErr, text = mcpCall(agent, "wiki_comment_resolve", map[string]any{"id": "c_agent", "resolved": false}); isErr || resolved("c_agent") != "0" {
		t.Fatalf("reopen: %v %s %s", isErr, text, resolved("c_agent"))
	}
	if isErr, text = mcpCall(agent, "wiki_comment_resolve", map[string]any{"id": "c_human"}); isErr || resolved("c_human") != "1" {
		t.Fatalf("editor-level key resolves others': %v %s", isErr, text)
	}
	// Commenter-level ceiling (owner is only a commenter on pr... use a reader/commenter project): bob commenter on pa.
	e.seedExec(`UPDATE memberships SET role='commenter' WHERE resource_id='sa' AND subject_id=?`, aliceID)
	e.seedExec(`UPDATE comments SET resolved=0`)
	own := mcpScopedCall(t, asKey("write", svc), "c_agent2")
	if !own {
		t.Fatal("commenter-level key must resolve the comments its own actor created")
	}
	if mcpScopedCall(t, asKey("write", svc), "c_human") {
		t.Fatal("commenter-level key resolved someone else's comment")
	}
	// A key of another actor (no service account) cannot claim the svc_ comment.
	if mcpScopedCall(t, asKey("write", ""), "c_agent") {
		t.Fatal("a key without the service actor resolved the service comment")
	}
	e.seedExec(`UPDATE memberships SET role='editor' WHERE resource_id='sa' AND subject_id=?`, aliceID)
	// Read-mode key: tool hidden and RLS refuses.
	ro := asKey("read", "")
	if mcpScopedTool(ro.principal, "wiki_comment_resolve") {
		t.Fatal("read key must not see the resolve tool")
	}
	if isErr, _ = mcpCall(ro, "wiki_comment_resolve", map[string]any{"id": "c_human"}); !isErr {
		t.Fatal("read-mode key resolved a comment")
	}
	if _, err := ro.DB.Exec(`UPDATE comments SET resolved=1 WHERE id='c_agent'`); err != nil {
		// refused by policy as an error or 0 rows
	}
	if resolved("c_agent") != "0" {
		t.Fatal("read-mode key changed a comment via SQL")
	}
	// Direct SQL under a write key: only resolved may change.
	w := asKey("write", svc)
	for _, q := range []string{
		`UPDATE comments SET body='tampered' WHERE id='c_agent'`,
		`UPDATE comments SET anchor='x' WHERE id='c_agent'`,
		`UPDATE comments SET author='` + aliceID + `' WHERE id='c_agent'`,
		`UPDATE comments SET article_id='cr_art', created_at='2000' WHERE id='c_agent'`,
	} {
		if _, err := w.DB.Exec(q); err == nil {
			t.Fatalf("MCP session changed more than resolved: %s", q)
		}
	}
	if _, err := w.DB.Exec(`DELETE FROM comments WHERE id='c_agent'`); err != nil {
		t.Logf("delete refused: %v", err)
	}
	var kept int
	if err := e.seedDB.QueryRow(`SELECT count(*) FROM comments WHERE id='c_agent' AND body='a'`).Scan(&kept); err != nil || kept != 1 {
		t.Fatal("comment changed or deleted under MCP")
	}
	// Browser behaviour unchanged: owner edits body; bob (no rights) is refused.
	rr := corporateHTTP(alice, "PATCH", "/api/comments/c_human", `{"body":"edited","resolved":true}`)
	if rr.Code != 200 {
		t.Fatalf("browser edit: %d %s", rr.Code, rr.Body)
	}
	rr = corporateHTTP(bob, "PATCH", "/api/comments/c_human", `{"resolved":false}`)
	if rr.Code != 403 && rr.Code != 404 {
		t.Fatalf("browser non-member: %d %s", rr.Code, rr.Body)
	}
	// The actor setting is reset with the session.
	var a string
	conn := asKey("write", svc)
	if err := conn.DB.QueryRow(`SELECT current_setting('app.actor_id',true)`).Scan(&a); err != nil || a != svc {
		t.Fatalf("actor setting %q %v", a, err)
	}
}

func mcpScopedCall(t *testing.T, s *Server, id string) bool {
	t.Helper()
	isErr, _ := mcpCall(s, "wiki_comment_resolve", map[string]any{"id": id})
	return !isErr
}
