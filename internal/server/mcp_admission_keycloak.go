package server

// Keycloak implementation of mcpAdmissionProvider.
//
// A separate confidential, read-only machine client (client_credentials)
// queries the Keycloak Admin REST API on every Check:
//
//	GET {AdminURL}/users/{subject}
//	GET {AdminURL}/users/{subject}/role-mappings/realm/composite
//
// The composite endpoint returns the realm roles the user effectively holds,
// including composite roles and roles inherited through groups (upstream
// RoleMapperResource filters realm roles with UserAdapter.hasRole). This
// matches the realm_access.roles claim used for browser logins, which is the
// only supported roles claim; custom claim mappings are not supported.
//
// Nothing about a user is cached: only the machine client's own access token
// is. Responses, URLs and secrets never appear in returned errors.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/clientcredentials"
)

const (
	keycloakAdmissionTimeout    = authHTTPTimeout
	keycloakAdmissionMaxSubject = 256
)

var errMCPAdmissionUnavailable = errors.New("MCP admission provider unavailable")

type keycloakAdmission struct {
	issuer     string
	adminBase  *url.URL
	accessRole string
	client     *http.Client
	tokens     *keycloakTokenCache
}

// newKeycloakAdmissionProvider returns nil, nil when the Admission config is
// entirely empty (persistent MCP credentials disabled). A partial or invalid
// configuration is a startup error.
func newKeycloakAdmissionProvider(cfg CorporateConfig, tokenURL string, client *http.Client) (mcpAdmissionProvider, error) {
	adminURL := strings.TrimSpace(cfg.Admission.AdminURL)
	clientID := cfg.Admission.ClientID
	secret := cfg.Admission.ClientSecret
	tokenURL = strings.TrimSpace(tokenURL)
	if adminURL == "" && clientID == "" && secret == "" {
		return nil, nil
	}
	if adminURL == "" || clientID == "" || secret == "" {
		return nil, errors.New("admission: admin URL, client id and client secret must be configured together")
	}
	if tokenURL == "" {
		return nil, errors.New("admission: token endpoint is required")
	}
	if r := strings.TrimSpace(cfg.RolesClaim); r != "" && r != authDefaultRolesClaim {
		return nil, fmt.Errorf("admission: only the %s roles claim is supported", authDefaultRolesClaim)
	}
	issuer := strings.TrimSpace(cfg.IssuerURL)
	if issuer == "" {
		return nil, errors.New("admission: issuer URL is required")
	}
	if err := authCheckEndpoint(tokenURL, cfg.AllowInsecureHTTP); err != nil {
		return nil, fmt.Errorf("admission: token endpoint: %w", err)
	}
	if err := authCheckEndpoint(adminURL, cfg.AllowInsecureHTTP); err != nil {
		return nil, fmt.Errorf("admission: admin URL: %w", err)
	}
	if err := authCheckEndpoint(issuer, cfg.AllowInsecureHTTP); err != nil {
		return nil, fmt.Errorf("admission: issuer: %w", err)
	}
	admin, err := keycloakDeriveAdminURL(issuer)
	if err != nil {
		return nil, err
	}
	given, err := url.Parse(adminURL)
	if err != nil || strings.ContainsAny(adminURL, "?#\\%") ||
		!strings.EqualFold(given.Scheme, admin.Scheme) || !strings.EqualFold(given.Host, admin.Host) ||
		given.Path != admin.Path || given.RawPath != "" || given.RawQuery != "" || given.ForceQuery ||
		given.Fragment != "" || given.User != nil {
		return nil, errors.New("admission: admin URL does not match the issuer realm")
	}
	role := cfg.AccessRole
	if role == "" {
		role = authDefaultAccessRole
	}

	guarded := keycloakGuardedClient(client, cfg.AllowInsecureHTTP)
	return &keycloakAdmission{
		issuer:     issuer,
		adminBase:  admin,
		accessRole: role,
		client:     guarded,
		tokens: &keycloakTokenCache{
			client: guarded,
			cfg: &clientcredentials.Config{
				ClientID:     clientID,
				ClientSecret: secret,
				TokenURL:     tokenURL,
				AuthStyle:    oauth2.AuthStyleInHeader,
			},
		},
	}, nil
}

// keycloakDeriveAdminURL maps https://host[/prefix]/realms/<realm> to
// https://host[/prefix]/admin/realms/<realm>.
func keycloakDeriveAdminURL(issuer string) (*url.URL, error) {
	bad := errors.New("admission: issuer must look like https://host[/prefix]/realms/<realm>")
	u, err := url.Parse(issuer)
	if err != nil || strings.ContainsAny(issuer, "?#\\%") || u.RawQuery != "" || u.ForceQuery ||
		u.Fragment != "" || u.User != nil || u.RawPath != "" {
		return nil, bad
	}
	i := strings.LastIndex(u.Path, "/realms/")
	if i < 0 {
		return nil, bad
	}
	prefix, realm := u.Path[:i], u.Path[i+len("/realms/"):]
	if realm == "" || strings.Contains(realm, "/") {
		return nil, bad
	}
	for _, seg := range strings.Split(strings.TrimPrefix(u.Path, "/"), "/") {
		if seg == "" || seg == "." || seg == ".." {
			return nil, bad
		}
	}
	return &url.URL{Scheme: u.Scheme, Host: u.Host, Path: prefix + "/admin/realms/" + realm}, nil
}

// keycloakGuardedClient never follows redirects (the machine bearer must not
// be forwarded) and applies the endpoint policy to every request.
func keycloakGuardedClient(in *http.Client, allowInsecure bool) *http.Client {
	var base http.RoundTripper
	timeout := keycloakAdmissionTimeout
	if in != nil {
		base = in.Transport
		if in.Timeout > 0 && in.Timeout < timeout {
			timeout = in.Timeout
		}
	}
	if base == nil {
		base = http.DefaultTransport.(*http.Transport).Clone()
	}
	return &http.Client{
		Timeout:   timeout,
		Transport: &authGuardTransport{base: base, allowInsecure: allowInsecure},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

type keycloakTokenCache struct {
	client *http.Client
	cfg    *clientcredentials.Config

	mu  sync.Mutex
	tok *oauth2.Token
}

func (c *keycloakTokenCache) token(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.tok.Valid() {
		return c.tok.AccessToken, nil
	}
	c.tok = nil
	t, err := c.cfg.Token(context.WithValue(ctx, oauth2.HTTPClient, c.client))
	if err != nil || t == nil || t.AccessToken == "" || !strings.EqualFold(t.Type(), "Bearer") {
		return "", errMCPAdmissionUnavailable
	}
	if !t.Expiry.IsZero() {
		c.tok = t
	}
	return t.AccessToken, nil
}

func (c *keycloakTokenCache) invalidate() {
	c.mu.Lock()
	c.tok = nil
	c.mu.Unlock()
}

// keycloakValidSubject accepts any single, plain path segment. Keycloak
// federation ids are not necessarily UUIDs.
func keycloakValidSubject(s string) bool {
	if s == "" || len(s) > keycloakAdmissionMaxSubject || s == "." || s == ".." || !utf8.ValidString(s) {
		return false
	}
	if strings.ContainsAny(s, "/\\?#%") {
		return false
	}
	for _, r := range s {
		if unicode.IsControl(r) || r == utf8.RuneError {
			return false
		}
	}
	return true
}

func (p *keycloakAdmission) Check(ctx context.Context, issuer, subject string) error {
	if issuer != p.issuer || !keycloakValidSubject(subject) {
		return errMCPAdmissionDenied
	}
	ctx, cancel := context.WithTimeout(ctx, keycloakAdmissionTimeout)
	defer cancel()

	token, err := p.tokens.token(ctx)
	if err != nil {
		return p.fail(ctx)
	}

	var user struct {
		ID      *string `json:"id"`
		Enabled *bool   `json:"enabled"`
	}
	status, err := p.get(ctx, token, []string{"users", subject}, &user)
	if err != nil {
		return p.fail(ctx)
	}
	switch status {
	case http.StatusOK:
	case http.StatusNotFound:
		return errMCPAdmissionDenied
	default:
		return p.fail(ctx)
	}
	if user.ID == nil || *user.ID != subject || user.Enabled == nil || !*user.Enabled {
		return errMCPAdmissionDenied
	}

	var roles []struct {
		Name *string `json:"name"`
	}
	status, err = p.get(ctx, token, []string{"users", subject, "role-mappings", "realm", "composite"}, &roles)
	if err != nil {
		return p.fail(ctx)
	}
	switch status {
	case http.StatusOK:
	case http.StatusNotFound:
		return errMCPAdmissionDenied
	default:
		return p.fail(ctx)
	}
	for _, r := range roles {
		if r.Name != nil && *r.Name == p.accessRole {
			return nil
		}
	}
	return errMCPAdmissionDenied
}

// fail reports a provider fault without leaking URLs, bodies or secrets.
func (p *keycloakAdmission) fail(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("%w: %w", errMCPAdmissionUnavailable, err)
	}
	return errMCPAdmissionUnavailable
}

// get performs an authenticated GET below the admin URL. The body is decoded
// into out only for a 200 response.
func (p *keycloakAdmission) get(ctx context.Context, token string, segments []string, out any) (int, error) {
	u := *p.adminBase
	u.Path += "/" + strings.Join(segments, "/")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return 0, errMCPAdmissionUnavailable
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	resp, err := p.client.Do(req)
	if err != nil {
		return 0, errMCPAdmissionUnavailable
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		p.tokens.invalidate()
	}
	if resp.StatusCode != http.StatusOK {
		if resp.StatusCode != http.StatusNotFound {
			log.Printf("mcp admission: admin API returned HTTP %d", resp.StatusCode)
		}
		io.Copy(io.Discard, io.LimitReader(resp.Body, authMaxBody))
		return resp.StatusCode, nil
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, authMaxBody+1))
	if err != nil || len(body) > authMaxBody {
		return 0, errMCPAdmissionUnavailable
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	if err := dec.Decode(out); err != nil {
		return 0, errMCPAdmissionUnavailable
	}
	if _, err := dec.Token(); err != io.EOF { // exactly one JSON value
		return 0, errMCPAdmissionUnavailable
	}
	return resp.StatusCode, nil
}
