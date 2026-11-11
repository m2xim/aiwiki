package server

import (
	"sort"
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
	ID               string         `json:"id"`
	Name             string         `json:"name"`
	OwnerID          string         `json:"-"`
	ServiceAccountID string         `json:"service_account_id,omitempty"`
	Scope            []mcpScopeView `json:"scope"`
	CreatedAt        time.Time      `json:"created_at"`
	ExpiresAt        time.Time      `json:"expires_at"`
	RevokedAt        *time.Time     `json:"revoked_at,omitempty"`
}

type mcpServiceAccount struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	SponsorID string    `json:"sponsor_id"`
	Disabled  bool      `json:"disabled"`
	CreatedAt time.Time `json:"created_at"`
}

// mcpScopeElement is one entry of a key scope: a whole space or a project (with its
// subprojects), each with its own mode. There is no key-level mode.
type mcpScopeElement struct {
	Type string `json:"type"`
	ID   string `json:"id"`
	Mode string `json:"mode"`
}

// mcpScopeView is the element as shown to clients, with display data.
type mcpScopeView struct {
	Type    string `json:"type"`
	ID      string `json:"id"`
	Mode    string `json:"mode"`
	Name    string `json:"name"`
	SpaceID string `json:"space_id"`
}

func (e mcpScopeElement) key() string { return e.Type + "\x00" + e.ID }

// mcpScopeHasWrite reports whether any element is writable.
func mcpScopeHasWrite(scope []mcpScopeElement) bool {
	for _, e := range scope {
		if e.Mode == "write" {
			return true
		}
	}
	return false
}

// mcpScopeNormalize validates and canonically orders a scope. ok is false for shape errors.
func mcpScopeNormalize(in []mcpScopeElement) ([]mcpScopeElement, bool) {
	if len(in) < 1 {
		return nil, false
	}
	seen := make(map[string]bool, len(in))
	out := make([]mcpScopeElement, 0, len(in))
	for _, e := range in {
		if (e.Type != "space" && e.Type != "project") || (e.Mode != "read" && e.Mode != "write") || !validPathID(e.ID) || seen[e.key()] {
			return nil, false
		}
		seen[e.key()] = true
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].key() < out[j].key() })
	return out, true
}

func mcpScopedTool(p *Principal, name string) bool {
	if p == nil || p.MCPScope == nil || mcpReadTool(name) {
		return true
	}
	if !mcpScopeHasWrite(p.MCPScope) {
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
