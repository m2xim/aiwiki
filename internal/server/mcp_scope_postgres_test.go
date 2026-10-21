package server

import (
	"context"
	"testing"
)

func TestCorporateMCPProjectScopePostgres(t *testing.T) {
	owner, root := corporateFixture(t)
	ctx := context.Background()
	const userID = "alice"
	seedDB, seedDone, err := owner.DB.scoped(ctx, userID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(seedDone)
	owner.DB = seedDB
	if _, err := owner.DB.Exec(`INSERT INTO teams(id,name) VALUES('mcp-metadata-team','Hidden team'); INSERT INTO team_members(team_id,user_id) VALUES('mcp-metadata-team','alice'); INSERT INTO security_audit(actor_id,action,resource_type,resource_id,details,created_at) VALUES('alice','fixture','team','mcp-metadata-team','{}','now')`); err != nil {
		t.Fatal(err)
	}
	suffix := randomSuffix()
	spaceID := "mcp-space-" + suffix
	projectA, projectB := "mcp-project-a-"+suffix, "mcp-project-b-"+suffix
	articleA, articleB := "mcp-article-a-"+suffix, "mcp-article-b-"+suffix
	articleWriteA := "mcp-article-write-a-" + suffix
	commentA, commentB := "mcp-comment-a-"+suffix, "mcp-comment-b-"+suffix
	cleanupData := func() {
		_, _ = owner.DB.Exec(`DELETE FROM memberships WHERE (resource_type='space' AND resource_id=?) OR (resource_type='project' AND resource_id IN (?,?))`, spaceID, projectA, projectB)
		_, _ = owner.DB.Exec(`DELETE FROM spaces WHERE id=?`, spaceID)
		_, _ = owner.DB.Exec(`DELETE FROM corporate_space_state WHERE space_id=?`, spaceID)
	}
	defer cleanupData()
	if _, err := owner.DB.Exec(`INSERT INTO spaces(id,name,description,owner_id,personal) VALUES(?,?, '', ?,0)`, spaceID, "MCP scope fixture", userID); err != nil {
		t.Fatal(err)
	}
	if _, err := owner.DB.Exec(`INSERT INTO memberships(resource_type,resource_id,subject_type,subject_id,role) VALUES('space',?,'user',?,'owner')`, spaceID, userID); err != nil {
		t.Fatal(err)
	}
	for _, project := range []string{projectA, projectB} {
		if _, err := owner.DB.Exec(`INSERT INTO projects(id,space_id,parent_id,name,description,inherit_acl) VALUES(?,?, '', ?, '',0)`, project, spaceID, project); err != nil {
			t.Fatal(err)
		}
		if _, err := owner.DB.Exec(`INSERT INTO memberships(resource_type,resource_id,subject_type,subject_id,role) VALUES('project',?,'user',?,'owner')`, project, userID); err != nil {
			t.Fatal(err)
		}
	}
	for _, item := range []struct{ id, project string }{{articleA, projectA}, {articleB, projectB}} {
		if _, err := owner.DB.Exec(`INSERT INTO articles(id,space_id,parent_id,project_id,title,summary,body,kind,status,pinned,tags,properties,revision,created_at,updated_at,author,deleted_at) VALUES(?,?, '', ?, ?, '', '', 'article','active',0,'[]','{}',1,'now','now',?,NULL)`, item.id, spaceID, item.project, item.id, userID); err != nil {
			t.Fatal(err)
		}
		if _, err := owner.DB.Exec(`INSERT INTO revisions(article_id,revision,author,message,created_at,snapshot) VALUES(?,1,?,'','now',?)`, item.id, userID, `{"space_id":"`+spaceID+`","project_id":"`+item.project+`"}`); err != nil {
			t.Fatal(err)
		}
		commentID := commentA
		if item.project == projectB {
			commentID = commentB
		}
		if _, err := owner.DB.Exec(`INSERT INTO comments(id,article_id,anchor,body,author,created_at,resolved) VALUES(?,?, '', '', ?,'now',0)`, commentID, item.id, userID); err != nil {
			t.Fatal(err)
		}
		if _, err := owner.DB.Exec(`INSERT INTO attachments(id,article_id,name,mime,size,stored_name,created_at) VALUES(?,?,'file','text/plain',1,?,'now')`, "mcp-file-"+item.id, item.id, "mcp-file-"+item.id); err != nil {
			t.Fatal(err)
		}
		if _, err := owner.DB.Exec(`INSERT INTO article_fts(article_id,title) VALUES(?,?)`, item.id, item.id); err != nil {
			t.Fatal(err)
		}
		if _, err := owner.DB.Exec(`INSERT INTO favorites(user_id,article_id,created_at) VALUES(?,?,'now')`, userID, item.id); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := owner.DB.Exec(`INSERT INTO articles(id,space_id,parent_id,project_id,title,summary,body,kind,status,pinned,tags,properties,revision,created_at,updated_at,author,deleted_at) VALUES(?,?, '', ?, ?, '', '', 'article','active',0,'[]','{}',1,'now','now',?,NULL)`, articleWriteA, spaceID, projectA, articleWriteA, userID); err != nil {
		t.Fatal(err)
	}

	scoped, done, err := root.DB.scopedProjects(ctx, userID, []string{projectA})
	if err != nil {
		t.Fatal(err)
	}
	defer done()
	var scopeSetting string
	if err = scoped.QueryRow(`SELECT current_setting('app.mcp_projects',true)`).Scan(&scopeSetting); err != nil || scopeSetting != `[`+`"`+projectA+`"`+`]` {
		t.Fatalf("MCP scope setting=%q err=%v", scopeSetting, err)
	}
	for _, query := range []struct {
		table string
		query string
		args  []any
	}{
		{"spaces", `SELECT count(*) FROM spaces WHERE id=?`, []any{spaceID}},
		{"articles", `SELECT count(*) FROM articles WHERE id IN (?,?)`, []any{articleA, articleB}},
		{"revisions", `SELECT count(*) FROM revisions WHERE article_id IN (?,?)`, []any{articleA, articleB}},
		{"comments", `SELECT count(*) FROM comments WHERE id IN (?,?)`, []any{commentA, commentB}},
		{"attachments", `SELECT count(*) FROM attachments WHERE article_id IN (?,?)`, []any{articleA, articleB}},
		{"favorites", `SELECT count(*) FROM favorites WHERE article_id IN (?,?)`, []any{articleA, articleB}},
		{"article_fts", `SELECT count(*) FROM article_fts WHERE article_id IN (?,?)`, []any{articleA, articleB}},
	} {
		var n int
		want := 1
		if query.table == "spaces" {
			want = 0 // Project scope never grants access to the containing root space.
		}
		if err = scoped.QueryRow(query.query, query.args...).Scan(&n); err != nil || n != want {
			t.Errorf("%s visible rows=%d err=%v, want %d", query.table, n, err, want)
		}
	}
	var role string
	for _, table := range []string{"users", "memberships", "teams", "team_members", "corporate_space_state", "security_audit"} {
		var visible int
		if err := scoped.QueryRow("SELECT count(*) FROM " + table).Scan(&visible); err != nil || visible != 0 {
			t.Errorf("metadata table %s leaked: count=%d err=%v", table, visible, err)
		}
	}
	var visibleProjects int
	if err := scoped.QueryRow(`SELECT count(*) FROM projects`).Scan(&visibleProjects); err != nil || visibleProjects != 1 {
		t.Errorf("project metadata scope count=%d err=%v", visibleProjects, err)
	}
	if _, err := scoped.Exec(`INSERT INTO security_audit(actor_id,action,resource_type,resource_id,details,created_at) VALUES(?,'spoof','mcp_delegation','x','{}','now')`, userID); err == nil {
		t.Error("direct scoped audit insertion allowed")
	}
	if err = scoped.QueryRow(`SELECT corporate_effective_role(?,?,?)`, userID, spaceID, projectA).Scan(&role); err != nil || role != "reader" {
		t.Errorf("scoped owner role=%q err=%v, want reader", role, err)
	}
	var roleB string
	if err = scoped.QueryRow(`SELECT coalesce(corporate_effective_role(?,?,?), '')`, userID, spaceID, projectB).Scan(&roleB); err != nil || roleB != "" {
		t.Errorf("unlisted project role=%q err=%v, want no access", roleB, err)
	}
	var denied int
	if err = scoped.QueryRow(`SELECT count(*) FROM spaces WHERE id=?`, spaceID).Scan(&denied); err != nil || denied != 0 {
		t.Errorf("project scope exposed its containing root space: count=%d err=%v", denied, err)
	}
	if _, err = scoped.Exec(`INSERT INTO spaces(id,name,description,owner_id,personal) VALUES('mcp-denied-space','denied','',?,1)`, userID); err == nil {
		t.Error("scoped space insert was allowed")
	}
	if _, err = scoped.Exec(`UPDATE revisions SET message='changed' WHERE article_id=?`, articleA); err != nil {
		t.Errorf("revision update returned error: %v", err)
	}
	if _, err = scoped.Exec(`UPDATE comments SET body='changed' WHERE id=?`, commentA); err != nil {
		t.Errorf("comment update returned error: %v", err)
	}
	if _, err = scoped.Exec(`UPDATE favorites SET created_at='changed' WHERE user_id=? AND article_id=?`, userID, articleA); err != nil {
		t.Errorf("favorite update returned error: %v", err)
	}
	if _, err = scoped.Exec(`INSERT INTO favorites(user_id,article_id,created_at) VALUES(?,?,'now')`, userID, articleWriteA); err == nil {
		t.Error("scoped favorite insert was allowed")
	}
	if _, err = scoped.Exec(`DELETE FROM favorites WHERE article_id=?`, articleA); err != nil {
		t.Errorf("favorite delete returned error: %v", err)
	}
	var changed, favorites int
	if err = owner.DB.QueryRow(`SELECT count(*) FROM revisions WHERE article_id=? AND message='changed'`, articleA).Scan(&changed); err != nil || changed != 0 {
		t.Errorf("revision changed under MCP scope: count=%d err=%v", changed, err)
	}
	if err = owner.DB.QueryRow(`SELECT count(*) FROM comments WHERE id=? AND body='changed'`, commentA).Scan(&changed); err != nil || changed != 0 {
		t.Errorf("comment changed under MCP scope: count=%d err=%v", changed, err)
	}
	if err = owner.DB.QueryRow(`SELECT count(*) FROM favorites WHERE user_id=? AND article_id=? AND created_at='now'`, userID, articleA).Scan(&favorites); err != nil || favorites != 1 {
		t.Errorf("favorite changed under MCP scope: count=%d err=%v", favorites, err)
	}

	// Cleanup must roll back a forgotten transaction and clear both session GUCs.
	tx, err := scoped.Begin()
	if err != nil {
		t.Fatal(err)
	}
	probeID := "dlg_rollback_" + suffix
	if _, err = tx.Exec(`SELECT corporate_mcp_request_audit(?, 'test', 'rollback_probe')`, probeID); err != nil {
		t.Fatal(err)
	}
	done()
	resumed, release, err := scoped.resumeAfterTransfer(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	var resumedScope string
	if err = resumed.QueryRow(`SELECT current_setting('app.mcp_projects',true)`).Scan(&resumedScope); err != nil || resumedScope != scopeSetting {
		t.Errorf("resumed scope=%q err=%v, want copied scope %q", resumedScope, err, scopeSetting)
	}
	release()
	root.DB.pool.SetMaxOpenConns(1)
	conn, err := root.DB.pool.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var clearedUser, clearedProjects string
	if err = conn.QueryRowContext(ctx, `SELECT coalesce(current_setting('app.user_id',true),''), coalesce(current_setting('app.mcp_projects',true),'')`).Scan(&clearedUser, &clearedProjects); err != nil || clearedUser != "" || clearedProjects != "" {
		t.Errorf("cleanup left session values user=%q projects=%q err=%v", clearedUser, clearedProjects, err)
	}
	_ = conn.Close()
	check, clear, err := root.DB.scopedProjects(ctx, userID, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer clear()
	var cleared string
	if err = check.QueryRow(`SELECT current_setting('app.mcp_projects',true)`).Scan(&cleared); err != nil || cleared != "" {
		t.Errorf("reborrowed connection retained MCP scope %q err=%v", cleared, err)
	}
	if err = check.QueryRow(`SELECT corporate_effective_role(?,?, '')`, userID, spaceID).Scan(&role); err != nil || role != "owner" {
		t.Errorf("unscoped legacy role=%q err=%v, want owner", role, err)
	}
	clear()
	if err = owner.DB.QueryRow(`SELECT count(*) FROM security_audit WHERE resource_id=?`, probeID).Scan(&changed); err != nil || changed != 0 {
		t.Errorf("forgotten transaction was not rolled back: count=%d err=%v", changed, err)
	}

	emptyScope, clearEmpty, err := root.DB.scopedProjects(ctx, userID, []string{})
	if err != nil {
		t.Fatal(err)
	}
	defer clearEmpty()
	if err = emptyScope.QueryRow(`SELECT count(*) FROM articles WHERE id=?`, articleA).Scan(&denied); err != nil || denied != 0 {
		t.Errorf("empty project scope count=%d err=%v, want deny all", denied, err)
	}
	clearEmpty()
}
