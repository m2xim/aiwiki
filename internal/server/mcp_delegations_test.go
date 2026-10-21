package server

import (
	"context"
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

func delegationLogin(t *testing.T, x *authHarness, sub string) (*http.Cookie, string) {
	t.Helper()
	c := x.session(authUserGrant(sub, "aiwiki-access", "aiwiki-admin"))
	return c, authUserID(x.iss.srv.URL, sub)
}

func delegationSeed(t *testing.T, a *AuthService, c *http.Cookie, owner, secret string, ttl time.Duration, projects ...string) *mcpDelegation {
	t.Helper()
	now := a.now()
	h := authHash(secret)
	g := &mcpDelegation{
		ID:         "dlg_" + hex.EncodeToString(h[:6]),
		Name:       "test",
		OwnerID:    owner,
		Projects:   projects,
		Created:    now,
		Expires:    now.Add(ttl),
		SessionKey: authHash(c.Value),
	}
	if st := a.reserveDelegation(h, g); st != 0 {
		t.Fatalf("reserve status %d", st)
	}
	return g
}

func delegationAuth(a *AuthService, token string) (Principal, *httptest.ResponseRecorder, bool) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/mcp", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	p, ok := a.authenticateDelegation(rec, req, token)
	return p, rec, ok
}

func delegationMustFail(t *testing.T, a *AuthService, token string, want int) {
	t.Helper()
	_, rec, ok := delegationAuth(a, token)
	if ok || rec.Code != want {
		t.Fatalf("want %d, got ok=%v status=%d body=%s", want, ok, rec.Code, rec.Body)
	}
	if len(rec.Result().Cookies()) != 0 {
		t.Fatal("bearer response must not set cookies")
	}
}

func delegationCount(a *AuthService) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.delegations)
}

func TestMCPDelegationAuthenticateByHash(t *testing.T) {
	x := newAuthHarness(t, nil)
	c, uid := delegationLogin(t, x, "alice")
	delegationMustFail(t, x.a, mcpDelegationPrefix+"unknown", http.StatusUnauthorized)

	secret := "s3cret-delegation-value"
	token := mcpDelegationPrefix + secret
	g := delegationSeed(t, x.a, c, uid, secret, time.Minute, "pa", "pb")

	x.a.mu.Lock()
	_, byHash := x.a.delegations[authHash(secret)]
	_, byToken := x.a.delegations[authHash(token)]
	x.a.mu.Unlock()
	if !byHash || byToken {
		t.Fatalf("grant must be keyed by hash of the secret only: hash=%v token=%v", byHash, byToken)
	}

	p, rec, ok := delegationAuth(x.a, token)
	if !ok {
		t.Fatalf("auth failed: %d %s", rec.Code, rec.Body)
	}
	if p.ID != uid || p.Admin || len(p.MCPProjects) != 2 || p.MCPProjects[0] != "pa" || p.MCPProjects[1] != "pb" {
		t.Fatalf("bad principal: %+v", p)
	}
	p.MCPProjects[0] = "mutated"
	if g.Projects[0] != "pa" {
		t.Fatal("principal projects must be a copy")
	}

	for _, bad := range []string{secret, mcpDelegationPrefix, "", "other_" + secret, mcpDelegationPrefix + secret + "x"} {
		delegationMustFail(t, x.a, bad, http.StatusUnauthorized)
	}
}

func TestMCPDelegationExpiry(t *testing.T) {
	x := newAuthHarness(t, nil)
	c, uid := delegationLogin(t, x, "alice")
	now := time.Now()
	x.a.now = func() time.Time { return now }
	delegationSeed(t, x.a, c, uid, "short", 10*time.Second, "pa")
	if _, _, ok := delegationAuth(x.a, mcpDelegationPrefix+"short"); !ok {
		t.Fatal("fresh grant must authenticate")
	}
	now = now.Add(10 * time.Second) // exactly at expiry is already dead
	delegationMustFail(t, x.a, mcpDelegationPrefix+"short", http.StatusUnauthorized)
	if delegationCount(x.a) != 0 {
		t.Fatal("expired grant must be removed")
	}
}

func TestMCPDelegationLocalRevoke(t *testing.T) {
	x := newAuthHarness(t, nil)
	c, uid := delegationLogin(t, x, "alice")
	delegationSeed(t, x.a, c, uid, "rev", time.Minute, "pa")
	x.a.removeDelegation(authHash("rev"))
	delegationMustFail(t, x.a, mcpDelegationPrefix+"rev", http.StatusUnauthorized)
}

func TestMCPDelegationSessionLogoutInvalidates(t *testing.T) {
	x := newAuthHarness(t, nil)
	c, uid := delegationLogin(t, x, "alice")
	delegationSeed(t, x.a, c, uid, "one", time.Minute, "pa")
	if _, _, ok := delegationAuth(x.a, mcpDelegationPrefix+"one"); !ok {
		t.Fatal("grant must work before logout")
	}
	csrf := x.a.sessionSnapshot(authHash(c.Value)).csrf
	rec := x.do("POST", "/auth/logout", "", map[string]string{"X-CSRF-Token": csrf}, c)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("logout %d", rec.Code)
	}
	delegationMustFail(t, x.a, mcpDelegationPrefix+"one", http.StatusUnauthorized)
	if delegationCount(x.a) != 0 {
		t.Fatal("grant must be gone after logout")
	}
}

func TestMCPDelegationSessionExpiryInvalidates(t *testing.T) {
	x := newAuthHarness(t, nil)
	c, uid := delegationLogin(t, x, "alice")
	now := time.Now()
	x.a.now = func() time.Time { return now }
	delegationSeed(t, x.a, c, uid, "long", 10*time.Minute, "pa")
	now = now.Add(authDefaultSessionTTL + time.Second)
	delegationMustFail(t, x.a, mcpDelegationPrefix+"long", http.StatusUnauthorized)
}

func TestMCPDelegationOwnerMismatchFailsClosed(t *testing.T) {
	x := newAuthHarness(t, nil)
	c, _ := delegationLogin(t, x, "alice")
	// Grant claims bob but is attached to alice's session: never usable.
	g := &mcpDelegation{ID: "dlg_x", OwnerID: authUserID(x.iss.srv.URL, "bob"), Projects: []string{"pa"},
		Created: time.Now(), Expires: time.Now().Add(time.Minute), SessionKey: authHash(c.Value)}
	x.a.mu.Lock()
	x.a.delegations = map[[32]byte]*mcpDelegation{authHash("mix"): g}
	x.a.mu.Unlock()
	delegationMustFail(t, x.a, mcpDelegationPrefix+"mix", http.StatusUnauthorized)

	// A direct reservation with a mismatched owner is refused too.
	if st := x.a.reserveDelegation(authHash("mix2"), g); st != http.StatusUnauthorized {
		t.Fatalf("reserve mismatched owner: %d", st)
	}
}

func TestMCPDelegationEveryRequestIntrospects(t *testing.T) {
	x := newAuthHarness(t, nil)
	c, uid := delegationLogin(t, x, "alice")
	token := x.a.sessionSnapshot(authHash(c.Value)).accessToken
	delegationSeed(t, x.a, c, uid, "intro", time.Minute, "pa")
	tok := mcpDelegationPrefix + "intro"

	// IdP outage: 503 and the grant survives.
	x.iss.setDown(true)
	delegationMustFail(t, x.a, tok, http.StatusServiceUnavailable)
	x.iss.setDown(false)
	if _, _, ok := delegationAuth(x.a, tok); !ok {
		t.Fatal("grant must recover after outage")
	}

	// Admission role lost upstream: denied, and grant removed.
	x.iss.setRoles(token, authRolesClaim("something-else"))
	delegationMustFail(t, x.a, tok, http.StatusForbidden)
	if delegationCount(x.a) != 0 {
		t.Fatal("grant must be removed after admission loss")
	}
}

func TestMCPDelegationInactiveUpstreamToken(t *testing.T) {
	x := newAuthHarness(t, nil)
	c, uid := delegationLogin(t, x, "alice")
	token := x.a.sessionSnapshot(authHash(c.Value)).accessToken
	delegationSeed(t, x.a, c, uid, "inact", time.Minute, "pa")
	x.iss.setActive(token, false)
	delegationMustFail(t, x.a, mcpDelegationPrefix+"inact", http.StatusUnauthorized)
	if delegationCount(x.a) != 0 {
		t.Fatal("grant must be removed when upstream token is inactive")
	}
}

func TestMCPDelegationDisabledUserAndStaleAdminFlag(t *testing.T) {
	x := newAuthHarness(t, nil)
	c, uid := delegationLogin(t, x, "alice")
	delegationSeed(t, x.a, c, uid, "dis", time.Minute, "pa")
	// A cached users.admin flag is irrelevant: the principal is never admin.
	x.users.mu.Lock()
	x.users.users[uid].Admin = true
	x.users.mu.Unlock()
	p, _, ok := delegationAuth(x.a, mcpDelegationPrefix+"dis")
	if !ok || p.Admin {
		t.Fatalf("delegation must never be admin: ok=%v %+v", ok, p)
	}
	x.users.disable(x.iss.srv.URL, "alice")
	delegationMustFail(t, x.a, mcpDelegationPrefix+"dis", http.StatusForbidden)
}

func TestMCPDelegationCaps(t *testing.T) {
	x := newAuthHarness(t, nil)
	c, uid := delegationLogin(t, x, "alice")
	c2, uid2 := delegationLogin(t, x, "bob")
	now := time.Now()
	x.a.now = func() time.Time { return now }

	for i := 0; i < mcpDelegationUserCap; i++ {
		delegationSeed(t, x.a, c, uid, fmt.Sprintf("cap-%d", i), time.Minute, "pa")
	}
	over := &mcpDelegation{ID: "dlg_over", OwnerID: uid, Projects: []string{"pa"}, Created: now, Expires: now.Add(time.Minute), SessionKey: authHash(c.Value)}
	if st := x.a.reserveDelegation(authHash("over"), over); st != http.StatusTooManyRequests {
		t.Fatalf("per-user cap: %d", st)
	}
	delegationSeed(t, x.a, c2, uid2, "bob-ok", time.Minute, "pa") // other users unaffected

	now = now.Add(2 * time.Minute) // everything above expired
	if st := x.a.reserveDelegation(authHash("over"), &mcpDelegation{ID: "dlg_over", OwnerID: uid, Projects: []string{"pa"}, Created: now, Expires: now.Add(time.Minute), SessionKey: authHash(c.Value)}); st != 0 {
		t.Fatalf("expired grants must free capacity: %d", st)
	}
}

func TestMCPDelegationGlobalCap(t *testing.T) {
	x := newAuthHarness(t, nil)
	c, uid := delegationLogin(t, x, "alice")
	now := x.a.now()
	x.a.mu.Lock()
	x.a.delegations = map[[32]byte]*mcpDelegation{}
	for i := 0; i < mcpDelegationGlobalCap; i++ {
		sk := authHash(fmt.Sprintf("fake-session-%d", i))
		owner := fmt.Sprintf("fake-user-%d", i)
		x.a.sessions[sk] = &authSession{userID: owner, expires: now.Add(time.Hour)}
		x.a.delegations[authHash(fmt.Sprintf("fake-%d", i))] = &mcpDelegation{OwnerID: owner, SessionKey: sk, Expires: now.Add(time.Hour)}
	}
	x.a.mu.Unlock()
	g := &mcpDelegation{ID: "dlg_g", OwnerID: uid, Projects: []string{"pa"}, Created: now, Expires: now.Add(time.Minute), SessionKey: authHash(c.Value)}
	if st := x.a.reserveDelegation(authHash("g"), g); st != http.StatusTooManyRequests {
		t.Fatalf("global cap: %d", st)
	}
}

func TestMCPDelegationConcurrentReserveObeysCap(t *testing.T) {
	x := newAuthHarness(t, nil)
	c, uid := delegationLogin(t, x, "alice")
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok := 0
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			now := time.Now()
			g := &mcpDelegation{ID: fmt.Sprintf("dlg_%d", i), OwnerID: uid, Projects: []string{"pa"}, Created: now, Expires: now.Add(time.Minute), SessionKey: authHash(c.Value)}
			if x.a.reserveDelegation(authHash(fmt.Sprintf("c-%d", i)), g) == 0 {
				mu.Lock()
				ok++
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()
	if ok != mcpDelegationUserCap || delegationCount(x.a) != mcpDelegationUserCap {
		t.Fatalf("admitted %d, stored %d, want %d", ok, delegationCount(x.a), mcpDelegationUserCap)
	}
}

func TestMCPDelegationNoSecretExposure(t *testing.T) {
	x := newAuthHarness(t, nil)
	c, uid := delegationLogin(t, x, "alice")
	secret := "very-secret-value"
	g := delegationSeed(t, x.a, c, uid, secret, time.Minute, "pa")
	h := authHash(secret)
	sk := authHash(c.Value)
	for _, v := range []any{g, g.public(), mcpDelegationIssued{mcpDelegation: g.public()}} {
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		s := string(raw)
		for _, leak := range []string{secret, hex.EncodeToString(h[:]), hex.EncodeToString(sk[:]), uid, c.Value,
			x.a.sessionSnapshot(sk).accessToken, "SessionKey", "OwnerID", "owner"} {
			if strings.Contains(s, leak) {
				t.Fatalf("serialised grant leaks %q: %s", leak, s)
			}
		}
	}
	var m map[string]any
	raw, _ := json.Marshal(g.public())
	json.Unmarshal(raw, &m)
	if len(m) != 5 {
		t.Fatalf("unexpected public fields: %v", m)
	}
}

// --- PostgreSQL-backed handler tests (skipped without a disposable database) ---

type delegationDB struct {
	owner, root *Server
	x           *authHarness
}

func delegationUser(t *testing.T, d *delegationDB, sub string) (*Server, *http.Cookie, string) {
	t.Helper()
	cookie, uid := delegationLogin(t, d.x, sub)
	if _, err := d.owner.DB.Exec(`INSERT INTO users(id,issuer,subject,name,admin) VALUES(?,?,?,?,0)`, uid, d.x.iss.srv.URL, sub, sub); err != nil {
		t.Fatal(err)
	}
	db, cleanup, err := d.root.DB.scoped(context.Background(), uid)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup)
	cp := *d.root
	cp.DB = db
	cp.auth = d.x.a
	cp.principal = &Principal{ID: uid, Name: sub}
	return &cp, cookie, uid
}

func delegationCall(s *Server, h func(http.ResponseWriter, *http.Request), method, body string, cookie *http.Cookie, id string) *httptest.ResponseRecorder {
	if s.DB.pg {
		db, done, err := s.DB.resumeAfterTransfer(context.Background())
		if err != nil {
			panic(err)
		}
		defer done()
		cp := *s
		cp.DB = db
		s = &cp
	}
	req := httptest.NewRequest(method, "/api/mcp/delegations", strings.NewReader(body))
	if cookie != nil {
		req.AddCookie(cookie)
	}
	if id != "" {
		req.SetPathValue("id", id)
	}
	rec := httptest.NewRecorder()
	h(rec, req)
	return rec
}

func delegationMint(t *testing.T, s *Server, cookie *http.Cookie, body string, want int) map[string]any {
	t.Helper()
	rec := delegationCall(s, s.mcpDelegations, "POST", body, cookie, "")
	if rec.Code != want {
		t.Fatalf("mint %s: want %d got %d %s", body, want, rec.Code, rec.Body)
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("responses must be no-store")
	}
	var out map[string]any
	if want == http.StatusCreated {
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
	}
	return out
}

func TestMCPDelegationPostgresLifecycle(t *testing.T) {
	owner, root := corporateFixture(t)
	d := &delegationDB{owner: owner, root: root, x: newAuthHarness(t, nil)}
	alice, aliceCookie, aliceID := delegationUser(t, d, "alice")
	bob, bobCookie, bobID := delegationUser(t, d, "bob")
	seedDB, seedDone, err := owner.DB.scoped(context.Background(), aliceID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(seedDone)
	owner.DB = seedDB

	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := owner.DB.Exec(q, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO spaces(id,name,owner_id) VALUES('s1','Space',?)`, aliceID)
	for _, p := range []string{"pa", "pb", "pn", "parch"} {
		exec(`INSERT INTO projects(id,space_id,name) VALUES(?,?,?)`, p, "s1", p)
	}
	exec(`UPDATE projects SET archived_at='2020-01-01T00:00:00Z' WHERE id='parch'`)
	for _, p := range []string{"pa", "pb", "parch"} {
		exec(`INSERT INTO memberships(resource_type,resource_id,subject_type,subject_id,role) VALUES('project',?,'user',?,'reader')`, p, aliceID)
	}
	exec(`INSERT INTO memberships(resource_type,resource_id,subject_type,subject_id,role) VALUES('project','pa','user',?,'reader')`, bobID)

	// Validation and admission.
	for _, body := range []string{
		`{"name":"","project_ids":["pa"]}`,
		`{"name":"` + strings.Repeat("x", 81) + `","project_ids":["pa"]}`,
		`{"name":"n","project_ids":[]}`,
		`{"name":"n","project_ids":["pa","pa"]}`,
		`{"name":"n","project_ids":["../x"]}`,
		`{"name":"n","project_ids":["pa"],"ttl_seconds":601}`,
		`{"name":"n","project_ids":["pa"],"ttl_seconds":-1}`,
		`{"name":"n","project_ids":["pa"],"admin":true}`,
		`{"name":"n","project_ids":["pa"]} {}`,
	} {
		delegationMint(t, alice, aliceCookie, body, http.StatusBadRequest)
	}
	for _, p := range []string{"pn", "parch", "s1", "missing"} {
		delegationMint(t, alice, aliceCookie, `{"name":"n","project_ids":["pa","`+p+`"]}`, http.StatusForbidden)
	}
	if rec := delegationCall(alice, alice.mcpDelegations, "POST", `{"name":"n","project_ids":["pa"]}`, nil, ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("missing session cookie: %d", rec.Code)
	}
	if rec := delegationCall(alice, alice.mcpDelegations, "PUT", ``, aliceCookie, ""); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("method: %d", rec.Code)
	}
	if delegationCount(d.x.a) != 0 {
		t.Fatal("rejected requests must not create grants")
	}

	// Mint.
	out := delegationMint(t, alice, aliceCookie, `{"name":"  agent  ","project_ids":["pb","pa"],"ttl_seconds":600}`, http.StatusCreated)
	token, _ := out["token"].(string)
	if !strings.HasPrefix(token, mcpDelegationPrefix) || len(out) != 6 {
		t.Fatalf("bad mint response: %v", out)
	}
	if out["name"] != "agent" {
		t.Fatalf("name not trimmed: %v", out["name"])
	}
	if ids, _ := json.Marshal(out["project_ids"]); string(ids) != `["pa","pb"]` {
		t.Fatalf("projects not sorted: %s", ids)
	}
	sess := d.x.a.sessionSnapshot(authHash(aliceCookie.Value))
	expires, err := time.Parse(time.RFC3339Nano, out["expires_at"].(string))
	if err != nil || expires.After(sess.expires) {
		t.Fatalf("expiry must not exceed the session: %v %v", expires, sess.expires)
	}
	secret := strings.TrimPrefix(token, mcpDelegationPrefix)
	h := authHash(secret)

	// Audit exists and carries no secret or hash.
	var details string
	if err := owner.DB.QueryRow(`SELECT details::text FROM security_audit WHERE action='mcp_delegation.create' AND actor_id=?`, aliceID).Scan(&details); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(details, secret) || strings.Contains(details, hex.EncodeToString(h[:])) {
		t.Fatalf("audit leaks the credential: %s", details)
	}

	// The token authenticates as a read-only exact-project principal.
	p, rec, ok := delegationAuth(d.x.a, token)
	if !ok || p.ID != aliceID || p.Admin || len(p.MCPProjects) != 2 {
		t.Fatalf("auth: ok=%v %d %+v", ok, rec.Code, p)
	}

	// Listing: owner only, never the secret, empty array rather than null.
	rec = delegationCall(alice, alice.mcpDelegations, "GET", ``, aliceCookie, "")
	if rec.Code != 200 || strings.Contains(rec.Body.String(), secret) || strings.Contains(rec.Body.String(), "token") {
		t.Fatalf("list: %d %s", rec.Code, rec.Body)
	}
	var list []map[string]any
	json.Unmarshal(rec.Body.Bytes(), &list)
	if len(list) != 1 || list[0]["id"] != out["id"] {
		t.Fatalf("list: %v", list)
	}
	rec = delegationCall(bob, bob.mcpDelegations, "GET", ``, bobCookie, "")
	if rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != "[]" {
		t.Fatalf("other user's list: %d %s", rec.Code, rec.Body)
	}

	// Another user cannot revoke it; the owner can; repeats are 404.
	id := out["id"].(string)
	if rec = delegationCall(bob, bob.mcpDelegation, "DELETE", ``, bobCookie, id); rec.Code != http.StatusNotFound {
		t.Fatalf("foreign revoke: %d", rec.Code)
	}
	if _, _, ok := delegationAuth(d.x.a, token); !ok {
		t.Fatal("foreign revoke must not affect the grant")
	}
	if rec = delegationCall(alice, alice.mcpDelegation, "DELETE", ``, aliceCookie, id); rec.Code != http.StatusNoContent {
		t.Fatalf("revoke: %d %s", rec.Code, rec.Body)
	}
	delegationMustFail(t, d.x.a, token, http.StatusUnauthorized)
	if rec = delegationCall(alice, alice.mcpDelegation, "DELETE", ``, aliceCookie, id); rec.Code != http.StatusNotFound {
		t.Fatalf("second revoke: %d", rec.Code)
	}
	var n int
	if err := owner.DB.QueryRow(`SELECT count(*) FROM security_audit WHERE action='mcp_delegation.revoke' AND resource_id=?`, id).Scan(&n); err != nil || n != 1 {
		t.Fatalf("revoke audit: %d %v", n, err)
	}

	// A delegated principal cannot mint more delegations.
	delegated := *alice
	delegated.principal = &Principal{ID: aliceID, MCPProjects: []string{"pa"}}
	if rec = delegationCall(&delegated, delegated.mcpDelegations, "POST", `{"name":"n","project_ids":["pa"]}`, aliceCookie, ""); rec.Code != http.StatusForbidden {
		t.Fatalf("delegated mint: %d", rec.Code)
	}

	// Per-user cap.
	for i := 0; i < mcpDelegationUserCap; i++ {
		delegationMint(t, alice, aliceCookie, `{"name":"n","project_ids":["pa"]}`, http.StatusCreated)
	}
	delegationMint(t, alice, aliceCookie, `{"name":"n","project_ids":["pa"]}`, http.StatusTooManyRequests)
	if err := owner.DB.QueryRow(`SELECT count(*) FROM security_audit WHERE action='mcp_delegation.create'`).Scan(&n); err != nil || n != mcpDelegationUserCap+1 {
		t.Fatalf("rejected mint must not be audited as created: %d %v", n, err)
	}

	// Audit failure: no key is issued and no grant is left behind.
	before := delegationCount(d.x.a)
	if _, err := owner.DB.Exec(`REVOKE INSERT ON security_audit FROM aiwiki_app`); err != nil {
		t.Fatal(err)
	}
	bobOut := delegationCall(bob, bob.mcpDelegations, "POST", `{"name":"n","project_ids":["pa"]}`, bobCookie, "")
	if _, err := owner.DB.Exec(`GRANT INSERT ON security_audit TO aiwiki_app`); err != nil {
		t.Fatal(err)
	}
	if bobOut.Code != http.StatusServiceUnavailable || strings.Contains(bobOut.Body.String(), mcpDelegationPrefix) || delegationCount(d.x.a) != before {
		t.Fatalf("audit failure must not issue a key: %d %s", bobOut.Code, bobOut.Body)
	}

	// Logging out of the parent session kills every grant and blocks minting.
	csrf := d.x.a.sessionSnapshot(authHash(aliceCookie.Value)).csrf
	if r := d.x.do("POST", "/auth/logout", "", map[string]string{"X-CSRF-Token": csrf}, aliceCookie); r.Code != http.StatusNoContent {
		t.Fatalf("logout %d", r.Code)
	}
	delegationMint(t, alice, aliceCookie, `{"name":"n","project_ids":["pa"]}`, http.StatusUnauthorized)
	d.x.a.mu.Lock()
	d.x.a.purgeDelegationsLocked(d.x.a.now())
	left := len(d.x.a.delegations)
	d.x.a.mu.Unlock()
	if left != 0 {
		t.Fatalf("%d grants survived logout", left)
	}
}
