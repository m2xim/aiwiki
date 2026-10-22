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
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// ImportRefusal is a reason to stop before anything is written (exit code 2).
type ImportRefusal struct{ Msg string }

func (e *ImportRefusal) Error() string { return e.Msg }

func refuse(format string, args ...any) error {
	return &ImportRefusal{Msg: fmt.Sprintf(format, args...)}
}

// Row types shared by the SQLite reader and the PostgreSQL verifier: both
// databases are read with the same column lists, so a row compares with
// reflect.DeepEqual after the intended normalisation.
type impSpace struct {
	ID, Name, Description string
	ArchivedAt            sql.NullString
}
type impArticle struct {
	ID, SpaceID, ParentID, ProjectID, Title, Summary, Body, Kind, Status string
	Pinned                                                               int
	Tags, Properties                                                     string
	Revision                                                             int
	CreatedAt, UpdatedAt, Author                                         string
	DeletedAt                                                            sql.NullString
}
type impRevision struct {
	ID                                   int64
	ArticleID                            string
	Revision                             int
	Author, Message, CreatedAt, Snapshot string
}
type impComment struct {
	ID, ArticleID, Anchor, Body, Author, CreatedAt string
	Resolved                                       int
}
type impAttachment struct {
	ID, ArticleID, Name, MIME string
	Size                      int64
	StoredName, CreatedAt     string
	DeletedAt                 sql.NullString
}
type impFavorite struct{ ArticleID, CreatedAt string }
type impArtifact struct {
	ID, SpaceID, ProjectID, Title, Description, Visibility, OwnerID string
	Revision                                                        int
	Author, CreatedAt, UpdatedAt                                    string
	DeletedAt                                                       sql.NullString
	FileCount                                                       int
	Projection, SearchText                                          string
}
type impArtifactRevision struct {
	ArtifactID                 string
	Revision                   int
	Author, Message, CreatedAt string
	Manifest                   string
	FileCount                  int
}
type impArtifactBlob struct {
	SHA256                      string
	Size                        int64
	MIME, StoredName, CreatedAt string
}
type impArtifactShare struct {
	ID, ArtifactID, TokenHash string
	Revision                  sql.NullInt64
	CreatedBy, CreatedAt      string
	RevokedAt                 sql.NullString
}
type impSetting struct{ Key, Value string }

// impData is the content of one wiki database, rows sorted by their key.
type impData struct {
	Spaces      []impSpace
	Articles    []impArticle
	Revisions   []impRevision
	Comments    []impComment
	Attachments []impAttachment
	Favorites   []impFavorite
	Artifacts   []impArtifact
	ArtRevs     []impArtifactRevision
	ArtBlobs    []impArtifactBlob
	ArtShares   []impArtifactShare
	Settings    []impSetting
	FTSRows     int // article_fts rows (source: informational)
}

type rowQuerier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// scanAll runs one query and scans every row with scan.
func scanAll[T any](ctx context.Context, q rowQuerier, query string, scan func(*sql.Rows) (T, error)) ([]T, error) {
	rows, err := q.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []T
	for rows.Next() {
		v, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// loadImpData reads the content tables. favoritesQuery differs by dialect.
func loadImpData(ctx context.Context, q rowQuerier, withBlobs bool) (*impData, error) {
	var d impData
	var err error
	step := func(name string, e error) {
		if err == nil && e != nil {
			err = fmt.Errorf("reading %s: %w", name, e)
		}
	}
	var e error
	d.Spaces, e = scanAll(ctx, q, `SELECT id,name,description,archived_at FROM spaces ORDER BY id`, func(r *sql.Rows) (v impSpace, err error) {
		err = r.Scan(&v.ID, &v.Name, &v.Description, &v.ArchivedAt)
		return
	})
	step("spaces", e)
	d.Articles, e = scanAll(ctx, q, `SELECT id,space_id,parent_id,project_id,title,summary,body,kind,status,pinned,tags,properties,revision,created_at,updated_at,author,deleted_at FROM articles ORDER BY id`, func(r *sql.Rows) (v impArticle, err error) {
		err = r.Scan(&v.ID, &v.SpaceID, &v.ParentID, &v.ProjectID, &v.Title, &v.Summary, &v.Body, &v.Kind, &v.Status, &v.Pinned, &v.Tags, &v.Properties, &v.Revision, &v.CreatedAt, &v.UpdatedAt, &v.Author, &v.DeletedAt)
		return
	})
	step("articles", e)
	d.Revisions, e = scanAll(ctx, q, `SELECT id,article_id,revision,author,message,created_at,snapshot FROM revisions ORDER BY id`, func(r *sql.Rows) (v impRevision, err error) {
		err = r.Scan(&v.ID, &v.ArticleID, &v.Revision, &v.Author, &v.Message, &v.CreatedAt, &v.Snapshot)
		return
	})
	step("revisions", e)
	d.Comments, e = scanAll(ctx, q, `SELECT id,article_id,anchor,body,author,created_at,resolved FROM comments ORDER BY id`, func(r *sql.Rows) (v impComment, err error) {
		err = r.Scan(&v.ID, &v.ArticleID, &v.Anchor, &v.Body, &v.Author, &v.CreatedAt, &v.Resolved)
		return
	})
	step("comments", e)
	d.Attachments, e = scanAll(ctx, q, `SELECT id,article_id,name,mime,size,stored_name,created_at,deleted_at FROM attachments ORDER BY id`, func(r *sql.Rows) (v impAttachment, err error) {
		err = r.Scan(&v.ID, &v.ArticleID, &v.Name, &v.MIME, &v.Size, &v.StoredName, &v.CreatedAt, &v.DeletedAt)
		return
	})
	step("attachments", e)
	d.Favorites, e = scanAll(ctx, q, `SELECT article_id,created_at FROM favorites ORDER BY article_id`, func(r *sql.Rows) (v impFavorite, err error) {
		err = r.Scan(&v.ArticleID, &v.CreatedAt)
		return
	})
	step("favorites", e)
	d.Artifacts, e = scanAll(ctx, q, `SELECT id,space_id,project_id,title,description,visibility,owner_id,revision,author,created_at,updated_at,deleted_at,file_count,projection,search_text FROM artifacts ORDER BY id`, func(r *sql.Rows) (v impArtifact, err error) {
		err = r.Scan(&v.ID, &v.SpaceID, &v.ProjectID, &v.Title, &v.Description, &v.Visibility, &v.OwnerID, &v.Revision, &v.Author, &v.CreatedAt, &v.UpdatedAt, &v.DeletedAt, &v.FileCount, &v.Projection, &v.SearchText)
		return
	})
	step("artifacts", e)
	d.ArtRevs, e = scanAll(ctx, q, `SELECT artifact_id,revision,author,message,created_at,manifest,file_count FROM artifact_revisions ORDER BY artifact_id,revision`, func(r *sql.Rows) (v impArtifactRevision, err error) {
		err = r.Scan(&v.ArtifactID, &v.Revision, &v.Author, &v.Message, &v.CreatedAt, &v.Manifest, &v.FileCount)
		return
	})
	step("artifact_revisions", e)
	if withBlobs {
		d.ArtBlobs, e = scanAll(ctx, q, `SELECT sha256,size,mime,stored_name,created_at FROM artifact_blobs ORDER BY sha256`, func(r *sql.Rows) (v impArtifactBlob, err error) {
			err = r.Scan(&v.SHA256, &v.Size, &v.MIME, &v.StoredName, &v.CreatedAt)
			return
		})
		step("artifact_blobs", e)
	}
	d.ArtShares, e = scanAll(ctx, q, `SELECT id,artifact_id,token_hash,revision,created_by,created_at,revoked_at FROM artifact_shares ORDER BY id`, func(r *sql.Rows) (v impArtifactShare, err error) {
		err = r.Scan(&v.ID, &v.ArtifactID, &v.TokenHash, &v.Revision, &v.CreatedBy, &v.CreatedAt, &v.RevokedAt)
		return
	})
	step("artifact_shares", e)
	d.Settings, e = scanAll(ctx, q, `SELECT key,value FROM app_settings ORDER BY key`, func(r *sql.Rows) (v impSetting, err error) {
		err = r.Scan(&v.Key, &v.Value)
		return
	})
	step("app_settings", e)
	if err == nil {
		err = func() error {
			rows, e := q.QueryContext(ctx, `SELECT count(*) FROM article_fts`)
			if e != nil {
				return fmt.Errorf("reading article_fts: %w", e)
			}
			defer rows.Close()
			if rows.Next() {
				if e = rows.Scan(&d.FTSRows); e != nil {
					return e
				}
			}
			return rows.Err()
		}()
	}
	if err != nil {
		return nil, err
	}
	return &d, nil
}

// importObject is one file that must be present in the bucket.
type importObject struct {
	Key    string
	Path   string
	Size   int64
	SHA256 string
	MIME   string
}

// importSource is the analysed trusted-mode installation.
type importSource struct {
	Data    *impData
	Objects []importObject // sorted by key
	// Articles in an order that satisfies the parent-in-same-container trigger.
	Ordered []impArticle
}

var sha256Pattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// copySourceDatabase copies wiki.db (and a pending -wal) into tmp. The source is
// read with os.Open only (never opened for writing); a change during the copy is refused.
func copySourceDatabase(from, tmp string) (string, error) {
	db := filepath.Join(from, "wiki.db")
	type stamp struct {
		size int64
		mod  int64
	}
	stat := func(p string) (stamp, bool, error) {
		fi, err := os.Stat(p)
		if errors.Is(err, os.ErrNotExist) {
			return stamp{}, false, nil
		}
		if err != nil {
			return stamp{}, false, err
		}
		if !fi.Mode().IsRegular() {
			return stamp{}, false, fmt.Errorf("%s is not a regular file", filepath.Base(p))
		}
		return stamp{fi.Size(), fi.ModTime().UnixNano()}, true, nil
	}
	b0, ok, err := stat(db)
	if err != nil {
		return "", refuse("cannot read the source database: %v", err)
	}
	if !ok {
		return "", refuse("no wiki.db in %s (-from must be a trusted-mode data directory)", from)
	}
	if j, hasJournal, jerr := stat(db + "-journal"); jerr != nil {
		return "", refuse("cannot read the source database: %v", jerr)
	} else if hasJournal && j.size > 0 {
		return "", refuse("wiki.db-journal exists and is not empty: the old server stopped in the middle of a write. Start the old server once so SQLite recovers the database, stop it, and run the import again")
	}
	w0, hasWAL, err := stat(db + "-wal")
	if err != nil {
		return "", refuse("cannot read the source database: %v", err)
	}
	dst := filepath.Join(tmp, "wiki.db")
	copyFile := func(src, to string) error {
		in, err := os.Open(src)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.OpenFile(to, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return err
		}
		_, err = io.Copy(out, in)
		if cerr := out.Close(); err == nil {
			err = cerr
		}
		return err
	}
	if err = copyFile(db, dst); err != nil {
		return "", refuse("cannot copy the source database: %v", err)
	}
	if hasWAL {
		if err = copyFile(db+"-wal", dst+"-wal"); err != nil {
			return "", refuse("cannot copy the source database journal: %v", err)
		}
	}
	b1, _, err1 := stat(db)
	w1, hasWAL1, err2 := stat(db + "-wal")
	if err1 != nil || err2 != nil || b0 != b1 || hasWAL != hasWAL1 || w0 != w1 {
		return "", refuse("the source database changed while it was copied; stop the old server and run the import again")
	}
	hdr := make([]byte, 16)
	f, err := os.Open(dst)
	if err != nil {
		return "", err
	}
	defer f.Close()
	if _, err = io.ReadFull(f, hdr); err != nil || string(hdr) != "SQLite format 3\x00" {
		return "", refuse("%s is not a SQLite database", db)
	}
	return dst, nil
}

// loadImportSource copies, migrates and reads the source and checks everything
// that can be checked without the target: schema, references, files.
func loadImportSource(ctx context.Context, from, tmp string) (*importSource, error) {
	copyPath, err := copySourceDatabase(from, tmp)
	if err != nil {
		return nil, err
	}
	pool, err := sql.Open("sqlite", copyPath)
	if err != nil {
		return nil, refuse("cannot open the source copy: %v", err)
	}
	defer pool.Close()
	pool.SetMaxOpenConns(1)
	if _, err = pool.ExecContext(ctx, "PRAGMA busy_timeout=5000"); err != nil {
		return nil, refuse("cannot open the source copy: %v", err)
	}
	var check string
	if err = pool.QueryRowContext(ctx, "PRAGMA quick_check").Scan(&check); err != nil || check != "ok" {
		return nil, refuse("the source database failed SQLite's integrity check (%s)", clipText(check, 120))
	}
	for _, table := range []string{"spaces", "articles", "revisions"} {
		var n int
		if err = pool.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&n); err != nil || n != 1 {
			return nil, refuse("the source database has no %q table: not a trusted-mode wiki database", table)
		}
	}
	// The normal trusted migration brings an older schema to the current one.
	trusted := &Server{DB: &database{pool: pool}, DataDir: tmp}
	if err = trusted.migrate(); err != nil {
		return nil, refuse("the source schema cannot be migrated by the trusted migration: %v", err)
	}
	data, err := loadImpData(ctx, pool, true)
	if err != nil {
		return nil, refuse("the source schema cannot be read after the trusted migration: %v", err)
	}
	src := &importSource{Data: data}
	if err = src.checkReferences(); err != nil {
		return nil, err
	}
	if src.Ordered, err = orderArticles(data.Articles); err != nil {
		return nil, err
	}
	if err = src.checkFiles(from); err != nil {
		return nil, err
	}
	return src, nil
}

func clipText(s string, n int) string {
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}

// first lists at most five problems.
type problems []string

func (p *problems) add(format string, args ...any) { *p = append(*p, fmt.Sprintf(format, args...)) }
func (p problems) err(what string) error {
	if len(p) == 0 {
		return nil
	}
	shown := p
	more := ""
	if len(p) > 5 {
		shown, more = p[:5], fmt.Sprintf(" (and %d more)", len(p)-5)
	}
	return refuse("%s: %s%s", what, strings.Join(shown, "; "), more)
}

// checkReferences refuses data PostgreSQL would reject or the owner could not see.
func (s *importSource) checkReferences() error {
	d := s.Data
	var bad problems
	spaces := map[string]bool{}
	for _, sp := range d.Spaces {
		spaces[sp.ID] = true
	}
	articles := map[string]impArticle{}
	for _, a := range d.Articles {
		articles[a.ID] = a
		if !spaces[a.SpaceID] {
			bad.add("article %s is in the missing space %q", a.ID, a.SpaceID)
		}
		if a.ProjectID != "" {
			bad.add("article %s has a project (%q); trusted mode has no projects", a.ID, a.ProjectID)
		}
	}
	if err := bad.err("the source database is inconsistent"); err != nil {
		return err
	}
	revs := map[string]bool{}
	for _, r := range d.Revisions {
		if _, ok := articles[r.ArticleID]; !ok {
			bad.add("revision %d belongs to the missing article %s", r.ID, r.ArticleID)
		}
		key := fmt.Sprintf("%s#%d", r.ArticleID, r.Revision)
		if revs[key] {
			bad.add("article %s has two revisions numbered %d", r.ArticleID, r.Revision)
		}
		revs[key] = true
		var snap struct {
			SpaceID string `json:"space_id"`
		}
		if err := json.Unmarshal([]byte(r.Snapshot), &snap); err != nil || !spaces[snap.SpaceID] {
			bad.add("revision %d of article %s has a snapshot that is not an article of an existing space", r.ID, r.ArticleID)
		}
	}
	for _, c := range d.Comments {
		if _, ok := articles[c.ArticleID]; !ok {
			bad.add("comment %s belongs to the missing article %s", c.ID, c.ArticleID)
		}
	}
	for _, a := range d.Attachments {
		if _, ok := articles[a.ArticleID]; !ok {
			bad.add("attachment %s belongs to the missing article %s", a.ID, a.ArticleID)
		}
	}
	for _, f := range d.Favorites {
		if _, ok := articles[f.ArticleID]; !ok {
			bad.add("favorite of the missing article %s", f.ArticleID)
		}
	}
	artifacts := map[string]bool{}
	for _, a := range d.Artifacts {
		artifacts[a.ID] = true
		if !spaces[a.SpaceID] {
			bad.add("artifact %s is in the missing space %q", a.ID, a.SpaceID)
		}
		if a.ProjectID != "" {
			bad.add("artifact %s has a project (%q); trusted mode has no projects", a.ID, a.ProjectID)
		}
	}
	for _, r := range d.ArtRevs {
		if !artifacts[r.ArtifactID] {
			bad.add("artifact revision %d belongs to the missing artifact %s", r.Revision, r.ArtifactID)
		}
	}
	for _, sh := range d.ArtShares {
		if !artifacts[sh.ArtifactID] {
			bad.add("artifact link %s belongs to the missing artifact %s", sh.ID, sh.ArtifactID)
		}
		if !sha256Pattern.MatchString(sh.TokenHash) {
			bad.add("artifact link %s has an invalid token hash", sh.ID)
		}
	}
	return bad.err("the source database is inconsistent")
}

// orderArticles puts every parent before its children and refuses a parent that
// is missing, in another space, or part of a cycle: the server's insert trigger
// requires the parent in the same space and project.
func orderArticles(in []impArticle) ([]impArticle, error) {
	byID := make(map[string]impArticle, len(in))
	for _, a := range in {
		byID[a.ID] = a
	}
	var bad problems
	for _, a := range in {
		if a.ParentID == "" {
			continue
		}
		p, ok := byID[a.ParentID]
		switch {
		case !ok:
			bad.add("article %s has the missing parent %s", a.ID, a.ParentID)
		case p.SpaceID != a.SpaceID:
			bad.add("article %s is in space %s but its parent %s is in space %s", a.ID, a.SpaceID, p.ID, p.SpaceID)
		}
	}
	if err := bad.err("the article tree cannot be imported"); err != nil {
		return nil, err
	}
	done := make(map[string]bool, len(in))
	out := make([]impArticle, 0, len(in))
	for len(out) < len(in) {
		progressed := false
		for _, a := range in {
			if done[a.ID] || (a.ParentID != "" && !done[a.ParentID]) {
				continue
			}
			done[a.ID] = true
			out = append(out, a)
			progressed = true
		}
		if !progressed {
			for _, a := range in {
				if !done[a.ID] {
					bad.add("article %s is part of a parent cycle", a.ID)
				}
			}
			return nil, bad.err("the article tree cannot be imported")
		}
	}
	return out, nil
}

// checkFiles finds every file the database lists in files/, hashes it and fills
// Objects. A missing, unreadable or wrong-sized file is a refusal.
func (s *importSource) checkFiles(from string) error {
	dir := filepath.Join(from, "files")
	objs := map[string]*importObject{}
	var bad problems
	locate := func(key, mime string, size int64, sha string, what string) {
		if !validObjectKey(key) {
			bad.add("%s has the invalid file name %q", what, clipText(key, 60))
			return
		}
		if o, ok := objs[key]; ok {
			if o.Size != size || (sha != "" && o.SHA256 != sha) {
				bad.add("%s disagrees with another row about the file %s", what, key)
			}
			return
		}
		p := filepath.Join(dir, key)
		fi, err := os.Lstat(p)
		if errors.Is(err, os.ErrNotExist) {
			bad.add("%s: file %s is missing in files/", what, key)
			return
		}
		if err != nil || !fi.Mode().IsRegular() {
			bad.add("%s: file %s is not a readable regular file", what, key)
			return
		}
		if fi.Size() != size {
			bad.add("%s: file %s has %d bytes, the database says %d", what, key, fi.Size(), size)
			return
		}
		f, err := os.Open(p)
		if err != nil {
			bad.add("%s: file %s cannot be opened", what, key)
			return
		}
		h := sha256.New()
		n, err := io.Copy(h, f)
		f.Close()
		if err != nil || n != size {
			bad.add("%s: file %s cannot be read", what, key)
			return
		}
		sum := hex.EncodeToString(h.Sum(nil))
		if sha != "" && sum != sha {
			bad.add("%s: file %s does not match its recorded SHA-256", what, key)
			return
		}
		objs[key] = &importObject{Key: key, Path: p, Size: size, SHA256: sum, MIME: mime}
	}
	for _, a := range s.Data.Attachments {
		locate(a.StoredName, a.MIME, a.Size, "", "attachment "+a.ID)
	}
	blobMeta := map[string]impArtifactBlob{}
	for _, b := range s.Data.ArtBlobs {
		if !sha256Pattern.MatchString(b.SHA256) || b.StoredName != artifactBlobKey(b.SHA256) || b.Size < 0 {
			bad.add("artifact blob %s has invalid metadata", clipText(b.SHA256, 16))
			continue
		}
		blobMeta[b.SHA256] = b
		locate(b.StoredName, b.MIME, b.Size, b.SHA256, "artifact blob "+b.SHA256[:12])
	}
	for _, r := range s.Data.ArtRevs {
		var files []artifactFile
		if err := json.Unmarshal([]byte(r.Manifest), &files); err != nil {
			bad.add("artifact %s revision %d has an unreadable manifest", r.ArtifactID, r.Revision)
			continue
		}
		for _, f := range files {
			if !sha256Pattern.MatchString(f.SHA256) {
				bad.add("artifact %s revision %d lists a file with an invalid SHA-256", r.ArtifactID, r.Revision)
				continue
			}
			if _, ok := blobMeta[f.SHA256]; !ok {
				// The server records a blob row for every stored file; recreate it from the manifest.
				s.Data.ArtBlobs = append(s.Data.ArtBlobs, impArtifactBlob{SHA256: f.SHA256, Size: f.Size, MIME: f.MIME, StoredName: artifactBlobKey(f.SHA256), CreatedAt: r.CreatedAt})
				blobMeta[f.SHA256] = s.Data.ArtBlobs[len(s.Data.ArtBlobs)-1]
			}
			locate(artifactBlobKey(f.SHA256), f.MIME, f.Size, f.SHA256, "artifact "+r.ArtifactID)
		}
	}
	if err := bad.err("files listed in the database are not usable"); err != nil {
		return err
	}
	keys := make([]string, 0, len(objs))
	for k := range objs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		s.Objects = append(s.Objects, *objs[k])
	}
	return nil
}
