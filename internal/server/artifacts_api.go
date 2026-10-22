package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgconn"
)

// queryer is what both *database and *transaction offer.
type queryer interface {
	QueryRow(string, ...any) *sql.Row
	Query(string, ...any) (*sql.Rows, error)
	Exec(string, ...any) (sql.Result, error)
}

// artifactError carries an HTTP status and optional extra JSON fields.
type artifactErrorExtra struct {
	*artifactError
	extra map[string]any
}

func (e *artifactErrorExtra) Unwrap() error { return e.artifactError }

func artConflict(current int) error {
	return &artifactErrorExtra{artErr(409, "revision conflict: current revision is %d", current), map[string]any{"current_revision": current}}
}

func writeArtifactError(w http.ResponseWriter, err error) {
	var withExtra *artifactErrorExtra
	if errors.As(err, &withExtra) {
		body := map[string]any{"error": withExtra.Msg}
		for k, v := range withExtra.extra {
			body[k] = v
		}
		writeJSON(w, withExtra.Code, body)
		return
	}
	var ae *artifactError
	if errors.As(err, &ae) {
		apiErr(w, ae.Code, ae.Msg)
		return
	}
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		switch pg.Code {
		case "42501":
			apiErr(w, http.StatusForbidden, "forbidden")
			return
		case "23505":
			apiErr(w, http.StatusConflict, "conflict")
			return
		case "23503", "23514":
			apiErr(w, http.StatusBadRequest, "invalid reference or value")
			return
		}
	}
	apiErr(w, http.StatusInternalServerError, "database error")
}

// artifactDecode reads a JSON request strictly (unknown fields are errors, so a
// misspelt key in a partial update is never silently ignored).
func artifactDecode(w http.ResponseWriter, r *http.Request, dst any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, artifactMaxBody))
	dec.DisallowUnknownFields()
	err := dec.Decode(dst)
	if err == nil {
		var extra any
		if e := dec.Decode(&extra); e != io.EOF {
			err = errors.New("unexpected data after the JSON value")
		}
	}
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			apiErr(w, http.StatusRequestEntityTooLarge, "request body too large")
		} else {
			apiErr(w, http.StatusBadRequest, "invalid input: "+clip(err.Error(), 200))
		}
		return false
	}
	return true
}

// ---- records ----

type artifactMeta struct {
	ID          string `json:"id"`
	SpaceID     string `json:"space_id"`
	ProjectID   string `json:"project_id"`
	Title       string `json:"title"`
	Description string `json:"description"`
	Visibility  string `json:"visibility"`
	OwnerID     string `json:"owner_id"`
	OwnerName   string `json:"owner_name"`
	Revision    int    `json:"revision"`
	Author      string `json:"author"`
	CreatedAt   string `json:"created_at"`
	UpdatedAt   string `json:"updated_at"`
	DeletedAt   string `json:"deleted_at,omitempty"`
	FileCount   int    `json:"file_count"`
}

type artifactPerms struct {
	CanEdit   bool `json:"can_edit"`
	CanShare  bool `json:"can_share"`
	CanManage bool `json:"can_manage"`
}

type artifactDetail struct {
	artifactMeta
	Files []artifactFileOut `json:"files"`
	artifactPerms
}

const artifactCols = `a.id,a.space_id,a.project_id,a.title,a.description,a.visibility,a.owner_id,a.revision,a.author,a.created_at,a.updated_at,a.deleted_at,a.file_count`

func (s *Server) ownerNameSQL() string {
	if s.DB.pg {
		return `coalesce((SELECT u.name FROM users u WHERE u.id=a.owner_id),'')`
	}
	return `''`
}

func scanArtifactMeta(row interface{ Scan(...any) error }) (artifactMeta, error) {
	var a artifactMeta
	var deleted sql.NullString
	err := row.Scan(&a.ID, &a.SpaceID, &a.ProjectID, &a.Title, &a.Description, &a.Visibility, &a.OwnerID, &a.Revision, &a.Author, &a.CreatedAt, &a.UpdatedAt, &deleted, &a.FileCount, &a.OwnerName)
	if deleted.Valid {
		a.DeletedAt = deleted.String
	}
	return a, err
}

func (s *Server) loadArtifact(q queryer, id string) (artifactMeta, error) {
	return scanArtifactMeta(q.QueryRow(`SELECT `+artifactCols+`,`+s.ownerNameSQL()+` FROM artifacts a WHERE a.id=?`, id))
}

func loadManifest(q queryer, id string, rev int) ([]artifactFile, bool, error) {
	var raw string
	err := q.QueryRow(`SELECT manifest FROM artifact_revisions WHERE artifact_id=? AND revision=?`, id, rev).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	var files []artifactFile
	if err := json.Unmarshal([]byte(raw), &files); err != nil {
		return nil, false, err
	}
	return files, true, nil
}

func (s *Server) isMCPScoped() bool { return s.principal != nil && s.principal.MCPProjects != nil }

// artifactRoleOf returns the caller's role rank (0 none, 3 editor, 4 manager,
// 5 owner) in a container and whether the container accepts writes. Trusted
// mode is anonymous: everything is allowed.
func (s *Server) artifactRoleOf(q queryer, space, project string) (int, bool) {
	if s.principal == nil {
		return 5, true
	}
	var role int
	var writable bool
	if q.QueryRow(`SELECT corporate_role_rank(corporate_effective_role(?,?,?)),corporate_container_writable(?,?)`, s.principal.ID, space, project, space, project).Scan(&role, &writable) != nil {
		return 0, false
	}
	return role, writable
}

// artifactPermsOf mirrors the Rights table of docs/ARTIFACTS.md. The database
// policies enforce the same rules; this only tells the client what to offer.
func (s *Server) artifactPermsOf(q queryer, a artifactMeta) artifactPerms {
	if s.principal == nil {
		return artifactPerms{true, true, true}
	}
	role, writable := s.artifactRoleOf(q, a.SpaceID, a.ProjectID)
	me := s.principal.ID
	edit := role >= 3 && writable && (a.Visibility == "inherit" || a.OwnerID == me)
	share := edit && (a.OwnerID == me || (a.Visibility == "inherit" && role >= 4))
	// Only the owner changes visibility: a manager who made an artifact private
	// would lose sight of it (private is the owner's alone).
	manage := edit && a.OwnerID == me
	if s.isMCPScoped() {
		share, manage = false, false
	}
	return artifactPerms{edit, share, manage}
}

func (s *Server) artifactDetailOf(q queryer, a artifactMeta, rev int) (artifactDetail, error) {
	if rev == 0 {
		rev = a.Revision
	}
	files, found, err := loadManifest(q, a.ID, rev)
	if err != nil {
		return artifactDetail{}, err
	}
	if !found {
		return artifactDetail{}, sql.ErrNoRows
	}
	return artifactDetail{a, publicManifest(files), s.artifactPermsOf(q, a)}, nil
}

// ---- audit ----

func (s *Server) artifactAudit(tx *transaction, action, resourceType, id string, details map[string]any) error {
	if s.principal == nil || s.isMCPScoped() {
		return nil // trusted mode has no audit table; scoped MCP requests are audited per request
	}
	if s.principal.MCPBearer {
		details["via"] = "mcp"
	}
	return corporateAudit(tx, s.principal.ID, action, resourceType, id, details)
}

// ---- validation helpers ----

func cleanArtifactText(title, description, message string) (string, string, string, error) {
	title = strings.TrimSpace(title)
	if title == "" || utf8.RuneCountInString(title) > artifactMaxTitle {
		return "", "", "", artErr(400, "title is required and at most %d characters", artifactMaxTitle)
	}
	if utf8.RuneCountInString(description) > artifactMaxDescription {
		return "", "", "", artErr(400, "description is at most %d characters", artifactMaxDescription)
	}
	if utf8.RuneCountInString(message) > artifactMaxMessage {
		return "", "", "", artErr(400, "message is at most %d characters", artifactMaxMessage)
	}
	return title, description, message, nil
}

func validArtifactVisibility(v string) bool { return v == "inherit" || v == "private" }

// ---- blob storage ----

func putArtifactBlob(ctx context.Context, store objectStore, b artifactBlob) error {
	err := store.Put(ctx, artifactBlobKey(b.File.SHA256), bytes.NewReader(b.Data), int64(len(b.Data)), b.File.MIME)
	if err != nil && errors.Is(err, fs.ErrExist) {
		return nil // content-addressed: the same bytes are already stored
	}
	return err
}

func readArtifactBlob(ctx context.Context, store objectStore, f artifactFile) ([]byte, error) {
	rc, err := store.Get(ctx, artifactBlobKey(f.SHA256))
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	data, err := io.ReadAll(io.LimitReader(rc, artifactMaxFileBytes+1))
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(data)
	if int64(len(data)) != f.Size || hex.EncodeToString(sum[:]) != f.SHA256 {
		return nil, errors.New("stored object does not match its manifest")
	}
	return data, nil
}

// artifactStoreBlobs writes new file contents to the object store outside any
// transaction (releasing a request-pinned connection meanwhile) and returns a
// server copy whose database is usable again.
func (s *Server) artifactStoreBlobs(ctx context.Context, blobs []artifactBlob) (*Server, func(), error) {
	if len(blobs) == 0 {
		return s, func() {}, nil
	}
	store, err := s.objectStorage()
	if err != nil {
		return nil, nil, artErr(503, "object storage unavailable")
	}
	s.DB.releaseForTransfer()
	done := map[string]bool{}
	for _, b := range blobs {
		if done[b.File.SHA256] {
			continue
		}
		done[b.File.SHA256] = true
		if err := putArtifactBlob(ctx, store, b); err != nil {
			return nil, nil, artErr(502, "could not store file")
		}
	}
	db, fin, err := s.DB.resumeAfterTransfer(ctx)
	if err != nil {
		return nil, nil, artErr(503, "database unavailable")
	}
	cp := *s
	cp.DB = db
	return &cp, fin, nil
}

func insertArtifactBlobs(tx *transaction, blobs []artifactBlob, ts string) error {
	seen := map[string]bool{}
	for _, b := range blobs {
		if seen[b.File.SHA256] {
			continue
		}
		seen[b.File.SHA256] = true
		if err := insertArtifactBlob(tx, b, ts); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) publicLinksEnabled(q queryer) bool {
	if !s.DB.pg {
		return true
	}
	var v string
	err := q.QueryRow(`SELECT value FROM app_settings WHERE key='public_artifact_links'`).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return true
	}
	return err == nil && v != "false"
}

// ---- list ----

func likeTerm(term string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return "%" + r.Replace(term) + "%"
}

type artifactListItem struct {
	ID          string `json:"id"`
	SpaceID     string `json:"space_id"`
	ProjectID   string `json:"project_id"`
	Title       string `json:"title"`
	Description string `json:"description"`
	Visibility  string `json:"visibility"`
	OwnerID     string `json:"owner_id"`
	OwnerName   string `json:"owner_name"`
	Revision    int    `json:"revision"`
	UpdatedAt   string `json:"updated_at"`
	FileCount   int    `json:"file_count"`
}

func (s *Server) listArtifacts(space, project, query string, limit, offset int) ([]artifactListItem, error) {
	where := []string{"a.deleted_at IS NULL"}
	args := []any{}
	if space != "" {
		where = append(where, "a.space_id=?")
		args = append(args, space)
	}
	if project != "" {
		where = append(where, "a.project_id=?")
		args = append(args, project)
	}
	for _, term := range searchTerms(query) {

		where = append(where, `a.search_text LIKE ? ESCAPE '\'`)
		args = append(args, likeTerm(term))
	}
	args = append(args, limit, offset)
	rows, err := s.DB.Query(`SELECT a.id,a.space_id,a.project_id,a.title,a.description,a.visibility,a.owner_id,`+s.ownerNameSQL()+`,a.revision,a.updated_at,a.file_count FROM artifacts a WHERE `+strings.Join(where, " AND ")+` ORDER BY a.updated_at DESC,a.id LIMIT ? OFFSET ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []artifactListItem{}
	for rows.Next() {
		var it artifactListItem
		if err := rows.Scan(&it.ID, &it.SpaceID, &it.ProjectID, &it.Title, &it.Description, &it.Visibility, &it.OwnerID, &it.OwnerName, &it.Revision, &it.UpdatedAt, &it.FileCount); err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

func intParam(r *http.Request, name string, def, max int) int {
	n, err := strconv.Atoi(r.URL.Query().Get(name))
	if err != nil || n < 0 {
		return def
	}
	if max > 0 && n > max {
		return max
	}
	return n
}

func (s *Server) artifactList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit := intParam(r, "limit", 100, 200)
	if limit == 0 {
		limit = 100
	}
	items, err := s.listArtifacts(q.Get("space_id"), q.Get("project_id"), q.Get("q"), limit, intParam(r, "offset", 0, 0))
	if err != nil {
		writeArtifactError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, items)
}

// ---- create ----

type artifactCreateIn struct {
	SpaceID     string           `json:"space_id"`
	ProjectID   string           `json:"project_id"`
	Title       string           `json:"title"`
	Description string           `json:"description"`
	Visibility  string           `json:"visibility"`
	Files       []artifactFileIn `json:"files"`
	Message     string           `json:"message"`
}

func (s *Server) artifactCreate(w http.ResponseWriter, r *http.Request) {
	// The body is read, then only space_id and project_id are looked at; the
	// container check (rights) comes before the files are parsed or validated.
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, artifactMaxBody))
	if err != nil {
		apiErr(w, http.StatusRequestEntityTooLarge, "request body too large")
		return
	}
	var head struct {
		SpaceID   string `json:"space_id"`
		ProjectID string `json:"project_id"`
	}
	if json.Unmarshal(raw, &head) != nil {
		apiErr(w, http.StatusBadRequest, "invalid input")
		return
	}
	if err := s.artifactCreateCheck(head.SpaceID, head.ProjectID); err != nil {
		writeArtifactError(w, err)
		return
	}
	var in artifactCreateIn
	if err := decodeStrictJSON(raw, &in); err != nil {
		apiErr(w, http.StatusBadRequest, "invalid input: "+clip(err.Error(), 200))
		return
	}
	detail, err := s.createArtifact(r.Context(), in)
	if err != nil {
		writeArtifactError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, detail)
}

// artifactCreateCheck is the cheap container check done before any file is
// parsed or stored: the space or project must accept the caller's new artifact.
func (s *Server) artifactCreateCheck(space, project string) error {
	if !validPathID(space) || project != "" && !validPathID(project) {
		return artErr(400, "space_id is required")
	}
	if s.principal == nil {
		var archived sql.NullString
		if e := s.DB.QueryRow(`SELECT archived_at FROM spaces WHERE id=?`, space).Scan(&archived); e != nil {
			return artErr(404, "space not found")
		}
		if archived.Valid {
			return artErr(409, "space is archived")
		}
	} else if role, writable := s.artifactRoleOf(s.DB, space, project); role < 3 || !writable {
		return artErr(403, "editor access required")
	}
	return nil
}

func (s *Server) createArtifact(ctx context.Context, in artifactCreateIn) (artifactDetail, error) {
	if err := s.artifactCreateCheck(in.SpaceID, in.ProjectID); err != nil {
		return artifactDetail{}, err
	}
	title, desc, msg, err := cleanArtifactText(in.Title, in.Description, in.Message)
	if err != nil {
		return artifactDetail{}, err
	}
	if in.Visibility == "" {
		in.Visibility = "inherit"
	}
	if !validArtifactVisibility(in.Visibility) {
		return artifactDetail{}, artErr(400, "visibility must be inherit or private")
	}
	if s.isMCPScoped() && in.Visibility != "inherit" {
		return artifactDetail{}, artErr(403, "MCP cannot set artifact visibility")
	}
	blobs, _, err := parseArtifactFiles(in.Files, false)
	if err != nil {
		return artifactDetail{}, err
	}
	files, err := mergeArtifactFiles(nil, blobs, nil)
	if err != nil {
		return artifactDetail{}, err
	}
	rs, done, err := s.artifactStoreBlobs(ctx, blobs)
	if err != nil {
		return artifactDetail{}, err
	}
	defer done()
	tx, err := rs.DB.Begin()
	if err != nil {
		return artifactDetail{}, err
	}
	defer tx.Rollback()
	if rs.DB.pg && in.ProjectID != "" {
		// Serializes with a project move, like article creation.
		lock := " FOR SHARE"
		if rs.principal != nil && (rs.principal.MCPMode != "" || rs.principal.MCPProjects != nil) {
			lock = ""
		}
		var one int
		if tx.QueryRow(`SELECT 1 FROM projects WHERE id=? AND space_id=?`+lock, in.ProjectID, in.SpaceID).Scan(&one) != nil {
			return artifactDetail{}, artErr(400, "project not found in this space")
		}
	}
	owner := ""
	if rs.principal != nil {
		owner = rs.principal.ID
	}
	ts := stamp()
	a := artifactMeta{ID: newID(), SpaceID: in.SpaceID, ProjectID: in.ProjectID, Title: title, Description: desc, Visibility: in.Visibility, OwnerID: owner, Revision: 1, Author: rs.changeAuthor("human"), CreatedAt: ts, UpdatedAt: ts, FileCount: len(files)}
	proj := artifactProjectionSet{}.next(files, blobs, nil)
	projRaw, _ := json.Marshal(proj)
	manifest, _ := json.Marshal(files)
	if _, err = tx.Exec(`INSERT INTO artifacts(id,space_id,project_id,title,description,visibility,owner_id,revision,author,created_at,updated_at,deleted_at,file_count,projection,search_text) VALUES(?,?,?,?,?,?,?,1,?,?,?,NULL,?,?,?)`, a.ID, a.SpaceID, a.ProjectID, a.Title, a.Description, a.Visibility, a.OwnerID, a.Author, ts, ts, a.FileCount, string(projRaw), artifactSearchText(title, desc, files, proj)); err != nil {
		return artifactDetail{}, err
	}
	if err = insertArtifactBlobs(tx, blobs, ts); err != nil {
		return artifactDetail{}, err
	}
	if _, err = tx.Exec(`INSERT INTO artifact_revisions(artifact_id,revision,author,message,created_at,manifest,file_count) VALUES(?,?,?,?,?,?,?)`, a.ID, 1, a.Author, msg, ts, string(manifest), len(files)); err != nil {
		return artifactDetail{}, err
	}
	if err = rs.artifactAudit(tx, "artifact.create", "artifact", a.ID, map[string]any{"space_id": a.SpaceID, "project_id": a.ProjectID, "revision": 1, "files": len(files), "visibility": a.Visibility}); err != nil {
		return artifactDetail{}, err
	}
	if err = tx.Commit(); err != nil {
		return artifactDetail{}, err
	}
	if rs.principal != nil {
		a.OwnerName = rs.principal.Name
	}
	return artifactDetail{a, publicManifest(files), rs.artifactPermsOf(rs.DB, a)}, nil
}

// ---- read ----

func (s *Server) artifactGet(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	a, err := s.loadArtifact(s.DB, id)
	if err != nil {
		artifactMissing(w, err)
		return
	}
	rev := intParam(r, "revision", 0, 0)
	if rev > a.Revision {
		apiErr(w, http.StatusNotFound, "revision not found")
		return
	}
	d, err := s.artifactDetailOf(s.DB, a, rev)
	if errors.Is(err, sql.ErrNoRows) {
		apiErr(w, http.StatusNotFound, "revision not found")
		return
	}
	if err != nil {
		writeArtifactError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, d)
}

func artifactMissing(w http.ResponseWriter, err error) {
	if errors.Is(err, sql.ErrNoRows) {
		apiErr(w, http.StatusNotFound, "artifact not found")
		return
	}
	writeArtifactError(w, err)
}

func (s *Server) artifactHistory(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := s.loadArtifact(s.DB, id); err != nil {
		artifactMissing(w, err)
		return
	}
	name := `''`
	if s.DB.pg {
		name = `coalesce((SELECT u.name FROM users u WHERE u.id=r.author),'')`
	}
	rows, err := s.DB.Query(`SELECT r.revision,r.author,`+name+`,r.message,r.created_at,r.file_count FROM artifact_revisions r WHERE r.artifact_id=? ORDER BY r.revision DESC`, id)
	if err != nil {
		writeArtifactError(w, err)
		return
	}
	defer rows.Close()
	type entry struct {
		Revision   int    `json:"revision"`
		Author     string `json:"author"`
		AuthorName string `json:"author_name"`
		Message    string `json:"message"`
		CreatedAt  string `json:"created_at"`
		FileCount  int    `json:"file_count"`
	}
	out := []entry{}
	for rows.Next() {
		var e entry
		if err := rows.Scan(&e.Revision, &e.Author, &e.AuthorName, &e.Message, &e.CreatedAt, &e.FileCount); err != nil {
			writeArtifactError(w, err)
			return
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		writeArtifactError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// artifactFileGet serves the raw bytes of one file for editing: always an
// attachment, never rendered, and sandboxed even if opened directly.
func (s *Server) artifactFileGet(w http.ResponseWriter, r *http.Request) {
	id, rel := r.PathValue("id"), r.PathValue("path")
	if _, err := validArtifactPath(rel); err != nil {
		apiErr(w, http.StatusNotFound, "file not found")
		return
	}
	a, err := s.loadArtifact(s.DB, id)
	if err != nil {
		artifactMissing(w, err)
		return
	}
	rev := intParam(r, "revision", a.Revision, 0)
	if rev < 1 || rev > a.Revision {
		apiErr(w, http.StatusNotFound, "revision not found")
		return
	}
	files, found, err := loadManifest(s.DB, id, rev)
	if err != nil {
		writeArtifactError(w, err)
		return
	}
	if !found {
		apiErr(w, http.StatusNotFound, "revision not found")
		return
	}
	var file artifactFile
	ok := false
	for _, f := range files {
		if f.Path == rel {
			file, ok = f, true
			break
		}
	}
	if !ok {
		apiErr(w, http.StatusNotFound, "file not found")
		return
	}
	store, err := s.objectStorage()
	if err != nil {
		apiErr(w, http.StatusServiceUnavailable, "object storage unavailable")
		return
	}
	s.DB.releaseForTransfer()
	data, err := readArtifactBlob(r.Context(), store, file)
	if err != nil {
		if errors.Is(err, errObjectNotFound) {
			apiErr(w, http.StatusNotFound, "file not found")
		} else {
			apiErr(w, http.StatusBadGateway, "could not read file")
		}
		return
	}
	h := w.Header()
	h.Set("Content-Type", artifactContentType(file.MIME))
	h.Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": path.Base(rel)}))
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", "sandbox; default-src 'none'")
	h.Set("Cache-Control", "private, no-store")
	h.Set("Content-Length", strconv.Itoa(len(data)))
	_, _ = w.Write(data)
}

// ---- update / delete / restore ----

type artifactChange struct {
	base       int
	title      *string
	desc       *string
	visibility *string
	blobs      []artifactBlob
	deletes    []string
	hasFiles   bool
	message    string
	lifecycle  string // "", "delete" or "restore"
}

type artifactUpdateIn struct {
	BaseRevision int               `json:"base_revision"`
	Title        *string           `json:"title"`
	Description  *string           `json:"description"`
	Visibility   *string           `json:"visibility"`
	Files        *[]artifactFileIn `json:"files"`
	Message      string            `json:"message"`
}

func (s *Server) artifactUpdate(w http.ResponseWriter, r *http.Request) {
	// Rights first: the body (up to 28 MB of files) is not even read for a caller
	// who may not edit.
	if cur, err := s.loadArtifact(s.DB, r.PathValue("id")); err != nil {
		artifactMissing(w, err)
		return
	} else if !s.artifactPermsOf(s.DB, cur).CanEdit {
		apiErr(w, http.StatusForbidden, "editing this artifact is not permitted")
		return
	}
	var in artifactUpdateIn
	if !artifactDecode(w, r, &in) {
		return
	}
	d, err := s.updateArtifact(r.Context(), r.PathValue("id"), in)
	if err != nil {
		writeArtifactError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, d)
}

func (s *Server) updateArtifact(ctx context.Context, id string, in artifactUpdateIn) (artifactDetail, error) {
	if in.BaseRevision < 1 {
		return artifactDetail{}, artErr(400, "base_revision required")
	}
	if cur, err := s.loadArtifact(s.DB, id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return artifactDetail{}, artErr(404, "artifact not found")
		}
		return artifactDetail{}, err
	} else if !s.artifactPermsOf(s.DB, cur).CanEdit {
		return artifactDetail{}, artErr(403, "editing this artifact is not permitted")
	}
	ch := artifactChange{base: in.BaseRevision, message: in.Message}
	if utf8.RuneCountInString(in.Message) > artifactMaxMessage {
		return artifactDetail{}, artErr(400, "message is at most %d characters", artifactMaxMessage)
	}
	if in.Title != nil {
		t, _, _, err := cleanArtifactText(*in.Title, "", "")
		if err != nil {
			return artifactDetail{}, err
		}
		ch.title = &t
	}
	if in.Description != nil {
		_, d, _, err := cleanArtifactText("x", *in.Description, "")
		if err != nil {
			return artifactDetail{}, err
		}
		ch.desc = &d
	}
	if in.Visibility != nil {
		if !validArtifactVisibility(*in.Visibility) {
			return artifactDetail{}, artErr(400, "visibility must be inherit or private")
		}
		if s.isMCPScoped() {
			return artifactDetail{}, artErr(403, "MCP cannot change artifact visibility")
		}
		ch.visibility = in.Visibility
	}
	if in.Files != nil {
		blobs, deletes, err := parseArtifactFiles(*in.Files, true)
		if err != nil {
			return artifactDetail{}, err
		}
		ch.blobs, ch.deletes, ch.hasFiles = blobs, deletes, true
	}
	return s.commitArtifact(ctx, id, ch)
}

type artifactBaseIn struct {
	BaseRevision int    `json:"base_revision"`
	Message      string `json:"message"`
}

func (s *Server) artifactDelete(w http.ResponseWriter, r *http.Request) {
	s.artifactLifecycle(w, r, "delete")
}
func (s *Server) artifactRestore(w http.ResponseWriter, r *http.Request) {
	s.artifactLifecycle(w, r, "restore")
}

func (s *Server) artifactLifecycle(w http.ResponseWriter, r *http.Request, kind string) {
	var in artifactBaseIn
	if !artifactDecode(w, r, &in) {
		return
	}
	if in.BaseRevision < 1 {
		apiErr(w, http.StatusBadRequest, "base_revision required")
		return
	}
	d, err := s.commitArtifact(r.Context(), r.PathValue("id"), artifactChange{base: in.BaseRevision, lifecycle: kind, message: in.Message})
	if err != nil {
		writeArtifactError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, d)
}

// commitArtifact applies one change as a new revision: permission and base
// revision checks, optimistic locking, blobs first (content-addressed, so a
// failure leaves only unreferenced objects), then one transaction.
func (s *Server) commitArtifact(ctx context.Context, id string, ch artifactChange) (artifactDetail, error) {
	cur, err := s.loadArtifact(s.DB, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return artifactDetail{}, artErr(404, "artifact not found")
		}
		return artifactDetail{}, err
	}
	if err := s.artifactCheckChange(s.DB, cur, ch); err != nil {
		return artifactDetail{}, err
	}
	rs, done, err := s.artifactStoreBlobs(ctx, ch.blobs)
	if err != nil {
		return artifactDetail{}, err
	}
	defer done()
	tx, err := rs.DB.Begin()
	if err != nil {
		return artifactDetail{}, err
	}
	defer tx.Rollback()
	if rs.DB.pg {
		var rev int
		if err := tx.QueryRow(`SELECT revision FROM artifacts WHERE id=? FOR UPDATE`, id).Scan(&rev); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return artifactDetail{}, artErr(403, "forbidden")
			}
			return artifactDetail{}, err
		}
	}
	cur, err = rs.loadArtifact(tx, id)
	if err != nil {
		return artifactDetail{}, artErr(404, "artifact not found")
	}
	if err := rs.artifactCheckChange(tx, cur, ch); err != nil {
		return artifactDetail{}, err
	}
	old, found, err := loadManifest(tx, id, cur.Revision)
	if err != nil || !found {
		return artifactDetail{}, errors.New("missing manifest")
	}
	files := old
	if ch.hasFiles {
		if files, err = mergeArtifactFiles(old, ch.blobs, ch.deletes); err != nil {
			return artifactDetail{}, err
		}
	}
	next := cur
	if ch.title != nil {
		next.Title = *ch.title
	}
	if ch.desc != nil {
		next.Description = *ch.desc
	}
	visibilityChanged := ch.visibility != nil && *ch.visibility != cur.Visibility
	if visibilityChanged {
		next.Visibility = *ch.visibility
	}
	filesChanged := manifestChanged(old, files)
	textChanged := next.Title != cur.Title || next.Description != cur.Description
	switch ch.lifecycle {
	case "":
		if !filesChanged && !textChanged && !visibilityChanged {
			// Nothing to record: answer with the current state, no new revision.
			return rs.artifactDetailOf(tx, cur, 0)
		}
	}
	ts := stamp()
	next.Revision = cur.Revision + 1
	next.UpdatedAt = ts
	next.Author = rs.changeAuthor("human")
	next.FileCount = len(files)
	message := ch.message
	switch ch.lifecycle {
	case "delete":
		next.DeletedAt = ts
		if message == "" {
			message = "Удаление артефакта"
		}
	case "restore":
		next.DeletedAt = ""
		if message == "" {
			message = "Восстановление артефакта"
		}
	}
	var projRaw string
	if err := tx.QueryRow(`SELECT projection FROM artifacts WHERE id=?`, id).Scan(&projRaw); err != nil {
		return artifactDetail{}, err
	}
	proj := parseProjectionSet(projRaw).next(files, ch.blobs, ch.deletes)
	newProj, _ := json.Marshal(proj)
	manifest, _ := json.Marshal(files)
	// The revision row goes first so that its policy sees the artifact in its
	// previous state.
	if _, err := tx.Exec(`INSERT INTO artifact_revisions(artifact_id,revision,author,message,created_at,manifest,file_count) VALUES(?,?,?,?,?,?,?)`, id, next.Revision, next.Author, message, ts, string(manifest), len(files)); err != nil {
		return artifactDetail{}, err
	}
	if err := insertArtifactBlobs(tx, ch.blobs, ts); err != nil {
		return artifactDetail{}, err
	}
	if ch.lifecycle == "delete" && !rs.DB.pg {
		if _, err := tx.Exec(`UPDATE artifact_shares SET revoked_at=? WHERE artifact_id=? AND revoked_at IS NULL`, ts, id); err != nil {
			return artifactDetail{}, err
		}
	}
	res, err := tx.Exec(`UPDATE artifacts SET title=?,description=?,visibility=?,revision=?,author=?,updated_at=?,deleted_at=?,file_count=?,projection=?,search_text=? WHERE id=? AND revision=?`, next.Title, next.Description, next.Visibility, next.Revision, next.Author, ts, nullString(next.DeletedAt), next.FileCount, string(newProj), artifactSearchText(next.Title, next.Description, files, proj), id, cur.Revision)
	if err != nil {
		return artifactDetail{}, err
	}
	if n, e := res.RowsAffected(); e != nil || n != 1 {
		return artifactDetail{}, artErr(403, "forbidden")
	}
	details := map[string]any{"artifact_id": id, "space_id": cur.SpaceID, "project_id": cur.ProjectID, "revision": next.Revision}
	switch ch.lifecycle {
	case "delete":
		err = rs.artifactAudit(tx, "artifact.delete", "artifact", id, details)
	case "restore":
		err = rs.artifactAudit(tx, "artifact.restore", "artifact", id, details)
	default:
		if filesChanged || textChanged {
			details["files_written"] = len(ch.blobs)
			details["files_deleted"] = len(ch.deletes)
			details["title_changed"] = next.Title != cur.Title
			details["description_changed"] = next.Description != cur.Description
			err = rs.artifactAudit(tx, "artifact.update", "artifact", id, details)
		}
		if err == nil && visibilityChanged {
			err = rs.artifactAudit(tx, "artifact.visibility", "artifact", id, map[string]any{"artifact_id": id, "revision": next.Revision, "from": cur.Visibility, "to": next.Visibility})
		}
	}
	if err != nil {
		return artifactDetail{}, err
	}
	if err := tx.Commit(); err != nil {
		return artifactDetail{}, err
	}
	return artifactDetail{next, publicManifest(files), rs.artifactPermsOf(rs.DB, next)}, nil
}

// artifactCheckChange applies the rights, lifecycle and base-revision rules.
func (s *Server) artifactCheckChange(q queryer, cur artifactMeta, ch artifactChange) error {
	perms := s.artifactPermsOf(q, cur)
	if !perms.CanEdit {
		return artErr(403, "editing this artifact is not permitted")
	}
	if ch.visibility != nil && *ch.visibility != cur.Visibility && !perms.CanManage {
		return artErr(403, "only the owner can change visibility")
	}
	switch ch.lifecycle {
	case "delete":
		if cur.DeletedAt != "" {
			return artErr(409, "artifact is already deleted")
		}
	case "restore":
		if cur.DeletedAt == "" {
			return artErr(409, "artifact is not deleted")
		}
	default:
		if cur.DeletedAt != "" {
			return artErr(409, "artifact is deleted; restore it first")
		}
	}
	if cur.Revision != ch.base {
		return artConflict(cur.Revision)
	}
	return nil
}

func manifestChanged(a, b []artifactFile) bool {
	if len(a) != len(b) {
		return true
	}
	for i := range a {
		if a[i] != b[i] {
			return true
		}
	}
	return false
}

// ---- view (render capability) ----

func (s *Server) artifactView(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var in struct {
		Revision *int `json:"revision"`
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 4096))
	if err != nil {
		apiErr(w, http.StatusBadRequest, "invalid input")
		return
	}
	if len(bytes.TrimSpace(body)) > 0 {
		dec := json.NewDecoder(bytes.NewReader(body))
		dec.DisallowUnknownFields()
		if dec.Decode(&in) != nil {
			apiErr(w, http.StatusBadRequest, "invalid input")
			return
		}
	}
	a, err := s.loadArtifact(s.DB, id)
	if err != nil || a.DeletedAt != "" {
		apiErr(w, http.StatusNotFound, "artifact not found")
		return
	}
	rev := a.Revision
	if in.Revision != nil {
		rev = *in.Revision
	}
	if rev < 1 || rev > a.Revision {
		apiErr(w, http.StatusNotFound, "revision not found")
		return
	}
	if _, found, err := loadManifest(s.DB, id, rev); err != nil || !found {
		apiErr(w, http.StatusNotFound, "revision not found")
		return
	}
	exp := time.Now().Add(artifactCapTTL)
	cap := s.artifacts().sign(artifactCap{Artifact: id, Revision: rev, Expires: exp.Unix(), Kind: "user"})
	writeJSON(w, http.StatusOK, map[string]any{"frame_url": s.artifactFramePrefix() + "/artifact-frame/" + cap + "/index.html", "expires_at": time.Unix(exp.Unix(), 0).UTC().Format(time.RFC3339)})
}

// ---- public links ----

type artifactShareOut struct {
	ID        string `json:"id"`
	Revision  *int   `json:"revision"`
	CreatedBy string `json:"created_by"`
	CreatedAt string `json:"created_at"`
	RevokedAt string `json:"revoked_at,omitempty"`
}

// artifactLinksAccess authorizes list/revoke (manage=true: no writable-container
// test, so links stay manageable in an archived container) or creation.
func (s *Server) artifactShareAccess(w http.ResponseWriter, id string, create bool) (artifactMeta, bool) {
	a, err := s.loadArtifact(s.DB, id)
	if err != nil {
		artifactMissing(w, err)
		return a, false
	}
	allowed := false
	if create {
		allowed = s.artifactPermsOf(s.DB, a).CanShare
	} else {
		allowed = s.artifactCanManageLinks(s.DB, a)
	}
	if !allowed {
		apiErr(w, http.StatusForbidden, "only the owner or a container manager can manage public links")
		return a, false
	}
	return a, true
}

func (s *Server) artifactShares(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := s.artifactShareAccess(w, id, false); !ok {
		return
	}
	q := `SELECT id,revision,created_by,created_at,revoked_at FROM artifact_shares WHERE artifact_id=?`
	if r.URL.Query().Get("include_revoked") != "true" {
		q += ` AND revoked_at IS NULL`
	}
	rows, err := s.DB.Query(q+` ORDER BY created_at DESC,id`, id)
	if err != nil {
		writeArtifactError(w, err)
		return
	}
	defer rows.Close()
	out := []artifactShareOut{}
	for rows.Next() {
		var sh artifactShareOut
		var rev sql.NullInt64
		var revoked sql.NullString
		if err := rows.Scan(&sh.ID, &rev, &sh.CreatedBy, &sh.CreatedAt, &revoked); err != nil {
			writeArtifactError(w, err)
			return
		}
		if rev.Valid {
			n := int(rev.Int64)
			sh.Revision = &n
		}
		sh.RevokedAt = revoked.String
		out = append(out, sh)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) artifactShareCreate(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var in struct {
		Revision *int `json:"revision"`
	}
	if !artifactDecode(w, r, &in) {
		return
	}
	a, ok := s.artifactShareAccess(w, id, true)
	if !ok {
		return
	}
	if a.DeletedAt != "" {
		apiErr(w, http.StatusConflict, "artifact is deleted")
		return
	}
	if !s.publicLinksEnabled(s.DB) {
		apiErr(w, http.StatusConflict, "public links are disabled by the administrator")
		return
	}
	var pinned any
	if in.Revision != nil {
		if *in.Revision < 1 || *in.Revision > a.Revision {
			apiErr(w, http.StatusBadRequest, "revision does not exist")
			return
		}
		pinned = *in.Revision
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		apiErr(w, http.StatusInternalServerError, "could not create link")
		return
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	share := artifactShareOut{ID: newID(), Revision: in.Revision, CreatedAt: stamp()}
	if s.principal != nil {
		share.CreatedBy = s.principal.ID
	}
	tx, err := s.DB.Begin()
	if err != nil {
		writeArtifactError(w, err)
		return
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`INSERT INTO artifact_shares(id,artifact_id,token_hash,revision,created_by,created_at) VALUES(?,?,?,?,?,?)`, share.ID, id, hashShareToken(token), pinned, share.CreatedBy, share.CreatedAt); err == nil {
		err = s.artifactAudit(tx, "artifact.share.create", "artifact", id, map[string]any{"artifact_id": id, "share_id": share.ID, "pinned_revision": in.Revision})
	}
	if err == nil {
		err = tx.Commit()
	}
	if err != nil {
		writeArtifactError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"id": share.ID, "url": "/s/" + token, "revision": share.Revision, "created_at": share.CreatedAt})
}

func (s *Server) artifactShareRevoke(w http.ResponseWriter, r *http.Request) {
	id, shareID := r.PathValue("id"), r.PathValue("share")
	if _, ok := s.artifactShareAccess(w, id, false); !ok {
		return
	}
	tx, err := s.DB.Begin()
	if err != nil {
		writeArtifactError(w, err)
		return
	}
	defer tx.Rollback()
	var revoked sql.NullString
	if err := tx.QueryRow(`SELECT revoked_at FROM artifact_shares WHERE id=? AND artifact_id=?`, shareID, id).Scan(&revoked); err != nil {
		artifactNotFoundJSON(w, err, "public link not found")
		return
	}
	if !revoked.Valid {
		res, err := tx.Exec(`UPDATE artifact_shares SET revoked_at=? WHERE id=? AND artifact_id=? AND revoked_at IS NULL`, stamp(), shareID, id)
		if err != nil {
			writeArtifactError(w, err)
			return
		}
		if n, e := res.RowsAffected(); e != nil || n != 1 {
			apiErr(w, http.StatusForbidden, "forbidden")
			return
		}
		if err := s.artifactAudit(tx, "artifact.share.revoke", "artifact", id, map[string]any{"artifact_id": id, "share_id": shareID}); err != nil {
			writeArtifactError(w, err)
			return
		}
	}
	if err := tx.Commit(); err != nil {
		writeArtifactError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func artifactNotFoundJSON(w http.ResponseWriter, err error, msg string) {
	if errors.Is(err, sql.ErrNoRows) {
		apiErr(w, http.StatusNotFound, msg)
		return
	}
	writeArtifactError(w, err)
}

// ---- administrator settings (corporate mode) ----

func (s *Server) adminSettingsGet(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.corporateActor(w, r, true); !ok {
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"public_artifact_links": s.publicLinksEnabled(s.DB)})
}

func (s *Server) adminSettingsPut(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.corporateActor(w, r, true)
	if !ok {
		return
	}
	var in struct {
		PublicArtifactLinks *bool `json:"public_artifact_links"`
	}
	if !artifactDecode(w, r, &in) {
		return
	}
	if in.PublicArtifactLinks == nil {
		apiErr(w, http.StatusBadRequest, "public_artifact_links is required")
		return
	}
	value := strconv.FormatBool(*in.PublicArtifactLinks)
	tx, err := s.DB.Begin()
	if err != nil {
		writeArtifactError(w, err)
		return
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`INSERT INTO app_settings(key,value) VALUES('public_artifact_links',?) ON CONFLICT (key) DO UPDATE SET value=EXCLUDED.value`, value); err == nil {
		err = corporateAudit(tx, actor, "settings.update", "settings", "public_artifact_links", map[string]any{"key": "public_artifact_links", "value": *in.PublicArtifactLinks})
	}
	if err == nil {
		err = tx.Commit()
	}
	if err != nil {
		writeArtifactError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"public_artifact_links": *in.PublicArtifactLinks})
}

// registerArtifactRoutes adds the artifact API, the isolated renderer and the
// public pages. The frame, runtime and /s/ routes are not behind the session
// check: the capability or token is the authorization.
func (s *Server) registerArtifactRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/artifacts", s.artifactList)
	mux.HandleFunc("POST /api/artifacts", s.artifactCreate)
	mux.HandleFunc("GET /api/artifacts/{id}", s.artifactGet)
	mux.HandleFunc("PUT /api/artifacts/{id}", s.artifactUpdate)
	mux.HandleFunc("DELETE /api/artifacts/{id}", s.artifactDelete)
	mux.HandleFunc("POST /api/artifacts/{id}/restore", s.artifactRestore)
	mux.HandleFunc("GET /api/artifacts/{id}/history", s.artifactHistory)
	mux.HandleFunc("GET /api/artifacts/{id}/files/{path...}", s.artifactFileGet)
	mux.HandleFunc("POST /api/artifacts/{id}/view", s.artifactView)
	mux.HandleFunc("GET /api/artifacts/{id}/shares", s.artifactShares)
	mux.HandleFunc("POST /api/artifacts/{id}/shares", s.artifactShareCreate)
	mux.HandleFunc("DELETE /api/artifacts/{id}/shares/{share}", s.artifactShareRevoke)
	s.registerArtifactAssetRoutes(mux)
}

func (s *Server) registerArtifactAssetRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /artifact-frame/{cap}/{path...}", s.artifactFrame)
	mux.HandleFunc("GET /artifact-runtime/{file...}", s.artifactRuntime)
	mux.HandleFunc("GET /s/{token}", s.publicArtifact)
}

// insertArtifactBlob records content-addressed object metadata once. The table
// has no SELECT policy (nobody can enumerate or probe hashes), and PostgreSQL
// requires SELECT visibility for ON CONFLICT, so a duplicate is detected by the
// unique violation inside a savepoint instead.
func insertArtifactBlob(tx *transaction, b artifactBlob, ts string) error {
	const insert = `INSERT INTO artifact_blobs(sha256,size,mime,stored_name,created_at) VALUES(?,?,?,?,?)`
	if !tx.pg {
		_, err := tx.Exec(insert+` ON CONFLICT (sha256) DO NOTHING`, b.File.SHA256, b.File.Size, b.File.MIME, artifactBlobKey(b.File.SHA256), ts)
		return err
	}
	if _, err := tx.Exec(`SAVEPOINT artifact_blob`); err != nil {
		return err
	}
	if _, err := tx.Exec(insert, b.File.SHA256, b.File.Size, b.File.MIME, artifactBlobKey(b.File.SHA256), ts); err != nil {
		var pg *pgconn.PgError
		if errors.As(err, &pg) && pg.Code == "23505" {
			_, rerr := tx.Exec(`ROLLBACK TO SAVEPOINT artifact_blob`)
			return rerr
		}
		return err
	}
	_, err := tx.Exec(`RELEASE SAVEPOINT artifact_blob`)
	return err
}

// artifactCanManageLinks mirrors artifact_can_manage_links: the owner while the
// owner can read the container, and managers for inherit artifacts. An archived
// container does not matter. Scoped MCP principals never manage links.
func (s *Server) artifactCanManageLinks(q queryer, a artifactMeta) bool {
	if s.principal == nil {
		return true
	}
	if s.isMCPScoped() {
		return false
	}
	role, _ := s.artifactRoleOf(q, a.SpaceID, a.ProjectID)
	return (a.OwnerID == s.principal.ID && role >= 1) || (a.Visibility == "inherit" && role >= 4)
}

// artifactLinkCreatorValid asks the database whether the creator of a link is
// still an active user entitled to share the artifact. Trusted mode has no users.
func (s *Server) artifactLinkCreatorValid(q queryer, artifactID, creator string) bool {
	if !s.DB.pg {
		return true
	}
	var ok bool
	return q.QueryRow(`SELECT artifact_link_valid(?,?)`, artifactID, creator).Scan(&ok) == nil && ok
}

// decodeStrictJSON is artifactDecode for a body that is already in memory.
func decodeStrictJSON(raw []byte, dst any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	var extra any
	if e := dec.Decode(&extra); e != io.EOF {
		return errors.New("unexpected data after the JSON value")
	}
	return nil
}

const (
	maxSearchTerms  = 10
	maxSearchLength = 100
)

// searchTerms lower-cases the query and keeps at most 10 words of at most 100
// characters; the rest is dropped.
func searchTerms(query string) []string {
	words := strings.Fields(strings.ToLower(query))
	if len(words) > maxSearchTerms {
		words = words[:maxSearchTerms]
	}
	for i, w := range words {
		if r := []rune(w); len(r) > maxSearchLength {
			words[i] = string(r[:maxSearchLength])
		}
	}
	return words
}
