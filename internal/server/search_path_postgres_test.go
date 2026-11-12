package server

import (
	"context"
	"testing"
)

// A temporary object of the runtime role must never run inside owner code. Before role_scope v3 a
// pg_temp.text domain whose CHECK calls a pg_temp function was evaluated as the table owner inside
// every SECURITY DEFINER function that declared a text variable.
func TestPostgresTempSchemaCannotHijackDefinerFunctions(t *testing.T) {
	owner, app := corporateFixture(t)
	ctx := context.Background()
	tx, err := app.DB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	for _, q := range []string{
		`SELECT set_config('app.user_id','nobody',true)`,
		`CREATE FUNCTION pg_temp.hook(v pg_catalog.text) RETURNS boolean LANGUAGE plpgsql AS $$ BEGIN IF current_user<>session_user THEN RAISE EXCEPTION 'hijacked as %', current_user; END IF; RETURN true; END $$`,
		`CREATE DOMAIN pg_temp.text AS pg_catalog.text CHECK (pg_temp.hook(VALUE))`,
	} {
		if _, err = tx.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	for _, q := range []string{
		`SELECT count(*) FROM corporate_agent_projects_visible(NULL)`,
		`SELECT count(*) FROM corporate_agent_project_rows(NULL)`,
		`SELECT corporate_mcp_project_scope('x','y') IS NULL`,
		`SELECT count(*) FROM corporate_role_scope()`,
		`SELECT corporate_ap_space_addable('x')`,
	} {
		if _, err = tx.ExecContext(ctx, `SAVEPOINT p`); err != nil {
			t.Fatal(err)
		}
		if _, err = tx.ExecContext(ctx, q); err != nil {
			t.Errorf("%s: %v", q, err)
		}
		if _, err = tx.ExecContext(ctx, `ROLLBACK TO SAVEPOINT p`); err != nil {
			t.Fatal(err)
		}
	}
	// No function in public pins a search_path without pg_temp last, and the startup guard enforces it.
	var bad int
	if err = owner.DB.QueryRow(`SELECT count(*) FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace CROSS JOIN LATERAL unnest(p.proconfig) c WHERE n.nspname='public' AND c LIKE 'search_path=%' AND c !~ '(=|,\s*)pg_temp\s*$'`).Scan(&bad); err != nil || bad != 0 {
		t.Fatalf("functions without pg_temp last: %d %v", bad, err)
	}
	if _, err = owner.DB.Exec(`ALTER FUNCTION corporate_ap_live_user() SET search_path=pg_catalog,public`); err != nil {
		t.Fatal(err)
	}
	if err = app.DB.validateRuntimeSchema(); err == nil {
		t.Error("startup guard accepted a definer function without pg_temp last")
	}
	if _, err = owner.DB.Exec(`ALTER FUNCTION corporate_ap_live_user() SET search_path=pg_catalog,public,pg_temp`); err != nil {
		t.Fatal(err)
	}
	if err = app.DB.validateRuntimeSchema(); err != nil {
		t.Fatalf("guard after repair: %v", err)
	}
}
