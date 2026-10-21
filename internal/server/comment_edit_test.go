package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func commentFixture(t *testing.T, s *Server, id string, resolved bool) Comment {
	t.Helper()
	a := Article{ID: "comment-article-" + id, SpaceID: "general", Title: "Comment fixture", Kind: "article", Status: "active", Tags: []string{}, Properties: map[string]string{}, Revision: 1, CreatedAt: stamp(), UpdatedAt: stamp(), Author: "fixture"}
	tx, err := s.DB.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err := insertArticle(tx, a, "fixture"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	c := Comment{ID: id, ArticleID: a.ID, Anchor: "section-1", Body: "original body", Author: "original author", CreatedAt: "2026-10-10T10:11:12Z", Resolved: resolved}
	if _, err := s.DB.Exec("INSERT INTO comments(id,article_id,anchor,body,author,created_at,resolved) VALUES(?,?,?,?,?,?,?)", c.ID, c.ArticleID, c.Anchor, c.Body, c.Author, c.CreatedAt, boolInt(c.Resolved)); err != nil {
		t.Fatal(err)
	}
	return c
}

func patchComment(t *testing.T, s *Server, id, payload string) (int, []byte) {
	t.Helper()
	return request(t, s, http.MethodPatch, "/api/comments/"+id, payload)
}

func decodeComment(t *testing.T, b []byte) Comment {
	t.Helper()
	var c Comment
	if err := json.Unmarshal(b, &c); err != nil {
		t.Fatalf("decode comment response: %v: %s", err, b)
	}
	return c
}

func TestCommentPatchEditsMarkdownWithoutChangingMetadata(t *testing.T) {
	s := testServer(t)
	want := commentFixture(t, s, "markdown-edit", false)
	code, body := patchComment(t, s, want.ID, `{"body":"**updated**\n\n- one\n- two"}`)
	if code != http.StatusOK {
		t.Fatalf("patch: %d %s", code, body)
	}
	got := decodeComment(t, body)
	if got.Body != "**updated**\n\n- one\n- two" || got.ID != want.ID || got.ArticleID != want.ArticleID || got.Anchor != want.Anchor || got.Author != want.Author || got.CreatedAt != want.CreatedAt || got.Resolved != want.Resolved {
		t.Fatalf("comment metadata/body mismatch: got %#v, want body updated with metadata %#v", got, want)
	}
}

func TestCommentPatchBodyPreservesResolvedAndExplicitFalseWorks(t *testing.T) {
	s := testServer(t)
	want := commentFixture(t, s, "resolved-edit", true)
	code, body := patchComment(t, s, want.ID, `{"body":"edited while resolved"}`)
	if code != http.StatusOK || !decodeComment(t, body).Resolved {
		t.Fatalf("body edit cleared resolved: %d %s", code, body)
	}
	code, body = patchComment(t, s, want.ID, `{"resolved":false}`)
	if code != http.StatusOK || decodeComment(t, body).Resolved {
		t.Fatalf("explicit false did not clear resolved: %d %s", code, body)
	}
}

func TestCommentPatchResolvePreservesTextAndAnchor(t *testing.T) {
	s := testServer(t)
	want := commentFixture(t, s, "resolve-preserves", false)
	code, body := patchComment(t, s, want.ID, `{"resolved":true}`)
	got := decodeComment(t, body)
	if code != http.StatusOK || !got.Resolved || got.Body != want.Body || got.Anchor != want.Anchor {
		t.Fatalf("resolve changed text or anchor: %d %#v", code, got)
	}
}

func TestCommentPatchAnchorCanBeRemoved(t *testing.T) {
	s := testServer(t)
	want := commentFixture(t, s, "anchor-remove", false)
	code, body := patchComment(t, s, want.ID, `{"anchor":""}`)
	if code != http.StatusOK || decodeComment(t, body).Anchor != "" {
		t.Fatalf("empty anchor did not remove binding: %d %s", code, body)
	}
}

func TestCommentPatchRejectsInvalidOrOversizedBodyWithoutChanges(t *testing.T) {
	s := testServer(t)
	want := commentFixture(t, s, "invalid-patch", true)
	for _, payload := range []string{`{}`, `{"body":" \n\t "}`, `{"body":false}`, `{"anchor":1}`, `{"resolved":"true"}`, fmt.Sprintf(`{"body":%q}`, strings.Repeat("x", (2<<20)+1))} {
		code, _ := patchComment(t, s, want.ID, payload)
		if code != http.StatusBadRequest {
			t.Fatalf("payload should be rejected with 400, got %d", code)
		}
	}
	var got Comment
	var resolved int
	err := s.DB.QueryRow("SELECT id,article_id,anchor,body,author,created_at,resolved FROM comments WHERE id=?", want.ID).Scan(&got.ID, &got.ArticleID, &got.Anchor, &got.Body, &got.Author, &got.CreatedAt, &resolved)
	if err != nil {
		t.Fatal(err)
	}
	got.Resolved = resolved != 0
	if got != want {
		t.Fatalf("invalid patch changed stored comment: got %#v want %#v", got, want)
	}
}

func TestCommentPatchMissingReturns404(t *testing.T) {
	s := testServer(t)
	code, body := patchComment(t, s, "missing-comment", `{"resolved":true}`)
	if code != http.StatusNotFound || !strings.Contains(string(body), `"error"`) {
		t.Fatalf("missing comment response: %d %s", code, body)
	}
}

func TestCommentPatchDatabaseFailureRollsBack(t *testing.T) {
	s := testServer(t)
	want := commentFixture(t, s, "rollback-comment", false)
	if _, err := s.DB.Exec(`CREATE TRIGGER fail_comment_update BEFORE UPDATE ON comments BEGIN SELECT RAISE(ABORT, 'forced failure'); END`); err != nil {
		t.Fatal(err)
	}
	code, _ := patchComment(t, s, want.ID, `{"body":"partial update","resolved":true}`)
	if code != http.StatusInternalServerError {
		t.Fatalf("forced update error returned %d", code)
	}
	var got Comment
	var resolved int
	err := s.DB.QueryRow("SELECT id,article_id,anchor,body,author,created_at,resolved FROM comments WHERE id=?", want.ID).Scan(&got.ID, &got.ArticleID, &got.Anchor, &got.Body, &got.Author, &got.CreatedAt, &resolved)
	if err != nil {
		t.Fatal(err)
	}
	got.Resolved = resolved != 0
	if got != want {
		t.Fatalf("failed update changed stored comment: got %#v want %#v", got, want)
	}
}
