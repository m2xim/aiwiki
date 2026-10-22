package main

import (
	"fmt"
	"os"
)

// runMigrateFromSQLite imports a trusted-mode data directory into a
// personal-mode PostgreSQL database and bucket (docs/PERSONAL.md).
func runMigrateFromSQLite(args []string) int {
	fmt.Fprintln(os.Stderr, "migrate-from-sqlite: not implemented yet")
	return 2
}
