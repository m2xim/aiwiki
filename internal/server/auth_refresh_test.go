package server

import (
	"net/http"
	"sync"
	"testing"
	"time"
)

// clockAt pins the clock of aiwiki and of the mock issuer (token issue and
// expiry times) and returns a function that moves both.
func (x *authHarness) clockAt(start time.Time) func(time.Duration) {
	var mu sync.Mutex
	now := start
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	x.a.now = clock
	x.iss.clock = clock // set before any request in the clock's time
	return func(d time.Duration) { mu.Lock(); now = now.Add(d); mu.Unlock() }
}

func TestAuthSessionRefreshOffByDefault(t *testing.T) {
	x := newAuthHarness(t, nil)
	g := authUserGrant("rob", "aiwiki-access")
	g.tokenTTL = time.Minute
	sc := x.session(g)
	if x.iss.refreshCount() != 0 {
		t.Fatal("no refresh without the option")
	}
	x.a.mu.Lock()
	for _, s := range x.a.sessions {
		if s.refresh != nil || s.expires.After(time.Now().Add(time.Minute+5*time.Second)) {
			t.Errorf("session must stay bound to the access token: %+v", s)
		}
	}
	x.a.mu.Unlock()
	if r := x.do("GET", "/api/echo", "", nil, sc); r.Code != 200 {
		t.Fatalf("fresh session: %d", r.Code)
	}
}

func TestAuthSessionRefreshRenewsAccessToken(t *testing.T) {
	x := newAuthHarness(t, func(c *CorporateConfig) { c.SessionRefresh = true; c.SessionMaxAge = 2 * time.Hour })
	g := authUserGrant("sue", "aiwiki-access", "aiwiki-admin")
	g.tokenTTL = time.Minute
	sc := x.session(g)

	// The session outlives the access token, up to the max age.
	x.a.mu.Lock()
	var sess *authSession
	for _, s := range x.a.sessions {
		sess = s
	}
	x.a.mu.Unlock()
	if sess == nil || sess.refresh == nil || sess.expires.Before(time.Now().Add(time.Hour)) {
		t.Fatalf("refreshable session expected: %+v", sess)
	}
	if c := x.do("GET", "/api/echo", "", nil, sc); c.Code != 200 || x.iss.refreshCount() != 0 {
		t.Fatalf("a fresh access token is not renewed: %d calls=%d", c.Code, x.iss.refreshCount())
	}

	advance := x.clockAt(time.Now())
	advance(2 * time.Minute) // past the access token
	if c := x.do("GET", "/api/echo", "", nil, sc); c.Code != 200 {
		t.Fatalf("expired access token must be renewed: %d %s", c.Code, c.Body)
	}
	if n := x.iss.refreshCount(); n != 1 {
		t.Fatalf("one renewal expected, got %d", n)
	}
	if c := x.do("GET", "/api/echo", "", nil, sc); c.Code != 200 || x.iss.refreshCount() != 1 {
		t.Fatalf("renewed token is reused: %d calls=%d", c.Code, x.iss.refreshCount())
	}

	// Concurrent requests on an expiring token renew exactly once (refresh
	// tokens rotate and are single use).
	advance(2 * time.Minute)
	var wg sync.WaitGroup
	codes := make(chan int, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); codes <- x.do("GET", "/api/echo", "", nil, sc).Code }()
	}
	wg.Wait()
	close(codes)
	for c := range codes {
		if c != 200 {
			t.Fatalf("concurrent renewal: %d", c)
		}
	}
	if n := x.iss.refreshCount(); n != 2 {
		t.Fatalf("concurrent requests must share one renewal, got %d", n-1)
	}

	// Provider outage: 503, the session survives.
	advance(2 * time.Minute)
	x.iss.setDown(true)
	if c := x.do("GET", "/api/echo", "", nil, sc); c.Code != http.StatusServiceUnavailable {
		t.Fatalf("outage: %d", c.Code)
	}
	x.iss.setDown(false)
	if c := x.do("GET", "/api/echo", "", nil, sc); c.Code != 200 {
		t.Fatalf("recovery after outage: %d %s", c.Code, c.Body)
	}

	// SSO session ended: the refresh is refused and the session ends.
	advance(2 * time.Minute)
	x.iss.revokeRefreshes()
	if c := x.do("GET", "/api/echo", "", nil, sc); c.Code != http.StatusUnauthorized {
		t.Fatalf("refused refresh: %d", c.Code)
	}
	if c := x.do("GET", "/api/echo", "", nil, sc); c.Code != http.StatusUnauthorized {
		t.Fatalf("session must be gone: %d", c.Code)
	}
}

func TestAuthSessionRefreshRespectsLocalChecks(t *testing.T) {
	x := newAuthHarness(t, func(c *CorporateConfig) { c.SessionRefresh = true })
	g := authUserGrant("tim", "aiwiki-access")
	g.tokenTTL = time.Minute
	sc := x.session(g)
	advance := x.clockAt(time.Now())
	advance(2 * time.Minute)
	x.users.disable(x.iss.srv.URL, "tim")
	if c := x.do("GET", "/api/echo", "", nil, sc); c.Code != http.StatusForbidden {
		t.Fatalf("locally disabled user after a successful refresh: %d", c.Code)
	}
}

func TestAuthSessionRefreshLifetimeBounds(t *testing.T) {
	// Max age bounds the session.
	x := newAuthHarness(t, func(c *CorporateConfig) { c.SessionRefresh = true; c.SessionMaxAge = 30 * time.Minute })
	start := time.Now()
	g := authUserGrant("uma", "aiwiki-access")
	g.tokenTTL = time.Minute
	x.session(g)
	x.a.mu.Lock()
	for _, s := range x.a.sessions {
		if s.expires.After(start.Add(30*time.Minute+5*time.Second)) || s.expires.Before(start.Add(29*time.Minute)) {
			t.Errorf("max age not applied: %v", s.expires.Sub(start))
		}
	}
	x.a.mu.Unlock()

	// The refresh token's own expiry bounds it further.
	y := newAuthHarness(t, func(c *CorporateConfig) { c.SessionRefresh = true; c.SessionMaxAge = 12 * time.Hour })
	y.iss.mu.Lock()
	y.iss.refreshTTL = 20 * time.Minute
	y.iss.mu.Unlock()
	y.session(g)
	y.a.mu.Lock()
	for _, s := range y.a.sessions {
		if s.expires.After(start.Add(20*time.Minute + 5*time.Second)) {
			t.Errorf("refresh_expires_in not applied: %v", s.expires.Sub(start))
		}
	}
	y.a.mu.Unlock()

	if _, err := normalizeAuthConfig(CorporateConfig{IssuerURL: "https://i", ClientID: "c", ClientSecret: "s",
		RedirectURL: "https://a/cb", SessionRefresh: true, SessionMaxAge: 8 * 24 * time.Hour}); err == nil {
		t.Fatal("max age above the cap must be refused")
	}
	cfg, err := normalizeAuthConfig(CorporateConfig{IssuerURL: "https://i", ClientID: "c", ClientSecret: "s", RedirectURL: "https://a/cb", SessionRefresh: true})
	if err != nil || cfg.SessionMaxAge != authDefaultSessionMaxAge {
		t.Fatalf("default max age: %v %v", cfg.SessionMaxAge, err)
	}
}

func TestAuthSessionRefreshMarksLogin(t *testing.T) {
	x := newAuthHarness(t, func(c *CorporateConfig) { c.SessionRefresh = true })
	g := authUserGrant("val", "aiwiki-access")
	g.tokenTTL = time.Minute
	sc := x.session(g)
	id := authUserID(x.iss.srv.URL, "val")
	start := time.Now()
	advance := x.clockAt(start)
	mark := func() time.Time {
		x.users.mu.Lock()
		defer x.users.mu.Unlock()
		return x.users.logins[id]
	}
	first := mark()
	if first.IsZero() {
		t.Fatal("login must be recorded at the callback")
	}
	advance(2 * time.Minute) // renewal within the hour: no new mark
	if c := x.do("GET", "/api/echo", "", nil, sc); c.Code != 200 || !mark().Equal(first) {
		t.Fatalf("early renewal re-marked the login: %d", c.Code)
	}
	advance(time.Hour) // renewal after an hour marks the active session
	if c := x.do("GET", "/api/echo", "", nil, sc); c.Code != 200 || !mark().After(first) {
		t.Fatalf("renewal after an hour must record the login: %d", c.Code)
	}
}
