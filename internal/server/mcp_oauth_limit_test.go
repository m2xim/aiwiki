package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestMCPOAuthLimiterWindowAndReset(t *testing.T) {
	l := newMCPOAuthLimiter()
	l.limit = 3
	now := time.Unix(1000, 0)
	for i := 0; i < 3; i++ {
		if !l.allow("a", now) {
			t.Fatalf("request %d refused", i)
		}
	}
	if l.allow("a", now.Add(59*time.Second)) {
		t.Fatal("limit not enforced inside the window")
	}
	if !l.allow("b", now.Add(59*time.Second)) {
		t.Fatal("keys must be independent")
	}
	if !l.allow("a", now.Add(time.Minute)) {
		t.Fatal("window did not reset")
	}
	// A clock that jumps backwards must not lock a key out forever.
	l2 := newMCPOAuthLimiter()
	l2.limit = 1
	l2.allow("a", now)
	if !l2.allow("a", now.Add(-time.Hour)) {
		t.Fatal("backward clock kept the old window")
	}
}

func TestMCPOAuthLimiterKeyBound(t *testing.T) {
	l := newMCPOAuthLimiter()
	l.maxKeys = 5
	now := time.Unix(1000, 0)
	for i := 0; i < 5; i++ {
		if !l.allow(fmt.Sprint("k", i), now) {
			t.Fatal("under the bound")
		}
	}
	if l.allow("new", now.Add(time.Second)) {
		t.Fatal("new key admitted while the table is full of live windows")
	}
	if !l.allow("k0", now.Add(time.Second)) {
		t.Fatal("existing key must keep working when the table is full")
	}
	if !l.allow("new", now.Add(time.Minute)) {
		t.Fatal("expired windows were not swept")
	}
	if len(l.windows) > 5 {
		t.Fatalf("table grew to %d", len(l.windows))
	}
}

func TestMCPOAuthLimiterConcurrent(t *testing.T) {
	l := newMCPOAuthLimiter()
	l.limit = 100
	now := time.Unix(1000, 0)
	var ok int64
	var wg sync.WaitGroup
	for g := 0; g < 16; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				if l.allow("shared", now) {
					atomic.AddInt64(&ok, 1)
				}
				l.allow(fmt.Sprint("own", g, i), now)
			}
		}(g)
	}
	wg.Wait()
	if ok != 100 {
		t.Fatalf("admitted %d, want exactly 100", ok)
	}
}

func TestMCPOAuthClientID(t *testing.T) {
	if got := mcpOAuthClientID(nil, map[string]any{}); got != "unknown" {
		t.Fatal(got)
	}
	if got := mcpOAuthClientID(map[string]any{"azp": "cli"}, map[string]any{"azp": "other"}); got != "cli" {
		t.Fatal(got)
	}
	if got := mcpOAuthClientID(map[string]any{}, map[string]any{"client_id": "intro"}); got != "intro" {
		t.Fatal(got)
	}
	got := mcpOAuthClientID(map[string]any{"azp": "a\x00b\nc" + strings.Repeat("x", 300)})
	if len([]rune(got)) != 100 || strings.ContainsAny(got, "\x00\n") {
		t.Fatalf("not sanitised: %q", got)
	}
	if got := mcpOAuthClientID(map[string]any{"azp": "\x01\x02"}); got != "unknown" {
		t.Fatal(got)
	}
}

type countingTransport struct {
	base  http.RoundTripper
	calls int64
}

func (c *countingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if strings.HasSuffix(r.URL.Path, "/introspect") {
		atomic.AddInt64(&c.calls, 1)
	}
	return c.base.RoundTrip(r)
}

func TestMCPOAuthRateLimitBeforeIntrospection(t *testing.T) {
	x := newMCPOAuthHarness(t)
	ct := &countingTransport{base: x.a.client.Transport}
	if ct.base == nil {
		ct.base = http.DefaultTransport
	}
	x.a.client.Transport = ct
	clock := time.Unix(5000, 0)
	x.a.now = func() time.Time { return clock }
	x.a.oauthLimit = newMCPOAuthLimiter()
	x.a.oauthLimit.limit = 3
	token := x.iss.bearer(oauthResourceGrant("rate-user"))
	other := x.iss.bearer(oauthResourceGrant("rate-other"))
	do := func(tok string) *httptest.ResponseRecorder {
		return x.do("POST", "/mcp", "", map[string]string{"Authorization": "Bearer " + tok})
	}
	for i := 0; i < 3; i++ {
		if w := do(token); w.Code != 200 {
			t.Fatalf("request %d: %d", i, w.Code)
		}
	}
	before := atomic.LoadInt64(&ct.calls)
	if before != 3 {
		t.Fatalf("introspections=%d", before)
	}
	w := do(token)
	if w.Code != 429 || w.Header().Get("Retry-After") != "60" || !strings.Contains(w.Body.String(), "rate limit") {
		t.Fatalf("over limit: %d %v %s", w.Code, w.Header(), w.Body)
	}
	if atomic.LoadInt64(&ct.calls) != before {
		t.Fatal("identity provider was called for a rate-limited request")
	}
	// Even a token the IdP would reject costs nothing once the subject is limited.
	x.iss.setActive(token, false)
	if w := do(token); w.Code != 429 || atomic.LoadInt64(&ct.calls) != before {
		t.Fatalf("revoked+limited: %d", w.Code)
	}
	if w := do(other); w.Code != 200 {
		t.Fatalf("other subject throttled: %d", w.Code)
	}
	clock = clock.Add(time.Minute)
	if w := do(token); w.Code != 401 {
		t.Fatalf("after reset the IdP verdict applies: %d", w.Code)
	}
}

func TestMCPOAuthPrincipalMarking(t *testing.T) {
	x := newMCPOAuthHarness(t)
	g := oauthResourceGrant("mark-user")
	g.claims["azp"] = "claude-desktop"
	r := httptest.NewRequest("POST", "/mcp", nil)
	r.Header.Set("Authorization", "Bearer "+x.iss.bearer(g))
	p, ok := x.a.authenticateBearer(httptest.NewRecorder(), r)
	if !ok || !p.MCPBearer || p.MCPClientID != "claude-desktop" {
		t.Fatalf("bearer principal not marked: ok=%v %+v", ok, p)
	}
	r = httptest.NewRequest("POST", "/mcp", nil)
	r.Header.Set("Authorization", "Bearer "+x.iss.bearer(oauthResourceGrant("mark-user")))
	if p, ok = x.a.authenticateBearer(httptest.NewRecorder(), r); !ok || p.MCPClientID != "unknown" {
		t.Fatalf("missing azp: %+v", p)
	}
}
