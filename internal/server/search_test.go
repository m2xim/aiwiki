package server

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func addArt(t *testing.T, s *Server, id, title, summary, body string, tags []string, props map[string]string, updated string) {
	t.Helper()
	if tags == nil {
		tags = []string{}
	}
	if props == nil {
		props = map[string]string{}
	}
	a := Article{ID: id, SpaceID: "general", Title: title, Summary: summary, Body: body, Kind: "article", Status: "active", Tags: tags, Properties: props, Revision: 1, CreatedAt: updated, UpdatedAt: updated, Author: "test"}
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
}

func find(t *testing.T, s *Server, q string) SearchResult {
	t.Helper()
	return findWith(t, s, q, false)
}

// findAny is find with the partial-match fallback (MCP wiki_search behaviour).
func findAny(t *testing.T, s *Server, q string) SearchResult {
	t.Helper()
	return findWith(t, s, q, true)
}

func findWith(t *testing.T, s *Server, q string, allowAny bool) SearchResult {
	t.Helper()
	res, code, err := s.searchArticles(context.Background(), SearchParams{Q: q, SpaceID: "general", Limit: 50, AllowAny: allowAny})
	if err != nil || code != 200 {
		t.Fatalf("search %q: %d %v", q, code, err)
	}
	return res
}

func ids(r SearchResult) []string {
	var out []string
	for _, h := range r.Hits {
		out = append(out, h.Article.ID)
	}
	return out
}

func has(r SearchResult, id string) bool {
	for _, h := range r.Hits {
		if h.Article.ID == id {
			return true
		}
	}
	return false
}

const d1 = "2026-01-01T00:00:00Z"
const d2 = "2026-01-02T00:00:00Z"

func TestSearchMorphology(t *testing.T) {
	s := testServer(t)
	addArt(t, s, "m1", "Установка", "", "Это сборка проекта", nil, nil, d1)
	addArt(t, s, "m2", "Прочее", "", "Новый сервер запущен", nil, nil, d1)
	addArt(t, s, "m3", "Прочее2", "", "Два сервера в стойке", nil, nil, d1)
	addArt(t, s, "m4", "Прочее3", "", "Работа с серверами", nil, nil, d1)
	addArt(t, s, "m5", "Прочее4", "", "Утро в Тишины", nil, nil, d1)
	addArt(t, s, "m6", "Ёжик", "", "Всё хорошо, елка стоит", nil, nil, d1)
	if r := find(t, s, "сборки"); !has(r, "m1") || r.Mode != "all" {
		t.Fatalf("сборки: %v %s", ids(r), r.Mode)
	}
	r := find(t, s, "серверы")
	for _, id := range []string{"m2", "m3", "m4"} {
		if !has(r, id) {
			t.Fatalf("серверы missing %s: %v", id, ids(r))
		}
	}
	if r := find(t, s, "Тишин"); !has(r, "m5") {
		t.Fatalf("Тишин: %v", ids(r))
	}
	if r := find(t, s, "ежик"); !has(r, "m6") {
		t.Fatalf("ежик: %v", ids(r))
	}
	if r := find(t, s, "ёлка"); !has(r, "m6") {
		t.Fatalf("ёлка: %v", ids(r))
	}
	if r := find(t, s, "все"); !has(r, "m6") {
		t.Fatalf("все/всё: %v", ids(r))
	}
}

func TestSearchRankingTitleOverBodyAndWeak(t *testing.T) {
	s := testServer(t)
	addArt(t, s, "w-body", "Заметки", "", "Здесь описан граф связей и графа узлы", nil, nil, d2)
	addArt(t, s, "w-weak", "Люди", "", "Аграфена пришла вчера", nil, nil, d2)
	addArt(t, s, "w-title", "Граф связей", "", "Ничего особенного", nil, nil, d1)
	r := find(t, s, "граф")
	got := ids(r)
	if len(got) != 3 || got[0] != "w-title" || got[1] != "w-body" || got[2] != "w-weak" {
		t.Fatalf("order: %v", got)
	}
	if r.Hits[0].Score <= r.Hits[1].Score || r.Hits[1].Score <= r.Hits[2].Score {
		t.Fatalf("scores: %v %v %v", r.Hits[0].Score, r.Hits[1].Score, r.Hits[2].Score)
	}
	if r.Hits[0].Article.Body != "" {
		t.Fatal("body must be empty")
	}
	if r.Hits[0].MatchedIn[0] != "title" {
		t.Fatalf("matched_in: %v", r.Hits[0].MatchedIn)
	}
}

func TestSearchTieBreakByDate(t *testing.T) {
	s := testServer(t)
	addArt(t, s, "t-old", "Кролик", "", "", nil, nil, d1)
	addArt(t, s, "t-new", "Кролик", "", "", nil, nil, d2)
	if got := ids(find(t, s, "кролик")); len(got) != 2 || got[0] != "t-new" {
		t.Fatalf("tie: %v", got)
	}
}

func TestSearchProperties(t *testing.T) {
	s := testServer(t)
	addArt(t, s, "p1", "Сервер", "", "", nil, map[string]string{"режим": "бэкап"}, d1)
	addArt(t, s, "p2", "Другое", "", "", nil, nil, d1)
	r := find(t, s, "бэкап")
	if len(r.Hits) != 1 || r.Hits[0].Article.ID != "p1" || r.Hits[0].MatchedIn[0] != "properties" {
		t.Fatalf("props: %+v", r)
	}
}

func TestSearchAnyMode(t *testing.T) {
	s := testServer(t)
	addArt(t, s, "a1", "Туннель", "", "wstunnel настроен", nil, nil, d1)
	addArt(t, s, "a2", "Прочее", "", "ничего", nil, nil, d1)
	r := findAny(t, s, "wstunnel несуществующееслово")
	if r.Mode != "any" || !has(r, "a1") || r.Total != 1 {
		t.Fatalf("any: %+v", r)
	}
	// REST contract: without AllowAny every term must occur, otherwise nothing.
	if r := find(t, s, "wstunnel несуществующееслово"); r.Mode != "all" || r.Total != 0 || len(r.Hits) != 0 {
		t.Fatalf("all-only: %+v", r)
	}
	if r := find(t, s, "wstunnel настроен"); r.Mode != "all" || !has(r, "a1") {
		t.Fatalf("all: %+v", r)
	}
}

func TestSearchSubstringAndHyphen(t *testing.T) {
	s := testServer(t)
	addArt(t, s, "s1", "Сеть", "wg-quick up", "адрес 192.168.1.5 и путь /api/x/y", nil, nil, d1)
	for _, q := range []string{"192.168", "/api/x", "wg", "quick", "wg-quick", "дрес"} {
		if r := find(t, s, q); !has(r, "s1") {
			t.Fatalf("%q not found", q)
		}
	}
}

func TestSearchPaging(t *testing.T) {
	s := testServer(t)
	for i := 0; i < 5; i++ {
		addArt(t, s, fmt.Sprintf("pg%d", i), "Страница", "", "", nil, nil, fmt.Sprintf("2026-02-0%dT00:00:00Z", i+1))
	}
	res, _, err := s.searchArticles(context.Background(), SearchParams{Q: "страница", SpaceID: "general", Limit: 2, Offset: 1})
	if err != nil || res.Total != 5 || len(res.Hits) != 2 || res.Hits[0].Article.ID != "pg3" || res.Hits[1].Article.ID != "pg2" {
		t.Fatalf("paging: %+v %v", ids(res), err)
	}
	res, _, _ = s.searchArticles(context.Background(), SearchParams{Q: "страница", SpaceID: "general", Limit: 2, Offset: 9})
	if res.Total != 5 || len(res.Hits) != 0 {
		t.Fatalf("offset beyond: %+v", res)
	}
	// Without Q: unchanged behaviour.
	res, _, _ = s.searchArticles(context.Background(), SearchParams{SpaceID: "general", Kind: "article", Limit: 3})
	if res.Mode != "" || res.Total != 5 || len(res.Hits) != 3 || res.Hits[0].Article.ID != "pg4" {
		t.Fatalf("no q: %+v", res)
	}
}

func TestSearchSnippet(t *testing.T) {
	s := testServer(t)
	filler := strings.Repeat("Длинный текст про ёжиков и яблоки.\nЕщё строка. ", 40)
	body := filler + " Здесь находится редкий маркер ШИФРОВАНИЕ данных. " + filler
	addArt(t, s, "sn1", "Статья", "", body, nil, nil, d1)
	addArt(t, s, "sn2", "Заголовок шифрование", "", "пусто", nil, nil, d1)
	addArt(t, s, "sn3", "Прочая", "краткое шифрование тут", "", nil, nil, d1)
	r := find(t, s, "шифрования")
	for _, h := range r.Hits {
		if !utf8.ValidString(h.Snippet) {
			t.Fatalf("invalid utf8: %q", h.Snippet)
		}
		if utf8.RuneCountInString(h.Snippet) > 240 || strings.ContainsAny(h.Snippet, "\n\r") {
			t.Fatalf("bad snippet %q", h.Snippet)
		}
		switch h.Article.ID {
		case "sn1":
			if !strings.Contains(strings.ToLower(h.Snippet), "шифрование") || !strings.HasPrefix(h.Snippet, "…") || !strings.HasSuffix(h.Snippet, "…") {
				t.Fatalf("sn1 snippet %q", h.Snippet)
			}
		case "sn2":
			if h.Snippet != "" {
				t.Fatalf("title-only snippet must be empty: %q", h.Snippet)
			}
		case "sn3":
			if !strings.Contains(h.Snippet, "шифрование") {
				t.Fatalf("sn3 snippet %q", h.Snippet)
			}
		}
	}
	if len(r.Hits) != 3 {
		t.Fatalf("hits: %v", ids(r))
	}
}

func syntheticCorpus(n, perArticle int) []Article {
	words := strings.Fields("сервер настройка сборка проекта туннель адрес сеть клиент запрос ответ база данных файл статья граф связь агент поиск индекс токен ключ порт маршрут правило фильтр пакет версия релиз тест ошибка журнал канал сообщение пользователь доступ права группа роль процесс служба конфигурация обновление резервная копия восстановление wireguard nginx docker systemd backup server config deploy")
	var out []Article
	seed := uint32(12345)
	for i := 0; i < n; i++ {
		var b strings.Builder
		for b.Len() < perArticle {
			seed = seed*1664525 + 1013904223
			w := words[int(seed>>16)%len(words)]
			if seed%7 == 0 {
				w += "ы"
			}
			b.WriteString(w)
			if seed%13 == 0 {
				b.WriteString(".\n")
			} else {
				b.WriteByte(' ')
			}
		}
		out = append(out, Article{ID: fmt.Sprintf("b%d", i), SpaceID: "general", Title: fmt.Sprintf("Статья %d про сервер", i), Body: b.String(), Tags: []string{"tag"}, Properties: map[string]string{"k": "v"}, UpdatedAt: d1})
	}
	return out
}

func TestSearchPerformanceSynthetic(t *testing.T) {
	arts := syntheticCorpus(250, 14000) // ~2M characters (Cyrillic, 2 bytes each in UTF-8 => ~4 MB)
	total := 0
	for _, a := range arts {
		total += len([]rune(a.Body))
	}
	m := newMatcher("резервные копии серверов")
	start := time.Now()
	var cands []scored
	for _, a := range arts {
		if sc, ok := m.rankArticle(a); ok {
			cands = append(cands, sc)
		}
	}
	res := m.rankAndPage(cands, SearchParams{Limit: 20})
	el := time.Since(start)
	t.Logf("corpus %d chars, %d hits, mode %s, rank+page time %v", total, res.Total, res.Mode, el)
	if el > 500*time.Millisecond {
		t.Fatalf("too slow: %v", el)
	}
}

func BenchmarkSearchSynthetic(b *testing.B) {
	arts := syntheticCorpus(250, 14000)
	for i := 0; i < b.N; i++ {
		m := newMatcher("резервные копии серверов")
		var cands []scored
		for _, a := range arts {
			if sc, ok := m.rankArticle(a); ok {
				cands = append(cands, sc)
			}
		}
		m.rankAndPage(cands, SearchParams{Limit: 20})
	}
}

// BenchmarkSnippetLargeBody: 100K-rune body in which the query word is very
// frequent, 20 hits on the page (snippet cost must be linear in body size).
func BenchmarkSnippetLargeBody(b *testing.B) {
	var sb strings.Builder
	for sb.Len() < 200000 { // ~100K Cyrillic runes
		sb.WriteString("сервер работает ")
	}
	body := sb.String()
	m := newMatcher("сервер")
	var cands []scored
	for i := 0; i < 20; i++ {
		a := Article{ID: fmt.Sprintf("big%d", i), Title: "Большая", Body: body}
		if sc, ok := m.rankArticle(a); ok {
			cands = append(cands, sc)
		}
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		res := m.rankAndPage(cands, SearchParams{Limit: 20})
		if len(res.Hits) != 20 || res.Hits[0].Snippet == "" {
			b.Fatal("bad result")
		}
	}
}
