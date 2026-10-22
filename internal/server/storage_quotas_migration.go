package server

import (
	"context"
	_ "embed"
	"errors"
)

//go:embed storage_quotas.sql
var storageQuotasSchemaSQL string

// migrateStorageQuotas adds the per-space storage quota column, its guard and the
// accounting functions (component "storage_quotas", append-only on top of
// corporate_acl, corporate_moves and artifacts).
func (s *Server) migrateStorageQuotas(ctx context.Context) error {
	if s == nil || s.DB == nil || !s.DB.pg {
		return errors.New("storage quota migration requires PostgreSQL")
	}
	return s.DB.runMigrations(ctx, "storage_quotas", []pgMigration{{
		Version: 1, Name: "per-space storage quotas", Statements: splitCorporateSQL(storageQuotasSchemaSQL),
	}})
}
