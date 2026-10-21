package server

import "time"

const mcpPersistentPrefix = "aiwiki_pat_"

type mcpPersistentAccess struct {
	db        *database // separate authentication pool, never the request-scoped view
	admission mcpAdmissionProvider
	now       func() time.Time
}

type mcpCredential struct {
	ID               string     `json:"id"`
	Name             string     `json:"name"`
	OwnerID          string     `json:"-"`
	ServiceAccountID string     `json:"service_account_id,omitempty"`
	ProjectIDs       []string   `json:"project_ids"`
	Mode             string     `json:"mode"`
	CreatedAt        time.Time  `json:"created_at"`
	ExpiresAt        time.Time  `json:"expires_at"`
	RevokedAt        *time.Time `json:"revoked_at,omitempty"`
}

type mcpServiceAccount struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	SponsorID string    `json:"sponsor_id"`
	Disabled  bool      `json:"disabled"`
	CreatedAt time.Time `json:"created_at"`
}

func mcpScopedTool(p *Principal, name string) bool {
	if p == nil || p.MCPProjects == nil || mcpReadTool(name) {
		return true
	}
	if p.MCPMode != "write" {
		return false
	}
	switch name {
	case "wiki_write", "wiki_edit", "wiki_comment", "wiki_restore":
		return true
	}
	return false
}
