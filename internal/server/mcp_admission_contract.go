package server

import (
	"context"
	"errors"
)

// KeycloakAdmissionConfig uses a separate read-only machine client. An empty
// configuration disables persistent MCP credentials; it never weakens OIDC.
type KeycloakAdmissionConfig struct {
	AdminURL     string // https://host[/prefix]/admin/realms/<realm>
	ClientID     string
	ClientSecret string
}

var errMCPAdmissionDenied = errors.New("MCP owner is not admitted")

// Check must query current provider state on every call. Only the machine
// client's own access token may be cached, never a positive admission result.
type mcpAdmissionProvider interface {
	Check(context.Context, string, string) error // issuer, immutable subject
}
