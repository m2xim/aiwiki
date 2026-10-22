package server

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

// StorageCheckConfig drives CheckStorage (the "aiwiki check-storage" command).
type StorageCheckConfig struct {
	// DatabaseURL must belong to a role that can read every row despite FORCE
	// ROW LEVEL SECURITY (superuser or BYPASSRLS). The runtime role and the
	// migration owner both see no content rows.
	DatabaseURL   string
	S3            S3Config
	VerifyHashes  bool // re-read artifact objects and compare sha256 with the database
	DeleteOrphans bool // with Yes: remove orphans older than OrphanMinAge
	Yes           bool // confirmation for DeleteOrphans
	// AllowEmptyDatabase lets DeleteOrphans proceed when the database references no objects.
	AllowEmptyDatabase bool
	OrphanMinAge       time.Duration // default 24h
	Out                io.Writer
}

// storageRef is one object that the database says must exist.
type storageRef struct {
	Key    string
	Size   int64 // -1 when the database does not record a size
	SHA256 string
	Kinds  []string // "attachment", "attachment (deleted)", "artifact blob", "artifact manifest"
}

// StorageReport is the outcome of one check.
type StorageReport struct {
	References     int
	Objects        int
	Missing        []storageRef
	SizeMismatch   []string
	HashMismatch   []string
	Orphans        []objectInfo
	YoungOrphans   int
	OrphansDeleted int
	OrphansSkipped int      // candidates that changed or reappeared before deletion
	Foreign        []string // keys under the prefix that the server could never have written
	Unverified     int      // objects that could not be read for the hash check
}

// Failed reports data loss: referenced objects that are absent or damaged.
func (r *StorageReport) Failed() bool {
	return len(r.Missing) > 0 || len(r.SizeMismatch) > 0 || len(r.HashMismatch) > 0 || r.Unverified > 0
}

const storageReportListCap = 200

// CheckStorage compares the object references in PostgreSQL with the bucket.
// It never prints object contents, only keys and sizes.
func CheckStorage(ctx context.Context, cfg StorageCheckConfig) (*StorageReport, error) {
	if cfg.Out == nil {
		cfg.Out = io.Discard
	}
	if cfg.DatabaseURL == "" {
		return nil, errors.New("database URL required (AIWIKI_BACKUP_DATABASE_URL)")
	}
	if cfg.DeleteOrphans && !cfg.Yes {
		fmt.Fprintln(cfg.Out, "note: -delete-orphans without -yes only reports; nothing is deleted")
	}
	pc, err := pgx.ParseConfig(cfg.DatabaseURL)
	if err != nil {
		return nil, errors.New("invalid PostgreSQL connection configuration")
	}
	pc.RuntimeParams["application_name"] = "aiwiki-check-storage"
	pc.RuntimeParams["default_transaction_read_only"] = "on"
	db := stdlib.OpenDB(*pc)
	defer db.Close()
	db.SetMaxOpenConns(2)
	if err = db.PingContext(ctx); err != nil {
		return nil, errors.New("PostgreSQL is not reachable; check the connection URL")
	}
	var bypass bool
	if err = db.QueryRowContext(ctx, `SELECT coalesce(bool_or(rolsuper OR rolbypassrls),false) FROM pg_roles WHERE rolname=current_user`).Scan(&bypass); err != nil {
		return nil, fmt.Errorf("cannot verify database role: %w", err)
	}
	if !bypass {
		return nil, errors.New("the database role cannot read all rows: content tables use FORCE ROW LEVEL SECURITY, which hides rows even from the table owner; use a dedicated backup role with BYPASSRLS (see docs/BACKUP.md)")
	}
	store, err := newS3Store(ctx, cfg.S3)
	if err != nil {
		return nil, err
	}
	rep, err := checkStorage(ctx, func(ctx context.Context) ([]storageRef, error) { return loadStorageRefs(ctx, db) }, store, cfg)
	if rep != nil {
		rep.write(cfg.Out)
	}
	return rep, err
}

// loadStorageRefs reads every object reference from the database.
func loadStorageRefs(ctx context.Context, db *sql.DB) ([]storageRef, error) {
	refs := map[string]*storageRef{}
	add := func(key string, size int64, sha, kind string) {
		r := refs[key]
		if r == nil {
			r = &storageRef{Key: key, Size: size, SHA256: sha}
			refs[key] = r
		}
		if r.Size < 0 {
			r.Size = size
		}
		if r.SHA256 == "" {
			r.SHA256 = sha
		}
		for _, k := range r.Kinds {
			if k == kind {
				return
			}
		}
		r.Kinds = append(r.Kinds, kind)
	}
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelRepeatableRead})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT stored_name,size,deleted_at IS NOT NULL FROM public.attachments`)
	if err != nil {
		return nil, fmt.Errorf("reading attachments: %w", err)
	}
	for rows.Next() {
		var key string
		var size int64
		var deleted bool
		if err = rows.Scan(&key, &size, &deleted); err != nil {
			rows.Close()
			return nil, err
		}
		kind := "attachment"
		if deleted {
			kind = "attachment (deleted)"
		}
		add(key, size, "", kind)
	}
	if err = rows.Close(); err == nil {
		err = rows.Err()
	}
	if err != nil {
		return nil, err
	}
	if rows, err = tx.QueryContext(ctx, `SELECT stored_name,size,sha256 FROM public.artifact_blobs`); err != nil {
		return nil, fmt.Errorf("reading artifact_blobs: %w", err)
	}
	for rows.Next() {
		var key, sha string
		var size int64
		if err = rows.Scan(&key, &size, &sha); err != nil {
			rows.Close()
			return nil, err
		}
		add(key, size, sha, "artifact blob")
	}
	if err = rows.Close(); err == nil {
		err = rows.Err()
	}
	if err != nil {
		return nil, err
	}
	// Revision manifests name blobs too; an entry without a blob row would be a
	// database inconsistency that still ends in a missing object.
	if rows, err = tx.QueryContext(ctx, `SELECT manifest FROM public.artifact_revisions`); err != nil {
		return nil, fmt.Errorf("reading artifact_revisions: %w", err)
	}
	for rows.Next() {
		var raw string
		if err = rows.Scan(&raw); err != nil {
			rows.Close()
			return nil, err
		}
		var files []artifactFile
		if json.Unmarshal([]byte(raw), &files) != nil {
			rows.Close()
			return nil, errors.New("an artifact revision manifest is not valid JSON")
		}
		for _, f := range files {
			add(artifactBlobKey(f.SHA256), f.Size, f.SHA256, "artifact manifest")
		}
	}
	if err = rows.Close(); err == nil {
		err = rows.Err()
	}
	if err != nil {
		return nil, err
	}
	out := make([]storageRef, 0, len(refs))
	for _, r := range refs {
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

// orphanDeleteBatch is how many candidates are deleted between two re-reads
// of the references.
const orphanDeleteBatch = 100

// errDeleteRefused is returned (together with the report) when -delete-orphans
// -yes would be unsafe; nothing has been deleted.
type errDeleteRefused string

func (e errDeleteRefused) Error() string { return "refusing to delete orphans: " + string(e) }

// checkStorage is the comparison itself, independent of PostgreSQL.
// References are read before the listing: an upload writes the object before
// its row, so every object referenced at read time exists when we list.
// On errDeleteRefused the report is returned as well.
func checkStorage(ctx context.Context, load func(context.Context) ([]storageRef, error), store objectStore, cfg StorageCheckConfig) (*StorageReport, error) {
	if cfg.OrphanMinAge <= 0 {
		cfg.OrphanMinAge = 24 * time.Hour
	}
	refs, err := load(ctx)
	if err != nil {
		return nil, err
	}
	listed, err := store.List(ctx, "")
	if err != nil {
		return nil, err
	}
	rep := &StorageReport{References: len(refs)}
	objs := listed[:0:0]
	for _, o := range listed {
		// The server only writes single-segment keys; anything else under the
		// prefix belongs to someone else and cannot be addressed by the store.
		if !validObjectKey(o.Key) {
			rep.Foreign = append(rep.Foreign, o.Key)
			continue
		}
		objs = append(objs, o)
	}
	rep.Objects = len(objs)
	byKey := make(map[string]objectInfo, len(objs))
	for _, o := range objs {
		byKey[o.Key] = o
	}
	known := make(map[string]bool, len(refs))
	for _, r := range refs {
		known[r.Key] = true
		o, ok := byKey[r.Key]
		if !ok {
			rep.Missing = append(rep.Missing, r)
			continue
		}
		if r.Size >= 0 && o.Size != r.Size {
			rep.SizeMismatch = append(rep.SizeMismatch, fmt.Sprintf("%s (database %d bytes, bucket %d bytes)", r.Key, r.Size, o.Size))
			continue
		}
		if cfg.VerifyHashes && r.SHA256 != "" {
			match, err := verifyObjectHash(ctx, store, r)
			switch {
			case err != nil:
				rep.Unverified++
			case !match:
				rep.HashMismatch = append(rep.HashMismatch, r.Key)
			}
		}
	}
	var candidates []objectInfo
	now := time.Now()
	for _, o := range objs {
		if known[o.Key] {
			continue
		}
		rep.Orphans = append(rep.Orphans, o)
		if !o.Modified.IsZero() && now.Sub(o.Modified) >= cfg.OrphanMinAge {
			candidates = append(candidates, o)
		} else {
			rep.YoungOrphans++
		}
	}
	sort.Slice(rep.Orphans, func(i, j int) bool { return rep.Orphans[i].Key < rep.Orphans[j].Key })
	if !cfg.DeleteOrphans || !cfg.Yes {
		return rep, nil
	}
	// Guard against the wrong database: deleting is allowed only when the
	// database plausibly belongs to this bucket.
	switch {
	case rep.Failed():
		return rep, errDeleteRefused("objects referenced by the database are missing or damaged; fix that first")
	case rep.References == 0 && rep.Objects > 0 && !cfg.AllowEmptyDatabase:
		return rep, errDeleteRefused("the database references no objects at all (empty or wrong database?); use -allow-empty-database only if the bucket really is meant to be emptied")
	case rep.References > 0 && rep.Objects > 0 && len(rep.Orphans) == rep.Objects:
		return rep, errDeleteRefused("none of the objects in the bucket is referenced by the database (wrong database or bucket?)")
	}
	if len(candidates) == 0 {
		return rep, nil
	}
	for start := 0; start < len(candidates); start += orphanDeleteBatch {
		end := min(start+orphanDeleteBatch, len(candidates))
		// Re-read the references right before each batch: a row may have appeared.
		fresh, err := load(ctx)
		if err != nil {
			return rep, err
		}
		inUse := make(map[string]bool, len(fresh))
		for _, r := range fresh {
			inUse[r.Key] = true
		}
		for _, o := range candidates[start:end] {
			if inUse[o.Key] {
				continue
			}
			// An upload of the same (content-addressed) key replaces the object
			// and moves its modification time: HEAD again right before DELETE.
			cur, err := store.Stat(ctx, o.Key)
			if errors.Is(err, errObjectNotFound) {
				continue
			}
			if err != nil {
				return rep, fmt.Errorf("checking orphan %s: %w", o.Key, err)
			}
			if !cur.Modified.Equal(o.Modified) || time.Since(cur.Modified) < cfg.OrphanMinAge {
				rep.OrphansSkipped++
				continue
			}
			if err := store.Delete(ctx, o.Key); err != nil {
				return rep, fmt.Errorf("deleting orphan %s: %w", o.Key, err)
			}
			rep.OrphansDeleted++
		}
	}
	return rep, nil
}

func verifyObjectHash(ctx context.Context, store objectStore, r storageRef) (bool, error) {
	rc, err := store.Get(ctx, r.Key)
	if err != nil {
		return false, err
	}
	defer rc.Close()
	h := sha256.New()
	if _, err = io.Copy(h, rc); err != nil {
		return false, err
	}
	return strings.EqualFold(hex.EncodeToString(h.Sum(nil)), r.SHA256), nil
}

func (r *StorageReport) write(w io.Writer) {
	fmt.Fprintf(w, "references in database: %d\nobjects in bucket:      %d\n", r.References, r.Objects)
	fmt.Fprintf(w, "MISSING (in database, not in bucket): %d\n", len(r.Missing))
	for i, m := range r.Missing {
		if i == storageReportListCap {
			fmt.Fprintf(w, "  ... and %d more\n", len(r.Missing)-i)
			break
		}
		fmt.Fprintf(w, "  %s  size=%d  (%s)\n", m.Key, m.Size, strings.Join(m.Kinds, ", "))
	}
	list := func(title string, items []string) {
		fmt.Fprintf(w, "%s: %d\n", title, len(items))
		for i, s := range items {
			if i == storageReportListCap {
				fmt.Fprintf(w, "  ... and %d more\n", len(items)-i)
				break
			}
			fmt.Fprintf(w, "  %s\n", s)
		}
	}
	list("SIZE MISMATCH", r.SizeMismatch)
	list("HASH MISMATCH", r.HashMismatch)
	if r.Unverified > 0 {
		fmt.Fprintf(w, "UNVERIFIED (read error during hash check): %d\n", r.Unverified)
	}
	fmt.Fprintf(w, "orphans (in bucket, not in database; informational): %d (%d younger than the grace period)\n", len(r.Orphans), r.YoungOrphans)
	for i, o := range r.Orphans {
		if i == storageReportListCap {
			fmt.Fprintf(w, "  ... and %d more\n", len(r.Orphans)-i)
			break
		}
		fmt.Fprintf(w, "  %s  size=%d\n", o.Key, o.Size)
	}
	if len(r.Foreign) > 0 {
		fmt.Fprintf(w, "skipped %d object(s) under the prefix whose key the server cannot have written (not checked, never deleted):\n", len(r.Foreign))
		for i, k := range r.Foreign {
			if i == storageReportListCap {
				fmt.Fprintf(w, "  ... and %d more\n", len(r.Foreign)-i)
				break
			}
			fmt.Fprintf(w, "  %s\n", k)
		}
	}
	if r.OrphansDeleted > 0 || r.OrphansSkipped > 0 {
		fmt.Fprintf(w, "orphans deleted: %d (skipped because they changed or reappeared: %d)\n", r.OrphansDeleted, r.OrphansSkipped)
	}
	if r.Failed() {
		fmt.Fprintln(w, "RESULT: FAILED (objects referenced by the database are missing or damaged)")
	} else {
		fmt.Fprintln(w, "RESULT: OK")
	}
}
