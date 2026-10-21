package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
)

// This suite uses a disposable PostgreSQL database, never the home wiki.
func corporateFixture(t *testing.T) (*Server, *Server) {
	t.Helper()
	ownerURL, appURL := os.Getenv("AIWIKI_TEST_POSTGRES_OWNER"), os.Getenv("AIWIKI_TEST_POSTGRES")
	if ownerURL == "" || appURL == "" {
		t.Skip("set AIWIKI_TEST_POSTGRES_OWNER and AIWIKI_TEST_POSTGRES to a disposable database")
	}
	owner, err := openPostgres(context.Background(), ownerURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { owner.Close() })
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
	if _, err = owner.Exec(`GRANT USAGE ON SCHEMA public TO aiwiki_app; GRANT SELECT,INSERT,UPDATE,DELETE ON ALL TABLES IN SCHEMA public TO aiwiki_app; GRANT USAGE,SELECT ON ALL SEQUENCES IN SCHEMA public TO aiwiki_app; GRANT EXECUTE ON ALL FUNCTIONS IN SCHEMA public TO aiwiki_app; REVOKE UPDATE,DELETE ON security_audit FROM aiwiki_app; REVOKE ALL ON schema_migrations FROM aiwiki_app; REVOKE EXECUTE ON FUNCTION corporate_owner_effective_role(text,text,text) FROM aiwiki_app`); err != nil {
		t.Fatal(err)
	}
	db, err := openPostgres(context.Background(), appURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	for _, id := range []string{"alice", "bob", "admin", "reader", "editor", "commenter"} {
		if _, err = owner.Exec(`INSERT INTO users(id,issuer,subject,name,admin) VALUES(?,?,?,?,?)`, id, "https://identity.example.org", id, id, boolInt(id == "admin")); err != nil {
			t.Fatal(err)
		}
	}
	return s, &Server{DB: db, DataDir: s.DataDir}
}
func corporateAs(t *testing.T, s *Server, id string) *Server {
	t.Helper()
	db, cleanup, err := s.DB.scoped(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup)
	cp := *s
	cp.DB = db
	cp.principal = &Principal{ID: id, Name: id, Admin: id == "admin"}
	return &cp
}
func corporateHTTP(s *Server, method, path, body string) *httptest.ResponseRecorder {
	if s.DB.pg && s.principal != nil {
		db, done, err := s.DB.resumeAfterTransfer(context.Background())
		if err != nil {
			panic(err)
		}
		defer done()
		cp := *s
		cp.DB = db
		s = &cp
	}
	rr := httptest.NewRecorder()
	s.routes().ServeHTTP(rr, httptest.NewRequest(method, path, strings.NewReader(body)))
	return rr
}
func corporateCreateSpace(t *testing.T, s *Server) string {
	t.Helper()
	r := corporateHTTP(s, "POST", "/api/corporate/spaces", `{"name":"Private Alice","personal":true}`)
	if r.Code != 201 {
		t.Fatalf("space %d %s", r.Code, r.Body.String())
	}
	var v map[string]any
	json.Unmarshal(r.Body.Bytes(), &v)
	return v["id"].(string)
}
func TestCorporatePostgresIsolation(t *testing.T) {
	owner, root := corporateFixture(t)
	alice := corporateAs(t, root, "alice")
	bob := corporateAs(t, root, "bob")
	admin := corporateAs(t, root, "admin")
	space := corporateCreateSpace(t, alice)
	r := corporateHTTP(alice, "POST", "/api/articles", `{"space_id":"`+space+`","title":"Hidden deployment secret","summary":"classified","body":"# Private\nsecret needle","author":"forged"}`)
	if r.Code != 201 {
		t.Fatalf("create %d %s", r.Code, r.Body.String())
	}
	var a Article
	json.Unmarshal(r.Body.Bytes(), &a)
	if a.Author != "alice" {
		t.Fatalf("spoofed author %q", a.Author)
	}
	for _, user := range []*Server{bob, admin} {
		for _, path := range []string{"/api/articles/" + a.ID, "/api/articles/" + a.ID + "/history", "/api/articles/" + a.ID + "/links", "/api/articles/" + a.ID + "/comments", "/api/articles/" + a.ID + "/attachments", "/api/spaces/" + space + "/audit"} {
			rr := corporateHTTP(user, "GET", path, "")
			if rr.Code != 404 {
				t.Errorf("hidden %s user%s: %d %s", path, user.principal.ID, rr.Code, rr.Body.String())
			}
		}
		for _, path := range []string{"/api/spaces", "/api/articles", "/api/search?q=secret", "/api/bootstrap", "/api/export", "/api/catalog", "/api/tags?space_id=" + space} {
			rr := corporateHTTP(user, "GET", path, "")
			if strings.Contains(rr.Body.String(), "Hidden deployment secret") || strings.Contains(rr.Body.String(), "Private Alice") || strings.Contains(rr.Body.String(), "classified") {
				t.Errorf("leak %s: %s", path, rr.Body.String())
			}
		}
		raw, _ := json.Marshal(user.toolReadChunk(map[string]json.RawMessage{"id": json.RawMessage(`"` + a.ID + `"`), "max_chars": json.RawMessage(`10`)}))
		if bytes.Contains(raw, []byte("secret needle")) {
			t.Fatal("MCP chunk leaked")
		}
	}
	if _, err := owner.DB.Exec(`INSERT INTO memberships(resource_type,resource_id,subject_type,subject_id,role) VALUES('space',?,'user','bob','reader')`, space); err != nil {
		t.Fatal(err)
	}
	if rr := corporateHTTP(bob, "GET", "/api/articles/"+a.ID, ""); rr.Code != 200 {
		t.Fatalf("newgrant %d %s", rr.Code, rr.Body.String())
	}
	if rr := corporateHTTP(bob, "PUT", "/api/articles/"+a.ID, `{"base_revision":1,"body":"hacked"}`); rr.Code < 400 {
		t.Fatal("reader wrote article")
	}
	for _, user := range []*Server{alice, bob} {
		if rr := corporateHTTP(user, "PUT", "/api/favorites/"+a.ID, ""); rr.Code != 204 {
			t.Fatalf("favorite %d %s", rr.Code, rr.Body.String())
		}
	}
	if rr := corporateHTTP(alice, "DELETE", "/api/favorites/"+a.ID, ""); rr.Code != 204 {
		t.Fatal(rr.Body.String())
	}
	if rr := corporateHTTP(bob, "GET", "/api/favorites", ""); !strings.Contains(rr.Body.String(), a.ID) {
		t.Fatal("favorites shared across users")
	}
	if _, err := owner.DB.Exec(`DELETE FROM memberships WHERE subject_id='bob'`); err != nil {
		t.Fatal(err)
	}
	if rr := corporateHTTP(bob, "GET", "/api/articles/"+a.ID, ""); rr.Code != 404 {
		t.Fatal("revoked reader still reads")
	}
	if _, err := owner.DB.Exec(`UPDATE users SET disabled=1 WHERE id='alice'`); err != nil {
		t.Fatal(err)
	}
	if rr := corporateHTTP(alice, "GET", "/api/articles/"+a.ID, ""); rr.Code != 404 {
		t.Fatal("disabled current scope reads")
	}
}
func TestCorporateConcurrentRevision(t *testing.T) {
	_, root := corporateFixture(t)
	alice := corporateAs(t, root, "alice")
	space := corporateCreateSpace(t, alice)
	rr := corporateHTTP(alice, "POST", "/api/articles", `{"space_id":"`+space+`","title":"Concurrent","body":"original"}`)
	if rr.Code != 201 {
		t.Fatal(rr.Body.String())
	}
	var a Article
	json.Unmarshal(rr.Body.Bytes(), &a)
	var wg sync.WaitGroup
	codes := make(chan int, 2)
	for i := 0; i < 2; i++ {
		cp := corporateAs(t, root, "alice")
		wg.Add(1)
		go func(s *Server) {
			defer wg.Done()
			r := corporateHTTP(s, "PUT", "/api/articles/"+a.ID, `{"base_revision":1,"body":"winner"}`)
			codes <- r.Code
		}(cp)
	}
	wg.Wait()
	close(codes)
	ok, conflict := 0, 0
	for c := range codes {
		if c == 200 {
			ok++
		}
		if c == 409 {
			conflict++
		}
	}
	if ok != 1 || conflict != 1 {
		t.Fatalf("want one success and conflict, got %d/%d", ok, conflict)
	}
}
