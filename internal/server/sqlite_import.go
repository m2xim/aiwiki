package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

// Identity of the single owner in personal mode (docs/PERSONAL.md, "Owner
// identity"). The personal server creates exactly the same rows.
const (
	personalOwnerID      = PersonalOwnerID
	personalOwnerIssuer  = PersonalIssuer
	personalOwnerSubject = PersonalOwnerID
)

// SQLiteImportConfig configures ImportSQLite ("aiwiki migrate-from-sqlite").
type SQLiteImportConfig struct {
	From   string // trusted-mode data directory (wiki.db, files/)
	DryRun bool
	// VerifyOnly writes nothing: it runs the verification against a target that was
	// imported earlier (for example after a commit whose outcome was unknown).
	VerifyOnly bool
	S3         S3Config
	// DatabaseURL is the runtime role, MigrationDatabaseURL and RuntimeRole are
	// the owner role and the runtime role name (as in corporate mode),
	// ImportDatabaseURL a role that bypasses row-level security.
	DatabaseURL          string
	MigrationDatabaseURL string
	RuntimeRole          string
	ImportDatabaseURL    string
	OwnerName            string // AIWIKI_PERSONAL_NAME
	Out                  io.Writer
}

// SQLiteImportReport is the outcome of an import that ran to the end.
type SQLiteImportReport struct {
	DryRun     bool
	Checks     int
	Mismatches int
	// Orphans are objects uploaded before a later step failed.
	Orphans []string
}

// Failed reports whether the verification found a mismatch (exit code 1).
func (r *SQLiteImportReport) Failed() bool { return r != nil && r.Mismatches > 0 }

type importRun struct {
	cfg SQLiteImportConfig
	out io.Writer
	rep *SQLiteImportReport
}

func (r *importRun) printf(format string, args ...any) { fmt.Fprintf(r.out, format+"\n", args...) }

// check records one verification line.
func (r *importRun) check(ok bool, name, detail string) {
	r.rep.Checks++
	status := "ok  "
	if !ok {
		status = "FAIL"
		r.rep.Mismatches++
	}
	r.printf("  %s %s %s", status, name, detail)
}

// openImportPostgres connects as the import role. Unlike openPostgres it
// requires a role that bypasses row-level security: that is its purpose.
func openImportPostgres(ctx context.Context, dsn string) (*sql.DB, error) {
	config, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, refuse("invalid AIWIKI_IMPORT_DATABASE_URL")
	}
	config.RuntimeParams["application_name"] = "aiwiki-import"
	config.RuntimeParams["lock_timeout"] = "30000"
	config.RuntimeParams["idle_in_transaction_session_timeout"] = "0"
	pool := stdlib.OpenDB(*config)
	pool.SetMaxOpenConns(2)
	pctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err = pool.PingContext(pctx); err != nil {
		pool.Close()
		return nil, errors.New("PostgreSQL is not reachable with AIWIKI_IMPORT_DATABASE_URL")
	}
	var bypass bool
	if err = pool.QueryRowContext(pctx, `SELECT coalesce(bool_or(rolsuper OR rolbypassrls),false) FROM pg_roles WHERE rolname=current_user`).Scan(&bypass); err != nil {
		pool.Close()
		return nil, fmt.Errorf("cannot verify the import role: %w", err)
	}
	if !bypass {
		pool.Close()
		return nil, refuse("AIWIKI_IMPORT_DATABASE_URL must name a superuser or BYPASSRLS role")
	}
	return pool, nil
}

func (cfg SQLiteImportConfig) validate() error {
	if strings.TrimSpace(cfg.From) == "" {
		return refuse("-from <data-dir> is required")
	}
	if strings.TrimSpace(cfg.S3.Bucket) == "" {
		return refuse("AIWIKI_S3_BUCKET is required")
	}
	if cfg.VerifyOnly {
		if cfg.DryRun {
			return refuse("-verify-only and -dry-run exclude each other")
		}
		if cfg.DatabaseURL == "" || cfg.ImportDatabaseURL == "" {
			return refuse("AIWIKI_DATABASE_URL and AIWIKI_IMPORT_DATABASE_URL are required")
		}
		return nil
	}
	if cfg.DatabaseURL == "" || cfg.MigrationDatabaseURL == "" || cfg.ImportDatabaseURL == "" {
		return refuse("AIWIKI_DATABASE_URL, AIWIKI_MIGRATION_DATABASE_URL and AIWIKI_IMPORT_DATABASE_URL are required")
	}
	if !validPathID(cfg.RuntimeRole) {
		return refuse("AIWIKI_DATABASE_ROLE (the runtime role name) is required")
	}
	return nil
}

// ImportSQLite copies a trusted-mode SQLite installation into a personal-mode
// PostgreSQL database and bucket (docs/PERSONAL.md, "Import"). A returned
// error means nothing was written (a refusal or an operational failure; the
// caller exits with 2); a report with Failed() means the data was imported but
// the verification found a mismatch (exit 1).
func ImportSQLite(ctx context.Context, cfg SQLiteImportConfig) (*SQLiteImportReport, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(cfg.OwnerName) == "" {
		cfg.OwnerName = PersonalDefaultName
	}
	out := cfg.Out
	if out == nil {
		out = io.Discard
	}
	run := &importRun{cfg: cfg, out: out, rep: &SQLiteImportReport{DryRun: cfg.DryRun}}

	tmp, err := os.MkdirTemp("", "aiwiki-import-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)
	run.printf("Reading the source %s (working on a copy)", cfg.From)
	src, err := loadImportSource(ctx, cfg.From, tmp)
	if err != nil {
		return nil, err
	}
	d := src.Data
	run.printf("  spaces %d, articles %d, revisions %d, comments %d, attachments %d, favorites %d, artifacts %d, files %d",
		len(d.Spaces), len(d.Articles), len(d.Revisions), len(d.Comments), len(d.Attachments), len(d.Favorites), len(d.Artifacts), len(src.Objects))

	store, err := newS3Store(ctx, cfg.S3)
	if err != nil {
		return nil, err
	}

	if cfg.VerifyOnly {
		imp, err := openImportPostgres(ctx, cfg.ImportDatabaseURL)
		if err != nil {
			return nil, err
		}
		defer imp.Close()
		run.printf("Verification only (runtime role scoped as the owner)")
		if err = run.verify(ctx, imp, store, src); err != nil {
			run.check(false, "verification", "could not run: "+err.Error())
		}
		return run.rep, nil
	}

	run.printf("Preparing the target schema (server migrations as the owner role)")
	if err = migrateAndGrant(ctx, cfg.MigrationDatabaseURL, cfg.RuntimeRole, "", ""); err != nil {
		return nil, err
	}
	imp, err := openImportPostgres(ctx, cfg.ImportDatabaseURL)
	if err != nil {
		return nil, err
	}
	defer imp.Close()

	tx, err := imp.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err = lockAndCheckEmpty(ctx, tx); err != nil {
		return nil, err
	}
	run.printf("Writing rows in one transaction")
	if err = run.writeRows(ctx, tx, src); err != nil {
		return nil, err
	}

	if cfg.DryRun {
		// Inside the same transaction the import role sees what it wrote.
		run.printf("Dry run: checking the rows in the transaction, then rolling back; nothing is uploaded")
		after, err := loadImpData(ctx, tx, true)
		if err != nil {
			return nil, err
		}
		run.compareData(src, after, true)
		if fts, ferr := ftsRowsOf(ctx, tx); ferr != nil {
			return nil, ferr
		} else {
			run.compareFTS(src, fts)
		}
		if err = tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
			return nil, err
		}
		run.printf("Would upload %d files (%s)", len(src.Objects), humanBytes(totalBytes(src.Objects)))
		run.printf("Dry run finished: the target was left unchanged")
		return run.rep, nil
	}

	run.printf("Uploading %d files (%s)", len(src.Objects), humanBytes(totalBytes(src.Objects)))
	for _, o := range src.Objects {
		if err = putImportObject(ctx, store, o); err != nil {
			return run.abort(store, err)
		}
		run.rep.Orphans = append(run.rep.Orphans, o.Key)
	}
	if bad := verifyObjects(ctx, store, src.Objects); len(bad) > 0 {
		return run.abort(store, fmt.Errorf("uploaded objects do not read back identically: %s", strings.Join(bad, "; ")))
	}
	// A sequence is not transactional: advance it last, right before the commit.
	if len(d.Revisions) > 0 {
		if _, err = tx.ExecContext(ctx, `SELECT setval(pg_get_serial_sequence('revisions','id'),(SELECT max(id) FROM revisions))`); err != nil {
			return run.abort(store, fmt.Errorf("revision sequence: %w", err))
		}
	}
	if err = tx.Commit(); err != nil {
		// The outcome of a failed COMMIT is unknown (the connection may have died
		// after the server applied it): never delete the uploaded objects here.
		return nil, fmt.Errorf("commit failed and its outcome is unknown (%v); the uploaded files were left in the bucket. Check the target, then run \"aiwiki migrate-from-sqlite -from %s -verify-only\": if it passes the import is complete, otherwise the target is empty and the import can be run again", err, cfg.From)
	}
	run.rep.Orphans = nil
	run.printf("Committed")

	run.printf("Verification (runtime role scoped as the owner)")
	if err = run.verify(ctx, imp, store, src); err != nil {
		// A verification that could not run is a failed verification.
		run.check(false, "verification", "could not run: "+err.Error())
	}
	return run.rep, nil
}

// abort ends a run before the commit: the transaction is rolled back (by the
// caller's deferred Rollback) and the objects uploaded so far are deleted, best
// effort; those that could not be deleted are listed.
func (r *importRun) abort(store objectStore, err error) (*SQLiteImportReport, error) {
	if len(r.rep.Orphans) == 0 {
		return nil, err
	}
	dctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	var left []string
	for _, k := range r.rep.Orphans {
		if derr := store.Delete(dctx, k); derr != nil {
			left = append(left, k)
		}
	}
	r.rep.Orphans = left
	sort.Strings(left)
	if len(left) == 0 {
		r.printf("The transaction was rolled back and the uploaded objects were deleted.")
		return nil, err
	}
	r.printf("The transaction was rolled back. These %d uploaded objects could not be deleted and are orphans: %s", len(left), strings.Join(left, ", "))
	r.printf("Delete them from the bucket by hand, or with \"aiwiki check-storage -delete-orphans -yes -allow-empty-database\" once they are older than 24 hours.")
	return nil, err
}

func totalBytes(objs []importObject) int64 {
	var n int64
	for _, o := range objs {
		n += o.Size
	}
	return n
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d bytes", n)
}

// lockAndCheckEmpty serialises against writers and refuses a target that holds
// content. EXCLUSIVE keeps writers out and lets readers continue.
func lockAndCheckEmpty(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, `LOCK TABLE spaces,articles,revisions,comments,attachments,favorites,article_fts,memberships,users,artifacts,artifact_revisions,artifact_shares,app_settings IN EXCLUSIVE MODE`); err != nil {
		return fmt.Errorf("cannot lock the target tables (is a server writing to it?): %w", err)
	}
	// A server started earlier in personal mode leaves exactly the owner's users
	// row (and audit entries): that is accepted. Every content table must be empty.
	for _, t := range []string{"spaces", "articles", "revisions", "comments", "attachments", "favorites", "article_fts", "artifacts", "artifact_revisions", "artifact_shares", "app_settings"} {
		var n int64
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM `+t).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			return refuse("the target already contains %d %s rows; import only into an empty personal-mode database", n, t)
		}
	}
	var n int64
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM memberships WHERE NOT (subject_type='user' AND subject_id=$1)`, personalOwnerID).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return refuse("the target already contains %d memberships of other subjects; import only into an empty personal-mode database", n)
	}
	var issuer, subject string
	var admin, disabled int
	err := tx.QueryRowContext(ctx, `SELECT issuer,subject,admin,disabled FROM users WHERE id=$1`, personalOwnerID).Scan(&issuer, &subject, &admin, &disabled)
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return err
	case issuer != personalOwnerIssuer || subject != personalOwnerSubject || admin != 1 || disabled != 0:
		return refuse("the target has a user %q that is not the personal-mode owner; this is not a personal-mode database", personalOwnerID)
	}
	return nil
}

// writeRows inserts everything through the import role. No trigger or policy is
// disabled: rows go in an order and shape the guards accept (see the comments).
func (r *importRun) writeRows(ctx context.Context, tx *sql.Tx, src *importSource) error {
	d := src.Data
	exec := func(what, q string, args ...any) error {
		if _, err := tx.ExecContext(ctx, q, args...); err != nil {
			return fmt.Errorf("writing %s: %w", what, err)
		}
		return nil
	}
	// Owner identity, exactly as the server creates it at start.
	if err := exec("owner", `INSERT INTO users(id,issuer,subject,name,admin,disabled) VALUES($1,$2,$3,$4,1,0) ON CONFLICT (id) DO UPDATE SET name=EXCLUDED.name`,
		personalOwnerID, personalOwnerIssuer, personalOwnerSubject, r.cfg.OwnerName); err != nil {
		return err
	}
	// Spaces: corporate_spaces_state_sync (AFTER INSERT/UPDATE OF archived_at)
	// fires legitimately and records the archive state; the storage-quota guard
	// passes because the quota stays NULL (server default).
	for _, s := range d.Spaces {
		if err := exec("space "+s.ID, `INSERT INTO spaces(id,name,description,archived_at,owner_id,personal) VALUES($1,$2,$3,$4,$5,0)`,
			s.ID, s.Name, s.Description, s.ArchivedAt, personalOwnerID); err != nil {
			return err
		}
		if err := exec("membership of "+s.ID, `INSERT INTO memberships(resource_type,resource_id,subject_type,subject_id,role) VALUES('space',$1,'user',$2,'owner')
ON CONFLICT (resource_type,resource_id,subject_type,subject_id) DO UPDATE SET role='owner'`, s.ID, personalOwnerID); err != nil {
			return err
		}
	}
	// Parents first: corporate_articles_parent_insert (BEFORE INSERT) requires an
	// existing parent in the same space and project.
	for _, a := range src.Ordered {
		if err := exec("article "+a.ID, `INSERT INTO articles(id,space_id,parent_id,project_id,title,summary,body,kind,status,pinned,tags,properties,revision,created_at,updated_at,author,deleted_at)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17)`,
			a.ID, a.SpaceID, a.ParentID, a.ProjectID, a.Title, a.Summary, a.Body, a.Kind, a.Status, a.Pinned, a.Tags, a.Properties, a.Revision, a.CreatedAt, a.UpdatedAt, a.Author, a.DeletedAt); err != nil {
			return err
		}
	}
	// Revisions keep their ids; the sequence is advanced before the commit.
	for _, v := range d.Revisions {
		if err := exec(fmt.Sprintf("revision %d", v.ID), `INSERT INTO revisions(id,article_id,revision,author,message,created_at,snapshot) VALUES($1,$2,$3,$4,$5,$6,$7)`,
			v.ID, v.ArticleID, v.Revision, v.Author, v.Message, v.CreatedAt, v.Snapshot); err != nil {
			return err
		}
	}
	for _, c := range d.Comments {
		if err := exec("comment "+c.ID, `INSERT INTO comments(id,article_id,anchor,body,author,created_at,resolved) VALUES($1,$2,$3,$4,$5,$6,$7)`,
			c.ID, c.ArticleID, c.Anchor, c.Body, c.Author, c.CreatedAt, c.Resolved); err != nil {
			return err
		}
	}
	for _, a := range d.Attachments {
		if err := exec("attachment "+a.ID, `INSERT INTO attachments(id,article_id,name,mime,size,stored_name,created_at,deleted_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`,
			a.ID, a.ArticleID, a.Name, a.MIME, a.Size, a.StoredName, a.CreatedAt, a.DeletedAt); err != nil {
			return err
		}
	}
	for _, f := range d.Favorites {
		if err := exec("favorite "+f.ArticleID, `INSERT INTO favorites(user_id,article_id,created_at) VALUES($1,$2,$3)`, personalOwnerID, f.ArticleID, f.CreatedAt); err != nil {
			return err
		}
	}
	// Full-text rows through the server's own indexing function. Deleted articles
	// have no row, as in the server (their row is removed on deletion).
	ftx := &transaction{Tx: tx, pg: true, ctx: ctx}
	for _, a := range src.Ordered {
		if a.DeletedAt.Valid {
			continue
		}
		if err := indexArticle(ftx, a.article()); err != nil {
			return fmt.Errorf("indexing article %s: %w", a.ID, err)
		}
	}
	// Artifacts (trusted mode has no owner: the owner becomes the personal owner,
	// without which "private" artifacts would be invisible). artifacts_guard is
	// BEFORE UPDATE only and artifact_rows_container_check is satisfied
	// (project_id is always '').
	for _, a := range d.Artifacts {
		if err := exec("artifact "+a.ID, `INSERT INTO artifacts(id,space_id,project_id,title,description,visibility,owner_id,revision,author,created_at,updated_at,deleted_at,file_count,projection,search_text)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)`,
			a.ID, a.SpaceID, a.ProjectID, a.Title, a.Description, a.Visibility, personalOwnerID, a.Revision, a.Author, a.CreatedAt, a.UpdatedAt, a.DeletedAt, a.FileCount, a.Projection, a.SearchText); err != nil {
			return err
		}
	}
	for _, v := range d.ArtRevs {
		if err := exec("artifact revision", `INSERT INTO artifact_revisions(artifact_id,revision,author,message,created_at,manifest,file_count) VALUES($1,$2,$3,$4,$5,$6,$7)`,
			v.ArtifactID, v.Revision, v.Author, v.Message, v.CreatedAt, v.Manifest, v.FileCount); err != nil {
			return err
		}
	}
	for _, b := range d.ArtBlobs {
		if err := exec("artifact blob", `INSERT INTO artifact_blobs(sha256,size,mime,stored_name,created_at) VALUES($1,$2,$3,$4,$5) ON CONFLICT (sha256) DO NOTHING`,
			b.SHA256, b.Size, b.MIME, b.StoredName, b.CreatedAt); err != nil {
			return err
		}
	}
	for _, s := range d.ArtShares {
		// A link stays valid only while its creator is an existing user with the
		// right to share (artifact_link_valid); trusted links have no creator.
		if err := exec("artifact link "+s.ID, `INSERT INTO artifact_shares(id,artifact_id,token_hash,revision,created_by,created_at,revoked_at) VALUES($1,$2,$3,$4,$5,$6,$7)`,
			s.ID, s.ArtifactID, s.TokenHash, s.Revision, personalOwnerID, s.CreatedAt, s.RevokedAt); err != nil {
			return err
		}
	}
	for _, s := range d.Settings {
		if err := exec("setting "+s.Key, `INSERT INTO app_settings(key,value) VALUES($1,$2) ON CONFLICT (key) DO UPDATE SET value=EXCLUDED.value`, s.Key, s.Value); err != nil {
			return err
		}
	}
	return nil
}

// article rebuilds the Article the server's indexing function takes.
func (a impArticle) article() Article {
	art, _ := scanArticle(articleRow{a})
	return art
}

// articleRow lets scanArticle (the server's decoder of tags/properties) read an
// impArticle.
type articleRow struct{ a impArticle }

func (r articleRow) Scan(dest ...any) error {
	a := r.a
	vals := []any{a.ID, a.SpaceID, a.ParentID, a.ProjectID, a.Title, a.Summary, a.Body, a.Kind, a.Status, a.Pinned, a.Tags, a.Properties, a.Revision, a.CreatedAt, a.UpdatedAt, a.Author, a.DeletedAt}
	for i, v := range vals {
		switch p := dest[i].(type) {
		case *string:
			*p = v.(string)
		case *int:
			*p = v.(int)
		case *sql.NullString:
			*p = v.(sql.NullString)
		default:
			return fmt.Errorf("unexpected scan target %T", dest[i])
		}
	}
	return nil
}
