package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
)

const (
	mcpOAuthMaxURL       = 2048
	mcpOAuthResourcePath = "/mcp"
	mcpOAuthMetadataPath = "/.well-known/oauth-protected-resource"
)

var errMCPOAuthResource = errors.New("mcp oauth: invalid resource URL")

// normalizeMCPOAuthConfig canonicalizes MCPOAuth.ResourceURL. An empty value
// disables the feature and returns cfg unchanged. cfg.RedirectURL is expected
// to have passed normalizeAuthConfig already. Errors never echo URLs.
func normalizeMCPOAuthConfig(cfg CorporateConfig) (CorporateConfig, error) {
	id, secret := cfg.MCPOAuth.IntrospectionClientID, cfg.MCPOAuth.IntrospectionClientSecret
	if (id == "") != (secret == "") || (id != "" && (strings.TrimSpace(id) != id || strings.TrimSpace(secret) == "" || cfg.MCPOAuth.ResourceURL == "")) {
		return cfg, errors.New("mcp oauth: invalid introspection credentials")
	}
	raw := strings.TrimSpace(cfg.MCPOAuth.ResourceURL)
	if raw == "" {
		if cfg.MCPOAuth.ResourceURL != "" {
			return cfg, errMCPOAuthResource
		}
		return cfg, nil
	}
	if len(raw) > mcpOAuthMaxURL {
		return cfg, errMCPOAuthResource
	}
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		if c <= ' ' || c >= 0x7f || c == '\\' || c == '%' || c == '@' {
			return cfg, errMCPOAuthResource
		}
	}
	u, err := url.Parse(raw)
	if err != nil || u.Opaque != "" || u.User != nil || u.Host == "" || u.Hostname() == "" ||
		u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawFragment != "" ||
		u.Path != mcpOAuthResourcePath || u.RawPath != "" {
		return cfg, errMCPOAuthResource
	}
	origin, err := canonicalAllowedOrigin(u.Scheme + "://" + u.Host)
	if err != nil {
		return cfg, errMCPOAuthResource
	}
	if err := authCheckEndpoint(origin, cfg.AllowInsecureHTTP); err != nil {
		return cfg, errMCPOAuthResource
	}
	redirect, err := url.Parse(strings.TrimSpace(cfg.RedirectURL))
	if err != nil || redirect.Host == "" {
		return cfg, errMCPOAuthResource
	}
	redirectOrigin, err := canonicalAllowedOrigin(redirect.Scheme + "://" + redirect.Host)
	if err != nil || redirectOrigin != origin {
		return cfg, errMCPOAuthResource
	}
	cfg.MCPOAuth.ResourceURL = origin + mcpOAuthResourcePath
	return cfg, nil
}

// mcpOAuthResource returns the canonical resource URL, or "" when the feature
// is disabled or the configuration is not valid.
func (a *AuthService) mcpOAuthResource() string {
	if a == nil || strings.TrimSpace(a.cfg.MCPOAuth.ResourceURL) == "" {
		return ""
	}
	cfg, err := normalizeMCPOAuthConfig(a.cfg)
	if err != nil {
		return ""
	}
	return cfg.MCPOAuth.ResourceURL
}

// mcpOAuthRoutes serves RFC 9728 protected-resource metadata. The metadata is
// static, built from configuration only, and never derived from the request.
func (a *AuthService) mcpOAuthRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET "+mcpOAuthMetadataPath, a.handleMCPOAuthMetadata)
	mux.HandleFunc("GET "+mcpOAuthMetadataPath+mcpOAuthResourcePath, a.handleMCPOAuthMetadata)
}

func (a *AuthService) handleMCPOAuthMetadata(w http.ResponseWriter, r *http.Request) {
	resource := a.mcpOAuthResource()
	if resource == "" {
		http.NotFound(w, r)
		return
	}
	body, err := json.Marshal(mcpOAuthMetadata{
		Resource:               resource,
		AuthorizationServers:   []string{strings.TrimSpace(a.cfg.IssuerURL)},
		ScopesSupported:        []string{mcpOAuthScope},
		BearerMethodsSupported: []string{"header"},
	})
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("Cache-Control", "public, max-age=300")
	h.Set("X-Content-Type-Options", "nosniff")
	if r.Method == http.MethodHead {
		return
	}
	w.Write(body)
}

// mcpBearerChallenge builds the WWW-Authenticate value for MCP responses.
// errorName is only emitted when it is invalid_token or insufficient_scope.
func (a *AuthService) mcpBearerChallenge(errorName string) string {
	var params []string
	switch errorName {
	case "invalid_token", "insufficient_scope":
		params = append(params, `error="`+errorName+`"`)
	}
	resource := a.mcpOAuthResource()
	if resource == "" {
		if len(params) == 0 {
			return "Bearer"
		}
		return "Bearer " + params[0]
	}
	origin := strings.TrimSuffix(resource, mcpOAuthResourcePath)
	params = append(params,
		`resource_metadata="`+origin+mcpOAuthMetadataPath+mcpOAuthResourcePath+`"`,
		`scope="`+mcpOAuthScope+`"`)
	return "Bearer " + strings.Join(params, ", ")
}

// authScopeContains reports whether v is a well-formed RFC 6749 scope string
// (space-separated tokens) that contains want as an exact token.
func authScopeContains(v any, want string) bool {
	s, ok := v.(string)
	if !ok || want == "" || !authScopeToken(want) {
		return false
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; c != ' ' && !authScopeChar(c) {
			return false
		}
	}
	for _, tok := range strings.Split(s, " ") {
		if tok == want {
			return true
		}
	}
	return false
}

func authScopeToken(s string) bool {
	for i := 0; i < len(s); i++ {
		if !authScopeChar(s[i]) {
			return false
		}
	}
	return s != ""
}

// authScopeChar is scope-token's nqchar: %x21 / %x23-5B / %x5D-7E.
func authScopeChar(c byte) bool {
	return c == 0x21 || (c >= 0x23 && c <= 0x5b) || (c >= 0x5d && c <= 0x7e)
}
