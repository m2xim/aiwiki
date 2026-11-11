package server

import (
	"context"
	"embed"
	"errors"
	"time"
)

//go:embed mcp_scope.sql mcp_persistent.sql mcp_oauth_audit.sql mcp_login_window.sql mcp_credential_scope_edit.sql
var mcpScopeSchema embed.FS

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
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	return s.DB.runMigrations(ctx, "mcp_access", []pgMigration{{
		Version: 1, Name: "exact MCP project read scope", Statements: splitCorporateSQL(string(schema)),
	}, {Version: 2, Name: "persistent MCP credentials and bounded content writes", Statements: splitCorporateSQL(string(persistent))}, {Version: 3, Name: "OAuth bearer request audit", Statements: splitCorporateSQL(string(oauthAudit))}, {Version: 4, Name: "last login time for local MCP credential admission", Statements: splitCorporateSQL(string(loginWindow))}, {Version: 5, Name: "checked owner edits of MCP credential projects and mode", Statements: splitCorporateSQL(string(scopeEdit))}})
}
