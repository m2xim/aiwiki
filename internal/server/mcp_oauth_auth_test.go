package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestMCPOAuthIntrospectionCredentialSeparation(t *testing.T) {
	var gotID, gotSecret string
	issuer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, secret, ok := r.BasicAuth()
		if !ok {
			t.Error("missing client authentication")
		}
		gotID, gotSecret = id, secret
		json.NewEncoder(w).Encode(map[string]any{"active": true, "iss": "https://issuer.example.org", "aud": []string{"browser", oauthTestResource}, "sub": "user", "exp": time.Now().Add(time.Minute).Unix()})
	}))
	defer issuer.Close()
	a := &AuthService{cfg: CorporateConfig{IssuerURL: "https://issuer.example.org", ClientID: "browser", ClientSecret: "browser-only", MCPOAuth: MCPOAuthConfig{ResourceURL: oauthTestResource, IntrospectionClientID: "resource", IntrospectionClientSecret: "resource-only"}}, client: issuer.Client(), introspectURL: issuer.URL, now: time.Now}
	if _, err := a.introspectAudience(context.Background(), "fixture-token", oauthTestResource); err != nil {
		t.Fatal(err)
	}
	if gotID != "resource" || gotSecret != "resource-only" {
		t.Fatal("MCP did not use resource credentials")
	}
	if _, err := a.introspect(context.Background(), "fixture-token"); err != nil {
		t.Fatal(err)
	}
	if gotID != "browser" || gotSecret != "browser-only" {
		t.Fatal("browser did not keep its own credentials")
	}
}

const oauthTestResource = "http://127.0.0.1:8080/mcp"

func newMCPOAuthHarness(t *testing.T) *authHarness {
	return newAuthHarness(t, func(c *CorporateConfig) { c.MCPOAuth.ResourceURL = oauthTestResource })
}

func oauthResourceGrant(sub string) authGrant {
	g := authUserGrant(sub, "aiwiki-access")
	g.claims["aud"] = oauthTestResource
	g.claims["scope"] = mcpOAuthScope
	return g
}

func TestMCPOAuthBearerBinding(t *testing.T) {
	x := newMCPOAuthHarness(t)
	for _, tc := range []struct {
		name       string
		modify     func(*authGrant)
		status     int
		scopeError bool
	}{
		{"resource audience", nil, 200, false},
		{"resource and browser", func(g *authGrant) { g.claims["aud"] = []string{oauthTestResource, authTestClientID} }, 200, false},
		{"browser only", func(g *authGrant) { g.claims["aud"] = authTestClientID }, 401, false},
		{"other resource", func(g *authGrant) { g.claims["aud"] = "https://other.example.org/mcp" }, 401, false},
		{"path mismatch", func(g *authGrant) { g.claims["aud"] = oauthTestResource + "/" }, 401, false},
		{"missing signed scope", func(g *authGrant) { delete(g.claims, "scope") }, 403, true},
		{"scope substring", func(g *authGrant) { g.claims["scope"] = "not-" + mcpOAuthScope }, 403, true},
		{"scope wrong type", func(g *authGrant) { g.claims["scope"] = []string{mcpOAuthScope} }, 403, true},
		{"wrong issuer", func(g *authGrant) { g.claims["iss"] = "https://other.example.org" }, 401, false},
		{"expired", func(g *authGrant) { g.claims["exp"] = time.Now().Add(-time.Minute).Unix() }, 401, false},
		{"ID token", func(g *authGrant) { g.claims["typ"] = "ID" }, 401, false},
		{"no admission", func(g *authGrant) { g.claims["realm_access"] = map[string]any{"roles": []string{"aiwiki-admin"}} }, 403, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := oauthResourceGrant("oauth-user")
			if tc.modify != nil {
				tc.modify(&g)
			}
			token := x.iss.bearer(g)
			w := x.do("POST", "/mcp", "", map[string]string{"Authorization": "Bearer " + token})
			if w.Code != tc.status {
				t.Fatalf("status=%d want=%d body=%s", w.Code, tc.status, w.Body)
			}
			challenge := w.Header().Get("WWW-Authenticate")
			if tc.status == 401 || tc.scopeError {
				if !strings.Contains(challenge, `resource_metadata="http://127.0.0.1:8080/.well-known/oauth-protected-resource/mcp"`) {
					t.Fatalf("missing discovery: %q", challenge)
				}
			}
			if strings.Contains(challenge, `error="insufficient_scope"`) != tc.scopeError {
				t.Fatalf("wrong scope challenge: %q", challenge)
			}
		})
	}
}

func TestMCPOAuthLiveIntrospection(t *testing.T) {
	x := newMCPOAuthHarness(t)
	token := x.iss.bearer(oauthResourceGrant("oauth-user"))
	request := func() *http.Response {
		return x.do("POST", "/mcp", "", map[string]string{"Authorization": "Bearer " + token}).Result()
	}
	if r := request(); r.StatusCode != 200 {
		t.Fatal(r.StatusCode)
	}
	for _, scope := range []any{"", []any{mcpOAuthScope}, "not-" + mcpOAuthScope} {
		x.iss.setRoles(token, map[string]any{"scope": scope})
		if r := request(); r.StatusCode != 403 || !strings.Contains(r.Header.Get("WWW-Authenticate"), "insufficient_scope") {
			t.Fatalf("live scope accepted: %d", r.StatusCode)
		}
	}
	x.iss.setRoles(token, map[string]any{"scope": mcpOAuthScope, "aud": authTestClientID})
	if r := request(); r.StatusCode != 401 {
		t.Fatalf("live audience accepted: %d", r.StatusCode)
	}
	x.iss.setRoles(token, map[string]any{"aud": oauthTestResource})
	x.iss.setDown(true)
	if r := request(); r.StatusCode != 503 {
		t.Fatalf("IdP outage status=%d", r.StatusCode)
	}
	x.iss.setDown(false)
	x.iss.setActive(token, false)
	if r := request(); r.StatusCode != 401 {
		t.Fatalf("inactive token status=%d", r.StatusCode)
	}
}

func TestMCPOAuthKeepsBrowserAudienceSeparate(t *testing.T) {
	x := newMCPOAuthHarness(t)
	cookie := authCookie(x.login(authUserGrant("browser-user", "aiwiki-access")), x.a.sessionCookie)
	if cookie == nil {
		t.Fatal("browser login failed in resource audience mode")
	}
	if w := x.do("GET", "/api/me", "", nil, cookie); w.Code != 200 {
		t.Fatal(w.Body)
	}
	if w := x.do("POST", "/mcp", "", nil, cookie); w.Code != 401 {
		t.Fatal("browser cookie authenticated MCP")
	}
	token := x.iss.bearer(oauthResourceGrant("oauth-user"))
	if w := x.do("GET", "/api/echo", "", map[string]string{"Authorization": "Bearer " + token}); w.Code != 401 {
		t.Fatal("resource bearer authenticated browser API")
	}
	x.users.disable(x.iss.srv.URL, "oauth-user")
	if w := x.do("POST", "/mcp", "", map[string]string{"Authorization": "Bearer " + token}); w.Code != 403 {
		t.Fatalf("local disabled user=%d", w.Code)
	}
}
