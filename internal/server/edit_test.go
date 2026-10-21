package server

import (
	"strings"
	"testing"
)

func readBody(t *testing.T, s *Server, id string) (string, int) {
	t.Helper()
	var a Article
	mustOK(t, s, "wiki_read", map[string]any{"id": id}, &a)
	return a.Body, a.Revision
}

func TestMCPEditOps(t *testing.T) {
	s := testServer(t)
	id, rev := writeArticle(t, s, map[string]any{"space_id": "general", "title": "Edit", "body": "# Top\nintro\n\n## A\naaa\n\n### A1\nsub\n\n## B\nbbb\n"})

	var r struct {
		ID        string `json:"id"`
		Revision  int    `json:"revision"`
		UpdatedAt string `json:"updated_at"`
		Chars     int    `json:"chars"`
	}
	text := mustOK(t, s, "wiki_edit", map[string]any{"id": id, "base_revision": rev, "op": "replace_text", "old": "bbb", "text": "BBB", "author": "t", "message": "m"}, &r)
	t.Logf("wiki_edit: %s", text)
	if r.Revision != 2 || r.ID != id || r.UpdatedAt == "" || r.Chars == 0 || strings.Contains(text, "body") {
		t.Fatalf("response: %s", text)
	}
	// delete via empty text
	mustOK(t, s, "wiki_edit", map[string]any{"id": id, "base_revision": 2, "op": "replace_text", "old": "intro"}, &r)
	// append into section A (before the next same-level heading, after its subsections)
	mustOK(t, s, "wiki_edit", map[string]any{"id": id, "base_revision": 3, "op": "append", "anchor": "a", "text": "appended"}, &r)
	body, _ := readBody(t, s, id)
	want := "# Top\n\n\n## A\naaa\n\n### A1\nsub\n\nappended\n\n## B\nBBB\n"
	if body != want {
		t.Fatalf("after append to section:\n%q\nwant\n%q", body, want)
	}
	// append at the end of the article
	mustOK(t, s, "wiki_edit", map[string]any{"id": id, "base_revision": 4, "op": "append", "text": "tail"}, &r)
	body, _ = readBody(t, s, id)
	if !strings.HasSuffix(body, "BBB\n\ntail\n") {
		t.Fatalf("append end: %q", body)
	}
	// replace_section
	mustOK(t, s, "wiki_edit", map[string]any{"id": id, "base_revision": 5, "op": "replace_section", "anchor": "a", "text": "## A renamed\nnew content"}, &r)
	body, rev = readBody(t, s, id)
	if !strings.Contains(body, "## A renamed\nnew content\n\n## B\n") || strings.Contains(body, "A1") || strings.Contains(body, "aaa") {
		t.Fatalf("replace_section: %q", body)
	}
	if rev != 6 {
		t.Fatalf("rev %d", rev)
	}
}

func TestMCPEditErrors(t *testing.T) {
	s := testServer(t)
	id, rev := writeArticle(t, s, map[string]any{"space_id": "general", "title": "Err", "body": "# T\nsame\nsame\n## S\nx\n"})
	check := func(args map[string]any, want string) {
		t.Helper()
		args["id"] = id
		isErr, text := rpc(t, s, "wiki_edit", args)
		if !isErr || !strings.Contains(text, want) {
			t.Fatalf("%v: isError=%v text=%s (want %q)", args, isErr, text, want)
		}
	}
	check(map[string]any{"base_revision": rev, "op": "replace_text", "old": "same", "text": "y"}, "found 2 occurrences")
	check(map[string]any{"base_revision": rev, "op": "replace_text", "old": "missing", "text": "y"}, "found 0 occurrences")
	check(map[string]any{"base_revision": rev, "op": "replace_section", "anchor": "s", "text": "no heading here"}, "must start with the section heading")
	check(map[string]any{"base_revision": rev, "op": "replace_section", "anchor": "nope", "text": "## N"}, "heading not found")
	check(map[string]any{"base_revision": rev, "op": "replace_section", "text": "## N"}, "anchor is required")
	check(map[string]any{"base_revision": rev, "op": "bogus"}, "op must be")
	check(map[string]any{"op": "append", "text": "x"}, "base_revision")
	// conflict
	mustOK(t, s, "wiki_edit", map[string]any{"id": id, "base_revision": rev, "op": "append", "text": "x"}, nil)
	check(map[string]any{"base_revision": rev, "op": "append", "text": "y"}, "revision conflict: current revision is 2")
	// nothing was changed by the failures
	body, r := readBody(t, s, id)
	if r != 2 || !strings.Contains(body, "same\nsame") {
		t.Fatalf("state after failures: rev=%d %q", r, body)
	}
}

func TestMCPEditReplaceSectionKeepsHeadingLevel(t *testing.T) {
	s := testServer(t)
	id, rev := writeArticle(t, s, map[string]any{"space_id": "general", "title": "Level", "body": "# Top\n\n## A\naaa\n\n## B\nbbb\n"})
	isErr, text := rpc(t, s, "wiki_edit", map[string]any{"id": id, "base_revision": rev, "op": "replace_section", "anchor": "a", "text": "### A\nnew"})
	if !isErr || !strings.Contains(text, "heading level changed from 2 to 3; use wiki_write to restructure") {
		t.Fatalf("level change: %v %s", isErr, text)
	}
	mustOK(t, s, "wiki_edit", map[string]any{"id": id, "base_revision": rev, "op": "replace_section", "anchor": "a", "text": "## A2\nnew"}, nil)
	body, _ := readBody(t, s, id)
	if !strings.Contains(body, "## A2\nnew") {
		t.Fatalf("body: %q", body)
	}
}
