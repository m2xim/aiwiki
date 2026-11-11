package server

import (
	"context"
	"os"
	"strings"
	"testing"
)

// A database at the previous release (mcp_access v1-v5, role_scope v1, old-format keys)
// is upgraded with mcp_access v6 and role_scope v2: keys are backfilled into project
// elements and keep their access, now also covering descendants and the general space.
func TestMCPScopeV6UpgradeKeepsAccessPostgres(t *testing.T) {
	ownerURL := os.Getenv("AIWIKI_TEST_POSTGRES_OWNER")
	appURL := os.Getenv("AIWIKI_TEST_POSTGRES")
	if ownerURL == "" || appURL == "" {
		t.Skip("set AIWIKI_TEST_POSTGRES_OWNER and AIWIKI_TEST_POSTGRES to a disposable database")
	}
	mcpScopeVersionCap, roleScopeVersionCap = 5, 1
	defer func() { mcpScopeVersionCap, roleScopeVersionCap = 0, 0 }()
	ctx := context.Background()
	owner, err := openPostgres(ctx, ownerURL)
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
	var cols int
	if err = owner.QueryRow(`SELECT count(*) FROM information_schema.columns WHERE table_name='mcp_credentials' AND column_name='scope'`).Scan(&cols); err != nil || cols != 0 {
		t.Fatalf("old schema already has scope: %d %v", cols, err)
	}
	ownerExecNoRLS(t, &Server{DB: owner},
		`INSERT INTO users(id,issuer,subject,name,admin) VALUES('alice','https://identity.example.org','alice','alice',0)`,
		`INSERT INTO spaces(id,name,description,owner_id,personal) VALUES('sp','sp','','',0),('general','general','','',0)`,
		`INSERT INTO projects(id,space_id,parent_id,name,description,inherit_acl) VALUES('pa','sp','','A','',1),('pa_child','sp','pa','Child','',1),('pb','sp','','B','',1)`,
		`INSERT INTO memberships(resource_type,resource_id,subject_type,subject_id,role) VALUES('space','sp','user','alice','editor')`,
		`INSERT INTO articles(id,space_id,parent_id,project_id,title,summary,body,kind,status,pinned,tags,properties,revision,created_at,updated_at,author) VALUES
		 ('a_pa','sp','','pa','x','','','article','active',0,'[]','{}',1,'now','now','alice'),
		 ('a_child','sp','','pa_child','x','','','article','active',0,'[]','{}',1,'now','now','alice'),
		 ('a_pb','sp','','pb','x','','','article','active',0,'[]','{}',1,'now','now','alice'),
		 ('a_gen','general','','','x','','','article','active',0,'[]','{}',1,'now','now','alice')`,
		`INSERT INTO mcp_credentials(id,owner_id,name,secret_hash,project_ids,mode,expires_at) VALUES
		 ('pat_r','alice','r',repeat('a',64),'["pa"]','read',now()+interval '1 day'),
		 ('pat_w','alice','w',repeat('b',64),'["pa","pb"]','write',now()+interval '1 day')`,
	)
	// Upgrade.
	mcpScopeVersionCap, roleScopeVersionCap = 0, 0
	if err = s.migrateCorporate(); err != nil {
		t.Fatal(err)
	}
	var scope1, scope2 string
	if err = owner.QueryRow(`SELECT scope::text FROM mcp_credentials WHERE id='pat_r'`).Scan(&scope1); err != nil || !strings.Contains(scope1, `"type": "project"`) || !strings.Contains(scope1, `"mode": "read"`) {
		t.Fatalf("backfill read key: %s %v", scope1, err)
	}
	if err = owner.QueryRow(`SELECT scope::text FROM mcp_credentials WHERE id='pat_w'`).Scan(&scope2); err != nil || strings.Count(scope2, `"mode": "write"`) != 2 {
		t.Fatalf("backfill write key: %s %v", scope2, err)
	}
	if _, err = owner.Exec(`GRANT USAGE ON SCHEMA public TO aiwiki_app; GRANT SELECT,INSERT,UPDATE,DELETE ON ALL TABLES IN SCHEMA public TO aiwiki_app; GRANT USAGE,SELECT ON ALL SEQUENCES IN SCHEMA public TO aiwiki_app; GRANT EXECUTE ON ALL FUNCTIONS IN SCHEMA public TO aiwiki_app; REVOKE UPDATE,DELETE ON security_audit FROM aiwiki_app; REVOKE ALL ON schema_migrations FROM aiwiki_app; REVOKE EXECUTE ON FUNCTION corporate_owner_effective_role(text,text,text) FROM aiwiki_app`); err != nil {
		t.Fatal(err)
	}
	app, err := openPostgres(ctx, appURL)
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	if err = app.validateRuntimeSchema(); err != nil {
		t.Fatalf("guards after upgrade: %v", err)
	}
	root := &Server{DB: app, DataDir: s.DataDir}
	readable := func(scope []mcpScopeElement) map[string]bool {
		db, done, err := root.DB.scopedMCP(ctx, "alice", scope)
		if err != nil {
			t.Fatal(err)
		}
		defer done()
		out := map[string]bool{}
		rows, err := db.Query(`SELECT id FROM articles ORDER BY id`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		for rows.Next() {
			var id string
			rows.Scan(&id)
			out[id] = true
		}
		return out
	}
	r := readable(projScope("read", "pa"))
	if !r["a_pa"] || !r["a_child"] || !r["a_gen"] || r["a_pb"] {
		t.Errorf("read key after upgrade (project + descendants + general): %v", r)
	}
	w := readable(projScope("write", "pa", "pb"))
	if !w["a_pa"] || !w["a_child"] || !w["a_pb"] || !w["a_gen"] {
		t.Errorf("write key after upgrade: %v", w)
	}
	// The stored scope is what the credential lookup and audit function compare against.
	db, done, err := root.DB.scopedMCP(ctx, "alice", projScope("read", "pa"))
	if err != nil {
		t.Fatal(err)
	}
	defer done()
	if _, err = db.Exec(`SELECT corporate_mcp_credential_audit('pat_r','tools/call','wiki_read')`); err != nil {
		t.Errorf("audit with backfilled scope: %v", err)
	}
	wdb, wdone, _ := root.DB.scopedMCP(ctx, "alice", projScope("write", "pa", "pb"))
	defer wdone()
	if _, err = wdb.Exec(`UPDATE articles SET body='ok' WHERE id='a_child'`); err != nil {
		t.Fatal(err)
	}
	if _, err = wdb.Exec(`INSERT INTO articles(id,space_id,parent_id,project_id,title,summary,body,kind,status,pinned,tags,properties,revision,created_at,updated_at,author) VALUES('a_gen2','general','','','x','','','article','active',0,'[]','{}',1,'now','now','alice')`); err == nil {
		t.Error("write key without an explicit general write element wrote to general")
	}
	// Legacy columns are no longer required.
	if _, err = owner.Exec(`INSERT INTO mcp_credentials(id,owner_id,name,secret_hash,scope,expires_at) VALUES('pat_new','alice','n',repeat('c',64),'[{"type":"space","id":"sp","mode":"write"}]',now()+interval '1 day')`); err != nil {
		t.Errorf("insert without legacy columns: %v", err)
	}
}
