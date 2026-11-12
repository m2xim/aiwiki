package server

import (
	"context"
	"encoding/json"
	"fmt"
)

// projScope builds a scope of (legacy) project elements, all with the same mode; the SQL functions
// still understand them although folders no longer exist.
func projScope(mode string, projects ...string) []mcpScopeElement {
	if projects == nil {
		return nil
	}
	out := make([]mcpScopeElement, 0, len(projects))
	for _, p := range projects {
		out = append(out, mcpScopeElement{Type: "project", ID: p, Mode: mode})
	}
	return out
}

// spaceScope builds a session scope of space elements, all with the same mode.
func spaceScope(mode string, spaces ...string) []mcpScopeElement {
	if spaces == nil {
		return nil
	}
	out := make([]mcpScopeElement, 0, len(spaces))
	for _, p := range spaces {
		out = append(out, mcpScopeElement{Type: "space", ID: p, Mode: mode})
	}
	return out
}

func scopeJSONText(scope []mcpScopeElement) string {
	b, _ := json.Marshal(scope)
	return string(b)
}

// scopedSpaces pins a read-mode space scope (nil: unrestricted).
func (d *database) scopedProjects(ctx context.Context, userID string, projects []string) (*database, func(), error) {
	if projects != nil && len(projects) == 0 {
		return nil, nil, fmt.Errorf("empty scope")
	}
	return d.scopedMCP(ctx, userID, projScope("read", projects...))
}

func (d *database) scopedSpaces(ctx context.Context, userID string, spaces []string) (*database, func(), error) {
	if spaces != nil && len(spaces) == 0 {
		return nil, nil, fmt.Errorf("empty scope")
	}
	return d.scopedMCP(ctx, userID, spaceScope("read", spaces...))
}
