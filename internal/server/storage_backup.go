package server

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// incomingDir holds partially written backup files. localStore.List ignores it
// (only regular files count), so a half-written object never shows up as one.
const incomingDir = ".incoming"

// copyReport counts one object copy run. Only keys and numbers are ever printed.
type copyReport struct {
	Listed     int
	Copied     int
	Bytes      int64
	Present    int // already in the destination with the same size
	Conflicts  int // present with a different size; never overwritten
	HashErrors int // art- objects whose bytes do not match their name; not stored
	Skipped    int // keys the store cannot address; never touched
	Errors     int // read, write or size failures of single objects
}

// Failed reports anything that leaves the destination incomplete or suspicious.
func (r *copyReport) Failed() bool { return r.Conflicts+r.HashErrors+r.Errors > 0 }

func (r *copyReport) write(w io.Writer, from, to string) {
	fmt.Fprintf(w, "objects in %s: %d\ncopied to %s: %d (%d bytes)\nalready present: %d\nskipped (unusable key): %d\nCONFLICTS (size differs, not overwritten): %d\nHASH ERRORS (art- bytes do not match the name, not stored): %d\nERRORS: %d\n",
		from, r.Listed, to, r.Copied, r.Bytes, r.Present, r.Skipped, r.Conflicts, r.HashErrors, r.Errors)
}

// artifactHashOfKey returns the sha256 an "art-<64 hex>" key promises.
func artifactHashOfKey(key string) (string, bool) {
	h, ok := strings.CutPrefix(key, "art-")
	if !ok || len(h) != 64 {
		return "", false
	}
	for _, c := range h {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return "", false
		}
	}
	return h, true
}

var errHashMismatch = errors.New("object hash mismatch")

// copyObjects copies every object of src that dst lacks. Objects are immutable,
// so an existing object is never replaced: a different size is reported as a
// conflict. Each object is staged in a temp file first, which gives the size and
// hash checks before anything becomes visible under its final key.
func copyObjects(ctx context.Context, src, dst objectStore, out io.Writer) (copyReport, error) {
	var rep copyReport
	listed, err := src.List(ctx, "")
	if err != nil {
		return rep, err
	}
	var stageDir string // empty = system temp dir (buffer for Put)
	if local, ok := dst.(localStore); ok {
		stageDir = filepath.Join(local.dir, incomingDir)
		if err = os.MkdirAll(stageDir, 0700); err != nil {
			return rep, err
		}
		// Leftovers of an interrupted run; the backup has a single writer.
		if old, err := os.ReadDir(stageDir); err == nil {
			for _, e := range old {
				_ = os.Remove(filepath.Join(stageDir, e.Name()))
			}
		}
	}
	for _, o := range listed {
		if err = ctx.Err(); err != nil {
			return rep, err
		}
		rep.Listed++
		if !validObjectKey(o.Key) || o.Key == incomingDir {
			rep.Skipped++
			fmt.Fprintf(out, "skipped unusable key %q\n", o.Key)
			continue
		}
		cur, err := dst.Stat(ctx, o.Key)
		switch {
		case err == nil && cur.Size == o.Size:
			rep.Present++
			continue
		case err == nil:
			rep.Conflicts++
			fmt.Fprintf(out, "CONFLICT %s: source %d bytes, destination %d bytes (kept)\n", o.Key, o.Size, cur.Size)
			continue
		case !errors.Is(err, errObjectNotFound):
			rep.Errors++
			fmt.Fprintf(out, "ERROR %s: %v\n", o.Key, err)
			continue
		}
		n, err := copyOne(ctx, src, dst, stageDir, o)
		switch {
		case err == nil:
			rep.Copied++
			rep.Bytes += n
		case errors.Is(err, errHashMismatch):
			rep.HashErrors++
			fmt.Fprintf(out, "HASH ERROR %s: bytes do not match the key (not stored)\n", o.Key)
		case ctx.Err() != nil:
			return rep, ctx.Err()
		default:
			rep.Errors++
			fmt.Fprintf(out, "ERROR %s: %v\n", o.Key, err)
		}
	}
	return rep, nil
}

func copyOne(ctx context.Context, src, dst objectStore, stageDir string, o objectInfo) (int64, error) {
	rc, err := src.Get(ctx, o.Key)
	if err != nil {
		return 0, err
	}
	defer rc.Close()
	// For S3 the temp file is only a buffer for Put (which needs a ReadSeeker),
	// kept on disk because objects can be large.
	tmp, err := os.CreateTemp(stageDir, ".obj-*")
	if err != nil {
		return 0, err
	}
	name := tmp.Name()
	defer os.Remove(name) // no-op after a successful rename
	defer tmp.Close()
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), rc)
	if err != nil {
		return 0, err
	}
	if n != o.Size {
		return 0, fmt.Errorf("size mismatch while reading (listed %d, read %d)", o.Size, n)
	}
	if want, ok := artifactHashOfKey(o.Key); ok && hex.EncodeToString(h.Sum(nil)) != want {
		return 0, errHashMismatch
	}
	if local, ok := dst.(localStore); ok {
		final, err := local.path(o.Key)
		if err != nil {
			return 0, err
		}
		if err = tmp.Sync(); err != nil {
			return 0, err
		}
		if err = tmp.Close(); err != nil {
			return 0, err
		}
		if err = os.Rename(name, final); err != nil {
			return 0, err
		}
		syncDir(local.dir)
		return n, nil
	}
	if _, err = tmp.Seek(0, io.SeekStart); err != nil {
		return 0, err
	}
	if err = dst.Put(ctx, o.Key, tmp, n, ""); err != nil {
		return 0, err
	}
	return n, nil
}

// syncDir makes a rename durable where the platform allows it (best effort).
func syncDir(dir string) {
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		d.Close()
	}
}

// BackupObjectsConfig drives BackupObjects and RestoreObjects.
type BackupObjectsConfig struct {
	DatabaseURL  string // same role requirements as check-storage
	S3           S3Config
	Dir          string
	VerifyHashes bool
	Out          io.Writer
}

// BackupObjectsReport is the outcome of a backup or restore run.
type BackupObjectsReport struct {
	Copy  copyReport
	Check *StorageReport
}

// Failed reports problems in the copy or in the final comparison.
func (r *BackupObjectsReport) Failed() bool {
	return r.Copy.Failed() || (r.Check != nil && r.Check.Failed())
}

func loadFrom(db *sql.DB) func(context.Context) ([]storageRef, error) {
	return func(ctx context.Context) ([]storageRef, error) { return loadStorageRefs(ctx, db) }
}

// BackupObjects copies the bucket into Dir, then checks the copy against the
// database. Run it after the database dump (docs/BACKUP.md). It never deletes.
func BackupObjects(ctx context.Context, cfg BackupObjectsConfig) (*BackupObjectsReport, error) {
	if cfg.Out == nil {
		cfg.Out = io.Discard
	}
	if strings.TrimSpace(cfg.Dir) == "" {
		return nil, errors.New("backup directory required")
	}
	db, err := openBypassDB(ctx, cfg.DatabaseURL, "aiwiki-backup-objects")
	if err != nil {
		return nil, err
	}
	defer db.Close()
	store, err := newS3Store(ctx, cfg.S3)
	if err != nil {
		return nil, err
	}
	if err = os.MkdirAll(cfg.Dir, 0700); err != nil {
		return nil, fmt.Errorf("cannot create backup directory: %w", err)
	}
	return backupAndCheck(ctx, loadFrom(db), store, localStore{dir: cfg.Dir}, cfg)
}

// backupAndCheck snapshots the database references BEFORE copying: a row is
// written after its object, so everything referenced at snapshot time is in
// the bucket when the copy lists it. References that appear during the copy
// are not in the snapshot and so are never reported as missing.
func backupAndCheck(ctx context.Context, load func(context.Context) ([]storageRef, error), src objectStore, local localStore, cfg BackupObjectsConfig) (*BackupObjectsReport, error) {
	refs, err := load(ctx)
	if err != nil {
		return nil, err
	}
	rep := &BackupObjectsReport{}
	if rep.Copy, err = copyObjects(ctx, src, local, cfg.Out); err != nil {
		return nil, err
	}
	rep.Copy.write(cfg.Out, "bucket", "backup")
	fmt.Fprintln(cfg.Out, "checking the backup directory against the database")
	snapshot := func(context.Context) ([]storageRef, error) { return refs, nil }
	rep.Check, err = checkStorage(ctx, snapshot, local, StorageCheckConfig{VerifyHashes: cfg.VerifyHashes})
	if err != nil {
		return nil, err
	}
	rep.Check.write(cfg.Out)
	return rep, nil
}

// RestoreObjects copies Dir back into the bucket, then runs the normal check
// against the bucket. Objects already in the bucket are kept.
func RestoreObjects(ctx context.Context, cfg BackupObjectsConfig) (*BackupObjectsReport, error) {
	if cfg.Out == nil {
		cfg.Out = io.Discard
	}
	if strings.TrimSpace(cfg.Dir) == "" {
		return nil, errors.New("backup directory required")
	}
	if fi, err := os.Stat(cfg.Dir); err != nil || !fi.IsDir() {
		return nil, errors.New("backup directory does not exist")
	}
	local := localStore{dir: cfg.Dir}
	have, err := local.List(ctx, "")
	if err != nil {
		return nil, err
	}
	if len(have) == 0 {
		return nil, errors.New("backup directory holds no objects; refusing to restore from an empty directory")
	}
	db, err := openBypassDB(ctx, cfg.DatabaseURL, "aiwiki-restore-objects")
	if err != nil {
		return nil, err
	}
	defer db.Close()
	store, err := newS3Store(ctx, cfg.S3)
	if err != nil {
		return nil, err
	}
	rep := &BackupObjectsReport{}
	if rep.Copy, err = copyObjects(ctx, local, store, cfg.Out); err != nil {
		return nil, err
	}
	rep.Copy.write(cfg.Out, "backup", "bucket")
	fmt.Fprintln(cfg.Out, "checking the bucket against the database")
	rep.Check, err = checkStorage(ctx, loadFrom(db), store, StorageCheckConfig{VerifyHashes: cfg.VerifyHashes})
	if err != nil {
		return nil, err
	}
	rep.Check.write(cfg.Out)
	return rep, nil
}
