package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func favList(t *testing.T, s *Server) []map[string]any {
	t.Helper()
	code, b := request(t, s, "GET", "/api/favorites", "")
	if code != 200 {
		t.Fatalf("list: %d %s", code, b)
	}
	if strings.TrimSpace(string(b)) == "null" {
		t.Fatal("favorites list is null")
	}
	var out []map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestFavoritesLifecycle(t *testing.T) {
	s := testServer(t)
	if code, b := request(t, s, "GET", "/api/favorites", ""); code != 200 || strings.TrimSpace(string(b)) != "[]" {
		t.Fatalf("empty: %d %s", code, b)
	}
	addArt(t, s, "fa", "Alpha", "sum-a", "", nil, nil, d1)
	addArt(t, s, "fb", "Beta", "", "", nil, nil, d1)
	for _, id := range []string{"fa", "fb"} {
		if code, b := request(t, s, "PUT", "/api/favorites/"+id, ""); code != 204 || len(b) != 0 {
			t.Fatalf("put %s: %d %s", id, code, b)
		}
	}
	// make timestamps distinct and old, then verify repeat PUT keeps created_at
	s.DB.Exec("UPDATE favorites SET created_at='2026-01-01T00:00:00Z' WHERE article_id='fa'")
	s.DB.Exec("UPDATE favorites SET created_at='2026-01-02T00:00:00Z' WHERE article_id='fb'")
	if code, _ := request(t, s, "PUT", "/api/favorites/fa", ""); code != 204 {
		t.Fatalf("repeat put: %d", code)
	}
	l := favList(t, s)
	if len(l) != 2 || l[0]["id"] != "fb" || l[1]["id"] != "fa" || l[1]["favorited_at"] != "2026-01-01T00:00:00Z" {
		t.Fatalf("order/idempotence: %v", l)
	}
	if l[1]["title"] != "Alpha" || l[1]["summary"] != "sum-a" || l[1]["space_id"] != "general" || l[1]["parent_id"] != "" || l[1]["kind"] != "article" || l[1]["status"] != "active" || l[1]["updated_at"] != d1 {
		t.Fatalf("fields: %v", l[1])
	}
	t.Logf("GET /api/favorites -> %v", l)
	if code, _ := request(t, s, "DELETE", "/api/favorites/fa", ""); code != 204 {
		t.Fatalf("delete: %d", code)
	}
	if code, _ := request(t, s, "DELETE", "/api/favorites/fa", ""); code != 204 {
		t.Fatalf("repeat delete: %d", code)
	}
	if code, _ := request(t, s, "DELETE", "/api/favorites/never-existed", ""); code != 204 {
		t.Fatalf("delete unknown: %d", code)
	}
	if l := favList(t, s); len(l) != 1 || l[0]["id"] != "fb" {
		t.Fatalf("after delete: %v", l)
	}
}

func TestFavoritesErrors(t *testing.T) {
	s := testServer(t)
	addArt(t, s, "fx", "X", "", "", nil, nil, d1)
	code, b := request(t, s, "PUT", "/api/favorites/unknown", "")
	if code != 404 || !strings.Contains(string(b), `"error"`) {
		t.Fatalf("unknown: %d %s", code, b)
	}
	t.Logf("PUT unknown -> %d %s", code, strings.TrimSpace(string(b)))
	if code, _ := request(t, s, "PUT", "/api/favorites/bad.id", ""); code != 400 {
		t.Fatalf("bad id: %d", code)
	}
	if code, _ := request(t, s, "DELETE", "/api/favorites/bad%20id", ""); code != 400 {
		t.Fatalf("bad id delete: %d", code)
	}
	for _, c := range []struct{ m, p string }{{"POST", "/api/favorites/fx"}, {"GET", "/api/favorites/fx"}, {"PATCH", "/api/favorites/fx"}, {"POST", "/api/favorites"}, {"PUT", "/api/favorites"}, {"DELETE", "/api/favorites"}} {
		if code, _ := request(t, s, c.m, c.p, ""); code != 405 {
			t.Fatalf("%s %s: %d", c.m, c.p, code)
		}
	}
	// deleted article: list hides it, PUT -> 409
	request(t, s, "PUT", "/api/favorites/fx", "")
	s.DB.Exec("UPDATE articles SET deleted_at='2026-01-05T00:00:00Z' WHERE id='fx'")
	if l := favList(t, s); len(l) != 0 {
		t.Fatalf("deleted still listed: %v", l)
	}
	code, b = request(t, s, "PUT", "/api/favorites/fx", "")
	if code != 409 {
		t.Fatalf("deleted put: %d %s", code, b)
	}
	t.Logf("PUT deleted -> %d %s", code, strings.TrimSpace(string(b)))
}

func TestFavoritesArchivedSpaceHiddenAndExport(t *testing.T) {
	s := testServer(t)
	addArt(t, s, "g1", "General one", "", "", nil, nil, d1)
	addArt(t, s, "e1", "Eng one", "", "", nil, nil, d1)
	s.DB.Exec("UPDATE articles SET space_id='engineering' WHERE id='e1'")
	request(t, s, "PUT", "/api/favorites/g1", "")
	request(t, s, "PUT", "/api/favorites/e1", "")
	if l := favList(t, s); len(l) != 2 {
		t.Fatalf("before archive: %v", l)
	}
	if code, b := request(t, s, "PATCH", "/api/spaces/engineering", `{"archived":true}`); code != 200 {
		t.Fatalf("archive: %d %s", code, b)
	}
	if l := favList(t, s); len(l) != 1 || l[0]["id"] != "g1" {
		t.Fatalf("archived visible: %v", l)
	}
	code, b := request(t, s, "GET", "/api/export", "")
	var exp struct {
		Favorites []map[string]string `json:"favorites"`
	}
	if code != 200 || json.Unmarshal(b, &exp) != nil || len(exp.Favorites) != 2 || exp.Favorites[0]["article_id"] == "" || exp.Favorites[0]["created_at"] == "" {
		t.Fatalf("export: %d favorites=%v", code, exp.Favorites)
	}
	t.Logf("export favorites -> %v", exp.Favorites)
	request(t, s, "DELETE", "/api/favorites/g1", "")
	request(t, s, "DELETE", "/api/favorites/e1", "")
	_, b = request(t, s, "GET", "/api/export", "")
	if !strings.Contains(string(b), `"favorites":[]`) {
		t.Fatalf("export favorites must be [] not null: %s", b[len(b)-200:])
	}
}

func TestForeignOriginCannotChangeFavorites(t *testing.T) {
	s := testServer(t)
	addArt(t, s, "fo", "O", "", "", nil, nil, d1)
	for _, m := range []string{http.MethodPut, http.MethodDelete} {
		r := httptest.NewRequest(m, "/api/favorites/fo", nil)
		r.Host = "localhost:8090"
		r.Header.Set("Origin", "https://attacker.example")
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		if w.Code != http.StatusForbidden {
			t.Fatalf("%s foreign origin: %d", m, w.Code)
		}
	}
	var n int
	s.DB.QueryRow("SELECT count(*) FROM favorites").Scan(&n)
	if n != 0 {
		t.Fatalf("foreign origin wrote %d favorites", n)
	}
	// local origin works
	r := httptest.NewRequest(http.MethodPut, "/api/favorites/fo", nil)
	r.Host = "localhost:8090"
	r.Header.Set("Origin", "http://localhost:5173")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != 204 {
		t.Fatalf("local origin: %d", w.Code)
	}
}
