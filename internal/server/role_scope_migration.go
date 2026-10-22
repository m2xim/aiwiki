package server

import (
	"context"
	_ "embed"
	"errors"
)

//go:embed role_scope.sql
var roleScopeSQL string

// migrateRoleScope adds corporate_role_scope() and switches the SELECT policies to
// the set-based check (component "role_scope", append-only on top of corporate_acl,
// mcp_scope, revision_columns). corporate_effective_role stays the reference.
// A later migration that recreates any of the 8 SELECT policies listed in
// role_scope_guard.go must keep the set-based predicate; the startup guard refuses to
// start otherwise.
func (s *Server) migrateRoleScope(ctx context.Context) error {
	if s == nil || s.DB == nil || !s.DB.pg {
		return errors.New("role scope migration requires PostgreSQL")
	}
	return s.DB.runMigrations(ctx, "role_scope", []pgMigration{{
		Version: 1, Name: "set-based read scope for RLS", Statements: splitCorporateSQL(roleScopeSQL),
	}})
}
