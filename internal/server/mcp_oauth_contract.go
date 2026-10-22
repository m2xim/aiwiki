package server

// MCPOAuthConfig enables OAuth resource discovery and a separate, strictly
// resource-bound bearer-token audience. Keycloak remains the authorization
// server: aiwiki never proxies registration, authorization or token issuance.
type MCPOAuthConfig struct {
	ResourceURL string // canonical public URL, same origin as RedirectURL, path /mcp
	// Optional separate resource-server credentials for live introspection.
	// Both must be supplied together; browser sessions keep their own credentials.
	IntrospectionClientID     string
	IntrospectionClientSecret string
}

const mcpOAuthScope = "aiwiki:mcp"

type mcpOAuthMetadata struct {
	Resource               string   `json:"resource"`
	AuthorizationServers   []string `json:"authorization_servers"`
	ScopesSupported        []string `json:"scopes_supported"`
	BearerMethodsSupported []string `json:"bearer_methods_supported"`
}
