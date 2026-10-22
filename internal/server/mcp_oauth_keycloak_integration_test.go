package server

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"html"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"
)

// Optional end-to-end test against a manually started, disposable Keycloak
// 26.8.0 (--features=resource-indicators) that imported
// testdata/keycloak-oauth-realm.json. It needs no real identity provider
// and is skipped unless AIWIKI_TEST_KEYCLOAK_ISSUER and the PostgreSQL test
// environment are set. No object storage is involved (content-only fixture).
const (
	kcRealmPath = "/realms/aiwiki-oauth-test"
	kcOrigin    = "http://127.0.0.1:8090"
	kcResource  = kcOrigin + "/mcp"
	kcCallback  = "http://127.0.0.1:58081/callback"
	kcMaxBody   = 1 << 20

	// Fixture-only credentials; the realm import defines the same values.
	kcBrowserSecret  = "fixture-browser-only"
	kcResourceSecret = "fixture-mcp-only"
)

var kcFormAction = regexp.MustCompile(`(?is)<form\b[^>]*?\baction="([^"]*)"`)

// kcIssuer refuses anything but an HTTP loopback realm URL, so the fixture
// password can never be sent to a remote host.
func kcIssuer(t *testing.T) string {
	t.Helper()
	raw := os.Getenv("AIWIKI_TEST_KEYCLOAK_ISSUER")
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "http" || u.Opaque != "" || u.User != nil || !authLoopbackHost(u.Hostname()) ||
		u.Path != kcRealmPath || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		t.Fatalf("AIWIKI_TEST_KEYCLOAK_ISSUER must be http://<loopback>[:port]%s", kcRealmPath)
	}
	return raw
}

func kcClient(t *testing.T) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Client{Timeout: 5 * time.Second, Jar: jar,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

// kcDo never reports URLs, bodies or wrapped errors: they may hold codes.
func kcDo(t *testing.T, c *http.Client, req *http.Request) (int, http.Header, []byte) {
	t.Helper()
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("keycloak request failed (%s %s)", req.Method, req.URL.Path)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, kcMaxBody))
	if err != nil {
		t.Fatalf("keycloak response unreadable (%s)", req.URL.Path)
	}
	return resp.StatusCode, resp.Header, body
}

// kcUnder reports whether raw is an http(s) URL on the issuer's origin below
// the realm path. Query strings (login form actions carry session data) are
// allowed; userinfo, fragments, opaque forms and dot segments are not.
func kcUnder(issuer, raw string) bool {
	i, err1 := url.Parse(issuer)
	u, err2 := url.Parse(raw)
	if err1 != nil || err2 != nil || u.Opaque != "" || u.User != nil || u.Fragment != "" {
		return false
	}
	for _, seg := range strings.Split(u.Path, "/") {
		if seg == "." || seg == ".." {
			return false
		}
	}
	return u.Scheme == i.Scheme && u.Host == i.Host && strings.HasPrefix(u.Path, i.Path+"/")
}

// kcIntrospect returns the "active" flag of an introspection response for the
// given client credentials. Neither secrets nor tokens are reported.
func kcIntrospect(t *testing.T, ctx context.Context, a *AuthService, id, secret, token string) bool {
	t.Helper()
	form := url.Values{"token": {token}, "token_type_hint": {"access_token"}}
	req, err := http.NewRequestWithContext(ctx, "POST", a.introspectURL, strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	req.SetBasicAuth(url.QueryEscape(id), url.QueryEscape(secret))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	_, _, body := kcDo(t, kcClient(t), req)
	var out struct {
		Active bool `json:"active"`
	}
	if json.Unmarshal(body, &out) != nil {
		return false
	}
	return out.Active
}

func kcRandom(t *testing.T) string {
	t.Helper()
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func kcChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// kcAuthorize drives the fixture login (fresh cookie jar) and returns the
// authorization code from the registered callback redirect, which is never
// followed.
func kcAuthorize(t *testing.T, issuer, authEndpoint, challenge string) string {
	t.Helper()
	c, state := kcClient(t), kcRandom(t)
	q := url.Values{"response_type": {"code"}, "client_id": {"aiwiki-cli"}, "redirect_uri": {kcCallback},
		"scope": {mcpOAuthScope}, "state": {state}, "resource": {kcResource},
		"code_challenge": {challenge}, "code_challenge_method": {"S256"}}
	req, err := http.NewRequest("GET", authEndpoint+"?"+q.Encode(), nil)
	if err != nil {
		t.Fatal(err)
	}
	status, _, page := kcDo(t, c, req)
	if status != 200 {
		t.Fatalf("authorization request: status %d (is resource-indicators enabled and the realm imported?)", status)
	}
	m := kcFormAction.FindSubmatch(page)
	if m == nil {
		t.Fatal("login form not found; browser login is not scriptable here, use a manual check")
	}
	action, err := req.URL.Parse(html.UnescapeString(string(m[1])))
	if err != nil || !kcUnder(issuer, action.String()) {
		t.Fatal("login form action is outside the authorization server")
	}
	form := url.Values{"username": {"employee"}, "password": {"fixture-password-only"}, "credentialId": {""}}
	post, err := http.NewRequest("POST", action.String(), strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	post.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	status, hdr, _ := kcDo(t, c, post)
	if status != 302 {
		t.Fatalf("login submit: status %d, want 302", status)
	}
	loc, err := url.Parse(hdr.Get("Location"))
	if err != nil || loc.Scheme+"://"+loc.Host+loc.Path != kcCallback {
		t.Fatal("login did not redirect to the registered callback")
	}
	got := loc.Query()
	if got.Get("state") != state || got.Get("iss") != issuer || got.Get("code") == "" {
		t.Fatal("callback lacks matching state/iss or code")
	}
	return got.Get("code")
}

// kcToken returns the HTTP status, the access token and the OAuth error name.
func kcToken(t *testing.T, tokenEndpoint, code, verifier, resource string) (int, string, string) {
	t.Helper()
	form := url.Values{"grant_type": {"authorization_code"}, "code": {code}, "code_verifier": {verifier},
		"client_id": {"aiwiki-cli"}, "redirect_uri": {kcCallback}, "resource": {resource}}
	req, err := http.NewRequest("POST", tokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	status, _, body := kcDo(t, kcClient(t), req)
	var out struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("token response is not JSON (status %d)", status)
	}
	return status, out.AccessToken, out.Error
}

func TestMCPOAuthKeycloakIntegration(t *testing.T) {
	if os.Getenv("AIWIKI_TEST_KEYCLOAK_ISSUER") == "" {
		t.Skip("set AIWIKI_TEST_KEYCLOAK_ISSUER to a local Keycloak 26.8.0 realm imported from testdata/keycloak-oauth-realm.json")
	}
	issuer := kcIssuer(t)
	owner, root := corporateFixture(t) // skips without the PostgreSQL test environment
	ctx := context.Background()
	auth, err := newAuthService(ctx, root.DB, CorporateConfig{IssuerURL: issuer, ClientID: "aiwiki-browser",
		ClientSecret: kcBrowserSecret, RedirectURL: kcOrigin + "/auth/callback", AllowInsecureHTTP: true,
		MCPOAuth: MCPOAuthConfig{ResourceURL: kcResource, IntrospectionClientID: kcResource, IntrospectionClientSecret: kcResourceSecret}})
	if err != nil {
		t.Fatalf("auth service: %v", err)
	}
	root.auth = auth
	if err := root.SetAllowedOrigin(kcOrigin); err != nil {
		t.Fatal(err)
	}
	h := root.Handler()
	serve := func(method, path, token, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, kcOrigin+path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		return rr
	}
	rpc := func(token, method string) *httptest.ResponseRecorder {
		return serve("POST", "/mcp", token, `{"jsonrpc":"2.0","id":1,"method":"`+method+`"}`)
	}
	tool := func(token, name string, args map[string]any) (bool, string) {
		raw, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": name, "arguments": args}})
		rr := serve("POST", "/mcp", token, string(raw))
		var env struct {
			Result struct {
				IsError bool `json:"isError"`
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
			} `json:"result"`
		}
		if rr.Code != 200 || json.Unmarshal(rr.Body.Bytes(), &env) != nil || len(env.Result.Content) == 0 {
			t.Fatalf("tool %s: status %d", name, rr.Code)
		}
		return env.Result.IsError, env.Result.Content[0].Text
	}

	// Our own discovery comes first: the client learns the authorization
	// server and scope from the protected resource, not from configuration.
	rr := serve("GET", "/.well-known/oauth-protected-resource/mcp", "", "")
	var meta mcpOAuthMetadata
	if rr.Code != 200 || json.Unmarshal(rr.Body.Bytes(), &meta) != nil {
		t.Fatalf("protected resource metadata: status %d", rr.Code)
	}
	if meta.Resource != kcResource || len(meta.AuthorizationServers) != 1 || meta.AuthorizationServers[0] != issuer || !authScopeContains(strings.Join(meta.ScopesSupported, " "), mcpOAuthScope) {
		t.Fatal("protected resource metadata does not describe the fixture realm")
	}
	if rr = rpc("", "ping"); rr.Code != 401 || !strings.Contains(rr.Header().Get("WWW-Authenticate"), `resource_metadata="`+kcOrigin+`/.well-known/oauth-protected-resource/mcp"`) {
		t.Fatalf("anonymous MCP: status %d, challenge %q", rr.Code, rr.Header().Get("WWW-Authenticate"))
	}

	// Authorization server metadata, then the flow it advertises.
	req, _ := http.NewRequest("GET", issuer+"/.well-known/openid-configuration", nil)
	status, _, body := kcDo(t, kcClient(t), req)
	var as struct {
		Issuer        string   `json:"issuer"`
		AuthEndpoint  string   `json:"authorization_endpoint"`
		TokenEndpoint string   `json:"token_endpoint"`
		PKCE          []string `json:"code_challenge_methods_supported"`
	}
	if status != 200 || json.Unmarshal(body, &as) != nil || as.Issuer != issuer {
		t.Fatalf("authorization server metadata: status %d", status)
	}
	if !strings.Contains(" "+strings.Join(as.PKCE, " ")+" ", " S256 ") || !kcUnder(issuer, as.AuthEndpoint) || !kcUnder(issuer, as.TokenEndpoint) {
		t.Fatal("authorization server does not publish S256 PKCE or endpoints leave the realm")
	}

	verifier := kcRandom(t)
	code := kcAuthorize(t, issuer, as.AuthEndpoint, kcChallenge(verifier))
	status, access, oerr := kcToken(t, as.TokenEndpoint, code, verifier, kcResource)
	if status != 200 || access == "" {
		t.Fatalf("token exchange: status %d error %q", status, oerr)
	}

	verified, err := auth.mcpVerifier.Verify(ctx, access)
	if err != nil {
		t.Fatal("MCP verifier rejected the token (audience is not the resource URL?)")
	}
	var claims map[string]any
	if err := verified.Claims(&claims); err != nil {
		t.Fatal(err)
	}
	if !authAudContains(claims["aud"], kcResource) || !authScopeContains(claims["scope"], mcpOAuthScope) || claims["azp"] != "aiwiki-cli" || claims["typ"] == "ID" {
		t.Fatalf("unexpected token claims: aud/scope/azp/typ mismatch")
	}
	if !authHasRole(authRoles(claims, auth.cfg.RolesClaim), auth.cfg.AccessRole) {
		t.Fatal("access token lacks the realm access role")
	}
	if _, err := auth.introspectAudience(ctx, access, kcResource); err != nil {
		t.Fatalf("introspection with the separate MCP resource client rejected the token: %v", err)
	}
	if !kcIntrospect(t, ctx, auth, kcResource, kcResourceSecret, access) {
		t.Fatal("MCP resource client does not see the token as active")
	}
	// Keycloak only reports a token active to a confidential client named in
	// its audience, so the browser credentials must not be able to use it.
	if kcIntrospect(t, ctx, auth, "aiwiki-browser", kcBrowserSecret, access) {
		t.Fatal("browser client credentials introspect the MCP token as active")
	}
	if _, err := auth.introspectAudience(ctx, access, kcOrigin+"/other"); err == nil {
		t.Fatal("introspection accepted a different audience")
	}

	// The first MCP request lazily creates the local user.
	uid := authUserID(issuer, verified.Subject)
	count := func() (n int) {
		if err := owner.DB.QueryRow(`SELECT count(*) FROM users WHERE id=?`, uid).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if count() != 0 {
		t.Fatal("fixture user exists before first request")
	}
	if rr = rpc(access, "ping"); rr.Code != 200 || count() != 1 {
		t.Fatalf("ping: status %d, users %d", rr.Code, count())
	}
	if rr = rpc(access, "initialize"); rr.Code != 200 || !strings.Contains(rr.Body.String(), `"aiwiki"`) {
		t.Fatalf("initialize: status %d", rr.Code)
	}
	if rr = rpc(access+"x", "ping"); rr.Code != 401 {
		t.Fatalf("tampered token: status %d", rr.Code)
	}

	// Current ACLs: the employee sees only their own article, Bob's stays hidden.
	emp, bob := corporateAs(t, root, uid), corporateAs(t, root, "bob")
	var mine, theirs Article
	for _, c := range []struct {
		s     *Server
		title string
		out   *Article
	}{{emp, "Employee fixture note", &mine}, {bob, "Bob fixture secret", &theirs}} {
		space := corporateCreateSpace(t, c.s)
		r := corporateHTTP(c.s, "POST", "/api/articles", `{"space_id":"`+space+`","title":"`+c.title+`","body":"fixture body"}`)
		if r.Code != 201 || json.Unmarshal(r.Body.Bytes(), c.out) != nil {
			t.Fatalf("seed %q: status %d", c.title, r.Code)
		}
	}
	isErr, text := tool(access, "wiki_search", map[string]any{"q": "fixture"})
	if isErr || !strings.Contains(text, mine.ID) || strings.Contains(text, theirs.ID) || strings.Contains(text, "Bob fixture secret") {
		t.Fatal("wiki_search did not return exactly the employee's own articles")
	}
	isErr, text = tool(access, "wiki_write", map[string]any{"article": map[string]any{"space_id": mine.SpaceID, "title": "Written over OAuth", "body": "oauth body", "author": "forged"}})
	var written struct {
		ID string `json:"id"`
	}
	if isErr || json.Unmarshal([]byte(text), &written) != nil || written.ID == "" {
		t.Fatal("wiki_write failed for the employee's own space")
	}
	var author string
	authorDB, authorDone, err := owner.DB.scoped(ctx, uid)
	if err != nil {
		t.Fatal(err)
	}
	defer authorDone()
	if err := authorDB.QueryRow(`SELECT author FROM revisions WHERE article_id=? AND revision=1`, written.ID).Scan(&author); err != nil || author != uid {
		t.Fatalf("author %q, want the authenticated user id (err %v)", author, err)
	}
	if _, err := owner.DB.Exec(`UPDATE users SET disabled=1 WHERE id=?`, uid); err != nil {
		t.Fatal(err)
	}
	if rr = rpc(access, "ping"); rr.Code != 403 {
		t.Fatalf("disabled user: status %d, want 403", rr.Code)
	}

	// Negative grants, each on a fresh code. Replay is last because a reused
	// code may legitimately revoke the session behind the first token.
	wrongVerifier := kcRandom(t)
	if status, tok, _ := kcToken(t, as.TokenEndpoint, kcAuthorize(t, issuer, as.AuthEndpoint, kcChallenge(verifier)), wrongVerifier, kcResource); status != 400 || tok != "" {
		t.Fatalf("wrong PKCE verifier: status %d, want 400", status)
	}
	if status, tok, oerr := kcToken(t, as.TokenEndpoint, kcAuthorize(t, issuer, as.AuthEndpoint, kcChallenge(verifier)), verifier, kcOrigin+"/other"); status != 400 || tok != "" || oerr != "invalid_target" {
		t.Fatalf("wrong resource: status %d error %q, want 400 invalid_target", status, oerr)
	}
	if status, tok, _ := kcToken(t, as.TokenEndpoint, code, verifier, kcResource); status != 400 || tok != "" {
		t.Fatalf("authorization code replay: status %d, want 400", status)
	}
}
