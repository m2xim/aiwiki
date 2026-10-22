package server

import (
	"strings"
	"time"
)

// Temporary delegations expire with the browser session that issued them.
// They are intentionally not long-lived PATs or service-account credentials.
const mcpDelegationPrefix = "aiwiki_tmp_"

type mcpDelegation struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	OwnerID     string    `json:"-"`
	Projects    []string  `json:"project_ids"`
	Created     time.Time `json:"created_at"`
	Expires     time.Time `json:"expires_at"`
	SessionKey  [32]byte  `json:"-"`
	windowStart time.Time
	requests    int
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

// Called only with a.mu held; every child dies with its issuing session.
func (a *AuthService) dropDelegationsLocked(session [32]byte) {
	for key, grant := range a.delegations {
		if grant.SessionKey == session {
			delete(a.delegations, key)
		}
	}
}
