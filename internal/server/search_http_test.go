package server

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"testing"
)

type httpSearch struct {
	Total      int              `json:"total"`
	Offset     int              `json:"offset"`
	Mode       string           `json:"mode"`
	Results    []map[string]any `json:"results"`
	NextOffset *int             `json:"next_offset"`
}

func httpFind(t *testing.T, s *Server, qs string) (int, httpSearch, string) {
	t.Helper()
	code, b := request(t, s, "GET", "/api/search"+qs, "")
	var r httpSearch
	if code == 200 {
		if err := json.Unmarshal(b, &r); err != nil {
			t.Fatal(err)
		}
	}
	return code, r, string(b)
}

func TestSearchHTTPFormatAndSnippet(t *testing.T) {
	s := testServer(t)
	addArt(t, s, "h1", "Туннель WireGuard", "про сеть", "настроен wstunnel для доступа", []string{"net"}, map[string]string{"os": "linux"}, d1)
	code, r, raw := httpFind(t, s, "?q=wstunnel&space_id=general&kind=article")
	if code != 200 || r.Total != 1 || r.Mode != "all" || r.NextOffset != nil || len(r.Results) != 1 {
		t.Fatalf("search: %d %s", code, raw)
	}
	t.Logf("GET /api/search?q=wstunnel -> %s", strings.TrimSpace(raw))
	it := r.Results[0]
	for _, k := range []string{"id", "title", "space_id", "parent_id", "kind", "status", "summary", "tags", "updated_at", "score", "snippet", "matched_in"} {
		if _, ok := it[k]; !ok {
			t.Fatalf("missing %q in %s", k, raw)
		}
	}
	if it["id"] != "h1" || it["snippet"] == "" || len(it["matched_in"].([]any)) == 0 {
		t.Fatalf("item: %v", it)
	}
	if sc := it["score"].(float64); math.Abs(sc*100-math.Round(sc*100)) > 1e-9 {
		t.Fatalf("score not rounded: %v", sc)
	}
	if _, ok := it["body"]; ok {
		t.Fatal("body leaked")
	}
}

func TestSearchHTTPAnyModeAndNoQ(t *testing.T) {
	s := testServer(t)
	addArt(t, s, "n1", "Туннель", "", "wstunnel настроен", []string{"net"}, map[string]string{"os": "linux"}, "2026-01-01T00:00:00Z")
	addArt(t, s, "n2", "Прочее", "", "ничего", nil, nil, "2026-01-02T00:00:00Z")
	_, r, raw := httpFind(t, s, "?q=wstunnel+несуществующееслово&space_id=general&kind=article")
	if r.Mode != "any" || r.Total != 1 || r.Results[0]["id"] != "n1" {
		t.Fatalf("any: %s", raw)
	}
	_, r, raw = httpFind(t, s, "?space_id=general&kind=article")
	if r.Total != 2 || r.Mode != "" || r.Results[0]["id"] != "n2" {
		t.Fatalf("no q: %s", raw)
	}
	for _, k := range []string{"score", "snippet", "matched_in", "mode"} {
		if strings.Contains(raw, `"`+k+`"`) {
			t.Fatalf("%s must be omitted without q: %s", k, raw)
		}
	}
	if !strings.Contains(raw, `"tags":[]`) {
		t.Fatalf("tags must be []: %s", raw)
	}
	t.Logf("GET /api/search (no q) -> %s", strings.TrimSpace(raw))
	if _, r, raw = httpFind(t, s, "?space_id=general&kind=article&tag=net"); r.Total != 1 || r.Results[0]["id"] != "n1" {
		t.Fatalf("tag: %s", raw)
	}
	if _, r, raw = httpFind(t, s, "?space_id=general&kind=article&property=os:linux"); r.Total != 1 || r.Results[0]["id"] != "n1" {
		t.Fatalf("property: %s", raw)
	}
	if _, r, raw = httpFind(t, s, "?space_id=general&kind=plan"); r.Total != 0 || !strings.Contains(raw, `"results":[]`) {
		t.Fatalf("kind: %s", raw)
	}
	s.DB.Exec("UPDATE articles SET pinned=1 WHERE id='n2'")
	if _, r, raw = httpFind(t, s, "?space_id=general&kind=article&pinned=true"); r.Total != 1 || r.Results[0]["id"] != "n2" {
		t.Fatalf("pinned: %s", raw)
	}
	code, _, raw := httpFind(t, s, "?property=broken")
	if code != 400 || !strings.Contains(raw, `"error"`) {
		t.Fatalf("bad property: %d %s", code, raw)
	}
	if code, _ := request(t, s, "POST", "/api/search", ""); code != 405 {
		t.Fatalf("POST: %d", code)
	}
}

func TestSearchHTTPPagingAndLimits(t *testing.T) {
	s := testServer(t)
	for i := 0; i < 5; i++ {
		addArt(t, s, fmt.Sprintf("p%d", i), "Страница", "", "", nil, nil, fmt.Sprintf("2026-02-0%dT00:00:00Z", i+1))
	}
	_, r, raw := httpFind(t, s, "?space_id=general&kind=article&limit=2")
	if r.Total != 5 || len(r.Results) != 2 || r.NextOffset == nil || *r.NextOffset != 2 || r.Offset != 0 {
		t.Fatalf("page1: %s", raw)
	}
	_, r, raw = httpFind(t, s, "?space_id=general&kind=article&limit=2&offset=4")
	if len(r.Results) != 1 || r.NextOffset != nil || r.Offset != 4 || strings.Contains(raw, "next_offset") {
		t.Fatalf("last page: %s", raw)
	}
	_, r, _ = httpFind(t, s, "?space_id=general&kind=article&limit=0")
	if len(r.Results) != 5 {
		t.Fatalf("default limit: %d", len(r.Results))
	}
	for i := 5; i < 120; i++ {
		addArt(t, s, fmt.Sprintf("p%d", i), "Страница", "", "", nil, nil, "2026-03-01T00:00:00Z")
	}
	_, r, _ = httpFind(t, s, "?space_id=general&kind=article")
	if len(r.Results) != 30 || r.NextOffset == nil || *r.NextOffset != 30 {
		t.Fatalf("default 30: %d", len(r.Results))
	}
	_, r, _ = httpFind(t, s, "?space_id=general&kind=article&limit=1000")
	if len(r.Results) != 100 || r.Total != 120 {
		t.Fatalf("max 100: %d total=%d", len(r.Results), r.Total)
	}
	_, r, _ = httpFind(t, s, "?q=страница&kind=article&limit=1000")
	if len(r.Results) != 100 {
		t.Fatalf("max 100 with q: %d", len(r.Results))
	}
}
