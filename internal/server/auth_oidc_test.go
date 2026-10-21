package server

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"golang.org/x/oauth2"
)

const (
	authTestClientID = "aiwiki"
	authTestSecret   = "s3cret-value"
	authTestRedirect = "http://127.0.0.1:8080/auth/callback"
)

type authGrant struct {
	sub      string
	name     string
	claims   map[string]any // merged into access token and introspection
	tokenTTL time.Duration
	nonce    string // overrides the nonce put in the ID token
}

func authRolesClaim(roles ...string) map[string]any {
	list := make([]any, len(roles))
	for i, r := range roles {
		list[i] = r
	}
	return map[string]any{"realm_access": map[string]any{"roles": list}}
}

type authMockIssuer struct {
	t      *testing.T
	srv    *httptest.Server
	key    *rsa.PrivateKey
	signer jose.Signer

	mu         sync.Mutex
	codes      map[string]authCode
	introspect map[string]map[string]any
	down       bool
}

type authCode struct {
	challenge string
	grant     authGrant
}

func authNewSigner(t *testing.T, key *rsa.PrivateKey) jose.Signer {
	t.Helper()
	s, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: jose.JSONWebKey{Key: key, KeyID: "k1"}},
		(&jose.SignerOptions{}).WithType("JWT"))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func newAuthMockIssuer(t *testing.T) *authMockIssuer {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	m := &authMockIssuer{t: t, key: key, signer: authNewSigner(t, key),
		codes: map[string]authCode{}, introspect: map[string]map[string]any{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"issuer":                                m.srv.URL,
			"authorization_endpoint":                m.srv.URL + "/authorize",
			"token_endpoint":                        m.srv.URL + "/token",
			"introspection_endpoint":                m.srv.URL + "/introspect",
			"jwks_uri":                              m.srv.URL + "/jwks",
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{
			{Key: &key.PublicKey, KeyID: "k1", Algorithm: "RS256", Use: "sig"}}})
	})
	mux.HandleFunc("/token", m.handleToken)
	mux.HandleFunc("/introspect", m.handleIntrospect)
	m.srv = httptest.NewServer(mux)
	t.Cleanup(m.srv.Close)
	return m
}

func (m *authMockIssuer) sign(signer jose.Signer, claims map[string]any) string {
	raw, err := jwt.Signed(signer).Claims(claims).Serialize()
	if err != nil {
		m.t.Fatal(err)
	}
	return raw
}

func (m *authMockIssuer) accessClaims(g authGrant) map[string]any {
	ttl := g.tokenTTL
	if ttl == 0 {
		ttl = 10 * time.Minute
	}
	c := map[string]any{
		"iss": m.srv.URL, "aud": authTestClientID, "sub": g.sub,
		"jti": newID(),
		"iat": time.Now().Add(-time.Second).Unix(), "exp": time.Now().Add(ttl).Unix(),
		"typ": "Bearer", "name": g.name,
	}
	for k, v := range g.claims {
		c[k] = v
	}
	return c
}

// bearer mints a signed access token and registers it as active.
func (m *authMockIssuer) bearer(g authGrant) string {
	c := m.accessClaims(g)
	raw := m.sign(m.signer, c)
	intro := map[string]any{"active": true}
	for k, v := range c {
		intro[k] = v
	}
	m.mu.Lock()
	m.introspect[raw] = intro
	m.mu.Unlock()
	return raw
}

func (m *authMockIssuer) setActive(token string, active bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !active {
		delete(m.introspect, token)
	}
}

func (m *authMockIssuer) setRoles(token string, claims map[string]any) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for k, v := range claims {
		m.introspect[token][k] = v
	}
}

func (m *authMockIssuer) setDown(down bool) {
	m.mu.Lock()
	m.down = down
	m.mu.Unlock()
}

func (m *authMockIssuer) issueCode(challenge string, g authGrant) string {
	b := make([]byte, 12)
	rand.Read(b)
	code := base64.RawURLEncoding.EncodeToString(b)
	m.mu.Lock()
	m.codes[code] = authCode{challenge: challenge, grant: g}
	m.mu.Unlock()
	return code
}

func (m *authMockIssuer) handleToken(w http.ResponseWriter, r *http.Request) {
	user, pass, ok := r.BasicAuth()
	if !ok || user != authTestClientID || pass != url.QueryEscape(authTestSecret) {
		http.Error(w, `{"error":"invalid_client"}`, http.StatusUnauthorized)
		return
	}
	r.ParseForm()
	m.mu.Lock()
	ac, found := m.codes[r.Form.Get("code")]
	delete(m.codes, r.Form.Get("code"))
	m.mu.Unlock()
	sum := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
	if !found || r.Form.Get("grant_type") != "authorization_code" ||
		base64.RawURLEncoding.EncodeToString(sum[:]) != ac.challenge ||
		r.Form.Get("redirect_uri") != authTestRedirect {
		http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
		return
	}
	g := ac.grant
	access := m.bearer(g)
	nonce := g.nonce
	idc := map[string]any{
		"iss": m.srv.URL, "aud": authTestClientID, "sub": g.sub, "name": g.name,
		"iat": time.Now().Unix(), "exp": time.Now().Add(time.Hour).Unix(), "nonce": nonce,
	}
	ttl := g.tokenTTL
	if ttl == 0 {
		ttl = 10 * time.Minute
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"access_token": access, "token_type": "Bearer", "expires_in": int(ttl.Seconds()),
		"id_token": m.sign(m.signer, idc),
	})
}

func (m *authMockIssuer) handleIntrospect(w http.ResponseWriter, r *http.Request) {
	user, pass, ok := r.BasicAuth()
	if !ok || user != authTestClientID || pass != url.QueryEscape(authTestSecret) {
		http.Error(w, "", http.StatusUnauthorized)
		return
	}
	m.mu.Lock()
	down := m.down
	r.ParseForm()
	resp, found := m.introspect[r.Form.Get("token")]
	m.mu.Unlock()
	if down {
		http.Error(w, "", http.StatusInternalServerError)
		return
	}
	if !found {
		resp = map[string]any{"active": false}
	}
	json.NewEncoder(w).Encode(resp)
}

type authFakeUsers struct {
	mu    sync.Mutex
	users map[string]*Principal
	off   map[string]bool
}

func (f *authFakeUsers) upsert(_ context.Context, p Principal) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := p
	f.users[p.ID] = &cp
	return f.off[p.ID], nil
}

func (f *authFakeUsers) find(_ context.Context, id string) (string, bool, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	u, ok := f.users[id]
	if !ok {
		return "", false, false, nil
	}
	return u.Name, f.off[id], true, nil
}

func (f *authFakeUsers) disable(issuer, sub string) {
	f.mu.Lock()
	f.off[authUserID(issuer, sub)] = true
	f.mu.Unlock()
}

type authHarness struct {
	t     *testing.T
	a     *AuthService
	iss   *authMockIssuer
	users *authFakeUsers
	h     http.Handler
}

func newAuthHarness(t *testing.T, mutate func(*CorporateConfig)) *authHarness {
	t.Helper()
	iss := newAuthMockIssuer(t)
	cfg := CorporateConfig{
		IssuerURL: iss.srv.URL, ClientID: authTestClientID, ClientSecret: authTestSecret,
		RedirectURL: authTestRedirect, AllowInsecureHTTP: true,
	}
	if mutate != nil {
		mutate(&cfg)
	}
	users := &authFakeUsers{users: map[string]*Principal{}, off: map[string]bool{}}
	a, err := newAuthServiceWithUsers(context.Background(), users, cfg)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	a.routes(mux)
	echo := func(w http.ResponseWriter, r *http.Request) {
		p, ok := a.authenticate(w, r)
		if !ok {
			return
		}
		body, _ := io.ReadAll(r.Body)
		json.NewEncoder(w).Encode(map[string]any{"principal": p, "body": string(body)})
	}
	mux.HandleFunc("/api/echo", echo)
	mux.HandleFunc("/mcp", echo)
	return &authHarness{t: t, a: a, iss: iss, users: users, h: mux}
}

func (x *authHarness) do(method, target, body string, hdr map[string]string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	x.h.ServeHTTP(rec, req)
	return rec
}

func authCookie(rec *httptest.ResponseRecorder, name string) *http.Cookie {
	for _, c := range rec.Result().Cookies() {
		if c.Name == name && c.MaxAge >= 0 && c.Value != "" {
			return c
		}
	}
	return nil
}

// startLogin hits /auth/login and returns the authorize query and login cookie.
func (x *authHarness) startLogin() (url.Values, *http.Cookie) {
	x.t.Helper()
	rec := x.do("GET", "/auth/login", "", nil)
	if rec.Code != http.StatusFound {
		x.t.Fatalf("login status %d", rec.Code)
	}
	loc, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		x.t.Fatal(err)
	}
	return loc.Query(), authCookie(rec, x.a.loginCookie)
}

// login runs the full flow and returns the callback recorder.
func (x *authHarness) login(g authGrant) *httptest.ResponseRecorder {
	x.t.Helper()
	q, lc := x.startLogin()
	if g.nonce == "" {
		g.nonce = q.Get("nonce")
	}
	code := x.iss.issueCode(q.Get("code_challenge"), g)
	return x.do("GET", "/auth/callback?code="+code+"&state="+url.QueryEscape(q.Get("state")), "", nil, lc)
}

func (x *authHarness) session(g authGrant) *http.Cookie {
	x.t.Helper()
	rec := x.login(g)
	if rec.Code != http.StatusSeeOther {
		x.t.Fatalf("callback status %d body %s", rec.Code, rec.Body)
	}
	c := authCookie(rec, x.a.sessionCookie)
	if c == nil {
		x.t.Fatal("no session cookie")
	}
	return c
}

func authUserGrant(sub string, roles ...string) authGrant {
	return authGrant{sub: sub, name: "User " + sub, claims: authRolesClaim(roles...)}
}

func TestAuthLoginStatePKCEAndSingleUse(t *testing.T) {
	x := newAuthHarness(t, nil)
	q, lc := x.startLogin()
	if q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" ||
		q.Get("state") == "" || q.Get("nonce") == "" || q.Get("response_type") != "code" ||
		q.Get("client_id") != authTestClientID || q.Get("redirect_uri") != authTestRedirect {
		t.Fatalf("bad authorize query: %v", q)
	}
	if lc == nil || !lc.HttpOnly || lc.SameSite != http.SameSiteLaxMode {
		t.Fatalf("bad login cookie: %+v", lc)
	}
	g := authUserGrant("alice", "aiwiki-access")
	g.nonce = q.Get("nonce")
	code := x.iss.issueCode(q.Get("code_challenge"), g)
	target := "/auth/callback?code=" + code + "&state=" + url.QueryEscape(q.Get("state"))

	// Missing browser binding cookie is refused, and burns the state.
	if rec := x.do("GET", target, "", nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("unbound callback: %d", rec.Code)
	}
	if rec := x.do("GET", target, "", nil, lc); rec.Code != http.StatusBadRequest {
		t.Fatalf("state must be single use: %d", rec.Code)
	}

	// A fresh login succeeds, redirects to / and sets a hardened cookie.
	rec := x.login(authUserGrant("alice", "aiwiki-access"))
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/" {
		t.Fatalf("callback: %d %s", rec.Code, rec.Header().Get("Location"))
	}
	sc := authCookie(rec, x.a.sessionCookie)
	if sc == nil || !sc.HttpOnly || sc.SameSite != http.SameSiteLaxMode {
		t.Fatalf("bad session cookie: %+v", sc)
	}
	if strings.Contains(sc.Value, "alice") {
		t.Fatal("cookie must be opaque")
	}

	// Replay of a completed callback fails.
	q2, lc2 := x.startLogin()
	g2 := authUserGrant("alice", "aiwiki-access")
	g2.nonce = q2.Get("nonce")
	code2 := x.iss.issueCode(q2.Get("code_challenge"), g2)
	ok := "/auth/callback?code=" + code2 + "&state=" + url.QueryEscape(q2.Get("state"))
	if r := x.do("GET", ok, "", nil, lc2); r.Code != http.StatusSeeOther {
		t.Fatalf("expected success, got %d", r.Code)
	}
	if r := x.do("GET", ok, "", nil, lc2); r.Code != http.StatusBadRequest {
		t.Fatalf("replay must fail, got %d", r.Code)
	}
}

func TestAuthRejectsWrongStateWrongPKCEAndNonce(t *testing.T) {
	x := newAuthHarness(t, nil)

	q, lc := x.startLogin()
	g := authUserGrant("bob", "aiwiki-access")
	g.nonce = q.Get("nonce")
	code := x.iss.issueCode(q.Get("code_challenge"), g)
	if r := x.do("GET", "/auth/callback?code="+code+"&state=forged", "", nil, lc); r.Code != http.StatusBadRequest {
		t.Fatalf("forged state: %d", r.Code)
	}

	// Code bound to a different PKCE challenge: token endpoint refuses.
	q, lc = x.startLogin()
	g.nonce = q.Get("nonce")
	code = x.iss.issueCode(authS256("other-verifier"), g)
	if r := x.do("GET", "/auth/callback?code="+code+"&state="+url.QueryEscape(q.Get("state")), "", nil, lc); r.Code != http.StatusUnauthorized {
		t.Fatalf("pkce mismatch: %d", r.Code)
	}

	// ID token with a nonce the caller did not request.
	g.nonce = "attacker-nonce"
	if r := x.login(g); r.Code != http.StatusUnauthorized {
		t.Fatalf("nonce mismatch: %d", r.Code)
	}
}

func authS256(v string) string {
	sum := sha256.Sum256([]byte(v))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func TestAuthMeLogoutAndCSRF(t *testing.T) {
	x := newAuthHarness(t, nil)
	sc := x.session(authUserGrant("carol", "aiwiki-access", "aiwiki-admin"))

	if rec := x.do("GET", "/api/me", "", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous me: %d", rec.Code)
	}
	rec := x.do("GET", "/api/me", "", nil, sc)
	if rec.Code != 200 {
		t.Fatalf("me: %d %s", rec.Code, rec.Body)
	}
	var me struct {
		Mode string `json:"mode"`
		User struct {
			ID    string `json:"id"`
			Name  string `json:"name"`
			Admin bool   `json:"admin"`
		} `json:"user"`
		CSRF string `json:"csrf_token"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &me); err != nil {
		t.Fatal(err)
	}
	if me.Mode != "corporate" || !me.User.Admin || me.CSRF == "" ||
		me.User.ID != authUserID(x.iss.srv.URL, "carol") || me.User.Name != "User carol" {
		t.Fatalf("unexpected me: %+v", me)
	}
	if strings.Contains(rec.Body.String(), authTestSecret) {
		t.Fatal("secret leaked")
	}

	// Unsafe request needs the CSRF header; the body is left untouched.
	if r := x.do("POST", "/api/echo", "payload", nil, sc); r.Code != http.StatusForbidden {
		t.Fatalf("no csrf: %d", r.Code)
	}
	if r := x.do("POST", "/api/echo", "payload", map[string]string{"X-CSRF-Token": "wrong"}, sc); r.Code != http.StatusForbidden {
		t.Fatalf("bad csrf: %d", r.Code)
	}
	r := x.do("POST", "/api/echo", "payload", map[string]string{"X-CSRF-Token": me.CSRF}, sc)
	if r.Code != 200 || !strings.Contains(r.Body.String(), `"body":"payload"`) {
		t.Fatalf("csrf ok: %d %s", r.Code, r.Body)
	}

	// Logout requires CSRF, then revokes locally.
	if r := x.do("POST", "/auth/logout", "", nil, sc); r.Code != http.StatusForbidden {
		t.Fatalf("logout without csrf: %d", r.Code)
	}
	if r := x.do("GET", "/api/me", "", nil, sc); r.Code != 200 {
		t.Fatal("session must survive failed logout")
	}
	lo := x.do("POST", "/auth/logout", "", map[string]string{"X-CSRF-Token": me.CSRF}, sc)
	if lo.Code != http.StatusNoContent {
		t.Fatalf("logout: %d", lo.Code)
	}
	var cleared bool
	for _, c := range lo.Result().Cookies() {
		cleared = cleared || (c.Name == x.a.sessionCookie && c.MaxAge < 0)
	}
	if !cleared {
		t.Fatal("cookie not cleared")
	}
	if r := x.do("GET", "/api/me", "", nil, sc); r.Code != http.StatusUnauthorized {
		t.Fatalf("revoked session still valid: %d", r.Code)
	}
}

func TestAuthConfigurableRoles(t *testing.T) {
	x := newAuthHarness(t, func(c *CorporateConfig) {
		c.AccessRole, c.AdminRole, c.RolesClaim = "wiki-user", "wiki-root", "resource_access.aiwiki.roles"
	})
	custom := func(roles ...string) map[string]any {
		list := make([]any, len(roles))
		for i, r := range roles {
			list[i] = r
		}
		return map[string]any{"resource_access": map[string]any{"aiwiki": map[string]any{"roles": list}}}
	}
	grant := func(sub string, claims map[string]any) authGrant {
		return authGrant{sub: sub, name: sub, claims: claims}
	}
	if r := x.login(grant("noroles", nil)); r.Code != http.StatusForbidden {
		t.Fatalf("missing roles must deny: %d", r.Code)
	}
	if r := x.login(grant("default-claim", authRolesClaim("aiwiki-access"))); r.Code != http.StatusForbidden {
		t.Fatalf("default claim path must not apply: %d", r.Code)
	}
	if r := x.login(grant("adminonly", custom("wiki-root"))); r.Code != http.StatusForbidden {
		t.Fatalf("admin without access must deny: %d", r.Code)
	}
	sc := x.session(grant("member", custom("wiki-user")))
	var me struct {
		User struct{ Admin bool } `json:"user"`
	}
	json.Unmarshal(x.do("GET", "/api/me", "", nil, sc).Body.Bytes(), &me)
	if me.User.Admin {
		t.Fatal("member must not be admin")
	}
	sc = x.session(grant("boss", custom("wiki-user", "wiki-root")))
	json.Unmarshal(x.do("GET", "/api/me", "", nil, sc).Body.Bytes(), &me)
	if !me.User.Admin {
		t.Fatal("expected admin")
	}
}

func TestAuthDefaultRolesAndDotKeys(t *testing.T) {
	cfg, err := normalizeAuthConfig(CorporateConfig{IssuerURL: "https://idp", ClientID: "c", ClientSecret: "s", RedirectURL: "https://app/cb"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AccessRole != "aiwiki-access" || cfg.AdminRole != "aiwiki-admin" ||
		cfg.RolesClaim != "realm_access.roles" || cfg.SessionTTL != 5*time.Minute {
		t.Fatalf("bad defaults: %+v", cfg)
	}
	got := authRoles(map[string]any{"a.b": map[string]any{"roles": []any{"x"}}}, "a.b.roles")
	if len(got) != 1 || got[0] != "x" {
		t.Fatalf("dotted key lookup: %v", got)
	}
}

func TestAuthDisabledUserNeverReenabled(t *testing.T) {
	x := newAuthHarness(t, nil)
	g := authUserGrant("dave", "aiwiki-access")
	sc := x.session(g)
	if r := x.do("GET", "/api/me", "", nil, sc); r.Code != 200 {
		t.Fatalf("me: %d", r.Code)
	}
	x.users.disable(x.iss.srv.URL, "dave")
	if r := x.do("GET", "/api/me", "", nil, sc); r.Code != http.StatusForbidden {
		t.Fatalf("disabled existing session: %d", r.Code)
	}
	if r := x.do("GET", "/api/me", "", nil, sc); r.Code != http.StatusUnauthorized {
		t.Fatalf("session should be dropped: %d", r.Code)
	}
	if r := x.login(g); r.Code != http.StatusForbidden {
		t.Fatalf("login must not re-enable: %d", r.Code)
	}
	if !x.users.off[authUserID(x.iss.srv.URL, "dave")] {
		t.Fatal("user was re-enabled")
	}
}

func TestAuthIdentityIsIssuerAndSubject(t *testing.T) {
	x := newAuthHarness(t, nil)
	g1 := authUserGrant("s1", "aiwiki-access")
	g1.claims["email"] = "same@example.com"
	g2 := authUserGrant("s2", "aiwiki-access")
	g2.claims["email"] = "same@example.com"
	x.session(g1)
	x.session(g2)
	if len(x.users.users) != 2 {
		t.Fatalf("users merged by email: %d", len(x.users.users))
	}
}

func TestAuthIntrospectionFailsClosedAndRevokes(t *testing.T) {
	x := newAuthHarness(t, nil)
	sc := x.session(authUserGrant("erin", "aiwiki-access"))

	x.iss.setDown(true)
	if r := x.do("GET", "/api/echo", "", nil, sc); r.Code != http.StatusServiceUnavailable {
		t.Fatalf("outage must fail closed: %d", r.Code)
	}
	x.iss.setDown(false)
	if r := x.do("GET", "/api/echo", "", nil, sc); r.Code != 200 {
		t.Fatalf("recovery: %d", r.Code)
	}

	// Roles revoked upstream take effect on the next request.
	x.iss.mu.Lock()
	var tok string
	for k := range x.iss.introspect {
		tok = k
	}
	x.iss.mu.Unlock()
	x.iss.setRoles(tok, authRolesClaim("aiwiki-admin"))
	if r := x.do("GET", "/api/echo", "", nil, sc); r.Code != http.StatusForbidden {
		t.Fatalf("access role removed: %d", r.Code)
	}
	if r := x.do("GET", "/api/echo", "", nil, sc); r.Code != http.StatusUnauthorized {
		t.Fatalf("session should be dropped: %d", r.Code)
	}

	sc = x.session(authUserGrant("erin", "aiwiki-access"))
	x.iss.mu.Lock()
	for k := range x.iss.introspect {
		delete(x.iss.introspect, k)
	}
	x.iss.mu.Unlock()
	if r := x.do("GET", "/api/echo", "", nil, sc); r.Code != http.StatusUnauthorized {
		t.Fatalf("inactive token: %d", r.Code)
	}
}

func TestAuthSessionBounds(t *testing.T) {
	x := newAuthHarness(t, func(c *CorporateConfig) { c.SessionTTL = 15 * time.Minute })
	start := time.Now()
	g := authUserGrant("fay", "aiwiki-access")
	g.tokenTTL = time.Minute
	x.session(g)
	x.a.mu.Lock()
	for _, s := range x.a.sessions {
		if s.expires.After(start.Add(time.Minute + 5*time.Second)) {
			t.Errorf("session outlives access token: %v", s.expires.Sub(start))
		}
	}
	x.a.mu.Unlock()

	if _, err := normalizeAuthConfig(CorporateConfig{IssuerURL: "https://i", ClientID: "c", ClientSecret: "s",
		RedirectURL: "https://a/cb", SessionTTL: 16 * time.Minute}); err == nil {
		t.Fatal("TTL above max must be refused")
	}

	x.a.maxSessions = 1
	if r := x.login(authUserGrant("gus", "aiwiki-access")); r.Code != http.StatusServiceUnavailable {
		t.Fatalf("session cap: %d", r.Code)
	}

	x.a.now = func() time.Time { return start.Add(time.Hour) }
	x.a.mu.Lock()
	x.a.purgeLocked(x.a.now())
	n := len(x.a.sessions)
	x.a.mu.Unlock()
	if n != 0 {
		t.Fatalf("expired sessions kept: %d", n)
	}
}

func TestAuthMCPBearerOnlyAndCookiesRefused(t *testing.T) {
	x := newAuthHarness(t, nil)
	bearer := func(tok string) map[string]string { return map[string]string{"Authorization": "Bearer " + tok} }

	good := x.iss.bearer(authUserGrant("hana", "aiwiki-access"))
	rec := x.do("POST", "/mcp", "{}", bearer(good))
	if rec.Code != 200 {
		t.Fatalf("valid bearer: %d %s", rec.Code, rec.Body)
	}
	var out struct{ Principal Principal }
	json.Unmarshal(rec.Body.Bytes(), &out)
	if out.Principal.ID != authUserID(x.iss.srv.URL, "hana") || out.Principal.Admin {
		t.Fatalf("principal: %+v", out.Principal)
	}

	// Browser cookie (even with CSRF) never authenticates /mcp.
	sc := x.session(authUserGrant("hana", "aiwiki-access"))
	if r := x.do("GET", "/mcp", "", nil, sc); r.Code != http.StatusUnauthorized {
		t.Fatalf("cookie on /mcp: %d", r.Code)
	}
	// Bearer tokens do not authenticate other paths.
	if r := x.do("GET", "/api/echo", "", bearer(good)); r.Code != http.StatusUnauthorized {
		t.Fatalf("bearer on /api: %d", r.Code)
	}

	// Token signed by an unknown key.
	rogueKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	forged := x.iss.sign(authNewSigner(t, rogueKey), x.iss.accessClaims(authUserGrant("hana", "aiwiki-access")))
	x.iss.mu.Lock()
	x.iss.introspect[forged] = map[string]any{"active": true}
	for k, v := range x.iss.accessClaims(authUserGrant("hana", "aiwiki-access")) {
		x.iss.introspect[forged][k] = v
	}
	x.iss.mu.Unlock()
	if r := x.do("POST", "/mcp", "", bearer(forged)); r.Code != http.StatusUnauthorized {
		t.Fatalf("bad signature: %d", r.Code)
	}

	signed := func(mut func(map[string]any)) string {
		c := x.iss.accessClaims(authUserGrant("hana", "aiwiki-access"))
		mut(c)
		raw := x.iss.sign(x.iss.signer, c)
		intro := map[string]any{"active": true}
		for k, v := range c {
			intro[k] = v
		}
		x.iss.mu.Lock()
		x.iss.introspect[raw] = intro
		x.iss.mu.Unlock()
		return raw
	}
	cases := map[string]string{
		"wrong audience": signed(func(c map[string]any) { c["aud"] = "other" }),
		"wrong issuer":   signed(func(c map[string]any) { c["iss"] = "http://127.0.0.1:1" }),
		"expired":        signed(func(c map[string]any) { c["exp"] = time.Now().Add(-time.Hour).Unix() }),
		"id token typ":   signed(func(c map[string]any) { c["typ"] = "ID" }),
		"garbage":        "not-a-jwt",
	}
	for name, tok := range cases {
		if r := x.do("POST", "/mcp", "", bearer(tok)); r.Code != http.StatusUnauthorized {
			t.Errorf("%s: got %d", name, r.Code)
		}
	}

	// Valid signature but introspection says inactive.
	revoked := x.iss.bearer(authUserGrant("hana", "aiwiki-access"))
	x.iss.setActive(revoked, false)
	if r := x.do("POST", "/mcp", "", bearer(revoked)); r.Code != http.StatusUnauthorized {
		t.Fatalf("inactive: %d", r.Code)
	}

	// Missing access role, admin role alone, and disabled user.
	if r := x.do("POST", "/mcp", "", bearer(x.iss.bearer(authUserGrant("noacc")))); r.Code != http.StatusForbidden {
		t.Fatalf("no roles: %d", r.Code)
	}
	if r := x.do("POST", "/mcp", "", bearer(x.iss.bearer(authUserGrant("adm", "aiwiki-admin")))); r.Code != http.StatusForbidden {
		t.Fatalf("admin without access: %d", r.Code)
	}
	x.users.disable(x.iss.srv.URL, "hana")
	if r := x.do("POST", "/mcp", "", bearer(good)); r.Code != http.StatusForbidden {
		t.Fatalf("disabled: %d", r.Code)
	}

	// Introspection outage fails closed.
	other := x.iss.bearer(authUserGrant("ivan", "aiwiki-access"))
	x.iss.setDown(true)
	if r := x.do("POST", "/mcp", "", bearer(other)); r.Code != http.StatusServiceUnavailable {
		t.Fatalf("outage: %d", r.Code)
	}
	if r := x.do("POST", "/mcp", "", nil); r.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous: %d", r.Code)
	}
}

func TestAuthEndpointPolicy(t *testing.T) {
	cases := []struct {
		url   string
		allow bool
		ok    bool
	}{
		{"https://idp.example.com/realm", false, true},
		{"http://idp.example.com/realm", false, false},
		{"http://127.0.0.1:8080/x", false, false},
		{"http://127.0.0.1:8080/x", true, true},
		{"http://localhost:8080/x", true, true},
		{"http://[::1]:8080/x", true, true},
		{"http://idp.example.com/x", true, false},
		{"http://169.254.169.254/x", true, false},
		{"https://user:pw@idp.example.com/", false, false},
		{"file:///etc/passwd", true, false},
		{"", true, false},
	}
	for _, c := range cases {
		if err := authCheckEndpoint(c.url, c.allow); (err == nil) != c.ok {
			t.Errorf("%q allow=%v: err=%v", c.url, c.allow, err)
		}
	}
	if _, err := newAuthServiceWithUsers(context.Background(), &authFakeUsers{}, CorporateConfig{
		IssuerURL: "http://127.0.0.1:1", ClientID: "c", ClientSecret: "s", RedirectURL: "https://a/cb"}); err == nil {
		t.Fatal("insecure issuer must be refused without AllowInsecureHTTP")
	}
}

var _ = oauth2.GenerateVerifier

func TestAuthMissingIntrospectionRolesDoesNotUseOldJWT(t *testing.T) {
	x := newAuthHarness(t, nil)
	claims := x.iss.accessClaims(authUserGrant("stale", "aiwiki-access"))
	raw := x.iss.sign(x.iss.signer, claims)
	intro := map[string]any{"active": true}
	for k, v := range claims {
		if k != "realm_access" {
			intro[k] = v
		}
	}
	x.iss.mu.Lock()
	x.iss.introspect[raw] = intro
	x.iss.mu.Unlock()
	r := x.do("POST", "/mcp", "{}", map[string]string{"Authorization": "Bearer " + raw})
	if r.Code != http.StatusForbidden {
		t.Fatalf("missing current admission role must deny despite signed old role: %d", r.Code)
	}
}
