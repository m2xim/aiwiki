package server

import (
	"encoding/json"
	"fmt"
	"testing"
)

func TestTrustedCatalog(t *testing.T) {
	s := testServer(t)
	if _, err := s.DB.Exec("INSERT INTO spaces(id,name,description) VALUES('arch','Архив','old'),('empty','Пустое','Без статей'),('work','Рабочее','d')"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec("UPDATE spaces SET archived_at='2026-01-01T00:00:00Z' WHERE id='arch'"); err != nil {
		t.Fatal(err)
	}
	base := Article{SpaceID: "work", Kind: "article", Status: "active", Properties: map[string]string{}, Revision: 1, CreatedAt: "2026-01-01T00:00:00Z", Author: "test"}
	tx, err := s.DB.Begin()
	if err != nil {
		t.Fatal(err)
	}
	add := func(id, updated string, tags []string) {
		a := base
		a.ID, a.Title, a.UpdatedAt, a.Tags = id, id, updated, tags
		if err := insertArticle(tx, a, "fixture"); err != nil {
			t.Fatal(err)
		}
	}
	add("a1", "2026-02-01T00:00:00Z", []string{"beta", "alpha"})
	add("a2", "2026-03-01T00:00:00Z", []string{"alpha", "gamma"})
	add("gone", "2026-09-01T00:00:00Z", []string{"deleted-only"})
	many := make([]string, 0, 30)
	for i := 0; i < 30; i++ {
		many = append(many, fmt.Sprintf("t%02d", i))
	}
	add("a3", "2026-01-15T00:00:00Z", many)
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec("UPDATE articles SET deleted_at='2026-09-02T00:00:00Z' WHERE id='gone'"); err != nil {
		t.Fatal(err)
	}
	code, body := request(t, s, "GET", "/api/catalog", "")
	if code != 200 {
		t.Fatalf("catalog: %d %s", code, body)
	}
	var spaces []struct {
		ID            string   `json:"id"`
		ArticleCount  int      `json:"article_count"`
		LastUpdated   string   `json:"last_updated"`
		Tags          []string `json:"tags"`
		Personal      bool     `json:"personal"`
		EffectiveRole string   `json:"effective_role"`
		Projects      []any    `json:"projects"`
	}
	if err := json.Unmarshal(body, &spaces); err != nil {
		t.Fatal(err)
	}
	byID := map[string]int{}
	for i, sp := range spaces {
		byID[sp.ID] = i
		if sp.Personal || sp.EffectiveRole != "owner" || sp.Projects == nil || len(sp.Projects) != 0 {
			t.Fatalf("space contract: %+v", sp)
		}
	}
	if _, ok := byID["arch"]; ok {
		t.Fatalf("archived space returned: %s", body)
	}
	g := spaces[byID["work"]]
	if g.ArticleCount != 3 || g.LastUpdated < "2026-03-01T00:00:00Z" || g.LastUpdated >= "2026-09-01" {
		t.Fatalf("work: %+v", g)
	}
	seen := map[string]bool{}
	for _, tag := range g.Tags {
		if seen[tag] || tag == "deleted-only" {
			t.Fatalf("tags: %v", g.Tags)
		}
		seen[tag] = true
	}
	if len(g.Tags) != 20 {
		t.Fatalf("tag limit: %v", g.Tags)
	}
	e := spaces[byID["empty"]]
	if e.ArticleCount != 0 || e.LastUpdated != "" || len(e.Tags) != 0 {
		t.Fatalf("empty: %+v", e)
	}
}
