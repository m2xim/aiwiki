package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

const mcpOAuthTestSecret = "s3cret-value"

func TestMCPOAuthIntrospectionConfiguration(t *testing.T) {
	for _, tc := range []struct {
		id, secret, resource string
		valid                bool
	}{
		{"", "", "https://wiki.example.com/mcp", true},
		{"resource", "secret", "https://wiki.example.com/mcp", true},
		{"resource", "", "https://wiki.example.com/mcp", false},
		{"", "secret", "https://wiki.example.com/mcp", false},
		{"resource", "secret", "", false},
		{" resource", "secret", "https://wiki.example.com/mcp", false},
		{"resource", "   ", "https://wiki.example.com/mcp", false},
	} {
		cfg := mcpOAuthTestConfig(tc.resource)
		cfg.MCPOAuth.IntrospectionClientID, cfg.MCPOAuth.IntrospectionClientSecret = tc.id, tc.secret
		_, err := normalizeMCPOAuthConfig(cfg)
		if (err == nil) != tc.valid {
			t.Fatalf("unexpected credential validation result: valid=%v", tc.valid)
		}
	}
}

func mcpOAuthTestConfig(resource string) CorporateConfig {
	return CorporateConfig{
		IssuerURL:    "https://id.example.com/realms/wiki",
		ClientID:     "aiwiki",
		ClientSecret: mcpOAuthTestSecret,
		RedirectURL:  "https://wiki.example.com/auth/callback",
		MCPOAuth:     MCPOAuthConfig{ResourceURL: resource},
	}
}

func mcpOAuthTestService(t *testing.T, cfg CorporateConfig) *AuthService {
	t.Helper()
	cfg, err := normalizeMCPOAuthConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return &AuthService{cfg: cfg}
}

func mcpOAuthServe(a *AuthService, method, target string, hdr map[string]string) *httptest.ResponseRecorder {
	mux := http.NewServeMux()
	a.mcpOAuthRoutes(mux)
	req := httptest.NewRequest(method, target, nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func TestMCPOAuthNormalizeDisabled(t *testing.T) {
	for _, v := range []string{""} {
		cfg := mcpOAuthTestConfig(v)
		got, err := normalizeMCPOAuthConfig(cfg)
		if err != nil || !reflect.DeepEqual(got, cfg) {
			t.Fatalf("%q: got %+v err %v", v, got, err)
		}
	}
	if _, err := normalizeMCPOAuthConfig(mcpOAuthTestConfig("   ")); err == nil {
		t.Fatal("whitespace-only resource must not produce inconsistent enabled/disabled modes")
	}
}

func TestMCPOAuthNormalizeValid(t *testing.T) {
	cases := []struct {
		name, in, redirect string
		insecure           bool
		want               string
	}{
		{"plain", "https://wiki.example.com/mcp", "", false, "https://wiki.example.com/mcp"},
		{"case and default port", " HTTPS://Wiki.Example.COM:443/mcp ", "", false, "https://wiki.example.com/mcp"},
		{"custom port", "https://wiki.example.com:8443/mcp", "https://wiki.example.com:8443/auth/callback", false, "https://wiki.example.com:8443/mcp"},
		{"redirect default port", "https://wiki.example.com/mcp", "https://WIKI.example.com:443/auth/callback", false, "https://wiki.example.com/mcp"},
		{"loopback http", "http://127.0.0.1:8080/mcp", "http://127.0.0.1:8080/auth/callback", true, "http://127.0.0.1:8080/mcp"},
		{"localhost http default port", "http://LOCALHOST:80/mcp", "http://localhost/auth/callback", true, "http://localhost/mcp"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := mcpOAuthTestConfig(c.in)
			if c.redirect != "" {
				cfg.RedirectURL = c.redirect
			}
			cfg.AllowInsecureHTTP = c.insecure
			orig := cfg
			got, err := normalizeMCPOAuthConfig(cfg)
			if err != nil {
				t.Fatal(err)
			}
			if got.MCPOAuth.ResourceURL != c.want {
				t.Fatalf("got %q want %q", got.MCPOAuth.ResourceURL, c.want)
			}
			got.MCPOAuth = orig.MCPOAuth
			if !reflect.DeepEqual(got, orig) {
				t.Fatal("normalize changed unrelated config")
			}
		})
	}
}

func TestMCPOAuthNormalizeInvalid(t *testing.T) {
	cases := []struct {
		name, in, redirect string
		insecure           bool
	}{
		{"cross origin host", "https://evil.example.com/mcp", "", false},
		{"cross origin port", "https://wiki.example.com:8443/mcp", "", false},
		{"cross origin scheme", "http://wiki.example.com/mcp", "", true},
		{"query", "https://wiki.example.com/mcp?a=b", "", false},
		{"empty query", "https://wiki.example.com/mcp?", "", false},
		{"fragment", "https://wiki.example.com/mcp#x", "", false},
		{"userinfo", "https://user:pw@wiki.example.com/mcp", "", false},
		{"raw path", "https://wiki.example.com/m%63p", "", false},
		{"encoded slash", "https://wiki.example.com/mcp%2f", "", false},
		{"trailing slash", "https://wiki.example.com/mcp/", "", false},
		{"no path", "https://wiki.example.com", "", false},
		{"root path", "https://wiki.example.com/", "", false},
		{"wrong path", "https://wiki.example.com/other", "", false},
		{"sub path", "https://wiki.example.com/x/mcp", "", false},
		{"case path", "https://wiki.example.com/MCP", "", false},
		{"dot segment", "https://wiki.example.com/a/../mcp", "", false},
		{"backslash", `https://wiki.example.com\mcp`, "", false},
		{"opaque", "https:wiki.example.com/mcp", "", false},
		{"relative", "/mcp", "", false},
		{"scheme relative", "//wiki.example.com/mcp", "", false},
		{"ftp", "ftp://wiki.example.com/mcp", "", false},
		{"plain http without allow", "http://127.0.0.1:8080/mcp", "http://127.0.0.1:8080/auth/callback", false},
		{"plain http non loopback", "http://wiki.example.com/mcp", "http://wiki.example.com/auth/callback", true},
		{"unicode host", "https://wíki.example.com/mcp", "https://wíki.example.com/auth/callback", false},
		{"idn host", "https://münchen.example/mcp", "https://xn--mnchen-3ya.example/auth/callback", false},
		{"space in host", "https://wiki .example.com/mcp", "", false},
		{"crlf suffix", "https://wiki.example.com/mcp\r\nX-Evil: 1", "", false},
		{"encoded crlf", "https://wiki.example.com/mcp%0d%0aX: 1", "", false},
		{"nul", "https://wiki.example.com/mcp\x00", "", false},
		{"tab", "https://wiki.example.com/\tmcp", "", false},
		{"del", "https://wiki.example.com/mcp\x7f", "", false},
		{"empty port", "https://wiki.example.com:/mcp", "", false},
		{"bad port", "https://wiki.example.com:99999/mcp", "", false},
		{"too long", "https://wiki.example.com/mcp?" + strings.Repeat("a", mcpOAuthMaxURL), "", false},
		{"long host", "https://" + strings.Repeat("a", mcpOAuthMaxURL) + ".example.com/mcp", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := mcpOAuthTestConfig(c.in)
			if c.redirect != "" {
				cfg.RedirectURL = c.redirect
			}
			cfg.AllowInsecureHTTP = c.insecure
			_, err := normalizeMCPOAuthConfig(cfg)
			if err == nil {
				t.Fatal("expected error")
			}
			msg := err.Error()
			if strings.Contains(msg, mcpOAuthTestSecret) || strings.Contains(msg, "example") || strings.Contains(msg, "127.0.0.1") {
				t.Fatalf("error leaks configuration: %q", msg)
			}
		})
	}
}

func TestMCPOAuthMetadataPublic(t *testing.T) {
	cfg := mcpOAuthTestConfig("HTTPS://Wiki.Example.com:443/mcp")
	a := mcpOAuthTestService(t, cfg)
	want := mcpOAuthMetadata{
		Resource:               "https://wiki.example.com/mcp",
		AuthorizationServers:   []string{cfg.IssuerURL},
		ScopesSupported:        []string{"aiwiki:mcp"},
		BearerMethodsSupported: []string{"header"},
	}
	for _, path := range []string{"/.well-known/oauth-protected-resource", "/.well-known/oauth-protected-resource/mcp"} {
		rec := mcpOAuthServe(a, "GET", path, map[string]string{
			"Host":              "attacker.example",
			"X-Forwarded-Host":  "attacker.example",
			"X-Forwarded-Proto": "http",
			"Forwarded":         "host=attacker.example;proto=http",
			"Cookie":            "aiwiki_session=x",
			"Authorization":     "Bearer abc",
		})
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status %d", path, rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
			t.Fatalf("content type %q", ct)
		}
		if cc := rec.Header().Get("Cache-Control"); cc != "public, max-age=300" {
			t.Fatalf("cache control %q", cc)
		}
		if _, ok := rec.Header()["Set-Cookie"]; ok {
			t.Fatal("metadata set a cookie")
		}
		var got mcpOAuthMetadata
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("got %+v want %+v", got, want)
		}
		var raw map[string]any
		json.Unmarshal(rec.Body.Bytes(), &raw)
		for k := range raw {
			if strings.Contains(k, "endpoint") || k == "jwks_uri" {
				t.Fatalf("metadata advertises authorization server field %q", k)
			}
		}
		if strings.Contains(rec.Body.String(), "attacker") {
			t.Fatal("metadata reflects request headers")
		}
	}
}

func TestMCPOAuthMetadataFeatureOff(t *testing.T) {
	a := mcpOAuthTestService(t, mcpOAuthTestConfig(""))
	for _, path := range []string{"/.well-known/oauth-protected-resource", "/.well-known/oauth-protected-resource/mcp"} {
		for _, m := range []string{"GET", "HEAD"} {
			rec := mcpOAuthServe(a, m, path, nil)
			if rec.Code != http.StatusNotFound {
				t.Fatalf("%s %s: status %d", m, path, rec.Code)
			}
			if _, ok := rec.Header()["Set-Cookie"]; ok {
				t.Fatal("404 set a cookie")
			}
		}
	}
}

func TestMCPOAuthMetadataMethods(t *testing.T) {
	a := mcpOAuthTestService(t, mcpOAuthTestConfig("https://wiki.example.com/mcp"))
	path := "/.well-known/oauth-protected-resource/mcp"
	// Go ServeMux: a "GET" pattern also matches HEAD.
	rec := mcpOAuthServe(a, "HEAD", path, nil)
	if rec.Code != http.StatusOK || rec.Body.Len() != 0 {
		t.Fatalf("HEAD: status %d body %q", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Cache-Control") != "public, max-age=300" {
		t.Fatal("HEAD lost cache headers")
	}
	for _, m := range []string{"POST", "PUT", "DELETE", "PATCH"} {
		rec := mcpOAuthServe(a, m, path, nil)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("%s: status %d", m, rec.Code)
		}
		allow := rec.Header().Get("Allow")
		if !strings.Contains(allow, "GET") || !strings.Contains(allow, "HEAD") {
			t.Fatalf("%s: Allow %q", m, allow)
		}
	}
	for _, p := range []string{
		"/.well-known/oauth-protected-resource/other",
		"/.well-known/oauth-protected-resource/mcp/",
		"/mcp/.well-known/oauth-protected-resource",
		"/.well-known/oauth-authorization-server",
		"/.well-known/openid-configuration",
	} {
		if rec := mcpOAuthServe(a, "GET", p, nil); rec.Code != http.StatusNotFound {
			t.Fatalf("%s: status %d", p, rec.Code)
		}
	}
}

func TestMCPOAuthChallenge(t *testing.T) {
	off := mcpOAuthTestService(t, mcpOAuthTestConfig(""))
	offCases := map[string]string{
		"":                   "Bearer",
		"invalid_token":      `Bearer error="invalid_token"`,
		"insufficient_scope": `Bearer error="insufficient_scope"`,
		`x", scope="evil`:    "Bearer",
		"invalid_request":    "Bearer",
	}
	for in, want := range offCases {
		if got := off.mcpBearerChallenge(in); got != want {
			t.Fatalf("off %q: got %q want %q", in, got, want)
		}
	}

	on := mcpOAuthTestService(t, mcpOAuthTestConfig("HTTPS://WIKI.example.com:443/mcp"))
	const meta = `resource_metadata="https://wiki.example.com/.well-known/oauth-protected-resource/mcp", scope="aiwiki:mcp"`
	onCases := map[string]string{
		"":                   "Bearer " + meta,
		"invalid_token":      `Bearer error="invalid_token", ` + meta,
		"insufficient_scope": `Bearer error="insufficient_scope", ` + meta,
		"invalid_token\r\nX": "Bearer " + meta,
		`"; evil="1`:         "Bearer " + meta,
		"server_error":       "Bearer " + meta,
	}
	for in, want := range onCases {
		got := on.mcpBearerChallenge(in)
		if got != want {
			t.Fatalf("on %q: got %q want %q", in, got, want)
		}
		if strings.ContainsAny(got, "\r\n") {
			t.Fatal("challenge contains CR/LF")
		}
	}
}

func TestMCPOAuthChallengeIgnoresRequest(t *testing.T) {
	// The helper takes no request, so Host and forwarding headers cannot reach
	// it; the metadata URL is the static configured origin.
	a := mcpOAuthTestService(t, mcpOAuthTestConfig("https://wiki.example.com/mcp"))
	got := a.mcpBearerChallenge("invalid_token")
	if strings.Contains(got, "attacker") || !strings.Contains(got, `"https://wiki.example.com/.well-known/oauth-protected-resource/mcp"`) {
		t.Fatalf("unexpected challenge %q", got)
	}
	// An unnormalized/invalid configuration degrades to the legacy challenge.
	bad := &AuthService{cfg: mcpOAuthTestConfig("https://evil.example.com/mcp")}
	if got := bad.mcpBearerChallenge("invalid_token"); got != `Bearer error="invalid_token"` {
		t.Fatalf("invalid config challenge %q", got)
	}
	if rec := mcpOAuthServe(bad, "GET", "/.well-known/oauth-protected-resource", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("invalid config metadata status %d", rec.Code)
	}
}

func TestMCPOAuthScopeContains(t *testing.T) {
	const want = mcpOAuthScope
	yes := []string{
		"aiwiki:mcp",
		"openid aiwiki:mcp",
		"openid  aiwiki:mcp  profile",
		" aiwiki:mcp ",
		"a aiwiki:mcp b",
	}
	for _, s := range yes {
		if !authScopeContains(s, want) {
			t.Fatalf("%q should contain scope", s)
		}
	}
	no := []any{
		nil,
		42,
		3.5,
		true,
		[]string{"aiwiki:mcp"},
		[]any{"aiwiki:mcp"},
		map[string]any{"aiwiki:mcp": true},
		"",
		"   ",
		"aiwiki:mcpx",
		"xaiwiki:mcp",
		"aiwiki:mc",
		"AIWIKI:MCP",
		"openid\naiwiki:mcp",
		"openid\r\naiwiki:mcp",
		"openid\taiwiki:mcp",
		"aiwiki:mcp\x00",
		"aiwiki:mcp\x7f",
		`aiwiki:mcp "quoted"`,
		`aiwiki:mcp back\slash`,
		"aiwiki:mcp é",
		"aiwiki:mcp\u00a0openid",
		"aiwiki:mcp,openid",
	}
	for _, v := range no {
		if authScopeContains(v, want) {
			t.Fatalf("%#v must not contain scope", v)
		}
	}
	for _, w := range []string{"", "a b", `a"b`, "a\nb"} {
		if authScopeContains("a b", w) {
			t.Fatalf("invalid wanted scope %q accepted", w)
		}
	}
}
