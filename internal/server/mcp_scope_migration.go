package server

import (
	"context"
	"embed"
	"errors"
	"time"
)

//go:embed mcp_scope.sql mcp_persistent.sql
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
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	return s.DB.runMigrations(ctx, "mcp_access", []pgMigration{{
		Version: 1, Name: "exact MCP project read scope", Statements: splitCorporateSQL(string(schema)),
	}, {Version: 2, Name: "persistent MCP credentials and bounded content writes", Statements: splitCorporateSQL(string(persistent))}})
}
