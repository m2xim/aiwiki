package server

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
	"unicode"
)

func (s *Server) bootstrap(w http.ResponseWriter, r *http.Request) {
	space := r.URL.Query().Get("space_id")
	spaces := []Space{}
	rows, e := s.DB.Query("SELECT id,name,description,archived_at FROM spaces WHERE archived_at IS NULL ORDER BY name")
	if e != nil {
		apiErr(w, 500, "database error")
		return
	}
	for rows.Next() {
		if x, err := scanSpace(rows); err == nil {
			spaces = append(spaces, x)
		}
	}
	rows.Close()
	cond := "deleted_at IS NULL AND pinned=1 AND status<>'archived' AND space_id IN (SELECT id FROM spaces WHERE archived_at IS NULL)"
	if s.DB.pg {
		cond = "deleted_at IS NULL AND pinned=1 AND status<>'archived' AND corporate_container_writable(space_id,project_id)"
	}
	args := []any{}
	if space != "" {
		cond += " AND (space_id='general' OR space_id=?)"
		args = append(args, space)
	}
	instructions := s.selectArticles("SELECT "+articleColumns+" FROM articles WHERE "+cond+" ORDER BY title", args...)
	recentQuery := "SELECT " + articleColumns + " FROM articles WHERE deleted_at IS NULL AND status<>'archived' AND space_id IN (SELECT id FROM spaces WHERE archived_at IS NULL)"
	if s.DB.pg {
		recentQuery = "SELECT " + articleColumns + " FROM articles WHERE deleted_at IS NULL AND status<>'archived' AND corporate_container_writable(space_id,project_id)"
	}
	recentArgs := []any{}
	if space != "" {
		recentQuery += " AND (space_id='general' OR space_id=?)"
		recentArgs = append(recentArgs, space)
	}
	recent := s.selectArticles(recentQuery+" ORDER BY updated_at DESC LIMIT 12", recentArgs...)
	for i := range recent {
		recent[i].Body = ""
	}
	writeJSON(w, 200, map[string]any{"instructions": instructions, "spaces": spaces, "recent": recent, "guide": "Search for relevant context, read full articles, and use base_revision on updates."})
}
func (s *Server) selectArticles(q string, args ...any) []Article {
	return selectArticlesFrom(s.DB, q, args...)
}
func selectArticlesFrom(db interface {
	Query(string, ...any) (*sql.Rows, error)
}, q string, args ...any) []Article {
	rows, e := db.Query(q, args...)
	if e != nil {
		return []Article{}
	}
	defer rows.Close()
	out := []Article{}
	for rows.Next() {
		if a, e := scanArticle(rows); e == nil {
			out = append(out, a)
		}
	}
	return out
}
func (s *Server) export(w http.ResponseWriter, r *http.Request) {
	if s.principal != nil {
		apiErr(w, 403, "bulk export is unavailable in corporate mode")
		return
	}
	tx, e := s.DB.Begin()
	if e != nil {
		apiErr(w, 500, "database error")
		return
	}
	defer tx.Rollback()
	spaces := []Space{}
	rows, e := tx.Query("SELECT id,name,description,archived_at FROM spaces")
	if e != nil {
		apiErr(w, 500, "database error")
		return
	}
	for rows.Next() {
		if x, err := scanSpace(rows); err == nil {
			spaces = append(spaces, x)
		}
	}
	rows.Close()
	articles := selectArticlesFrom(tx, "SELECT "+articleColumns+" FROM articles")
	revs := []Revision{}
	rr, e := tx.Query("SELECT id,article_id,revision,author,message,created_at,snapshot FROM revisions")
	if e != nil {
		apiErr(w, 500, "database error")
		return
	}
	for rr.Next() {
		var x Revision
		var raw string
		if rr.Scan(&x.ID, &x.ArticleID, &x.Revision, &x.Author, &x.Message, &x.CreatedAt, &raw) == nil {
			_ = json.Unmarshal([]byte(raw), &x.Snapshot)
			revs = append(revs, x)
		}
	}
	rr.Close()
	comments := []Comment{}
	cr, e := tx.Query("SELECT id,article_id,anchor,body,author,created_at,resolved FROM comments")
	if e != nil {
		apiErr(w, 500, "database error")
		return
	}
	for cr.Next() {
		var c Comment
		var v int
		if cr.Scan(&c.ID, &c.ArticleID, &c.Anchor, &c.Body, &c.Author, &c.CreatedAt, &v) == nil {
			c.Resolved = v != 0
			comments = append(comments, c)
		}
	}
	cr.Close()
	atts := []Attachment{}
	ar, e := tx.Query("SELECT id,article_id,name,mime,size,created_at,deleted_at FROM attachments")
	if e != nil {
		apiErr(w, 500, "database error")
		return
	}
	for ar.Next() {
		var a Attachment
		var deleted sql.NullString
		if ar.Scan(&a.ID, &a.ArticleID, &a.Name, &a.MIME, &a.Size, &a.CreatedAt, &deleted) == nil {
			if deleted.Valid {
				a.DeletedAt = deleted.String
			}
			a.URL = "/files/" + a.ID
			atts = append(atts, a)
		}
	}
	ar.Close()
	favs := exportFavorites(tx)
	if e = tx.Commit(); e != nil {
		apiErr(w, 500, "database error")
		return
	}
	w.Header().Set("Content-Disposition", "attachment; filename=aiwiki-export.json")
	writeJSON(w, 200, map[string]any{"format": "aiwiki-v1", "spaces": spaces, "articles": articles, "revisions": revs, "comments": comments, "attachments": atts, "favorites": favs})
}

type rpcRequest struct {
	JSONRPC string                     `json:"jsonrpc"`
	ID      any                        `json:"id"`
	Method  string                     `json:"method"`
	Params  map[string]json.RawMessage `json:"params"`
}

func (s *Server) mcp(w http.ResponseWriter, r *http.Request) {
	if r.Method == "GET" {
		apiErr(w, 405, "SSE is not supported")
		return
	}
	if r.Method != "POST" {
		apiErr(w, 405, "method not allowed")
		return
	}
	var q rpcRequest
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 3<<20)).Decode(&q) != nil {
		apiErr(w, 400, "invalid JSON-RPC")
		return
	}
	if strings.HasPrefix(q.Method, "notifications/") {
		w.WriteHeader(202)
		return
	}
	if s.principal != nil && s.principal.MCPDelegationID != "" {
		method, tool := q.Method, ""
		_ = json.Unmarshal(q.Params["name"], &tool)
		method, tool = mcpAuditName(method), mcpAuditName(tool)
		_, err := s.DB.Exec(`SELECT corporate_mcp_request_audit(?,?,?)`, s.principal.MCPDelegationID, method, tool)
		if err != nil {
			apiErr(w, 503, "audit unavailable")
			return
		}
	}
	var result any
	switch q.Method {
	case "initialize":
		instructions := mcpInstructions
		if s.principal != nil && s.principal.MCPProjects != nil {
			instructions = "Temporary read-only access to exact projects " + jsonText(s.principal.MCPProjects) + ". Space-level and global rules are outside this credential's scope; if bootstrap is empty, use wiki_search to discover permitted articles. Use revision-pinned bounded reads (max_chars=4000). Access expires at " + s.principal.MCPExpiresAt.UTC().Format("2006-01-02T15:04:05Z") + "."
		}
		result = map[string]any{"protocolVersion": "2025-03-26", "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]string{"name": "aiwiki", "version": "1.0"}, "instructions": instructions}
	case "ping":
		result = map[string]any{}
	case "tools/list":
		tools := mcpToolList()
		if s.principal != nil && s.principal.MCPProjects != nil {
			visible := make([]map[string]any, 0, len(tools))
			for _, tool := range tools {
				if name, _ := tool["name"].(string); mcpReadTool(name) {
					visible = append(visible, tool)
				}
			}
			tools = visible
		}
		result = map[string]any{"tools": tools}
	case "tools/call":
		result = s.callTool(q.Params)
	default:
		writeJSON(w, 200, map[string]any{"jsonrpc": "2.0", "id": q.ID, "error": map[string]any{"code": -32601, "message": "method not found"}})
		return
	}
	writeJSON(w, 200, map[string]any{"jsonrpc": "2.0", "id": q.ID, "result": result})
}

func objectSchema(properties map[string]any, required ...string) map[string]any {
	s := map[string]any{"type": "object", "properties": properties}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}
func stringSchema() map[string]any { return map[string]any{"type": "string"} }
func stringArraySchema() map[string]any {
	return map[string]any{"type": "array", "items": stringSchema()}
}
func validToolID(id string) bool {
	if id == "" {
		return false
	}
	for _, c := range id {
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

func toolResult(isError bool, text string) any {
	return map[string]any{"isError": isError, "content": []any{map[string]string{"type": "text", "text": text}}}
}

var inlineHeadingLink = regexp.MustCompile(`!?\[([^\]]+)\]\([^)]*\)`)

// markdownSlug mirrors the web UI (web/src/markdown.tsx): the plain text of the
// heading (links and images reduced to their label, code spans to their text),
// lower-cased, every run of characters that are not letters or digits becomes
// one '-', trimmed of '-' at both ends; an empty result is "section".
func markdownSlug(text string) string {
	text = inlineHeadingLink.ReplaceAllString(text, "$1")
	text = strings.ReplaceAll(text, "`", "")
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(text) {
		if unicode.IsLetter(r) || unicode.IsNumber(r) {
			if dash && b.Len() > 0 {
				b.WriteByte('-')
			}
			b.WriteRune(r)
			dash = false
		} else {
			dash = true
		}
	}
	if b.Len() == 0 {
		return "section"
	}
	return b.String()
}
