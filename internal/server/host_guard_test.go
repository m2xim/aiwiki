package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const mcpWriteBody = `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"wiki_write","arguments":{"article":{"space_id":"general","title":"rebound","kind":"article","status":"draft"}}}}`

// hostRequest builds a request with an explicit Host and optional Origin; the
// httptest default of example.com is never relied upon.
func hostRequest(method, target, host, origin, body string) *http.Request {
	r := httptest.NewRequest(method, target, strings.NewReader(body))
	r.Host = host
	if origin != "" {
		r.Header.Set("Origin", origin)
	}
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	return r
}

func serve(s *Server, r *http.Request) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	return w
}

func TestForeignHostRejectedWithoutOrigin(t *testing.T) {
	hosts := []string{
		"attacker.example", "attacker.example:8090", "example.com", "127.0.0.1.attacker.example",
		"localhost.attacker.example", "evil.localhost", "192.0.2.10:8090", "",
		"localhost@attacker.example", "attacker.example@localhost", "localhost:8090/x", "localhost/",
		"[::1", "::1", "::1:8090", "[::1]x", "localhost:", "localhost:abc", "localhost:0", "localhost:99999",
		"localhost:80:80", "local host", "localhost%00", "[fe80::1%25eth0]",
	}
	for _, configured := range []string{"", "https://wiki.example.test:8443"} {
		s := testServer(t)
		if configured != "" {
			if err := s.SetAllowedOrigin(configured); err != nil {
				t.Fatal(err)
			}
		}
		for _, host := range hosts {
			for _, c := range []struct{ method, path, body string }{
				{http.MethodGet, "/api/export", ""},
				{http.MethodGet, "/api/spaces", ""},
				{http.MethodGet, "/api/health", ""},
				{http.MethodPost, "/mcp", mcpWriteBody},
				{http.MethodOptions, "/api/spaces", ""},
			} {
				r := hostRequest(c.method, c.path, host, "", c.body)
				r.Header.Set("X-Forwarded-Host", "localhost")
				w := serve(s, r)
				if w.Code != http.StatusForbidden {
					t.Errorf("configured=%q Host=%q %s %s: status %d, want 403", configured, host, c.method, c.path, w.Code)
				}
				if strings.Contains(w.Body.String(), "start-here") {
					t.Errorf("Host=%q %s %s leaked article data", host, c.method, c.path)
				}
			}
		}
		var n int
		if err := s.DB.QueryRow("SELECT count(*) FROM articles WHERE title='rebound'").Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Fatalf("foreign Host created %d articles over MCP", n)
		}
	}
}

func TestForwardedHostIsNotTrusted(t *testing.T) {
	s := testServer(t)
	r := hostRequest(http.MethodGet, "/api/export", "attacker.example", "", "")
	r.Header.Set("X-Forwarded-Host", "localhost:8090")
	r.Header.Set("Forwarded", "host=localhost:8090")
	if w := serve(s, r); w.Code != http.StatusForbidden {
		t.Fatalf("forwarded header made a foreign Host acceptable: %d", w.Code)
	}
	r = hostRequest(http.MethodGet, "/api/health", "localhost:8090", "", "")
	r.Header.Set("X-Forwarded-Host", "attacker.example")
	if w := serve(s, r); w.Code != http.StatusOK {
		t.Fatalf("loopback Host with forwarded header: %d", w.Code)
	}
}

func TestLoopbackHostsAccepted(t *testing.T) {
	s := testServer(t)
	for _, host := range []string{
		"localhost", "localhost:8090", "LOCALHOST:3000", "127.0.0.1", "127.0.0.1:8090", "127.5.5.5:1",
		"[::1]", "[::1]:8090", "[0:0:0:0:0:0:0:1]:8090",
	} {
		for _, path := range []string{"/api/health", "/api/export", "/api/spaces"} {
			if w := serve(s, hostRequest(http.MethodGet, path, host, "", "")); w.Code != http.StatusOK {
				t.Errorf("Host=%q %s: status %d, want 200 (%s)", host, path, w.Code, w.Body.String())
			}
		}
		if w := serve(s, hostRequest(http.MethodPost, "/mcp", host, "", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)); w.Code != http.StatusOK {
			t.Errorf("Host=%q /mcp: status %d, want 200", host, w.Code)
		}
	}
}

func TestConfiguredHostAndEffectivePort(t *testing.T) {
	cases := []struct {
		configured string
		allowed    []string
		rejected   []string
	}{
		{"https://wiki.example.test", []string{"wiki.example.test", "wiki.example.test:443", "WIKI.Example.Test"}, []string{"wiki.example.test:80", "wiki.example.test:8443", "other.example.test", "sub.wiki.example.test"}},
		{"http://wiki.example.test", []string{"wiki.example.test", "wiki.example.test:80", "wiki.example.test:080"}, []string{"wiki.example.test:443", "wiki.example.test:8090"}},
		{"https://wiki.example.test:443", []string{"wiki.example.test", "wiki.example.test:443"}, []string{"wiki.example.test:8443"}},
		{"http://wiki.example.test:80", []string{"wiki.example.test", "wiki.example.test:80"}, []string{"wiki.example.test:81"}},
		{"https://wiki.example.test:8443", []string{"wiki.example.test:8443"}, []string{"wiki.example.test", "wiki.example.test:443", "wiki.example.test:9443"}},
		{"http://192.0.2.10:8090", []string{"192.0.2.10:8090"}, []string{"192.0.2.10", "192.0.2.10:80", "192.0.2.11:8090"}},
		{"http://[2001:db8::1]:8090", []string{"[2001:db8::1]:8090", "[2001:DB8:0::1]:8090"}, []string{"[2001:db8::1]", "[2001:db8::2]:8090", "2001:db8::1"}},
	}
	for _, c := range cases {
		s := testServer(t)
		if err := s.SetAllowedOrigin(c.configured); err != nil {
			t.Fatal(err)
		}
		for _, host := range c.allowed {
			if w := serve(s, hostRequest(http.MethodGet, "/api/export", host, "", "")); w.Code != http.StatusOK {
				t.Errorf("configured=%q Host=%q: status %d, want 200", c.configured, host, w.Code)
			}
		}
		for _, host := range c.rejected {
			if w := serve(s, hostRequest(http.MethodGet, "/api/export", host, "", "")); w.Code != http.StatusForbidden {
				t.Errorf("configured=%q Host=%q: status %d, want 403", c.configured, host, w.Code)
			}
		}
		// Loopback stays reachable locally whatever is configured.
		if w := serve(s, hostRequest(http.MethodGet, "/api/health", "localhost:8090", "", "")); w.Code != http.StatusOK {
			t.Errorf("configured=%q: loopback Host rejected: %d", c.configured, w.Code)
		}
	}
	// Without a configured origin a named host is not trusted at all.
	s := testServer(t)
	if w := serve(s, hostRequest(http.MethodGet, "/api/export", "wiki.example.test", "", "")); w.Code != http.StatusForbidden {
		t.Errorf("unconfigured named Host: status %d, want 403", w.Code)
	}
}

func TestAllowedOriginCanonicalization(t *testing.T) {
	cases := []struct {
		configured string
		canonical  string
		host       string
		origin     string
	}{
		{"HTTPS://Wiki.Example.Test:443", "https://wiki.example.test", "wiki.example.test", "https://wiki.example.test"},
		{"HTTP://Wiki.Example.Test:80", "http://wiki.example.test", "wiki.example.test", "http://wiki.example.test"},
		{"HTTPS://[2001:0DB8:0:0::1]:443", "https://[2001:db8::1]", "[2001:db8::1]", "https://[2001:db8::1]"},
	}
	for _, c := range cases {
		s := testServer(t)
		if err := s.SetAllowedOrigin(c.configured); err != nil {
			t.Fatalf("SetAllowedOrigin(%q): %v", c.configured, err)
		}
		if s.allowedOrigin != c.canonical {
			t.Errorf("configured=%q canonical=%q, want %q", c.configured, s.allowedOrigin, c.canonical)
		}
		w := serve(s, hostRequest(http.MethodGet, "/api/health", c.host, c.origin, ""))
		if w.Code != http.StatusOK || w.Header().Get("Access-Control-Allow-Origin") != c.origin {
			t.Errorf("configured=%q Host=%q Origin=%q: status %d allow-origin=%q", c.configured, c.host, c.origin, w.Code, w.Header().Get("Access-Control-Allow-Origin"))
		}
	}
}

func TestAllowedOriginRejectsInvalidAuthority(t *testing.T) {
	for _, origin := range []string{
		"http://wiki.example.test:0", "http://wiki.example.test:99999", "http://wiki.example.test:",
		"http://[fe80::1%25eth0]",
	} {
		if err := ValidateAllowedOrigin(origin); err == nil {
			t.Errorf("ValidateAllowedOrigin(%q) succeeded, want error", origin)
		}
	}
}

func TestMCPWriteSucceedsForLoopbackAndConfiguredRemoteHost(t *testing.T) {
	s := testServer(t)
	if err := s.SetAllowedOrigin("HTTPS://Wiki.Example.Test:443"); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		host, origin, body string
	}{
		{"localhost:8090", "", mcpWriteBody},
		{"wiki.example.test", "https://wiki.example.test", strings.Replace(mcpWriteBody, `"title":"rebound"`, `"title":"remote rebound"`, 1)},
		{"wiki.example.test", "", strings.Replace(mcpWriteBody, `"title":"rebound"`, `"title":"remote tokenless"`, 1)},
	}
	for _, c := range cases {
		w := serve(s, hostRequest(http.MethodPost, "/mcp", c.host, c.origin, c.body))
		if w.Code != http.StatusOK {
			t.Fatalf("Host=%q Origin=%q: status %d: %s", c.host, c.origin, w.Code, w.Body.String())
		}
		var response struct {
			Result struct {
				IsError bool `json:"isError"`
			} `json:"result"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
			t.Fatalf("Host=%q invalid MCP response: %v (%s)", c.host, err, w.Body.String())
		}
		if response.Result.IsError {
			t.Fatalf("Host=%q wiki_write returned isError=true: %s", c.host, w.Body.String())
		}
	}
	var n int
	if err := s.DB.QueryRow("SELECT count(*) FROM articles WHERE title IN ('rebound', 'remote rebound', 'remote tokenless')").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != len(cases) {
		t.Fatalf("wiki_write created %d articles, want %d", n, len(cases))
	}
}

func TestDevOriginsOnlyForLoopbackHost(t *testing.T) {
	const remote = "https://wiki.example.test:8443"
	s := testServer(t)
	if err := s.SetAllowedOrigin(remote); err != nil {
		t.Fatal(err)
	}
	devOrigins := []string{"http://localhost:8090", "http://localhost:5173", "http://127.0.0.1:4173", "http://[::1]:18088"}
	for _, host := range []string{"localhost:8090", "127.0.0.1:8090", "[::1]:8090"} {
		for _, origin := range devOrigins {
			w := serve(s, hostRequest(http.MethodGet, "/api/health", host, origin, ""))
			if w.Code != http.StatusOK || w.Header().Get("Access-Control-Allow-Origin") != origin {
				t.Errorf("loopback Host=%q Origin=%q: status %d allow-origin=%q", host, origin, w.Code, w.Header().Get("Access-Control-Allow-Origin"))
			}
		}
		for _, origin := range []string{"http://localhost:9999", "https://localhost:5173", "https://attacker.example", "null"} {
			if w := serve(s, hostRequest(http.MethodGet, "/api/health", host, origin, "")); w.Code != http.StatusForbidden {
				t.Errorf("loopback Host=%q foreign Origin=%q: status %d, want 403", host, origin, w.Code)
			}
		}
	}
	// The configured remote host accepts only its exact allowed Origin.
	if w := serve(s, hostRequest(http.MethodGet, "/api/health", "wiki.example.test:8443", remote, "")); w.Code != http.StatusOK || w.Header().Get("Access-Control-Allow-Origin") != remote {
		t.Errorf("remote Host with its own Origin: status %d allow-origin=%q", w.Code, w.Header().Get("Access-Control-Allow-Origin"))
	}
	for _, origin := range append(devOrigins, "https://attacker.example", "https://wiki.example.test", "http://wiki.example.test:8443", "https://wiki.example.test:8443/") {
		w := serve(s, hostRequest(http.MethodPost, "/mcp", "wiki.example.test:8443", origin, mcpWriteBody))
		if w.Code != http.StatusForbidden || w.Header().Get("Access-Control-Allow-Origin") != "" {
			t.Errorf("remote Host with Origin=%q: status %d allow-origin=%q, want 403", origin, w.Code, w.Header().Get("Access-Control-Allow-Origin"))
		}
	}
	if w := serve(s, hostRequest(http.MethodOptions, "/api/articles", "wiki.example.test:8443", "http://localhost:5173", "")); w.Code != http.StatusForbidden {
		t.Errorf("preflight from dev origin to remote Host: status %d, want 403", w.Code)
	}
	if w := serve(s, hostRequest(http.MethodOptions, "/api/articles", "localhost:8090", "http://localhost:5173", "")); w.Code != http.StatusNoContent {
		t.Errorf("preflight from dev origin to loopback Host: status %d, want 204", w.Code)
	}
	var n int
	if err := s.DB.QueryRow("SELECT count(*) FROM articles WHERE title='rebound'").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("rejected origins created %d articles", n)
	}
}

func TestSeedAgentRulesStayNeutral(t *testing.T) {
	s := testServer(t)
	var title, summary, body string
	var pinned int
	if err := s.DB.QueryRow("SELECT title,summary,body,pinned FROM articles WHERE id='agent-rules'").Scan(&title, &summary, &body, &pinned); err != nil {
		t.Fatal(err)
	}
	if pinned != 1 {
		t.Errorf("agent-rules must stay pinned")
	}
	text := strings.ToLower(title + summary + body)
	for _, banned := range []string{"делегир", "дешёв", "дешев", "лидер"} {
		if strings.Contains(text, banned) {
			t.Errorf("seed agent-rules still mentions %q", banned)
		}
	}
	for _, kept := range []string{"проверя", "владел", "явного разрешения"} {
		if !strings.Contains(text, kept) {
			t.Errorf("seed agent-rules lost guidance containing %q", kept)
		}
	}
}
