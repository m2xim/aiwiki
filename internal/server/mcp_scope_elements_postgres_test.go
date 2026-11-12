package server

import (
	"context"
	"testing"
)

// Space elements, project-with-descendants, mixed modes and the explicit general write.
func TestMCPScopeElementsPostgres(t *testing.T) {
	e := newGPEnv(t)
	ctx := context.Background()
	ownerExecNoRLS(t, e.owner,
		`INSERT INTO spaces(id,name,description,owner_id,personal) VALUES('el_sp','el_sp','','',0)`,
		`INSERT INTO projects(id,space_id,parent_id,name,description,inherit_acl) VALUES('el_a','el_sp','','A','',1),('el_a1','el_sp','el_a','A1','',1),('el_a11','el_sp','el_a1','A11','',1),('el_b','el_sp','','B','',1)`,
		`INSERT INTO memberships(resource_type,resource_id,subject_type,subject_id,role) VALUES('space','el_sp','user','bob','editor')`,
		`INSERT INTO articles(id,space_id,parent_id,project_id,title,summary,body,kind,status,pinned,tags,properties,revision,created_at,updated_at,author) VALUES
		 ('el_root','el_sp','','','x','','','article','active',0,'[]','{}',1,'now','now','bob'),
		 ('el_a_art','el_sp','','el_a','x','','','article','active',0,'[]','{}',1,'now','now','bob'),
		 ('el_a11_art','el_sp','','el_a11','x','','','article','active',0,'[]','{}',1,'now','now','bob'),
		 ('el_b_art','el_sp','','el_b','x','','','article','active',0,'[]','{}',1,'now','now','bob')`,
	)
	as := func(scope ...mcpScopeElement) *Server {
		db, done, err := e.root.DB.scopedMCP(ctx, "bob", scope)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(done)
		cp := *e.root
		cp.DB = db
		cp.principal = &Principal{ID: "bob", MCPScope: scope, MCPDelegationID: "pat_test"}
		return &cp
	}
	el := func(t, id, m string) mcpScopeElement { return mcpScopeElement{t, id, m} }
	type want struct{ read, write []string }
	check := func(name string, s *Server, readable, unreadable, writable, unwritable [][2]string) {
		t.Helper()
		for _, c := range readable {
			if !s.gpReadable(c[0]) {
				t.Errorf("%s: cannot read %s", name, c[0])
			}
		}
		for _, c := range unreadable {
			if s.gpReadable(c[0]) {
				t.Errorf("%s: reads %s", name, c[0])
			}
		}
		for _, c := range writable {
			if !s.gpCanWrite(c[0], c[1]) {
				t.Errorf("%s: cannot write %v", name, c)
			}
		}
		for _, c := range unwritable {
			if s.gpCanWrite(c[0], c[1]) {
				t.Errorf("%s: writes %v", name, c)
			}
		}
	}
	// project element: the project and descendants, not the space root, not siblings
	check("project-write", as(el("project", "el_a", "write")),
		[][2]string{{"el_a_art"}, {"el_a11_art"}, {"gp_general_root_art"}},
		[][2]string{{"el_root"}, {"el_b_art"}, {"gp_other_root_art"}},
		[][2]string{{"el_sp", "el_a"}, {"el_sp", "el_a11"}},
		[][2]string{{"el_sp", ""}, {"el_sp", "el_b"}, {"gp_general", ""}})
	// space element: root and every project
	check("space-read", as(el("space", "el_sp", "read")),
		[][2]string{{"el_root"}, {"el_a11_art"}, {"el_b_art"}}, nil, nil,
		[][2]string{{"el_sp", ""}, {"el_sp", "el_b"}})
	// mixed: write on one project, read on its space
	check("mixed", as(el("space", "el_sp", "read"), el("project", "el_b", "write")),
		[][2]string{{"el_root"}, {"el_b_art"}}, nil,
		[][2]string{{"el_sp", "el_b"}}, [][2]string{{"el_sp", ""}, {"el_sp", "el_a"}})
	// explicit general write (bob is editor there); implicit general stays read-only
	check("general-explicit", as(el("space", "gp_general", "write"), el("project", "el_b", "read")),
		[][2]string{{"gp_general_root_art"}, {"el_b_art"}}, [][2]string{{"el_a_art"}},
		[][2]string{{"gp_general", ""}, {"gp_general", "gp_general_proj"}}, [][2]string{{"el_sp", "el_b"}})
	check("general-implicit", as(el("project", "el_b", "write")),
		[][2]string{{"gp_general_proj_art"}}, nil, nil, [][2]string{{"gp_general", ""}})
	// A key never exceeds its owner: alice has no membership in el_sp
	db, done, err := e.root.DB.scopedMCP(ctx, "alice", []mcpScopeElement{el("space", "el_sp", "write"), el("space", "gp_general", "write")})
	if err != nil {
		t.Fatal(err)
	}
	defer done()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM articles WHERE space_id='el_sp'`).Scan(&n); err != nil || n != 0 {
		t.Errorf("key exceeds owner: n=%d err=%v", n, err)
	}
	var role string
	if err := db.QueryRow(`SELECT coalesce(corporate_effective_role('alice','gp_general','gp_general_proj'),'')`).Scan(&role); err != nil || role != "reader" {
		t.Errorf("alice general write key role=%q err=%v", role, err)
	}
	// content writes need at least one write element
	ro := as(el("project", "el_a", "read"))
	var allowed bool
	if err := ro.DB.QueryRow(`SELECT corporate_mcp_content_write_allowed()`).Scan(&allowed); err != nil || allowed {
		t.Errorf("read-only scope content_write_allowed=%v err=%v", allowed, err)
	}
	_ = want{}
}
