package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// rpc posts a JSON-RPC tools/call through POST /mcp.
func rpc(t *testing.T, s *Server, name string, args any) (bool, string) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": name, "arguments": args}})
	code, data := request(t, s, http.MethodPost, "/mcp", string(body))
	if code != http.StatusOK {
		t.Fatalf("MCP %s: %d %s", name, code, data)
	}
	var env struct {
		Result struct {
			IsError bool `json:"isError"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if err := json.Unmarshal(data, &env); err != nil || len(env.Result.Content) != 1 {
		t.Fatalf("MCP %s bad envelope: %s (%v)", name, data, err)
	}
	return env.Result.IsError, env.Result.Content[0].Text
}

func mustOK(t *testing.T, s *Server, name string, args any, out any) string {
	t.Helper()
	isErr, text := rpc(t, s, name, args)
	if isErr {
		t.Fatalf("%s failed: %s", name, text)
	}
	if out != nil {
		if err := json.Unmarshal([]byte(text), out); err != nil {
			t.Fatalf("%s decode: %v in %s", name, err, text)
		}
	}
	return text
}

func writeArticle(t *testing.T, s *Server, article map[string]any) (id string, rev int) {
	t.Helper()
	var r struct {
		ID       string `json:"id"`
		Revision int    `json:"revision"`
	}
	mustOK(t, s, "wiki_write", map[string]any{"article": article}, &r)
	return r.ID, r.Revision
}

func TestMCPInitializeAndToolList(t *testing.T) {
	s := testServer(t)
	code, data := request(t, s, http.MethodPost, "/mcp", `{"jsonrpc":"2.0","id":1,"method":"initialize"}`)
	if code != 200 || !strings.Contains(string(data), `"instructions":"AI Wiki workflow`) {
		t.Fatalf("initialize: %d %s", code, data)
	}
	code, data = request(t, s, http.MethodPost, "/mcp", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	var env struct {
		Result struct {
			Tools []struct {
				Name        string `json:"name"`
				Description string `json:"description"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(data, &env); err != nil {
		t.Fatal(err)
	}
	names := []string{}
	for _, tl := range env.Result.Tools {
		names = append(names, tl.Name)
		if tl.Description == "" {
			t.Errorf("%s has no description", tl.Name)
		}
	}
	for _, want := range []string{"wiki_bootstrap", "wiki_search", "wiki_read", "wiki_edit", "wiki_write", "wiki_links", "wiki_history", "wiki_comments", "wiki_restore", "wiki_comment"} {
		if !strings.Contains(strings.Join(names, ","), want) {
			t.Errorf("tool %s missing from %v", want, names)
		}
	}
	// Every property of every tool (and of the nested article) must be described.
	for _, tl := range mcpToolList() {
		props := tl["inputSchema"].(map[string]any)["properties"].(map[string]any)
		for k, v := range props {
			if tl["name"] == "wiki_comment" || tl["name"] == "wiki_comments" || tl["name"] == "wiki_restore" {
				continue
			}
			if _, ok := v.(map[string]any)["description"]; !ok {
				t.Errorf("%s.%s has no description", tl["name"], k)
			}
		}
	}
}

func TestMCPUnknownToolAndBadArguments(t *testing.T) {
	s := testServer(t)
	isErr, text := rpc(t, s, "wiki_nope", map[string]any{})
	if !isErr || !strings.Contains(text, "available tools") || !strings.Contains(text, "wiki_edit") {
		t.Fatalf("unknown tool: %v %s", isErr, text)
	}
	isErr, text = rpc(t, s, "wiki_search", "not an object")
	if !isErr || !strings.Contains(text, "arguments") {
		t.Fatalf("bad arguments: %v %s", isErr, text)
	}
}

func TestMCPWriteCompactAndMerge(t *testing.T) {
	s := testServer(t)
	text := ""
	var created struct {
		ID, Title, SpaceID string
		Revision           int
	}
	text = mustOK(t, s, "wiki_write", map[string]any{"article": map[string]any{"space_id": "general", "title": "Merge note", "body": "BIGBODY", "kind": "instruction", "status": "verified", "pinned": true, "tags": []string{"a"}}}, &created)
	if strings.Contains(text, "BIGBODY") || strings.Contains(text, `"body"`) || created.Revision != 1 {
		t.Fatalf("write response must be compact: %s", text)
	}
	t.Logf("wiki_write: %s", text)
	var upd struct{ Revision int }
	mustOK(t, s, "wiki_write", map[string]any{"id": created.ID, "article": map[string]any{"title": "Merge note 2", "base_revision": 1}}, &upd)
	if upd.Revision != 2 {
		t.Fatalf("revision %d", upd.Revision)
	}
	var a Article
	mustOK(t, s, "wiki_read", map[string]any{"id": created.ID}, &a)
	if a.Status != "verified" || !a.Pinned || a.Kind != "instruction" || a.Body != "BIGBODY" || a.Title != "Merge note 2" {
		t.Fatalf("merge lost fields: %#v", a)
	}
	isErr, text := rpc(t, s, "wiki_write", map[string]any{"id": created.ID, "article": map[string]any{"title": "x", "base_revision": 1}})
	if !isErr || !strings.Contains(text, "revision conflict") {
		t.Fatalf("conflict: %s", text)
	}
}

func TestMCPSearchCompactAndPaging(t *testing.T) {
	s := testServer(t)
	for i := 0; i < 5; i++ {
		writeArticle(t, s, map[string]any{"space_id": "general", "title": "Pagingtoken item " + string(rune('A'+i)), "summary": "sum", "body": "pagingtoken body", "tags": []string{"pg"}})
	}
	var r struct {
		Total      int              `json:"total"`
		Offset     int              `json:"offset"`
		NextOffset int              `json:"next_offset"`
		Results    []map[string]any `json:"results"`
	}
	text := mustOK(t, s, "wiki_search", map[string]any{"q": "pagingtoken", "limit": 2}, &r)
	t.Logf("wiki_search: %s", text)
	if r.Total != 5 || len(r.Results) != 2 || r.NextOffset != 2 {
		t.Fatalf("paging: %s", text)
	}
	if _, has := r.Results[0]["body"]; has {
		t.Fatal("body in results")
	}
	if _, has := r.Results[0]["score"]; !has {
		t.Fatalf("score missing: %s", text)
	}
	mustOK(t, s, "wiki_search", map[string]any{"q": "pagingtoken", "limit": 2, "offset": 4}, &r)
	if len(r.Results) != 1 || text == "" {
		t.Fatalf("last page: %#v", r)
	}
	var last map[string]any
	raw := mustOK(t, s, "wiki_search", map[string]any{"q": "pagingtoken", "limit": 2, "offset": 4}, &last)
	if _, has := last["next_offset"]; has {
		t.Fatalf("next_offset on last page: %s", raw)
	}
	// limit is capped at 100
	mustOK(t, s, "wiki_search", map[string]any{"limit": 1000}, &r)
	if r.Total < 5 {
		t.Fatalf("total %d", r.Total)
	}
}

const outlineBody = "# Main\nintro\n## Данные\nalpha\n```md\n## Ignored\n```\n## Данные\nbeta\n### Sub\nsubtext\n## Конец\nend\n"

func TestMCPReadOutlineAnchorsAndRevision(t *testing.T) {
	s := testServer(t)
	id, rev := writeArticle(t, s, map[string]any{"space_id": "general", "title": "Outline", "summary": "s", "body": outlineBody})
	var o struct {
		Body     *string `json:"body"`
		Chars    int     `json:"chars"`
		Revision int     `json:"revision"`
		Headings []struct {
			Level  int
			Title  string
			Anchor string
			Chars  int
		} `json:"headings"`
	}
	text := mustOK(t, s, "wiki_read", map[string]any{"id": id, "outline": true}, &o)
	t.Logf("outline: %s", text)
	if o.Body != nil || o.Chars != len([]rune(outlineBody)) || len(o.Headings) != 5 {
		t.Fatalf("outline: %s", text)
	}
	wantAnchors := []string{"main", "данные", "данные-2", "sub", "конец"}
	for i, h := range o.Headings {
		if h.Anchor != wantAnchors[i] {
			t.Fatalf("anchor %d = %q, want %q", i, h.Anchor, wantAnchors[i])
		}
		// every outline anchor is accepted by anchored read and chars match the section
		var sec struct {
			Section string `json:"section"`
			Partial bool   `json:"partial"`
		}
		mustOK(t, s, "wiki_read", map[string]any{"id": id, "anchor": h.Anchor}, &sec)
		if !sec.Partial || len([]rune(sec.Section)) != h.Chars {
			t.Fatalf("section %s chars %d != %d", h.Anchor, len([]rune(sec.Section)), h.Chars)
		}
	}
	var sec map[string]any
	text = mustOK(t, s, "wiki_read", map[string]any{"id": id, "anchor": "данные-2"}, &sec)
	t.Logf("anchor read: %s", text)
	if _, has := sec["summary"]; has || sec["partial"] != true || !strings.Contains(sec["section"].(string), "subtext") || strings.Contains(sec["section"].(string), "alpha") {
		t.Fatalf("anchor read: %s", text)
	}
	// historical read
	var upd struct{ Revision int }
	mustOK(t, s, "wiki_edit", map[string]any{"id": id, "base_revision": rev, "op": "replace_text", "old": "intro", "text": "CHANGED"}, &upd)
	var h Article
	var hist struct {
		Article
		Historical bool `json:"historical"`
	}
	mustOK(t, s, "wiki_read", map[string]any{"id": id, "revision": 1}, &hist)
	if !hist.Historical || !strings.Contains(hist.Body, "intro") || hist.Revision != 1 {
		t.Fatalf("historical: %#v", hist)
	}
	mustOK(t, s, "wiki_read", map[string]any{"id": id}, &h)
	if strings.Contains(h.Body, "intro") || h.Revision != 2 {
		t.Fatalf("current: %#v", h)
	}
	if isErr, _ := rpc(t, s, "wiki_read", map[string]any{"id": id, "revision": 99}); !isErr {
		t.Fatal("missing revision accepted")
	}
	if isErr, _ := rpc(t, s, "wiki_read", map[string]any{"id": id, "anchor": "nope"}); !isErr {
		t.Fatal("missing anchor accepted")
	}
}

func TestMCPHistoryCompact(t *testing.T) {
	s := testServer(t)
	id, rev := writeArticle(t, s, map[string]any{"space_id": "general", "title": "Hist", "body": "SECRETBODY"})
	mustOK(t, s, "wiki_edit", map[string]any{"id": id, "base_revision": rev, "op": "append", "text": "more", "author": "bot", "message": "added"}, nil)
	var list []map[string]any
	text := mustOK(t, s, "wiki_history", map[string]any{"id": id}, &list)
	t.Logf("history: %s", text)
	if len(list) != 2 || list[0]["revision"].(float64) != 2 || list[0]["author"] != "bot" || list[0]["message"] != "added" || strings.Contains(text, "SECRETBODY") || strings.Contains(text, "snapshot") {
		t.Fatalf("history: %s", text)
	}
	text = mustOK(t, s, "wiki_history", map[string]any{"id": id, "revision": 1}, nil)
	if !strings.Contains(text, "SECRETBODY") || !strings.Contains(text, "snapshot") {
		t.Fatalf("snapshot: %s", text)
	}
}

func TestMCPLinks(t *testing.T) {
	s := testServer(t)
	parent, _ := writeArticle(t, s, map[string]any{"space_id": "general", "title": "Parent"})
	target, _ := writeArticle(t, s, map[string]any{"space_id": "general", "title": "Target", "parent_id": parent})
	child2, _ := writeArticle(t, s, map[string]any{"space_id": "general", "title": "Zeta child", "parent_id": target})
	child1, _ := writeArticle(t, s, map[string]any{"space_id": "general", "title": "Alpha child", "parent_id": target})
	deleted, drev := writeArticle(t, s, map[string]any{"space_id": "general", "title": "Deleted target"})
	abs, _ := writeArticle(t, s, map[string]any{"space_id": "general", "title": "Absolute linked"})
	angle, _ := writeArticle(t, s, map[string]any{"space_id": "general", "title": "Angle linked"})
	rel, _ := writeArticle(t, s, map[string]any{"space_id": "general", "title": "Relative linked"})
	body := "see [a](http://192.0.2.10:8090/?article=" + abs + "#sec-one) and again [a](/?article=" + abs + ") and <https://wiki.example/?article=" + angle + "> and [r](/?article=" + rel + "#x)\n" +
		"[self](/?article=" + target + ") [gone](/?article=" + deleted + ") [none](/?article=no-such)"
	mustOK(t, s, "wiki_write", map[string]any{"id": target, "article": map[string]any{"body": body, "base_revision": 1}}, nil)
	if code, b := request(t, s, "DELETE", "/api/articles/"+deleted, `{"base_revision":`+string(rune('0'+drev))+`}`); code >= 300 {
		t.Fatalf("delete: %d %s", code, b)
	}
	inc, _ := writeArticle(t, s, map[string]any{"space_id": "general", "title": "Incoming", "body": "x [t](/?article=" + target + "#a) y [t](http://h:1/?article=" + target + ")"})
	writeArticle(t, s, map[string]any{"space_id": "general", "title": "Not incoming", "body": "mentions " + target + " plain and /?article=" + target + "extra-no"})

	var r struct {
		Article  map[string]string `json:"article"`
		Parent   *compactArticle   `json:"parent"`
		Children []compactArticle  `json:"children"`
		Outgoing []struct {
			ID     string `json:"id"`
			Anchor string `json:"anchor"`
		} `json:"outgoing"`
		Incoming []compactArticle `json:"incoming"`
	}
	text := mustOK(t, s, "wiki_links", map[string]any{"id": target}, &r)
	t.Logf("wiki_links: %s", text)
	if r.Parent == nil || r.Parent.ID != parent || len(r.Children) != 2 || r.Children[0].ID != child1 || r.Children[1].ID != child2 {
		t.Fatalf("parent/children: %s", text)
	}
	if len(r.Outgoing) != 3 || r.Outgoing[0].ID != abs || r.Outgoing[0].Anchor != "sec-one" || r.Outgoing[1].ID != angle || r.Outgoing[2].ID != rel || r.Outgoing[2].Anchor != "x" {
		t.Fatalf("outgoing: %s", text)
	}
	if len(r.Incoming) != 1 || r.Incoming[0].ID != inc {
		t.Fatalf("incoming: %s", text)
	}
	var top struct {
		Parent json.RawMessage `json:"parent"`
	}
	mustOK(t, s, "wiki_links", map[string]any{"id": parent}, &top)
	if string(top.Parent) != "null" {
		t.Fatalf("top-level parent should be null: %s", top.Parent)
	}
	// REST keeps its shape and has no duplicates.
	code, b := request(t, s, "GET", "/api/articles/"+target+"/links", "")
	var rest struct {
		Outgoing, Incoming []Article
	}
	if code != 200 || json.Unmarshal(b, &rest) != nil || len(rest.Outgoing) != 3 || len(rest.Incoming) != 1 {
		t.Fatalf("REST links: %d %s", code, b)
	}
	if isErr, _ := rpc(t, s, "wiki_links", map[string]any{"id": "missing"}); !isErr {
		t.Fatal("missing article accepted")
	}
}

func TestMCPBootstrapScopes(t *testing.T) {
	s := testServer(t)
	writeArticle(t, s, map[string]any{"space_id": "engineering", "title": "Eng rule", "body": "ENGSECRETBODY", "pinned": true, "kind": "instruction", "status": "active"})
	writeArticle(t, s, map[string]any{"space_id": "general", "title": "Fresh general", "body": "x"})
	var r struct {
		Instructions []Article        `json:"instructions"`
		OtherPinned  []compactArticle `json:"other_pinned"`
		Spaces       []Space          `json:"spaces"`
		Recent       []compactArticle `json:"recent"`
		Guide        string           `json:"guide"`
	}
	text := mustOK(t, s, "wiki_bootstrap", map[string]any{}, &r)
	if strings.Contains(text, "ENGSECRETBODY") || len(r.OtherPinned) != 1 || r.OtherPinned[0].Title != "Eng rule" || !strings.Contains(r.Guide, "space_id") || len(r.Spaces) == 0 {
		t.Fatalf("bootstrap w/o space: %s", text)
	}
	inInstr := map[string]bool{}
	for _, a := range r.Instructions {
		if a.SpaceID != "general" || !a.Pinned || a.Body == "" {
			t.Fatalf("instruction: %#v", a)
		}
		inInstr[a.ID] = true
	}
	if len(r.Instructions) < 3 {
		t.Fatalf("instructions: %d", len(r.Instructions))
	}
	for _, a := range r.Recent {
		if inInstr[a.ID] {
			t.Fatalf("recent duplicates instruction %s", a.ID)
		}
	}
	if len(r.Recent) == 0 {
		t.Fatal("recent empty")
	}
	t.Logf("bootstrap (truncated): %.600s", text)
	var r2 struct {
		Instructions []Article `json:"instructions"`
		OtherPinned  []any     `json:"other_pinned"`
	}
	text = mustOK(t, s, "wiki_bootstrap", map[string]any{"space_id": "engineering"}, &r2)
	if !strings.Contains(text, "ENGSECRETBODY") || r2.OtherPinned != nil {
		t.Fatalf("bootstrap with space: %.300s", text)
	}
}

func TestMarkdownSlugMatchesUI(t *testing.T) {
	for in, want := range map[string]string{
		"wg_quick setup": "wg-quick-setup",
		"a~b":            "a-b",
		"Title **важно** [материал](https://x)": "title-важно-материал",
		"`code` and ![alt text](img.png)":       "code-and-alt-text",
		"???":                                   "section",
		"":                                      "section",
		"  C# и Go!  ":                          "c-и-go",
	} {
		if got := markdownSlug(in); got != want {
			t.Errorf("markdownSlug(%q) = %q, want %q", in, got, want)
		}
	}
	hs := parseMarkdownHeadings(strings.Split("## ???\n\ntext\n\n## !!!\n\n## Section\n\n## C# ##\n", "\n"))
	var got []string
	for _, h := range hs {
		got = append(got, h.slug)
	}
	if strings.Join(got, ",") != "section,section-2,section-3,c" {
		t.Fatalf("slugs = %v", got)
	}
	if hs[3].title != "C#" {
		t.Fatalf("closing sequence not stripped correctly: %q", hs[3].title)
	}
}

func headingSlugs(body string) string {
	var out []string
	for _, h := range parseMarkdownHeadings(strings.Split(body, "\n")) {
		out = append(out, h.slug)
	}
	return strings.Join(out, ",")
}

func TestMarkdownHeadingsCommonMark(t *testing.T) {
	for name, tc := range map[string]struct{ body, want string }{
		"list item then thematic break": {"# A\n\n- item\n---\n\ntext\n\n## B", "a,b"},
		"indented not heading":          {"    # not heading\n\n# Real", "real"},
		"tab not heading":               {"\t# not heading\n# Real", "real"},
		"three spaces ok":               {"   ## Ok", "ok"},
		"setext h1 and h2":              {"Title\n=====\n\ntext\n\nSub\n---\n", "title,sub"},
		"setext indent and trailing":    {"Title\n  ===  \n", "title"},
		"hr after blank is break":       {"text\n\n---\n\n# H", "h"},
		"quote then rule":               {"> quoted\n---\n# H", "h"},
		"table then rule":               {"| a | b |\n---\n# H", "h"},
		"ordered list then rule":        {"1. one\n---\n# H", "h"},
		"atx before underline":          {"# H\n---\n", "h"},
		"fence then rule":               {"```\ncode\n```\n---\n# H", "h"},
		"heading in fence ignored":      {"```\n# no\n```\n# yes", "yes"},
		"multi-line paragraph setext":   {"first\nsecond\n---\n", "first-second"},
	} {
		if got := headingSlugs(tc.body); got != tc.want {
			t.Errorf("%s: slugs %q, want %q", name, got, tc.want)
		}
	}
}

func TestMCPStrictArguments(t *testing.T) {
	s := testServer(t)
	id, rev := writeArticle(t, s, map[string]any{"space_id": "general", "title": "Strict", "body": "# Top\nkeep me\n"})
	for _, tc := range []struct {
		tool string
		args map[string]any
		want string
	}{
		{"wiki_edit", map[string]any{"id": id, "base_revision": rev, "op": "replace_text", "old": "keep me", "text": 123}, "text must be a string"},
		{"wiki_edit", map[string]any{"id": id, "base_revision": "1", "op": "append", "text": "x"}, "base_revision must be a integer"},
		{"wiki_edit", map[string]any{"id": id, "base_revision": rev, "op": 5}, "op must be a string"},
		{"wiki_search", map[string]any{"q": 5}, "q must be a string"},
		{"wiki_search", map[string]any{"limit": "5"}, "limit must be a integer"},
		{"wiki_search", map[string]any{"pinned": "yes"}, "pinned must be a boolean"},
		{"wiki_search", map[string]any{"tag": []any{"a", 1}}, "tag must be a string or an array of strings"},
		{"wiki_search", map[string]any{"property": []any{nil}}, "property must be a string or an array of strings"},
		{"wiki_search", map[string]any{"tag": 7}, "tag must be a string or an array of strings"},
		{"wiki_read", map[string]any{"id": id, "outline": "true"}, "outline must be a boolean"},
		{"wiki_read", map[string]any{"id": id, "anchor": 1}, "anchor must be a string"},
		{"wiki_read", map[string]any{"id": id, "revision": "1"}, "revision must be a integer"},
		{"wiki_history", map[string]any{"id": id, "revision": true}, "revision must be a integer"},
		{"wiki_bootstrap", map[string]any{"space_id": 1}, "space_id must be a string"},
	} {
		isErr, text := rpc(t, s, tc.tool, tc.args)
		if !isErr || !strings.Contains(text, tc.want) {
			t.Errorf("%s %v: isErr=%v %s, want %q", tc.tool, tc.args, isErr, text, tc.want)
		}
	}
	// the rejected edit must not have touched the body
	if body, r := readBody(t, s, id); body != "# Top\nkeep me\n" || r != rev {
		t.Fatalf("body changed by rejected call: %q rev %d", body, r)
	}
	// null and absent keys keep defaults
	mustOK(t, s, "wiki_search", map[string]any{"q": nil, "limit": nil, "tag": nil, "property": []any{}, "pinned": nil}, nil)
	mustOK(t, s, "wiki_search", map[string]any{"tag": "x", "property": []string{"a:b"}}, nil)
}

func TestMCPDeletedMarkers(t *testing.T) {
	s := testServer(t)
	id, rev := writeArticle(t, s, map[string]any{"space_id": "general", "title": "Gone soon", "body": "# H\ntext\n"})
	mustOK(t, s, "wiki_edit", map[string]any{"id": id, "base_revision": rev, "op": "append", "text": "more"}, nil)
	var live map[string]any
	mustOK(t, s, "wiki_read", map[string]any{"id": id, "outline": true}, &live)
	if _, has := live["deleted_at"]; has {
		t.Fatalf("live article has deleted_at: %v", live)
	}
	if code, b := request(t, s, "DELETE", "/api/articles/"+id, `{"base_revision":2}`); code >= 300 {
		t.Fatalf("delete: %d %s", code, b)
	}
	for name, args := range map[string]map[string]any{
		"outline": {"id": id, "outline": true},
		"anchor":  {"id": id, "anchor": "h"},
		"links":   {"id": id},
	} {
		tool := "wiki_read"
		if name == "links" {
			tool = "wiki_links"
		}
		var out map[string]any
		text := mustOK(t, s, tool, args, &out)
		if name == "links" {
			a, _ := out["article"].(map[string]any)
			if a == nil || a["deleted_at"] == nil || a["deleted_at"] == "" {
				t.Errorf("links: %s", text)
			}
		} else if out["deleted_at"] == nil || out["deleted_at"] == "" {
			t.Errorf("%s: no deleted_at: %s", name, text)
		}
	}
	var hist map[string]any
	text := mustOK(t, s, "wiki_read", map[string]any{"id": id, "revision": 1}, &hist)
	if hist["historical"] != true || hist["current_deleted_at"] == nil || hist["current_deleted_at"] == "" {
		t.Errorf("revision read: %s", text)
	}
}

func TestMCPWriteArticleSchemaHasNoRequired(t *testing.T) {
	code, body := request(t, testServer(t), http.MethodPost, "/mcp", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	if code != http.StatusOK {
		t.Fatalf("tools/list: %d %s", code, body)
	}
	var env struct {
		Result struct {
			Tools []struct {
				Name   string `json:"name"`
				Schema struct {
					Required   []string                   `json:"required"`
					Properties map[string]json.RawMessage `json:"properties"`
				} `json:"inputSchema"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatal(err)
	}
	for _, tl := range env.Result.Tools {
		if tl.Name != "wiki_write" {
			continue
		}
		var article map[string]json.RawMessage
		if err := json.Unmarshal(tl.Schema.Properties["article"], &article); err != nil {
			t.Fatal(err)
		}
		if _, has := article["required"]; has {
			t.Fatalf("article schema must not declare required: %s", article["required"])
		}
		if len(tl.Schema.Required) != 1 || tl.Schema.Required[0] != "article" {
			t.Fatalf("wiki_write required = %v", tl.Schema.Required)
		}
		return
	}
	t.Fatal("wiki_write not listed")
}
