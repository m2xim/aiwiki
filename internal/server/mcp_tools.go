package server

import (
	"bytes"
	"encoding/json"
	"io"
	"math"
	"net/http/httptest"
	"strconv"
	"strings"
	"unicode/utf8"
)

const mcpInstructions = "AI Wiki workflow: (1) call wiki_bootstrap with the space_id of your project to get its pinned rules in full; " +
	"(2) wiki_search to find articles, or wiki_search_sections to find sections and get revision-pinned read_args (ranked, supports Russian and English); (3) for large articles use wiki_read with max_chars=4000 and follow next_read as needed; alternatively use outline=true then anchor; " +
	"(4) wiki_links shows the parent, children and linked neighbours of an article; (5) use wiki_edit for small changes (replace_section, append, replace_text) instead of resending the whole article; " +
	"(6) wiki_write creates articles or updates them (unsent fields are kept) and always needs base_revision for updates. " +
	"Link articles in Markdown as [text](/?article=ID#anchor)."

// jsonText marshals v without HTML escaping or a trailing newline.
func jsonText(v any) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
	return strings.TrimRight(buf.String(), "\n")
}

func toolOK(v any) any { return toolResult(false, jsonText(v)) }
func toolErr(msg string) any {
	return toolResult(true, jsonText(map[string]string{"error": msg}))
}

// compactArticle is the token-light article view used by MCP tools.
type compactArticle struct {
	ID        string   `json:"id"`
	Title     string   `json:"title"`
	SpaceID   string   `json:"space_id"`
	ProjectID string   `json:"project_id,omitempty"`
	Kind      string   `json:"kind,omitempty"`
	Status    string   `json:"status,omitempty"`
	Summary   string   `json:"summary,omitempty"`
	Tags      []string `json:"tags,omitempty"`
	UpdatedAt string   `json:"updated_at,omitempty"`
}

func compact(a Article) compactArticle {
	return compactArticle{ID: a.ID, Title: a.Title, SpaceID: a.SpaceID, ProjectID: a.ProjectID, Kind: a.Kind, Status: a.Status, Summary: a.Summary, Tags: a.Tags, UpdatedAt: a.UpdatedAt}
}
func compactAll(in []Article) []compactArticle {
	out := make([]compactArticle, 0, len(in))
	for _, a := range in {
		out = append(out, compact(a))
	}
	return out
}

func prop(typ, desc string) map[string]any { return map[string]any{"type": typ, "description": desc} }
func strArrProp(desc string) map[string]any {
	return map[string]any{"type": "array", "items": stringSchema(), "description": desc}
}

func mcpToolList() []map[string]any {
	idDesc := "Article id (letters, digits, underscore, hyphen)."
	article := objectSchema(map[string]any{
		"space_id":      prop("string", "Space the article belongs to (e.g. general). Required for create."),
		"project_id":    prop("string", "Project within the space; empty for a direct space document."),
		"parent_id":     prop("string", "Id of the parent article (tree structure), empty for top level."),
		"title":         prop("string", "Article title, at most 300 characters. Required for create."),
		"summary":       prop("string", "Short plain-text description (1-2 sentences) of what the article contains; it is shown in search results and indexed for search, so make it informative."),
		"body":          prop("string", "Full Markdown body. Link other articles as [text](/?article=ID#anchor)."),
		"kind":          map[string]any{"type": "string", "enum": []string{"article", "instruction", "research", "plan", "source"}, "description": "Article type: instruction = rules for agents (usually pinned), research = findings, plan = work plan, source = raw source material, article = default."},
		"status":        map[string]any{"type": "string", "enum": []string{"draft", "active", "verified", "archived"}, "description": "Lifecycle: draft (default), active, verified (checked by a human), archived (hidden from bootstrap/recent)."},
		"pinned":        prop("boolean", "Pinned articles are returned in full by wiki_bootstrap (use for rules every agent must read)."),
		"tags":          map[string]any{"type": "array", "items": stringSchema(), "description": "Tags for filtering."},
		"properties":    map[string]any{"type": "object", "additionalProperties": stringSchema(), "description": "String key/value metadata, searchable with the property filter as key:value."},
		"base_revision": map[string]any{"type": "integer", "minimum": 1, "description": "Revision you last read; required when updating. A mismatch returns a revision conflict."},
		"author":        prop("string", "Name of the author of this change (e.g. agent name)."),
		"message":       prop("string", "Short change description stored in history."),
	})
	article["description"] = "Editable article fields. Create: space_id and title required. Update (id given): include base_revision; fields not sent keep their current values."
	return []map[string]any{
		{"name": "wiki_bootstrap", "description": "Start of every session. Without space_id: returns full pinned instructions of the general space, compact other_pinned (pinned articles of other spaces without bodies), spaces and recent changes. Call it again with the space_id of your project to get that space's pinned rules in full.", "inputSchema": objectSchema(map[string]any{"space_id": prop("string", "Project space id; when given, pinned articles of general and this space are returned in full.")})},
		{"name": "wiki_search", "description": "Ranked full-text search over title, summary, tags, properties and body with Russian and English morphology (word forms match). All words must match (AND); if no article matches all words, the best partial matches are returned and mode is \"any\". Results are compact (no body) with score, snippet and matched_in; most relevant first. Filters narrow the result. Paging: if next_offset is present, call again with offset=next_offset.", "inputSchema": objectSchema(map[string]any{
			"q":          prop("string", "Search words, whitespace separated. Omit to list by recent update."),
			"space_id":   prop("string", "Only articles of this space."),
			"project_id": prop("string", "Only articles directly in this project."),
			"tag":        strArrProp("Only articles having all of these tags (exact match)."),
			"property":   strArrProp("Only articles having all of these properties, each as key:value (exact match)."),
			"kind":       prop("string", "Only this kind: article, instruction, research, plan or source."),
			"pinned":     prop("boolean", "true: only pinned articles."),
			"limit":      prop("integer", "Page size, default 20, maximum 100."),
			"offset":     prop("integer", "Number of results to skip (use next_offset from the previous response)."),
		})},
		{"name": "wiki_search_sections", "description": "Search sections using their own heading/text, weaker ancestor headings and article identity (title, summary, tags, property values). Common function words are ignored when other terms remain; rare terms and local evidence rank higher. mode all covers effective terms, otherwise partial fallback any. matched_in separates local fields from article context; context-only hits may have empty snippet and need verification. Use revision-pinned read_args directly in wiki_read. Global search excludes archived spaces unless explicitly scoped. Pages reflect current data, not a snapshot.", "inputSchema": objectSchema(map[string]any{
			"q":          prop("string", "Required nonempty search words; Russian and English morphology."),
			"space_id":   prop("string", "Only this space; omit for global search."),
			"project_id": prop("string", "Only this project; omit for all accessible projects."),
			"kind":       prop("string", "Only this article kind."),
			"pinned":     prop("boolean", "true: only pinned articles."),
			"tag":        strArrProp("Require all exact tags."),
			"property":   strArrProp("Require all exact key:value properties."),
			"limit":      map[string]any{"type": "integer", "minimum": 1, "maximum": 20, "description": "Page size, default 5, maximum 20."},
			"offset":     map[string]any{"type": "integer", "minimum": 0, "description": "Skip this many results; use next_offset when present."},
		}, "q")},
		{"name": "wiki_read", "description": "Read an article or anchor section (heading to next peer). For bounded text use max_chars=4000; returns text, offset, total_chars, has_more and next_read. Follow next_read verbatim to preserve revision after edits. Without max_chars/offset legacy full body or section is returned. outline=true returns headings without body and cannot combine with bounded mode. revision selects a saved snapshot. Use wiki_comments for discussion.", "inputSchema": objectSchema(map[string]any{
			"id":        prop("string", idDesc),
			"outline":   prop("boolean", "true: return metadata, total chars and the list of headings {level,title,anchor,chars} without body."),
			"anchor":    prop("string", "Heading anchor (from outline): lowercase slug of the heading text, repeated headings get -2, -3. Returns only that section as 'section'."),
			"revision":  map[string]any{"type": "integer", "minimum": 1, "description": "Read this historical revision instead of the current one (see wiki_history)."},
			"max_chars": map[string]any{"type": "integer", "minimum": 1, "maximum": 16000, "description": "Opt into bounded reading, at most this many Unicode code points (not tokens), default 4000 when offset is supplied."},
			"offset":    map[string]any{"type": "integer", "minimum": 0, "description": "Code point offset within selected section or full body. Nonzero requires revision. Prefer returned next_read to continue."},
		}, "id")},
		{"name": "wiki_edit", "description": "Small targeted edit without resending the whole article. op=replace_section: replace the section at anchor (text must start with the new heading line); op=append: add text at the end of the article, or at the end of the section at anchor; op=replace_text: replace the single occurrence of old with text (text may be empty to delete). Needs the revision you last read as base_revision. Returns {id,revision,updated_at,chars}.", "inputSchema": objectSchema(map[string]any{
			"id":            prop("string", idDesc),
			"base_revision": map[string]any{"type": "integer", "minimum": 1, "description": "Revision you last read; a mismatch returns a revision conflict."},
			"op":            map[string]any{"type": "string", "enum": []string{"replace_section", "append", "replace_text"}, "description": "Edit operation."},
			"anchor":        prop("string", "Heading anchor from wiki_read outline. Required for replace_section, optional for append."),
			"text":          prop("string", "New content. replace_section: whole section including its heading line. append: text to add. replace_text: replacement (may be empty)."),
			"old":           prop("string", "replace_text only: exact text to replace; must occur in the body exactly once."),
			"author":        prop("string", "Name of the author of this change."),
			"message":       prop("string", "Short change description stored in history."),
		}, "id", "base_revision", "op")},
		{"name": "wiki_write", "description": "Create an article (no id) or update one (id + article.base_revision). Update is a merge: fields you do not send keep their current values, so you may send only what changes. For small body changes prefer wiki_edit. Link other articles as [text](/?article=ID). Returns {id,revision,updated_at,title,space_id} without the body.", "inputSchema": objectSchema(map[string]any{"id": prop("string", "Id of the article to update; omit to create."), "article": article}, "article")},
		{"name": "wiki_links", "description": "Navigate relations of an article: parent, children (sorted by title), outgoing links (articles it links to, with anchor when the link has one) and incoming links (articles linking to it). All entries are compact; deleted articles are skipped. To link articles in Markdown write [text](/?article=ID#anchor).", "inputSchema": objectSchema(map[string]any{"id": prop("string", idDesc)}, "id")},
		{"name": "wiki_history", "description": "List revisions of an article, newest first, as compact {revision,author,message,created_at} without content. With revision returns that single snapshot in full (wiki_read with revision returns the body in a lighter form).", "inputSchema": objectSchema(map[string]any{"id": prop("string", idDesc), "revision": map[string]any{"type": "integer", "minimum": 1, "description": "Return this revision's full snapshot."}}, "id")},
		{"name": "wiki_comments", "description": "Read all comments on an article, including resolved comments, in chronological order", "inputSchema": objectSchema(map[string]any{"id": stringSchema()}, "id")},
		{"name": "wiki_restore", "description": "Restore a prior revision as a new revision", "inputSchema": objectSchema(map[string]any{"id": stringSchema(), "revision": map[string]any{"type": "integer", "minimum": 1}, "base_revision": map[string]any{"type": "integer", "minimum": 1}, "author": stringSchema(), "message": stringSchema()}, "id", "revision", "base_revision")},
		{"name": "wiki_comment", "description": "Add a comment to an article heading", "inputSchema": objectSchema(map[string]any{"id": stringSchema(), "anchor": stringSchema(), "body": stringSchema(), "author": stringSchema()}, "id", "body")},
	}
}

func (s *Server) callTool(params map[string]json.RawMessage) any {
	var name string
	_ = json.Unmarshal(params["name"], &name)
	if s.principal != nil && s.principal.MCPProjects != nil && !mcpReadTool(name) {
		return toolErr("temporary MCP access is read-only")
	}
	var args map[string]json.RawMessage
	if raw, ok := params["arguments"]; ok && strings.TrimSpace(string(raw)) != "null" {
		if json.Unmarshal(raw, &args) != nil {
			return toolErr("arguments must be a JSON object")
		}
	}
	switch name {
	case "wiki_bootstrap":
		return s.toolBootstrap(args)
	case "wiki_search":
		return s.toolSearch(args)
	case "wiki_search_sections":
		return s.toolSearchSections(args)
	case "wiki_read":
		return s.toolRead(args)
	case "wiki_edit":
		return s.toolEdit(args)
	case "wiki_write":
		return s.toolWrite(args)
	case "wiki_links":
		return s.toolLinks(args)
	case "wiki_history":
		return s.toolHistory(args)
	case "wiki_comments", "wiki_restore", "wiki_comment":
		id, bad := toolID(args)
		if bad != nil {
			return bad
		}
		switch name {
		case "wiki_comments":
			return s.proxy("GET", "/api/articles/"+id+"/comments", nil)
		case "wiki_restore":
			b, _ := json.Marshal(args)
			return s.proxy("POST", "/api/articles/"+id+"/restore", b)
		default:
			b, _ := json.Marshal(args)
			return s.proxy("POST", "/api/articles/"+id+"/comments", b)
		}
	}
	names := []string{}
	for _, t := range mcpToolList() {
		names = append(names, t["name"].(string))
	}
	return toolErr("unknown tool " + strconv.Quote(name) + "; available tools: " + strings.Join(names, ", "))
}

// proxy runs a REST request in-process and wraps the response as a tool result.
func (s *Server) proxy(method, path string, body []byte) any {
	if body == nil {
		body = []byte("{}")
	}
	rr := httptest.NewRecorder()
	// This in-process request follows the outer MCP request's Host/Origin checks.
	s.routes().ServeHTTP(rr, httptest.NewRequest(method, path, bytes.NewReader(body)))
	b, _ := io.ReadAll(rr.Result().Body)
	return toolResult(rr.Code >= 400, string(b))
}

// toolID extracts and validates the required id argument.
func toolID(args map[string]json.RawMessage) (string, any) {
	raw, ok := args["id"]
	if !ok {
		return "", toolErr("id is required")
	}
	var id string
	if json.Unmarshal(raw, &id) != nil || !validToolID(id) {
		return "", toolErr("id must contain only letters, digits, underscores, or hyphens")
	}
	return id, nil
}

// argReader reads typed tool arguments strictly: an absent or null key yields
// the zero value, a present key of the wrong type records "<name> must be a
// <type>" (the first such error wins) and yields the zero value. Callers read
// all arguments and then return r.fail() when it is not nil.
type argReader struct {
	args map[string]json.RawMessage
	err  string
}

func newArgs(args map[string]json.RawMessage) *argReader { return &argReader{args: args} }

func (r *argReader) value(key string, typ string, dst any) {
	raw, ok := r.args[key]
	if !ok || strings.TrimSpace(string(raw)) == "null" {
		return
	}
	if json.Unmarshal(raw, dst) != nil && r.err == "" {
		r.err = key + " must be a " + typ
	}
}

func (r *argReader) str(key string) string {
	var v string
	r.value(key, "string", &v)
	return v
}
func (r *argReader) integer(key string) int {
	var v int
	r.value(key, "integer", &v)
	return v
}
func (r *argReader) boolean(key string) bool {
	var v bool
	r.value(key, "boolean", &v)
	return v
}

// strings reads a string or an array of strings (a lone string is one value, an
// empty one none); a non-string element is an error "<key> must be a string or
// an array of strings".
func (r *argReader) strings(key string) []string {
	raw, ok := r.args[key]
	if !ok || strings.TrimSpace(string(raw)) == "null" {
		return []string{}
	}
	var single string
	if json.Unmarshal(raw, &single) == nil {
		if single == "" {
			return []string{}
		}
		return []string{single}
	}
	var items []json.RawMessage
	out := []string{}
	if json.Unmarshal(raw, &items) == nil {
		for _, it := range items {
			var v string
			if strings.TrimSpace(string(it)) == "null" || json.Unmarshal(it, &v) != nil {
				out = nil
				break
			}
			out = append(out, v)
		}
	} else {
		out = nil
	}
	if out == nil {
		if r.err == "" {
			r.err = key + " must be a string or an array of strings"
		}
		return []string{}
	}
	return out
}

// fail returns the tool error for the first bad argument, or nil.
func (r *argReader) fail() any {
	if r.err == "" {
		return nil
	}
	return toolErr(r.err)
}

// ---- wiki_search ----

type searchItem struct {
	compactArticle
	Score     *float64 `json:"score,omitempty"`
	Snippet   string   `json:"snippet,omitempty"`
	MatchedIn []string `json:"matched_in,omitempty"`
}

func (s *Server) toolSearch(args map[string]json.RawMessage) any {
	ar := newArgs(args)
	p := SearchParams{ProjectID: ar.str("project_id"), Q: ar.str("q"), SpaceID: ar.str("space_id"), Kind: ar.str("kind"), Pinned: ar.boolean("pinned"), Limit: 20, AllowAny: true}
	limit, offset := ar.integer("limit"), ar.integer("offset")
	p.Tags = ar.strings("tag")
	p.Properties = ar.strings("property")
	if bad := ar.fail(); bad != nil {
		return bad
	}
	if limit > 0 {
		p.Limit = limit
	}
	if p.Limit > 100 {
		p.Limit = 100
	}
	if offset > 0 {
		p.Offset = offset
	}
	res, _, err := s.searchArticles(s.DB.context(), p)
	if err != nil {
		return toolErr(err.Error())
	}
	items := make([]searchItem, 0, len(res.Hits))
	for _, h := range res.Hits {
		it := searchItem{compactArticle: compact(h.Article), Snippet: h.Snippet, MatchedIn: h.MatchedIn}
		if strings.TrimSpace(p.Q) != "" {
			sc := math.Round(h.Score*100) / 100
			it.Score = &sc
		}
		items = append(items, it)
	}
	out := struct {
		Total      int          `json:"total"`
		Offset     int          `json:"offset"`
		Mode       string       `json:"mode,omitempty"`
		Results    []searchItem `json:"results"`
		NextOffset *int         `json:"next_offset,omitempty"`
	}{res.Total, p.Offset, res.Mode, items, nil}
	if res.Total > p.Offset+len(items) && len(items) > 0 {
		n := p.Offset + len(items)
		out.NextOffset = &n
	}
	return toolOK(out)
}

// ---- wiki_links ----

// linksSubject identifies the article of wiki_links; deleted_at is set when it is deleted.
type linksSubject struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	DeletedAt string `json:"deleted_at,omitempty"`
}

type outgoingItem struct {
	compactArticle
	Anchor string `json:"anchor,omitempty"`
}

func (s *Server) toolLinks(args map[string]json.RawMessage) any {
	id, bad := toolID(args)
	if bad != nil {
		return bad
	}
	a, err := s.articleByID(s.DB, id)
	if err != nil {
		return toolErr("article not found")
	}
	outgoing, incoming, err := s.articleLinks(a)
	if err != nil {
		return toolErr("database error")
	}
	var parent any
	if a.ParentID != "" {
		if p, e := s.articleByID(s.DB, a.ParentID); e == nil && p.DeletedAt == "" {
			parent = compact(p)
		}
	}
	children := s.selectArticles("SELECT "+articleColumns+" FROM articles WHERE parent_id=? AND deleted_at IS NULL AND id<>? ORDER BY title,id", a.ID, a.ID)
	out := make([]outgoingItem, 0, len(outgoing))
	for _, l := range outgoing {
		out = append(out, outgoingItem{compact(l.Article), l.Anchor})
	}
	return toolOK(map[string]any{
		"article":  linksSubject{a.ID, a.Title, a.DeletedAt},
		"parent":   parent,
		"children": compactAll(children),
		"outgoing": out,
		"incoming": compactAll(incoming),
	})
}

// ---- wiki_read ----

type outlineHeading struct {
	Level  int    `json:"level"`
	Title  string `json:"title"`
	Anchor string `json:"anchor"`
	Chars  int    `json:"chars"`
}

func (s *Server) snapshotRevision(id string, rev int) (Article, bool) {
	var raw string
	if s.DB.QueryRow("SELECT snapshot FROM revisions WHERE article_id=? AND revision=?", id, rev).Scan(&raw) != nil {
		return Article{}, false
	}
	var a Article
	if json.Unmarshal([]byte(raw), &a) != nil {
		return Article{}, false
	}
	a.ID = id
	a.Revision = rev
	return a, true
}

func (s *Server) toolRead(args map[string]json.RawMessage) any {
	_, boundedSize := args["max_chars"]
	_, boundedOffset := args["offset"]
	if boundedSize || boundedOffset {
		return s.toolReadChunk(args)
	}
	id, bad := toolID(args)
	if bad != nil {
		return bad
	}
	ar := newArgs(args)
	rev, anchor, outline := ar.integer("revision"), ar.str("anchor"), ar.boolean("outline")
	if bad := ar.fail(); bad != nil {
		return bad
	}
	a, err := s.articleByID(s.DB, id)
	if err != nil {
		return toolErr("article not found")
	}
	currentDeleted := a.DeletedAt
	historical := false
	if rev > 0 && rev != a.Revision {
		old, ok := s.snapshotRevision(id, rev)
		if !ok {
			return toolErr("revision not found")
		}
		a, historical = old, true
	}
	if anchor != "" {
		section, ok := markdownSection(a.Body, anchor)
		if !ok {
			return toolErr("heading not found")
		}
		return toolOK(struct {
			ID         string `json:"id"`
			Title      string `json:"title"`
			Revision   int    `json:"revision"`
			Anchor     string `json:"anchor"`
			Partial    bool   `json:"partial"`
			Historical bool   `json:"historical,omitempty"`
			DeletedAt  string `json:"deleted_at,omitempty"`
			Section    string `json:"section"`
		}{a.ID, a.Title, a.Revision, anchor, true, historical, currentDeleted, section})
	}
	if outline {
		lines := strings.Split(a.Body, "\n")
		hs := []outlineHeading{}
		for _, h := range parseMarkdownHeadings(lines) {
			hs = append(hs, outlineHeading{h.level, h.title, h.slug, utf8.RuneCountInString(strings.Join(lines[h.start:h.end], "\n"))})
		}
		return toolOK(struct {
			ID         string           `json:"id"`
			Title      string           `json:"title"`
			Summary    string           `json:"summary,omitempty"`
			SpaceID    string           `json:"space_id"`
			ParentID   string           `json:"parent_id,omitempty"`
			Kind       string           `json:"kind"`
			Status     string           `json:"status"`
			Revision   int              `json:"revision"`
			UpdatedAt  string           `json:"updated_at"`
			Historical bool             `json:"historical,omitempty"`
			DeletedAt  string           `json:"deleted_at,omitempty"`
			Chars      int              `json:"chars"`
			Headings   []outlineHeading `json:"headings"`
		}{a.ID, a.Title, a.Summary, a.SpaceID, a.ParentID, a.Kind, a.Status, a.Revision, a.UpdatedAt, historical, currentDeleted, utf8.RuneCountInString(a.Body), hs})
	}
	if historical {
		return toolOK(struct {
			Article
			Historical       bool   `json:"historical"`
			CurrentDeletedAt string `json:"current_deleted_at,omitempty"`
		}{a, true, currentDeleted})
	}
	return toolOK(a)
}

// ---- wiki_write ----

func (s *Server) toolWrite(args map[string]json.RawMessage) any {
	id := ""
	if _, has := args["id"]; has {
		var bad any
		if id, bad = toolID(args); bad != nil {
			return bad
		}
	}
	var article json.RawMessage
	_ = json.Unmarshal(args["article"], &article)
	if len(article) == 0 || !json.Valid(article) {
		return toolErr("article object is required")
	}
	method, path := "POST", "/api/articles"
	if id != "" {
		method, path = "PUT", "/api/articles/"+id
	}
	res := s.proxy(method, path, article).(map[string]any)
	if res["isError"] == true {
		return res
	}
	var a Article
	text := res["content"].([]any)[0].(map[string]string)["text"]
	if json.Unmarshal([]byte(text), &a) != nil {
		return res
	}
	return toolOK(map[string]any{"id": a.ID, "revision": a.Revision, "updated_at": a.UpdatedAt, "title": a.Title, "space_id": a.SpaceID})
}

// ---- wiki_history ----

func (s *Server) toolHistory(args map[string]json.RawMessage) any {
	id, bad := toolID(args)
	if bad != nil {
		return bad
	}
	ar := newArgs(args)
	rev := ar.integer("revision")
	if bad := ar.fail(); bad != nil {
		return bad
	}
	if _, err := s.articleByID(s.DB, id); err != nil {
		return toolErr("article not found")
	}
	if rev > 0 {
		var x Revision
		var raw string
		err := s.DB.QueryRow("SELECT id,article_id,revision,author,message,created_at,snapshot FROM revisions WHERE article_id=? AND revision=?", id, rev).Scan(&x.ID, &x.ArticleID, &x.Revision, &x.Author, &x.Message, &x.CreatedAt, &raw)
		if err != nil {
			return toolErr("revision not found")
		}
		_ = json.Unmarshal([]byte(raw), &x.Snapshot)
		return toolOK(x)
	}
	rows, err := s.DB.Query("SELECT revision,author,message,created_at FROM revisions WHERE article_id=? ORDER BY revision DESC", id)
	if err != nil {
		return toolErr("database error")
	}
	defer rows.Close()
	type item struct {
		Revision  int    `json:"revision"`
		Author    string `json:"author"`
		Message   string `json:"message,omitempty"`
		CreatedAt string `json:"created_at"`
	}
	out := []item{}
	for rows.Next() {
		var it item
		if rows.Scan(&it.Revision, &it.Author, &it.Message, &it.CreatedAt) == nil {
			out = append(out, it)
		}
	}
	return toolOK(out)
}

// ---- wiki_bootstrap ----

func (s *Server) toolBootstrap(args map[string]json.RawMessage) any {
	ar := newArgs(args)
	space := ar.str("space_id")
	if bad := ar.fail(); bad != nil {
		return bad
	}
	spaces := []Space{}
	rows, e := s.DB.Query("SELECT id,name,description,archived_at FROM spaces WHERE archived_at IS NULL ORDER BY name")
	if e != nil {
		return toolErr("database error")
	}
	for rows.Next() {
		if x, err := scanSpace(rows); err == nil {
			spaces = append(spaces, x)
		}
	}
	rows.Close()
	live := "deleted_at IS NULL AND status<>'archived' AND space_id IN (SELECT id FROM spaces WHERE archived_at IS NULL)"
	if s.DB.pg {
		live = "deleted_at IS NULL AND status<>'archived' AND corporate_container_writable(space_id,project_id)"
	}
	out := map[string]any{"spaces": spaces}
	var instructions []Article
	recentWhere, recentArgs := live, []any{}
	if space == "" {
		instructions = s.selectArticles("SELECT " + articleColumns + " FROM articles WHERE " + live + " AND pinned=1 AND space_id='general' ORDER BY title,id")
		others := s.selectArticles("SELECT " + articleColumns + " FROM articles WHERE " + live + " AND pinned=1 AND space_id<>'general' ORDER BY space_id,title,id")
		out["other_pinned"] = compactAll(others)
		out["guide"] = "Only pinned rules of the general space are included in full. other_pinned lists pinned articles of other spaces without bodies. Call wiki_bootstrap again with the space_id of your project to get its pinned rules in full, then use wiki_search / wiki_read (outline, anchor) / wiki_links, edit with wiki_edit, and pass base_revision on updates."
	} else {
		instructions = s.selectArticles("SELECT "+articleColumns+" FROM articles WHERE "+live+" AND pinned=1 AND (space_id='general' OR space_id=?) ORDER BY title,id", space)
		recentWhere += " AND (space_id='general' OR space_id=?)"
		recentArgs = append(recentArgs, space)
		out["guide"] = "Pinned rules of general and this space are included in full. Use wiki_search / wiki_read (outline, anchor) / wiki_links to find context, wiki_edit for small changes, and pass base_revision on updates."
	}
	inInstr := map[string]bool{}
	for _, a := range instructions {
		inInstr[a.ID] = true
	}
	out["instructions"] = instructions
	cand := s.selectArticles("SELECT "+articleColumns+" FROM articles WHERE "+recentWhere+" ORDER BY updated_at DESC,id LIMIT ?", append(recentArgs, 12+len(instructions))...)
	recent := []compactArticle{}
	for _, a := range cand {
		if !inInstr[a.ID] && len(recent) < 12 {
			recent = append(recent, compact(a))
		}
	}
	out["recent"] = recent
	return toolOK(out)
}

// ---- markdown headings ----

type markdownHeading struct {
	start, end, level int
	title, slug       string
}

// lineIndent returns the indentation width of line (tab stops of 4) and the
// line without its leading whitespace.
func lineIndent(line string) (int, string) {
	col := 0
	for i := 0; i < len(line); i++ {
		switch line[i] {
		case ' ':
			col++
		case '\t':
			col += 4 - col%4
		default:
			return col, line[i:]
		}
	}
	return col, ""
}

// fenceOpen reports an opening code fence (rest has no indentation).
func fenceOpen(rest string) (char byte, n int, ok bool) {
	if len(rest) < 3 || (rest[0] != '`' && rest[0] != '~') {
		return 0, 0, false
	}
	char = rest[0]
	for n < len(rest) && rest[n] == char {
		n++
	}
	if n < 3 || (char == '`' && strings.IndexByte(rest[n:], '`') >= 0) {
		return 0, 0, false
	}
	return char, n, true
}

// allSame reports whether s consists only of c (at least one).
func allSame(s string, c byte) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] != c {
			return false
		}
	}
	return true
}

func isThematicBreak(rest string) bool {
	var c byte
	n := 0
	for i := 0; i < len(rest); i++ {
		switch ch := rest[i]; ch {
		case ' ', '\t':
		case '-', '*', '_':
			if c != 0 && ch != c {
				return false
			}
			c = ch
			n++
		default:
			return false
		}
	}
	return n >= 3
}

func isListMarker(rest string) bool {
	if len(rest) >= 2 && (rest[0] == '-' || rest[0] == '*' || rest[0] == '+') && (rest[1] == ' ' || rest[1] == '\t') {
		return true
	}
	i := 0
	for i < len(rest) && i < 9 && rest[i] >= '0' && rest[i] <= '9' {
		i++
	}
	return i > 0 && i+1 < len(rest) && (rest[i] == '.' || rest[i] == ')') && (rest[i+1] == ' ' || rest[i+1] == '\t')
}

// atxHeading parses an ATX heading line (rest has no indentation): the level
// and the title without the optional closing '#' sequence.
func atxHeading(rest string) (int, string, bool) {
	level := 0
	for level < len(rest) && level < 7 && rest[level] == '#' {
		level++
	}
	if level == 0 || level > 6 || (level < len(rest) && rest[level] != ' ' && rest[level] != '\t') {
		return 0, "", false
	}
	title := strings.TrimSpace(rest[level:])
	end := len(title)
	for end > 0 && title[end-1] == '#' {
		end--
	}
	if end == 0 {
		title = ""
	} else if end < len(title) && (title[end-1] == ' ' || title[end-1] == '\t') {
		title = strings.TrimSpace(title[:end])
	}
	return level, title, true
}

// Paragraph state while scanning lines.
const (
	blockNone   = iota // blank, heading, fence, code: nothing to underline
	blockPara          // plain paragraph text: a setext underline makes it a heading
	blockOpaque        // list item, quote, table: its lazy lines are not underlinable
)

// parseMarkdownHeadings finds ATX and Setext headings following CommonMark:
// ATX needs an indent of at most 3 spaces; a setext underline (=== or ---, up
// to 3 spaces of indent) turns the preceding paragraph into a heading, and
// otherwise --- is a thematic break; fenced and indented code never contain
// headings. start is the first line of the heading; end is the exclusive end
// of its section (next heading of the same or higher level, or the end).
// Slugs match the web UI: see markdownSlug; headings with an empty slug are
// "section", duplicates get -2, -3 by base slug.
func parseMarkdownHeadings(lines []string) []markdownHeading {
	counts := map[string]int{}
	headings := []markdownHeading{}
	add := func(start, level int, title string) {
		slug := markdownSlug(title)
		counts[slug]++
		if counts[slug] > 1 {
			slug += "-" + strconv.Itoa(counts[slug])
		}
		headings = append(headings, markdownHeading{start: start, level: level, title: title, slug: slug})
	}
	fenceChar, fenceLen := byte(0), 0
	block, paraStart := blockNone, 0
	for i, line := range lines {
		indent, rest := lineIndent(line)
		rest = strings.TrimRight(rest, " \t\r")
		if fenceChar != 0 {
			if indent <= 3 && len(rest) >= fenceLen && allSame(rest, fenceChar) {
				fenceChar, fenceLen = 0, 0
			}
			continue
		}
		if rest == "" {
			block = blockNone
			continue
		}
		if indent >= 4 {
			// Indented code, or a lazy continuation line of an open paragraph.
			continue
		}
		if c, n, ok := fenceOpen(rest); ok {
			fenceChar, fenceLen, block = c, n, blockNone
			continue
		}
		if level, title, ok := atxHeading(rest); ok {
			add(i, level, title)
			block = blockNone
			continue
		}
		if block == blockPara && (allSame(rest, '=') || allSame(rest, '-')) {
			level := 2
			if rest[0] == '=' {
				level = 1
			}
			parts := make([]string, 0, i-paraStart)
			for _, l := range lines[paraStart:i] {
				parts = append(parts, strings.TrimSpace(l))
			}
			add(paraStart, level, strings.Join(parts, " "))
			block = blockNone
			continue
		}
		switch {
		case isThematicBreak(rest):
			block = blockNone
		case rest[0] == '>' || rest[0] == '|' || isListMarker(rest):
			block = blockOpaque
		case block == blockNone:
			block, paraStart = blockPara, i
		}
	}
	for i := range headings {
		headings[i].end = len(lines)
		for _, h := range headings[i+1:] {
			if h.level <= headings[i].level {
				headings[i].end = h.start
				break
			}
		}
	}
	return headings
}

func markdownSection(body, anchor string) (string, bool) {
	lines := strings.Split(body, "\n")
	for _, h := range parseMarkdownHeadings(lines) {
		if h.slug == anchor {
			return strings.Join(lines[h.start:h.end], "\n"), true
		}
	}
	return "", false
}
