package server

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// memStore is an in-memory objectStore for the PostgreSQL fixtures.
type memStore struct {
	mu sync.Mutex
	m  map[string][]byte
}

func newMemStore() *memStore { return &memStore{m: map[string][]byte{}} }
func (m *memStore) Put(_ context.Context, key string, body io.ReadSeeker, _ int64, _ string) error {
	b, err := io.ReadAll(body)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.m[key] = b
	return err
}
func (m *memStore) Get(_ context.Context, key string) (io.ReadCloser, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.m[key]
	if !ok {
		return nil, errObjectNotFound
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}
func (m *memStore) Delete(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.m, key)
	return nil
}

type fileSpec struct {
	Path     string `json:"path"`
	Content  string `json:"content,omitempty"`
	Encoding string `json:"encoding,omitempty"`
	Delete   bool   `json:"delete,omitempty"`
}

const exactFrameCSP = "sandbox allow-scripts allow-modals; default-src 'none'; script-src 'self' 'unsafe-inline'; style-src 'self' 'unsafe-inline'; img-src 'self' data: blob:; font-src 'self' data:; media-src 'self' data: blob:; connect-src 'none'; form-action 'none'; base-uri 'none'; webrtc 'block'; frame-ancestors 'self'"

// artCall performs a request through the full handler stack of s.
func artCall(t *testing.T, s *Server, method, target string, body any) (int, http.Header, []byte) {
	t.Helper()
	var rd io.Reader
	switch v := body.(type) {
	case nil:
	case string:
		rd = strings.NewReader(v)
	default:
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		rd = bytes.NewReader(raw)
	}
	r := httptest.NewRequest(method, target, rd)
	r.Host = "127.0.0.1:8090"
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	return w.Code, w.Result().Header, w.Body.Bytes()
}

func decodeJSON(t *testing.T, raw []byte, dst any) {
	t.Helper()
	if err := json.Unmarshal(raw, dst); err != nil {
		t.Fatalf("%v: %s", err, raw)
	}
}

func basicFiles() []fileSpec {
	return []fileSpec{{Path: "index.html", Content: `<!doctype html><title>Калькулятор</title><h1>Привет мир</h1><script src="app.js"></script>`}, {Path: "app.js", Content: "window.x=1"}}
}

func createArtifactT(t *testing.T, s *Server, extra map[string]any) artifactDetail {
	t.Helper()
	body := map[string]any{"space_id": "general", "title": "Мой артефакт", "description": "Описание", "files": basicFiles(), "message": "первая"}
	for k, v := range extra {
		body[k] = v
	}
	code, _, raw := artCall(t, s, "POST", "/api/artifacts", body)
	if code != 201 {
		t.Fatalf("create: %d %s", code, raw)
	}
	var d artifactDetail
	decodeJSON(t, raw, &d)
	return d
}

func TestArtifactLifecycleTrusted(t *testing.T) {
	s := testServer(t)
	d := createArtifactT(t, s, nil)
	if d.Revision != 1 || d.FileCount != 2 || len(d.Files) != 2 || !d.CanEdit || !d.CanShare {
		t.Fatalf("created: %+v", d)
	}
	if d.Files[0].Path != "app.js" || len(d.Files[0].SHA256) != 64 || d.Files[0].MIME != "text/javascript" {
		t.Fatalf("manifest: %+v", d.Files)
	}
	// partial update: only app.js changes, index.html and the title are kept
	code, _, raw := artCall(t, s, "PUT", "/api/artifacts/"+d.ID, map[string]any{"base_revision": 1, "files": []fileSpec{{Path: "app.js", Content: "window.x=2"}, {Path: "style.css", Content: "body{}"}}, "message": "правка"})
	if code != 200 {
		t.Fatalf("update: %d %s", code, raw)
	}
	var u artifactDetail
	decodeJSON(t, raw, &u)
	if u.Revision != 2 || u.Title != "Мой артефакт" || len(u.Files) != 3 {
		t.Fatalf("updated: %+v", u)
	}
	// stale base revision
	code, _, raw = artCall(t, s, "PUT", "/api/artifacts/"+d.ID, map[string]any{"base_revision": 1, "title": "stale"})
	var conflict map[string]any
	decodeJSON(t, raw, &conflict)
	if code != 409 || conflict["current_revision"].(float64) != 2 {
		t.Fatalf("conflict: %d %s", code, raw)
	}
	// unknown field is an error, not silently ignored
	if code, _, _ = artCall(t, s, "PUT", "/api/artifacts/"+d.ID, `{"base_revision":2,"fiels":[]}`); code != 400 {
		t.Fatalf("unknown field accepted: %d", code)
	}
	// an update that changes nothing records no revision
	code, _, raw = artCall(t, s, "PUT", "/api/artifacts/"+d.ID, map[string]any{"base_revision": 2, "files": []fileSpec{{Path: "app.js", Content: "window.x=2"}}})
	decodeJSON(t, raw, &u)
	if code != 200 || u.Revision != 2 {
		t.Fatalf("noop: %d %s", code, raw)
	}
	// delete a file; index.html cannot be deleted
	if code, _, _ = artCall(t, s, "PUT", "/api/artifacts/"+d.ID, map[string]any{"base_revision": 2, "files": []fileSpec{{Path: "index.html", Delete: true}}}); code != 400 {
		t.Fatalf("index.html deleted: %d", code)
	}
	if code, _, _ = artCall(t, s, "PUT", "/api/artifacts/"+d.ID, map[string]any{"base_revision": 2, "files": []fileSpec{{Path: "nope.js", Delete: true}}}); code != 400 {
		t.Fatalf("deleting a missing file: %d", code)
	}
	code, _, raw = artCall(t, s, "PUT", "/api/artifacts/"+d.ID, map[string]any{"base_revision": 2, "files": []fileSpec{{Path: "style.css", Delete: true}}})
	decodeJSON(t, raw, &u)
	if code != 200 || u.Revision != 3 || len(u.Files) != 2 {
		t.Fatalf("file delete: %d %s", code, raw)
	}
	// old revision still readable
	code, _, raw = artCall(t, s, "GET", "/api/artifacts/"+d.ID+"?revision=2", nil)
	var old artifactDetail
	decodeJSON(t, raw, &old)
	if code != 200 || len(old.Files) != 3 {
		t.Fatalf("revision 2: %d %s", code, raw)
	}
	// soft delete and restore
	if code, _, _ = artCall(t, s, "DELETE", "/api/artifacts/"+d.ID, map[string]any{"base_revision": 2}); code != 409 {
		t.Fatalf("delete with stale revision: %d", code)
	}
	code, _, raw = artCall(t, s, "DELETE", "/api/artifacts/"+d.ID, map[string]any{"base_revision": 3})
	var del artifactDetail
	decodeJSON(t, raw, &del)
	if code != 200 || del.DeletedAt == "" || del.Revision != 4 {
		t.Fatalf("delete: %d %s", code, raw)
	}
	_, _, raw = artCall(t, s, "GET", "/api/artifacts", nil)
	if strings.Contains(string(raw), d.ID) {
		t.Fatal("deleted artifact listed")
	}
	if code, _, _ = artCall(t, s, "PUT", "/api/artifacts/"+d.ID, map[string]any{"base_revision": 4, "title": "x"}); code != 409 {
		t.Fatalf("update of a deleted artifact: %d", code)
	}
	if code, _, _ = artCall(t, s, "POST", "/api/artifacts/"+d.ID+"/view", ""); code != 404 {
		t.Fatalf("view of a deleted artifact: %d", code)
	}
	code, _, raw = artCall(t, s, "POST", "/api/artifacts/"+d.ID+"/restore", map[string]any{"base_revision": 4})
	del = artifactDetail{}
	decodeJSON(t, raw, &del)
	if code != 200 || del.DeletedAt != "" || del.Revision != 5 {
		t.Fatalf("restore: %d %s", code, raw)
	}
	code, _, raw = artCall(t, s, "GET", "/api/artifacts/"+d.ID+"/history", nil)
	var hist []map[string]any
	decodeJSON(t, raw, &hist)
	if code != 200 || len(hist) != 5 || hist[0]["revision"].(float64) != 5 || hist[4]["message"] != "первая" || hist[4]["file_count"].(float64) != 2 {
		t.Fatalf("history: %d %s", code, raw)
	}
	// metadata-only update and visibility are allowed in trusted mode
	code, _, raw = artCall(t, s, "PUT", "/api/artifacts/"+d.ID, map[string]any{"base_revision": 5, "title": "Новое имя", "visibility": "private"})
	decodeJSON(t, raw, &u)
	if code != 200 || u.Title != "Новое имя" || u.Visibility != "private" || u.Revision != 6 {
		t.Fatalf("metadata: %d %s", code, raw)
	}
}

func TestArtifactLimitsAndPaths(t *testing.T) {
	s := testServer(t)
	post := func(files []fileSpec) int {
		code, _, _ := artCall(t, s, "POST", "/api/artifacts", map[string]any{"space_id": "general", "title": "t", "files": files})
		return code
	}
	index := fileSpec{Path: "index.html", Content: "<p>x"}
	bad := []string{"../x.js", "a/../b.js", "/abs.js", `a\b.js`, "a//b.js", ".hidden.js", "a/.b.js", "a..b.js", "noext", "x.exe", "x.php", "a/b/c/d/e.js", "x.js.", "sp ace.js", "ü.js", "index.html/x.js", strings.Repeat("a", 201) + ".js", ""}
	for _, p := range bad {
		if code := post([]fileSpec{index, {Path: p, Content: "x"}}); code != 400 {
			t.Errorf("path %q accepted: %d", p, code)
		}
	}
	good := []string{"a.js", "dir/a.mjs", "a/b/c/d.css", "Data-1_2.JSON", "p.PNG", "x.jpeg", "x.webp", "x.gif", "x.svg", "x.csv", "x.txt", "x.md"}
	for _, p := range good {
		if code := post([]fileSpec{index, {Path: p, Content: "x"}}); code != 201 {
			t.Errorf("path %q rejected: %d", p, code)
		}
	}
	if code := post([]fileSpec{{Path: "a.js", Content: "x"}}); code != 400 {
		t.Errorf("missing index.html accepted: %d", code)
	}
	if code := post([]fileSpec{index, {Path: "Index.html", Content: "x"}}); code != 400 {
		t.Errorf("case-duplicate accepted: %d", code)
	}
	if code := post([]fileSpec{index, {Path: "a.js", Content: "x"}, {Path: "a.js", Content: "y"}}); code != 400 {
		t.Errorf("duplicate accepted: %d", code)
	}
	if code := post([]fileSpec{index, {Path: "a.js", Content: "x"}, {Path: "a.js/b.js", Content: "y"}}); code != 400 {
		t.Errorf("file/dir clash accepted: %d", code)
	}
	if code := post([]fileSpec{index, {Path: "a.png", Content: "!!!", Encoding: "base64"}}); code != 400 {
		t.Errorf("bad base64 accepted: %d", code)
	}
	if code := post([]fileSpec{index, {Path: "a.png", Content: "AAAA", Encoding: "hex"}}); code != 400 {
		t.Errorf("bad encoding accepted: %d", code)
	}
	if code := post([]fileSpec{index, {Path: "a.png", Content: base64.StdEncoding.EncodeToString([]byte{0x89, 'P', 'N', 'G'}), Encoding: "base64"}}); code != 201 {
		t.Errorf("good base64 rejected: %d", code)
	}
	// sizes
	if code := post([]fileSpec{index, {Path: "big.txt", Content: strings.Repeat("a", 5<<20)}}); code != 201 {
		t.Errorf("5 MB file rejected: %d", code)
	}
	if code := post([]fileSpec{index, {Path: "big.txt", Content: strings.Repeat("a", 5<<20+1)}}); code != 413 {
		t.Errorf("file over 5 MB accepted: %d", code)
	}
	var many []fileSpec
	many = append(many, index)
	for i := 0; i < 49; i++ {
		many = append(many, fileSpec{Path: "f" + string(rune('a'+i/10)) + string(rune('a'+i%10)) + ".txt", Content: "x"})
	}
	if code := post(many); code != 201 {
		t.Errorf("50 files rejected: %d", code)
	}
	if code := post(append(many, fileSpec{Path: "one-more.txt", Content: "x"})); code != 400 && code != 413 {
		t.Errorf("51 files accepted: %d", code)
	}
	var heavy []fileSpec
	heavy = append(heavy, index)
	for i := 0; i < 5; i++ {
		heavy = append(heavy, fileSpec{Path: "h" + string(rune('a'+i)) + ".txt", Content: strings.Repeat("b", 5<<20)})
	}
	if code := post(heavy); code != 413 {
		t.Errorf("revision over 20 MB accepted: %d", code)
	}
	if code, _, _ := artCall(t, s, "POST", "/api/artifacts", map[string]any{"space_id": "nope", "title": "t", "files": []fileSpec{index}}); code != 404 {
		t.Errorf("unknown space: %d", code)
	}
	if code, _, _ := artCall(t, s, "POST", "/api/artifacts", map[string]any{"space_id": "general", "title": " ", "files": []fileSpec{index}}); code != 400 {
		t.Errorf("empty title: %d", code)
	}
	if code, _, _ := artCall(t, s, "POST", "/api/artifacts", map[string]any{"space_id": "general", "title": "t", "visibility": "public", "files": []fileSpec{index}}); code != 400 {
		t.Errorf("bad visibility: %d", code)
	}
}

func frameURL(t *testing.T, s *Server, id string, body any) string {
	t.Helper()
	code, _, raw := artCall(t, s, "POST", "/api/artifacts/"+id+"/view", body)
	if code != 200 {
		t.Fatalf("view: %d %s", code, raw)
	}
	var v struct {
		FrameURL  string `json:"frame_url"`
		ExpiresAt string `json:"expires_at"`
	}
	decodeJSON(t, raw, &v)
	exp, err := time.Parse(time.RFC3339, v.ExpiresAt)
	if err != nil || time.Until(exp) > artifactCapTTL || time.Until(exp) < 14*time.Minute {
		t.Fatalf("expiry: %s", v.ExpiresAt)
	}
	if !strings.HasPrefix(v.FrameURL, "/artifact-frame/") || !strings.HasSuffix(v.FrameURL, "/index.html") {
		t.Fatalf("frame url: %s", v.FrameURL)
	}
	return v.FrameURL
}

func TestArtifactFrameHeadersAndCapability(t *testing.T) {
	if got := artifactFrameCSP("'self'"); got != exactFrameCSP {
		t.Fatalf("CSP:\n%s\nwant:\n%s", got, exactFrameCSP)
	}
	s := testServer(t)
	d := createArtifactT(t, s, nil)
	u := frameURL(t, s, d.ID, nil)
	code, h, body := artCall(t, s, "GET", u, nil)
	if code != 200 || !strings.Contains(string(body), "Привет мир") {
		t.Fatalf("frame: %d %s", code, body)
	}
	want := map[string]string{
		"Content-Security-Policy":      exactFrameCSP,
		"X-Content-Type-Options":       "nosniff",
		"Referrer-Policy":              "no-referrer",
		"Cross-Origin-Resource-Policy": "cross-origin",
		"Permissions-Policy":           "camera=(), microphone=(), geolocation=(), payment=(), usb=(), clipboard-read=()",
		"Content-Type":                 "text/html; charset=utf-8",
	}
	for k, v := range want {
		if h.Get(k) != v {
			t.Errorf("%s = %q, want %q", k, h.Get(k), v)
		}
	}
	cc := h.Get("Cache-Control")
	if !strings.HasPrefix(cc, "private, max-age=") || cc == "private, max-age=0" {
		t.Errorf("Cache-Control %q", cc)
	}
	if h.Get("Set-Cookie") != "" {
		t.Error("frame sets a cookie")
	}
	// the theme query is ignored; sub-files resolve against the capability path
	if code, _, b := artCall(t, s, "GET", u+"?theme=dark", nil); code != 200 || !strings.Contains(string(b), "Привет") {
		t.Fatalf("theme query: %d", code)
	}
	sub := strings.TrimSuffix(u, "index.html") + "app.js"
	code, h, body = artCall(t, s, "GET", sub, nil)
	if code != 200 || string(body) != "window.x=1" || h.Get("Content-Type") != "text/javascript; charset=utf-8" {
		t.Fatalf("sub-file: %d %q %s", code, body, h.Get("Content-Type"))
	}
	if code, _, _ = artCall(t, s, "GET", strings.TrimSuffix(u, "index.html")+"missing.js", nil); code != 404 {
		t.Errorf("missing file: %d", code)
	}
	if code, _, _ = artCall(t, s, "GET", strings.TrimSuffix(u, "index.html")+"x.php", nil); code != 404 {
		t.Errorf("disallowed extension: %d", code)
	}
	// a sandboxed frame loads module scripts with Origin: null
	r := httptest.NewRequest("GET", sub, nil)
	r.Host = "127.0.0.1:8090"
	r.Header.Set("Origin", "null")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != 200 || w.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Errorf("Origin null: %d %v", w.Code, w.Header())
	}
	r = httptest.NewRequest("GET", "/api/artifacts", nil)
	r.Host = "127.0.0.1:8090"
	r.Header.Set("Origin", "null")
	w = httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != 403 {
		t.Errorf("Origin null reached the API: %d", w.Code)
	}

	// forged and tampered capabilities
	cap := strings.Split(u, "/")[2]
	payload, sig, _ := strings.Cut(cap, ".")
	other := createArtifactT(t, s, nil)
	forged := []string{
		payload + "." + sig[:len(sig)-2] + "AA",
		payload + ".",
		payload,
		"." + sig,
		strings.ToUpper(payload) + "." + sig,
		payload + "." + sig + ".x",
		"garbage",
	}
	// a valid signature of another secret
	foreign := &artifactState{secret: []byte("another-secret-0123456789abcdef"), limiter: newRateLimiter()}
	forged = append(forged,
		foreign.sign(artifactCap{Artifact: d.ID, Revision: 1, Expires: time.Now().Add(time.Minute).Unix(), Kind: "user"}))
	// swapping the artifact id inside a valid payload breaks the signature
	rawPayload, _ := base64.RawURLEncoding.DecodeString(payload)
	swapped := strings.Replace(string(rawPayload), d.ID, other.ID, 1)
	forged = append(forged, base64.RawURLEncoding.EncodeToString([]byte(swapped))+"."+sig)
	swapped = strings.Replace(string(rawPayload), `"r":1`, `"r":2`, 1)
	forged = append(forged, base64.RawURLEncoding.EncodeToString([]byte(swapped))+"."+sig)
	for _, f := range forged {
		if code, _, _ := artCall(t, s, "GET", "/artifact-frame/"+f+"/index.html", nil); code != 404 {
			t.Errorf("forged capability %.40s accepted: %d", f, code)
		}
	}
	st := s.artifacts()
	expired := st.sign(artifactCap{Artifact: d.ID, Revision: 1, Expires: time.Now().Add(-time.Second).Unix(), Kind: "user"})
	if code, _, _ := artCall(t, s, "GET", "/artifact-frame/"+expired+"/index.html", nil); code != 404 {
		t.Errorf("expired capability accepted: %d", code)
	}
	tooLong := st.sign(artifactCap{Artifact: d.ID, Revision: 1, Expires: time.Now().Add(2 * time.Hour).Unix(), Kind: "user"})
	if code, _, _ := artCall(t, s, "GET", "/artifact-frame/"+tooLong+"/index.html", nil); code != 404 {
		t.Errorf("capability longer than 15 minutes accepted: %d", code)
	}
	futureRev := st.sign(artifactCap{Artifact: d.ID, Revision: 9, Expires: time.Now().Add(time.Minute).Unix(), Kind: "user"})
	if code, _, _ := artCall(t, s, "GET", "/artifact-frame/"+futureRev+"/index.html", nil); code != 404 {
		t.Errorf("capability for a missing revision accepted: %d", code)
	}
	publicWithoutShare := st.sign(artifactCap{Artifact: d.ID, Revision: 1, Expires: time.Now().Add(time.Minute).Unix(), Kind: "public"})
	if code, _, _ := artCall(t, s, "GET", "/artifact-frame/"+publicWithoutShare+"/index.html", nil); code != 404 {
		t.Errorf("public capability without link accepted: %d", code)
	}
	// a capability is pinned to its revision
	if code, _, raw := artCall(t, s, "PUT", "/api/artifacts/"+d.ID, map[string]any{"base_revision": 1, "files": []fileSpec{{Path: "index.html", Content: "<p>second</p>"}}}); code != 200 {
		t.Fatalf("update: %d %s", code, raw)
	}
	if _, _, b := artCall(t, s, "GET", u, nil); !strings.Contains(string(b), "Привет") {
		t.Errorf("pinned revision lost: %s", b)
	}
	u2 := frameURL(t, s, d.ID, nil)
	if _, _, b := artCall(t, s, "GET", u2, nil); !strings.Contains(string(b), "second") {
		t.Errorf("current revision: %s", b)
	}
	u1 := frameURL(t, s, d.ID, map[string]int{"revision": 1})
	if _, _, b := artCall(t, s, "GET", u1, nil); !strings.Contains(string(b), "Привет") {
		t.Errorf("revision 1 view: %s", b)
	}
	if code, _, _ := artCall(t, s, "POST", "/api/artifacts/"+d.ID+"/view", map[string]int{"revision": 7}); code != 404 {
		t.Errorf("view of a missing revision: %d", code)
	}
	// delete breaks an already issued capability at once
	if code, _, _ := artCall(t, s, "DELETE", "/api/artifacts/"+d.ID, map[string]any{"base_revision": 2}); code != 200 {
		t.Fatal("delete failed")
	}
	if code, _, _ := artCall(t, s, "GET", u1, nil); code != 404 {
		t.Errorf("deleted artifact still served: %d", code)
	}
}

func TestArtifactFileEndpointIsAttachment(t *testing.T) {
	s := testServer(t)
	d := createArtifactT(t, s, map[string]any{"files": []fileSpec{{Path: "index.html", Content: "<script>alert(1)</script>"}, {Path: "d/x.svg", Content: "<svg xmlns='http://www.w3.org/2000/svg'><script>1</script></svg>"}}})
	for _, p := range []string{"index.html", "d/x.svg"} {
		code, h, body := artCall(t, s, "GET", "/api/artifacts/"+d.ID+"/files/"+p+"?revision=1", nil)
		if code != 200 || !strings.Contains(string(body), "script") {
			t.Fatalf("%s: %d", p, code)
		}
		if !strings.HasPrefix(h.Get("Content-Disposition"), "attachment") || h.Get("X-Content-Type-Options") != "nosniff" || h.Get("Content-Security-Policy") != "sandbox; default-src 'none'" {
			t.Errorf("%s headers: %v", p, h)
		}
	}
	for _, p := range []string{"missing.js", "../../etc/passwd", "x.exe"} {
		if code, _, _ := artCall(t, s, "GET", "/api/artifacts/"+d.ID+"/files/"+p, nil); code == 200 {
			t.Errorf("%s: %d", p, code)
		}
	}
	if code, _, _ := artCall(t, s, "GET", "/api/artifacts/"+d.ID+"/files/index.html?revision=3", nil); code != 404 {
		t.Errorf("missing revision: %d", code)
	}
}

func makeShare(t *testing.T, s *Server, id string, rev any) (string, string) {
	t.Helper()
	code, _, raw := artCall(t, s, "POST", "/api/artifacts/"+id+"/shares", map[string]any{"revision": rev})
	if code != 201 {
		t.Fatalf("share: %d %s", code, raw)
	}
	var v struct{ ID, URL string }
	decodeJSON(t, raw, &v)
	if !strings.HasPrefix(v.URL, "/s/") || len(v.URL) != 3+43 {
		t.Fatalf("share url: %s", v.URL)
	}
	return v.ID, v.URL
}

func publicFrame(t *testing.T, s *Server, page []byte) string {
	t.Helper()
	i := strings.Index(string(page), `src="/artifact-frame/`)
	if i < 0 {
		t.Fatalf("no iframe: %s", page)
	}
	rest := string(page)[i+5:]
	return rest[:strings.Index(rest, `"`)]
}

func TestPublicLinkPageAndRevocation(t *testing.T) {
	s := testServer(t)
	d := createArtifactT(t, s, map[string]any{"title": `<img src=x onerror=alert(1)> & "q"`})
	shareID, url := makeShare(t, s, d.ID, nil)
	code, h, page := artCall(t, s, "GET", url, nil)
	if code != 200 {
		t.Fatalf("page: %d %s", code, page)
	}
	body := string(page)
	if strings.Contains(body, "<img src=x") || !strings.Contains(body, "&lt;img src=x onerror=alert(1)&gt; &amp; &#34;q&#34;") {
		t.Errorf("title not escaped: %s", body)
	}
	if !strings.Contains(body, `sandbox="allow-scripts allow-modals"`) || !strings.Contains(body, `referrerpolicy="no-referrer"`) || strings.Contains(body, "allow-same-origin") || strings.Contains(body, "/assets/") {
		t.Errorf("iframe attributes: %s", body)
	}
	if h.Get("Cache-Control") != "no-store" || !strings.Contains(h.Get("X-Robots-Tag"), "noindex") || !strings.Contains(body, `content="noindex`) || h.Get("Set-Cookie") != "" {
		t.Errorf("page headers: %v", h)
	}
	if !strings.Contains(h.Get("Content-Security-Policy"), "frame-ancestors 'none'") || !strings.Contains(h.Get("Content-Security-Policy"), "nonce-") {
		t.Errorf("page CSP: %s", h.Get("Content-Security-Policy"))
	}
	frame := publicFrame(t, s, page)
	if code, fh, fb := artCall(t, s, "GET", frame, nil); code != 200 || fh.Get("Content-Security-Policy") != exactFrameCSP || !strings.Contains(string(fb), "Привет") {
		t.Fatalf("public frame: %d", code)
	}
	list := func() []map[string]any {
		_, _, raw := artCall(t, s, "GET", "/api/artifacts/"+d.ID+"/shares", nil)
		var out []map[string]any
		decodeJSON(t, raw, &out)
		return out
	}
	if l := list(); len(l) != 1 || l[0]["id"] != shareID {
		t.Fatalf("shares: %v", l)
	}
	// revoke: page and the already issued capability stop at once
	if code, _, _ := artCall(t, s, "DELETE", "/api/artifacts/"+d.ID+"/shares/"+shareID, nil); code != 204 {
		t.Fatalf("revoke: %d", code)
	}
	if code, _, _ := artCall(t, s, "DELETE", "/api/artifacts/"+d.ID+"/shares/"+shareID, nil); code != 204 {
		t.Fatalf("second revoke: %d", code)
	}
	if code, _, _ := artCall(t, s, "DELETE", "/api/artifacts/"+d.ID+"/shares/nope", nil); code != 404 {
		t.Fatalf("unknown share: %d", code)
	}
	if code, _, _ := artCall(t, s, "GET", url, nil); code != 404 {
		t.Errorf("revoked page: %d", code)
	}
	if code, _, _ := artCall(t, s, "GET", frame, nil); code != 404 {
		t.Errorf("revoked capability: %d", code)
	}
	if l := list(); len(l) != 0 {
		t.Errorf("revoked share listed: %v", l)
	}
	// delete of the artifact breaks links and they stay revoked after restore
	_, url2 := makeShare(t, s, d.ID, nil)
	_, _, page = artCall(t, s, "GET", url2, nil)
	frame2 := publicFrame(t, s, page)
	if code, _, _ := artCall(t, s, "DELETE", "/api/artifacts/"+d.ID, map[string]any{"base_revision": 1}); code != 200 {
		t.Fatal("delete")
	}
	if code, _, _ := artCall(t, s, "GET", url2, nil); code != 404 {
		t.Errorf("link of deleted artifact: %d", code)
	}
	if code, _, _ := artCall(t, s, "GET", frame2, nil); code != 404 {
		t.Errorf("public capability of deleted artifact: %d", code)
	}
	if code, _, _ := artCall(t, s, "POST", "/api/artifacts/"+d.ID+"/shares", map[string]any{}); code != 409 {
		t.Errorf("share of a deleted artifact: %d", code)
	}
	if code, _, _ := artCall(t, s, "POST", "/api/artifacts/"+d.ID+"/restore", map[string]any{"base_revision": 2}); code != 200 {
		t.Fatal("restore")
	}
	if code, _, _ := artCall(t, s, "GET", url2, nil); code != 404 {
		t.Errorf("link revived by restore: %d", code)
	}
}

func TestPublicLinkPinnedAndUnpinned(t *testing.T) {
	s := testServer(t)
	d := createArtifactT(t, s, nil)
	_, pinned := makeShare(t, s, d.ID, 1)
	_, latest := makeShare(t, s, d.ID, nil)
	if code, _, _ := artCall(t, s, "POST", "/api/artifacts/"+d.ID+"/shares", map[string]any{"revision": 5}); code != 400 {
		t.Errorf("share of a missing revision: %d", code)
	}
	if code, _, raw := artCall(t, s, "PUT", "/api/artifacts/"+d.ID, map[string]any{"base_revision": 1, "files": []fileSpec{{Path: "index.html", Content: "<p>новая версия</p>"}}}); code != 200 {
		t.Fatalf("update: %d %s", code, raw)
	}
	get := func(url string) string {
		_, _, page := artCall(t, s, "GET", url, nil)
		_, _, b := artCall(t, s, "GET", publicFrame(t, s, page), nil)
		return string(b)
	}
	if b := get(pinned); !strings.Contains(b, "Привет мир") {
		t.Errorf("pinned shows %s", b)
	}
	if b := get(latest); !strings.Contains(b, "новая версия") {
		t.Errorf("unpinned shows %s", b)
	}
}

func TestPublicLinkNeutralNotFoundAndRateLimit(t *testing.T) {
	s := testServer(t)
	d := createArtifactT(t, s, nil)
	revokedID, revoked := makeShare(t, s, d.ID, nil)
	artCall(t, s, "DELETE", "/api/artifacts/"+d.ID+"/shares/"+revokedID, nil)
	gone := createArtifactT(t, s, nil)
	_, deletedURL := makeShare(t, s, gone.ID, nil)
	artCall(t, s, "DELETE", "/api/artifacts/"+gone.ID, map[string]any{"base_revision": 1})
	cases := []string{revoked, deletedURL, "/s/" + strings.Repeat("A", 43), "/s/short", "/s/" + strings.Repeat("%21", 43), "/s/" + strings.Repeat("a", 500)}
	var first []byte
	var firstHeaders http.Header
	for i, c := range cases {
		code, h, b := artCall(t, s, "GET", c, nil)
		if code != 404 || !strings.Contains(string(b), "Ссылка недоступна") {
			t.Fatalf("%s: %d %s", c, code, b)
		}
		if i == 0 {
			first, firstHeaders = b, h
			continue
		}
		if !bytes.Equal(b, first) {
			t.Errorf("%s: body differs", c)
		}
		for _, k := range []string{"Cache-Control", "Content-Security-Policy", "X-Robots-Tag", "Content-Type"} {
			if h.Get(k) != firstHeaders.Get(k) {
				t.Errorf("%s: header %s differs", c, k)
			}
		}
	}
	// per-token limit
	token := "/s/" + strings.Repeat("B", 43)
	var limited bool
	for i := 0; i < publicTokenLimit+5; i++ {
		if code, _, _ := artCall(t, s, "GET", token, nil); code == 429 {
			limited = true
			break
		}
	}
	if !limited {
		t.Error("no per-token rate limit")
	}
	// per-IP limit across different tokens
	s2 := testServer(t)
	limited = false
	for i := 0; i < publicIPLimit+5; i++ {
		tok := "/s/" + strings.Repeat("C", 40) + string(rune('a'+i/100)) + string(rune('a'+i/10%10)) + string(rune('a'+i%10))
		if code, _, _ := artCall(t, s2, "GET", tok, nil); code == 429 {
			limited = true
			break
		}
	}
	if !limited {
		t.Error("no per-IP rate limit")
	}
}

func TestArtifactSearchAndProjection(t *testing.T) {
	s := testServer(t)
	a := createArtifactT(t, s, map[string]any{"title": "Диаграмма Ганта", "files": []fileSpec{
		{Path: "index.html", Content: `<html><head><title>Заголовок страницы</title><style>.секретстиль{}</style><script>var секретскрипт=1</script></head><body><!-- комментарий --><p>Видимый текст про ЖИРАФОВ</p></body></html>`},
		{Path: "data.csv", Content: "город,население\nКазань,1200000\nМосква,13000000\n"},
		{Path: "notes.md", Content: "# Заметки\nуникальныйтермин"},
		{Path: "conf.json", Content: `{"ключ":"значениеджейсон","n":1}`},
	}})
	createArtifactT(t, s, map[string]any{"title": "Другой"})
	find := func(q string) int {
		_, _, raw := artCall(t, s, "GET", "/api/artifacts?q="+url.QueryEscape(q), nil)
		var items []artifactListItem
		decodeJSON(t, raw, &items)
		n := 0
		for _, it := range items {
			if it.ID == a.ID {
				n++
			}
		}
		return n
	}
	for _, q := range []string{"ганта", "жирафов", "ЗАГОЛОВОК", "казань", "уникальныйтермин", "значениеджейсон", "описание", "жирафов страницы", "100%"[:0] + "ган"} {
		if find(q) != 1 {
			t.Errorf("%q not found", q)
		}
	}
	for _, q := range []string{"секретстиль", "секретскрипт", "комментарий", "жирафов лошадей", "50%", "_"} {
		if find(q) != 0 {
			t.Errorf("%q wrongly found", q)
		}
	}
	// the projection follows updates
	code, _, raw := artCall(t, s, "PUT", "/api/artifacts/"+a.ID, map[string]any{"base_revision": 1, "files": []fileSpec{{Path: "notes.md", Delete: true}, {Path: "data.csv", Content: "а,б\nновыйгород,1"}}})
	if code != 200 {
		t.Fatalf("%d %s", code, raw)
	}
	if find("уникальныйтермин") != 0 || find("казань") != 0 || find("новыйгород") != 1 || find("жирафов") != 1 {
		t.Error("projection not updated")
	}
	if p := artifactProjection("x.html", []byte(strings.Repeat("слово ", 20000))); len(p) > artifactFileProjection {
		t.Errorf("projection %d bytes", len(p))
	}
	if txt := artifactSearchText("t", "d", []artifactFile{{Path: "a.txt"}}, artifactProjectionSet{"a.txt": strings.Repeat("я", 200000)}); len(txt) > artifactMaxProjection {
		t.Errorf("search text %d bytes", len(txt))
	}
}

func TestArtifactRuntimeFiles(t *testing.T) {
	web := t.TempDir()
	s, err := Open(t.TempDir(), web)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if code, _, _ := artCall(t, s, "GET", "/artifact-runtime/aiwiki-kit.js", nil); code != 404 {
		t.Fatalf("without directory: %d", code)
	}
	dir := filepath.Join(web, "artifact-runtime")
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "aiwiki-kit.js"), []byte("window.kit=1"), 0o644)
	os.WriteFile(filepath.Join(dir, "aiwiki-kit.css"), []byte("a{}"), 0o644)
	os.WriteFile(filepath.Join(dir, "secret.env"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(web, "outside.js"), []byte("outside"), 0o644)
	code, h, b := artCall(t, s, "GET", "/artifact-runtime/aiwiki-kit.js", nil)
	if code != 200 || string(b) != "window.kit=1" || h.Get("Content-Type") != "text/javascript; charset=utf-8" || h.Get("X-Content-Type-Options") != "nosniff" || h.Get("Cross-Origin-Resource-Policy") != "cross-origin" || !strings.Contains(h.Get("Cache-Control"), "no-cache") {
		t.Fatalf("kit: %d %v", code, h)
	}
	if code, h, _ = artCall(t, s, "GET", "/artifact-runtime/aiwiki-kit.css", nil); code != 200 || h.Get("Content-Type") != "text/css; charset=utf-8" {
		t.Fatalf("css: %d %v", code, h)
	}
	for _, p := range []string{"secret.env", "sub", "sub/", "../outside.js", "%2e%2e/outside.js", "..%2foutside.js", ".hidden.js", "missing.js", "a%5cb.js"} {
		if code, _, b := artCall(t, s, "GET", "/artifact-runtime/"+p, nil); code == 200 && string(b) == "outside" || code == 200 {
			t.Errorf("%s served: %d", p, code)
		}
	}
	// the application itself is unaffected
	os.WriteFile(filepath.Join(web, "index.html"), []byte("<html>app</html>"), 0o644)
	if code, _, b := artCall(t, s, "GET", "/", nil); code != 200 || !strings.Contains(string(b), "app") {
		t.Errorf("app index: %d", code)
	}
}

func TestArtifactSeparateOrigin(t *testing.T) {
	t.Setenv("AIWIKI_ARTIFACT_ORIGIN", "https://frames.example.org")
	s := testServer(t)
	if err := s.SetAllowedOrigin("https://wiki.example.org"); err != nil {
		t.Fatal(err)
	}
	d := createArtifactT(t, s, nil)
	_, _, raw := artCall(t, s, "POST", "/api/artifacts/"+d.ID+"/view", "")
	var v struct {
		FrameURL string `json:"frame_url"`
	}
	decodeJSON(t, raw, &v)
	if !strings.HasPrefix(v.FrameURL, "https://frames.example.org/artifact-frame/") {
		t.Fatalf("frame url %s", v.FrameURL)
	}
	path := strings.TrimPrefix(v.FrameURL, "https://frames.example.org")
	do := func(host, target string) (int, http.Header) {
		r := httptest.NewRequest("GET", target, nil)
		r.Host = host
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		return w.Code, w.Result().Header
	}
	code, h := do("frames.example.org", path)
	if code != 200 || !strings.HasSuffix(h.Get("Content-Security-Policy"), "webrtc 'block'; frame-ancestors https://wiki.example.org") {
		t.Errorf("frame host: %d %s", code, h.Get("Content-Security-Policy"))
	}
	// frames and the runtime exist on the frame host only
	for _, p := range []string{path, strings.TrimSuffix(path, "index.html") + "app.js", "/artifact-runtime/resize.js"} {
		if code, _ := do("wiki.example.org", p); code != 404 {
			t.Errorf("%s on the application host: %d", p, code)
		}
		if code, _ := do("frames.example.org", p); code != 200 {
			t.Errorf("%s on the frame host: %d", p, code)
		}
	}
	for _, target := range []string{path, "/artifact-runtime/resize.js"} {
		r := httptest.NewRequest("GET", target, nil)
		r.Host = "frames.example.org"
		r.Header.Set("Origin", "null")
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		if w.Code != 200 || w.Header().Get("Access-Control-Allow-Origin") != "*" {
			t.Errorf("Origin null on the frame host %s: %d %v", target, w.Code, w.Header())
		}
	}

	if code, _ := do("frames.example.org", "/api/artifacts"); code != 404 {
		t.Errorf("API on the frame host: %d", code)
	}
	if code, _ := do("frames.example.org", "/s/"+strings.Repeat("a", 43)); code != 404 {
		t.Errorf("public page on the frame host: %d", code)
	}
}

func TestArtifactMCPTrusted(t *testing.T) {
	s := testServer(t)
	call := func(tool string, args map[string]any) (bool, string) { return rpc(t, s, tool, args) }
	isErr, text := call("wiki_artifact_write", map[string]any{"space_id": "general", "title": "Из MCP", "files": []map[string]any{{"path": "index.html", "content": "<p>" + strings.Repeat("абв", 3000) + "</p>"}, {"path": "logo.png", "content": "iVBORw0K", "encoding": "base64"}}})
	if isErr {
		t.Fatalf("create: %s", text)
	}
	var created struct {
		ID       string
		Revision int
	}
	decodeJSON(t, []byte(strings.NewReplacer(`"id"`, `"ID"`, `"revision"`, `"Revision"`).Replace(text)), &created)
	if created.ID == "" || created.Revision != 1 {
		t.Fatalf("created %s", text)
	}
	if isErr, text = call("wiki_artifact_write", map[string]any{"space_id": "general", "title": "x", "visibility": "private", "files": []map[string]any{{"path": "index.html", "content": "x"}}}); !isErr || !strings.Contains(text, "visibility") {
		t.Errorf("visibility accepted over MCP: %v %s", isErr, text)
	}
	if isErr, text = call("wiki_artifact_write", map[string]any{"id": created.ID, "base_revision": 1, "files": []map[string]any{{"path": "app.js", "content": "1"}}, "message": "m"}); isErr {
		t.Fatalf("update: %s", text)
	}
	if isErr, text = call("wiki_artifact_write", map[string]any{"id": created.ID, "base_revision": 1, "title": "stale"}); !isErr || !strings.Contains(text, "revision conflict") {
		t.Errorf("stale write: %v %s", isErr, text)
	}
	_, text = call("wiki_artifact_list", map[string]any{"q": "из mcp"})
	if !strings.Contains(text, created.ID) {
		t.Errorf("list: %s", text)
	}
	_, text = call("wiki_artifact_read", map[string]any{"id": created.ID})
	if !strings.Contains(text, `"app.js"`) || !strings.Contains(text, `"current_revision":2`) {
		t.Errorf("manifest: %s", text)
	}
	_, text = call("wiki_artifact_read", map[string]any{"id": created.ID, "path": "index.html", "revision": 2, "max_chars": 100})
	var chunk struct {
		Text     string         `json:"text"`
		HasMore  bool           `json:"has_more"`
		Total    int            `json:"total_chars"`
		NextRead map[string]any `json:"next_read"`
	}
	decodeJSON(t, []byte(text), &chunk)
	if len([]rune(chunk.Text)) != 100 || !chunk.HasMore || chunk.Total != 9007 || chunk.NextRead["offset"].(float64) != 100 {
		t.Fatalf("chunk: %s", text)
	}
	// continuation stays pinned to revision 2 even after a newer revision
	call("wiki_artifact_write", map[string]any{"id": created.ID, "base_revision": 2, "files": []map[string]any{{"path": "index.html", "content": "<p>short</p>"}}})
	_, text = call("wiki_artifact_read", map[string]any{"id": created.ID, "path": "index.html", "revision": 2, "offset": 100, "max_chars": 16000})
	decodeJSON(t, []byte(text), &chunk)
	if chunk.Total != 9007 || chunk.HasMore || len([]rune(chunk.Text)) != 9007-100 {
		t.Fatalf("pinned continuation: %.200s", text)
	}
	if isErr, text = call("wiki_artifact_read", map[string]any{"id": created.ID, "path": "index.html", "offset": 5}); !isErr || !strings.Contains(text, "revision") {
		t.Errorf("offset without revision: %v %s", isErr, text)
	}
	if isErr, text = call("wiki_artifact_read", map[string]any{"id": created.ID, "path": "logo.png"}); !isErr || !strings.Contains(text, "binary") {
		t.Errorf("binary read: %v %s", isErr, text)
	}
	if isErr, _ = call("wiki_artifact_read", map[string]any{"id": "nope"}); !isErr {
		t.Error("missing artifact")
	}
	// no tool for delete or sharing exists
	for _, tl := range mcpToolList() {
		if n := tl["name"].(string); strings.Contains(n, "artifact") && (strings.Contains(n, "delete") || strings.Contains(n, "share")) {
			t.Errorf("forbidden tool %s", n)
		}
	}
}

func TestArtifactCapabilityUnit(t *testing.T) {
	st := &artifactState{secret: []byte("0123456789abcdef0123"), limiter: newRateLimiter()}
	now := time.Unix(2_000_000_000, 0)
	c := artifactCap{Artifact: "abc", Revision: 3, Expires: now.Add(10 * time.Minute).Unix(), Kind: "user"}
	token := st.sign(c)
	if got, ok := st.verify(token, now); !ok || got != c {
		t.Fatalf("round trip: %v %+v", ok, got)
	}
	if _, ok := st.verify(token, now.Add(11*time.Minute)); ok {
		t.Error("expired accepted")
	}
	p, m, _ := strings.Cut(token, ".")
	for _, bad := range []string{p + "." + m[:len(m)-1], p[:len(p)-1] + "." + m, m + "." + p} {
		if _, ok := st.verify(bad, now); ok {
			t.Errorf("tampered accepted: %.30s", bad)
		}
	}
	if _, ok := st.verify(st.sign(artifactCap{Artifact: "abc", Revision: 1, Expires: now.Add(time.Hour).Unix(), Kind: "user"}), now); ok {
		t.Error("lifetime over 15 minutes accepted")
	}
	if _, ok := st.verify(st.sign(artifactCap{Artifact: "abc", Revision: 1, Expires: now.Add(time.Minute).Unix(), Kind: "root"}), now); ok {
		t.Error("unknown kind accepted")
	}
	if _, ok := st.verify(st.sign(artifactCap{Artifact: "a/../b", Revision: 1, Expires: now.Add(time.Minute).Unix(), Kind: "user"}), now); ok {
		t.Error("bad artifact id accepted")
	}
}

func TestArtifactFrameResizeInjection(t *testing.T) {
	s := testServer(t)
	script := `<script src="/artifact-runtime/resize.js"></script>`
	cases := map[string]struct{ in, want string }{
		"with body":    {"<html><body><p>x</p></body></html>", "<html><body><p>x</p>" + script + "</body></html>"},
		"upper case":   {"<HTML><BODY>x</BODY></HTML>", "<HTML><BODY>x" + script + "</BODY></HTML>"},
		"two closings": {"<body>a</body><!-- </body> -->", "<body>a</body><!-- " + script + "</body> -->"},
		"no body":      {"<h1>Привет İİİ</h1>", "<h1>Привет İİİ</h1>" + script},
	}
	for name, c := range cases {
		d := createArtifactT(t, s, map[string]any{"files": []fileSpec{{Path: "index.html", Content: c.in}, {Path: "other.html", Content: c.in}, {Path: "app.js", Content: "</body>"}}})
		u := frameURL(t, s, d.ID, nil)
		if _, _, b := artCall(t, s, "GET", u, nil); string(b) != c.want {
			t.Errorf("%s: got %q want %q", name, b, c.want)
		}
		base := strings.TrimSuffix(u, "index.html")
		if _, _, b := artCall(t, s, "GET", base+"other.html", nil); string(b) != c.in {
			t.Errorf("%s: other.html changed: %q", name, b)
		}
		if _, _, b := artCall(t, s, "GET", base+"app.js", nil); string(b) != "</body>" {
			t.Errorf("%s: app.js changed", name)
		}
		// stored bytes and the manifest hash are those of the original file
		_, _, raw := artCall(t, s, "GET", "/api/artifacts/"+d.ID+"/files/index.html", nil)
		if string(raw) != c.in {
			t.Errorf("%s: stored index.html changed", name)
		}
		for _, f := range d.Files {
			if f.Path == "index.html" && f.Size != int64(len(c.in)) {
				t.Errorf("%s: manifest size", name)
			}
		}
	}
	// Content-Length matches the injected body
	d := createArtifactT(t, s, nil)
	_, h, b := artCall(t, s, "GET", frameURL(t, s, d.ID, nil), nil)
	if h.Get("Content-Length") != strconv.Itoa(len(b)) || !strings.Contains(string(b), script) {
		t.Errorf("content length %s vs %d", h.Get("Content-Length"), len(b))
	}
}

func TestArtifactResizeScriptServed(t *testing.T) {
	s := testServer(t) // no web directory: the script is embedded
	code, h, b := artCall(t, s, "GET", "/artifact-runtime/resize.js", nil)
	if code != 200 || !bytes.Contains(b, []byte("aiwiki:resize")) || !bytes.Contains(b, []byte("ResizeObserver")) || !bytes.Contains(b, []byte("window.AIWiki")) || !bytes.Contains(b, []byte("requestAnimationFrame")) {
		t.Fatalf("resize.js: %d", code)
	}
	for k, v := range map[string]string{"Content-Type": "text/javascript; charset=utf-8", "X-Content-Type-Options": "nosniff", "Cross-Origin-Resource-Policy": "cross-origin"} {
		if h.Get(k) != v {
			t.Errorf("%s = %q", k, h.Get(k))
		}
	}
	if code, _, _ = artCall(t, s, "GET", "/artifact-runtime/missing.js", nil); code != 404 {
		t.Errorf("missing runtime file: %d", code)
	}
}

func (m *memStore) List(_ context.Context, prefix string) ([]objectInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []objectInfo
	for k, v := range m.m {
		if strings.HasPrefix(k, prefix) {
			out = append(out, objectInfo{Key: k, Size: int64(len(v)), Modified: time.Now()})
		}
	}
	return out, nil
}
