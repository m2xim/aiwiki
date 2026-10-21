package server

import "errors"

func (d *database) validateRuntimeSchema() error {
	var count int
	if err := d.QueryRow(`SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='public' AND c.relname IN ('spaces','articles','revisions','comments','attachments','favorites','article_fts') AND (NOT c.relrowsecurity OR NOT c.relforcerowsecurity OR pg_has_role(current_user,c.relowner,'MEMBER'))`).Scan(&count); err != nil || count != 0 {
		return errors.New("runtime role must not own content tables and FORCE RLS must be enabled")
	}
	if err := d.QueryRow(`SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='public' AND c.relname IN ('spaces','articles','revisions','comments','attachments','favorites','article_fts')`).Scan(&count); err != nil || count != 7 {
		return errors.New("corporate schema is not initialized")
	}
	if err := d.QueryRow(`SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='public' AND c.relname IN ('users','teams','team_members','projects','memberships','security_audit','corporate_space_state','schema_migrations') AND pg_has_role(current_user,c.relowner,'MEMBER')`).Scan(&count); err != nil || count != 0 {
		return errors.New("runtime role must not own ACL, identity or migration tables")
	}
	var unsafe bool
	if err := d.QueryRow(`SELECT has_schema_privilege(current_user,'public','CREATE') OR has_table_privilege(current_user,'security_audit','UPDATE,DELETE') OR EXISTS(SELECT 1 FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='public' AND c.relkind IN ('r','p') AND has_table_privilege(current_user,c.oid,'TRUNCATE,TRIGGER')) OR EXISTS(SELECT 1 FROM pg_roles WHERE rolname=current_user AND (rolsuper OR rolbypassrls OR rolcreaterole))`).Scan(&unsafe); err != nil || unsafe {
		return errors.New("runtime role has unsafe schema, audit or role privileges")
	}
	return nil
}
