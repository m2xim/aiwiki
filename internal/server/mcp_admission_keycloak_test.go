package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	kcTestSubject = "7f0c1a52-3d1e-4b0a-9f55-0a1b2c3d4e5f"
	kcTestClient  = "svc-admission"
	kcTestSecret  = "s3cret-value"
	kcTestToken   = "machine-token-1"
)

// fakeKeycloak is an HTTP mock of the token endpoint and the two Admin REST
// endpoints used by the provider. It does not talk to a real Keycloak.
type fakeKeycloak struct {
	srv *httptest.Server

	mu          sync.Mutex
	tokenCalls  int
	tokenUser   string
	tokenPass   string
	tokenStatus int
	tokenRedir  string
	adminAuth   []string
	requests    []string
	userStatus  int
	userBody    string
	rolesStatus int
	rolesBody   string
	redirectTo  string
	hang        bool
}

func newFakeKeycloak(t *testing.T) *fakeKeycloak {
	t.Helper()
	k := &fakeKeycloak{
		tokenStatus: http.StatusOK,
		userStatus:  http.StatusOK,
		userBody:    `{"id":"` + kcTestSubject + `","enabled":true,"username":"alice"}`,
		rolesStatus: http.StatusOK,
		rolesBody:   `[{"id":"1","name":"aiwiki-access","composite":false}]`,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/auth/realms/r/protocol/openid-connect/token", k.handleToken)
	mux.HandleFunc("/auth/admin/realms/r/users/", k.handleAdmin)
	k.srv = httptest.NewServer(mux)
	t.Cleanup(k.srv.Close)
	return k
}

func (k *fakeKeycloak) handleToken(w http.ResponseWriter, r *http.Request) {
	k.mu.Lock()
	k.tokenCalls++
	k.tokenUser, k.tokenPass, _ = r.BasicAuth()
	status, redir := k.tokenStatus, k.tokenRedir
	k.mu.Unlock()
	if r.Method != http.MethodPost {
		http.Error(w, "method", http.StatusMethodNotAllowed)
		return
	}
	r.ParseForm()
	if r.PostForm.Get("grant_type") != "client_credentials" {
		http.Error(w, "grant", http.StatusBadRequest)
		return
	}
	if redir != "" {
		http.Redirect(w, r, redir, http.StatusFound)
		return
	}
	if status != http.StatusOK {
		http.Error(w, "token denied "+kcTestSecret, status)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"access_token":"` + kcTestToken + `","token_type":"Bearer","expires_in":300}`))
}

func (k *fakeKeycloak) handleAdmin(w http.ResponseWriter, r *http.Request) {
	k.mu.Lock()
	k.adminAuth = append(k.adminAuth, r.Header.Get("Authorization"))
	k.requests = append(k.requests, r.Method+" "+r.URL.RequestURI())
	status, body := k.userStatus, k.userBody
	if strings.HasSuffix(r.URL.Path, "/role-mappings/realm/composite") {
		status, body = k.rolesStatus, k.rolesBody
	}
	redirect, hang := k.redirectTo, k.hang
	k.mu.Unlock()
	if hang {
		select {
		case <-r.Context().Done():
		case <-time.After(10 * time.Second):
		}
		return
	}
	if redirect != "" {
		http.Redirect(w, r, redirect, http.StatusFound)
		return
	}
	if status != http.StatusOK {
		http.Error(w, "admin body secret "+kcTestSecret, status)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(body))
}

func (k *fakeKeycloak) set(f func(*fakeKeycloak)) {
	k.mu.Lock()
	defer k.mu.Unlock()
	f(k)
}

func (k *fakeKeycloak) adminRequests() []string {
	k.mu.Lock()
	defer k.mu.Unlock()
	return append([]string(nil), k.requests...)
}

func (k *fakeKeycloak) config() CorporateConfig {
	return CorporateConfig{
		IssuerURL:         k.srv.URL + "/auth/realms/r",
		AllowInsecureHTTP: true,
		Admission: KeycloakAdmissionConfig{
			AdminURL:     k.srv.URL + "/auth/admin/realms/r",
			ClientID:     kcTestClient,
			ClientSecret: kcTestSecret,
		},
	}
}

func (k *fakeKeycloak) tokenURL() string {
	return k.srv.URL + "/auth/realms/r/protocol/openid-connect/token"
}

func (k *fakeKeycloak) provider(t *testing.T) mcpAdmissionProvider {
	t.Helper()
	p, err := newKeycloakAdmissionProvider(k.config(), k.tokenURL(), nil)
	if err != nil || p == nil {
		t.Fatalf("provider: %v %v", p, err)
	}
	return p
}

func (k *fakeKeycloak) issuer() string { return k.srv.URL + "/auth/realms/r" }

// assertProviderFault requires a generic failure that is not a denial and
// does not echo secrets, URLs or response bodies.
func assertProviderFault(t *testing.T, k *fakeKeycloak, err error) {
	t.Helper()
	if err == nil || errors.Is(err, errMCPAdmissionDenied) {
		t.Fatalf("want provider fault, got %v", err)
	}
	for _, leak := range []string{kcTestSecret, kcTestToken, k.srv.URL, "admin body", "token denied"} {
		if strings.Contains(err.Error(), leak) {
			t.Fatalf("error leaks %q: %v", leak, err)
		}
	}
}

func TestKeycloakAdmissionDisabledAndPartial(t *testing.T) {
	p, err := newKeycloakAdmissionProvider(CorporateConfig{IssuerURL: "https://idp.example/realms/r"}, "", nil)
	if p != nil || err != nil {
		t.Fatalf("empty config must disable: %v %v", p, err)
	}
	for _, a := range []KeycloakAdmissionConfig{
		{AdminURL: "https://idp.example/admin/realms/r"},
		{ClientID: "c"},
		{ClientSecret: "s"},
		{AdminURL: "https://idp.example/admin/realms/r", ClientID: "c"},
		{ClientID: "c", ClientSecret: "s"},
	} {
		p, err := newKeycloakAdmissionProvider(CorporateConfig{IssuerURL: "https://idp.example/realms/r", Admission: a}, "https://idp.example/token", nil)
		if p != nil || err == nil {
			t.Fatalf("partial %+v must fail: %v %v", a, p, err)
		}
	}
}

func TestKeycloakAdmissionConfigInvalid(t *testing.T) {
	base := func() CorporateConfig {
		return CorporateConfig{
			IssuerURL: "https://idp.example/auth/realms/r",
			Admission: KeycloakAdmissionConfig{AdminURL: "https://idp.example/auth/admin/realms/r", ClientID: "c", ClientSecret: "s"},
		}
	}
	tok := "https://idp.example/auth/realms/r/protocol/openid-connect/token"
	if p, err := newKeycloakAdmissionProvider(base(), tok, nil); err != nil || p == nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	cases := map[string]func(*CorporateConfig, *string){
		"wrong realm": func(c *CorporateConfig, _ *string) {
			c.Admission.AdminURL = "https://idp.example/auth/admin/realms/other"
		},
		"wrong prefix": func(c *CorporateConfig, _ *string) { c.Admission.AdminURL = "https://idp.example/admin/realms/r" },
		"wrong origin": func(c *CorporateConfig, _ *string) { c.Admission.AdminURL = "https://evil.example/auth/admin/realms/r" },
		"wrong port": func(c *CorporateConfig, _ *string) {
			c.Admission.AdminURL = "https://idp.example:8443/auth/admin/realms/r"
		},
		"query":       func(c *CorporateConfig, _ *string) { c.Admission.AdminURL += "?x=1" },
		"empty query": func(c *CorporateConfig, _ *string) { c.Admission.AdminURL += "?" },
		"fragment":    func(c *CorporateConfig, _ *string) { c.Admission.AdminURL += "#x" },
		"userinfo": func(c *CorporateConfig, _ *string) {
			c.Admission.AdminURL = "https://u:p@idp.example/auth/admin/realms/r"
		},
		"encoded slash": func(c *CorporateConfig, _ *string) {
			c.Admission.AdminURL = "https://idp.example/auth/admin/realms/r%2Fx"
		},
		"encoded realm": func(c *CorporateConfig, _ *string) {
			c.Admission.AdminURL = "https://idp.example/auth/admin/realms/%72"
		},
		"dot segment": func(c *CorporateConfig, _ *string) {
			c.Admission.AdminURL = "https://idp.example/auth/x/../admin/realms/r"
		},
		"trailing slash":   func(c *CorporateConfig, _ *string) { c.Admission.AdminURL += "/" },
		"extra path":       func(c *CorporateConfig, _ *string) { c.Admission.AdminURL += "/users" },
		"backslash":        func(c *CorporateConfig, _ *string) { c.Admission.AdminURL = `https://idp.example/auth/admin\realms/r` },
		"http admin":       func(c *CorporateConfig, _ *string) { c.Admission.AdminURL = "http://idp.example/auth/admin/realms/r" },
		"issuer no realm":  func(c *CorporateConfig, _ *string) { c.IssuerURL = "https://idp.example/auth" },
		"issuer query":     func(c *CorporateConfig, _ *string) { c.IssuerURL += "?a=b" },
		"issuer fragment":  func(c *CorporateConfig, _ *string) { c.IssuerURL += "#a" },
		"issuer userinfo":  func(c *CorporateConfig, _ *string) { c.IssuerURL = "https://u@idp.example/auth/realms/r" },
		"issuer slash":     func(c *CorporateConfig, _ *string) { c.IssuerURL += "/" },
		"issuer encoded":   func(c *CorporateConfig, _ *string) { c.IssuerURL = "https://idp.example/auth/realms/r%2e" },
		"issuer dot":       func(c *CorporateConfig, _ *string) { c.IssuerURL = "https://idp.example/a/../realms/r" },
		"issuer empty":     func(c *CorporateConfig, _ *string) { c.IssuerURL = "" },
		"custom claim":     func(c *CorporateConfig, _ *string) { c.RolesClaim = "groups" },
		"empty token url":  func(_ *CorporateConfig, t *string) { *t = "" },
		"http token url":   func(_ *CorporateConfig, t *string) { *t = "http://idp.example/token" },
		"userinfo token":   func(_ *CorporateConfig, t *string) { *t = "https://u:p@idp.example/token" },
		"bad token scheme": func(_ *CorporateConfig, t *string) { *t = "ftp://idp.example/token" },
	}
	for name, mut := range cases {
		cfg, tu := base(), tok
		mut(&cfg, &tu)
		if p, err := newKeycloakAdmissionProvider(cfg, tu, nil); p != nil || err == nil {
			t.Errorf("%s: must fail, got %v %v", name, p, err)
		}
	}
	// Explicit default claim and a bare-host issuer (no prefix) are fine.
	cfg := base()
	cfg.RolesClaim = authDefaultRolesClaim
	if _, err := newKeycloakAdmissionProvider(cfg, tok, nil); err != nil {
		t.Errorf("default claim: %v", err)
	}
	cfg = CorporateConfig{
		IssuerURL: "https://idp.example/realms/r",
		Admission: KeycloakAdmissionConfig{AdminURL: "https://IDP.example/admin/realms/r", ClientID: "c", ClientSecret: "s"},
	}
	if _, err := newKeycloakAdmissionProvider(cfg, tok, nil); err != nil {
		t.Errorf("no prefix: %v", err)
	}
}

func TestKeycloakAdmissionAllowedAndTokenClient(t *testing.T) {
	k := newFakeKeycloak(t)
	p := k.provider(t)
	for i := 0; i < 2; i++ {
		if err := p.Check(context.Background(), k.issuer(), kcTestSubject); err != nil {
			t.Fatalf("check %d: %v", i, err)
		}
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.tokenCalls != 1 {
		t.Fatalf("machine token must be cached, got %d fetches", k.tokenCalls)
	}
	if k.tokenUser != kcTestClient || k.tokenPass != kcTestSecret {
		t.Fatalf("token client auth = %q/%q", k.tokenUser, k.tokenPass)
	}
	want := []string{
		"GET /auth/admin/realms/r/users/" + kcTestSubject,
		"GET /auth/admin/realms/r/users/" + kcTestSubject + "/role-mappings/realm/composite",
	}
	if len(k.requests) != 4 || k.requests[0] != want[0] || k.requests[1] != want[1] {
		t.Fatalf("requests = %v", k.requests)
	}
	for _, a := range k.adminAuth {
		if a != "Bearer "+kcTestToken {
			t.Fatalf("admin auth = %q", a)
		}
	}
}

func TestKeycloakAdmissionNoPositiveCache(t *testing.T) {
	k := newFakeKeycloak(t)
	p := k.provider(t)
	check := func() error { return p.Check(context.Background(), k.issuer(), kcTestSubject) }
	if err := check(); err != nil {
		t.Fatal(err)
	}
	k.set(func(k *fakeKeycloak) { k.rolesBody = `[{"name":"other"}]` })
	if err := check(); !errors.Is(err, errMCPAdmissionDenied) {
		t.Fatalf("role removed: %v", err)
	}
	k.set(func(k *fakeKeycloak) { k.rolesBody = `[{"name":"aiwiki-access"}]` })
	if err := check(); err != nil {
		t.Fatalf("role restored: %v", err)
	}
	k.set(func(k *fakeKeycloak) { k.userBody = `{"id":"` + kcTestSubject + `","enabled":false}` })
	if err := check(); !errors.Is(err, errMCPAdmissionDenied) {
		t.Fatalf("user disabled: %v", err)
	}
	k.set(func(k *fakeKeycloak) { k.userStatus = http.StatusNotFound })
	if err := check(); !errors.Is(err, errMCPAdmissionDenied) {
		t.Fatalf("user deleted: %v", err)
	}
}

func TestKeycloakAdmissionCompositeRoles(t *testing.T) {
	k := newFakeKeycloak(t)
	p := k.provider(t)
	check := func() error { return p.Check(context.Background(), k.issuer(), kcTestSubject) }
	// The composite endpoint returns effective roles, including inherited ones.
	k.set(func(k *fakeKeycloak) {
		k.rolesBody = `[{"name":"offline_access"},{"name":"team","composite":true},{"name":"aiwiki-access","description":"via group"}]`
	})
	if err := check(); err != nil {
		t.Fatalf("composite: %v", err)
	}
	for name, body := range map[string]string{
		"case differs": `[{"name":"AIWIKI-ACCESS"}]`,
		"prefix":       `[{"name":"aiwiki-access-x"}]`,
		"padded":       `[{"name":" aiwiki-access"}]`,
		"admin only":   `[{"name":"aiwiki-admin"}]`,
		"empty":        `[]`,
		"null":         `null`,
		"no name":      `[{"id":"aiwiki-access"}]`,
		"null name":    `[{"name":null}]`,
	} {
		k.set(func(k *fakeKeycloak) { k.rolesBody = body })
		if err := check(); !errors.Is(err, errMCPAdmissionDenied) {
			t.Errorf("%s: want denied, got %v", name, err)
		}
	}
	// Configured role name is used.
	cfg := k.config()
	cfg.AccessRole = "custom-role"
	cp, err := newKeycloakAdmissionProvider(cfg, k.tokenURL(), nil)
	if err != nil {
		t.Fatal(err)
	}
	k.set(func(k *fakeKeycloak) { k.rolesBody = `[{"name":"custom-role"}]` })
	if err := cp.Check(context.Background(), k.issuer(), kcTestSubject); err != nil {
		t.Fatalf("custom role: %v", err)
	}
	k.set(func(k *fakeKeycloak) { k.rolesBody = `[{"name":"aiwiki-access"}]` })
	if err := cp.Check(context.Background(), k.issuer(), kcTestSubject); !errors.Is(err, errMCPAdmissionDenied) {
		t.Fatalf("default role must not satisfy custom: %v", err)
	}
}

func TestKeycloakAdmissionIdentityAndEnabled(t *testing.T) {
	k := newFakeKeycloak(t)
	p := k.provider(t)
	for name, body := range map[string]string{
		"id mismatch":    `{"id":"someone-else","enabled":true}`,
		"id case":        `{"id":"` + strings.ToUpper(kcTestSubject) + `","enabled":true}`,
		"id missing":     `{"enabled":true}`,
		"id null":        `{"id":null,"enabled":true}`,
		"enabled false":  `{"id":"` + kcTestSubject + `","enabled":false}`,
		"enabled absent": `{"id":"` + kcTestSubject + `"}`,
		"enabled null":   `{"id":"` + kcTestSubject + `","enabled":null}`,
	} {
		k.set(func(k *fakeKeycloak) { k.userBody = body })
		if err := p.Check(context.Background(), k.issuer(), kcTestSubject); !errors.Is(err, errMCPAdmissionDenied) {
			t.Errorf("%s: want denied, got %v", name, err)
		}
	}
	// Non-UUID federation ids are valid when the provider confirms them.
	const fed = "f:5b2c:alice@corp"
	k.set(func(k *fakeKeycloak) { k.userBody = `{"id":"` + fed + `","enabled":true}` })
	if err := p.Check(context.Background(), k.issuer(), fed); err != nil {
		t.Fatalf("federation id: %v", err)
	}
	if err := p.Check(context.Background(), "https://other.example/realms/r", kcTestSubject); !errors.Is(err, errMCPAdmissionDenied) {
		t.Fatalf("foreign issuer: %v", err)
	}
}

func TestKeycloakAdmissionProviderFaults(t *testing.T) {
	check := func(k *fakeKeycloak) error {
		return k.provider(t).Check(context.Background(), k.issuer(), kcTestSubject)
	}
	for _, st := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusInternalServerError, http.StatusBadGateway, http.StatusTooManyRequests, http.StatusBadRequest} {
		k := newFakeKeycloak(t)
		k.set(func(k *fakeKeycloak) { k.userStatus = st })
		assertProviderFault(t, k, check(k))

		k = newFakeKeycloak(t)
		k.set(func(k *fakeKeycloak) { k.rolesStatus = st })
		assertProviderFault(t, k, check(k))
	}
	k := newFakeKeycloak(t)
	k.set(func(k *fakeKeycloak) { k.tokenStatus = http.StatusUnauthorized })
	assertProviderFault(t, k, check(k))
	if len(k.adminRequests()) != 0 {
		t.Fatal("admin API must not be called without a machine token")
	}

	// Provider unreachable.
	k = newFakeKeycloak(t)
	p := k.provider(t)
	k.srv.Close()
	assertProviderFault(t, k, p.Check(context.Background(), k.issuer(), kcTestSubject))

	// A 401 drops the cached machine token.
	k = newFakeKeycloak(t)
	p = k.provider(t)
	if err := p.Check(context.Background(), k.issuer(), kcTestSubject); err != nil {
		t.Fatal(err)
	}
	k.set(func(k *fakeKeycloak) { k.userStatus = http.StatusUnauthorized })
	assertProviderFault(t, k, p.Check(context.Background(), k.issuer(), kcTestSubject))
	k.set(func(k *fakeKeycloak) { k.userStatus = http.StatusOK })
	if err := p.Check(context.Background(), k.issuer(), kcTestSubject); err != nil {
		t.Fatal(err)
	}
	k.mu.Lock()
	calls := k.tokenCalls
	k.mu.Unlock()
	if calls != 2 {
		t.Fatalf("token fetches = %d, want 2", calls)
	}
}

func TestKeycloakAdmissionBadResponses(t *testing.T) {
	huge := strings.Repeat("a", authMaxBody)
	userCases := map[string]string{
		"too large":     `{"id":"` + kcTestSubject + `","enabled":true,"pad":"` + huge + `"}`,
		"malformed":     `{"id":`,
		"not json":      `<html>login</html>`,
		"empty":         ``,
		"trailing":      `{"id":"` + kcTestSubject + `","enabled":true} {}`,
		"trailing junk": `{"id":"` + kcTestSubject + `","enabled":true}x`,
		"array":         `[]`,
		"id type":       `{"id":5,"enabled":true}`,
		"enabled type":  `{"id":"` + kcTestSubject + `","enabled":"true"}`,
	}
	for name, body := range userCases {
		k := newFakeKeycloak(t)
		k.set(func(k *fakeKeycloak) { k.userBody = body })
		assertProviderFault(t, k, k.provider(t).Check(context.Background(), k.issuer(), kcTestSubject))
		_ = name
	}
	roleCases := map[string]string{
		"too large": `[{"name":"aiwiki-access","pad":"` + huge + `"}]`,
		"malformed": `[{"name":`,
		"object":    `{"name":"aiwiki-access"}`,
		"trailing":  `[{"name":"aiwiki-access"}] []`,
		"name type": `[{"name":7}]`,
	}
	for _, body := range roleCases {
		k := newFakeKeycloak(t)
		k.set(func(k *fakeKeycloak) { k.rolesBody = body })
		assertProviderFault(t, k, k.provider(t).Check(context.Background(), k.issuer(), kcTestSubject))
	}
}

func TestKeycloakAdmissionRedirectsNotFollowed(t *testing.T) {
	var mu sync.Mutex
	var hits []string
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits = append(hits, r.Header.Get("Authorization"))
		mu.Unlock()
		w.Write([]byte(`{"access_token":"x","token_type":"Bearer","expires_in":300}`))
	}))
	defer other.Close()

	k := newFakeKeycloak(t)
	k.set(func(k *fakeKeycloak) { k.redirectTo = other.URL + "/steal" })
	assertProviderFault(t, k, k.provider(t).Check(context.Background(), k.issuer(), kcTestSubject))

	k = newFakeKeycloak(t)
	k.set(func(k *fakeKeycloak) { k.tokenRedir = other.URL + "/token" })
	assertProviderFault(t, k, k.provider(t).Check(context.Background(), k.issuer(), kcTestSubject))

	mu.Lock()
	defer mu.Unlock()
	if len(hits) != 0 {
		t.Fatalf("redirect target was contacted: %v", hits)
	}
}

func TestKeycloakAdmissionMaliciousSubjects(t *testing.T) {
	k := newFakeKeycloak(t)
	p := k.provider(t)
	bad := []string{
		"", ".", "..", "a/b", "../x", "a\\b", "a?b", "a#b", "a%2Fb", "%2e%2e", "a%00", "a\nb", "a\x00b",
		"a\tb", "a\x7fb", "a\u0085b", "\xff\xfe", strings.Repeat("a", keycloakAdmissionMaxSubject+1),
		"x/../../../../master", "?", "#", "%",
	}
	for _, s := range bad {
		if err := p.Check(context.Background(), k.issuer(), s); !errors.Is(err, errMCPAdmissionDenied) {
			t.Errorf("subject %q: want denied, got %v", s, err)
		}
	}
	if reqs := k.adminRequests(); len(reqs) != 0 {
		t.Fatalf("malicious subjects reached the provider: %v", reqs)
	}
	// The longest allowed subject is accepted and sent as one path segment.
	long := strings.Repeat("a", keycloakAdmissionMaxSubject)
	k.set(func(k *fakeKeycloak) { k.userBody = `{"id":"` + long + `","enabled":true}` })
	if err := p.Check(context.Background(), k.issuer(), long); err != nil {
		t.Fatalf("max-length subject: %v", err)
	}
	// Unusual but harmless characters stay inside a single escaped segment.
	const odd = "a b@c+d"
	k.set(func(k *fakeKeycloak) { k.userBody = `{"id":"` + odd + `","enabled":true}` })
	if err := p.Check(context.Background(), k.issuer(), odd); err != nil {
		t.Fatalf("odd subject: %v", err)
	}
	reqs := k.adminRequests()
	if got := reqs[len(reqs)-1]; got != "GET /auth/admin/realms/r/users/a%20b@c+d/role-mappings/realm/composite" {
		t.Fatalf("request = %q", got)
	}
}

func TestKeycloakAdmissionCancellation(t *testing.T) {
	k := newFakeKeycloak(t)
	p := k.provider(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := p.Check(ctx, k.issuer(), kcTestSubject)
	assertProviderFault(t, k, err)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled in chain, got %v", err)
	}

	k.set(func(k *fakeKeycloak) { k.hang = true })
	ctx, cancel = context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	err = p.Check(ctx, k.issuer(), kcTestSubject)
	assertProviderFault(t, k, err)
	if time.Since(start) > 3*time.Second {
		t.Fatal("cancellation did not abort the request promptly")
	}

	// A deadline already expired is reported the same way.
	ctx, cancel = context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	time.Sleep(time.Millisecond)
	assertProviderFault(t, k, p.Check(ctx, k.issuer(), kcTestSubject))
}
