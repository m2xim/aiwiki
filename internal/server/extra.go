package server

import (
	"database/sql"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

func (s *Server) restore(w http.ResponseWriter, r *http.Request, aid string) {
	var v struct {
		Revision     int    `json:"revision"`
		BaseRevision int    `json:"base_revision"`
		Author       string `json:"author"`
		Message      string `json:"message"`
	}
	if decode(r, &v) != nil || v.Revision < 1 || v.BaseRevision < 1 {
		apiErr(w, 400, "revision and base_revision required")
		return
	}
	if v.Author == "" {
		v.Author = "human"
	}
	tx, e := s.DB.Begin()
	if e != nil {
		apiErr(w, 500, "database error")
		return
	}
	defer tx.Rollback()
	cur, e := s.articleByID(tx, aid)
	if e != nil {
		apiErr(w, 404, "article not found")
		return
	}
	if cur.Revision != v.BaseRevision {
		apiErr(w, 409, "revision conflict")
		return
	}
	if cur.DeletedAt != "" {
		apiErr(w, 409, "article is deleted; use undelete first")
		return
	}
	var raw string
	if e = tx.QueryRow("SELECT snapshot FROM revisions WHERE article_id=? AND revision=?", aid, v.Revision).Scan(&raw); e != nil {
		apiErr(w, 404, "revision not found")
		return
	}
	var a Article
	if json.Unmarshal([]byte(raw), &a) != nil {
		apiErr(w, 500, "invalid revision")
		return
	}
	a.Revision = cur.Revision + 1
	a.CreatedAt = cur.CreatedAt
	a.UpdatedAt = stamp()
	a.Author = v.Author
	// A content restore never changes lifecycle state. In particular, a historic
	// deleted snapshot cannot be used to bypass the explicit undelete endpoint.
	a.DeletedAt = cur.DeletedAt
	if e = requiredArticle(a); e != nil {
		apiErr(w, 500, "invalid revision")
		return
	}
	if e = s.parentOK(tx, a, aid); e != nil {
		code := 400
		if e.Error() == "parent article is deleted" {
			code = 409
		}
		apiErr(w, code, e.Error())
		return
	}

	_, e = tx.Exec("UPDATE articles SET space_id=?,parent_id=?,title=?,summary=?,body=?,kind=?,status=?,pinned=?,tags=?,properties=?,revision=?,updated_at=?,author=? WHERE id=? AND revision=?", a.SpaceID, a.ParentID, a.Title, a.Summary, a.Body, a.Kind, a.Status, boolInt(a.Pinned), encode(a.Tags), encode(a.Properties), a.Revision, a.UpdatedAt, a.Author, aid, v.BaseRevision)
	if e != nil {
		apiErr(w, 500, "database error")
		return
	}
	if _, e = tx.Exec("INSERT INTO revisions(article_id,revision,author,message,created_at,snapshot) VALUES(?,?,?,?,?,?)", aid, a.Revision, a.Author, v.Message, a.UpdatedAt, encode(a)); e != nil {
		apiErr(w, 500, "database error")
		return
	}
	if e = indexArticle(tx, a); e != nil {
		apiErr(w, 500, "database error")
		return
	}
	if cur.SpaceID != a.SpaceID {
		if e = moveDescendants(tx, a); e != nil {
			apiErr(w, 500, "database error")
			return
		}
	}
	if e = tx.Commit(); e != nil {
		apiErr(w, 500, "database error")
		return
	}
	writeJSON(w, 200, a)
}
func (s *Server) articleComments(w http.ResponseWriter, aid string) {
	if _, e := s.articleByID(s.DB, aid); e != nil {
		apiErr(w, 404, "article not found")
		return
	}
	rows, e := s.DB.Query("SELECT id,article_id,anchor,body,author,created_at,resolved FROM comments WHERE article_id=? ORDER BY created_at", aid)
	if e != nil {
		apiErr(w, 500, "database error")
		return
	}
	defer rows.Close()
	out := []Comment{}
	for rows.Next() {
		var c Comment
		var v int
		if rows.Scan(&c.ID, &c.ArticleID, &c.Anchor, &c.Body, &c.Author, &c.CreatedAt, &v) == nil {
			c.Resolved = v != 0
			out = append(out, c)
		}
	}
	writeJSON(w, 200, out)
}
func (s *Server) addComment(w http.ResponseWriter, r *http.Request, aid string) {
	var v struct {
		Anchor string `json:"anchor"`
		Body   string `json:"body"`
		Author string `json:"author"`
	}
	if decode(r, &v) != nil || strings.TrimSpace(v.Body) == "" {
		apiErr(w, 400, "body required")
		return
	}
	if v.Author == "" {
		v.Author = "human"
	}
	c := Comment{ID: newID(), ArticleID: aid, Anchor: v.Anchor, Body: v.Body, Author: v.Author, CreatedAt: stamp()}
	_, e := s.DB.Exec("INSERT INTO comments(id,article_id,anchor,body,author,created_at,resolved) VALUES(?,?,?,?,?,?,0)", c.ID, c.ArticleID, c.Anchor, c.Body, c.Author, c.CreatedAt)
	if e != nil {
		apiErr(w, 404, "article not found")
		return
	}
	writeJSON(w, 201, c)
}
func (s *Server) commentRoute(w http.ResponseWriter, r *http.Request) {
	if r.Method != "PATCH" {
		apiErr(w, 405, "method not allowed")
		return
	}
	var v struct {
		Body     *string `json:"body"`
		Anchor   *string `json:"anchor"`
		Resolved *bool   `json:"resolved"`
	}
	cid := strings.TrimPrefix(r.URL.Path, "/api/comments/")
	if decode(r, &v) != nil || (v.Body == nil && v.Anchor == nil && v.Resolved == nil) {
		apiErr(w, 400, "invalid input")
		return
	}
	if v.Body != nil && (strings.TrimSpace(*v.Body) == "" || len(*v.Body) > 2<<20) {
		apiErr(w, 400, "body must be non-empty and at most 2 MB")
		return
	}
	tx, e := s.DB.Begin()
	if e != nil {
		apiErr(w, 500, "database error")
		return
	}
	defer tx.Rollback()
	var c Comment
	var rv int
	e = tx.QueryRow("SELECT id,article_id,anchor,body,author,created_at,resolved FROM comments WHERE id=?", cid).Scan(&c.ID, &c.ArticleID, &c.Anchor, &c.Body, &c.Author, &c.CreatedAt, &rv)
	if e == sql.ErrNoRows {
		apiErr(w, 404, "comment not found")
		return
	}
	if e != nil {
		apiErr(w, 500, "database error")
		return
	}
	c.Resolved = rv != 0
	sets := []string{}
	args := []any{}
	if v.Body != nil {
		sets = append(sets, "body=?")
		args = append(args, *v.Body)
		c.Body = *v.Body
	}
	if v.Anchor != nil {
		sets = append(sets, "anchor=?")
		args = append(args, *v.Anchor)
		c.Anchor = *v.Anchor
	}
	if v.Resolved != nil {
		sets = append(sets, "resolved=?")
		args = append(args, boolInt(*v.Resolved))
		c.Resolved = *v.Resolved
	}
	args = append(args, cid)
	if _, e = tx.Exec("UPDATE comments SET "+strings.Join(sets, ",")+" WHERE id=?", args...); e != nil {
		apiErr(w, 500, "database error")
		return
	}
	if e = tx.Commit(); e != nil {
		apiErr(w, 500, "database error")
		return
	}
	writeJSON(w, 200, c)
}
func (s *Server) listAttachments(w http.ResponseWriter, aid string) {
	if _, e := s.articleByID(s.DB, aid); e != nil {
		apiErr(w, 404, "article not found")
		return
	}
	rows, e := s.DB.Query("SELECT id,article_id,name,mime,size,created_at FROM attachments WHERE article_id=? AND deleted_at IS NULL ORDER BY created_at", aid)
	if e != nil {
		apiErr(w, 500, "database error")
		return
	}
	defer rows.Close()
	out := []Attachment{}
	for rows.Next() {
		var a Attachment
		if rows.Scan(&a.ID, &a.ArticleID, &a.Name, &a.MIME, &a.Size, &a.CreatedAt) == nil {
			a.URL = "/files/" + a.ID
			out = append(out, a)
		}
	}
	writeJSON(w, 200, out)
}
func (s *Server) deleteAttachment(w http.ResponseWriter, aid, fid string) {
	if _, err := s.articleByID(s.DB, aid); err != nil {
		apiErr(w, 404, "article not found")
		return
	}
	var exists int
	if err := s.DB.QueryRow("SELECT 1 FROM attachments WHERE id=? AND article_id=?", fid, aid).Scan(&exists); err != nil {
		apiErr(w, 404, "attachment not found")
		return
	}
	if _, err := s.DB.Exec("UPDATE attachments SET deleted_at=COALESCE(deleted_at, ?) WHERE id=? AND article_id=?", stamp(), fid, aid); err != nil {
		apiErr(w, 500, "database error")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
func (s *Server) addAttachment(w http.ResponseWriter, r *http.Request, aid string) {
	if _, e := s.articleByID(s.DB, aid); e != nil {
		apiErr(w, 404, "article not found")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 25<<20)
	if e := r.ParseMultipartForm(25 << 20); e != nil {
		apiErr(w, 400, "upload max 25 MB")
		return
	}
	f, h, e := r.FormFile("file")
	if e != nil {
		apiErr(w, 400, "file field required")
		return
	}
	defer f.Close()
	stored := newID() + filepath.Ext(filepath.Base(h.Filename))
	path := filepath.Join(s.DataDir, "files", stored)
	out, e := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		apiErr(w, 500, "could not store file")
		return
	}
	size, e := io.Copy(out, io.LimitReader(f, (25<<20)+1))
	ce := out.Close()
	if e != nil || ce != nil || size > 25<<20 {
		_ = os.Remove(path)
		apiErr(w, 413, "file exceeds 25 MB")
		return
	}
	fid := strings.TrimSuffix(stored, filepath.Ext(stored))
	buf := make([]byte, 512)
	_, _ = f.Seek(0, io.SeekStart)
	n, _ := f.Read(buf)
	typ := http.DetectContentType(buf[:n])
	a := Attachment{ID: fid, ArticleID: aid, Name: filepath.Base(h.Filename), MIME: typ, Size: size, URL: "/files/" + fid, CreatedAt: stamp()}
	_, e = s.DB.Exec("INSERT INTO attachments(id,article_id,name,mime,size,stored_name,created_at) VALUES(?,?,?,?,?,?,?)", a.ID, aid, a.Name, a.MIME, a.Size, stored, a.CreatedAt)
	if e != nil {
		_ = os.Remove(path)
		apiErr(w, 500, "database error")
		return
	}
	writeJSON(w, 201, a)
}
func (s *Server) fileRoute(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		apiErr(w, 405, "method not allowed")
		return
	}
	fid := strings.TrimPrefix(r.URL.Path, "/files/")
	var name, typ, stored string
	if e := s.DB.QueryRow("SELECT name,mime,stored_name FROM attachments WHERE id=? AND deleted_at IS NULL", fid).Scan(&name, &typ, &stored); e != nil {
		apiErr(w, 404, "file not found")
		return
	}
	f, e := os.Open(filepath.Join(s.DataDir, "files", filepath.Base(stored)))
	if e != nil {
		apiErr(w, 404, "file not found")
		return
	}
	defer f.Close()
	safe := typ == "image/png" || typ == "image/jpeg" || typ == "image/gif" || typ == "image/webp"
	if !safe {
		w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": name}))
		w.Header().Set("X-Content-Type-Options", "nosniff")
	} else {
		w.Header().Set("Content-Disposition", "inline")
	}
	if _, _, e = mime.ParseMediaType(typ); e != nil {
		typ = "application/octet-stream"
	}
	w.Header().Set("Content-Type", typ)
	_, _ = io.Copy(w, f)
}

// linkPattern recognises internal article links in Markdown: relative or
// absolute (http/https) `](...?article=ID#anchor)` and autolinks `<...?article=ID>`.
// Group 1 is the article id, group 2 the optional anchor.
var linkPattern = regexp.MustCompile(`(?:\]\(\s*|<)(?:https?://[^)>\s/]+)?/\?article=([A-Za-z0-9_-]+)(?:#([^)>\s]*))?`)

type linkRef struct{ ID, Anchor string }

// extractLinks returns the distinct article ids linked from body in order of
// first appearance; the anchor is the first non-empty one seen for that id.
func extractLinks(body string) []linkRef {
	out := []linkRef{}
	seen := map[string]int{}
	for _, m := range linkPattern.FindAllStringSubmatch(body, -1) {
		if i, ok := seen[m[1]]; ok {
			if out[i].Anchor == "" {
				out[i].Anchor = m[2]
			}
			continue
		}
		seen[m[1]] = len(out)
		out = append(out, linkRef{m[1], m[2]})
	}
	return out
}

type linkedArticle struct {
	Article Article
	Anchor  string
}

// articleLinks resolves outgoing (live, distinct, non-self) and incoming links.
func (s *Server) articleLinks(a Article) ([]linkedArticle, []Article, error) {
	out := []linkedArticle{}
	for _, l := range extractLinks(a.Body) {
		if l.ID == a.ID {
			continue
		}
		if v, er := s.articleByID(s.DB, l.ID); er == nil && v.DeletedAt == "" {
			v.Body = ""
			out = append(out, linkedArticle{v, l.Anchor})
		}
	}
	rows, e := s.DB.Query("SELECT id,body FROM articles WHERE body LIKE ? AND id<>? AND deleted_at IS NULL ORDER BY updated_at DESC,id", "%/?article="+a.ID+"%", a.ID)
	if e != nil {
		return nil, nil, e
	}
	incomingIDs := []string{}
	for rows.Next() {
		var x, body string
		if rows.Scan(&x, &body) == nil {
			for _, l := range extractLinks(body) {
				if l.ID == a.ID {
					incomingIDs = append(incomingIDs, x)
					break
				}
			}
		}
	}
	rowErr := rows.Err()
	_ = rows.Close()
	if rowErr != nil {
		return nil, nil, rowErr
	}
	incoming := []Article{}
	for _, x := range incomingIDs {
		if v, er := s.articleByID(s.DB, x); er == nil {
			v.Body = ""
			incoming = append(incoming, v)
		}
	}
	return out, incoming, nil
}

func (s *Server) links(w http.ResponseWriter, aid string) {
	a, e := s.articleByID(s.DB, aid)
	if e != nil {
		apiErr(w, 404, "article not found")
		return
	}
	out, incoming, e := s.articleLinks(a)
	if e != nil {
		apiErr(w, 500, "database error")
		return
	}
	plain := make([]Article, 0, len(out))
	for _, l := range out {
		plain = append(plain, l.Article)
	}
	writeJSON(w, 200, map[string][]Article{"outgoing": plain, "incoming": incoming})
}
