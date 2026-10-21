package server

import (
	"encoding/json"
	"strings"
	"testing"
)

type chunkResp struct {
	ID         string         `json:"id"`
	Title      string         `json:"title"`
	Revision   int            `json:"revision"`
	Anchor     string         `json:"anchor"`
	Partial    bool           `json:"partial"`
	Historical bool           `json:"historical"`
	Offset     int            `json:"offset"`
	Chars      int            `json:"chars"`
	TotalChars int            `json:"total_chars"`
	HasMore    bool           `json:"has_more"`
	Text       string         `json:"text"`
	NextRead   map[string]any `json:"next_read"`
}

// callChunk invokes toolReadChunk directly with JSON-encoded arguments.
func callChunk(t *testing.T, s *Server, args map[string]any) (bool, string) {
	t.Helper()
	raw := map[string]json.RawMessage{}
	for k, v := range args {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		raw[k] = b
	}
	res := s.toolReadChunk(raw).(map[string]any)
	return res["isError"] == true, res["content"].([]any)[0].(map[string]string)["text"]
}

func chunkOK(t *testing.T, s *Server, args map[string]any) chunkResp {
	t.Helper()
	isErr, text := callChunk(t, s, args)
	if isErr {
		t.Fatalf("read %v failed: %s", args, text)
	}
	var r chunkResp
	if err := json.Unmarshal([]byte(text), &r); err != nil {
		t.Fatalf("decode %s: %v", text, err)
	}
	return r
}

// readAll follows next_read to the end and returns the concatenated text.
func readAll(t *testing.T, s *Server, args map[string]any) (string, []chunkResp) {
	t.Helper()
	var sb strings.Builder
	var all []chunkResp
	for i := 0; i < 1000; i++ {
		r := chunkOK(t, s, args)
		all = append(all, r)
		sb.WriteString(r.Text)
		if !r.HasMore {
			if r.NextRead != nil {
				t.Fatalf("next_read on final slice: %#v", r)
			}
			return sb.String(), all
		}
		if r.NextRead == nil {
			t.Fatalf("has_more without next_read: %#v", r)
		}
		if int(r.NextRead["revision"].(float64)) != r.Revision {
			t.Fatalf("next_read does not pin revision: %#v", r.NextRead)
		}
		args = r.NextRead
	}
	t.Fatal("did not terminate")
	return "", nil
}

const chunkBody = "Preamble 😀 текст é\n# Main\nintro Привет 世界 🎉\n## Данные\nalpha\n```md\n## Ignored\n```\n## Данные\nbeta 😀😀\n### Sub\nsubtext\n\nSetext\n------\nsetext body\n## Конец\nend\n"

func TestReadChunkUnicodeReconstructionBody(t *testing.T) {
	s := testServer(t)
	id, rev := writeArticle(t, s, map[string]any{"space_id": "general", "title": "Chunky", "body": chunkBody})
	total := len([]rune(chunkBody))
	for _, size := range []int{1, 2, 3, 7, 16, 50, total - 1, total, total + 1, 16000} {
		if size > 16000 {
			continue
		}
		got, all := readAll(t, s, map[string]any{"id": id, "max_chars": size})
		if got != chunkBody {
			t.Fatalf("size %d: reconstruction differs:\n%q", size, got)
		}
		first := all[0]
		if !first.Partial || first.Offset != 0 || first.Revision != rev || first.TotalChars != total || first.Title != "Chunky" || first.Anchor != "" {
			t.Fatalf("size %d first: %#v", size, first)
		}
		for _, r := range all {
			if r.Chars != len([]rune(r.Text)) || r.Chars > size {
				t.Fatalf("size %d chars: %#v", size, r)
			}
		}
	}
}

func TestReadChunkDefaultsAndOffsets(t *testing.T) {
	s := testServer(t)
	body := strings.Repeat("ж😀", 3000) // 6000 code points
	id, rev := writeArticle(t, s, map[string]any{"space_id": "general", "title": "Long", "body": body})
	r := chunkOK(t, s, map[string]any{"id": id, "offset": 0})
	if r.Chars != 4000 || !r.HasMore || r.NextRead["max_chars"].(float64) != 4000 || r.NextRead["offset"].(float64) != 4000 || r.TotalChars != 6000 {
		t.Fatalf("default: %#v", r)
	}
	// beginning/middle/end with explicit revision
	runes := []rune(body)
	for _, off := range []int{0, 1, 2999, 5990} {
		r := chunkOK(t, s, map[string]any{"id": id, "offset": off, "max_chars": 10, "revision": rev})
		end := off + 10
		if end > 6000 {
			end = 6000
		}
		if r.Text != string(runes[off:end]) || r.Offset != off || r.HasMore != (end < 6000) {
			t.Fatalf("offset %d: %#v", off, r)
		}
	}
	// exactly at the end: empty final slice
	r = chunkOK(t, s, map[string]any{"id": id, "offset": 6000, "revision": rev})
	if r.Text != "" || r.Chars != 0 || r.HasMore || r.NextRead != nil || r.Offset != 6000 {
		t.Fatalf("at end: %#v", r)
	}
	// exact fit has no more
	r = chunkOK(t, s, map[string]any{"id": id, "max_chars": 6000})
	if r.HasMore || r.Chars != 6000 || r.Text != body {
		t.Fatalf("exact fit: has_more=%v chars=%d", r.HasMore, r.Chars)
	}
	r = chunkOK(t, s, map[string]any{"id": id, "max_chars": 16000, "revision": rev})
	if r.Historical {
		t.Fatalf("current explicit revision marked historical: %#v", r)
	}
}

func TestReadChunkAnchoredRanges(t *testing.T) {
	s := testServer(t)
	id, _ := writeArticle(t, s, map[string]any{"space_id": "general", "title": "Anch", "body": chunkBody})
	for _, anchor := range []string{"main", "данные", "данные-2", "sub", "setext", "конец"} {
		want, ok := markdownSection(chunkBody, anchor)
		if !ok {
			t.Fatalf("fixture anchor %q missing", anchor)
		}
		got, all := readAll(t, s, map[string]any{"id": id, "anchor": anchor, "max_chars": 5})
		if got != want {
			t.Fatalf("anchor %s: %q != %q", anchor, got, want)
		}
		if all[0].Anchor != anchor || all[0].TotalChars != len([]rune(want)) {
			t.Fatalf("anchor %s first: %#v", anchor, all[0])
		}
		if len(all) > 1 && all[0].NextRead["anchor"] != anchor {
			t.Fatalf("next_read lost anchor: %#v", all[0].NextRead)
		}
	}
	// nested section includes children and not the next peer
	r := chunkOK(t, s, map[string]any{"id": id, "anchor": "данные-2", "max_chars": 16000})
	if !strings.Contains(r.Text, "subtext") || strings.Contains(r.Text, "alpha") || strings.Contains(r.Text, "Конец") {
		t.Fatalf("section: %q", r.Text)
	}
	// anchored offset past the section end, even if inside the body
	rev := 1
	if isErr, _ := callChunk(t, s, map[string]any{"id": id, "anchor": "конец", "offset": 1000, "revision": rev}); !isErr {
		t.Fatal("offset past section accepted")
	}
	if isErr, text := callChunk(t, s, map[string]any{"id": id, "anchor": "nope", "max_chars": 10}); !isErr || !strings.Contains(text, "heading not found") {
		t.Fatalf("missing anchor: %v %s", isErr, text)
	}
}

func TestReadChunkRevisionContinuationAfterEditAndRename(t *testing.T) {
	s := testServer(t)
	id, rev := writeArticle(t, s, map[string]any{"space_id": "general", "title": "Pin", "body": chunkBody})
	first := chunkOK(t, s, map[string]any{"id": id, "anchor": "конец", "max_chars": 3})
	whole := chunkOK(t, s, map[string]any{"id": id, "max_chars": 20})
	if first.Revision != rev || !first.HasMore || !whole.HasMore {
		t.Fatalf("setup: %#v %#v", first, whole)
	}
	var upd struct{ Revision int }
	mustOK(t, s, "wiki_edit", map[string]any{"id": id, "base_revision": rev, "op": "replace_text", "old": "## Конец", "text": "## Финал"}, &upd)
	mustOK(t, s, "wiki_edit", map[string]any{"id": id, "base_revision": upd.Revision, "op": "replace_text", "old": "Preamble", "text": "CHANGED"}, &upd)
	if upd.Revision != rev+2 {
		t.Fatalf("revision %d", upd.Revision)
	}
	// anchored continuation still reads the renamed heading from the old snapshot
	got := first.Text
	next := first.NextRead
	for next != nil {
		r := chunkOK(t, s, next)
		if r.Revision != rev || !r.Historical || r.Anchor != "конец" {
			t.Fatalf("continuation: %#v", r)
		}
		got += r.Text
		next = r.NextRead
	}
	if got != "## Конец\nend\n" {
		t.Fatalf("continued section %q", got)
	}
	// body continuation reconstructs the original body
	got, _ = readAll(t, s, whole.NextRead)
	if whole.Text+got != chunkBody {
		t.Fatalf("body continuation differs: %q", whole.Text+got)
	}
	// latest revision: old anchor is gone, new anchor works, unpinned read is current
	if isErr, _ := callChunk(t, s, map[string]any{"id": id, "anchor": "конец", "max_chars": 5}); !isErr {
		t.Fatal("old anchor resolved on latest revision")
	}
	r := chunkOK(t, s, map[string]any{"id": id, "anchor": "финал", "max_chars": 100})
	if r.Revision != upd.Revision || r.Historical || !strings.HasPrefix(r.Text, "## Финал") {
		t.Fatalf("latest: %#v", r)
	}
	if r := chunkOK(t, s, map[string]any{"id": id, "max_chars": 8}); r.Text != "CHANGED " {
		t.Fatalf("unpinned read: %#v", r)
	}
	// missing revision is an explicit error, never the latest body
	isErr, text := callChunk(t, s, map[string]any{"id": id, "offset": 3, "revision": 99})
	if !isErr || !strings.Contains(text, "revision not found") {
		t.Fatalf("missing revision: %v %s", isErr, text)
	}
}

func TestReadChunkUnchangedSnapshotAcrossReads(t *testing.T) {
	s := testServer(t)
	id, rev := writeArticle(t, s, map[string]any{"space_id": "general", "title": "Same", "body": strings.Repeat("абв😀\n", 100)})
	a := chunkOK(t, s, map[string]any{"id": id, "max_chars": 50})
	b := chunkOK(t, s, map[string]any{"id": id, "max_chars": 50})
	if a.Text != b.Text || a.Revision != rev || b.Revision != rev || a.NextRead["offset"] != b.NextRead["offset"] {
		t.Fatalf("not stable: %#v %#v", a, b)
	}
	// continuing at revision after no edits is not historical
	c := chunkOK(t, s, a.NextRead)
	if c.Historical || c.Offset != 50 {
		t.Fatalf("continuation: %#v", c)
	}
}

func TestReadChunkInvalidArguments(t *testing.T) {
	s := testServer(t)
	id, rev := writeArticle(t, s, map[string]any{"space_id": "general", "title": "Bad", "body": "héllo 😀 wörld"})
	total := len([]rune("héllo 😀 wörld"))
	cases := []struct {
		name string
		args map[string]any
		want string
	}{
		{"max zero", map[string]any{"id": id, "max_chars": 0}, "max_chars"},
		{"max negative", map[string]any{"id": id, "max_chars": -1}, "max_chars"},
		{"max too big", map[string]any{"id": id, "max_chars": 16001}, "max_chars"},
		{"max string", map[string]any{"id": id, "max_chars": "10"}, "max_chars must be an integer"},
		{"max float", map[string]any{"id": id, "max_chars": 1.5}, "max_chars must be an integer"},
		{"max null", map[string]any{"id": id, "max_chars": nil}, "max_chars must be an integer"},
		{"max bool", map[string]any{"id": id, "max_chars": true}, "max_chars must be an integer"},
		{"offset negative", map[string]any{"id": id, "offset": -1, "revision": rev}, "offset"},
		{"offset string", map[string]any{"id": id, "offset": "1"}, "offset must be an integer"},
		{"offset float", map[string]any{"id": id, "offset": 0.5}, "offset must be an integer"},
		{"offset null", map[string]any{"id": id, "offset": nil}, "offset must be an integer"},
		{"offset huge", map[string]any{"id": id, "offset": 1e30, "revision": rev}, "offset must be an integer"},
		{"offset needs revision", map[string]any{"id": id, "offset": 1}, "requires an explicit positive revision"},
		{"offset needs revision max", map[string]any{"id": id, "offset": 1, "max_chars": 4}, "requires an explicit positive revision"},
		{"offset zero revision", map[string]any{"id": id, "offset": 1, "revision": 0}, "revision must be a positive integer"},
		{"revision negative", map[string]any{"id": id, "max_chars": 5, "revision": -2}, "revision must be a positive integer"},
		{"revision zero", map[string]any{"id": id, "max_chars": 5, "revision": 0}, "revision must be a positive integer"},
		{"revision string", map[string]any{"id": id, "max_chars": 5, "revision": "1"}, "revision must be an integer"},
		{"revision null", map[string]any{"id": id, "max_chars": 5, "revision": nil}, "revision must be an integer"},
		{"revision missing", map[string]any{"id": id, "max_chars": 5, "revision": rev + 5}, "revision not found"},
		{"offset past end", map[string]any{"id": id, "offset": total + 1, "revision": rev}, "past the end"},
		{"outline true", map[string]any{"id": id, "max_chars": 5, "outline": true}, "outline"},
		{"outline string", map[string]any{"id": id, "max_chars": 5, "outline": "yes"}, "outline must be a boolean"},
		{"outline null", map[string]any{"id": id, "max_chars": 5, "outline": nil}, "outline must be a boolean"},
		{"anchor number", map[string]any{"id": id, "max_chars": 5, "anchor": 3}, "anchor must be a string"},
		{"anchor null", map[string]any{"id": id, "max_chars": 5, "anchor": nil}, "anchor must be a string"},
		{"anchor empty", map[string]any{"id": id, "max_chars": 5, "anchor": ""}, "anchor"},
		{"id missing", map[string]any{"max_chars": 5}, "id is required"},
		{"id bad", map[string]any{"id": "a/b", "max_chars": 5}, "id must contain"},
		{"article missing", map[string]any{"id": "no-such-article", "max_chars": 5}, "article not found"},
	}
	for _, c := range cases {
		isErr, text := callChunk(t, s, c.args)
		if !isErr || !strings.Contains(text, c.want) {
			t.Errorf("%s: isErr=%v text=%s (want %q)", c.name, isErr, text, c.want)
		}
	}
	// outline:false is harmless; offset exactly at end is fine
	chunkOK(t, s, map[string]any{"id": id, "max_chars": 5, "outline": false})
	if r := chunkOK(t, s, map[string]any{"id": id, "offset": total, "revision": rev}); r.Text != "" || r.HasMore {
		t.Fatalf("at end: %#v", r)
	}
}

func TestReadChunkDeletedMarker(t *testing.T) {
	s := testServer(t)
	id, rev := writeArticle(t, s, map[string]any{"space_id": "general", "title": "Gone", "body": "abcdef"})
	if _, err := s.DB.Exec("UPDATE articles SET deleted_at=? WHERE id=?", "2026-01-01T00:00:00Z", id); err != nil {
		t.Fatal(err)
	}
	isErr, text := callChunk(t, s, map[string]any{"id": id, "max_chars": 3, "revision": rev})
	if isErr || !strings.Contains(text, `"deleted_at":"2026-01-01T00:00:00Z"`) || !strings.Contains(text, `"text":"abc"`) {
		t.Fatalf("deleted: %v %s", isErr, text)
	}
}
