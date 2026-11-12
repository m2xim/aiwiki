package server

import (
	"context"
	"testing"
)

// Space elements with their own modes, the explicit general write, and keys never exceeding their owner.
func TestMCPScopeElementsPostgres(t *testing.T) {
	e := newGPEnv(t)
	ctx := context.Background()
	ownerExecNoRLS(t, e.owner,
		`INSERT INTO spaces(id,name,description,owner_id) VALUES('el_a','el_a','',''),('el_b','el_b','','')`,
		`INSERT INTO memberships(resource_type,resource_id,subject_type,subject_id,role) VALUES('space','el_a','user','bob','editor'),('space','el_b','user','bob','editor')`,
		`INSERT INTO articles(id,space_id,parent_id,project_id,title,summary,body,kind,status,pinned,tags,properties,revision,created_at,updated_at,author) VALUES
		 ('el_a_art','el_a','','','x','','','article','active',0,'[]','{}',1,'now','now','bob'),
		 ('el_b_art','el_b','','','x','','','article','active',0,'[]','{}',1,'now','now','bob')`,
	)
	as := func(user string, scope ...mcpScopeElement) *Server { return e.asScope(user, scope...) }
	el := func(t, id, m string) mcpScopeElement { return mcpScopeElement{t, id, m} }
	check := func(name string, s *Server, readable, unreadable, writable, unwritable []string) {
		t.Helper()
		for _, c := range readable {
			if !s.gpReadable(c) {
				t.Errorf("%s: cannot read %s", name, c)
			}
		}
		for _, c := range unreadable {
			if s.gpReadable(c) {
				t.Errorf("%s: reads %s", name, c)
			}
		}
		for _, c := range writable {
			if !s.gpCanWrite(c, "") {
				t.Errorf("%s: cannot write %s", name, c)
			}
		}
		for _, c := range unwritable {
			if s.gpCanWrite(c, "") {
				t.Errorf("%s: writes %s", name, c)
			}
		}
	}
	// mixed modes: write one space, read another; general readable, not writable
	check("mixed", as("bob", el("space", "el_a", "write"), el("space", "el_b", "read")),
		[]string{"el_a_art", "el_b_art", "gp_general_root_art"}, []string{"gp_other_root_art"},
		[]string{"el_a"}, []string{"el_b", "gp_general"})
	// the explicit general write works for an owner who is an editor there
	check("general-explicit", as("bob", el("space", "gp_general", "write"), el("space", "el_b", "read")),
		[]string{"gp_general_root_art", "el_b_art"}, []string{"el_a_art"},
		[]string{"gp_general"}, []string{"el_b"})
	check("general-implicit", as("bob", el("space", "el_a", "write")),
		[]string{"gp_general_root_art"}, nil, nil, []string{"gp_general"})
	// A key never exceeds its owner: alice has no membership in el_a
	db, done, err := e.root.DB.scopedMCP(ctx, "alice", []mcpScopeElement{el("space", "el_a", "write"), el("space", "gp_general", "write")})
	if err != nil {
		t.Fatal(err)
	}
	defer done()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM articles WHERE space_id='el_a'`).Scan(&n); err != nil || n != 0 {
		t.Errorf("key exceeds owner: n=%d err=%v", n, err)
	}
	var role string
	if err := db.QueryRow(`SELECT coalesce(corporate_effective_role('alice','gp_general',''),'')`).Scan(&role); err != nil || role != "reader" {
		t.Errorf("alice general write key role=%q err=%v", role, err)
	}
	// content writes need at least one write element
	ro := as("bob", el("space", "el_a", "read"))
	var allowed bool
	if err := ro.DB.QueryRow(`SELECT corporate_mcp_content_write_allowed()`).Scan(&allowed); err != nil || allowed {
		t.Errorf("read-only scope content_write_allowed=%v err=%v", allowed, err)
	}
}
