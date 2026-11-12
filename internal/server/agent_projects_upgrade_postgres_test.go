package server

import (
	"context"
	"os"
	"strings"
	"testing"
)

// A database at the current production versions (mcp_access v7, role_scope v2, every other component
// current) with v6-format keys, folders holding articles and artifacts, folder grants and the rest is
// upgraded with agent_projects v1 and mcp_access v8.
func TestAgentProjectsUpgradeFromV7Postgres(t *testing.T) {
	ownerURL := os.Getenv("AIWIKI_TEST_POSTGRES_OWNER")
	appURL := os.Getenv("AIWIKI_TEST_POSTGRES")
	if ownerURL == "" || appURL == "" {
		t.Skip("set AIWIKI_TEST_POSTGRES_OWNER and AIWIKI_TEST_POSTGRES to a disposable database")
	}
	mcpScopeVersionCap, agentProjectsVersionCap = 7, -1
	defer func() { mcpScopeVersionCap, agentProjectsVersionCap = 0, 0 }()
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
	// The state really is the production one.
	var v7, rs2, ap int
	_ = owner.QueryRow(`SELECT count(*) FROM schema_migrations WHERE component='mcp_access' AND version=7`).Scan(&v7)
	_ = owner.QueryRow(`SELECT count(*) FROM schema_migrations WHERE component='role_scope' AND version=2`).Scan(&rs2)
	_ = owner.QueryRow(`SELECT count(*) FROM schema_migrations WHERE component IN ('agent_projects') OR component='mcp_access' AND version=8`).Scan(&ap)
	if v7 != 1 || rs2 != 1 || ap != 0 {
		t.Fatalf("starting state: v7=%d role_scope2=%d new=%d", v7, rs2, ap)
	}
	ownerExecNoRLS(t, &Server{DB: owner},
		`INSERT INTO users(id,issuer,subject,name,admin) VALUES
		 ('alice','https://identity.example.org','alice','Alice',0),('bob','https://identity.example.org','bob','Bob',0),('carol','https://identity.example.org','carol','Carol',0)`,
		`INSERT INTO spaces(id,name,description,owner_id,personal) VALUES('sp','Space','','alice',0),('sp2','Other','','bob',0),('general','General','','',0)`,
		`INSERT INTO projects(id,space_id,parent_id,name,description,inherit_acl) VALUES
		 ('pa','sp','','Folder A','',1),('pa_child','sp','pa','Child','',1),('pc','sp','','Closed','',0),('px','sp2','','Folder X','',1)`,
		`INSERT INTO memberships(resource_type,resource_id,subject_type,subject_id,role) VALUES
		 ('space','sp','user','alice','owner'),('space','sp','user','carol','reader'),('space','sp2','user','bob','owner'),
		 ('project','pa','user','alice','editor'),('project','pa','user','bob','reader'),('project','pc','user','carol','owner'),('project','px','user','alice','manager')`,
		`INSERT INTO articles(id,space_id,parent_id,project_id,title,summary,body,kind,status,pinned,tags,properties,revision,created_at,updated_at,author) VALUES
		 ('a_root','sp','','','root','','','article','active',0,'[]','{}',1,'now','now','alice'),
		 ('a_pa','sp','','pa','in A','','','article','active',0,'[]','{}',1,'now','now','alice'),
		 ('a_child','sp','','pa_child','in child','','','article','active',0,'[]','{}',2,'now','now','alice'),
		 ('a_pc','sp','','pc','in closed','','','article','active',0,'[]','{}',1,'now','now','carol'),
		 ('a_px','sp2','','px','in X','','','article','active',0,'[]','{}',1,'now','now','bob')`,
	)
	if _, err = owner.Exec(`ALTER TABLE revisions NO FORCE ROW LEVEL SECURITY`); err != nil {
		t.Fatal(err)
	}
	if _, err = owner.Exec(`INSERT INTO revisions(article_id,revision,author,message,created_at,snapshot) VALUES
	 ('a_pa',1,'alice','','now','{"id":"a_pa","space_id":"sp","project_id":"pa","title":"in A"}'),
	 ('a_child',2,'alice','','now','{"id":"a_child","space_id":"sp","project_id":"pa_child","title":"in child"}')`); err != nil {
		t.Fatal(err)
	}
	if _, err = owner.Exec(`ALTER TABLE revisions FORCE ROW LEVEL SECURITY`); err != nil {
		t.Fatal(err)
	}
	// An artifact in a folder.
	if _, err = owner.Exec(`ALTER TABLE artifacts NO FORCE ROW LEVEL SECURITY; ALTER TABLE artifacts DISABLE TRIGGER USER`); err != nil {
		t.Fatal(err)
	}
	if _, err = owner.Exec(`INSERT INTO artifacts(id,space_id,project_id,title,owner_id,revision,author,created_at,updated_at) VALUES('art_pa','sp','pa','Art','alice',1,'alice','now','now')`); err != nil {
		t.Fatal(err)
	}
	if _, err = owner.Exec(`ALTER TABLE artifacts ENABLE TRIGGER USER; ALTER TABLE artifacts FORCE ROW LEVEL SECURITY`); err != nil {
		t.Fatal(err)
	}
	// Keys in the v6 format: space and project elements, read and write, a revoked one, one with an unknown project.
	if _, err = owner.Exec(`INSERT INTO mcp_credentials(id,owner_id,service_account_id,name,secret_hash,scope,expires_at) VALUES
	 ('pat_proj','alice','','Folder key',repeat('a',64),'[{"type":"project","id":"pa","mode":"write"},{"type":"project","id":"pa_child","mode":"read"},{"type":"space","id":"sp2","mode":"read"}]',now()+interval '10 days'),
	 ('pat_space','alice','','Space key',repeat('b',64),'[{"type":"space","id":"sp","mode":"write"},{"type":"space","id":"general","mode":"read"}]',now()+interval '10 days'),
	 ('pat_gone','alice','','Gone',repeat('c',64),'[{"type":"project","id":"missing","mode":"read"}]',now()+interval '10 days'),
	 ('pat_bob','bob','','Bobs',repeat('d',64),'[{"type":"project","id":"px","mode":"write"}]',now()+interval '10 days'),
	 ('pat_old','alice','','Revoked',repeat('e',64),'[{"type":"space","id":"sp","mode":"read"}]',now()+interval '10 days')`); err != nil {
		t.Fatal(err)
	}
	if _, err = owner.Exec(`UPDATE mcp_credentials SET revoked_at=now() WHERE id='pat_old'`); err != nil {
		t.Fatal(err)
	}

	// Upgrade.
	mcpScopeVersionCap, agentProjectsVersionCap = 0, 0
	if err = s.migrateCorporate(); err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	// The migrations ran once, in the intended components, with role_scope last.
	rows, err := owner.Query(`SELECT component||':'||version FROM schema_migrations ORDER BY applied_at, component, version`)
	if err != nil {
		t.Fatal(err)
	}
	var applied []string
	for rows.Next() {
		var c string
		rows.Scan(&c)
		applied = append(applied, c)
	}
	rows.Close()
	joined := strings.Join(applied, ",")
	if !strings.Contains(joined, "mcp_access:8") || !strings.Contains(joined, "agent_projects:1") || !strings.HasSuffix(joined, "role_scope:2") && !strings.Contains(joined, "role_scope:2") {
		t.Fatalf("applied: %s", joined)
	}
	if strings.LastIndex(joined, "role_scope") < strings.LastIndex(joined, "agent_projects") {
		t.Fatalf("role_scope must stay the last component: %s", joined)
	}

	tables := []string{"articles", "revisions", "artifacts"}
	setForce := func(on bool) {
		for _, tbl := range tables {
			word := map[bool]string{true: "FORCE", false: "NO FORCE"}[on]
			if _, err := owner.Exec(`ALTER TABLE ` + tbl + ` ` + word + ` ROW LEVEL SECURITY`); err != nil {
				t.Fatal(err)
			}
		}
	}
	setForce(false)
	count := func(q string, args ...any) int {
		t.Helper()
		var n int
		if err := owner.QueryRow(q, args...).Scan(&n); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		return n
	}
	// A. Folders gone, content stays in its space.
	if count(`SELECT count(*) FROM projects`) != 0 || count(`SELECT count(*) FROM memberships WHERE resource_type='project'`) != 0 {
		t.Fatal("folders or folder grants remain")
	}
	if count(`SELECT count(*) FROM articles WHERE project_id<>''`) != 0 || count(`SELECT count(*) FROM artifacts WHERE project_id<>''`) != 0 || count(`SELECT count(*) FROM revisions WHERE project_id<>'' OR jsonb_exists(snapshot::jsonb,'project_id')`) != 0 {
		t.Fatal("project_id not cleared")
	}
	if count(`SELECT count(*) FROM articles WHERE space_id='sp'`) != 4 || count(`SELECT count(*) FROM articles`) != 5 || count(`SELECT count(*) FROM artifacts WHERE space_id='sp'`) != 1 {
		t.Fatal("content lost or moved")
	}
	// Space grants are untouched; folder grants were NOT turned into space grants (access narrows).
	if count(`SELECT count(*) FROM memberships WHERE resource_type='space'`) != 3 {
		t.Fatalf("space grants: %d", count(`SELECT count(*) FROM memberships`))
	}
	if count(`SELECT count(*) FROM memberships WHERE resource_type='space' AND resource_id='sp' AND subject_id='bob'`) != 0 {
		t.Fatal("a folder grant was widened to its space")
	}
	// The audit row lists the removed grants.
	var audit string
	if err = owner.QueryRow(`SELECT details::text FROM security_audit WHERE action='migration.project_grants_removed'`).Scan(&audit); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"count": 4`, `"resource_id": "pa"`, `"subject_id": "bob"`, `"resource_id": "pc"`, `"resource_id": "px"`} {
		if !strings.Contains(audit, want) {
			t.Fatalf("audit lacks %s: %s", want, audit)
		}
	}
	// D. Keys: a private project per live key, built from its scope; folders become their space with the max mode.
	type projRow struct{ name, owner, vis string }
	proj := func(keyID string) (id string, p projRow, rows map[string]string) {
		t.Helper()
		var ids string
		if err := owner.QueryRow(`SELECT agent_project_ids::text FROM mcp_credentials WHERE id=?`, keyID).Scan(&ids); err != nil {
			t.Fatal(err)
		}
		id = strings.Trim(ids, `["]`)
		if err := owner.QueryRow(`SELECT name,owner_id,visibility FROM agent_projects WHERE id=?`, id).Scan(&p.name, &p.owner, &p.vis); err != nil {
			t.Fatalf("project of %s (%s): %v", keyID, ids, err)
		}
		rows = map[string]string{}
		r, err := owner.Query(`SELECT space_id,mode FROM agent_project_spaces WHERE project_id=?`, id)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Close()
		for r.Next() {
			var sp, m string
			r.Scan(&sp, &m)
			rows[sp] = m
		}
		return
	}
	_, p1, r1 := proj("pat_proj")
	if p1.name != "Ключ «Folder key»" || p1.owner != "alice" || p1.vis != "private" || len(r1) != 2 || r1["sp"] != "write" || r1["sp2"] != "read" {
		t.Fatalf("folder key project: %+v %v", p1, r1)
	}
	_, p2, r2 := proj("pat_space")
	if p2.owner != "alice" || len(r2) != 2 || r2["sp"] != "write" || r2["general"] != "read" {
		t.Fatalf("space key project: %+v %v", p2, r2)
	}
	_, p3, r3 := proj("pat_gone")
	if len(r3) != 0 || p3.owner != "alice" {
		t.Fatalf("a key of a vanished folder yields an empty project (general read only): %+v %v", p3, r3)
	}
	if _, pb, rb := proj("pat_bob"); pb.owner != "bob" || rb["sp2"] != "write" || len(rb) != 1 {
		t.Fatalf("bob's key: %+v %v", pb, rb)
	}
	if count(`SELECT count(*) FROM mcp_credentials WHERE id='pat_old' AND agent_project_ids IS NULL`) != 1 {
		t.Fatal("a revoked key gets no project")
	}
	if count(`SELECT count(*) FROM mcp_credentials WHERE read_only`) != 0 {
		t.Fatal("migrated keys are not read-only")
	}
	if count(`SELECT count(*) FROM mcp_credentials WHERE scope IS NOT NULL`) != 5 {
		t.Fatal("scope stays as history")
	}
	if count(`SELECT count(*) FROM security_audit WHERE action='migration.key_project_created'`) != 4 {
		t.Fatal("one audit row per migrated key")
	}
	// scope is history: the immutability guard still holds for the runtime role (checked below with the app role).

	setForce(true)
	// Runtime: guards pass, access follows the new model.
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
	id1, _, _ := proj("pat_proj")
	db, done, scope, err := app.scopedAgent(ctx, "alice", "alice", "pat_proj", id1)
	if err != nil {
		t.Fatal(err)
	}
	defer done()
	if len(scope) != 2 {
		t.Fatalf("session scope %v", scope)
	}
	readable := map[string]bool{}
	arows, err := db.Query(`SELECT id FROM articles`)
	if err != nil {
		t.Fatal(err)
	}
	for arows.Next() {
		var id string
		arows.Scan(&id)
		readable[id] = true
	}
	arows.Close()
	// alice: owner of sp (write via the key), no role in sp2 (read row yields nothing), folder grants gone.
	if len(readable) != 4 || readable["a_px"] || !readable["a_pa"] || !readable["a_child"] || !readable["a_pc"] {
		t.Fatalf("readable after upgrade: %v", readable)
	}
	if _, err = db.Exec(`UPDATE articles SET body='ok' WHERE id='a_pa'`); err != nil {
		t.Fatalf("write in sp through the key: %v", err)
	}
	// carol lost her folder ownership: she reads sp only as a space reader and sees no folder privilege.
	cdb, cdone, err := app.scoped(ctx, "carol")
	if err != nil {
		t.Fatal(err)
	}
	defer cdone()
	var n int
	if err = cdb.QueryRow(`SELECT count(*) FROM articles WHERE id='a_pc'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("carol reads the space: %d %v", n, err)
	}
	if _, err = cdb.Exec(`UPDATE articles SET body='x' WHERE id='a_pc'`); err != nil {
		t.Log("reader write refused:", err)
	}
	var body string
	owner.QueryRow(`SELECT body FROM articles WHERE id='a_pc'`).Scan(&body)
	if body == "x" {
		t.Fatal("carol, a reader, wrote after losing the folder ownership")
	}
	// Bob: lost the reader role on folder A (which never gave him the space).
	bdb, bdone, _ := app.scoped(ctx, "bob")
	defer bdone()
	if err = bdb.QueryRow(`SELECT count(*) FROM articles WHERE id='a_pa'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("bob still reads the folder: %d %v", n, err)
	}
	// The migration is idempotent: a second run changes nothing.
	if err = s.migrateCorporate(); err != nil {
		t.Fatal(err)
	}
	if count(`SELECT count(*) FROM agent_projects`) != 4 {
		t.Fatalf("a second run created projects: %d", count(`SELECT count(*) FROM agent_projects`))
	}
}

// A fresh database ends at the same schema; role_scope is the last component and the equivalence
// helper function stays intact.
func TestAgentProjectsFreshSchemaOrder(t *testing.T) {
	owner, _ := corporateFixture(t)
	// applied_at has second resolution: role_scope must not be older than any other component.
	var older int
	if err := owner.DB.QueryRow(`SELECT count(*) FROM schema_migrations m WHERE m.component<>'role_scope' AND m.applied_at > (SELECT max(r.applied_at) FROM schema_migrations r WHERE r.component='role_scope')`).Scan(&older); err != nil || older != 0 {
		t.Fatalf("role_scope is not the last migration: %d %v", older, err)
	}
	var n int
	if err := owner.DB.QueryRow(`SELECT count(*) FROM schema_migrations WHERE (component='mcp_access' AND version=8) OR (component='agent_projects' AND version=1)`).Scan(&n); err != nil || n != 2 {
		t.Fatalf("new versions: %d %v", n, err)
	}
	// Every new SECURITY DEFINER function pins its search_path and is closed to PUBLIC.
	var bad int
	if err := owner.DB.QueryRow(`SELECT count(*) FROM pg_proc p WHERE (p.proname LIKE 'corporate_ap_%' OR p.proname LIKE 'corporate_agent_%' OR p.proname IN ('corporate_mcp_project_scope','corporate_user_project_scope','corporate_admin_space_directory','corporate_admin_space_exists','corporate_space_assignable_v2','corporate_mcp_credential_set_projects','corporate_reject_project_insert')) AND (p.proconfig IS NULL OR NOT EXISTS (SELECT 1 FROM unnest(p.proconfig) c WHERE c LIKE 'search_path=%') OR EXISTS (SELECT 1 FROM aclexplode(COALESCE(p.proacl,acldefault('f',p.proowner))) a WHERE a.grantee=0))`).Scan(&bad); err != nil || bad != 0 {
		t.Fatalf("functions without a pinned search_path or open to PUBLIC: %d %v", bad, err)
	}
}

// The chain from a database at mcp_access v6 (the release before the last) runs through v7 to v8 in one go.
func TestAgentProjectsUpgradeFromV6Postgres(t *testing.T) {
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
	ownerExecNoRLS(t, &Server{DB: owner},
		`INSERT INTO users(id,issuer,subject,name,admin) VALUES('alice','https://identity.example.org','alice','Alice',0)`,
		`INSERT INTO spaces(id,name,description,owner_id,personal) VALUES('sp','Space','','alice',0)`,
		`INSERT INTO mcp_credentials(id,owner_id,name,secret_hash,scope,expires_at) VALUES('pat_v6','alice','v6 key',repeat('a',64),'[{"type":"space","id":"sp","mode":"read"}]',now()+interval '1 day')`,
	)
	mcpScopeVersionCap = 0
	if err = s.migrateCorporate(); err != nil {
		t.Fatal(err)
	}
	var ids string
	var ro bool
	if err = owner.QueryRow(`SELECT agent_project_ids::text, read_only FROM mcp_credentials WHERE id='pat_v6'`).Scan(&ids, &ro); err != nil || ids == "" || ro {
		t.Fatalf("v6 key not migrated: %q %v %v", ids, ro, err)
	}
	var n int
	if err = owner.QueryRow(`SELECT count(*) FROM agent_project_spaces WHERE space_id='sp' AND mode='read'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("project rows: %d %v", n, err)
	}
}
