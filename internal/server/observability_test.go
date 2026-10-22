package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
)

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) lines() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Split(strings.TrimSpace(l.b.String()), "\n")
}

func exposition(m *metrics) string {
	var b bytes.Buffer
	m.write(&b, metricsSource{mode: "trusted"})
	return b.String()
}

// A corporate-style stack: an outer mux with the catch-all "/" delegates to an
// inner mux. The inner pattern must win and no id may reach the label.
func TestRouteLabelNestedMuxInnermostWins(t *testing.T) {
	s := &Server{}
	buf := &lockedBuffer{}
	s.SetAccessLog(buf)
	m := newMetrics()

	inner := http.NewServeMux()
	inner.HandleFunc("GET /api/spaces/{id}/audit", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("audit " + r.PathValue("id")))
	})
	inner.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) })
	innerTracked := trackMux(inner)
	outer := http.NewServeMux()
	outer.HandleFunc("GET /api/health", func(w http.ResponseWriter, r *http.Request) {})
	outer.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// like corporateHandler: the request is handed on (and may be copied)
		innerTracked.ServeHTTP(w, r.WithContext(r.Context()))
	}))
	h := s.observeWith(m, trackMux(outer))

	do := func(method, target string) *httptest.ResponseRecorder {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(method, target, nil))
		return rr
	}
	if rr := do("GET", "/api/spaces/secret-space-4711/audit?token=hunter2"); rr.Code != 200 {
		t.Fatalf("status %d", rr.Code)
	}
	do("GET", "/api/health")
	do("GET", "/index.html")       // inner "/" -> static
	do("GET", "/api/nothing/here") // inner "/" below /api/ -> other
	do("BREW", "/api/health")      // method outside the known set

	out := exposition(m)
	for _, want := range []string{
		`aiwiki_http_requests_total{route="GET /api/spaces/{id}/audit",method="GET",code="2xx"} 1`,
		`aiwiki_http_requests_total{route="GET /api/health",method="GET",code="2xx"} 1`,
		`aiwiki_http_requests_total{route="static",method="GET",code="4xx"} 1`,
		`aiwiki_http_requests_total{route="other",method="GET",code="4xx"} 1`,
		`aiwiki_http_request_duration_seconds_count{route="GET /api/spaces/{id}/audit",method="GET"} 1`,
		`method="OTHER"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("exposition lacks %q\n%s", want, out)
		}
	}
	for _, leak := range []string{"secret-space-4711", "hunter2", "BREW", "/index.html", "nothing"} {
		if strings.Contains(out, leak) {
			t.Errorf("exposition leaks %q", leak)
		}
	}
	for _, line := range buf.lines() {
		for _, leak := range []string{"secret-space-4711", "hunter2", "token=", "/index.html"} {
			if strings.Contains(line, leak) {
				t.Errorf("access log leaks %q: %s", leak, line)
			}
		}
	}
	first := map[string]any{}
	if err := json.Unmarshal([]byte(buf.lines()[0]), &first); err != nil {
		t.Fatal(err)
	}
	if first["route"] != "GET /api/spaces/{id}/audit" || first["status"] != float64(200) || first["method"] != "GET" {
		t.Fatalf("unexpected access log line %v", first)
	}
	for _, k := range []string{"time", "request_id", "duration_ms", "bytes"} {
		if _, ok := first[k]; !ok {
			t.Errorf("access log line lacks %s", k)
		}
	}
	if first["bytes"] != float64(len("audit secret-space-4711")) {
		t.Errorf("bytes %v", first["bytes"])
	}
	for _, banned := range []string{"query", "path", "client_ip", "remote", "user", "headers"} {
		if _, ok := first[banned]; ok {
			t.Errorf("access log has forbidden field %s", banned)
		}
	}
}

func TestRequestIDHeaderAndUpstream(t *testing.T) {
	s := &Server{}
	buf := &lockedBuffer{}
	s.SetAccessLog(buf)
	h := s.observeWith(newMetrics(), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	hex32 := regexp.MustCompile(`^[0-9a-f]{32}$`)
	seen := map[string]bool{}
	for _, up := range []string{"abc.DEF_123-x", "bad id with spaces", strings.Repeat("a", 65), ""} {
		req := httptest.NewRequest("GET", "/x", nil)
		if up != "" {
			req.Header.Set("X-Request-Id", up)
		}
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		id := rr.Header().Get("X-Request-Id")
		if !hex32.MatchString(id) || id == up || seen[id] {
			t.Fatalf("bad own request id %q for upstream %q", id, up)
		}
		seen[id] = true
	}
	lines := buf.lines()
	var entries []map[string]any
	for _, l := range lines {
		e := map[string]any{}
		if err := json.Unmarshal([]byte(l), &e); err != nil {
			t.Fatal(err)
		}
		entries = append(entries, e)
	}
	if entries[0]["upstream_request_id"] != "abc.DEF_123-x" {
		t.Fatalf("valid upstream id not logged: %v", entries[0])
	}
	for _, e := range entries[1:] {
		if _, ok := e["upstream_request_id"]; ok {
			t.Fatalf("invalid upstream id logged: %v", e)
		}
	}
}

func TestPanicCountsAs500AndInFlightReturnsToZero(t *testing.T) {
	m := newMetrics()
	h := (&Server{}).observeWith(m, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { panic("boom") }))
	func() {
		defer func() { _ = recover() }()
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/x", nil))
	}()
	out := exposition(m)
	if !strings.Contains(out, `code="5xx"} 1`) || !strings.Contains(out, "aiwiki_http_in_flight_requests 0") {
		t.Fatal(out)
	}
}

// The real handler in trusted mode: readiness, static and MCP.
func TestTrustedHandlerReadyAndMCPMetrics(t *testing.T) {
	s := testServer(t)
	buf := &lockedBuffer{}
	s.SetAccessLog(buf)
	h := s.Handler()
	get := func(path string) *httptest.ResponseRecorder {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest("GET", "http://127.0.0.1"+path, nil))
		return rr
	}
	rr := get("/api/ready")
	if rr.Code != 200 || strings.TrimSpace(rr.Body.String()) != `{"ok":true}` {
		t.Fatalf("ready: %d %q", rr.Code, rr.Body.String())
	}
	call := func(name string) {
		body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"` + name + `","arguments":{"query":"x"}}}`
		req := httptest.NewRequest("POST", "http://127.0.0.1/mcp", strings.NewReader(body))
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if rr.Code != 200 {
			t.Fatalf("mcp %d", rr.Code)
		}
	}
	call("wiki_search")
	call(`evil\"name{x}`)
	get("/api/nope")
	out := exposition(defaultMetrics)
	for _, want := range []string{
		`aiwiki_mcp_tool_calls_total{tool="wiki_search",outcome="ok"}`,
		`aiwiki_mcp_tool_calls_total{tool="unknown",outcome="error"}`,
		`aiwiki_mcp_tool_duration_seconds_count{tool="wiki_search"}`,
		`aiwiki_http_requests_total{route="POST /mcp"`,
		`aiwiki_http_requests_total{route="/mcp"`,
		`aiwiki_http_requests_total{route="GET /api/ready",method="GET",code="2xx"}`,
	} {
		if want == `aiwiki_http_requests_total{route="POST /mcp"` {
			continue // /mcp is registered without a method
		}
		if !strings.Contains(out, want) {
			t.Errorf("exposition lacks %q\n%s", want, out)
		}
	}
	if strings.Contains(out, "evil") {
		t.Error("tool name leaked into labels")
	}
	last := buf.lines()
	if !strings.Contains(last[len(last)-1], `"route":"other"`) {
		t.Errorf("404 under /api should be route other: %s", last[len(last)-1])
	}
}

func TestRateLimitAndLoginCounters(t *testing.T) {
	m := newMetrics()
	m.rateLimited("mcp_oauth")
	m.rateLimited("artifact_frame")
	m.rateLimited("not-a-limiter")
	m.login("ok")
	m.login("denied")
	m.login("whatever")
	out := exposition(m)
	for _, want := range []string{
		`aiwiki_rate_limited_total{limiter="mcp_oauth"} 1`,
		`aiwiki_rate_limited_total{limiter="artifact_frame"} 1`,
		`aiwiki_rate_limited_total{limiter="artifact_token"} 0`,
		`aiwiki_auth_logins_total{outcome="ok"} 1`,
		`aiwiki_auth_logins_total{outcome="denied"} 1`,
		`aiwiki_auth_logins_total{outcome="failed"} 0`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q", want)
		}
	}
	if strings.Contains(out, "not-a-limiter") || strings.Contains(out, "whatever") {
		t.Error("unbounded label value accepted")
	}
}

func TestHistogramBucketsAreCumulative(t *testing.T) {
	m := newMetrics()
	m.observeTool("wiki_read", false, 3_000_000)     // 3 ms
	m.observeTool("wiki_read", false, 30_000_000)    // 30 ms
	m.observeTool("wiki_read", true, 20_000_000_000) // 20 s
	out := exposition(m)
	for _, want := range []string{
		`aiwiki_mcp_tool_duration_seconds_bucket{tool="wiki_read",le="0.005"} 1`,
		`aiwiki_mcp_tool_duration_seconds_bucket{tool="wiki_read",le="0.05"} 2`,
		`aiwiki_mcp_tool_duration_seconds_bucket{tool="wiki_read",le="10"} 2`,
		`aiwiki_mcp_tool_duration_seconds_bucket{tool="wiki_read",le="+Inf"} 3`,
		`aiwiki_mcp_tool_duration_seconds_count{tool="wiki_read"} 3`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q\n%s", want, out)
		}
	}
}

func TestSeriesCardinalityIsBounded(t *testing.T) {
	m := newMetrics()
	for i := 0; i < maxSeries*3; i++ {
		m.observeRequest("route-"+strings.Repeat("x", i%5000), "GET", 200, 0)
	}
	if n := len(m.requests.snapshot()); n > maxSeries+1 {
		t.Fatalf("%d series", n)
	}
}

func TestValidateMetricsConfig(t *testing.T) {
	long := strings.Repeat("t", 32)
	cases := []struct {
		addr, token string
		ok          bool
	}{
		{"", "", true},
		{"127.0.0.1:9464", "", true},
		{"[::1]:9464", "", true},
		{"localhost:9464", "", false},
		{"localhost:9464", long, true},
		{"0.0.0.0:9464", "", false},
		{":9464", "", false},
		{"192.0.2.10:9464", "", false},
		{"192.0.2.10:9464", strings.Repeat("t", 31), false},
		{"192.0.2.10:9464", long, true},
		{"metrics.example.org:9464", long, true},
		{"nonsense", "", false},
	}
	for _, c := range cases {
		if err := ValidateMetricsConfig(c.addr, c.token); (err == nil) != c.ok {
			t.Errorf("addr %q token len %d: err=%v want ok=%v", c.addr, len(c.token), err, c.ok)
		}
	}
}

func TestMetricsHandlerTokenAndScope(t *testing.T) {
	s := testServer(t)
	_ = s.Handler()
	token := strings.Repeat("s3cr3t-", 6)
	h := s.MetricsHandler(token)
	do := func(method, path, auth string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, nil)
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		return rr
	}
	for _, a := range []string{"", "Bearer wrong", "Bearer " + token + "x", "Basic " + token, token} {
		if rr := do("GET", "/metrics", a); rr.Code != 401 {
			t.Errorf("auth %q: %d", a, rr.Code)
		}
	}
	rr := do("GET", "/metrics", "Bearer "+token)
	if rr.Code != 200 || !strings.HasPrefix(rr.Header().Get("Content-Type"), "text/plain; version=0.0.4") {
		t.Fatalf("%d %s", rr.Code, rr.Header().Get("Content-Type"))
	}
	body := rr.Body.String()
	for _, want := range []string{"aiwiki_build_info{", `mode="trusted"`, "aiwiki_ready 1", `aiwiki_db_open_connections{pool="app"}`, "go_goroutines", "process_start_time_seconds", "go_memstats_heap_alloc_bytes"} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q", want)
		}
	}
	if rr := do("GET", "/api/ready", "Bearer "+token); rr.Code != 200 {
		t.Errorf("ready on metrics listener: %d", rr.Code)
	}
	if rr := do("GET", "/api/spaces", "Bearer "+token); rr.Code != 404 {
		t.Errorf("metrics listener serves more than metrics and ready: %d", rr.Code)
	}
	if rr := do("POST", "/metrics", "Bearer "+token); rr.Code != 405 {
		t.Errorf("POST /metrics: %d", rr.Code)
	}
	open := s.MetricsHandler("")
	rr = httptest.NewRecorder()
	open.ServeHTTP(rr, httptest.NewRequest("GET", "/metrics", nil))
	if rr.Code != 200 {
		t.Errorf("loopback without token: %d", rr.Code)
	}
}

func TestParseAccessLogMode(t *testing.T) {
	for v, want := range map[string]bool{"": false, "off": false, "json": true} {
		if got, err := ParseAccessLogMode(v); err != nil || got != want {
			t.Errorf("%q: %v %v", v, got, err)
		}
	}
	if _, err := ParseAccessLogMode("text"); err == nil {
		t.Error("text accepted")
	}
}

type readerFromRecorder struct {
	*httptest.ResponseRecorder
	readFromCalls int
}

func (r *readerFromRecorder) ReadFrom(src io.Reader) (int64, error) {
	r.readFromCalls++
	return io.Copy(r.ResponseRecorder, src)
}

func TestStatusWriterReadFromDelegatesAndCounts(t *testing.T) {
	m := newMetrics()
	buf := &lockedBuffer{}
	s := &Server{}
	s.SetAccessLog(buf)
	rec := &readerFromRecorder{ResponseRecorder: httptest.NewRecorder()}
	h := s.observeWith(m, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rf, ok := w.(io.ReaderFrom)
		if !ok {
			t.Fatal("wrapper hides io.ReaderFrom")
		}
		if _, err := rf.ReadFrom(strings.NewReader("hello world")); err != nil {
			t.Fatal(err)
		}
	}))
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/x", nil))
	if rec.readFromCalls != 1 || rec.Body.String() != "hello world" {
		t.Fatalf("not delegated: calls=%d body=%q", rec.readFromCalls, rec.Body.String())
	}
	if l := buf.lines()[0]; !strings.Contains(l, `"bytes":11`) || !strings.Contains(l, `"status":200`) {
		t.Fatal(l)
	}
	// Underlying writer without ReaderFrom falls back to io.Copy.
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("GET", "/x", nil))
	if rr.Body.String() != "hello world" {
		t.Fatalf("fallback body %q", rr.Body.String())
	}
}

func TestPanickingHandlerKeepsRealRoute(t *testing.T) {
	m := newMetrics()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/spaces/{id}/audit", func(w http.ResponseWriter, r *http.Request) { panic("boom") })
	h := (&Server{}).observeWith(m, trackMux(mux))
	func() {
		defer func() { _ = recover() }()
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/api/spaces/abc/audit", nil))
	}()
	if out := exposition(m); !strings.Contains(out, `route="GET /api/spaces/{id}/audit",method="GET",code="5xx"} 1`) {
		t.Fatal(out)
	}
}

func TestAccessLogEnabledAfterHandlerBuilt(t *testing.T) {
	s := &Server{}
	h := s.observeWith(newMetrics(), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	buf := &lockedBuffer{}
	s.SetAccessLog(buf)
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/x", nil))
	if !strings.Contains(buf.lines()[0], `"request_id"`) {
		t.Fatal("late SetAccessLog ignored")
	}
}
