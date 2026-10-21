package server

import "time"

// CorporateConfig enables the isolated PostgreSQL/OIDC edition.
type CorporateConfig struct {
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
}
