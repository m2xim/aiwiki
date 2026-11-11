package server

import (
	"strings"
	"time"
)

const mcpPersistentPrefix = "aiwiki_pat_"

type mcpPersistentAccess struct {
	db  *database // separate authentication pool, never the request-scoped view
	now func() time.Time
	// checkpoint, when set (tests only), runs between the rate-limited first
	// verification and the final re-read of a credential.
	checkpoint func()
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
	case "wiki_write", "wiki_edit", "wiki_comment", "wiki_restore", "wiki_artifact_write":
		return true
	}
	return false
}

func mcpReadTool(name string) bool {
	switch name {
	case "wiki_bootstrap", "wiki_search", "wiki_search_sections", "wiki_read", "wiki_links", "wiki_history", "wiki_comments", "wiki_artifact_list", "wiki_artifact_read":
		return true
	}
	return false
}

func mcpAuditName(value string) string {
	value = strings.ReplaceAll(strings.ToValidUTF8(value, ""), "\x00", "")
	runes := []rune(value)
	if len(runes) > 100 {
		return string(runes[:100])
	}
	return value
}
