package server

import (
	"context"
	"embed"
	"errors"
	"time"
)

//go:embed mcp_scope.sql mcp_persistent.sql mcp_oauth_audit.sql mcp_login_window.sql mcp_credential_scope_edit.sql mcp_credential_scope_v6.sql
var mcpScopeSchema embed.FS

// migrationVersionCap limits applied versions (0 = no limit). Tests use it to build a
// database as it was before a release and upgrade it afterwards.
var mcpScopeVersionCap, roleScopeVersionCap int

func capMigrations(list []pgMigration, limit int) []pgMigration {
	if limit <= 0 {
		return list
	}
	out := list[:0:0]
	for _, m := range list {
		if m.Version <= limit {
			out = append(out, m)
		}
	}
	return out
}

func (s *Server) migrateMCPScope(ctx context.Context) error {
	if s == nil || s.DB == nil || !s.DB.pg {
		return errors.New("MCP scope migration requires PostgreSQL")
	}
	schema, err := mcpScopeSchema.ReadFile("mcp_scope.sql")
	if err != nil {
		return err
	}
	persistent, err := mcpScopeSchema.ReadFile("mcp_persistent.sql")
	if err != nil {
		return err
	}
	oauthAudit, err := mcpScopeSchema.ReadFile("mcp_oauth_audit.sql")
	if err != nil {
		return err
	}
	loginWindow, err := mcpScopeSchema.ReadFile("mcp_login_window.sql")
	if err != nil {
		return err
	}
	scopeEdit, err := mcpScopeSchema.ReadFile("mcp_credential_scope_edit.sql")
	if err != nil {
		return err
	}
	scopeV6, err := mcpScopeSchema.ReadFile("mcp_credential_scope_v6.sql")
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	return s.DB.runMigrations(ctx, "mcp_access", capMigrations([]pgMigration{{
		Version: 1, Name: "exact MCP project read scope", Statements: splitCorporateSQL(string(schema)),
	}, {Version: 2, Name: "persistent MCP credentials and bounded content writes", Statements: splitCorporateSQL(string(persistent))}, {Version: 3, Name: "OAuth bearer request audit", Statements: splitCorporateSQL(string(oauthAudit))}, {Version: 4, Name: "last login time for local MCP credential admission", Statements: splitCorporateSQL(string(loginWindow))}, {Version: 5, Name: "checked owner edits of MCP credential projects and mode", Statements: splitCorporateSQL(string(scopeEdit))}, {Version: 6, Name: "per-element MCP key scope (spaces and projects with own modes)", Statements: splitCorporateSQL(string(scopeV6))}}, mcpScopeVersionCap))
}
