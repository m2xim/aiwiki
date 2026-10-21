package server

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"
)

type sectionSearchResponse struct {
	Total      int                `json:"total"`
	Offset     int                `json:"offset"`
	Mode       string             `json:"mode"`
	Results    []sectionSearchHit `json:"results"`
	NextOffset *int               `json:"next_offset"`
}

func callSectionSearch(t *testing.T, s *Server, args map[string]any) (bool, string, sectionSearchResponse) {
	t.Helper()
	rawArgs, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	var parsed map[string]json.RawMessage
	if err := json.Unmarshal(rawArgs, &parsed); err != nil {
		t.Fatal(err)
	}
	result := s.toolSearchSections(parsed).(map[string]any)
	content := result["content"].([]any)
	message := content[0].(map[string]string)["text"]
	var response sectionSearchResponse
	if result["isError"] == false {
		if err := json.Unmarshal([]byte(message), &response); err != nil {
			t.Fatalf("decode section search response: %v: %s", err, message)
		}
	}
	return result["isError"].(bool), message, response
}

func TestToolSearchSectionsOwnSegmentsAnchorsAndReadArgs(t *testing.T) {
	s := testServer(t)
	long := strings.Repeat("ordinary words ", 420)
	body := "Preamble has preamble-token.\n" +
		"# Repeat\nroot-only.\n" +
		"## Repeat\n" + long + "Late Engines appear here.\n" +
		"### Grandchild\nchild-only.\n" +
		"## Repeat\nthird-only.\n" +
		"```md\n## Fake Heading\nfake-only\n```\n" +
		"Setext heading\n===============\nsetext-only.\n"
	id, _ := writeArticle(t, s, map[string]any{"space_id": "general", "title": "Sections", "body": body})

	_, _, r := callSectionSearch(t, s, map[string]any{"q": "engines", "limit": 20})
	if r.Mode != "all" || r.Total != 1 || len(r.Results) != 1 {
		t.Fatalf("late nested match: mode=%s total=%d results=%#v", r.Mode, r.Total, r.Results)
	}
	hit := r.Results[0]
	if hit.ID != id || hit.Anchor != "repeat-2" || hit.HeadingPath != "Repeat > Repeat" {
		t.Fatalf("nested heading/anchor: %#v", hit)
	}
	if utf8.RuneCountInString(hit.Snippet) > 240 {
		t.Fatalf("snippet too long: %d", utf8.RuneCountInString(hit.Snippet))
	}
	if hit.ReadArgs["id"] != id || hit.ReadArgs["revision"] != float64(1) || hit.ReadArgs["anchor"] != "repeat-2" || hit.ReadArgs["max_chars"] != float64(4000) {
		t.Fatalf("read args: %#v", hit.ReadArgs)
	}
	offset, ok := hit.ReadArgs["offset"].(float64)
	if !ok || offset < 1000 {
		t.Fatalf("late match offset not useful: %#v", hit.ReadArgs)
	}
	readArgsJSON, _ := json.Marshal(hit.ReadArgs)
	var readArgs map[string]json.RawMessage
	_ = json.Unmarshal(readArgsJSON, &readArgs)
	readResult := s.toolReadChunk(readArgs).(map[string]any)
	if readResult["isError"] != false {
		t.Fatalf("read_args failed: %#v", readResult)
	}
	readText := readResult["content"].([]any)[0].(map[string]string)["text"]
	var chunk struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal([]byte(readText), &chunk); err != nil || !strings.Contains(chunk.Text, "Late Engines") {
		t.Fatalf("late match missing from executable read_args: %s (%v)", readText, err)
	}

	_, _, r = callSectionSearch(t, s, map[string]any{"q": "preamble-token"})
	if r.Total != 1 || r.Results[0].Anchor != "" || r.Results[0].Heading != "" || r.Results[0].ReadArgs["offset"] != float64(0) {
		t.Fatalf("preamble result: %#v", r)
	}
	readArgsJSON, _ = json.Marshal(r.Results[0].ReadArgs)
	readArgs = nil
	_ = json.Unmarshal(readArgsJSON, &readArgs)
	readResult = s.toolReadChunk(readArgs).(map[string]any)
	readText = readResult["content"].([]any)[0].(map[string]string)["text"]
	if !strings.Contains(readText, "Preamble has preamble-token") {
		t.Fatalf("preamble read_args did not address whole body: %s", readText)
	}

	// Child content appears once under its own heading and never in the parent hit.
	_, _, r = callSectionSearch(t, s, map[string]any{"q": "child-only"})
	if r.Total != 1 || r.Results[0].Heading != "Grandchild" || r.Results[0].Anchor != "grandchild" {
		t.Fatalf("child duplicated into ancestor: %#v", r)
	}
	_, _, r = callSectionSearch(t, s, map[string]any{"q": "fake-only"})
	if r.Total != 1 || r.Results[0].Anchor != "repeat-3" {
		t.Fatalf("fenced heading should not create a section boundary: %#v", r)
	}
	_, _, r = callSectionSearch(t, s, map[string]any{"q": "setext-only"})
	if r.Total != 1 || r.Results[0].Heading != "Setext heading" {
		t.Fatalf("setext heading: %#v", r)
	}
	isErr, readMessage := rpc(t, s, "wiki_read", map[string]any{
		"id": id, "revision": 1, "offset": len([]rune(body)) + 1, "max_chars": 20,
	})
	if !isErr || !strings.Contains(readMessage, "offset is past the end") {
		t.Fatalf("offset past total should fail: %s", readMessage)
	}
}

func TestToolSearchSectionsGlobalModeAndFilters(t *testing.T) {
	s := testServer(t)
	allID, _ := writeArticle(t, s, map[string]any{
		"space_id": "general", "title": "All terms", "kind": "instruction", "pinned": true,
		"tags": []string{"needle-tag"}, "properties": map[string]string{"group": "one"},
		"body": "# Match\nalpha beta\n",
	})
	writeArticle(t, s, map[string]any{"space_id": "general", "title": "Partial", "body": "# Root\n## Alpha\nalpha only\n## Other\nbeta only\n"})
	archivedID, _ := writeArticle(t, s, map[string]any{"space_id": "engineering", "title": "Scoped archive", "body": "# Match\nalpha beta\n"})
	deletedID, _ := writeArticle(t, s, map[string]any{"space_id": "general", "title": "Deleted", "body": "# Match\ndeletedneedle alpha beta\n"})
	if _, err := s.DB.Exec("UPDATE spaces SET archived_at=? WHERE id=?", stamp(), "engineering"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec("UPDATE articles SET deleted_at=? WHERE id=?", stamp(), deletedID); err != nil {
		t.Fatal(err)
	}

	_, _, r := callSectionSearch(t, s, map[string]any{"q": "alpha beta", "limit": 20})
	if r.Mode != "all" || r.Total != 1 || r.Results[0].ID != allID {
		t.Fatalf("all terms must win globally: %#v", r)
	}
	_, _, r = callSectionSearch(t, s, map[string]any{"q": "alpha missing", "limit": 20})
	if r.Mode != "any" || r.Total != 2 {
		t.Fatalf("global any fallback: %#v", r)
	}
	_, _, r = callSectionSearch(t, s, map[string]any{"q": "alpha beta", "space_id": "engineering"})
	if r.Total != 1 || r.Results[0].ID != archivedID {
		t.Fatalf("explicit scope must include archived space: %#v", r)
	}
	_, _, r = callSectionSearch(t, s, map[string]any{
		"q": "alpha beta", "kind": "instruction", "pinned": true,
		"tag": "needle-tag", "property": "group:one",
	})
	if r.Total != 1 || r.Results[0].ID != allID {
		t.Fatalf("filters did not match article search semantics: %#v", r)
	}
	_, _, r = callSectionSearch(t, s, map[string]any{"q": "alpha beta", "tag": "missing"})
	if r.Total != 0 {
		t.Fatalf("tag filter ignored: %#v", r)
	}
	_, _, r = callSectionSearch(t, s, map[string]any{"q": "deletedneedle"})
	if r.Total != 0 {
		t.Fatalf("deleted article returned by section search: %#v", r)
	}
	_, _, r = callSectionSearch(t, s, map[string]any{"q": "alpha beta", "limit": 1})
	if r.Total != 1 || r.NextOffset != nil {
		t.Fatalf("single result paging: %#v", r)
	}
}

func TestToolSearchSectionsValidationAndMorphology(t *testing.T) {
	s := testServer(t)
	id, _ := writeArticle(t, s, map[string]any{"space_id": "general", "title": "Morph", "body": "# Rule\nсерверы работают быстро.\n"})
	_, _, r := callSectionSearch(t, s, map[string]any{"q": "сервер"})
	if r.Total != 1 || r.Results[0].ID != id {
		t.Fatalf("Russian morphology: %#v", r)
	}
	for _, tc := range []struct {
		args map[string]any
		want string
	}{
		{map[string]any{}, "q must contain searchable terms"},
		{map[string]any{"q": "   "}, "q must contain searchable terms"},
		{map[string]any{"q": "server", "limit": 0}, "limit must be between 1 and 20"},
		{map[string]any{"q": "server", "limit": 21}, "limit must be between 1 and 20"},
		{map[string]any{"q": "server", "limit": nil}, "limit must be an integer"},
		{map[string]any{"q": "server", "offset": -1}, "offset must be 0 or greater"},
		{map[string]any{"q": "server", "offset": nil}, "offset must be an integer"},
		{map[string]any{"q": 3}, "q must be a string"},
		{map[string]any{"q": "server", "limit": "5"}, "limit must be an integer"},
		{map[string]any{"q": "server", "pinned": "yes"}, "pinned must be a boolean"},
		{map[string]any{"q": "server", "property": "broken"}, "property must be key:value"},
		{map[string]any{"q": "server", "tag": []any{"ok", 1}}, "tag must be a string or an array of strings"},
	} {
		isErr, message, _ := callSectionSearch(t, s, tc.args)
		if !isErr || !strings.Contains(message, tc.want) {
			t.Errorf("args %s: isError=%v, want %q", fmt.Sprint(tc.args), isErr, tc.want)
		}
	}
}

func TestToolSearchSectionsReadOffsetFollowsBestMatchWindow(t *testing.T) {
	s := testServer(t)
	text := "# Commonheading\n" + strings.Repeat("commonheading ", 1200) +
		"Commonheading appears beside the unique-latemarker.\n"
	id, _ := writeArticle(t, s, map[string]any{"space_id": "general", "title": "Late cluster", "body": text})
	_, _, r := callSectionSearch(t, s, map[string]any{"q": "commonheading unique-latemarker"})
	if r.Total != 1 || r.Results[0].ID != id {
		t.Fatalf("cluster query: %#v", r)
	}
	if !strings.Contains(r.Results[0].Snippet, "unique-latemarker") {
		t.Fatalf("snippet did not use the late best-match window: %q", r.Results[0].Snippet)
	}
	args := r.Results[0].ReadArgs
	if args["offset"].(float64) < 5000 {
		t.Fatalf("read offset chose the early heading match instead of the best late window: %#v", args)
	}
	encoded, _ := json.Marshal(args)
	var raw map[string]json.RawMessage
	_ = json.Unmarshal(encoded, &raw)
	result := s.toolReadChunk(raw).(map[string]any)
	chunk := result["content"].([]any)[0].(map[string]string)["text"]
	var read struct {
		Text string `json:"text"`
	}
	if result["isError"] != false || json.Unmarshal([]byte(chunk), &read) != nil || !strings.Contains(read.Text, "unique-latemarker") {
		t.Fatalf("late cluster read failed: %s", chunk)
	}

	// U+0130 lowercases to a rune with a different UTF-8 byte width. Offsets
	// must remain rune based after normalization.
	unicodeBody := "# Unicode\n" + strings.Repeat("İ", 4300) + "\nuniqueunicodeword\n"
	unicodeID, _ := writeArticle(t, s, map[string]any{"space_id": "general", "title": "Unicode offset", "body": unicodeBody})
	_, _, r = callSectionSearch(t, s, map[string]any{"q": "uniqueunicodeword"})
	var unicodeHit *sectionSearchHit
	for i := range r.Results {
		if r.Results[i].ID == unicodeID {
			unicodeHit = &r.Results[i]
			break
		}
	}
	if unicodeHit == nil {
		t.Fatalf("Unicode hit not returned: %#v", r)
	}
	encoded, _ = json.Marshal(unicodeHit.ReadArgs)
	raw = nil
	_ = json.Unmarshal(encoded, &raw)
	result = s.toolReadChunk(raw).(map[string]any)
	chunk = result["content"].([]any)[0].(map[string]string)["text"]
	if result["isError"] != false || json.Unmarshal([]byte(chunk), &read) != nil || !strings.Contains(read.Text, "uniqueunicodeword") {
		t.Fatalf("Unicode read offset was not rune-safe: %s", chunk)
	}
}

func TestToolSearchSectionsCollisionFallbackThroughRPC(t *testing.T) {
	s := testServer(t)
	body := "# Repeat\nfirst section\n## Repeat\nsecond section\n## Repeat 2\n" +
		strings.Repeat("ordinary filler ", 500) + "collisionmarker is here.\n"
	id, _ := writeArticle(t, s, map[string]any{"space_id": "general", "title": "Anchor collision", "body": body})
	var response sectionSearchResponse
	mustOK(t, s, "wiki_search_sections", map[string]any{"q": "collisionmarker"}, &response)
	if response.Total != 1 || len(response.Results) != 1 {
		t.Fatalf("collision search result: %#v", response)
	}
	hit := response.Results[0]
	if hit.ID != id || hit.Heading != "Repeat 2" || hit.Anchor != "" {
		t.Fatalf("colliding heading must not expose ambiguous anchor: %#v", hit)
	}
	if _, hasAnchor := hit.ReadArgs["anchor"]; hasAnchor {
		t.Fatalf("fallback read args contains ambiguous anchor: %#v", hit.ReadArgs)
	}
	if hit.ReadArgs["offset"].(float64) < 5000 {
		t.Fatalf("fallback offset is not near the late section: %#v", hit.ReadArgs)
	}
	var read struct {
		Text string `json:"text"`
	}
	mustOK(t, s, "wiki_read", hit.ReadArgs, &read)
	if !strings.Contains(read.Text, "collisionmarker") || strings.Contains(read.Text, "first section") || strings.Contains(read.Text, "second section") {
		t.Fatalf("full-body offset did not read the intended colliding section: %.500s", read.Text)
	}
}
