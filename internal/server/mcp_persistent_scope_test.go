package server

import (
	"context"
	"database/sql"
	"testing"
)

func TestMCPPersistentWriteScope(t *testing.T) {
	owner, root := corporateFixture(t)
	ctx := context.Background()
	seed, release, err := owner.DB.scoped(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	for _, q := range []string{
		`INSERT INTO spaces(id,name,description,owner_id,personal) VALUES('pat-space','fixture','','alice',0)`,
		`INSERT INTO memberships(resource_type,resource_id,subject_type,subject_id,role) VALUES('space','pat-space','user','alice','owner')`,
		`INSERT INTO projects(id,space_id,parent_id,name,description,inherit_acl) VALUES('pat-a','pat-space','','A','',0),('pat-b','pat-space','','B','',0)`,
		`INSERT INTO memberships(resource_type,resource_id,subject_type,subject_id,role) VALUES('project','pat-a','user','alice','owner'),('project','pat-b','user','alice','owner')`,
		`INSERT INTO articles(id,space_id,parent_id,project_id,title,summary,body,kind,status,pinned,tags,properties,revision,created_at,updated_at,author) VALUES('pat-doc-a','pat-space','','pat-a','A','','','article','active',0,'[]','{}',1,'now','now','alice'),('pat-doc-b','pat-space','','pat-b','B','','','article','active',0,'[]','{}',1,'now','now','alice')`,
	} {
		if _, err = seed.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	scope, done, err := root.DB.scopedMCP(ctx, "alice", []string{"pat-a"}, "write")
	if err != nil {
		t.Fatal(err)
	}
	defer done()
	var role string
	if err = scope.QueryRow(`SELECT corporate_effective_role('alice','pat-space','pat-a')`).Scan(&role); err != nil || role != "editor" {
		t.Fatalf("ceiling=%q err=%v", role, err)
	}
	checkAffected := func(q string, want int64) {
		t.Helper()
		r, e := scope.Exec(q)
		if e != nil {
			t.Fatal(e)
		}
		n, e := r.RowsAffected()
		if e != nil || n != want {
			t.Fatalf("affected=%d want=%d err=%v query=%s", n, want, e, q)
		}
	}
	checkAffected(`UPDATE articles SET body='allowed' WHERE id='pat-doc-a'`, 1)
	checkAffected(`UPDATE articles SET body='forbidden' WHERE id='pat-doc-b'`, 0)
	checkAffected(`UPDATE spaces SET name='forbidden' WHERE id='pat-space'`, 0)
	checkAffected(`UPDATE projects SET name='forbidden' WHERE id='pat-a'`, 0)
	checkAffected(`DELETE FROM memberships WHERE resource_id='pat-a'`, 0)
	for _, table := range []string{"users", "memberships", "security_audit", "mcp_credentials", "mcp_service_accounts"} {
		var n int
		if err = scope.QueryRow(`SELECT count(*) FROM ` + table).Scan(&n); err != nil || n != 0 {
			t.Fatalf("metadata %s count=%d err=%v", table, n, err)
		}
	}
	// ACL changes constrain subsequent statements, even on an admitted scope.
	if _, err = seed.Exec(`UPDATE memberships SET role='reader' WHERE resource_id='pat-a'`); err != nil {
		t.Fatal(err)
	}
	checkAffected(`UPDATE articles SET body='after-demotion' WHERE id='pat-doc-a'`, 0)
	if err = scope.QueryRow(`SELECT corporate_effective_role('alice','pat-space','pat-a')`).Scan(&role); err != nil || role != "reader" {
		t.Fatalf("demotion=%q err=%v", role, err)
	}
	// Missing/invalid modes fail closed, even if scope JSON remains valid.
	if _, err = scope.Exec(`SELECT set_config('app.mcp_mode','',false)`); err != nil {
		t.Fatal(err)
	}
	var absent sql.NullString
	if err = scope.QueryRow(`SELECT corporate_effective_role('alice','pat-space','pat-a')`).Scan(&absent); err != nil || absent.Valid {
		t.Fatalf("missing mode allowed role: %v %v", absent, err)
	}
	done()
	resumed, finish, err := scope.resumeAfterTransfer(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var mode string
	if err = resumed.QueryRow(`SELECT current_setting('app.mcp_mode',true)`).Scan(&mode); err != nil || mode != "write" {
		t.Fatalf("resumed mode=%q err=%v", mode, err)
	}
	finish()
	root.DB.pool.SetMaxOpenConns(1)
	c, err := root.DB.pool.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err = c.QueryRowContext(ctx, `SELECT coalesce(current_setting('app.mcp_mode',true),'')`).Scan(&mode); err != nil || mode != "" {
		t.Fatalf("leaked mode=%q err=%v", mode, err)
	}
}

func TestMCPPersistentInvalidScopeMode(t *testing.T) {
	d := &database{}
	for _, test := range []struct {
		projects []string
		mode     string
	}{{nil, "write"}, {[]string{}, ""}, {[]string{"p"}, "owner"}} {
		if _, _, err := d.scopedMCP(context.Background(), "alice", test.projects, test.mode); err == nil {
			t.Fatalf("accepted invalid scope %+v", test)
		}
	}
}

func TestMCPPersistentMetadataGuards(t *testing.T) {
	owner, root := corporateFixture(t)
	for _, q := range []string{
		`INSERT INTO mcp_service_accounts(id,owner_id,name) VALUES('svc_alice','alice','Alice'),('svc_bob','bob','Bob')`,
		`INSERT INTO mcp_credentials(id,owner_id,name,secret_hash,project_ids,mode,expires_at) VALUES('pat_alice','alice','Alice',repeat('a',64),'["project"]','read',now()+interval '1 day'),('pat_bob','bob','Bob',repeat('b',64),'["project"]','read',now()+interval '1 day')`,
	} {
		if _, err := owner.DB.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	scope, done, err := root.DB.scoped(context.Background(), "alice")
	if err != nil {
		t.Fatal(err)
	}
	defer done()
	for _, table := range []string{"mcp_credentials", "mcp_service_accounts"} {
		var count int
		if err = scope.QueryRow(`SELECT count(*) FROM ` + table).Scan(&count); err != nil || count != 1 {
			t.Fatalf("owner isolation %s count=%d err=%v", table, count, err)
		}
	}
	for _, q := range []string{
		`UPDATE mcp_credentials SET project_ids='["expanded"]' WHERE id='pat_alice'`,
		`UPDATE mcp_credentials SET owner_id='bob' WHERE id='pat_alice'`,
		`UPDATE mcp_credentials SET expires_at=expires_at+interval '1 hour' WHERE id='pat_alice'`,
		`UPDATE mcp_credentials SET mode='write' WHERE id='pat_alice'`,
		`UPDATE mcp_service_accounts SET owner_id='bob' WHERE id='svc_alice'`,
	} {
		if _, err = scope.Exec(q); err == nil {
			t.Fatalf("immutable authority changed: %s", q)
		}
	}
	for _, q := range []string{
		`DELETE FROM mcp_credentials WHERE id='pat_alice'`,
		`DELETE FROM mcp_service_accounts WHERE id='svc_alice'`,
		`UPDATE mcp_credentials SET revoked_at=now() WHERE id='pat_bob'`,
	} {
		r, e := scope.Exec(q)
		if e != nil {
			t.Fatal(e)
		}
		n, _ := r.RowsAffected()
		if n != 0 {
			t.Fatalf("forbidden metadata DML affected %d: %s", n, q)
		}
	}
	if _, err = scope.Exec(`UPDATE mcp_credentials SET revoked_at=now() WHERE id='pat_alice'`); err != nil {
		t.Fatal(err)
	}
	if _, err = scope.Exec(`UPDATE mcp_credentials SET revoked_at=NULL WHERE id='pat_alice'`); err == nil {
		t.Fatal("revoked credential resurrected")
	}
	if _, err = scope.Exec(`UPDATE mcp_service_accounts SET disabled=1 WHERE id='svc_alice'`); err != nil {
		t.Fatal(err)
	}
	if _, err = scope.Exec(`UPDATE mcp_service_accounts SET disabled=0 WHERE id='svc_alice'`); err == nil {
		t.Fatal("disabled account re-enabled")
	}
	// The separate empty-user authentication pool must still locate all hashes.
	var count int
	if err = root.DB.QueryRow(`SELECT count(*) FROM mcp_credentials`).Scan(&count); err != nil || count != 2 {
		t.Fatalf("auth lookup count=%d err=%v", count, err)
	}
}

func TestMCPPersistentStartupGuards(t *testing.T) {
	for _, test := range []struct{ name, sql string }{
		{"metadata owner policy", `ALTER POLICY mcp_scope_mcp_credentials_default ON mcp_credentials USING(true) WITH CHECK(true)`},
		{"extra permissive policy", `CREATE POLICY unexpected_allow ON mcp_credentials FOR SELECT USING(true)`},
		{"immutable trigger", `ALTER TABLE mcp_credentials DISABLE TRIGGER mcp_credential_immutable`},
		{"space writes", `ALTER POLICY mcp_scope_spaces_update ON spaces USING(true) WITH CHECK(true)`},
		{"unused attachment writes", `ALTER POLICY mcp_scope_attachments_insert ON attachments WITH CHECK(corporate_mcp_content_write_allowed())`},
	} {
		t.Run(test.name, func(t *testing.T) {
			owner, root := corporateFixture(t)
			if err := root.DB.validateRuntimeSchema(); err != nil {
				t.Fatalf("valid schema rejected: %v", err)
			}
			if _, err := owner.DB.Exec(test.sql); err != nil {
				t.Fatal(err)
			}
			if err := root.DB.validateRuntimeSchema(); err == nil {
				t.Fatal("tampered MCP guards accepted")
			}
		})
	}
}
