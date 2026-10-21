package server

// OIDC authentication for the corporate edition.
//
// Browser flow: authorization code + PKCE (S256) + state bound to a browser
// cookie + nonce. The resulting access token is kept only in process memory
// behind an opaque session cookie (the map is keyed by SHA-256 of the cookie
// secret). There is no refresh: a session never outlives the access token or
// the configured session TTL, and a restart logs everybody out.
//
// Every authenticated request introspects the access token (RFC 7662, using
// the confidential client credentials) and re-evaluates the access/admin
// roles and the local users.disabled flag. If the introspection endpoint is
// unavailable the request fails closed (503).
//
// Known limitation: an account disabled in the upstream directory is only
// noticed once the identity provider reports the token inactive. Claims in an
// already issued token (roles) can therefore lag the real directory state for
// at most the access token lifetime. Disabling a user locally takes effect on
// the next request.
//
// /mcp accepts only an OIDC access token (Authorization: Bearer). Its JWT
// signature, issuer, audience (the configured client ID) and expiry are
// verified, then it is introspected and the local user is checked. Browser
// cookies never authenticate /mcp, and bearer tokens never authenticate any
// other path.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

const (
	authDefaultAccessRole = "aiwiki-access"
	authDefaultAdminRole  = "aiwiki-admin"
	authDefaultRolesClaim = "realm_access.roles"

	authDefaultSessionTTL = 5 * time.Minute
	authMaxSessionTTL     = 15 * time.Minute
	authLoginTTL          = 10 * time.Minute
	authHTTPTimeout       = 5 * time.Second
	authMaxBody           = 1 << 20
	authMaxSessions       = 10000
	authMaxLogins         = 10000
)

var errAuthTokenInactive = errors.New("token inactive")

// AuthService implements OIDC login, local sessions and per-request
// authorization for the corporate edition.
type AuthService struct {
	persistent    *mcpPersistentAccess
	cfg           CorporateConfig
	users         authUsers
	verifier      *oidc.IDTokenVerifier
	oauth         *oauth2.Config
	introspectURL string
	client        *http.Client
	secureCookies bool
	sessionCookie string
	loginCookie   string
	now           func() time.Time
	maxSessions   int
	maxLogins     int

	mu          sync.Mutex
	sessions    map[[32]byte]*authSession
	logins      map[[32]byte]*authLogin
	delegations map[[32]byte]*mcpDelegation
}

type authSession struct {
	userID      string
	subject     string
	accessToken string
	csrf        string
	expires     time.Time
}

type authLogin struct {
	binder   [32]byte
	nonce    string
	verifier string
	expires  time.Time
}

// authUsers is the persistence the auth slice needs from the users table.
type authUsers interface {
	// upsert inserts or refreshes name/admin. It never changes disabled.
	upsert(ctx context.Context, p Principal) (disabled bool, err error)
	find(ctx context.Context, id string) (name string, disabled bool, found bool, err error)
}

type authSQLUsers struct{ db *database }

func (s authSQLUsers) upsert(ctx context.Context, p Principal) (bool, error) {
	admin := 0
	if p.Admin {
		admin = 1
	}
	var disabled int
	err := s.db.QueryRowContext(ctx,
		`INSERT INTO users(id, issuer, subject, name, admin, disabled) VALUES(?, ?, ?, ?, ?, 0)
		 ON CONFLICT(issuer, subject) DO UPDATE SET name = excluded.name, admin = excluded.admin
		 RETURNING disabled`,
		p.ID, p.Issuer, p.Subject, p.Name, admin).Scan(&disabled)
	if err != nil {
		return false, err
	}
	return disabled != 0, nil
}

func (s authSQLUsers) find(ctx context.Context, id string) (string, bool, bool, error) {
	var name string
	var disabled int
	err := s.db.QueryRowContext(ctx, `SELECT name, disabled FROM users WHERE id = ?`, id).Scan(&name, &disabled)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", false, false, nil
		}
		return "", false, false, err
	}
	return name, disabled != 0, true, nil
}

func newAuthService(ctx context.Context, db *database, cfg CorporateConfig) (*AuthService, error) {
	if db == nil {
		return nil, errors.New("auth: database is required")
	}
	return newAuthServiceWithUsers(ctx, authSQLUsers{db}, cfg)
}

func newAuthServiceWithUsers(ctx context.Context, users authUsers, cfg CorporateConfig) (*AuthService, error) {
	cfg, err := normalizeAuthConfig(cfg)
	if err != nil {
		return nil, err
	}
	base := http.DefaultTransport.(*http.Transport).Clone()
	client := &http.Client{
		Timeout:   authHTTPTimeout,
		Transport: &authGuardTransport{base: base, allowInsecure: cfg.AllowInsecureHTTP},
	}
	provider, err := oidc.NewProvider(oidc.ClientContext(ctx, client), cfg.IssuerURL)
	if err != nil {
		return nil, fmt.Errorf("auth: oidc discovery: %w", err)
	}
	var meta struct {
		Introspection string `json:"introspection_endpoint"`
		JWKS          string `json:"jwks_uri"`
	}
	if err := provider.Claims(&meta); err != nil {
		return nil, fmt.Errorf("auth: oidc metadata: %w", err)
	}
	endpoint := provider.Endpoint()
	for name, u := range map[string]string{
		"authorization_endpoint": endpoint.AuthURL,
		"token_endpoint":         endpoint.TokenURL,
		"introspection_endpoint": meta.Introspection,
		"jwks_uri":               meta.JWKS,
	} {
		if err := authCheckEndpoint(u, cfg.AllowInsecureHTTP); err != nil {
			return nil, fmt.Errorf("auth: %s: %w", name, err)
		}
	}
	endpoint.AuthStyle = oauth2.AuthStyleInHeader

	redirect, _ := url.Parse(cfg.RedirectURL)
	secure := redirect.Scheme == "https"
	a := &AuthService{
		cfg:      cfg,
		users:    users,
		verifier: provider.Verifier(&oidc.Config{ClientID: cfg.ClientID}),
		oauth: &oauth2.Config{
			ClientID:     cfg.ClientID,
			ClientSecret: cfg.ClientSecret,
			Endpoint:     endpoint,
			RedirectURL:  cfg.RedirectURL,
			Scopes:       []string{oidc.ScopeOpenID, "profile"},
		},
		introspectURL: meta.Introspection,
		client:        client,
		secureCookies: secure,
		sessionCookie: "aiwiki_session",
		loginCookie:   "aiwiki_login",
		now:           time.Now,
		maxSessions:   authMaxSessions,
		maxLogins:     authMaxLogins,
		sessions:      map[[32]byte]*authSession{},
		delegations:   map[[32]byte]*mcpDelegation{},
		logins:        map[[32]byte]*authLogin{},
	}
	if secure {
		a.sessionCookie = "__Host-aiwiki_session"
		a.loginCookie = "__Host-aiwiki_login"
	}
	admission, err := newKeycloakAdmissionProvider(cfg, endpoint.TokenURL, client)
	if err != nil {
		return nil, err
	}
	if admission != nil {
		if sqlUsers, ok := users.(authSQLUsers); ok {
			a.persistent = &mcpPersistentAccess{db: sqlUsers.db, admission: admission, now: time.Now}
		}
	}
	return a, nil
}

func normalizeAuthConfig(cfg CorporateConfig) (CorporateConfig, error) {
	cfg.IssuerURL = strings.TrimSpace(cfg.IssuerURL)
	cfg.RedirectURL = strings.TrimSpace(cfg.RedirectURL)
	if cfg.IssuerURL == "" || cfg.ClientID == "" || cfg.ClientSecret == "" || cfg.RedirectURL == "" {
		return cfg, errors.New("auth: issuer, client id, client secret and redirect URL are required")
	}
	if err := authCheckEndpoint(cfg.IssuerURL, cfg.AllowInsecureHTTP); err != nil {
		return cfg, fmt.Errorf("auth: issuer: %w", err)
	}
	if err := authCheckEndpoint(cfg.RedirectURL, cfg.AllowInsecureHTTP); err != nil {
		return cfg, fmt.Errorf("auth: redirect URL: %w", err)
	}
	if cfg.AccessRole == "" {
		cfg.AccessRole = authDefaultAccessRole
	}
	if cfg.AdminRole == "" {
		cfg.AdminRole = authDefaultAdminRole
	}
	if cfg.RolesClaim == "" {
		cfg.RolesClaim = authDefaultRolesClaim
	}
	switch {
	case cfg.SessionTTL == 0:
		cfg.SessionTTL = authDefaultSessionTTL
	case cfg.SessionTTL < 0 || cfg.SessionTTL > authMaxSessionTTL:
		return cfg, fmt.Errorf("auth: session TTL must be within (0, %s]", authMaxSessionTTL)
	}
	return cfg, nil
}

// authCheckEndpoint requires HTTPS; plain HTTP is accepted only when
// explicitly allowed AND the host is a loopback address.
func authCheckEndpoint(raw string, allowInsecure bool) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil {
		return errors.New("invalid URL")
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		if allowInsecure && authLoopbackHost(u.Hostname()) {
			return nil
		}
		return errors.New("plain HTTP is only allowed for loopback hosts with AllowInsecureHTTP")
	}
	return errors.New("unsupported URL scheme")
}

func authLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// authGuardTransport applies the endpoint policy to every outgoing request,
// including redirects, so the identity provider cannot bounce us elsewhere.
type authGuardTransport struct {
	base          http.RoundTripper
	allowInsecure bool
}

func (t *authGuardTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if err := authCheckEndpoint(req.URL.String(), t.allowInsecure); err != nil {
		return nil, fmt.Errorf("auth: blocked request: %w", err)
	}
	return t.base.RoundTrip(req)
}

func (a *AuthService) routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /auth/login", a.handleLogin)
	mux.HandleFunc("GET /auth/callback", a.handleCallback)
	mux.HandleFunc("POST /auth/logout", a.handleLogout)
	mux.HandleFunc("GET /api/me", a.handleMe)
}

// authenticate resolves the caller for /api, /files and /mcp requests. On
// failure it has already written the response.
func (a *AuthService) authenticate(w http.ResponseWriter, r *http.Request) (Principal, bool) {
	if authIsMCPPath(r.URL.Path) {
		return a.authenticateBearer(w, r)
	}
	_, p, ok := a.authenticateSession(w, r)
	return p, ok
}

func authIsMCPPath(p string) bool {
	return p == "/mcp" || strings.HasPrefix(p, "/mcp/")
}

func authSafeMethod(m string) bool {
	return m == http.MethodGet || m == http.MethodHead || m == http.MethodOptions
}

func (a *AuthService) authenticateSession(w http.ResponseWriter, r *http.Request) (*authSession, Principal, bool) {
	c, err := r.Cookie(a.sessionCookie)
	if err != nil || c.Value == "" {
		authError(w, http.StatusUnauthorized, "authentication required")
		return nil, Principal{}, false
	}
	key := authHash(c.Value)
	sess := a.sessionSnapshot(key)
	if sess == nil {
		a.clearSessionCookie(w)
		authError(w, http.StatusUnauthorized, "authentication required")
		return nil, Principal{}, false
	}
	if !authSafeMethod(r.Method) && !authEqual(r.Header.Get("X-CSRF-Token"), sess.csrf) {
		authError(w, http.StatusForbidden, "invalid CSRF token")
		return nil, Principal{}, false
	}
	info, err := a.introspect(r.Context(), sess.accessToken)
	if err == nil && info.subject != sess.subject {
		err = errAuthTokenInactive
	}
	if err != nil {
		if errors.Is(err, errAuthTokenInactive) {
			a.dropSession(key)
			a.clearSessionCookie(w)
			authError(w, http.StatusUnauthorized, "session expired")
		} else {
			log.Printf("auth: introspection unavailable: %v", err)
			authError(w, http.StatusServiceUnavailable, "authentication unavailable")
		}
		return nil, Principal{}, false
	}
	p, fail := a.resolve(r.Context(), info, nil, "", authSyncNone)
	if fail != 0 {
		if fail != http.StatusServiceUnavailable {
			a.dropSession(key)
			a.clearSessionCookie(w)
		}
		authError(w, fail, authFailText(fail))
		return nil, Principal{}, false
	}
	return sess, p, true
}

func (a *AuthService) authenticateBearer(w http.ResponseWriter, r *http.Request) (Principal, bool) {
	deny := func(status int, msg string) (Principal, bool) {
		if status == http.StatusUnauthorized {
			w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token"`)
		}
		authError(w, status, msg)
		return Principal{}, false
	}
	h := r.Header.Get("Authorization")
	if len(h) < 8 || !strings.EqualFold(h[:7], "bearer ") {
		w.Header().Set("WWW-Authenticate", "Bearer")
		authError(w, http.StatusUnauthorized, "bearer token required")
		return Principal{}, false
	}
	raw := strings.TrimSpace(h[7:])
	if raw == "" {
		return deny(http.StatusUnauthorized, "invalid token")
	}
	if strings.HasPrefix(raw, mcpDelegationPrefix) {
		return a.authenticateDelegation(w, r, raw)
	}
	if strings.HasPrefix(raw, mcpPersistentPrefix) {
		return a.authenticatePersistent(w, r, raw)
	}
	tok, err := a.verifier.Verify(r.Context(), raw)
	if err != nil {
		return deny(http.StatusUnauthorized, "invalid token")
	}
	var claims map[string]any
	if err := tok.Claims(&claims); err != nil {
		return deny(http.StatusUnauthorized, "invalid token")
	}
	// An ID token is not an access token.
	if typ, _ := claims["typ"].(string); strings.EqualFold(typ, "ID") {
		return deny(http.StatusUnauthorized, "invalid token")
	}
	info, err := a.introspect(r.Context(), raw)
	if err == nil && info.subject != tok.Subject {
		err = errAuthTokenInactive
	}
	if err != nil {
		if errors.Is(err, errAuthTokenInactive) {
			return deny(http.StatusUnauthorized, "invalid token")
		}
		log.Printf("auth: introspection unavailable: %v", err)
		return deny(http.StatusServiceUnavailable, "authentication unavailable")
	}
	p, fail := a.resolve(r.Context(), info, claims, "", authSyncLazy)
	if fail != 0 {
		return deny(fail, authFailText(fail))
	}
	return p, true
}

const (
	authSyncNone  = iota // session: user must already exist
	authSyncLazy         // bearer: create the user when unknown
	authSyncLogin        // login: create/refresh the user
)

func authFailText(status int) string {
	switch status {
	case http.StatusForbidden:
		return "access denied"
	case http.StatusServiceUnavailable:
		return "authentication unavailable"
	}
	return "authentication required"
}

// resolve turns verified token information into a Principal. It returns a
// non-zero HTTP status when the caller must be refused. Roles come from the
// introspection response. Missing admission claims always deny access.
func (a *AuthService) resolve(ctx context.Context, info *authTokenInfo, fallback map[string]any, name string, mode int) (Principal, int) {
	source := info.claims
	roles := authRoles(source, a.cfg.RolesClaim)
	access := authHasRole(roles, a.cfg.AccessRole)
	if !access { // admin never implies access
		return Principal{}, http.StatusForbidden
	}
	p := Principal{
		ID:      authUserID(a.cfg.IssuerURL, info.subject),
		Issuer:  a.cfg.IssuerURL,
		Subject: info.subject,
		Admin:   authHasRole(roles, a.cfg.AdminRole),
	}
	if name == "" {
		name = authName(info.claims, fallback, info.subject)
	}
	p.Name = name

	var disabled bool
	switch mode {
	case authSyncLogin:
		d, err := a.users.upsert(ctx, p)
		if err != nil {
			log.Printf("auth: user upsert failed: %v", err)
			return Principal{}, http.StatusServiceUnavailable
		}
		disabled = d
	default:
		n, d, found, err := a.users.find(ctx, p.ID)
		if err != nil {
			log.Printf("auth: user lookup failed: %v", err)
			return Principal{}, http.StatusServiceUnavailable
		}
		if !found {
			if mode != authSyncLazy {
				return Principal{}, http.StatusUnauthorized
			}
			d, err = a.users.upsert(ctx, p)
			if err != nil {
				log.Printf("auth: user upsert failed: %v", err)
				return Principal{}, http.StatusServiceUnavailable
			}
		} else if n != "" {
			p.Name = n
		}
		disabled = d
	}
	if disabled {
		return Principal{}, http.StatusForbidden
	}
	if mode != authSyncLogin {
		disabled, err := a.users.upsert(ctx, p)
		if err != nil {
			return Principal{}, http.StatusServiceUnavailable
		}
		if disabled {
			return Principal{}, http.StatusForbidden
		}
	}
	return p, 0
}

func authUserID(issuer, subject string) string {
	sum := sha256.Sum256([]byte(issuer + "\x00" + subject))
	return "u_" + hex.EncodeToString(sum[:])
}

func authName(a, b map[string]any, subject string) string {
	for _, m := range []map[string]any{a, b} {
		for _, k := range []string{"name", "preferred_username"} {
			if s, _ := m[k].(string); strings.TrimSpace(s) != "" {
				return strings.TrimSpace(s)
			}
		}
	}
	return subject
}

// authLookup finds a dot-notation path; keys that themselves contain dots
// are honoured.
func authLookup(m map[string]any, path string) (any, bool) {
	if v, ok := m[path]; ok {
		return v, true
	}
	for i := 0; i < len(path); i++ {
		if path[i] != '.' {
			continue
		}
		if sub, ok := m[path[:i]].(map[string]any); ok {
			if v, ok := authLookup(sub, path[i+1:]); ok {
				return v, true
			}
		}
	}
	return nil, false
}

func authRoles(m map[string]any, path string) []string {
	v, ok := authLookup(m, path)
	if !ok {
		return nil
	}
	switch t := v.(type) {
	case []any:
		var out []string
		for _, e := range t {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	case string:
		return strings.Fields(t)
	}
	return nil
}

func authHasRole(roles []string, want string) bool {
	if want == "" {
		return false
	}
	for _, r := range roles {
		if r == want {
			return true
		}
	}
	return false
}

type authTokenInfo struct {
	subject string
	expires time.Time
	claims  map[string]any
}

// introspect asks the identity provider whether the access token is still
// active. errAuthTokenInactive means the token is definitively refused; any
// other error is an outage and callers must fail closed.
func (a *AuthService) introspect(ctx context.Context, token string) (*authTokenInfo, error) {
	form := url.Values{"token": {token}, "token_type_hint": {"access_token"}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.introspectURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.SetBasicAuth(url.QueryEscape(a.cfg.ClientID), url.QueryEscape(a.cfg.ClientSecret))
	resp, err := a.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("introspection request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, io.LimitReader(resp.Body, authMaxBody))
		return nil, fmt.Errorf("introspection status %d", resp.StatusCode)
	}
	dec := json.NewDecoder(io.LimitReader(resp.Body, authMaxBody))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("introspection response: %w", err)
	}
	if active, _ := m["active"].(bool); !active {
		return nil, errAuthTokenInactive
	}
	if iss, _ := m["iss"].(string); iss != a.cfg.IssuerURL {
		return nil, errAuthTokenInactive
	}
	if !authAudContains(m["aud"], a.cfg.ClientID) {
		return nil, errAuthTokenInactive
	}
	sub, _ := m["sub"].(string)
	exp, ok := authUnix(m["exp"])
	if sub == "" || !ok || !exp.After(a.now()) {
		return nil, errAuthTokenInactive
	}
	return &authTokenInfo{subject: sub, expires: exp, claims: m}, nil
}

func authAudContains(v any, want string) bool {
	switch t := v.(type) {
	case string:
		return t == want
	case []any:
		for _, e := range t {
			if s, _ := e.(string); s == want {
				return true
			}
		}
	}
	return false
}

func authUnix(v any) (time.Time, bool) {
	n, ok := v.(json.Number)
	if !ok {
		return time.Time{}, false
	}
	f, err := n.Float64()
	if err != nil || f <= 0 {
		return time.Time{}, false
	}
	return time.Unix(int64(f), 0), true
}

// --- sessions ---

func authRandom(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func authHash(s string) [32]byte { return sha256.Sum256([]byte(s)) }

func authEqual(a, b string) bool {
	return len(b) > 0 && subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

func (a *AuthService) purgeLocked(now time.Time) {
	for k, s := range a.sessions {
		if !now.Before(s.expires) {
			delete(a.sessions, k)
			a.dropDelegationsLocked(k)
		}
	}
	for k, l := range a.logins {
		if !now.Before(l.expires) {
			delete(a.logins, k)
		}
	}
}

func (a *AuthService) sessionSnapshot(key [32]byte) *authSession {
	a.mu.Lock()
	defer a.mu.Unlock()
	s := a.sessions[key]
	if s == nil {
		return nil
	}
	if !a.now().Before(s.expires) {
		delete(a.sessions, key)
		a.dropDelegationsLocked(key)
		return nil
	}
	cp := *s
	return &cp
}

func (a *AuthService) dropSession(key [32]byte) {
	a.mu.Lock()
	delete(a.sessions, key)
	a.dropDelegationsLocked(key)
	a.mu.Unlock()
}

func (a *AuthService) newSession(s *authSession) (string, bool) {
	secret, err := authRandom(32)
	if err != nil {
		return "", false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.purgeLocked(a.now())
	if len(a.sessions) >= a.maxSessions {
		return "", false
	}
	a.sessions[authHash(secret)] = s
	return secret, true
}

func (a *AuthService) setCookie(w http.ResponseWriter, name, value string, maxAge int) {
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    value,
		Path:     "/",
		MaxAge:   maxAge,
		HttpOnly: true,
		Secure:   a.secureCookies,
		SameSite: http.SameSiteLaxMode,
	})
}

func (a *AuthService) clearSessionCookie(w http.ResponseWriter) {
	a.setCookie(w, a.sessionCookie, "", -1)
}

// --- handlers ---

func (a *AuthService) handleLogin(w http.ResponseWriter, r *http.Request) {
	state, e1 := authRandom(32)
	nonce, e2 := authRandom(32)
	binder, e3 := authRandom(32)
	if e1 != nil || e2 != nil || e3 != nil {
		authError(w, http.StatusInternalServerError, "login unavailable")
		return
	}
	verifier := oauth2.GenerateVerifier()
	now := a.now()
	a.mu.Lock()
	a.purgeLocked(now)
	if len(a.logins) >= a.maxLogins {
		a.mu.Unlock()
		authError(w, http.StatusServiceUnavailable, "too many pending logins")
		return
	}
	a.logins[authHash(state)] = &authLogin{
		binder:   authHash(binder),
		nonce:    nonce,
		verifier: verifier,
		expires:  now.Add(authLoginTTL),
	}
	a.mu.Unlock()

	a.setCookie(w, a.loginCookie, binder, int(authLoginTTL.Seconds()))
	target := a.oauth.AuthCodeURL(state,
		oauth2.S256ChallengeOption(verifier),
		oauth2.SetAuthURLParam("nonce", nonce))
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, target, http.StatusFound)
}

func (a *AuthService) handleCallback(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	q := r.URL.Query()
	state := q.Get("state")
	c, cerr := r.Cookie(a.loginCookie)
	a.setCookie(w, a.loginCookie, "", -1) // browser binding is single use too
	if state == "" {
		authError(w, http.StatusBadRequest, "invalid login state")
		return
	}
	key := authHash(state)
	a.mu.Lock()
	lg := a.logins[key]
	delete(a.logins, key) // single use, whatever happens next
	a.mu.Unlock()
	var bound [32]byte
	if cerr == nil {
		bound = authHash(c.Value)
	}
	if lg == nil || cerr != nil || c.Value == "" || !a.now().Before(lg.expires) ||
		subtle.ConstantTimeCompare(bound[:], lg.binder[:]) != 1 {
		authError(w, http.StatusBadRequest, "invalid login state")
		return
	}
	code := q.Get("code")
	if q.Get("error") != "" || code == "" {
		authError(w, http.StatusBadRequest, "login failed")
		return
	}

	ctx := context.WithValue(r.Context(), oauth2.HTTPClient, a.client)
	tok, err := a.oauth.Exchange(ctx, code, oauth2.VerifierOption(lg.verifier))
	if err != nil {
		log.Printf("auth: code exchange failed: %v", err)
		authError(w, http.StatusUnauthorized, "login failed")
		return
	}
	rawID, _ := tok.Extra("id_token").(string)
	idt, err := a.verifier.Verify(ctx, rawID)
	if err != nil || !authEqual(idt.Nonce, lg.nonce) {
		authError(w, http.StatusUnauthorized, "login failed")
		return
	}
	var idClaims map[string]any
	if err := idt.Claims(&idClaims); err != nil {
		authError(w, http.StatusUnauthorized, "login failed")
		return
	}
	info, err := a.introspect(ctx, tok.AccessToken)
	if err != nil || info.subject != idt.Subject {
		if err != nil && !errors.Is(err, errAuthTokenInactive) {
			log.Printf("auth: introspection unavailable: %v", err)
			authError(w, http.StatusServiceUnavailable, "authentication unavailable")
			return
		}
		authError(w, http.StatusUnauthorized, "login failed")
		return
	}
	p, fail := a.resolve(ctx, info, nil, authName(idClaims, nil, idt.Subject), authSyncLogin)
	if fail != 0 {
		authError(w, fail, authFailText(fail))
		return
	}

	now := a.now()
	expires := now.Add(a.cfg.SessionTTL)
	if info.expires.Before(expires) {
		expires = info.expires
	}
	if !tok.Expiry.IsZero() && tok.Expiry.Before(expires) {
		expires = tok.Expiry
	}
	if !expires.After(now) {
		authError(w, http.StatusUnauthorized, "login failed")
		return
	}
	csrf, err := authRandom(32)
	if err != nil {
		authError(w, http.StatusInternalServerError, "login unavailable")
		return
	}
	secret, ok := a.newSession(&authSession{
		userID:      p.ID,
		subject:     info.subject,
		accessToken: tok.AccessToken,
		csrf:        csrf,
		expires:     expires,
	})
	if !ok {
		authError(w, http.StatusServiceUnavailable, "login unavailable")
		return
	}
	a.setCookie(w, a.sessionCookie, secret, int(expires.Sub(now).Seconds()))
	http.Redirect(w, r, "/", http.StatusSeeOther) // never caller-controlled
}

func (a *AuthService) handleMe(w http.ResponseWriter, r *http.Request) {
	sess, p, ok := a.authenticateSession(w, r)
	if !ok {
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"mode": "corporate",
		"user": map[string]any{
			"id":    p.ID,
			"name":  p.Name,
			"admin": p.Admin,
		},
		"csrf_token": sess.csrf,
	})
}

// handleLogout revokes the local session only (no upstream logout, no
// introspection, so it works during an identity provider outage).
func (a *AuthService) handleLogout(w http.ResponseWriter, r *http.Request) {
	c, err := r.Cookie(a.sessionCookie)
	if err != nil || c.Value == "" {
		a.clearSessionCookie(w)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	key := authHash(c.Value)
	a.mu.Lock()
	s := a.sessions[key]
	if s != nil && authEqual(r.Header.Get("X-CSRF-Token"), s.csrf) {
		delete(a.sessions, key)
		a.dropDelegationsLocked(key)
		s = nil
	}
	a.mu.Unlock()
	if s != nil {
		authError(w, http.StatusForbidden, "invalid CSRF token")
		return
	}
	a.clearSessionCookie(w)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
}

func authError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
