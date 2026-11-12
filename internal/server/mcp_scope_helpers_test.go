package server

import (
	"context"
	"encoding/json"
	"fmt"
)

// projScope builds a scope of project elements, all with the same mode.
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

func scopeJSONText(scope []mcpScopeElement) string {
	b, _ := json.Marshal(scope)
	return string(b)
}

// scopedProjects pins a read-mode project scope (nil: unrestricted).
func (d *database) scopedProjects(ctx context.Context, userID string, projects []string) (*database, func(), error) {
	if projects != nil && len(projects) == 0 {
		return nil, nil, fmt.Errorf("empty scope")
	}
	return d.scopedMCP(ctx, userID, projScope("read", projects...))
}
