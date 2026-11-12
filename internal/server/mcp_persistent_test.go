package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// Everything below that touches persistence uses the disposable PostgreSQL
// fixture (skipped without AIWIKI_TEST_POSTGRES*). Admission is local: the
// owner must not be disabled and must have logged in within the login window.

// persistentProbe counts passes through the checkpoint between the first,
// rate-limited verification and the final re-read; hook runs inside it.
type persistentProbe struct {
	mu    sync.Mutex
	calls int
	hook  func()
}

func (p *persistentProbe) checkpoint() {
	p.mu.Lock()
	p.calls++
	hook := p.hook
	p.mu.Unlock()
	if hook != nil {
		hook()
	}
}

func (p *persistentProbe) setHook(f func()) { p.mu.Lock(); p.hook = f; p.mu.Unlock() }

func (p *persistentProbe) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

type persistentClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *persistentClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *persistentClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

type persistentRoute func(*Server) http.HandlerFunc

var (
	persistentCredentials = func(s *Server) http.HandlerFunc { return s.mcpCredentials }
	persistentCredentialR = func(s *Server) http.HandlerFunc { return s.mcpCredential }
	persistentAccounts    = func(s *Server) http.HandlerFunc { return s.mcpServiceAccounts }
	persistentAccountR    = func(s *Server) http.HandlerFunc { return s.mcpServiceAccount }
)

type persistentEnv struct {
	t      *testing.T
	owner  *Server
	root   *Server
	admin  *database // migration owner, unscoped: tampering and inspection
	seedDB *database // migration owner scoped to the space owner: spaces/projects/ACLs
	x      *authHarness
	iss    string
	prov   *persistentProbe
	clk    *persistentClock
}

func newPersistentEnv(t *testing.T) *persistentEnv {
	t.Helper()
	owner, root := corporateFixture(t)
	x := newAuthHarness(t, nil)
	x.a.users = authSQLUsers{db: root.DB}
	e := &persistentEnv{
		t: t, owner: owner, root: root, admin: owner.DB, x: x, iss: x.iss.srv.URL,
		prov: &persistentProbe{},
		clk:  &persistentClock{t: time.Now().UTC().Truncate(time.Second)},
	}
	x.a.persistent = e.access()
	root.auth = x.a
	return e
}

func (e *persistentEnv) access() *mcpPersistentAccess {
	return &mcpPersistentAccess{db: e.root.DB, now: e.clk.now, checkpoint: e.prov.checkpoint}
}

// user inserts a local user for the harness issuer, logged in now, and returns
// a browser-side Server for it plus its id.
func (e *persistentEnv) user(sub string) (*Server, string) {
	e.t.Helper()
	uid := authUserID(e.iss, sub)
	e.exec(`INSERT INTO users(id,issuer,subject,name,admin,last_login_at) VALUES(?,?,?,?,0,?)`, uid, e.iss, sub, sub, e.clk.now())
	db, cleanup, err := e.root.DB.scoped(context.Background(), uid)
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(cleanup)
	cp := *e.root
	cp.DB = db
	cp.auth = e.x.a
	cp.principal = &Principal{ID: uid, Issuer: e.iss, Subject: sub, Name: sub}
	return &cp, uid
}

// seed creates spaces and agent projects. Agent projects (named like the old folders for brevity):
//
//	pa, pb, pe  alice's private projects over sa/sb/se (alice editor there), mode write
//	pr          alice's private project over sr (alice reader), mode write
//	pn          bob's private project (invisible to alice)
//	pub         bob's public project over sbob (alice has no role there)
//	pbob        bob's private project over sbob
func (e *persistentEnv) seed(alice, bob string) {
	e.t.Helper()
	db, done, err := e.owner.DB.scoped(context.Background(), alice)
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(done)
	e.seedDB = db
	bdb, bdone, err := e.owner.DB.scoped(context.Background(), bob)
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(bdone)
	for _, sp := range []string{"sa", "sb", "se", "sr", "sarch"} {
		e.seedExec(`INSERT INTO spaces(id,name,owner_id) VALUES(?,?,?)`, sp, "Space "+sp, alice)
	}
	e.seedExec(`UPDATE spaces SET archived_at='2020-01-01T00:00:00Z' WHERE id='sarch'`)
	for sp, role := range map[string]string{"sa": "editor", "sb": "editor", "se": "editor", "sr": "reader", "sarch": "editor"} {
		e.member(sp, alice, role)
	}
	for _, sp := range []string{"sn", "sbob"} {
		if _, err := bdb.Exec(`INSERT INTO spaces(id,name,owner_id) VALUES(?,?,?)`, sp, "Space "+sp, bob); err != nil {
			e.t.Fatal(err)
		}
		if _, err := bdb.Exec(`INSERT INTO memberships(resource_type,resource_id,subject_type,subject_id,role) VALUES('space',?,'user',?,'owner')`, sp, bob); err != nil {
			e.t.Fatal(err)
		}
	}
	for p, space := range map[string]string{"pa": "sa", "pb": "sb", "pe": "se", "pr": "sr"} {
		e.seedExec(`INSERT INTO agent_projects(id,name,owner_id,visibility) VALUES(?,?,?,'private')`, p, p, alice)
		e.seedExec(`INSERT INTO agent_project_spaces(project_id,row_id,space_id,mode) VALUES(?,?,?,'write')`, p, "row_"+p, space)
	}
	for p, vis := range map[string]string{"pn": "private", "pub": "public", "pbob": "private"} {
		space := "sbob"
		if p == "pn" {
			space = "sn"
		}
		if _, err := bdb.Exec(`INSERT INTO agent_projects(id,name,owner_id,visibility) VALUES(?,?,?,?)`, p, p, bob, vis); err != nil {
			e.t.Fatal(err)
		}
		if _, err := bdb.Exec(`INSERT INTO agent_project_spaces(project_id,row_id,space_id,mode) VALUES(?,?,?,'write')`, p, "row_"+p, space); err != nil {
			e.t.Fatal(err)
		}
	}
}

func (e *persistentEnv) seedExec(q string, args ...any) {
	e.t.Helper()
	if _, err := e.seedDB.Exec(q, args...); err != nil {
		e.t.Fatal(err)
	}
}

func (e *persistentEnv) member(space, user, role string) {
	e.t.Helper()
	e.seedExec(`INSERT INTO memberships(resource_type,resource_id,subject_type,subject_id,role) VALUES('space',?,'user',?,?)`, space, user, role)
}

func (e *persistentEnv) exec(q string, args ...any) {
	e.t.Helper()
	if _, err := e.admin.Exec(q, args...); err != nil {
		e.t.Fatal(err)
	}
}

func (e *persistentEnv) count(q string, args ...any) int {
	e.t.Helper()
	var n int
	if err := e.admin.QueryRow(q, args...).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

func (e *persistentEnv) text(q string, args ...any) string {
	e.t.Helper()
	var s string
	if err := e.admin.QueryRow(q, args...).Scan(&s); err != nil {
		e.t.Fatal(err)
	}
	return s
}

// call runs a management handler on a fresh request-scoped connection.
func (e *persistentEnv) call(s *Server, route persistentRoute, method, body, id string) *httptest.ResponseRecorder {
	db, done, err := s.DB.resumeAfterTransfer(context.Background())
	if err != nil {
		panic(err)
	}
	defer done()
	cp := *s
	cp.DB = db
	req := httptest.NewRequest(method, "/api/mcp/credentials", strings.NewReader(body))
	if id != "" {
		req.SetPathValue("id", id)
	}
	rec := httptest.NewRecorder()
	route(&cp)(rec, req)
	return rec
}

// requestScope computes the session scope of a key's request for a project, as the HTTP handler does.
func (e *persistentEnv) requestScope(p Principal, project string) ([]mcpScopeElement, bool, error) {
	view, done, scope, err := e.root.DB.scopedAgent(context.Background(), p.ID, p.MCPActorID, p.MCPDelegationID, project)
	if err != nil {
		return nil, false, err
	}
	defer done()
	_ = view
	return scope, true, nil
}

func persistentJSON(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("%v: %s", err, rec.Body)
	}
	return out
}

func (e *persistentEnv) expect(rec *httptest.ResponseRecorder, want int) {
	e.t.Helper()
	if rec.Code != want {
		e.t.Fatalf("want %d got %d: %s", want, rec.Code, rec.Body)
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		e.t.Fatalf("response must be no-store: %v", rec.Header())
	}
	if want >= 400 && strings.Contains(rec.Body.String(), mcpPersistentPrefix) {
		e.t.Fatalf("error leaks a token: %s", rec.Body)
	}
}

func (e *persistentEnv) mint(s *Server, body string, want int) map[string]any {
	e.t.Helper()
	rec := e.call(s, persistentCredentials, "POST", body, "")
	e.expect(rec, want)
	if want != http.StatusCreated {
		return nil
	}
	return persistentJSON(e.t, rec)
}

// mintToken mints a credential and returns its id and one-time token.
func (e *persistentEnv) mintToken(s *Server, body string) (string, string) {
	e.t.Helper()
	out := e.mint(s, body, http.StatusCreated)
	id, _ := out["id"].(string)
	token, _ := out["token"].(string)
	if id == "" || !strings.HasPrefix(token, mcpPersistentPrefix) {
		e.t.Fatalf("bad mint response: %v", out)
	}
	return id, token
}

func (e *persistentEnv) account(s *Server, name string) string {
	e.t.Helper()
	rec := e.call(s, persistentAccounts, "POST", `{"name":"`+name+`"}`, "")
	e.expect(rec, http.StatusCreated)
	id, _ := persistentJSON(e.t, rec)["id"].(string)
	if !strings.HasPrefix(id, "svc_") {
		e.t.Fatalf("bad service account id %q", id)
	}
	return id
}

func persistentAuthOn(a *AuthService, token string) (Principal, *httptest.ResponseRecorder, bool) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/mcp", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	p, ok := a.authenticatePersistent(rec, req, token)
	return p, rec, ok
}

func (e *persistentEnv) mustAuth(token string) Principal {
	e.t.Helper()
	p, rec, ok := persistentAuthOn(e.x.a, token)
	if !ok {
		e.t.Fatalf("authentication failed: %d %s", rec.Code, rec.Body)
	}
	return p
}

func (e *persistentEnv) mustFail(token string, want int) {
	e.t.Helper()
	e.mustFailOn(e.x.a, token, want)
}

func (e *persistentEnv) mustFailOn(a *AuthService, token string, want int) {
	e.t.Helper()
	p, rec, ok := persistentAuthOn(a, token)
	if ok || rec.Code != want {
		e.t.Fatalf("want %d, got ok=%v status=%d body=%s principal=%+v", want, ok, rec.Code, rec.Body, p)
	}
	if len(rec.Result().Cookies()) != 0 || rec.Header().Get("Cache-Control") != "no-store" {
		e.t.Fatal("bearer refusal must not set cookies and must be no-store")
	}
	if strings.Contains(rec.Body.String(), token) {
		e.t.Fatal("refusal leaks the token")
	}
	if want == http.StatusUnauthorized && rec.Header().Get("WWW-Authenticate") == "" {
		e.t.Fatal("401 must carry WWW-Authenticate")
	}
}

func persistentFullHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func persistentAuditDump(e *persistentEnv) string {
	e.t.Helper()
	return e.text(`SELECT coalesce(string_agg(actor_id||' | '||action||' | '||resource_type||' | '||resource_id||' | '||details::text,' ## '),'') FROM security_audit`)
}

// --- no database needed ---

func TestMCPPersistentDisabledFailsClosed(t *testing.T) {
	token := mcpPersistentPrefix + strings.Repeat("A", mcpPersistentSecretLen)
	for name, a := range map[string]*AuthService{
		"no feature":  {},
		"no database": {persistent: &mcpPersistentAccess{now: time.Now}},
	} {
		_, rec, ok := persistentAuthOn(a, token)
		if ok || rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s: want 503, got ok=%v %d %s", name, ok, rec.Code, rec.Body)
		}
		if strings.Contains(rec.Body.String(), token) {
			t.Fatalf("%s: leaks the token", name)
		}
	}
}

func TestMCPPersistentTokenShapeAndHash(t *testing.T) {
	prov := &persistentProbe{}
	// The database is never reached for malformed tokens.
	a := &AuthService{persistent: &mcpPersistentAccess{db: &database{}, now: time.Now, checkpoint: prov.checkpoint}}
	aaa := strings.Repeat("A", mcpPersistentSecretLen)
	for _, raw := range []string{
		mcpPersistentPrefix,
		mcpPersistentPrefix + aaa + "A",
		mcpPersistentPrefix + aaa[:mcpPersistentSecretLen-1],
		mcpPersistentPrefix + aaa[:mcpPersistentSecretLen-1] + "B", // non-canonical trailing bits
		mcpPersistentPrefix + aaa[:mcpPersistentSecretLen-1] + "+", // not url-safe base64
		mcpPersistentPrefix + aaa[:mcpPersistentSecretLen-1] + "=",
		mcpPersistentPrefix + aaa[:mcpPersistentSecretLen-1] + " ",
		"AIWIKI_PAT_" + aaa,
		"aiwiki_tmp_" + aaa,
		aaa,
	} {
		_, rec, ok := persistentAuthOn(a, raw)
		if ok || rec.Code != http.StatusUnauthorized || rec.Header().Get("WWW-Authenticate") == "" {
			t.Fatalf("%q: want 401, got ok=%v %d", raw, ok, rec.Code)
		}
	}
	if prov.count() != 0 {
		t.Fatal("malformed tokens must not get past the first check")
	}

	secret, err := authRandom(32)
	if err != nil {
		t.Fatal(err)
	}
	token := mcpPersistentPrefix + secret
	if !mcpPersistentWellFormed(token) || !mcpPersistentWellFormed(mcpPersistentPrefix+aaa) {
		t.Fatal("canonical tokens must be accepted")
	}
	if mcpPersistentHash(token) != persistentFullHash(token) || mcpPersistentHash(token) == persistentFullHash(secret) {
		t.Fatal("the hash must cover the full token including the prefix")
	}
}

func TestMCPPersistentStoredProjectsParsing(t *testing.T) {
	for _, raw := range []string{``, `null`, `[]`, `[1]`, `{"a":1}`, `"a"`, `["a"`, `["../x"]`, `[""]`, `["a","a"]`, `[{"type":"project","id":"a","mode":"read"}]`} {
		if p, ok := mcpPersistentProjects([]byte(raw)); ok {
			t.Fatalf("%q must fail closed, got %v", raw, p)
		}
	}
	ids := make([]string, mcpPersistentMaxProjects+1)
	for i := range ids {
		ids[i] = fmt.Sprintf("p%d", i)
	}
	raw, _ := json.Marshal(ids)
	if _, ok := mcpPersistentProjects(raw); ok {
		t.Fatal("too many projects must fail closed")
	}
	if p, ok := mcpPersistentProjects([]byte(`["pa","pb"]`)); !ok || len(p) != 2 {
		t.Fatalf("valid list rejected: %v %v", p, ok)
	}
}

// --- PostgreSQL-backed behaviour ---

func TestMCPPersistentMintListRevokeLifecycle(t *testing.T) {
	e := newPersistentEnv(t)
	alice, aliceID := e.user("alice")
	bob, bobID := e.user("bob")
	e.seed(aliceID, bobID)

	rec := e.call(alice, persistentCredentials, "GET", "", "")
	e.expect(rec, 200)
	if got := strings.ReplaceAll(strings.TrimSpace(rec.Body.String()), " ", ""); got != `{"credentials":[],"enabled":true}` {
		t.Fatalf("empty list: %s", got)
	}

	out := e.mint(alice, `{"name":"  agent  ","projects":["pb","pa"],"ttl_seconds":3600}`, http.StatusCreated)
	token, _ := out["token"].(string)
	id, _ := out["id"].(string)
	if !strings.HasPrefix(token, mcpPersistentPrefix) || len(token) != len(mcpPersistentPrefix)+mcpPersistentSecretLen || !mcpPersistentWellFormed(token) {
		t.Fatalf("bad token shape %q", token)
	}
	if !strings.HasPrefix(id, "pat_") || out["name"] != "agent" || out["mode"] != nil || out["scope"] != nil || out["read_only"] != false || len(out) != 7 {
		t.Fatalf("bad mint response: %v", out)
	}
	if sc, _ := json.Marshal(out["projects"]); string(sc) != `[{"available":true,"id":"pb","name":"pb","visibility":"private"},{"available":true,"id":"pa","name":"pa","visibility":"private"}]` {
		t.Fatalf("projects: %s", sc)
	}
	created, err1 := time.Parse(time.RFC3339Nano, out["created_at"].(string))
	expires, err2 := time.Parse(time.RFC3339Nano, out["expires_at"].(string))
	if err1 != nil || err2 != nil || expires.Sub(created) != time.Hour || !created.Equal(e.clk.now()) {
		t.Fatalf("bad times: %v %v %v", out["created_at"], out["expires_at"], err2)
	}

	// Only the hash of the full token is stored; the secret is nowhere else.
	secret := strings.TrimPrefix(token, mcpPersistentPrefix)
	if got := e.text(`SELECT secret_hash FROM mcp_credentials WHERE id=?`, id); got != persistentFullHash(token) {
		t.Fatalf("stored hash %q", got)
	}
	rowDump := e.text(`SELECT coalesce(string_agg(c::text,' ## '),'') FROM mcp_credentials c`)
	audit := persistentAuditDump(e)
	for _, leak := range []string{token, secret} {
		if strings.Contains(rowDump, leak) || strings.Contains(audit, leak) {
			t.Fatalf("secret stored in the clear")
		}
	}
	if strings.Contains(audit, persistentFullHash(token)) || !strings.Contains(audit, aliceID+" | mcp_credential.create | mcp_credential | "+id) {
		t.Fatalf("audit must record the mint without the hash: %s", audit)
	}

	// The token authenticates as an exact-project, non-admin sponsor principal.
	before := e.prov.count()
	p := e.mustAuth(token)
	if p.ID != aliceID || p.Issuer != e.iss || p.Subject != "alice" || p.Admin || p.MCPReadOnly || p.MCPScope != nil ||
		p.MCPDelegationID != id || p.MCPActorID != aliceID || !p.MCPExpiresAt.Equal(expires) ||
		len(p.MCPKeyProjects) != 2 || p.MCPKeyProjects[0] != "pb" || p.MCPKeyProjects[1] != "pa" {
		t.Fatalf("bad principal: %+v", p)
	}
	if e.prov.count() != before+1 {
		t.Fatalf("a valid request re-verifies the credential once: %d", e.prov.count()-before)
	}
	// The session scope is computed per request from the project, so the admitted principal
	// carries none yet; with a project named it has write elements.
	sc, ok, _ := e.requestScope(p, "pa")
	if !ok || !mcpScopeHasWrite(sc) || !mcpScopedTool(&Principal{MCPScope: sc}, "wiki_write") || mcpScopedTool(&Principal{MCPScope: sc}, "wiki_delete") {
		t.Fatalf("write credential tool scope: %v %v", sc, ok)
	}

	// Listing: own metadata only, never secrets, hashes or owners.
	rec = e.call(alice, persistentCredentials, "GET", "", "")
	e.expect(rec, 200)
	for _, leak := range []string{token, secret, persistentFullHash(token), aliceID, "secret", "token", "owner"} {
		if strings.Contains(rec.Body.String(), leak) {
			t.Fatalf("list leaks %q: %s", leak, rec.Body)
		}
	}
	var list struct {
		Enabled     bool             `json:"enabled"`
		Credentials []map[string]any `json:"credentials"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil || !list.Enabled || len(list.Credentials) != 1 || list.Credentials[0]["id"] != id {
		t.Fatalf("list: %v %s", err, rec.Body)
	}
	rec = e.call(bob, persistentCredentials, "GET", "", "")
	e.expect(rec, 200)
	if strings.Contains(rec.Body.String(), id) || !strings.Contains(rec.Body.String(), `"credentials":[]`) {
		t.Fatalf("other owner's list: %s", rec.Body)
	}

	// Another owner can neither see nor revoke it.
	e.expect(e.call(bob, persistentCredentialR, "DELETE", "", id), 404)
	e.expect(e.call(bob, persistentCredentialR, "DELETE", "", "../x"), 404)
	e.mustAuth(token)

	// Revocation is immediate, audited atomically, and never undone.
	e.expect(e.call(alice, persistentCredentialR, "DELETE", "", id), 204)
	e.mustFail(token, http.StatusUnauthorized)
	revokedAt := e.text(`SELECT revoked_at::text FROM mcp_credentials WHERE id=?`, id)
	e.clk.advance(time.Minute)
	e.expect(e.call(alice, persistentCredentialR, "DELETE", "", id), 204)
	if again := e.text(`SELECT revoked_at::text FROM mcp_credentials WHERE id=?`, id); again != revokedAt {
		t.Fatalf("revocation time changed: %s -> %s", revokedAt, again)
	}
	if n := e.count(`SELECT count(*) FROM security_audit WHERE action='mcp_credential.revoke' AND resource_id=?`, id); n != 1 {
		t.Fatalf("revoke audits: %d", n)
	}
	e.expect(e.call(alice, persistentCredentialR, "DELETE", "", "pat_missing"), 404)
	rec = e.call(alice, persistentCredentials, "GET", "", "")
	if !strings.Contains(rec.Body.String(), `"revoked_at"`) {
		t.Fatalf("revoked credentials stay listed: %s", rec.Body)
	}
	for _, m := range []string{"PUT", "POST", "GET"} {
		e.expect(e.call(alice, persistentCredentialR, m, "", id), 405)
	}
	// PATCH edits projects/read_only of a live key; this one is revoked.
	e.expect(e.call(alice, persistentCredentialR, "PATCH", `{"projects":["pa"],"read_only":true}`, id), 404)
	for _, m := range []string{"PUT", "DELETE", "PATCH"} {
		e.expect(e.call(alice, persistentCredentials, m, "", ""), 405)
	}
}

func TestMCPPersistentMintValidation(t *testing.T) {
	e := newPersistentEnv(t)
	alice, aliceID := e.user("alice")
	_, bobID := e.user("bob")
	e.seed(aliceID, bobID)

	many := make([]string, mcpPersistentMaxProjects+1)
	for i := range many {
		many[i] = fmt.Sprintf(`"p%d"`, i)
	}
	for _, body := range []string{
		`{"name":"","projects":["pa"]}`,
		`{"name":"   ","projects":["pa"]}`,
		`{"name":"` + strings.Repeat("x", 81) + `","projects":["pa"]}`,
		`{"name":"n","projects":[]}`,
		`{"name":"n"}`,
		`{"name":"n","projects":[` + strings.Join(many, ",") + `]}`,
		`{"name":"n","projects":["pa","pa"]}`,
		`{"name":"n","projects":["../x"]}`,
		`{"name":"n","projects":[""]}`,
		`{"name":"n","projects":["pa"],"mode":"write"}`, // removed fields
		`{"name":"n","projects":["pa"],"scope":[]}`,
		`{"name":"n","project_ids":["pa"]}`,
		`{"name":"n","projects":["pa"],"read_only":"yes"}`,
		`{"name":"n","projects":["pa"],"ttl_seconds":59}`,
		`{"name":"n","projects":["pa"],"ttl_seconds":-1}`,
		`{"name":"n","projects":["pa"],"ttl_seconds":15552001}`, // 180 days + 1s
		`{"name":"n","projects":["pa"],"ttl_seconds":1.5}`,
		`{"name":"n","projects":["pa"],"admin":true}`,
		`{"name":"n","projects":["pa"],"owner_id":"someone"}`,
		`{"name":"n","projects":["pa"],"service_account_id":"a/b"}`,
		`{"name":"n","projects":["pa"]} {}`,
		`not json`,
		``,
	} {
		e.mint(alice, body, http.StatusBadRequest)
	}
	if n := e.count(`SELECT count(*) FROM mcp_credentials`); n != 0 {
		t.Fatalf("rejected requests stored %d credentials", n)
	}
	if n := e.count(`SELECT count(*) FROM security_audit WHERE action LIKE 'mcp_credential.%'`); n != 0 {
		t.Fatalf("rejected requests were audited as created: %d", n)
	}

	span := func(out map[string]any) time.Duration {
		c, _ := time.Parse(time.RFC3339Nano, out["created_at"].(string))
		x, _ := time.Parse(time.RFC3339Nano, out["expires_at"].(string))
		return x.Sub(c)
	}
	out := e.mint(alice, `{"name":"defaults","projects":["pa"]}`, http.StatusCreated)
	if out["read_only"] != false || span(out) != 30*24*time.Hour {
		t.Fatalf("defaults: %v", out)
	}
	if out := e.mint(alice, `{"name":"min","projects":["pa"],"read_only":true,"ttl_seconds":60}`, http.StatusCreated); span(out) != time.Minute {
		t.Fatalf("min ttl: %v", out)
	}
	if out := e.mint(alice, `{"name":"max","projects":["pa"],"ttl_seconds":15552000}`, http.StatusCreated); span(out) != 180*24*time.Hour {
		t.Fatalf("max ttl: %v", out)
	}

	// MCP credentials can never manage credentials.
	for name, pr := range map[string]*Principal{
		"scoped":     {ID: aliceID, Issuer: e.iss, Subject: "alice", MCPScope: spaceScope("read", "sa")},
		"keyed":      {ID: aliceID, Issuer: e.iss, Subject: "alice", MCPKeyProjects: []string{"pa"}},
		"credential": {ID: aliceID, Issuer: e.iss, Subject: "alice", MCPDelegationID: "pat_x"},
		"actor":      {ID: aliceID, Issuer: e.iss, Subject: "alice", MCPActorID: "svc_x"},
	} {
		cp := *alice
		cp.principal = pr
		e.expect(e.call(&cp, persistentCredentials, "POST", `{"name":"n","projects":["pa"]}`, ""), 403)
		e.expect(e.call(&cp, persistentCredentials, "GET", "", ""), 403)
		e.expect(e.call(&cp, persistentAccounts, "POST", `{"name":"n"}`, ""), 403)
		e.expect(e.call(&cp, persistentAccountR, "DELETE", "", "svc_x"), 403)
		e.expect(e.call(&cp, persistentCredentialR, "DELETE", "", "pat_x"), 403)
		_ = name
	}
	if n := e.count(`SELECT count(*) FROM mcp_credentials`); n != 3 {
		t.Fatalf("expected the three valid credentials, got %d", n)
	}
}

func TestMCPPersistentMintProjectPermissions(t *testing.T) {
	e := newPersistentEnv(t)
	alice, aliceID := e.user("alice")
	bob, bobID := e.user("bob")
	e.seed(aliceID, bobID)

	// Own projects and other people's public projects are allowed; a project grants nothing by itself.
	e.mint(alice, `{"name":"n","projects":["pr"]}`, http.StatusCreated)
	e.mint(alice, `{"name":"n","projects":["pe","pub"],"read_only":true}`, http.StatusCreated)
	e.mint(bob, `{"name":"n","projects":["pbob"]}`, http.StatusCreated)
	allowed := e.count(`SELECT count(*) FROM mcp_credentials`)
	audited := e.count(`SELECT count(*) FROM security_audit`)

	for _, body := range []string{
		`{"name":"n","projects":["pn"]}`,      // someone else's private project
		`{"name":"n","projects":["pa","pn"]}`, // one hidden project spoils the set
		`{"name":"n","projects":["missing"]}`, // unknown
		`{"name":"n","projects":["sa"]}`,      // a space is not a project
		`{"name":"n","projects":["pbob"]}`,    // bob's private project
	} {
		e.mint(alice, body, http.StatusBadRequest)
	}
	e.mint(bob, `{"name":"n","projects":["pa"]}`, http.StatusBadRequest) // alice's private project
	if e.count(`SELECT count(*) FROM mcp_credentials`) != allowed || e.count(`SELECT count(*) FROM security_audit`) != audited {
		t.Fatal("forbidden mints must neither store nor audit anything")
	}

	// The key may list a project whose spaces the owner cannot read: it simply yields no rights.
	e.mint(alice, `{"name":"n","projects":["pub"]}`, http.StatusCreated)
	// Visibility is checked at mint time against the live data.
	e.seedExec(`DELETE FROM agent_projects WHERE id='pe'`)
	e.mint(alice, `{"name":"n","projects":["pe"]}`, http.StatusBadRequest)
}

func TestMCPPersistentMintCaps(t *testing.T) {
	e := newPersistentEnv(t)
	alice, aliceID := e.user("alice")
	_, bobID := e.user("bob")
	e.seed(aliceID, bobID)
	body := `{"name":"n","projects":["pub"]}`

	// Active cap per owner; revoking frees a slot, other owners are unaffected.
	carol, carolID := e.user("carol")
	ids := make([]string, 0, mcpPersistentActiveCap)
	for i := 0; i < mcpPersistentActiveCap; i++ {
		id, _ := e.mintToken(carol, body)
		ids = append(ids, id)
	}
	e.mint(carol, body, http.StatusTooManyRequests)
	e.mint(alice, body, http.StatusCreated)
	e.expect(e.call(carol, persistentCredentialR, "DELETE", "", ids[0]), 204)
	e.mint(carol, body, http.StatusCreated)
	e.mint(carol, body, http.StatusTooManyRequests)

	// Expired credentials stop counting as active but still count in the total.
	// (Keep carol inside the login window: it is unrelated to the caps.)
	e.clk.advance(mcpPersistentDefaultTTL + 24*time.Hour)
	e.exec(`UPDATE users SET last_login_at=? WHERE id=?`, e.clk.now(), carolID)
	e.mint(carol, body, http.StatusCreated)
	if n := e.count(`SELECT count(*) FROM mcp_credentials WHERE owner_id=?`, carolID); n != mcpPersistentActiveCap+2 {
		t.Fatalf("carol rows: %d", n)
	}

	// Total cap counts revoked and expired credentials too.
	dave, daveID := e.user("dave")
	e.exec(`INSERT INTO mcp_credentials(id,owner_id,name,secret_hash,agent_project_ids,created_at,expires_at,revoked_at)
SELECT 'pat_bulk'||g::text,?::text,'old',md5(?::text||g::text)||md5(g::text||?::text),'["pub"]'::jsonb,now(),now()+interval '1 day',now()
FROM generate_series(1,?) g`, daveID, daveID, daveID, mcpPersistentTotalCap)
	e.mint(dave, body, http.StatusTooManyRequests)
	e.exec(`DELETE FROM mcp_credentials WHERE id='pat_bulk1'`)
	e.mint(dave, body, http.StatusCreated)
	e.mint(dave, body, http.StatusTooManyRequests)

	// Concurrent mints obey the cap exactly.
	eve, eveID := e.user("eve")
	var mu sync.Mutex
	codes := map[int]int{}
	var wg sync.WaitGroup
	for i := 0; i < mcpPersistentActiveCap+10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec := e.call(eve, persistentCredentials, "POST", body, "")
			mu.Lock()
			codes[rec.Code]++
			mu.Unlock()
		}()
	}
	wg.Wait()
	if codes[201] != mcpPersistentActiveCap || codes[429] != 10 || len(codes) != 2 {
		t.Fatalf("concurrent mint outcomes: %v", codes)
	}
	if n := e.count(`SELECT count(*) FROM mcp_credentials WHERE owner_id=?`, eveID); n != mcpPersistentActiveCap {
		t.Fatalf("eve rows: %d", n)
	}
}

func TestMCPPersistentServiceAccountLifecycle(t *testing.T) {
	e := newPersistentEnv(t)
	alice, aliceID := e.user("alice")
	bob, bobID := e.user("bob")
	e.seed(aliceID, bobID)

	for _, body := range []string{`{"name":""}`, `{"name":"` + strings.Repeat("x", 81) + `"}`, `{"name":"n","admin":true}`, `{}`, `x`} {
		e.expect(e.call(alice, persistentAccounts, "POST", body, ""), 400)
	}
	rec := e.call(alice, persistentAccounts, "GET", "", "")
	e.expect(rec, 200)
	if got := strings.ReplaceAll(strings.TrimSpace(rec.Body.String()), " ", ""); got != `{"enabled":true,"service_accounts":[]}` {
		t.Fatalf("empty list: %s", got)
	}

	rec = e.call(alice, persistentAccounts, "POST", `{"name":"  ci bot "}`, "")
	e.expect(rec, 201)
	acct := persistentJSON(t, rec)
	svc, _ := acct["id"].(string)
	if !strings.HasPrefix(svc, "svc_") || acct["name"] != "ci bot" || acct["sponsor_id"] != aliceID || acct["disabled"] != false {
		t.Fatalf("bad account: %v", acct)
	}
	if !strings.Contains(persistentAuditDump(e), aliceID+" | mcp_service_account.create | mcp_service_account | "+svc) {
		t.Fatal("account creation must be audited")
	}

	// Strictly owner-scoped.
	rec = e.call(bob, persistentAccounts, "GET", "", "")
	if strings.Contains(rec.Body.String(), svc) {
		t.Fatalf("other owner sees the account: %s", rec.Body)
	}
	e.expect(e.call(bob, persistentAccountR, "DELETE", "", svc), 404)
	e.mint(bob, `{"name":"n","projects":["pbob"],"service_account_id":"`+svc+`"}`, http.StatusForbidden)
	e.mint(alice, `{"name":"n","projects":["pa"],"service_account_id":"svc_missing"}`, http.StatusForbidden)

	// Service-account credentials are attributed to the account, authorised as the sponsor.
	svcCred, svcToken := e.mintToken(alice, `{"name":"svc","projects":["pa"],"service_account_id":"`+svc+`"}`)
	ownCred, ownToken := e.mintToken(alice, `{"name":"own","projects":["pa"]}`)
	p := e.mustAuth(svcToken)
	if p.ID != aliceID || p.MCPActorID != svc || p.MCPDelegationID != svcCred || p.Admin {
		t.Fatalf("service principal: %+v", p)
	}
	if p := e.mustAuth(ownToken); p.MCPActorID != aliceID || p.ID != aliceID {
		t.Fatalf("owner principal: %+v", p)
	}
	rec = e.call(alice, persistentCredentials, "GET", "", "")
	if !strings.Contains(rec.Body.String(), `"service_account_id":"`+svc+`"`) {
		t.Fatalf("list shows the service account: %s", rec.Body)
	}

	// Disabling revokes every credential of the account, only that account's.
	e.expect(e.call(alice, persistentAccountR, "DELETE", "", svc), 204)
	e.mustFail(svcToken, http.StatusUnauthorized)
	e.mustAuth(ownToken)
	if e.text(`SELECT coalesce(revoked_at::text,'') FROM mcp_credentials WHERE id=?`, svcCred) == "" ||
		e.text(`SELECT coalesce(revoked_at::text,'') FROM mcp_credentials WHERE id=?`, ownCred) != "" {
		t.Fatal("only the service account's credentials may be revoked")
	}
	audit := persistentAuditDump(e)
	for _, want := range []string{
		aliceID + " | mcp_credential.revoke | mcp_credential | " + svcCred,
		aliceID + " | mcp_service_account.disable | mcp_service_account | " + svc,
	} {
		if !strings.Contains(audit, want) {
			t.Fatalf("missing audit %q in %s", want, audit)
		}
	}
	if strings.Contains(audit, svcToken) || strings.Contains(audit, persistentFullHash(svcToken)) {
		t.Fatal("audit leaks the credential")
	}
	rec = e.call(alice, persistentAccounts, "GET", "", "")
	if !strings.Contains(rec.Body.String(), `"disabled":true`) {
		t.Fatalf("disabled account stays listed: %s", rec.Body)
	}

	// Permanent: cannot mint for it, repeat is a no-op, nothing re-enables it.
	e.mint(alice, `{"name":"n","projects":["pa"],"service_account_id":"`+svc+`"}`, http.StatusForbidden)
	e.expect(e.call(alice, persistentAccountR, "DELETE", "", svc), 204)
	if n := e.count(`SELECT count(*) FROM security_audit WHERE action='mcp_service_account.disable'`); n != 1 {
		t.Fatalf("repeat disable audits: %d", n)
	}
	for _, m := range []string{"PUT", "POST", "GET"} { // PATCH renames
		e.expect(e.call(alice, persistentAccountR, m, "", svc), 405)
	}
	for _, m := range []string{"PUT", "DELETE", "PATCH"} {
		e.expect(e.call(alice, persistentAccounts, m, "", ""), 405)
	}
	if e.count(`SELECT disabled FROM mcp_service_accounts WHERE id=?`, svc) != 1 {
		t.Fatal("account must stay disabled")
	}

	// Cap per owner; disabled accounts still count.
	frank, _ := e.user("frank")
	first := e.account(frank, "a0")
	for i := 1; i < mcpPersistentAccountCap; i++ {
		e.account(frank, fmt.Sprintf("a%d", i))
	}
	e.expect(e.call(frank, persistentAccounts, "POST", `{"name":"over"}`, ""), 429)
	e.expect(e.call(frank, persistentAccountR, "DELETE", "", first), 204)
	e.expect(e.call(frank, persistentAccounts, "POST", `{"name":"over"}`, ""), 429)
}

func TestMCPPersistentAuthenticateStates(t *testing.T) {
	e := newPersistentEnv(t)
	alice, aliceID := e.user("alice")
	_, bobID := e.user("bob")
	e.seed(aliceID, bobID)
	body := `{"name":"n","projects":["pa"],"ttl_seconds":600}`

	// Unknown but well-formed.
	secret, _ := authRandom(32)
	e.mustFail(mcpPersistentPrefix+secret, http.StatusUnauthorized)

	// Expiry: exactly at expires_at is already dead.
	_, token := e.mintToken(alice, body)
	e.clk.advance(600*time.Second - time.Second)
	e.mustAuth(token)
	e.clk.advance(time.Second)
	e.mustFail(token, http.StatusUnauthorized)
	e.clk.advance(time.Hour)
	e.mustFail(token, http.StatusUnauthorized)

	// A one-character change is a different (unknown) token.
	id, token := e.mintToken(alice, body)
	flipped := token[:len(token)-2] + "AA"
	if flipped == token {
		flipped = token[:len(token)-2] + "BA"
	}
	e.mustFail(flipped, http.StatusUnauthorized)
	e.mustFail(strings.TrimPrefix(token, mcpPersistentPrefix), http.StatusUnauthorized)

	// Revoked in the database by any path.
	e.exec(`UPDATE mcp_credentials SET revoked_at=? WHERE id=?`, e.clk.now(), id)
	e.mustFail(token, http.StatusUnauthorized)

	// Owner disabled locally (takes effect on the next request).
	id, token = e.mintToken(alice, body)
	e.exec(`UPDATE users SET disabled=1 WHERE id=?`, aliceID)
	e.mustFail(token, http.StatusForbidden)
	e.exec(`UPDATE users SET disabled=0 WHERE id=?`, aliceID)
	e.mustAuth(token)

	// Identity must be the immutable (issuer, subject) the owner id derives from.
	e.exec(`UPDATE users SET issuer='https://other.example' WHERE id=?`, aliceID)
	e.mustFail(token, http.StatusForbidden)
	e.exec(`UPDATE users SET issuer=? WHERE id=?`, e.iss, aliceID)
	e.exec(`UPDATE users SET subject='mallory' WHERE id=?`, aliceID)
	e.mustFail(token, http.StatusForbidden)
	e.exec(`UPDATE users SET subject='alice' WHERE id=?`, aliceID)
	e.mustAuth(token)

	// Invalid stored projects fail closed.
	// Migration-owner corruption simulation; runtime cannot update authority.
	e.exec(`ALTER TABLE mcp_credentials DISABLE TRIGGER mcp_credential_immutable`)
	defer e.exec(`ALTER TABLE mcp_credentials ENABLE TRIGGER mcp_credential_immutable`)
	for _, projects := range []string{`["../x"]`, `["pa","pa"]`, `[]`, `[1]`} {
		e.exec(`ALTER TABLE mcp_credentials DROP CONSTRAINT IF EXISTS mcp_credentials_agent_project_ids_check`)
		e.exec(`UPDATE mcp_credentials SET agent_project_ids=?::jsonb WHERE id=?`, projects, id)
		e.mustFail(token, http.StatusUnauthorized)
	}
	e.exec(`UPDATE mcp_credentials SET agent_project_ids='["pa"]' WHERE id=?`, id)
	e.exec(`ALTER TABLE mcp_credentials ENABLE TRIGGER mcp_credential_immutable`)
	e.mustAuth(token)

	// Service accounts: disabled, or no longer owned by the credential's owner.
	svc := e.account(alice, "bot")
	_, svcToken := e.mintToken(alice, `{"name":"n","projects":["pa"],"service_account_id":"`+svc+`"}`)
	e.mustAuth(svcToken)
	e.exec(`ALTER TABLE mcp_service_accounts DISABLE TRIGGER mcp_service_immutable`)
	defer e.exec(`ALTER TABLE mcp_service_accounts ENABLE TRIGGER mcp_service_immutable`)
	e.exec(`UPDATE mcp_service_accounts SET owner_id=? WHERE id=?`, bobID, svc)
	e.mustFail(svcToken, http.StatusForbidden)
	e.exec(`UPDATE mcp_service_accounts SET owner_id=? WHERE id=?`, aliceID, svc)
	e.exec(`ALTER TABLE mcp_service_accounts ENABLE TRIGGER mcp_service_immutable`)
	e.mustAuth(svcToken)
	e.exec(`UPDATE mcp_service_accounts SET disabled=1 WHERE id=?`, svc)
	e.mustFail(svcToken, http.StatusForbidden)
}

func TestMCPPersistentLoginWindow(t *testing.T) {
	e := newPersistentEnv(t)
	alice, aliceID := e.user("alice")
	_, bobID := e.user("bob")
	e.seed(aliceID, bobID)
	id, token := e.mintToken(alice, `{"name":"n","projects":["pa"],"ttl_seconds":15552000}`)

	// Evaluated on every request, from the database only.
	base := e.prov.count()
	for i := 1; i <= 3; i++ {
		e.mustAuth(token)
		if e.prov.count() != base+i {
			t.Fatalf("every request must be re-verified: %d", e.prov.count()-base)
		}
	}

	// Exactly at the end of the window the owner is no longer admitted.
	e.clk.advance(mcpPersistentLoginWindow - time.Second)
	e.mustAuth(token)
	e.clk.advance(time.Second)
	e.mustFail(token, http.StatusForbidden)

	// A new interactive login re-admits the same credential; nothing was revoked.
	e.exec(`UPDATE users SET last_login_at=? WHERE id=?`, e.clk.now(), aliceID)
	e.mustAuth(token)
	if e.text(`SELECT coalesce(revoked_at::text,'') FROM mcp_credentials WHERE id=?`, id) != "" {
		t.Fatal("an expired login window must not alter the credential")
	}

	// Never logged in (e.g. before the column existed) is not admitted.
	e.exec(`UPDATE users SET last_login_at=NULL WHERE id=?`, aliceID)
	e.mustFail(token, http.StatusForbidden)

	// A login time in the future (clock skew) still counts as recent.
	e.exec(`UPDATE users SET last_login_at=? WHERE id=?`, e.clk.now().Add(time.Hour), aliceID)
	e.mustAuth(token)
}

func TestMCPPersistentLoginWindowGovernsUseNotManagement(t *testing.T) {
	e := newPersistentEnv(t)
	alice, aliceID := e.user("alice")
	_, bobID := e.user("bob")
	e.seed(aliceID, bobID)
	body := `{"name":"n","projects":["pa"]}`

	credA, tokenA := e.mintToken(alice, body)
	credB, _ := e.mintToken(alice, body)
	svcA := e.account(alice, "a")
	svcB := e.account(alice, "b")
	_, svcTokenA := e.mintToken(alice, `{"name":"n","projects":["pa"],"service_account_id":"`+svcA+`"}`)

	// Listing, revocation and disabling never depend on the login window.
	e.clk.advance(mcpPersistentLoginWindow + time.Hour)
	e.mustFail(tokenA, http.StatusForbidden)
	e.expect(e.call(alice, persistentCredentials, "GET", "", ""), 200)
	e.expect(e.call(alice, persistentAccounts, "GET", "", ""), 200)
	e.expect(e.call(alice, persistentCredentialR, "DELETE", "", credA), 204)
	e.expect(e.call(alice, persistentAccountR, "DELETE", "", svcA), 204)
	e.exec(`UPDATE users SET last_login_at=? WHERE id=?`, e.clk.now(), aliceID)
	e.mustFail(tokenA, http.StatusUnauthorized)
	e.mustFail(svcTokenA, http.StatusUnauthorized)

	// Feature switched off (no database): management reads and revocation still
	// work, creation and authentication are refused.
	live := e.mintTokenFor(alice, body)
	for name, off := range map[string]*mcpPersistentAccess{
		"nil":         nil,
		"no database": {now: e.clk.now},
	} {
		saved := e.x.a.persistent
		e.x.a.persistent = off
		e.mint(alice, body, http.StatusServiceUnavailable)
		e.expect(e.call(alice, persistentAccounts, "POST", `{"name":"x"}`, ""), 503)
		rec := e.call(alice, persistentCredentials, "GET", "", "")
		e.expect(rec, 200)
		if !strings.Contains(rec.Body.String(), `"enabled":false`) || !strings.Contains(rec.Body.String(), credB) {
			t.Fatalf("%s: list while disabled: %s", name, rec.Body)
		}
		rec = e.call(alice, persistentAccounts, "GET", "", "")
		e.expect(rec, 200)
		if !strings.Contains(rec.Body.String(), `"enabled":false`) || !strings.Contains(rec.Body.String(), svcB) {
			t.Fatalf("%s: accounts while disabled: %s", name, rec.Body)
		}
		e.mustFail(live, http.StatusServiceUnavailable)
		e.x.a.persistent = saved
	}
	e.mustAuth(live)
	e.x.a.persistent = nil
	e.expect(e.call(alice, persistentAccountR, "DELETE", "", svcB), 204)
	e.expect(e.call(alice, persistentCredentialR, "DELETE", "", credB), 204)
	e.x.a.persistent = e.access()
	e.mustAuth(live) // revoking credB and disabling svcB touched nothing else
}

// mintTokenFor mints a default credential and returns only its token.
func (e *persistentEnv) mintTokenFor(s *Server, body string) string {
	e.t.Helper()
	_, token := e.mintToken(s, body)
	return token
}

func TestMCPPersistentMintRequiresLiveIdentity(t *testing.T) {
	e := newPersistentEnv(t)
	alice, aliceID := e.user("alice")
	_, bobID := e.user("bob")
	e.seed(aliceID, bobID)
	body := `{"name":"n","projects":["pa"]}`

	for name, pr := range map[string]*Principal{
		"subject differs from the id": {ID: aliceID, Issuer: e.iss, Subject: "mallory"},
		"foreign issuer":              {ID: aliceID, Issuer: "https://evil.example", Subject: "alice"},
		"no issuer":                   {ID: aliceID, Subject: "alice"},
		"no subject":                  {ID: aliceID, Issuer: e.iss},
		"unknown local user":          {ID: authUserID(e.iss, "ghost"), Issuer: e.iss, Subject: "ghost"},
	} {
		cp := *alice
		cp.principal = pr
		cp.DB = alice.DB
		rec := e.call(&cp, persistentCredentials, "POST", body, "")
		if rec.Code != 403 {
			t.Fatalf("%s: %d %s", name, rec.Code, rec.Body)
		}
		rec = e.call(&cp, persistentAccounts, "POST", `{"name":"n"}`, "")
		if rec.Code != 403 {
			t.Fatalf("%s: account %d %s", name, rec.Code, rec.Body)
		}
	}
	// A locally disabled owner can do nothing, including listing and revoking.
	id, token := e.mintToken(alice, body)
	e.exec(`UPDATE users SET disabled=1 WHERE id=?`, aliceID)
	e.expect(e.call(alice, persistentCredentials, "POST", body, ""), 403)
	e.expect(e.call(alice, persistentCredentials, "GET", "", ""), 403)
	e.expect(e.call(alice, persistentAccounts, "POST", `{"name":"n"}`, ""), 403)
	e.expect(e.call(alice, persistentCredentialR, "DELETE", "", id), 403)
	e.mustFail(token, http.StatusForbidden)
}

func TestMCPPersistentAuditFailureRollsBack(t *testing.T) {
	e := newPersistentEnv(t)
	alice, aliceID := e.user("alice")
	_, bobID := e.user("bob")
	e.seed(aliceID, bobID)
	body := `{"name":"n","projects":["pa"]}`

	svc := e.account(alice, "bot")
	id, token := e.mintToken(alice, body)
	svcCred, svcToken := e.mintToken(alice, `{"name":"n","projects":["pa"],"service_account_id":"`+svc+`"}`)
	rows := e.count(`SELECT count(*) FROM mcp_credentials`)
	accounts := e.count(`SELECT count(*) FROM mcp_service_accounts`)

	e.exec(`REVOKE INSERT ON security_audit FROM aiwiki_app`)
	restored := false
	restore := func() {
		if !restored {
			restored = true
			e.exec(`GRANT INSERT ON security_audit TO aiwiki_app`)
		}
	}
	defer restore()

	rec := e.call(alice, persistentCredentials, "POST", body, "")
	e.expect(rec, 503)
	e.expect(e.call(alice, persistentAccounts, "POST", `{"name":"x"}`, ""), 503)
	e.expect(e.call(alice, persistentCredentialR, "DELETE", "", id), 503)
	e.expect(e.call(alice, persistentAccountR, "DELETE", "", svc), 503)
	restore()

	if e.count(`SELECT count(*) FROM mcp_credentials`) != rows || e.count(`SELECT count(*) FROM mcp_service_accounts`) != accounts {
		t.Fatal("an unaudited mint must not leave a credential or account behind")
	}
	// Nothing was revoked or disabled either: the mutation and its audit are one unit.
	e.mustAuth(token)
	e.mustAuth(svcToken)
	if e.count(`SELECT disabled FROM mcp_service_accounts WHERE id=?`, svc) != 0 ||
		e.text(`SELECT coalesce(revoked_at::text,'') FROM mcp_credentials WHERE id=?`, svcCred) != "" {
		t.Fatal("unaudited disable must roll back")
	}
}

func TestMCPPersistentRateLimit(t *testing.T) {
	e := newPersistentEnv(t)
	alice, aliceID := e.user("alice")
	_, bobID := e.user("bob")
	e.seed(aliceID, bobID)
	body := `{"name":"n","projects":["pa"]}`
	idA, tokenA := e.mintToken(alice, body)
	_, tokenB := e.mintToken(alice, body)

	asked := e.prov.count()
	for i := 0; i < mcpPersistentRateLimit; i++ {
		e.mustAuth(tokenA)
	}
	if e.prov.count()-asked != mcpPersistentRateLimit {
		t.Fatalf("verified requests: %d", e.prov.count()-asked)
	}
	_, rec, ok := persistentAuthOn(e.x.a, tokenA)
	if ok || rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") != "60" {
		t.Fatalf("over the limit: ok=%v %d %v", ok, rec.Code, rec.Header())
	}
	e.mustFail(tokenA, http.StatusTooManyRequests)
	if e.prov.count()-asked != mcpPersistentRateLimit {
		t.Fatal("rate limited requests must stop before the final re-check")
	}

	// Limits are per credential and per minute (window boundary inclusive).
	e.mustAuth(tokenB)
	e.clk.advance(59 * time.Second)
	e.mustFail(tokenA, http.StatusTooManyRequests)
	e.clk.advance(time.Second)
	for i := 0; i < mcpPersistentRateLimit; i++ {
		e.mustAuth(tokenA)
	}
	e.mustFail(tokenA, http.StatusTooManyRequests)

	// A revoked credential is refused as such, not as rate limited.
	e.expect(e.call(alice, persistentCredentialR, "DELETE", "", idA), 204)
	e.mustFail(tokenA, http.StatusUnauthorized)
	if n := e.count(`SELECT rate_requests FROM mcp_credentials WHERE secret_hash=?`, persistentFullHash(tokenB)); n != 1 {
		t.Fatalf("other credential's counter: %d", n)
	}
}

func TestMCPPersistentRateLimitIsAtomicUnderConcurrency(t *testing.T) {
	e := newPersistentEnv(t)
	alice, aliceID := e.user("alice")
	_, bobID := e.user("bob")
	e.seed(aliceID, bobID)
	_, token := e.mintToken(alice, `{"name":"n","projects":["pa"]}`)

	asked := e.prov.count()
	var mu sync.Mutex
	codes := map[int]int{}
	var wg sync.WaitGroup
	for i := 0; i < mcpPersistentRateLimit+30; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, rec, ok := persistentAuthOn(e.x.a, token)
			mu.Lock()
			if ok {
				codes[200]++
			} else {
				codes[rec.Code]++
			}
			mu.Unlock()
		}()
	}
	wg.Wait()
	if codes[200] != mcpPersistentRateLimit || codes[429] != 30 || len(codes) != 2 {
		t.Fatalf("outcomes: %v", codes)
	}
	if e.prov.count()-asked != mcpPersistentRateLimit {
		t.Fatalf("verified requests %d", e.prov.count()-asked)
	}
}

func TestMCPPersistentChangesBetweenChecksAreHonoured(t *testing.T) {
	e := newPersistentEnv(t)
	alice, aliceID := e.user("alice")
	_, bobID := e.user("bob")
	e.seed(aliceID, bobID)
	body := `{"name":"n","projects":["pa"],"ttl_seconds":600}`
	svc := e.account(alice, "bot")
	defer e.prov.setHook(nil)

	// Revoked between the first check and the final re-read.
	id, token := e.mintToken(alice, body)
	e.prov.setHook(func() { e.exec(`UPDATE mcp_credentials SET revoked_at=? WHERE id=?`, e.clk.now(), id) })
	e.mustFail(token, http.StatusUnauthorized)

	// Owner disabled meanwhile.
	_, token = e.mintToken(alice, body)
	e.prov.setHook(func() { e.exec(`UPDATE users SET disabled=1 WHERE id=?`, aliceID) })
	e.mustFail(token, http.StatusForbidden)
	e.prov.setHook(nil)
	e.exec(`UPDATE users SET disabled=0 WHERE id=?`, aliceID)
	e.mustAuth(token)

	// Service account disabled meanwhile.
	_, svcToken := e.mintToken(alice, `{"name":"n","projects":["pa"],"service_account_id":"`+svc+`"}`)
	e.prov.setHook(func() { e.exec(`UPDATE mcp_service_accounts SET disabled=1 WHERE id=?`, svc) })
	e.mustFail(svcToken, http.StatusForbidden)
	e.prov.setHook(nil)

	// Expired meanwhile.
	_, token = e.mintToken(alice, body)
	e.prov.setHook(func() { e.clk.advance(time.Hour) })
	e.mustFail(token, http.StatusUnauthorized)
	e.prov.setHook(nil)

	// Login window elapsed meanwhile.
	_, token = e.mintToken(alice, `{"name":"n","projects":["pa"],"ttl_seconds":15552000}`)
	e.exec(`UPDATE users SET last_login_at=? WHERE id=?`, e.clk.now(), aliceID)
	e.prov.setHook(func() { e.clk.advance(mcpPersistentLoginWindow) })
	e.mustFail(token, http.StatusForbidden)
}

func TestMCPPersistentSurvivesRestartWithoutCache(t *testing.T) {
	e := newPersistentEnv(t)
	alice, aliceID := e.user("alice")
	_, bobID := e.user("bob")
	e.seed(aliceID, bobID)
	id, token := e.mintToken(alice, `{"name":"n","projects":["pa","pb"]}`)
	_, other := e.mintToken(alice, `{"name":"n","projects":["pa"]}`)
	before := e.mustAuth(token)

	// A brand new service over the same database knows nothing but the store.
	restarted, err := newAuthServiceWithUsers(context.Background(), authSQLUsers{db: e.root.DB}, e.x.a.cfg)
	if err != nil {
		t.Fatal(err)
	}
	if restarted == e.x.a {
		t.Fatal("restart must use a new AuthService")
	}
	if restarted.persistent == nil || restarted.persistent.db != e.root.DB {
		t.Fatal("a new service enables persistent credentials from its database")
	}
	restarted.persistent = e.access()
	p, rec, ok := persistentAuthOn(restarted, token)
	if !ok {
		t.Fatalf("token must survive a restart: %d %s", rec.Code, rec.Body)
	}
	if p.ID != before.ID || p.MCPDelegationID != before.MCPDelegationID || len(p.MCPKeyProjects) != 2 || !p.MCPExpiresAt.Equal(before.MCPExpiresAt) {
		t.Fatalf("restarted principal differs: %+v vs %+v", p, before)
	}

	// Revocation through one instance is visible to the other at once.
	e.expect(e.call(alice, persistentCredentialR, "DELETE", "", id), 204)
	e.mustFailOn(restarted, token, http.StatusUnauthorized)
	e.mustFail(token, http.StatusUnauthorized)
	if _, _, ok := persistentAuthOn(restarted, other); !ok {
		t.Fatal("unrelated credential must keep working")
	}

	// A restarted service without a database refuses everything.
	restarted.persistent = nil
	e.mustFailOn(restarted, other, http.StatusServiceUnavailable)
}

func TestMCPPersistentModeMapsToToolScope(t *testing.T) {
	e := newPersistentEnv(t)
	alice, aliceID := e.user("alice")
	_, bobID := e.user("bob")
	e.seed(aliceID, bobID)
	_, readToken := e.mintToken(alice, `{"name":"r","projects":["pe"],"read_only":true}`)
	_, writeToken := e.mintToken(alice, `{"name":"w","projects":["pe"]}`)

	r, w := e.mustAuth(readToken), e.mustAuth(writeToken)
	if !r.MCPReadOnly || w.MCPReadOnly {
		t.Fatalf("read_only switch: %+v %+v", r, w)
	}
	// The scope is computed for the request from the project: read_only forces every element to read.
	var ok bool
	if r.MCPScope, ok, _ = e.requestScope(r, "pe"); !ok || mcpScopeHasWrite(r.MCPScope) || len(r.MCPScope) != 1 {
		t.Fatalf("read-only scope: %+v", r.MCPScope)
	}
	if w.MCPScope, ok, _ = e.requestScope(w, "pe"); !ok || !mcpScopeHasWrite(w.MCPScope) {
		t.Fatalf("write scope: %+v", w.MCPScope)
	}
	for _, tool := range []string{"wiki_write", "wiki_edit", "wiki_comment", "wiki_restore"} {
		if mcpScopedTool(&r, tool) || !mcpScopedTool(&w, tool) {
			t.Fatalf("%s: read=%v write=%v", tool, mcpScopedTool(&r, tool), mcpScopedTool(&w, tool))
		}
	}
	for _, tool := range []string{"wiki_bootstrap", "wiki_search", "wiki_read", "wiki_history", "wiki_comments"} {
		if !mcpScopedTool(&r, tool) || !mcpScopedTool(&w, tool) {
			t.Fatalf("%s must be readable in both modes", tool)
		}
	}
	for _, tool := range []string{"wiki_delete", "wiki_admin", ""} {
		if mcpScopedTool(&r, tool) || mcpScopedTool(&w, tool) {
			t.Fatalf("unknown tool %q must be denied", tool)
		}
	}
}
