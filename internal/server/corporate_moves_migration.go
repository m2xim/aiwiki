package server

import (
	"context"
	"embed"
	"errors"
)

//go:embed corporate_moves.sql
var corporateMovesSchema embed.FS

// migrateCorporateMoves adds the access-impact function and the move guards
// (component corporate_moves, append-only; corporate_acl v1 is not edited).
func (s *Server) migrateCorporateMoves(ctx context.Context) error {
	if s == nil || s.DB == nil || !s.DB.pg {
		return errors.New("safe-move migration requires PostgreSQL")
	}
	schema, err := corporateMovesSchema.ReadFile("corporate_moves.sql")
	if err != nil {
		return err
	}
	return s.DB.runMigrations(ctx, "corporate_moves", []pgMigration{{
		Version: 1, Name: "access-impact preview and guarded moves", Statements: splitCorporateSQL(string(schema)),
	}})
}
