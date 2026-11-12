package server

import (
	"context"
	"testing"
)

// A scoped session sees content of its spaces only, no metadata table at all, and cannot write outside
// the content operations; cleanup clears every session value.
func TestCorporateMCPSpaceScopePostgres(t *testing.T) {
	owner, root := corporateFixture(t)
	ctx := context.Background()
	const userID = "alice"
	seedDB, seedDone, err := owner.DB.scoped(ctx, userID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(seedDone)
	owner.DB = seedDB
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := owner.DB.Exec(q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	exec(`INSERT INTO teams(id,name) VALUES('mcp-metadata-team','Hidden team'); INSERT INTO team_members(team_id,user_id) VALUES('mcp-metadata-team','alice'); INSERT INTO security_audit(actor_id,action,resource_type,resource_id,details,created_at) VALUES('alice','fixture','team','mcp-metadata-team','{}','now')`)
	for _, sp := range []string{"scope_a", "scope_b"} {
		exec(`INSERT INTO spaces(id,name,description,owner_id) VALUES(?,?, '', ?)`, sp, sp, userID)
		exec(`INSERT INTO memberships(resource_type,resource_id,subject_type,subject_id,role) VALUES('space',?,'user',?,'owner')`, sp, userID)
		art := "art_" + sp
		exec(`INSERT INTO articles(id,space_id,parent_id,project_id,title,summary,body,kind,status,pinned,tags,properties,revision,created_at,updated_at,author,deleted_at) VALUES(?,?, '', '', ?, '', '', 'article','active',0,'[]','{}',1,'now','now',?,NULL)`, art, sp, art, userID)
		exec(`INSERT INTO revisions(article_id,revision,author,message,created_at,snapshot) VALUES(?,1,?,'','now',?)`, art, userID, `{"space_id":"`+sp+`"}`)
		exec(`INSERT INTO comments(id,article_id,anchor,body,author,created_at,resolved) VALUES(?,?, '', '', ?,'now',0)`, "c_"+sp, art, userID)
		exec(`INSERT INTO attachments(id,article_id,name,mime,size,stored_name,created_at) VALUES(?,?,'file','text/plain',1,?,'now')`, "f_"+sp, art, "f_"+sp)
		exec(`INSERT INTO article_fts(article_id,title) VALUES(?,?)`, art, art)
		exec(`INSERT INTO favorites(user_id,article_id,created_at) VALUES(?,?,'now')`, userID, art)
	}
	scoped, done, err := root.DB.scopedMCP(ctx, userID, spaceScope("read", "scope_a"))
	if err != nil {
		t.Fatal(err)
	}
	defer done()
	var setting string
	if err = scoped.QueryRow(`SELECT current_setting('app.mcp_scope',true)`).Scan(&setting); err != nil || setting != `[{"type":"space","id":"scope_a","mode":"read"}]` {
		t.Fatalf("scope setting %q %v", setting, err)
	}
	for _, c := range []struct{ table, q string }{
		{"spaces", `SELECT count(*) FROM spaces WHERE id IN ('scope_a','scope_b')`},
		{"articles", `SELECT count(*) FROM articles WHERE id IN ('art_scope_a','art_scope_b')`},
		{"revisions", `SELECT count(*) FROM revisions WHERE article_id IN ('art_scope_a','art_scope_b')`},
		{"comments", `SELECT count(*) FROM comments WHERE id IN ('c_scope_a','c_scope_b')`},
		{"attachments", `SELECT count(*) FROM attachments WHERE article_id IN ('art_scope_a','art_scope_b')`},
		{"favorites", `SELECT count(*) FROM favorites WHERE article_id IN ('art_scope_a','art_scope_b')`},
		{"article_fts", `SELECT count(*) FROM article_fts WHERE article_id IN ('art_scope_a','art_scope_b')`},
	} {
		var n int
		if err = scoped.QueryRow(c.q).Scan(&n); err != nil || n != 1 {
			t.Errorf("%s visible rows=%d err=%v, want 1 (only the listed space)", c.table, n, err)
		}
	}
	for _, table := range []string{"users", "memberships", "teams", "team_members", "corporate_space_state", "security_audit", "projects"} {
		var visible int
		if err := scoped.QueryRow("SELECT count(*) FROM " + table).Scan(&visible); err != nil || visible != 0 {
			t.Errorf("metadata table %s leaked: count=%d err=%v", table, visible, err)
		}
	}
	// agent project tables: a session of a key without a project sees none; the SQL view functions stay closed.
	var n int
	for _, q := range []string{`SELECT count(*) FROM agent_projects`, `SELECT count(*) FROM agent_project_spaces`, `SELECT count(*) FROM corporate_agent_projects_visible(NULL)`, `SELECT count(*) FROM corporate_agent_project_rows(NULL)`} {
		if err := scoped.QueryRow(q).Scan(&n); err != nil || n != 0 {
			t.Errorf("%s under a scope without a project: %d %v", q, n, err)
		}
	}
	if _, err := scoped.Exec(`INSERT INTO agent_projects(id,name,owner_id,visibility) VALUES('sneak','n',?,'private')`, userID); err == nil {
		t.Error("project created from a scoped session")
	}
	if _, err := scoped.Exec(`INSERT INTO security_audit(actor_id,action,resource_type,resource_id,details,created_at) VALUES(?,'spoof','mcp_delegation','x','{}','now')`, userID); err == nil {
		t.Error("direct scoped audit insertion allowed")
	}
	var role string
	if err = scoped.QueryRow(`SELECT corporate_effective_role(?,?, '')`, userID, "scope_a").Scan(&role); err != nil || role != "reader" {
		t.Errorf("scoped owner role=%q err=%v, want reader", role, err)
	}
	if err = scoped.QueryRow(`SELECT coalesce(corporate_effective_role(?,?, ''), '')`, userID, "scope_b").Scan(&role); err != nil || role != "" {
		t.Errorf("unlisted space role=%q err=%v, want none", role, err)
	}
	if _, err = scoped.Exec(`INSERT INTO spaces(id,name,description,owner_id) VALUES('mcp-denied-space','denied','',?)`, userID); err == nil {
		t.Error("scoped space insert was allowed")
	}
	exec2 := func(q string, args ...any) {
		if _, err := scoped.Exec(q, args...); err != nil {
			t.Errorf("%s: %v", q, err)
		}
	}
	exec2(`UPDATE revisions SET message='changed' WHERE article_id='art_scope_a'`)
	exec2(`UPDATE comments SET body='changed' WHERE id='c_scope_a'`)
	exec2(`DELETE FROM favorites WHERE article_id='art_scope_a'`)
	if _, err = scoped.Exec(`INSERT INTO favorites(user_id,article_id,created_at) VALUES(?,'art_scope_a','now')`, userID); err == nil {
		t.Error("scoped favorite insert was allowed")
	}
	var changed, favorites int
	if err = owner.DB.QueryRow(`SELECT count(*) FROM revisions WHERE message='changed'`).Scan(&changed); err != nil || changed != 0 {
		t.Errorf("revision changed under MCP scope: %d %v", changed, err)
	}
	if err = owner.DB.QueryRow(`SELECT count(*) FROM comments WHERE body='changed'`).Scan(&changed); err != nil || changed != 0 {
		t.Errorf("comment changed under MCP scope: %d %v", changed, err)
	}
	if err = owner.DB.QueryRow(`SELECT count(*) FROM favorites WHERE user_id=? AND article_id='art_scope_a'`, userID).Scan(&favorites); err != nil || favorites != 1 {
		t.Errorf("favorite changed under MCP scope: %d %v", favorites, err)
	}

	// Cleanup rolls back a forgotten transaction and clears every session value.
	tx, err := scoped.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(`SELECT corporate_mcp_request_audit(?, 'test', 'rollback_probe')`, "dlg_rollback_x"); err != nil {
		t.Fatal(err)
	}
	done()
	resumed, release, err := scoped.resumeAfterTransfer(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var resumedScope string
	if err = resumed.QueryRow(`SELECT current_setting('app.mcp_scope',true)`).Scan(&resumedScope); err != nil || resumedScope != setting {
		t.Errorf("resumed scope=%q err=%v", resumedScope, err)
	}
	release()
	root.DB.pool.SetMaxOpenConns(1)
	conn, err := root.DB.pool.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var a, b, c, d string
	if err = conn.QueryRowContext(ctx, `SELECT coalesce(current_setting('app.user_id',true),''), coalesce(current_setting('app.mcp_scope',true),''), coalesce(current_setting('app.mcp_project',true),''), coalesce(current_setting('app.mcp_credential',true),'')`).Scan(&a, &b, &c, &d); err != nil || a != "" || b != "" || c != "" || d != "" {
		t.Errorf("cleanup left session values %q %q %q %q err=%v", a, b, c, d, err)
	}
	_ = conn.Close()
	check, clear, err := root.DB.scopedMCP(ctx, userID, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer clear()
	if err = check.QueryRow(`SELECT current_setting('app.mcp_scope',true)`).Scan(&setting); err != nil || setting != "" {
		t.Errorf("reborrowed connection retained MCP scope %q %v", setting, err)
	}
	clear()
	if err = owner.DB.QueryRow(`SELECT count(*) FROM security_audit WHERE resource_id='dlg_rollback_x'`).Scan(&changed); err != nil || changed != 0 {
		t.Errorf("forgotten transaction was not rolled back: %d %v", changed, err)
	}
	// An empty scope is refused (it never degrades to unrestricted).
	if _, _, err := root.DB.scopedMCP(ctx, userID, []mcpScopeElement{}); err == nil {
		t.Error("empty scope accepted")
	}
	// A stale process that still sets the pre-scope setting gets a scoped session with no valid scope.
	legacy, err := root.DB.pool.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer legacy.Close()
	if _, err = legacy.ExecContext(ctx, `SELECT set_config('app.user_id',$1,false), set_config('app.mcp_projects','["x"]',false)`, userID); err != nil {
		t.Fatal(err)
	}
	if err = legacy.QueryRowContext(ctx, `SELECT count(*) FROM articles WHERE id='art_scope_a'`).Scan(&n); err != nil || n != 0 {
		t.Errorf("legacy setting count=%d err=%v, want deny", n, err)
	}
	legacy.ExecContext(ctx, `SELECT set_config('app.user_id','',false), set_config('app.mcp_projects','',false)`)
}
