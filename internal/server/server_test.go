package server

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testServer(t *testing.T) *Server {
	t.Helper()
	s, e := Open(t.TempDir(), "")
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestSubstringSearchAndTagsEndpoint(t *testing.T) {
	s := testServer(t)
	article := Article{ID: "substring-fixture", SpaceID: "general", Title: "Северный СЕГМЕНТ", Summary: "second-token", Body: "body has МЕТЕОРИТЫ inside a word", Kind: "article", Status: "active", Tags: []string{}, Properties: map[string]string{}, Revision: 1, CreatedAt: stamp(), UpdatedAt: stamp(), Author: "test"}
	tx, err := s.DB.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err := insertArticle(tx, article, "fixture"); err != nil {
		t.Fatal(err)
	}
	var tags []string
	for i := 0; i < 205; i++ {
		tags = append(tags, fmt.Sprintf("tag-%03d", i))
	}
	article.ID = "many-tags"
	article.Title = "Tags"
	article.Tags = tags
	article.Body = "no search match"
	if err := insertArticle(tx, article, "fixture"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	code, body := request(t, s, "GET", "/api/articles?q=%D0%BC%D0%B5%D1%82%D0%B5%D0%BE%D1%80%D0%B8%D1%82%D1%8B%20second-token", "")
	if code != 200 {
		t.Fatalf("substring search: %d %s", code, body)
	}
	var found []Article
	if err := json.Unmarshal(body, &found); err != nil {
		t.Fatal(err)
	}
	if len(found) != 1 || found[0].ID != "substring-fixture" {
		t.Fatalf("AND substring search results: %#v", found)
	}
	code, body = request(t, s, "GET", "/api/articles?q=%D1%81%D0%B5%D0%B3%D0%BC%D0%B5%D0%BD%D1%82", "")
	if code != 200 || !strings.Contains(string(body), "substring-fixture") {
		t.Fatalf("case insensitive middle-of-word title search: %d %s", code, body)
	}
	code, body = request(t, s, "GET", "/api/tags?space_id=general", "")
	if code != 200 {
		t.Fatalf("tags: %d %s", code, body)
	}
	var foundTags []string
	if err := json.Unmarshal(body, &foundTags); err != nil {
		t.Fatal(err)
	}
	if len(foundTags) != 205 || foundTags[204] != "tag-204" {
		t.Fatalf("expected all sorted tags, got %d: %#v", len(foundTags), foundTags[len(foundTags)-1])
	}
}

func TestAttachmentSoftDeleteIsScopedAndRetainsBytes(t *testing.T) {
	s := testServer(t)
	for _, id := range []string{"owner-a", "owner-b"} {
		a := Article{ID: id, SpaceID: "general", Title: id, Kind: "article", Status: "active", Tags: []string{}, Properties: map[string]string{}, Revision: 1, CreatedAt: stamp(), UpdatedAt: stamp(), Author: "test"}
		tx, _ := s.DB.Begin()
		if err := insertArticle(tx, a, "fixture"); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	stored := "retained.bin"
	if err := os.WriteFile(filepath.Join(s.DataDir, "files", stored), []byte("kept bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec("INSERT INTO attachments(id,article_id,name,mime,size,stored_name,created_at) VALUES(?,?,?,?,?,?,?)", "file-a", "owner-a", "original.txt", "text/plain", 10, stored, stamp()); err != nil {
		t.Fatal(err)
	}
	code, _ := request(t, s, http.MethodDelete, "/api/articles/owner-b/attachments/file-a", "")
	if code != 404 {
		t.Fatalf("cross-owner deletion returned %d", code)
	}
	code, _ = request(t, s, http.MethodDelete, "/api/articles/owner-a/attachments/file-a", "")
	if code != 204 {
		t.Fatalf("delete returned %d", code)
	}
	code, _ = request(t, s, http.MethodDelete, "/api/articles/owner-a/attachments/file-a", "")
	if code != 204 {
		t.Fatalf("repeat delete returned %d", code)
	}
	code, body := request(t, s, http.MethodGet, "/api/articles/owner-a/attachments", "")
	if code != 200 || strings.Contains(string(body), "file-a") {
		t.Fatalf("deleted attachment listed: %d %s", code, body)
	}
	code, _ = request(t, s, http.MethodGet, "/files/file-a", "")
	if code != 404 {
		t.Fatalf("deleted file fetch returned %d", code)
	}
	bytes, err := os.ReadFile(filepath.Join(s.DataDir, "files", stored))
	if err != nil || string(bytes) != "kept bytes" {
		t.Fatalf("file bytes not retained: %q %v", bytes, err)
	}
	code, body = request(t, s, http.MethodGet, "/api/export", "")
	if code != 200 || !strings.Contains(string(body), "deleted_at") || !strings.Contains(string(body), "file-a") {
		t.Fatalf("export missing deletion marker: %d %s", code, body)
	}
}

func TestAttachmentDeletedAtMigrationPreservesExistingRows(t *testing.T) {
	s := testServer(t)
	if _, err := s.DB.Exec(`DROP TABLE attachments; CREATE TABLE attachments(id TEXT PRIMARY KEY,article_id TEXT NOT NULL REFERENCES articles(id),name TEXT NOT NULL,mime TEXT NOT NULL,size INTEGER NOT NULL,stored_name TEXT NOT NULL,created_at TEXT NOT NULL); INSERT INTO attachments(id,article_id,name,mime,size,stored_name,created_at) VALUES('legacy-file','start-here','legacy.txt','text/plain',4,'legacy.txt','2026-10-08T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if err := s.migrate(); err != nil {
		t.Fatalf("migrate old schema: %v", err)
	}
	var name string
	var deleted sql.NullString
	if err := s.DB.QueryRow("SELECT name,deleted_at FROM attachments WHERE id='legacy-file'").Scan(&name, &deleted); err != nil {
		t.Fatal(err)
	}
	if name != "legacy.txt" || deleted.Valid {
		t.Fatalf("existing attachment changed during migration: name=%q deleted=%v", name, deleted)
	}
}

func TestArticleDeletedAtMigrationPreservesExistingRows(t *testing.T) {
	s := testServer(t)
	if _, err := s.DB.Exec(`PRAGMA foreign_keys=OFF; DROP TABLE articles; CREATE TABLE articles(id TEXT PRIMARY KEY,space_id TEXT NOT NULL REFERENCES spaces(id),parent_id TEXT NOT NULL DEFAULT '',title TEXT NOT NULL,summary TEXT NOT NULL DEFAULT '',body TEXT NOT NULL DEFAULT '',kind TEXT NOT NULL,status TEXT NOT NULL,pinned INTEGER NOT NULL DEFAULT 0,tags TEXT NOT NULL DEFAULT '[]',properties TEXT NOT NULL DEFAULT '{}',revision INTEGER NOT NULL,created_at TEXT NOT NULL,updated_at TEXT NOT NULL,author TEXT NOT NULL); INSERT INTO articles VALUES('legacy-article','general','','Legacy','summary','body','article','active',0,'[]','{}',1,'2026-10-08T00:00:00Z','2026-10-08T00:00:00Z','test')`); err != nil {
		t.Fatal(err)
	}
	if err := s.migrate(); err != nil {
		t.Fatalf("migrate old article schema: %v", err)
	}
	a, err := s.articleByID(s.DB, "legacy-article")
	if err != nil || a.Title != "Legacy" || a.DeletedAt != "" {
		t.Fatalf("existing article changed during migration: %#v %v", a, err)
	}
}

func TestArticleSoftDeleteConflictChildrenHistoryAndUndelete(t *testing.T) {
	s := testServer(t)
	create := func(title, parent string) Article {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"space_id": "general", "parent_id": parent, "title": title, "kind": "article", "status": "active", "tags": []string{"soft-delete-tag"}, "author": "tester"})
		code, b := request(t, s, http.MethodPost, "/api/articles", string(body))
		if code != 201 {
			t.Fatalf("create: %d %s", code, b)
		}
		var a Article
		if err := json.Unmarshal(b, &a); err != nil {
			t.Fatal(err)
		}
		return a
	}
	parent := create("delete parent", "")
	child := create("delete child", parent.ID)
	if _, err := s.DB.Exec(`UPDATE articles SET tags='["soft-delete-only"]' WHERE id=?`, child.ID); err != nil {
		t.Fatal(err)
	}
	deleteWith := func(a Article, revision int) (int, []byte) {
		body, _ := json.Marshal(map[string]any{"base_revision": revision, "author": "tester"})
		return request(t, s, http.MethodDelete, "/api/articles/"+a.ID, string(body))
	}
	code, b := deleteWith(parent, 1)
	if code != 409 || !strings.Contains(string(b), "дочерние") {
		t.Fatalf("parent with child: %d %s", code, b)
	}
	code, b = deleteWith(child, 99)
	if code != 409 {
		t.Fatalf("stale delete: %d %s", code, b)
	}
	code, b = deleteWith(child, 1)
	if code != 200 {
		t.Fatalf("delete child: %d %s", code, b)
	}
	var deleted Article
	if err := json.Unmarshal(b, &deleted); err != nil {
		t.Fatal(err)
	}
	if deleted.Revision != 2 || deleted.DeletedAt == "" {
		t.Fatalf("delete snapshot: %#v", deleted)
	}
	code, b = request(t, s, http.MethodGet, "/api/articles/"+child.ID, "")
	if code != 200 || !strings.Contains(string(b), "deleted_at") {
		t.Fatalf("direct recovery read: %d %s", code, b)
	}
	code, b = request(t, s, http.MethodGet, "/api/articles?q=delete%20child", "")
	if code != 200 || strings.Contains(string(b), child.ID) {
		t.Fatalf("deleted article in search: %d %s", code, b)
	}
	code, b = request(t, s, http.MethodGet, "/api/tags?space_id=general", "")
	if code != 200 || strings.Contains(string(b), "soft-delete-only") {
		t.Fatalf("deleted tag remained: %d %s", code, b)
	}
	code, b = request(t, s, http.MethodPut, "/api/articles/"+child.ID, `{"space_id":"general","title":"changed","kind":"article","status":"active","base_revision":2}`)
	if code != 409 {
		t.Fatalf("PUT on deleted article: %d %s", code, b)
	}
	code, b = request(t, s, http.MethodPost, "/api/articles/"+child.ID+"/restore", `{"revision":1,"base_revision":2}`)
	if code != 409 {
		t.Fatalf("restore on deleted article: %d %s", code, b)
	}
	code, b = request(t, s, http.MethodGet, "/api/articles/"+child.ID+"/history", "")
	if code != 200 {
		t.Fatalf("history: %d %s", code, b)
	}
	var history []Revision
	if err := json.Unmarshal(b, &history); err != nil {
		t.Fatal(err)
	}
	if len(history) != 2 || history[0].Snapshot.DeletedAt == "" {
		t.Fatalf("delete revision missing: %#v", history)
	}
	code, b = request(t, s, http.MethodPost, "/api/articles/"+child.ID+"/undelete", `{"base_revision":2,"author":"tester"}`)
	if code != 200 {
		t.Fatalf("undelete: %d %s", code, b)
	}
	var restored Article
	if err := json.Unmarshal(b, &restored); err != nil {
		t.Fatal(err)
	}
	if restored.Revision != 3 || restored.DeletedAt != "" {
		t.Fatalf("undelete result: %#v", restored)
	}
	code, b = request(t, s, http.MethodPost, "/api/articles/"+child.ID+"/restore", `{"revision":2,"base_revision":3}`)
	if code != 200 {
		t.Fatalf("restore historic deleted snapshot: %d %s", code, b)
	}
	var contentRestored Article
	if err := json.Unmarshal(b, &contentRestored); err != nil {
		t.Fatal(err)
	}
	if contentRestored.DeletedAt != "" {
		t.Fatalf("content restore changed lifecycle: %#v", contentRestored)
	}
	code, b = deleteWith(child, 4)
	if code != 200 {
		t.Fatalf("delete child again: %d %s", code, b)
	}
	code, b = deleteWith(parent, 1)
	if code != 200 {
		t.Fatalf("delete parent after child: %d %s", code, b)
	}
	code, b = request(t, s, http.MethodPost, "/api/articles/"+child.ID+"/undelete", `{"base_revision":5}`)
	if code != 409 {
		t.Fatalf("undelete beneath deleted parent: %d %s", code, b)
	}
}
func request(t *testing.T, s *Server, m, p, b string) (int, []byte) {
	t.Helper()
	r := httptest.NewRequest(m, p, bytes.NewBufferString(b))
	r.Host = "127.0.0.1:8090"
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	return w.Code, w.Body.Bytes()
}
func TestSeedAndLiteralFTSSearch(t *testing.T) {
	s := testServer(t)
	code, b := request(t, s, "GET", "/api/spaces", "")
	if code != 200 {
		t.Fatalf("spaces: %d %s", code, b)
	}
	var spaces []Space
	if e := json.Unmarshal(b, &spaces); e != nil {
		t.Fatal(e)
	}
	if len(spaces) != 3 {
		t.Fatalf("expected three seeded spaces, got %d", len(spaces))
	}
	code, b = request(t, s, "GET", "/api/articles?q=безуспешные%20проверки", "")
	if code != 200 {
		t.Fatalf("search: %d %s", code, b)
	}
	var found []Article
	if e := json.Unmarshal(b, &found); e != nil {
		t.Fatal(e)
	}
	if len(found) == 0 || found[0].Body != "" {
		t.Fatalf("expected compact seed search result: %#v", found)
	}
	code, b = request(t, s, "GET", "/api/articles?q=NOT%20%28", "")
	if code != 200 {
		t.Fatalf("query should be treated literally: %d %s", code, b)
	}
}
func TestRevisionConflictAndRestoreAreImmutable(t *testing.T) {
	s := testServer(t)
	input := "{\"space_id\":\"general\",\"title\":\"Evidence note\",\"summary\":\"summary\",\"body\":\"first body\",\"kind\":\"research\",\"status\":\"draft\",\"tags\":[],\"properties\":{},\"author\":\"human\",\"message\":\"created\"}"
	code, b := request(t, s, http.MethodPost, "/api/articles", input)
	if code != 201 {
		t.Fatalf("create: %d %s", code, b)
	}
	var first Article
	if e := json.Unmarshal(b, &first); e != nil {
		t.Fatal(e)
	}
	update := "{\"space_id\":\"general\",\"title\":\"Evidence note\",\"summary\":\"summary\",\"body\":\"revised body\",\"kind\":\"research\",\"status\":\"active\",\"tags\":[],\"properties\":{},\"base_revision\":1,\"author\":\"human\",\"message\":\"edited\"}"
	code, b = request(t, s, http.MethodPut, "/api/articles/"+first.ID, update)
	if code != 200 {
		t.Fatalf("update: %d %s", code, b)
	}
	var second Article
	_ = json.Unmarshal(b, &second)
	if second.Revision != 2 {
		t.Fatalf("revision=%d want 2", second.Revision)
	}
	code, b = request(t, s, http.MethodPut, "/api/articles/"+first.ID, update)
	if code != 409 {
		t.Fatalf("stale update=%d: %s", code, b)
	}
	restore := "{\"revision\":1,\"base_revision\":2,\"author\":\"human\",\"message\":\"restore original\"}"
	code, b = request(t, s, http.MethodPost, "/api/articles/"+first.ID+"/restore", restore)
	if code != 200 {
		t.Fatalf("restore: %d %s", code, b)
	}
	var restored Article
	_ = json.Unmarshal(b, &restored)
	if restored.Revision != 3 || restored.Body != "first body" {
		t.Fatalf("restore result: %#v", restored)
	}
	code, b = request(t, s, "GET", "/api/articles/"+first.ID+"/history", "")
	if code != 200 {
		t.Fatalf("history: %d %s", code, b)
	}
	var history []Revision
	if e := json.Unmarshal(b, &history); e != nil {
		t.Fatal(e)
	}
	if len(history) != 3 || history[0].Revision != 3 || history[1].Revision != 2 || history[2].Revision != 1 {
		t.Fatalf("history incomplete: %#v", history)
	}
}
func TestCrossSpaceParentRejected(t *testing.T) {
	s := testServer(t)
	code, b := request(t, s, http.MethodPost, "/api/articles", "{\"space_id\":\"general\",\"title\":\"root\",\"kind\":\"article\",\"status\":\"draft\"}")
	if code != 201 {
		t.Fatalf("root: %d %s", code, b)
	}
	var a Article
	_ = json.Unmarshal(b, &a)
	body := "{\"space_id\":\"genealogy\",\"parent_id\":\"" + a.ID + "\",\"title\":\"child\",\"kind\":\"article\",\"status\":\"draft\"}"
	code, b = request(t, s, http.MethodPost, "/api/articles", body)
	if code != 400 {
		t.Fatalf("cross-space parent accepted: %d %s", code, b)
	}
}

func TestIncomingLinksDoNotDeadlockSingleConnection(t *testing.T) {
	s := testServer(t)
	code, body := request(t, s, http.MethodPost, "/api/articles", "{\"space_id\":\"general\",\"title\":\"Target\",\"kind\":\"article\",\"status\":\"draft\"}")
	if code != http.StatusCreated {
		t.Fatalf("target create: %d %s", code, body)
	}
	var target Article
	if err := json.Unmarshal(body, &target); err != nil {
		t.Fatal(err)
	}
	sourceBody := fmt.Sprintf("{\"space_id\":\"general\",\"title\":\"Source\",\"body\":\"[target](/?article=%s)\",\"kind\":\"article\",\"status\":\"draft\"}", target.ID)
	code, body = request(t, s, http.MethodPost, "/api/articles", sourceBody)
	if code != http.StatusCreated {
		t.Fatalf("source create: %d %s", code, body)
	}
	var source Article
	if err := json.Unmarshal(body, &source); err != nil {
		t.Fatal(err)
	}
	type response struct {
		code int
		body []byte
	}
	done := make(chan response, 1)
	go func() {
		code, body := request(t, s, http.MethodGet, "/api/articles/"+target.ID+"/links", "")
		done <- response{code, body}
	}()
	select {
	case got := <-done:
		if got.code != http.StatusOK {
			t.Fatalf("links status=%d body=%s", got.code, got.body)
		}
		var result struct {
			Incoming []Article `json:"incoming"`
		}
		if err := json.Unmarshal(got.body, &result); err != nil {
			t.Fatal(err)
		}
		if len(result.Incoming) != 1 || result.Incoming[0].ID != source.ID {
			t.Fatalf("incoming links: %#v", result)
		}
	case <-time.After(time.Second):
		t.Fatal("links request timed out; likely SQLite connection deadlock")
	}
}

func TestParentMovesWithItsChildren(t *testing.T) {
	s := testServer(t)
	code, body := request(t, s, http.MethodPost, "/api/articles", "{\"space_id\":\"general\",\"title\":\"Parent\",\"kind\":\"article\",\"status\":\"draft\"}")
	if code != http.StatusCreated {
		t.Fatalf("parent create: %d %s", code, body)
	}
	var parent Article
	_ = json.Unmarshal(body, &parent)
	child := fmt.Sprintf("{\"space_id\":\"general\",\"parent_id\":%q,\"title\":\"Child\",\"kind\":\"article\",\"status\":\"draft\"}", parent.ID)
	code, body = request(t, s, http.MethodPost, "/api/articles", child)
	if code != http.StatusCreated {
		t.Fatalf("child create: %d %s", code, body)
	}
	move := "{\"space_id\":\"engineering\",\"title\":\"Parent\",\"kind\":\"article\",\"status\":\"draft\",\"base_revision\":1}"
	code, body = request(t, s, http.MethodPut, "/api/articles/"+parent.ID, move)
	if code != http.StatusOK {
		t.Fatalf("parent move: %d %s", code, body)
	}
}

func TestBootstrapIncludesPinnedMaterialAndScopesRecent(t *testing.T) {
	s := testServer(t)
	create := func(space, title, kind, status string, pinned bool) Article {
		t.Helper()
		payload, _ := json.Marshal(map[string]any{"space_id": space, "title": title, "kind": kind, "status": status, "pinned": pinned})
		code, body := request(t, s, http.MethodPost, "/api/articles", string(payload))
		if code != http.StatusCreated {
			t.Fatalf("create %s: %d %s", title, code, body)
		}
		var a Article
		if err := json.Unmarshal(body, &a); err != nil {
			t.Fatal(err)
		}
		return a
	}
	selected := create("genealogy", "Pinned research", "research", "active", true)
	archived := create("genealogy", "Archived pin", "plan", "archived", true)
	other := create("engineering", "Other pin", "plan", "active", true)
	code, body := request(t, s, http.MethodGet, "/api/bootstrap?space_id=genealogy", "")
	if code != http.StatusOK {
		t.Fatalf("bootstrap: %d %s", code, body)
	}
	var response struct {
		Instructions []Article `json:"instructions"`
		Recent       []Article `json:"recent"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, a := range response.Instructions {
		ids[a.ID] = true
	}
	if !ids[selected.ID] || ids[archived.ID] || ids[other.ID] {
		t.Fatalf("pinned bootstrap selection wrong: %#v", response.Instructions)
	}
	for _, a := range response.Recent {
		if a.SpaceID != "general" && a.SpaceID != "genealogy" {
			t.Fatalf("recent article from unselected space: %#v", a)
		}
	}
}

func TestMCPBootstrapSearchWriteAndSectionRead(t *testing.T) {
	s := testServer(t)
	call := func(name string, args any) (bool, string) {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": name, "arguments": args}})
		code, data := request(t, s, http.MethodPost, "/mcp", string(body))
		if code != http.StatusOK {
			t.Fatalf("MCP %s: %d %s", name, code, data)
		}
		var envelope struct {
			Result struct {
				IsError bool `json:"isError"`
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
			} `json:"result"`
		}
		if err := json.Unmarshal(data, &envelope); err != nil {
			t.Fatal(err)
		}
		if len(envelope.Result.Content) != 1 {
			t.Fatalf("MCP %s content: %s", name, data)
		}
		return envelope.Result.IsError, envelope.Result.Content[0].Text
	}
	isError, text := call("wiki_bootstrap", map[string]any{"space_id": "general"})
	if isError {
		t.Fatalf("bootstrap: %s", text)
	}
	var bootstrap struct {
		Instructions []Article `json:"instructions"`
	}
	if err := json.Unmarshal([]byte(text), &bootstrap); err != nil || len(bootstrap.Instructions) < 3 {
		t.Fatalf("bootstrap instructions: %s (%v)", text, err)
	}
	md := "# Main\n## Данные\nalpha\n```md\n## Игнорировать\n```\n## Данные\nbeta\n## Конец\n\nTitle **важно** [материал](https://example.test)\n---\nsetext content\n"
	article := map[string]any{"space_id": "general", "title": "MCP literal filter note", "summary": "MCP summary", "body": md, "kind": "research", "status": "active", "tags": []string{"mcp-tag", "second-tag"}, "properties": map[string]string{"category": "demo", "state": "active"}}
	isError, text = call("wiki_write", map[string]any{"article": article})
	if isError {
		t.Fatalf("wiki_write: %s", text)
	}
	var created Article
	if err := json.Unmarshal([]byte(text), &created); err != nil {
		t.Fatal(err)
	}
	if created.ID == "" || created.Revision != 1 {
		t.Fatalf("wiki_write result: %#v", created)
	}
	isError, text = call("wiki_search", map[string]any{"q": "MCP literal", "space_id": "general", "tag": []string{"mcp-tag", "second-tag"}, "property": []string{"category:demo", "state:active"}})
	if isError {
		t.Fatalf("wiki_search: %s", text)
	}
	var matches struct {
		Total   int       `json:"total"`
		Results []Article `json:"results"`
	}
	if err := json.Unmarshal([]byte(text), &matches); err != nil {
		t.Fatal(err)
	}
	if matches.Total != 1 || len(matches.Results) != 1 || matches.Results[0].ID != created.ID {
		t.Fatalf("filtered MCP search: %s", text)
	}
	isError, text = call("wiki_read", map[string]any{"id": created.ID, "anchor": "данные-2"})
	if isError {
		t.Fatalf("wiki_read section: %s", text)
	}
	var partial struct {
		Partial  bool   `json:"partial"`
		Anchor   string `json:"anchor"`
		Revision int    `json:"revision"`
		Section  string `json:"section"`
	}
	if err := json.Unmarshal([]byte(text), &partial); err != nil {
		t.Fatal(err)
	}
	if !partial.Partial || partial.Anchor != "данные-2" || partial.Revision != 1 {
		t.Fatalf("section not marked partial: %s", text)
	}
	if !strings.Contains(partial.Section, "beta") || strings.Contains(partial.Section, "alpha") || strings.Contains(partial.Section, "Игнорировать") || strings.Contains(partial.Section, "## Конец") {
		t.Fatalf("wrong section: %s", partial.Section)
	}
	isError, text = call("wiki_read", map[string]any{"id": created.ID, "anchor": "title-важно-материал"})
	if isError || !strings.Contains(text, "setext content") {
		t.Fatalf("Setext/inline heading read: isError=%v text=%s", isError, text)
	}
	isError, _ = call("wiki_read", map[string]any{"id": "bad/id"})
	if !isError {
		t.Fatal("wiki_read accepted malformed ID")
	}
	isError, _ = call("wiki_history", map[string]any{})
	if !isError {
		t.Fatal("wiki_history accepted missing ID")
	}
}

func TestMCPToolSchemasExposeContractFields(t *testing.T) {
	s := testServer(t)
	code, body := request(t, s, http.MethodPost, "/mcp", "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"tools/list\"}")
	if code != http.StatusOK {
		t.Fatalf("tools/list: %d %s", code, body)
	}
	var response struct {
		Result struct {
			Tools []struct {
				Name   string `json:"name"`
				Schema struct {
					Properties map[string]json.RawMessage `json:"properties"`
				} `json:"inputSchema"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		t.Fatal(err)
	}
	schemas := map[string]map[string]json.RawMessage{}
	for _, tool := range response.Result.Tools {
		schemas[tool.Name] = tool.Schema.Properties
	}
	for _, field := range []string{"q", "space_id", "tag", "property", "kind", "pinned", "limit", "offset"} {
		if _, ok := schemas["wiki_search"][field]; !ok {
			t.Errorf("wiki_search schema missing %q", field)
		}
	}
	var articleSchema struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(schemas["wiki_write"]["article"], &articleSchema); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"space_id", "parent_id", "title", "summary", "body", "kind", "status", "pinned", "tags", "properties", "base_revision", "author", "message"} {
		if _, ok := articleSchema.Properties[field]; !ok {
			t.Errorf("wiki_write article schema missing %q", field)
		}
	}
}

func TestForeignOriginCannotPostMCPWrite(t *testing.T) {
	s := testServer(t)
	body := "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"tools/call\",\"params\":{\"name\":\"wiki_write\",\"arguments\":{\"article\":{\"space_id\":\"general\",\"title\":\"forbidden\",\"kind\":\"article\",\"status\":\"draft\"}}}}"
	r := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	r.Host = "localhost:8090"
	r.Header.Set("Origin", "https://attacker.example")
	r.Header.Set("Content-Type", "text/plain")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("foreign origin status=%d body=%s", w.Code, w.Body.String())
	}
	var count int
	if err := s.DB.QueryRow("SELECT count(*) FROM articles WHERE title='forbidden'").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("foreign origin created %d articles", count)
	}
}

func TestTrustedLANOriginCanPostMCPWrite(t *testing.T) {
	s := testServer(t)
	if err := s.SetAllowedOrigin("http://192.0.2.10:8090"); err != nil {
		t.Fatal(err)
	}
	body := "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"tools/call\",\"params\":{\"name\":\"wiki_write\",\"arguments\":{\"article\":{\"space_id\":\"general\",\"title\":\"trusted lan\",\"kind\":\"article\",\"status\":\"draft\"}}}}"
	r := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	r.Host = "192.0.2.10:8090"
	r.Header.Set("Origin", "http://192.0.2.10:8090")
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusOK || w.Header().Get("Access-Control-Allow-Origin") != "http://192.0.2.10:8090" {
		t.Fatalf("trusted origin status=%d allow-origin=%q body=%s", w.Code, w.Header().Get("Access-Control-Allow-Origin"), w.Body.String())
	}
	var count int
	if err := s.DB.QueryRow("SELECT count(*) FROM articles WHERE title='trusted lan'").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("trusted origin created %d articles", count)
	}
}

func TestAllowedOriginRejectsUnexpectedValue(t *testing.T) {
	s := testServer(t)
	for _, origin := range []string{"ftp://192.0.2.10:8090", "http://192.0.2.10:8090/", "https://user@attacker.example", "https://attacker.example?x=1", "https://attacker.example#x", "http:///missing-host"} {
		if err := s.SetAllowedOrigin(origin); err == nil {
			t.Errorf("SetAllowedOrigin(%q) succeeded", origin)
		}
	}
}

func TestExplicitAllowedOriginAcceptsConfiguredHostOnly(t *testing.T) {
	s := testServer(t)
	const trusted = "https://wiki.example.test:8443"
	if err := s.SetAllowedOrigin(trusted); err != nil {
		t.Fatal(err)
	}
	for _, origin := range []string{trusted, "https://other.example.test:8443"} {
		r := httptest.NewRequest(http.MethodGet, "/api/health", nil)
		r.Host = "wiki.example.test:8443"
		r.Header.Set("Origin", origin)
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		if origin == trusted {
			if w.Code != http.StatusOK || w.Header().Get("Access-Control-Allow-Origin") != trusted {
				t.Errorf("configured origin status=%d allow-origin=%q", w.Code, w.Header().Get("Access-Control-Allow-Origin"))
			}
		} else if w.Code != http.StatusForbidden {
			t.Errorf("foreign origin %q status=%d, want forbidden", origin, w.Code)
		}
	}
}

func TestUpdateKeysAreCaseInsensitive(t *testing.T) {
	s := testServer(t)
	code, body := request(t, s, http.MethodPost, "/api/articles", `{"space_id":"general","title":"Original","body":"keep"}`)
	if code != 200 && code != 201 {
		t.Fatalf("create: %d %s", code, body)
	}
	var a Article
	if err := json.Unmarshal(body, &a); err != nil {
		t.Fatal(err)
	}
	code, body = request(t, s, http.MethodPut, "/api/articles/"+a.ID, `{"Title":"Changed","base_revision":1}`)
	if code != 200 {
		t.Fatalf("update: %d %s", code, body)
	}
	var b Article
	if err := json.Unmarshal(body, &b); err != nil {
		t.Fatal(err)
	}
	if b.Title != "Changed" || b.Body != "keep" {
		t.Fatalf("Title key ignored or body lost: %+v", b)
	}
}
