package server

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestWebCacheHeaders(t *testing.T) {
	web := t.TempDir()
	if err := os.MkdirAll(filepath.Join(web, "assets"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(web, "index.html"), []byte("<html></html>"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(web, "assets", "index-abc.js"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := testServer(t)
	s.WebDir = web
	for path, want := range map[string]string{
		"/":                    "no-cache",
		"/?article=x":          "no-cache",
		"/assets/index-abc.js": "public, max-age=31536000, immutable",
	} {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Host = "localhost:8090"
		s.Handler().ServeHTTP(rr, req)
		if rr.Code != http.StatusOK || rr.Header().Get("Cache-Control") != want {
			t.Fatalf("%s: code %d, Cache-Control %q, want %q", path, rr.Code, rr.Header().Get("Cache-Control"), want)
		}
	}
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/health", nil)
	req.Host = "localhost:8090"
	s.Handler().ServeHTTP(rr, req)
	if rr.Header().Get("Cache-Control") != "" {
		t.Fatalf("API must not get static cache headers: %q", rr.Header().Get("Cache-Control"))
	}
}
