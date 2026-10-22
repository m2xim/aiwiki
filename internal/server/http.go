package server

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
)

func writeJSON(w http.ResponseWriter, c int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(c)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}
func apiErr(w http.ResponseWriter, c int, m string) { writeJSON(w, c, map[string]string{"error": m}) }
func decode(r *http.Request, v any) error {
	return json.NewDecoder(http.MaxBytesReader(nil, r.Body, 3<<20)).Decode(v)
}
func newID() string { return randomSuffix() }
func (s *Server) tags(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		apiErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	space := r.URL.Query().Get("space_id")
	if space == "" {
		apiErr(w, http.StatusBadRequest, "space_id is required")
		return
	}
	rows, err := s.DB.Query(s.DB.distinctTagsSQL(), space)
	if err != nil {
		apiErr(w, http.StatusInternalServerError, "database error")
		return
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var tag string
		if err := rows.Scan(&tag); err != nil {
			apiErr(w, 500, "database error")
			return
		}
		out = append(out, tag)
	}
	if err := rows.Err(); err != nil {
		apiErr(w, 500, "database error")
		return
	}
	writeJSON(w, http.StatusOK, out)
}
func (s *Server) articles(w http.ResponseWriter, r *http.Request) {
	if r.Method == "GET" {
		s.listArticles(w, r)
	} else if r.Method == "POST" {
		s.createArticle(w, r)
	} else {
		apiErr(w, 405, "method not allowed")
	}
}
func (s *Server) listArticles(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	p := SearchParams{ProjectID: q.Get("project_id"), Q: q.Get("q"), SpaceID: q.Get("space_id"), Kind: q.Get("kind"), Pinned: q.Get("pinned") == "true", Tags: q["tag"], Properties: q["property"], Limit: 200}
	if n, e := strconv.Atoi(q.Get("limit")); e == nil && n > 0 && n < 200 {
		p.Limit = n
	}
	if n, e := strconv.Atoi(q.Get("offset")); e == nil && n > 0 {
		p.Offset = n
	}
	res, code, e := s.searchArticles(r.Context(), p)
	if e != nil {
		apiErr(w, code, e.Error())
		return
	}
	out := make([]Article, 0, len(res.Hits))
	for _, h := range res.Hits {
		out = append(out, h.Article)
	}
	writeJSON(w, 200, out)
}

type payload struct {
	ProjectID    string            `json:"project_id"`
	SpaceID      string            `json:"space_id"`
	ParentID     string            `json:"parent_id"`
	Title        string            `json:"title"`
	Summary      string            `json:"summary"`
	Body         string            `json:"body"`
	Kind         string            `json:"kind"`
	Status       string            `json:"status"`
	Pinned       bool              `json:"pinned"`
	Tags         []string          `json:"tags"`
	Properties   map[string]string `json:"properties"`
	BaseRevision int               `json:"base_revision"`
	Author       string            `json:"author"`
	Message      string            `json:"message"`
}

// articleFields are the editable keys a client may send; on update only the
// keys present in the request replace current values.
var articleFields = []string{"project_id", "space_id", "parent_id", "title", "summary", "body", "kind", "status", "pinned", "tags", "properties"}

func parseInput(r *http.Request) (Article, payload, map[string]json.RawMessage, error) {
	var p payload
	present := map[string]json.RawMessage{}
	raw, e := io.ReadAll(http.MaxBytesReader(nil, r.Body, 3<<20))
	if e == nil {
		e = json.Unmarshal(raw, &p)
	}
	if e == nil {
		// encoding/json matches struct keys case-insensitively, so present
		// is keyed by the lower-cased name to agree with what was decoded.
		var keys map[string]json.RawMessage
		if e = json.Unmarshal(raw, &keys); e == nil {
			for k, v := range keys {
				present[strings.ToLower(k)] = v
			}
		}
	}
	a, p := articleFromPayload(p)
	return a, p, present, e
}

func articleFromPayload(p payload) (Article, payload) {
	a := Article{ProjectID: p.ProjectID, SpaceID: p.SpaceID, ParentID: p.ParentID, Title: strings.TrimSpace(p.Title), Summary: p.Summary, Body: p.Body, Kind: p.Kind, Status: p.Status, Pinned: p.Pinned, Tags: p.Tags, Properties: p.Properties}
	if a.Kind == "" {
		a.Kind = "article"
	}
	if a.Status == "" {
		a.Status = "draft"
	}
	if a.Tags == nil {
		a.Tags = []string{}
	}
	if a.Properties == nil {
		a.Properties = map[string]string{}
	}
	if p.Author == "" {
		p.Author = "human"
	}
	return a, p
}

// mergePresent overlays the fields present in an update request onto the current article.
func mergePresent(old, in Article, present map[string]json.RawMessage) Article {
	out := old
	for _, k := range articleFields {
		if _, ok := present[k]; !ok {
			continue
		}
		switch k {
		case "project_id":
			out.ProjectID = in.ProjectID
		case "space_id":
			out.SpaceID = in.SpaceID
		case "parent_id":
			out.ParentID = in.ParentID
		case "title":
			out.Title = in.Title
		case "summary":
			out.Summary = in.Summary
		case "body":
			out.Body = in.Body
		case "kind":
			out.Kind = in.Kind
		case "status":
			out.Status = in.Status
		case "pinned":
			out.Pinned = in.Pinned
		case "tags":
			out.Tags = in.Tags
		case "properties":
			out.Properties = in.Properties
		}
	}
	return out
}
func (s *Server) parentOK(q interface{ QueryRow(string, ...any) *sql.Row }, a Article, self string) error {
	if a.ParentID == "" {
		return nil
	}
	var p Article
	var e error
	if _, isTx := q.(*transaction); isTx && s.DB.pg {
		// A share lock conflicts with the FOR NO KEY UPDATE a move takes on its
		// subtree, so a child cannot be attached to a parent that is being moved.
		p, e = scanArticle(q.QueryRow("SELECT "+articleColumns+" FROM articles WHERE id=? FOR SHARE", a.ParentID))
	} else {
		p, e = s.articleByID(q, a.ParentID)
	}
	if e != nil {
		return fmt.Errorf("parent not found")
	}
	if p.DeletedAt != "" {
		return fmt.Errorf("parent article is deleted")
	}
	if p.SpaceID != a.SpaceID || p.ProjectID != a.ProjectID {
		return fmt.Errorf("parent must be in same space")
	}
	for p.ID != "" {
		if p.ID == self {
			return fmt.Errorf("parent cycle")
		}
		if p.ParentID == "" {
			break
		}
		// Ancestors are read without locks (the cycle check only needs a snapshot).
		p, e = scanArticle(q.QueryRow("SELECT "+articleColumns+" FROM articles WHERE id=?", p.ParentID))
		if e != nil {
			break
		}
		if p.DeletedAt != "" {
			return fmt.Errorf("parent article is deleted")
		}
	}
	return nil
}
func (s *Server) createArticle(w http.ResponseWriter, r *http.Request) {
	a, p, _, e := parseInput(r)
	if e != nil {
		apiErr(w, 400, "invalid input")
		return
	}
	if e = requiredArticle(a); e != nil {
		apiErr(w, 400, e.Error())
		return
	}
	tx, e := s.DB.Begin()
	if e != nil {
		apiErr(w, 500, "database error")
		return
	}
	defer tx.Rollback()
	if s.DB.pg && a.ProjectID != "" {
		// Serializes with a project move: it holds the project row FOR NO KEY UPDATE.
		// Delegated MCP sessions cannot lock the row (no UPDATE policy on projects for
		// them) and only check that it exists.
		lockClause := " FOR SHARE"
		if s.principal != nil && (s.principal.MCPMode != "" || s.principal.MCPProjects != nil) {
			lockClause = ""
		}
		var one int
		if tx.QueryRow(`SELECT 1 FROM projects WHERE id=? AND space_id=?`+lockClause, a.ProjectID, a.SpaceID).Scan(&one) != nil {
			apiErr(w, 400, "could not create article")
			return
		}
	}
	if e = s.parentOK(tx, a, ""); e != nil {
		code := 400
		if e.Error() == "parent article is deleted" {
			code = 409
		}
		apiErr(w, code, e.Error())
		return
	}
	a.ID = newID()
	a.Revision = 1
	a.Author = s.changeAuthor(p.Author)
	a.CreatedAt = stamp()
	a.UpdatedAt = a.CreatedAt
	if e = insertArticle(tx, a, p.Message); e != nil {
		apiErr(w, 400, "could not create article")
		return
	}
	if e = tx.Commit(); e != nil {
		apiErr(w, 500, "database error")
		return
	}
	writeJSON(w, 201, a)
}
func (s *Server) articleRoute(w http.ResponseWriter, r *http.Request) {
	p := strings.Split(strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/articles/"), "/"), "/")
	if len(p) == 0 || p[0] == "" {
		apiErr(w, 404, "not found")
		return
	}
	aid := p[0]
	if len(p) == 1 {
		if r.Method == http.MethodDelete {
			s.deleteArticle(w, r, aid)
			return
		}
		if r.Method == "PUT" {
			s.updateArticle(w, r, aid)
			return
		}
		if r.Method != http.MethodGet {
			apiErr(w, 405, "method not allowed")
			return
		}
		a, e := s.articleByID(s.DB, aid)
		if errors.Is(e, sql.ErrNoRows) {
			apiErr(w, 404, "article not found")
		} else if e != nil {
			apiErr(w, 500, "database error")
		} else {
			writeJSON(w, 200, a)
		}
		return
	}
	if len(p) == 3 && p[1] == "attachments" {
		if r.Method != http.MethodDelete {
			apiErr(w, 405, "method not allowed")
			return
		}
		s.deleteAttachment(w, aid, p[2])
		return
	}
	if len(p) != 2 {
		apiErr(w, 404, "not found")
		return
	}
	switch p[1] {
	case "history":
		if r.Method != http.MethodGet {
			apiErr(w, 405, "method not allowed")
			return
		}
		if _, err := s.articleByID(s.DB, aid); err != nil {
			apiErr(w, 404, "article not found")
			return
		}
		s.history(w, aid)
	case "restore":
		if r.Method != http.MethodPost {
			apiErr(w, 405, "method not allowed")
			return
		}
		s.restore(w, r, aid)
	case "undelete":
		if r.Method != http.MethodPost {
			apiErr(w, 405, "method not allowed")
			return
		}
		s.undeleteArticle(w, r, aid)
	case "links":
		if r.Method != http.MethodGet {
			apiErr(w, 405, "method not allowed")
			return
		}
		s.links(w, aid)
	case "comments":
		if r.Method == http.MethodPost {
			s.addComment(w, r, aid)
		} else if r.Method == http.MethodGet {
			s.articleComments(w, aid)
		} else {
			apiErr(w, 405, "method not allowed")
		}
	case "attachments":
		if r.Method != http.MethodGet && r.Method != http.MethodPost {
			apiErr(w, 405, "method not allowed")
			return
		}
		if r.Method == http.MethodPost {
			s.addAttachment(w, r, aid)
		} else if r.Method == http.MethodGet {
			s.listAttachments(w, aid)
		} else {
			apiErr(w, 405, "method not allowed")
		}
	case "markdown":
		if r.Method != http.MethodGet {
			apiErr(w, 405, "method not allowed")
			return
		}
		a, e := s.articleByID(s.DB, aid)
		if e != nil {
			apiErr(w, 404, "article not found")
		} else {
			w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
			_, _ = io.WriteString(w, a.Body)
		}
	default:
		apiErr(w, 404, "not found")
	}
}
func (s *Server) updateArticle(w http.ResponseWriter, r *http.Request, aid string) {
	in, p, present, e := parseInput(r)
	if e != nil || p.BaseRevision < 1 {
		apiErr(w, 400, "base_revision required")
		return
	}
	a, code, e := s.commitUpdate(aid, p.BaseRevision, p.Author, p.Message, func(old Article) (Article, error) {
		return mergePresent(old, in, present), nil
	})
	if e != nil {
		apiErr(w, code, e.Error())
		return
	}
	writeJSON(w, 200, a)
}

// commitUpdate stores a new revision of aid produced by mutate from the current
// article. It checks base revision, deletion, required fields and parent rules.
// On failure it returns the HTTP status and a client-facing error.
func (s *Server) commitUpdate(aid string, base int, author, message string, mutate func(old Article) (Article, error)) (Article, int, error) {
	if author == "" {
		author = "human"
	}
	tx, e := s.DB.Begin()
	if e != nil {
		return Article{}, 500, errors.New("database error")
	}
	defer tx.Rollback()
	old, e := s.articleByID(tx, aid)
	if e != nil {
		return Article{}, 404, errors.New("article not found")
	}
	if old.Revision != base {
		return Article{}, 409, fmt.Errorf("revision conflict: current revision is %d", old.Revision)
	}
	if old.DeletedAt != "" {
		return Article{}, 409, errors.New("article is deleted; use undelete first")
	}
	a, e := mutate(old)
	if e != nil {
		return Article{}, 400, e
	}
	// In corporate mode a parent change inside the same container is an ordinary
	// edit (editor+, enforced by RLS and the database trigger); a change of space
	// or project needs the access-impact preview.
	if s.principal != nil && (a.SpaceID != old.SpaceID || a.ProjectID != old.ProjectID) {
		return Article{}, 400, errors.New("moving an article to another space or project requires an access-impact preview: use POST /api/moves/preview and /api/moves/apply")
	}
	if e = requiredArticle(a); e != nil {
		return Article{}, 400, e
	}
	if e = s.parentOK(tx, a, aid); e != nil {
		code := 400
		if e.Error() == "parent article is deleted" {
			code = 409
		}
		return Article{}, code, e
	}
	a.ID = aid
	a.Revision = old.Revision + 1
	a.CreatedAt = old.CreatedAt
	a.UpdatedAt = stamp()
	a.Author = s.changeAuthor(author)
	a.DeletedAt = ""
	_, e = tx.Exec("UPDATE articles SET space_id=?,parent_id=?,project_id=?,title=?,summary=?,body=?,kind=?,status=?,pinned=?,tags=?,properties=?,revision=?,updated_at=?,author=? WHERE id=? AND revision=?", a.SpaceID, a.ParentID, a.ProjectID, a.Title, a.Summary, a.Body, a.Kind, a.Status, boolInt(a.Pinned), encode(a.Tags), encode(a.Properties), a.Revision, a.UpdatedAt, a.Author, aid, base)
	if e != nil {
		return Article{}, 400, errors.New("could not update article")
	}
	if _, e = tx.Exec("INSERT INTO revisions(article_id,revision,author,message,created_at,snapshot) VALUES(?,?,?,?,?,?)", aid, a.Revision, a.Author, message, a.UpdatedAt, encode(a)); e != nil {
		return Article{}, 500, errors.New("database error")
	}
	if e = indexArticle(tx, a); e != nil {
		return Article{}, 500, errors.New("database error")
	}
	if old.SpaceID != a.SpaceID {
		if e = moveDescendants(tx, a); e != nil {
			return Article{}, 500, errors.New("database error")
		}
	}
	if e = tx.Commit(); e != nil {
		return Article{}, 500, errors.New("database error")
	}
	return a, 200, nil
}

func (s *Server) deleteArticle(w http.ResponseWriter, r *http.Request, aid string) {
	var v struct {
		BaseRevision int    `json:"base_revision"`
		Author       string `json:"author"`
	}
	if decode(r, &v) != nil || v.BaseRevision < 1 {
		apiErr(w, 400, "base_revision required")
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
	a, e := s.articleByID(tx, aid)
	if e != nil {
		apiErr(w, 404, "article not found")
		return
	}
	if a.Revision != v.BaseRevision {
		apiErr(w, 409, "revision conflict")
		return
	}
	if a.DeletedAt != "" {
		apiErr(w, 409, "article is already deleted")
		return
	}
	var children int
	if e = tx.QueryRow("SELECT count(*) FROM articles WHERE parent_id=? AND deleted_at IS NULL", aid).Scan(&children); e != nil {
		apiErr(w, 500, "database error")
		return
	}
	if children > 0 {
		apiErr(w, 409, "Нельзя удалить статью: сначала перенесите или удалите дочерние статьи")
		return
	}
	a.Revision++
	a.UpdatedAt = stamp()
	a.DeletedAt = a.UpdatedAt
	a.Author = s.changeAuthor(v.Author)
	if _, e = tx.Exec("UPDATE articles SET deleted_at=?,revision=?,updated_at=?,author=? WHERE id=? AND revision=? AND deleted_at IS NULL", a.DeletedAt, a.Revision, a.UpdatedAt, a.Author, aid, v.BaseRevision); e != nil {
		apiErr(w, 500, "database error")
		return
	}
	if _, e = tx.Exec("INSERT INTO revisions(article_id,revision,author,message,created_at,snapshot) VALUES(?,?,?,?,?,?)", aid, a.Revision, a.Author, "Удаление статьи", a.UpdatedAt, encode(a)); e != nil {
		apiErr(w, 500, "database error")
		return
	}
	if e = tx.Commit(); e != nil {
		apiErr(w, 500, "database error")
		return
	}
	writeJSON(w, 200, a)
}

func (s *Server) undeleteArticle(w http.ResponseWriter, r *http.Request, aid string) {
	var v struct {
		BaseRevision int    `json:"base_revision"`
		Author       string `json:"author"`
	}
	if decode(r, &v) != nil || v.BaseRevision < 1 {
		apiErr(w, 400, "base_revision required")
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
	a, e := s.articleByID(tx, aid)
	if e != nil {
		apiErr(w, 404, "article not found")
		return
	}
	if a.Revision != v.BaseRevision {
		apiErr(w, 409, "revision conflict")
		return
	}
	if a.DeletedAt == "" {
		apiErr(w, 409, "article is not deleted")
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
	a.Revision++
	a.UpdatedAt = stamp()
	a.DeletedAt = ""
	a.Author = s.changeAuthor(v.Author)
	if _, e = tx.Exec("UPDATE articles SET deleted_at=NULL,revision=?,updated_at=?,author=? WHERE id=? AND revision=? AND deleted_at IS NOT NULL", a.Revision, a.UpdatedAt, a.Author, aid, v.BaseRevision); e != nil {
		apiErr(w, 500, "database error")
		return
	}
	if _, e = tx.Exec("INSERT INTO revisions(article_id,revision,author,message,created_at,snapshot) VALUES(?,?,?,?,?,?)", aid, a.Revision, a.Author, "Восстановление статьи", a.UpdatedAt, encode(a)); e != nil {
		apiErr(w, 500, "database error")
		return
	}
	if e = indexArticle(tx, a); e != nil {
		apiErr(w, 500, "database error")
		return
	}
	if e = tx.Commit(); e != nil {
		apiErr(w, 500, "database error")
		return
	}
	writeJSON(w, 200, a)
}

func (s *Server) history(w http.ResponseWriter, aid string) {
	rows, e := s.DB.Query("SELECT id,article_id,revision,author,message,created_at,snapshot FROM revisions WHERE article_id=? ORDER BY revision DESC", aid)
	if e != nil {
		apiErr(w, 500, "database error")
		return
	}
	defer rows.Close()
	out := []Revision{}
	for rows.Next() {
		var x Revision
		var raw string
		if rows.Scan(&x.ID, &x.ArticleID, &x.Revision, &x.Author, &x.Message, &x.CreatedAt, &raw) == nil {
			_ = json.Unmarshal([]byte(raw), &x.Snapshot)
			out = append(out, x)
		}
	}
	writeJSON(w, 200, out)
}

var _ = json.Valid
