package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// Everything below that touches persistence uses the disposable PostgreSQL
// fixture (skipped without AIWIKI_TEST_POSTGRES*). The identity provider's
// admission API is always a mock.

type persistentProvider struct {
	mu     sync.Mutex
	denied map[string]bool
	fault  bool
	calls  []string
	hook   func()
}

func (p *persistentProvider) Check(_ context.Context, issuer, subject string) error {
	p.mu.Lock()
	p.calls = append(p.calls, issuer+"|"+subject)
	denied, fault, hook := p.denied[subject], p.fault, p.hook
	p.mu.Unlock()
	if hook != nil {
		hook()
	}
	if fault {
		return errors.New("provider unavailable")
	}
	if denied {
		return errMCPAdmissionDenied
	}
	return nil
}

func (p *persistentProvider) deny(subject string, v bool) {
	p.mu.Lock()
	if p.denied == nil {
		p.denied = map[string]bool{}
	}
	p.denied[subject] = v
	p.mu.Unlock()
}

func (p *persistentProvider) setFault(v bool) { p.mu.Lock(); p.fault = v; p.mu.Unlock() }

func (p *persistentProvider) setHook(f func()) { p.mu.Lock(); p.hook = f; p.mu.Unlock() }

func (p *persistentProvider) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.calls)
}

func (p *persistentProvider) last() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.calls) == 0 {
		return ""
	}
	return p.calls[len(p.calls)-1]
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
	prov   *persistentProvider
	clk    *persistentClock
}

func newPersistentEnv(t *testing.T) *persistentEnv {
	t.Helper()
	owner, root := corporateFixture(t)
	x := newAuthHarness(t, nil)
	x.a.users = authSQLUsers{db: root.DB}
	e := &persistentEnv{
		t: t, owner: owner, root: root, admin: owner.DB, x: x, iss: x.iss.srv.URL,
		prov: &persistentProvider{denied: map[string]bool{}},
		clk:  &persistentClock{t: time.Now().UTC().Truncate(time.Second)},
	}
	x.a.persistent = &mcpPersistentAccess{db: root.DB, admission: e.prov, now: e.clk.now}
	root.auth = x.a
	return e
}

// user inserts a local user for the harness issuer and returns a browser-side
// Server for it plus its id.
func (e *persistentEnv) user(sub string) (*Server, string) {
	e.t.Helper()
	uid := authUserID(e.iss, sub)
	e.exec(`INSERT INTO users(id,issuer,subject,name,admin) VALUES(?,?,?,?,0)`, uid, e.iss, sub, sub)
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

// seed creates space s1 owned by alice with projects:
// pa, pb, pe (alice editor), pr (alice reader), pn (alice: nothing),
// parch (archived, alice editor), pbob (bob editor).
func (e *persistentEnv) seed(alice, bob string) {
	e.t.Helper()
	db, done, err := e.owner.DB.scoped(context.Background(), alice)
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(done)
	e.seedDB = db
	e.seedExec(`INSERT INTO spaces(id,name,owner_id) VALUES('s1','Space',?)`, alice)
	for _, p := range []string{"pa", "pb", "pe", "pr", "pn", "parch", "pbob"} {
		e.seedExec(`INSERT INTO projects(id,space_id,name) VALUES(?,?,?)`, p, "s1", p)
	}
	e.seedExec(`UPDATE projects SET archived_at='2020-01-01T00:00:00Z' WHERE id='parch'`)
	for p, role := range map[string]string{"pa": "editor", "pb": "editor", "pe": "editor", "pr": "reader", "parch": "editor"} {
		e.member(p, alice, role)
	}
	e.member("pbob", bob, "editor")
}

func (e *persistentEnv) seedExec(q string, args ...any) {
	e.t.Helper()
	if _, err := e.seedDB.Exec(q, args...); err != nil {
		e.t.Fatal(err)
	}
}

func (e *persistentEnv) member(project, user, role string) {
	e.t.Helper()
	e.seedExec(`INSERT INTO memberships(resource_type,resource_id,subject_type,subject_id,role) VALUES('project',?,'user',?,?)`, project, user, role)
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
		"no provider": {persistent: &mcpPersistentAccess{db: &database{}}},
		"no database": {persistent: &mcpPersistentAccess{admission: &persistentProvider{}}},
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
	prov := &persistentProvider{}
	// The database is never reached for malformed tokens.
	a := &AuthService{persistent: &mcpPersistentAccess{db: &database{}, admission: prov, now: time.Now}}
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
		t.Fatal("malformed tokens must not reach the provider")
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

func TestMCPPersistentStoredScopeParsing(t *testing.T) {
	for _, raw := range []string{``, `null`, `[]`, `[1]`, `["a",1]`, `["../x"]`, `["a","a"]`, `{"a":1}`, `"a"`, `["a"`, `[""]`} {
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
	if p, ok := mcpPersistentProjects([]byte(`["pa","pb"]`)); !ok || len(p) != 2 || p[0] != "pa" || p == nil {
		t.Fatalf("valid scope rejected: %v %v", p, ok)
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

	out := e.mint(alice, `{"name":"  agent  ","project_ids":["pb","pa"],"mode":"write","ttl_seconds":3600}`, http.StatusCreated)
	token, _ := out["token"].(string)
	id, _ := out["id"].(string)
	if !strings.HasPrefix(token, mcpPersistentPrefix) || len(token) != len(mcpPersistentPrefix)+mcpPersistentSecretLen || !mcpPersistentWellFormed(token) {
		t.Fatalf("bad token shape %q", token)
	}
	if !strings.HasPrefix(id, "pat_") || out["name"] != "agent" || out["mode"] != "write" || len(out) != 7 {
		t.Fatalf("bad mint response: %v", out)
	}
	if ids, _ := json.Marshal(out["project_ids"]); string(ids) != `["pa","pb"]` {
		t.Fatalf("projects not sorted: %s", ids)
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
	if p.ID != aliceID || p.Issuer != e.iss || p.Subject != "alice" || p.Admin || p.MCPMode != "write" ||
		p.MCPDelegationID != id || p.MCPActorID != aliceID || !p.MCPExpiresAt.Equal(expires) ||
		len(p.MCPProjects) != 2 || p.MCPProjects[0] != "pa" || p.MCPProjects[1] != "pb" {
		t.Fatalf("bad principal: %+v", p)
	}
	if e.prov.count() != before+1 || e.prov.last() != e.iss+"|alice" {
		t.Fatalf("provider must be asked about the owner: %d %q", e.prov.count(), e.prov.last())
	}
	if !mcpScopedTool(&p, "wiki_write") || !mcpScopedTool(&p, "wiki_read") || mcpScopedTool(&p, "wiki_delete") {
		t.Fatal("write credential tool scope")
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
	for _, m := range []string{"PUT", "PATCH", "POST", "GET"} {
		e.expect(e.call(alice, persistentCredentialR, m, "", id), 405)
	}
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
		`{"name":"","project_ids":["pa"]}`,
		`{"name":"   ","project_ids":["pa"]}`,
		`{"name":"` + strings.Repeat("x", 81) + `","project_ids":["pa"]}`,
		`{"name":"n","project_ids":[]}`,
		`{"name":"n"}`,
		`{"name":"n","project_ids":[` + strings.Join(many, ",") + `]}`,
		`{"name":"n","project_ids":["pa","pa"]}`,
		`{"name":"n","project_ids":["../x"]}`,
		`{"name":"n","project_ids":[""]}`,
		`{"name":"n","project_ids":["pa"],"mode":"admin"}`,
		`{"name":"n","project_ids":["pa"],"mode":"WRITE"}`,
		`{"name":"n","project_ids":["pa"],"ttl_seconds":59}`,
		`{"name":"n","project_ids":["pa"],"ttl_seconds":-1}`,
		`{"name":"n","project_ids":["pa"],"ttl_seconds":2592001}`,
		`{"name":"n","project_ids":["pa"],"ttl_seconds":1.5}`,
		`{"name":"n","project_ids":["pa"],"admin":true}`,
		`{"name":"n","project_ids":["pa"],"owner_id":"someone"}`,
		`{"name":"n","project_ids":["pa"],"service_account_id":"a/b"}`,
		`{"name":"n","project_ids":["pa"]} {}`,
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
	out := e.mint(alice, `{"name":"defaults","project_ids":["pa"]}`, http.StatusCreated)
	if out["mode"] != "read" || span(out) != 7*24*time.Hour {
		t.Fatalf("defaults: %v", out)
	}
	if out := e.mint(alice, `{"name":"min","project_ids":["pa"],"mode":"read","ttl_seconds":60}`, http.StatusCreated); span(out) != time.Minute {
		t.Fatalf("min ttl: %v", out)
	}
	if out := e.mint(alice, `{"name":"max","project_ids":["pa"],"ttl_seconds":2592000}`, http.StatusCreated); span(out) != 30*24*time.Hour {
		t.Fatalf("max ttl: %v", out)
	}

	// Credentials and delegations can never manage credentials.
	for name, pr := range map[string]*Principal{
		"scoped":     {ID: aliceID, Issuer: e.iss, Subject: "alice", MCPProjects: []string{"pa"}},
		"credential": {ID: aliceID, Issuer: e.iss, Subject: "alice", MCPDelegationID: "pat_x"},
		"actor":      {ID: aliceID, Issuer: e.iss, Subject: "alice", MCPActorID: "svc_x"},
	} {
		cp := *alice
		cp.principal = pr
		e.expect(e.call(&cp, persistentCredentials, "POST", `{"name":"n","project_ids":["pa"]}`, ""), 403)
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

	e.mint(alice, `{"name":"n","project_ids":["pr"],"mode":"read"}`, http.StatusCreated)
	e.mint(alice, `{"name":"n","project_ids":["pe"],"mode":"write"}`, http.StatusCreated)
	e.mint(bob, `{"name":"n","project_ids":["pbob"],"mode":"write"}`, http.StatusCreated)
	allowed := e.count(`SELECT count(*) FROM mcp_credentials`)
	audited := e.count(`SELECT count(*) FROM security_audit`)

	for _, body := range []string{
		`{"name":"n","project_ids":["pr"],"mode":"write"}`,        // reader cannot write
		`{"name":"n","project_ids":["pe","pr"],"mode":"write"}`,   // one weak project spoils the set
		`{"name":"n","project_ids":["pn"],"mode":"read"}`,         // no rights
		`{"name":"n","project_ids":["pa","pn"],"mode":"read"}`,    // one hidden project
		`{"name":"n","project_ids":["parch"],"mode":"read"}`,      // archived
		`{"name":"n","project_ids":["parch"],"mode":"write"}`,     // archived
		`{"name":"n","project_ids":["missing"],"mode":"read"}`,    // unknown
		`{"name":"n","project_ids":["s1"],"mode":"read"}`,         // a space is not a project
		`{"name":"n","project_ids":["pbob"],"mode":"read"}`,       // someone else's project
		`{"name":"n","project_ids":["pbob","pa"],"mode":"write"}`, // mixed ownership
	} {
		e.mint(alice, body, http.StatusForbidden)
	}
	e.mint(bob, `{"name":"n","project_ids":["pa"],"mode":"read"}`, http.StatusForbidden)
	if e.count(`SELECT count(*) FROM mcp_credentials`) != allowed || e.count(`SELECT count(*) FROM security_audit`) != audited {
		t.Fatal("forbidden mints must neither store nor audit anything")
	}

	// The check happens at mint time against the live ACL.
	e.seedExec(`DELETE FROM memberships WHERE resource_id='pe' AND subject_id=?`, aliceID)
	e.mint(alice, `{"name":"n","project_ids":["pe"],"mode":"read"}`, http.StatusForbidden)
	e.seedExec(`UPDATE projects SET archived_at='2020-01-01T00:00:00Z' WHERE id='pa'`)
	e.mint(alice, `{"name":"n","project_ids":["pa"],"mode":"read"}`, http.StatusForbidden)
}

func TestMCPPersistentMintCaps(t *testing.T) {
	e := newPersistentEnv(t)
	alice, aliceID := e.user("alice")
	_, bobID := e.user("bob")
	e.seed(aliceID, bobID)
	body := `{"name":"n","project_ids":["pr"]}`

	// Active cap per owner; revoking frees a slot, other owners are unaffected.
	carol, carolID := e.user("carol")
	e.member("pr", carolID, "reader")
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
	e.clk.advance(8 * 24 * time.Hour)
	e.mint(carol, body, http.StatusCreated)
	if n := e.count(`SELECT count(*) FROM mcp_credentials WHERE owner_id=?`, carolID); n != mcpPersistentActiveCap+2 {
		t.Fatalf("carol rows: %d", n)
	}

	// Total cap counts revoked and expired credentials too.
	dave, daveID := e.user("dave")
	e.member("pr", daveID, "reader")
	e.exec(`INSERT INTO mcp_credentials(id,owner_id,name,secret_hash,project_ids,mode,created_at,expires_at,revoked_at)
SELECT 'pat_bulk'||g::text,?::text,'old',md5(?::text||g::text)||md5(g::text||?::text),'["pr"]'::jsonb,'read',now(),now()+interval '1 day',now()
FROM generate_series(1,?) g`, daveID, daveID, daveID, mcpPersistentTotalCap)
	e.mint(dave, body, http.StatusTooManyRequests)
	e.exec(`DELETE FROM mcp_credentials WHERE id='pat_bulk1'`)
	e.mint(dave, body, http.StatusCreated)
	e.mint(dave, body, http.StatusTooManyRequests)

	// Concurrent mints obey the cap exactly.
	eve, eveID := e.user("eve")
	e.member("pr", eveID, "reader")
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
	e.mint(bob, `{"name":"n","project_ids":["pbob"],"service_account_id":"`+svc+`"}`, http.StatusForbidden)
	e.mint(alice, `{"name":"n","project_ids":["pa"],"service_account_id":"svc_missing"}`, http.StatusForbidden)

	// Service-account credentials are attributed to the account, authorised as the sponsor.
	svcCred, svcToken := e.mintToken(alice, `{"name":"svc","project_ids":["pa"],"mode":"write","service_account_id":"`+svc+`"}`)
	ownCred, ownToken := e.mintToken(alice, `{"name":"own","project_ids":["pa"]}`)
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
	e.mint(alice, `{"name":"n","project_ids":["pa"],"service_account_id":"`+svc+`"}`, http.StatusForbidden)
	e.expect(e.call(alice, persistentAccountR, "DELETE", "", svc), 204)
	if n := e.count(`SELECT count(*) FROM security_audit WHERE action='mcp_service_account.disable'`); n != 1 {
		t.Fatalf("repeat disable audits: %d", n)
	}
	for _, m := range []string{"PUT", "PATCH", "POST", "GET"} {
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
	body := `{"name":"n","project_ids":["pa"],"ttl_seconds":600}`

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

	// Invalid stored scope fails closed.
	// Migration-owner corruption simulation; runtime cannot update authority.
	e.exec(`ALTER TABLE mcp_credentials DISABLE TRIGGER mcp_credential_immutable`)
	defer e.exec(`ALTER TABLE mcp_credentials ENABLE TRIGGER mcp_credential_immutable`)
	for _, scope := range []string{`["../x"]`, `[1]`, `["pa","pa"]`, `[""]`} {
		e.exec(`UPDATE mcp_credentials SET project_ids=? WHERE id=?`, scope, id)
		e.mustFail(token, http.StatusUnauthorized)
	}
	e.exec(`UPDATE mcp_credentials SET project_ids='["pa"]' WHERE id=?`, id)
	e.exec(`ALTER TABLE mcp_credentials ENABLE TRIGGER mcp_credential_immutable`)
	e.mustAuth(token)

	// Service accounts: disabled, or no longer owned by the credential's owner.
	svc := e.account(alice, "bot")
	_, svcToken := e.mintToken(alice, `{"name":"n","project_ids":["pa"],"service_account_id":"`+svc+`"}`)
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

func TestMCPPersistentProviderEveryRequest(t *testing.T) {
	e := newPersistentEnv(t)
	alice, aliceID := e.user("alice")
	_, bobID := e.user("bob")
	e.seed(aliceID, bobID)
	id, token := e.mintToken(alice, `{"name":"n","project_ids":["pa"]}`)

	base := e.prov.count()
	for i := 1; i <= 3; i++ {
		e.mustAuth(token)
		if e.prov.count() != base+i || e.prov.last() != e.iss+"|alice" {
			t.Fatalf("every request must consult the provider: %d %q", e.prov.count(), e.prov.last())
		}
	}

	// A denial is never cached either way, and never revokes by itself.
	e.prov.deny("alice", true)
	e.mustFail(token, http.StatusForbidden)
	e.prov.deny("alice", false)
	e.mustAuth(token)

	// Provider faults fail closed without changing the credential.
	e.prov.setFault(true)
	e.mustFail(token, http.StatusServiceUnavailable)
	e.prov.setFault(false)
	e.mustAuth(token)
	if e.text(`SELECT coalesce(revoked_at::text,'') FROM mcp_credentials WHERE id=?`, id) != "" {
		t.Fatal("provider outcomes must not alter the credential")
	}
}

func TestMCPPersistentProviderRequiredToMintNotToRevoke(t *testing.T) {
	e := newPersistentEnv(t)
	alice, aliceID := e.user("alice")
	_, bobID := e.user("bob")
	e.seed(aliceID, bobID)
	body := `{"name":"n","project_ids":["pa"]}`

	credA, tokenA := e.mintToken(alice, body)
	credB, _ := e.mintToken(alice, body)
	svcA := e.account(alice, "a")
	svcB := e.account(alice, "b")
	_, svcTokenA := e.mintToken(alice, `{"name":"n","project_ids":["pa"],"service_account_id":"`+svcA+`"}`)
	rows := e.count(`SELECT count(*) FROM mcp_credentials`)
	accounts := e.count(`SELECT count(*) FROM mcp_service_accounts`)

	// Denied or failing provider: nothing new can be created.
	e.prov.deny("alice", true)
	e.mint(alice, body, http.StatusForbidden)
	e.expect(e.call(alice, persistentAccounts, "POST", `{"name":"x"}`, ""), 403)
	e.prov.deny("alice", false)
	e.prov.setFault(true)
	e.mint(alice, body, http.StatusServiceUnavailable)
	e.expect(e.call(alice, persistentAccounts, "POST", `{"name":"x"}`, ""), 503)
	if e.count(`SELECT count(*) FROM mcp_credentials`) != rows || e.count(`SELECT count(*) FROM mcp_service_accounts`) != accounts {
		t.Fatal("refused creation must store nothing")
	}

	// ...but listing, revocation and disabling keep working, without asking it.
	asked := e.prov.count()
	e.expect(e.call(alice, persistentCredentials, "GET", "", ""), 200)
	e.expect(e.call(alice, persistentAccounts, "GET", "", ""), 200)
	e.expect(e.call(alice, persistentCredentialR, "DELETE", "", credA), 204)
	e.expect(e.call(alice, persistentAccountR, "DELETE", "", svcA), 204)
	if e.prov.count() != asked {
		t.Fatal("revocation must not depend on the provider")
	}
	e.prov.setFault(false)
	e.mustFail(tokenA, http.StatusUnauthorized)
	e.mustFail(svcTokenA, http.StatusUnauthorized)

	// Feature switched off: management reads and revocation still work, creation and
	// authentication are refused.
	live := e.mintTokenFor(alice, body)
	for name, off := range map[string]*mcpPersistentAccess{
		"nil":         nil,
		"no provider": {db: e.root.DB, now: e.clk.now},
		"no database": {admission: e.prov, now: e.clk.now},
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
	e.x.a.persistent = &mcpPersistentAccess{db: e.root.DB, admission: e.prov, now: e.clk.now}
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
	body := `{"name":"n","project_ids":["pa"]}`
	asked := e.prov.count()

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
	if e.prov.count() != asked {
		t.Fatal("the provider must only be asked about a consistent identity")
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
	body := `{"name":"n","project_ids":["pa"]}`

	svc := e.account(alice, "bot")
	id, token := e.mintToken(alice, body)
	svcCred, svcToken := e.mintToken(alice, `{"name":"n","project_ids":["pa"],"service_account_id":"`+svc+`"}`)
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
	body := `{"name":"n","project_ids":["pa"]}`
	idA, tokenA := e.mintToken(alice, body)
	_, tokenB := e.mintToken(alice, body)

	asked := e.prov.count()
	for i := 0; i < mcpPersistentRateLimit; i++ {
		e.mustAuth(tokenA)
	}
	if e.prov.count()-asked != mcpPersistentRateLimit {
		t.Fatalf("provider calls: %d", e.prov.count()-asked)
	}
	_, rec, ok := persistentAuthOn(e.x.a, tokenA)
	if ok || rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") != "60" {
		t.Fatalf("over the limit: ok=%v %d %v", ok, rec.Code, rec.Header())
	}
	e.mustFail(tokenA, http.StatusTooManyRequests)
	if e.prov.count()-asked != mcpPersistentRateLimit {
		t.Fatal("rate limited requests must not reach the provider")
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
	_, token := e.mintToken(alice, `{"name":"n","project_ids":["pa"]}`)

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
		t.Fatalf("provider calls %d", e.prov.count()-asked)
	}
}

func TestMCPPersistentChangesDuringProviderCheckAreHonoured(t *testing.T) {
	e := newPersistentEnv(t)
	alice, aliceID := e.user("alice")
	_, bobID := e.user("bob")
	e.seed(aliceID, bobID)
	body := `{"name":"n","project_ids":["pa"],"ttl_seconds":600}`
	svc := e.account(alice, "bot")
	defer e.prov.setHook(nil)

	// Revoked while the provider is being asked.
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
	_, svcToken := e.mintToken(alice, `{"name":"n","project_ids":["pa"],"service_account_id":"`+svc+`"}`)
	e.prov.setHook(func() { e.exec(`UPDATE mcp_service_accounts SET disabled=1 WHERE id=?`, svc) })
	e.mustFail(svcToken, http.StatusForbidden)
	e.prov.setHook(nil)

	// Expired meanwhile.
	_, token = e.mintToken(alice, body)
	e.prov.setHook(func() { e.clk.advance(time.Hour) })
	e.mustFail(token, http.StatusUnauthorized)
}

func TestMCPPersistentSurvivesRestartWithoutCache(t *testing.T) {
	e := newPersistentEnv(t)
	alice, aliceID := e.user("alice")
	_, bobID := e.user("bob")
	e.seed(aliceID, bobID)
	id, token := e.mintToken(alice, `{"name":"n","project_ids":["pa","pb"],"mode":"write"}`)
	_, other := e.mintToken(alice, `{"name":"n","project_ids":["pa"]}`)
	before := e.mustAuth(token)

	// A brand new service over the same database knows nothing but the store.
	restarted, err := newAuthServiceWithUsers(context.Background(), authSQLUsers{db: e.root.DB}, e.x.a.cfg)
	if err != nil {
		t.Fatal(err)
	}
	if restarted == e.x.a {
		t.Fatal("restart must use a new AuthService")
	}
	restarted.persistent = &mcpPersistentAccess{db: e.root.DB, admission: e.prov, now: e.clk.now}
	p, rec, ok := persistentAuthOn(restarted, token)
	if !ok {
		t.Fatalf("token must survive a restart: %d %s", rec.Code, rec.Body)
	}
	if p.ID != before.ID || p.MCPDelegationID != before.MCPDelegationID || p.MCPMode != "write" || len(p.MCPProjects) != 2 || !p.MCPExpiresAt.Equal(before.MCPExpiresAt) {
		t.Fatalf("restarted principal differs: %+v vs %+v", p, before)
	}

	// Revocation through one instance is visible to the other at once.
	e.expect(e.call(alice, persistentCredentialR, "DELETE", "", id), 204)
	e.mustFailOn(restarted, token, http.StatusUnauthorized)
	e.mustFail(token, http.StatusUnauthorized)
	if _, _, ok := persistentAuthOn(restarted, other); !ok {
		t.Fatal("unrelated credential must keep working")
	}

	// A restarted service without the provider configured refuses everything.
	restarted.persistent = nil
	e.mustFailOn(restarted, other, http.StatusServiceUnavailable)
}

func TestMCPPersistentModeMapsToToolScope(t *testing.T) {
	e := newPersistentEnv(t)
	alice, aliceID := e.user("alice")
	_, bobID := e.user("bob")
	e.seed(aliceID, bobID)
	_, readToken := e.mintToken(alice, `{"name":"r","project_ids":["pe"],"mode":"read"}`)
	_, writeToken := e.mintToken(alice, `{"name":"w","project_ids":["pe"],"mode":"write"}`)

	r, w := e.mustAuth(readToken), e.mustAuth(writeToken)
	if r.MCPMode != "read" || w.MCPMode != "write" || r.MCPProjects == nil || w.MCPProjects == nil {
		t.Fatalf("modes: %+v %+v", r, w)
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
