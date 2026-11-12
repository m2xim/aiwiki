package server

import (
	"context"
	_ "embed"
	"errors"
)

//go:embed agent_projects.sql
var agentProjectsSQL string

// agentProjectsVersionCap limits applied versions (0 = no limit, negative = none); tests build the
// previous state with it.
var agentProjectsVersionCap int

// migrateAgentProjects removes the projects inside spaces from the model and adds agent projects
// (component "agent_projects", append-only; it runs before role_scope, which stays the last
// migration). mcp_access v8 (agent_project_ids, read_only on keys) is a prerequisite and has run before.
func (s *Server) migrateAgentProjects(ctx context.Context) error {
	if s == nil || s.DB == nil || !s.DB.pg {
		return errors.New("agent projects migration requires PostgreSQL")
	}
	// mcp_access v8 (agent_project_ids on keys) is a prerequisite: a database held back below it, or an
	// explicit negative cap, stops here (tests build the previous release this way).
	if agentProjectsVersionCap < 0 || mcpScopeVersionCap > 0 && mcpScopeVersionCap < 8 {
		return nil
	}
	return s.DB.runMigrations(ctx, "agent_projects", capMigrations([]pgMigration{{
		Version: 1, Name: "agent projects; projects inside spaces removed", Statements: splitCorporateSQL(agentProjectsSQL),
	}}, agentProjectsVersionCap))
}
