package server

import (
	"encoding/json"
	"testing"
)

func mcpCommentTool(t *testing.T, s *Server, args string) (bool, string) {
	t.Helper()
	params := map[string]json.RawMessage{
		"name":      json.RawMessage(`"wiki_comments"`),
		"arguments": json.RawMessage(args),
	}
	raw, err := json.Marshal(s.callTool(params))
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		IsError bool `json:"isError"`
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Content) != 1 {
		t.Fatalf("unexpected MCP result: %s", raw)
	}
	return result.IsError, result.Content[0].Text
}

func addMCPCommentArticle(t *testing.T, s *Server, id string) {
	t.Helper()
	a := Article{ID: id, SpaceID: "general", Title: id, Kind: "article", Status: "active", Tags: []string{}, Properties: map[string]string{}, Revision: 1, CreatedAt: stamp(), UpdatedAt: stamp(), Author: "fixture"}
	tx, err := s.DB.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err := insertArticle(tx, a, "fixture"); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func TestMCPWikiCommentsToolListingSchema(t *testing.T) {
	var found bool
	for _, tool := range mcpToolList() {
		if tool["name"] != "wiki_comments" {
			continue
		}
		found = true
		input, ok := tool["inputSchema"].(map[string]any)
		if !ok || input["type"] != "object" {
			t.Fatalf("invalid input schema: %#v", tool["inputSchema"])
		}
		required, ok := input["required"].([]string)
		if !ok || len(required) != 1 || required[0] != "id" {
			t.Fatalf("id must be required, got %#v", input["required"])
		}
		properties, ok := input["properties"].(map[string]any)
		if !ok || len(properties) != 1 {
			t.Fatalf("expected only the article id argument, got %#v", input["properties"])
		}
		idSchema, ok := properties["id"].(map[string]any)
		if !ok || idSchema["type"] != "string" {
			t.Fatalf("id must be a string: %#v", properties["id"])
		}
	}
	if !found {
		t.Fatal("wiki_comments missing from MCP tool list")
	}
}

func TestMCPWikiCommentsReturnsChronologicalArticleComments(t *testing.T) {
	s := testServer(t)
	addMCPCommentArticle(t, s, "mcp-comment-article")
	addMCPCommentArticle(t, s, "mcp-comment-other")
	want := []Comment{
		{ID: "mcp-comment-later", ArticleID: "mcp-comment-article", Anchor: "heading-a", Body: "**Markdown** after", Author: "Alice", CreatedAt: "2026-10-10T12:30:00Z", Resolved: true},
		{ID: "mcp-comment-earlier", ArticleID: "mcp-comment-article", Anchor: "heading-b", Body: "earlier comment", Author: "Bob", CreatedAt: "2026-10-10T12:00:00Z", Resolved: false},
	}
	for _, c := range want {
		if _, err := s.DB.Exec("INSERT INTO comments(id,article_id,anchor,body,author,created_at,resolved) VALUES(?,?,?,?,?,?,?)", c.ID, c.ArticleID, c.Anchor, c.Body, c.Author, c.CreatedAt, boolInt(c.Resolved)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.DB.Exec("INSERT INTO comments(id,article_id,anchor,body,author,created_at,resolved) VALUES(?,?,?,?,?,?,?)", "mcp-comment-foreign", "mcp-comment-other", "other", "must not leak", "Eve", "2026-10-10T11:00:00Z", 0); err != nil {
		t.Fatal(err)
	}

	isError, text := mcpCommentTool(t, s, `{"id":"mcp-comment-article"}`)
	if isError {
		t.Fatalf("wiki_comments failed: %s", text)
	}
	var got []Comment
	if err := json.Unmarshal([]byte(text), &got); err != nil {
		t.Fatalf("decode comment array %q: %v", text, err)
	}
	// The REST contract orders by created_at ascending and keeps full comment metadata.
	if len(got) != 2 || got[0].ID != "mcp-comment-earlier" || got[1].ID != "mcp-comment-later" {
		t.Fatalf("unexpected order or cross-article results: %#v", got)
	}
	if got[0].ArticleID != want[1].ArticleID || got[0].Anchor != want[1].Anchor || got[0].Body != want[1].Body || got[0].Author != want[1].Author || got[0].CreatedAt != want[1].CreatedAt || got[0].Resolved {
		t.Fatalf("earlier comment metadata mismatch: %#v", got[0])
	}
	if got[1] != want[0] {
		t.Fatalf("resolved Markdown comment metadata mismatch: got %#v want %#v", got[1], want[0])
	}

	addMCPCommentArticle(t, s, "mcp-comment-empty")
	isError, text = mcpCommentTool(t, s, `{"id":"mcp-comment-empty"}`)
	var empty []Comment
	if err := json.Unmarshal([]byte(text), &empty); err != nil {
		t.Fatalf("decode empty comments %q: %v", text, err)
	}
	if isError || empty == nil || len(empty) != 0 {
		t.Fatalf("empty article comments should return [], got isError=%t text=%q", isError, text)
	}
}

func TestMCPWikiCommentsRejectsMissingInvalidAndUnknownIDs(t *testing.T) {
	s := testServer(t)
	cases := []struct {
		name string
		args string
	}{
		{"missing id", `{}`},
		{"invalid id", `{"id":"bad/id"}`},
		{"nonexistent article", `{"id":"no-such-article"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			isError, text := mcpCommentTool(t, s, tc.args)
			if !isError {
				t.Fatalf("expected MCP error, got %q", text)
			}
		})
	}
}
