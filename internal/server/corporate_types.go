package server

import "time"

// CorporateConfig enables the isolated PostgreSQL/OIDC edition.
type CorporateConfig struct {
	MCPOAuth             MCPOAuthConfig
	Admission            KeycloakAdmissionConfig
	S3                   S3Config
	DatabaseURL          string
	MigrationDatabaseURL string
	RuntimeRole          string
	IssuerURL            string
	ClientID             string
	ClientSecret         string
	RedirectURL          string
	AccessRole           string
	AdminRole            string
	RolesClaim           string
	SessionTTL           time.Duration
	AllowInsecureHTTP    bool
}

type Principal struct {
	ID      string `json:"id"`
	Issuer  string `json:"-"`
	Subject string `json:"-"`
	Name    string `json:"name"`
	Admin   bool   `json:"admin"`
	// nil means normal OIDC access; a non-nil set is an exact project scope.
	MCPProjects     []string  `json:"-"`
	MCPDelegationID string    `json:"-"`
	MCPExpiresAt    time.Time `json:"-"`
	MCPMode         string    `json:"-"`
	MCPActorID      string    `json:"-"` // distinct service-account authorship; ID remains sponsor for ACLs
	MCPBearer       bool      `json:"-"` // signed in with a provider bearer token on /mcp
	MCPClientID     string    `json:"-"` // azp/client_id of that token, sanitised; "unknown" when absent
}
