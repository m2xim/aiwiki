package server

import (
	"context"
	_ "embed"
	"errors"
)

//go:embed revision_columns.sql
var revisionColumnsSQL string

// migrateRevisionColumns adds revisions.space_id/project_id, their fill trigger, the
// index and the column-based read policy (component "revision_columns", append-only
// on top of corporate_acl). The backfill and its check run in the migration's single
// transaction, so a bad row aborts the whole migration.
func (s *Server) migrateRevisionColumns(ctx context.Context) error {
	if s == nil || s.DB == nil || !s.DB.pg {
		return errors.New("revision columns migration requires PostgreSQL")
	}
	return s.DB.runMigrations(ctx, "revision_columns", []pgMigration{{
		Version: 1, Name: "revision space/project columns", Statements: splitCorporateSQL(revisionColumnsSQL),
	}})
}
