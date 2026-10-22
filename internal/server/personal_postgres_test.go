package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// personalFixture resets the disposable database and returns a config that
// OpenPersonal can start repeatedly, plus the migration-owner connection.
func personalFixture(t *testing.T) (PersonalConfig, *database) {
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
	s3Env(t)
	_, endpoint := newFakeS3(t)
	return PersonalConfig{
		S3:          S3Config{Bucket: "wiki", Region: "us-east-1", Endpoint: endpoint.URL, PathStyle: true, AllowInsecureLoopback: true},
		DatabaseURL: appURL, MigrationDatabaseURL: ownerURL, RuntimeRole: "aiwiki_app",
		Name: "Хозяин",
	}, owner
}

func openPersonalForTest(t *testing.T, cfg PersonalConfig) *Server {
	t.Helper()
	s, err := OpenPersonal(context.Background(), cfg, t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func personalDo(h http.Handler, method, path, body string, headers ...string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Host = "127.0.0.1:8090"
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

func personalCount(t *testing.T, db *database, query string, args ...any) int {
	t.Helper()
	var n int
	if err := db.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// addForeignSpace creates a space and an article owned by another user the way a
// corporate deployment (or an import) would, with the migration-owner role.
func addForeignSpace(t *testing.T, owner *database, user, space string) {
	t.Helper()
	if _, err := owner.Exec(`INSERT INTO users(id,issuer,subject,name,admin) VALUES(?,?,?,?,0) ON CONFLICT DO NOTHING`, user, "https://identity.example.org", user, user); err != nil {
		t.Fatal(err)
	}
	scoped, done, err := owner.scopedMCP(context.Background(), user, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	defer done()
	if _, err = scoped.Exec(`INSERT INTO spaces(id,name,description,owner_id,personal) VALUES(?,?,'',?,0)`, space, "Space of "+user, user); err != nil {
		t.Fatal(err)
	}
	if _, err = scoped.Exec(`INSERT INTO memberships(resource_type,resource_id,subject_type,subject_id,role) VALUES('space',?,'user',?,'owner')`, space, user); err != nil {
		t.Fatal(err)
	}
	now := "2026-01-01T00:00:00Z"
	if _, err = scoped.Exec(`INSERT INTO articles(id,space_id,title,summary,body,kind,tags,properties,status,revision,author,created_at,updated_at,project_id) VALUES(?,?,?,?,?,'article','[]','{}','active',1,?,?,?,'')`, "art-"+space, space, "Note of "+user, "", "body of "+user, user, now, now); err != nil {
		t.Fatal(err)
	}
}

func TestPersonalOwnerRowsExactlyOnceAndIdempotent(t *testing.T) {
	cfg, owner := personalFixture(t)
	first := openPersonalForTest(t, cfg)
	// The unscoped runtime connection sees no spaces (row-level security), so
	// count through a connection scoped to the owner.
	viewer, done, err := first.DB.scopedMCP(context.Background(), "owner", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	defer done()
	check := func(label string, wantAudit int) {
		t.Helper()
		if n := personalCount(t, owner, `SELECT count(*) FROM users WHERE id='owner' AND issuer='aiwiki:personal' AND subject='owner' AND admin=1 AND disabled=0`); n != 1 {
			t.Fatalf("%s: owner rows %d", label, n)
		}
		if n := personalCount(t, owner, `SELECT count(*) FROM users WHERE id<>'visitor'`); n != 1 {
			t.Fatalf("%s: unexpected users %d", label, n)
		}
		spaces := personalCount(t, owner, `SELECT count(*) FROM corporate_space_state`)
		if n := personalCount(t, owner, `SELECT count(*) FROM memberships WHERE resource_type='space' AND subject_type='user' AND subject_id='owner' AND role='owner'`); n != spaces {
			t.Fatalf("%s: memberships %d of %d spaces", label, n, spaces)
		}
		if n := personalCount(t, owner, `SELECT count(*) FROM memberships WHERE subject_id<>'visitor'`); n != spaces {
			t.Fatalf("%s: unexpected memberships %d", label, n)
		}
		if n := personalCount(t, viewer, `SELECT count(*) FROM spaces WHERE owner_id<>'owner' OR personal<>0`); n != 0 {
			t.Fatalf("%s: spaces with foreign owner or personal flag", label)
		}
		if n := personalCount(t, owner, `SELECT count(*) FROM security_audit WHERE action='personal.owner.ensure'`); n != wantAudit {
			t.Fatalf("%s: audit rows %d, want %d", label, n, wantAudit)
		}
	}
	check("first start, no spaces yet", 1)
	personalSpace(t, first.Handler(), "Один")
	personalSpace(t, first.Handler(), "Два")
	check("first start", 1)
	for i := 0; i < 2; i++ {
		again := openPersonalForTest(t, cfg)
		_ = again
		check("restart", 1)
	}
	// Without the migration URL (already migrated database) the guard and the
	// idempotent ensure still run through the runtime role.
	noMigration := cfg
	noMigration.MigrationDatabaseURL = ""
	openPersonalForTest(t, noMigration)
	check("restart without migration role", 1)

	// A changed name is updated; nothing else about the row changes.
	renamed := cfg
	renamed.Name = "Другое имя"
	openPersonalForTest(t, renamed)
	var name string
	var admin, disabled int
	if err = owner.QueryRow(`SELECT name,admin,disabled FROM users WHERE id='owner'`).Scan(&name, &admin, &disabled); err != nil || name != "Другое имя" || admin != 1 || disabled != 0 {
		t.Fatalf("rename: %q %d %d %v", name, admin, disabled, err)
	}
	check("rename", 2)

	// A space that appeared since (corporate-style data, or an import) is picked up.
	addForeignSpace(t, owner, "visitor", "visitor-space")
	openPersonalForTest(t, renamed)
	check("late space", 3)
	// ... but the foreign user keeps only his own membership, and the original
	// user rows are untouched.
	if n := personalCount(t, owner, `SELECT count(*) FROM memberships WHERE subject_id='visitor'`); n != 1 {
		t.Fatalf("visitor memberships %d", n)
	}
}

func TestPersonalRefusesToTakeOverForeignOwnerRow(t *testing.T) {
	cfg, owner := personalFixture(t)
	openPersonalForTest(t, cfg)
	for _, set := range []string{`issuer='https://identity.example.org'`, `admin=0`, `disabled=1`} {
		if _, err := owner.Exec(`UPDATE users SET ` + set + ` WHERE id='owner'`); err != nil {
			t.Fatal(err)
		}
		if s, err := OpenPersonal(context.Background(), cfg, t.TempDir(), ""); err == nil {
			s.Close()
			t.Fatalf("started over a tampered owner row (%s)", set)
		}
		if _, err := owner.Exec(`UPDATE users SET issuer='aiwiki:personal',admin=1,disabled=0 WHERE id='owner'`); err != nil {
			t.Fatal(err)
		}
	}
	openPersonalForTest(t, cfg)
}

func TestPersonalRequiresS3AndDatabase(t *testing.T) {
	cfg, _ := personalFixture(t)
	noS3 := cfg
	noS3.S3.Bucket = ""
	if s, err := OpenPersonal(context.Background(), noS3, t.TempDir(), ""); err == nil {
		s.Close()
		t.Fatal("personal mode started without S3")
	}
	noDB := cfg
	noDB.DatabaseURL = ""
	if s, err := OpenPersonal(context.Background(), noDB, t.TempDir(), ""); err == nil {
		s.Close()
		t.Fatal("personal mode started without a database")
	}
	// The migration role is not allowed as the runtime role.
	privileged := cfg
	privileged.DatabaseURL = cfg.MigrationDatabaseURL
	if s, err := OpenPersonal(context.Background(), privileged, t.TempDir(), ""); err == nil {
		s.Close()
		t.Fatal("personal mode accepted the table-owner role at runtime")
	}
}

func TestPersonalServesOwnerEverywhere(t *testing.T) {
	cfg, owner := personalFixture(t)
	firstRun := openPersonalForTest(t, cfg)
	personalSpace(t, firstRun.Handler(), "Один")
	firstRun.Close()
	addForeignSpace(t, owner, "visitor", "visitor-space")
	s := openPersonalForTest(t, cfg)
	h := s.Handler()

	me := personalDo(h, "GET", "/api/me", "")
	if me.Code != 200 {
		t.Fatalf("me %d %s", me.Code, me.Body.String())
	}
	var meBody struct {
		Mode string `json:"mode"`
		User struct {
			ID    string `json:"id"`
			Name  string `json:"name"`
			Admin bool   `json:"admin"`
		} `json:"user"`
	}
	raw := me.Body.Bytes()
	if err := json.Unmarshal(raw, &meBody); err != nil || meBody.Mode != "personal" || meBody.User.ID != "owner" || meBody.User.Name != "Хозяин" || !meBody.User.Admin {
		t.Fatalf("me %s %v", raw, err)
	}
	var generic map[string]json.RawMessage
	json.Unmarshal(raw, &generic)
	if _, hasCSRF := generic["csrf_token"]; len(generic) != 2 || hasCSRF {
		t.Fatalf("unexpected /api/me shape %s", raw)
	}

	// Spaces of every kind are listed.
	list := personalDo(h, "GET", "/api/spaces", "")
	if list.Code != 200 || !strings.Contains(list.Body.String(), "visitor-space") || !strings.Contains(list.Body.String(), "Один") {
		t.Fatalf("spaces %d %s", list.Code, list.Body.String())
	}
	// Read and edit the article that another user created.
	got := personalDo(h, "GET", "/api/articles/art-visitor-space", "")
	if got.Code != 200 || !strings.Contains(got.Body.String(), "body of visitor") {
		t.Fatalf("read foreign article %d %s", got.Code, got.Body.String())
	}
	edit := personalDo(h, "PUT", "/api/articles/art-visitor-space", `{"base_revision":1,"body":"edited by owner","author":"forged"}`)
	if edit.Code != 200 {
		t.Fatalf("edit foreign article %d %s", edit.Code, edit.Body.String())
	}
	var edited Article
	json.Unmarshal(edit.Body.Bytes(), &edited)
	if edited.Author != "owner" || edited.Revision != 2 {
		t.Fatalf("edited article %+v", edited)
	}
	// A new space gets the owner membership, personal=0, and holds new articles.
	created := personalDo(h, "POST", "/api/spaces", `{"name":"Новое","description":"d"}`)
	if created.Code != 201 {
		t.Fatalf("create space %d %s", created.Code, created.Body.String())
	}
	var space struct{ ID string }
	json.Unmarshal(created.Body.Bytes(), &space)
	if n := personalCount(t, owner, `SELECT count(*) FROM memberships WHERE resource_id=? AND subject_id='owner' AND role='owner'`, space.ID); n != 1 {
		t.Fatalf("new space membership %d", n)
	}
	var ownerID string
	var personal int
	scoped, done, err := s.DB.scopedMCP(context.Background(), "owner", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	defer done()
	if err = scoped.QueryRow(`SELECT owner_id,personal FROM spaces WHERE id=?`, space.ID).Scan(&ownerID, &personal); err != nil || ownerID != "owner" || personal != 0 {
		t.Fatalf("new space owner=%q personal=%d %v", ownerID, personal, err)
	}
	art := personalDo(h, "POST", "/api/articles", `{"space_id":"`+space.ID+`","title":"Fresh","body":"text"}`)
	if art.Code != 201 {
		t.Fatalf("create article %d %s", art.Code, art.Body.String())
	}
	// The admin surface works for the owner.
	for _, path := range []string{"/api/admin/users", "/api/catalog", "/api/projects", "/api/teams"} {
		if rr := personalDo(h, "GET", path, ""); rr.Code != 200 {
			t.Errorf("%s: %d %s", path, rr.Code, rr.Body.String())
		}
	}
	// MCP works without a key.
	mcp := personalDo(h, "POST", "/mcp", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"wiki_search","arguments":{"query":"Fresh"}}}`)
	if mcp.Code != 200 || !strings.Contains(mcp.Body.String(), "Fresh") {
		t.Fatalf("mcp %d %s", mcp.Code, mcp.Body.String())
	}
	// Host/Origin checks of trusted mode still apply.
	req := httptest.NewRequest("GET", "/api/me", nil)
	req.Host = "evil.example.org"
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != 403 {
		t.Fatalf("foreign host %d", rr.Code)
	}
	if rr := personalDo(h, "GET", "/api/me", "", "Origin", "http://evil.example.org"); rr.Code != 403 {
		t.Fatalf("foreign origin %d", rr.Code)
	}
}

func TestPersonalAuthorizationHeaderRefusedAndOAuthRoutesAbsent(t *testing.T) {
	cfg, _ := personalFixture(t)
	h := openPersonalForTest(t, cfg).Handler()
	for _, tc := range [][2]string{{"GET", "/api/me"}, {"GET", "/api/articles"}, {"POST", "/mcp"}, {"GET", "/api/health"}, {"GET", "/"}, {"GET", "/files/x"}} {
		for _, value := range []string{"Bearer anything", "Basic Zm9vOmJhcg==", "x"} {
			rr := personalDo(h, tc[0], tc[1], "{}", "Authorization", value)
			if rr.Code != 401 {
				t.Errorf("%s %s with %q: %d", tc[0], tc[1], value, rr.Code)
			}
			if strings.Contains(rr.Body.String(), "anything") {
				t.Errorf("echoed credential: %s", rr.Body.String())
			}
		}
	}
	if rr := personalDo(h, "POST", "/mcp", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`); rr.Code != 200 {
		t.Fatalf("mcp without key %d %s", rr.Code, rr.Body.String())
	}
	for _, path := range []string{"/auth/login", "/auth/callback", "/auth/logout", "/.well-known/oauth-protected-resource", "/.well-known/oauth-protected-resource/mcp", "/.well-known/oauth-authorization-server", "/api/mcp/credentials", "/api/mcp/delegations", "/api/mcp/service-accounts"} {
		for _, method := range []string{"GET", "POST"} {
			if rr := personalDo(h, method, path, ""); rr.Code != 404 {
				t.Errorf("%s %s: %d, want 404", method, path, rr.Code)
			}
		}
	}
	for _, path := range []string{"/api/health", "/api/ready"} {
		if rr := personalDo(h, "GET", path, ""); rr.Code != 200 {
			t.Errorf("%s: %d %s", path, rr.Code, rr.Body.String())
		}
	}
}

func TestPersonalSecondUserGetsNoAccessAndRLSStaysOn(t *testing.T) {
	cfg, owner := personalFixture(t)
	s := openPersonalForTest(t, cfg)
	h := s.Handler()
	general := personalSpace(t, h, "Общее")
	made := personalDo(h, "POST", "/api/articles", `{"space_id":"`+general+`","title":"Mine","body":"private note"}`)
	if made.Code != 201 {
		t.Fatalf("create %d %s", made.Code, made.Body.String())
	}
	var a Article
	json.Unmarshal(made.Body.Bytes(), &a)
	if _, err := owner.Exec(`INSERT INTO users(id,issuer,subject,name,admin) VALUES('mallory','https://identity.example.org','mallory','Mallory',1)`); err != nil {
		t.Fatal(err)
	}
	// A user created directly in the database has no access through RLS ...
	mallory := corporateAs(t, &Server{DB: s.DB}, "mallory")
	if rr := corporateHTTP(mallory, "GET", "/api/articles/"+a.ID, ""); rr.Code != 404 {
		t.Fatalf("second user reads owner article: %d %s", rr.Code, rr.Body.String())
	}
	if n := personalCount(t, mallory.DB, `SELECT count(*) FROM articles`); n != 0 {
		t.Fatalf("second user sees %d articles", n)
	}
	// ... the HTTP surface cannot be used to act as him ...
	for _, header := range [][]string{{"X-Forwarded-User", "mallory"}, {"X-User", "mallory"}, {"Cookie", "aiwiki_session=mallory"}} {
		rr := personalDo(h, "GET", "/api/me", "", header...)
		if rr.Code != 200 || !strings.Contains(rr.Body.String(), `"id":"owner"`) {
			t.Fatalf("identity from header %v: %d %s", header, rr.Code, rr.Body.String())
		}
	}
	// ... and the unscoped runtime connection sees nothing at all.
	if n := personalCount(t, s.DB, `SELECT count(*) FROM articles`); n != 0 {
		t.Fatalf("unscoped runtime role sees %d articles", n)
	}
	if n := personalCount(t, s.DB, `SELECT count(*) FROM spaces`); n != 0 {
		t.Fatalf("unscoped runtime role sees %d spaces", n)
	}
	// The owner's access is a membership, not a bypass: dropping it hides the
	// space from the running personal server, and the next start restores it.
	if _, err := owner.Exec(`DELETE FROM memberships WHERE subject_id='owner' AND resource_id=?`, general); err != nil {
		t.Fatal(err)
	}
	if rr := personalDo(h, "GET", "/api/articles/"+a.ID, ""); rr.Code != 404 {
		t.Fatalf("article readable without membership: %d", rr.Code)
	}
	h2 := openPersonalForTest(t, cfg).Handler()
	if rr := personalDo(h2, "GET", "/api/articles/"+a.ID, ""); rr.Code != 200 {
		t.Fatalf("membership not restored: %d %s", rr.Code, rr.Body.String())
	}
	// Forced RLS on every content table, and the runtime role is a plain role.
	if n := personalCount(t, s.DB, `SELECT count(*) FROM pg_class WHERE relname IN ('spaces','articles','revisions','comments','attachments','favorites','article_fts') AND relrowsecurity AND relforcerowsecurity`); n != 7 {
		t.Fatalf("forced RLS on %d of 7 content tables", n)
	}
	if n := personalCount(t, s.DB, `SELECT count(*) FROM pg_roles WHERE rolname=current_user AND (rolsuper OR rolbypassrls)`); n != 0 {
		t.Fatal("runtime role bypasses RLS")
	}
}

func TestPersonalMetricsAndReadinessMode(t *testing.T) {
	cfg, _ := personalFixture(t)
	s := openPersonalForTest(t, cfg)
	rr := httptest.NewRecorder()
	s.MetricsHandler("").ServeHTTP(rr, httptest.NewRequest("GET", "/metrics", nil))
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), `mode="personal"`) {
		t.Fatalf("metrics %d %s", rr.Code, rr.Body.String())
	}
	if _, ok := s.readiness().checks["storage"]; !ok {
		t.Fatal("storage readiness check is missing in personal mode")
	}
}

func personalSpace(t *testing.T, h http.Handler, name string) string {
	t.Helper()
	rr := personalDo(h, "POST", "/api/spaces", `{"name":"`+name+`","description":""}`)
	if rr.Code != 201 {
		t.Fatalf("create space %d %s", rr.Code, rr.Body.String())
	}
	var v struct{ ID string }
	json.Unmarshal(rr.Body.Bytes(), &v)
	return v.ID
}

func TestPersonalOwnerCannotBeDisabled(t *testing.T) {
	cfg, _ := personalFixture(t)
	h := openPersonalForTest(t, cfg).Handler()
	rr := personalDo(h, "PATCH", "/api/admin/users/"+PersonalOwnerID, `{"disabled":true}`)
	if rr.Code != 403 {
		t.Fatalf("disable owner: %d %s", rr.Code, rr.Body.String())
	}
	// The instance still works and restarts.
	if rr := personalDo(h, "GET", "/api/spaces", ""); rr.Code != 200 {
		t.Fatalf("after refused disable: %d %s", rr.Code, rr.Body.String())
	}
	openPersonalForTest(t, cfg)
}
