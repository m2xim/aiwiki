package server

import (
	"strings"
	"testing"
)

// Exercise public dispatch, search addresses and saved-revision reads together.
func TestMCPSectionAddressSurvivesRename(t *testing.T) {
	s := testServer(t)
	body := "Introduction.\n\n## Guide\n" + strings.Repeat("Ж😀 usual text. ", 1200) + " LATE-SERVICE-91 evidence\n\n## Other\nUnrelated."
	id, rev := writeArticle(t, s, map[string]any{"space_id": "engineering", "title": "Integration fixture", "body": body})
	var hits struct {
		Total   int `json:"total"`
		Results []struct {
			ID       string         `json:"id"`
			Anchor   string         `json:"anchor"`
			Revision int            `json:"revision"`
			ReadArgs map[string]any `json:"read_args"`
		} `json:"results"`
	}
	mustOK(t, s, "wiki_search_sections", map[string]any{"q": "LATE-SERVICE-91", "space_id": "engineering"}, &hits)
	if hits.Total != 1 || len(hits.Results) != 1 {
		t.Fatalf("hits: %+v", hits)
	}
	h := hits.Results[0]
	if h.ID != id || h.Anchor != "guide" || h.Revision != rev {
		t.Fatalf("address: %+v", h)
	}
	mustOK(t, s, "wiki_edit", map[string]any{"id": id, "base_revision": rev, "op": "replace_text", "old": "## Guide", "text": "## Renamed"}, nil)
	var page struct {
		Text       string `json:"text"`
		Revision   int    `json:"revision"`
		Historical bool   `json:"historical"`
	}
	mustOK(t, s, "wiki_read", h.ReadArgs, &page)
	if !page.Historical || page.Revision != rev || !strings.Contains(page.Text, "LATE-SERVICE-91") {
		t.Fatalf("read: %+v", page)
	}
	var legacy struct {
		Section string `json:"section"`
	}
	mustOK(t, s, "wiki_read", map[string]any{"id": id, "anchor": "renamed"}, &legacy)
	if !strings.HasPrefix(legacy.Section, "## Renamed") {
		t.Fatalf("legacy response changed: %+v", legacy)
	}
}
