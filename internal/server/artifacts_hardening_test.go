package server

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestArtifactSecretAndProxyConfiguration(t *testing.T) {
	t.Setenv("AIWIKI_ARTIFACT_SECRET", "too-short-secret")
	if _, err := Open(t.TempDir(), ""); err == nil || !strings.Contains(err.Error(), "at least 32") {
		t.Fatalf("a short secret must stop the start: %v", err)
	}
	t.Setenv("AIWIKI_ARTIFACT_SECRET", strings.Repeat("s", 32))
	s, err := Open(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	t.Setenv("AIWIKI_ARTIFACT_SECRET", "")
	a, b := randomArtifactState(), randomArtifactState()
	if bytes.Equal(a.secret, b.secret) || len(a.secret) != 32 {
		t.Error("fallback secrets must be random")
	}
	src, err := os.ReadFile("artifacts_render.go")
	if err != nil || strings.Contains(string(src), "fallback-artifact-secret") {
		t.Error("a constant fallback secret is present")
	}
	t.Setenv("AIWIKI_TRUSTED_PROXIES", "10.0.0.0/8, not-an-address")
	if _, err := Open(t.TempDir(), ""); err == nil {
		t.Error("invalid AIWIKI_TRUSTED_PROXIES accepted")
	}
}

func xffRequest(remote string, xff ...string) *http.Request {
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = remote
	for _, v := range xff {
		r.Header.Add("X-Forwarded-For", v)
	}
	return r
}

func TestArtifactClientIPBehindTrustedProxy(t *testing.T) {
	proxies, err := parseTrustedProxies("10.0.0.0/8 192.0.2.7")
	if err != nil {
		t.Fatal(err)
	}
	st := &artifactState{proxies: proxies, limiter: newRateLimiter()}
	cases := []struct{ got, want string }{
		{st.clientIP(xffRequest("203.0.113.9:1", "1.2.3.4")), "203.0.113.9"},                // untrusted peer: header ignored
		{st.clientIP(xffRequest("10.1.1.1:1", "198.51.100.5")), "198.51.100.5"},             // trusted peer
		{st.clientIP(xffRequest("10.1.1.1:1", "6.6.6.6, 198.51.100.5")), "198.51.100.5"},    // forged left part ignored
		{st.clientIP(xffRequest("10.1.1.1:1", "198.51.100.5, 10.2.2.2")), "198.51.100.5"},   // inner trusted hops skipped
		{st.clientIP(xffRequest("10.1.1.1:1", "198.51.100.5", "10.3.3.3")), "198.51.100.5"}, // several header lines
		{st.clientIP(xffRequest("192.0.2.7:1", "2001:db8::1")), "2001:db8::1"},              // single-IP entry, IPv6 client
		{st.clientIP(xffRequest("10.1.1.1:1")), "10.1.1.1"},                                 // no header
		{st.clientIP(xffRequest("10.1.1.1:1", "garbage")), "10.1.1.1"},                      // malformed: the peer
		{st.clientIP(xffRequest("10.1.1.1:1", "10.9.9.9, 10.8.8.8")), "10.1.1.1"},           // only trusted hops
		{(&artifactState{}).clientIP(xffRequest("10.1.1.1:1", "5.5.5.5")), "10.1.1.1"},      // not configured
	}
	for i, c := range cases {
		if c.got != c.want {
			t.Errorf("case %d: got %s want %s", i, c.got, c.want)
		}
	}
}

func TestArtifactRateLimitsPerClientAndFrame(t *testing.T) {
	s := testServer(t)
	s.art.proxies, _ = parseTrustedProxies("192.0.2.0/24")
	d := createArtifactT(t, s, nil)
	u := frameURL(t, s, d.ID, nil)
	get := func(target, remote, xff string) int {
		r := httptest.NewRequest("GET", target, nil)
		r.Host = "127.0.0.1:8090"
		r.RemoteAddr = remote
		if xff != "" {
			r.Header.Set("X-Forwarded-For", xff)
		}
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		return w.Code
	}
	// /s/: one client over its limit does not block another behind the same proxy
	for i := 0; i < publicIPLimit; i++ {
		get("/s/"+strings.Repeat("A", 40)+"x"+string(rune('a'+i/26%26))+string(rune('a'+i%26)), "192.0.2.1:1", "198.51.100.1")
	}
	if code := get("/s/"+strings.Repeat("A", 43), "192.0.2.1:1", "198.51.100.1"); code != 429 {
		t.Errorf("client over the /s/ limit: %d", code)
	}
	if code := get("/s/"+strings.Repeat("A", 43), "192.0.2.1:1", "198.51.100.2"); code != 404 {
		t.Errorf("another client behind the proxy: %d", code)
	}
	// frames have their own, larger budget, per client address
	for i := 0; i < frameIPLimit; i++ {
		if code := get(u, "203.0.113.5:1", ""); code != 200 {
			t.Fatalf("frame request %d: %d", i, code)
		}
	}
	if code := get(u, "203.0.113.5:1", ""); code != 429 {
		t.Errorf("frame over the limit: %d", code)
	}
	if code := get(u, "203.0.113.6:1", ""); code != 200 {
		t.Errorf("another address: %d", code)
	}
	if code := get("/s/"+strings.Repeat("B", 43), "203.0.113.5:1", ""); code != 404 {
		t.Errorf("the /s/ budget is separate from frames: %d", code)
	}
	if publicTokenLimit != 600 || publicIPLimit != 300 || frameIPLimit != 1200 {
		t.Error("limits differ from the specification")
	}
	// the table is bounded
	l := newRateLimiter()
	for i := 0; i < 60000; i++ {
		l.allow("k"+strconv.Itoa(i), 5, time.Hour)
	}
	if len(l.m) > 20001 {
		t.Errorf("limiter table grew to %d", len(l.m))
	}
}

func TestArtifactSearchTermLimits(t *testing.T) {
	words := make([]string, 30)
	for i := range words {
		words[i] = "w" + strconv.Itoa(i)
	}
	got := searchTerms(strings.Join(words, " ") + " " + strings.Repeat("я", 500))
	if len(got) != 10 || got[9] != "w9" {
		t.Errorf("terms: %v", got)
	}
	got = searchTerms(strings.Repeat("Я", 500))
	if len(got) != 1 || got[0] != strings.Repeat("я", 100) {
		t.Errorf("long term: %d", len([]rune(got[0])))
	}
	// only the first ten terms decide: a mismatch beyond the tenth is ignored
	s := testServer(t)
	createArtifactT(t, s, map[string]any{"title": "один два три четыре пять шесть семь восемь девять десять"})
	_, _, raw := artCall(t, s, "GET", "/api/artifacts?q="+url.QueryEscape("один два три четыре пять шесть семь восемь девять десять нетакогослова"), nil)
	if !strings.Contains(string(raw), "один два") {
		t.Errorf("terms beyond the tenth must be dropped: %s", raw)
	}
}

func TestArtifactOrderingOfChecksTrusted(t *testing.T) {
	s := testServer(t)
	d := createArtifactT(t, s, nil)
	huge := `{"base_revision":1,"files":[{"path":"../bad","content":"` + strings.Repeat("a", 25<<20) + `"}]}`
	if code, _, _ := artCall(t, s, "PUT", "/api/artifacts/missing", huge); code != 404 {
		t.Errorf("missing artifact: %d", code)
	}
	if code, _, _ := artCall(t, s, "PUT", "/api/artifacts/"+d.ID, huge); code != 400 {
		t.Errorf("an editor gets the validation error: %d", code)
	}
	body := `{"space_id":"nope","title":"t","files":[{"path":"../bad","content":"` + strings.Repeat("a", 25<<20) + `"}]}`
	if code, _, raw := artCall(t, s, "POST", "/api/artifacts", body); code != 404 || !strings.Contains(string(raw), "space") {
		t.Errorf("unknown space: %d %.100s", code, raw)
	}
}
