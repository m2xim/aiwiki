package server

import (
	"encoding/json"
	"hash/fnv"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeEmbedder maps words of a few concepts, in Russian and English, to shared
// dimensions, and every other word to a hashed dimension with a small weight, so
// a paraphrase in another language lands near the original.
type fakeEmbedder struct {
	srv   *httptest.Server
	calls atomic.Int64
	dims  atomic.Int64 // 0 means 64
}

var fakeConcepts = map[string]int{
	"parcel": 0, "посылка": 0, "упаковка": 0, "box": 0,
	"torn": 1, "damaged": 1, "повреждена": 1, "порвана": 1,
	"rollout": 2, "выкладка": 2, "deploy": 2,
}

func fakeVector(text string) []float64 {
	v := make([]float64, 64)
	for _, w := range strings.FieldsFunc(strings.ToLower(text), func(r rune) bool { return !(r >= 'a' && r <= 'z' || r >= 'а' && r <= 'я' || r == 'ё') }) {
		if d, ok := fakeConcepts[w]; ok {
			v[d] += 1
			continue
		}
		h := fnv.New32a()
		h.Write([]byte(w))
		v[8+int(h.Sum32()%56)] += 0.15
	}
	v[63] += 0.01
	return v
}

func newFakeEmbedder(t *testing.T) *fakeEmbedder {
	t.Helper()
	f := &fakeEmbedder{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.calls.Add(1)
		var in struct {
			Model string   `json:"model"`
			Input []string `json:"input"`
		}
		if json.NewDecoder(r.Body).Decode(&in) != nil || in.Model != "fake" {
			http.Error(w, "bad", 400)
			return
		}
		type item struct {
			Index     int       `json:"index"`
			Embedding []float64 `json:"embedding"`
		}
		out := struct {
			Data []item `json:"data"`
		}{}
		for i := len(in.Input) - 1; i >= 0; i-- { // out of order on purpose
			v := fakeVector(in.Input[i])
			if n := f.dims.Load(); n > 0 {
				v = v[:n]
			}
			out.Data = append(out.Data, item{Index: i, Embedding: v})
		}
		_ = json.NewEncoder(w).Encode(out)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

type semanticResponse struct {
	sectionSearchResponse
	Semantic string   `json:"semantic"`
	Coverage *float64 `json:"semantic_coverage"`
}

func semanticSearch(t *testing.T, s *Server, args map[string]any) semanticResponse {
	t.Helper()
	isErr, msg, _ := callSectionSearch(t, s, args)
	if isErr {
		t.Fatalf("search error: %s", msg)
	}
	var r semanticResponse
	if err := json.Unmarshal([]byte(msg), &r); err != nil {
		t.Fatal(err)
	}
	return r
}

// searchIndexed repeats a search until every visible section has a vector.
func searchIndexed(t *testing.T, s *Server, args map[string]any) semanticResponse {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		r := semanticSearch(t, s, args)
		if r.Semantic == "on" {
			return r
		}
		if r.Semantic != "indexing" || time.Now().After(deadline) {
			t.Fatalf("semantic status %q", r.Semantic)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestSemanticConfigValidation(t *testing.T) {
	for _, c := range []struct {
		cfg SemanticConfig
		ok  bool
	}{
		{SemanticConfig{}, true},
		{SemanticConfig{URL: "http://127.0.0.1:1234/v1/embeddings", Model: "m"}, true},
		{SemanticConfig{URL: "http://[::1]:1234/v1/embeddings", Model: "m"}, true},
		{SemanticConfig{URL: "http://127.0.0.1:1234/v1/embeddings"}, false},
		{SemanticConfig{URL: "http://localhost:1234/v1/embeddings", Model: "m"}, false},
		{SemanticConfig{URL: "https://api.example.com/v1/embeddings", Model: "m"}, false},
		{SemanticConfig{URL: "https://api.example.com/v1/embeddings", Model: "m", AllowRemote: true}, true},
		{SemanticConfig{URL: "http://user:pw@127.0.0.1/v1/embeddings", Model: "m"}, false},
		{SemanticConfig{URL: "ftp://127.0.0.1/x", Model: "m"}, false},
	} {
		if err := c.cfg.validate(); (err == nil) != c.ok {
			t.Errorf("%+v: err=%v", c.cfg, err)
		}
	}
}

func TestSplitChunksKeepsRunesAndParagraphs(t *testing.T) {
	text := strings.Repeat("ж", 2500) + "\n\nхвост"
	chunks := splitChunks(text, 1200)
	joined := strings.Join(chunks, "")
	if len(chunks) != 3 || !strings.HasSuffix(chunks[2], "хвост") || strings.Count(joined, "ж") != 2500 {
		t.Fatalf("chunks=%d last=%q", len(chunks), chunks[len(chunks)-1])
	}
	for _, c := range chunks {
		if !strings.Contains(c, "ж") && !strings.Contains(c, "хвост") {
			t.Fatalf("empty chunk %q", c)
		}
	}
}

func TestSemanticSearchTrusted(t *testing.T) {
	s := testServer(t)
	f := newFakeEmbedder(t)
	if err := s.EnableSemantic(SemanticConfig{URL: f.srv.URL, Model: "fake"}); err != nil {
		t.Fatal(err)
	}
	returns, rev := writeArticle(t, s, map[string]any{"space_id": "general", "title": "Возвраты", "body": "## Проверка\n\nЕсли упаковка повреждена, назначьте ручную проверку."})
	deploy, _ := writeArticle(t, s, map[string]any{"space_id": "general", "title": "Release", "body": "## Steps\n\nStart the rollout in small batches."})

	// Cold start: nothing is embedded yet, the partial lexical fallback stays.
	r := semanticSearch(t, s, map[string]any{"q": "rollout zebra"})
	if r.Semantic != "indexing" || len(r.Results) == 0 || r.Results[0].ID != deploy || r.Mode != "any" {
		t.Fatalf("cold start: %+v", r)
	}

	// Lexically nothing matches; semantically the Russian section does.
	r = searchIndexed(t, s, map[string]any{"q": "torn parcel"})
	if len(r.Results) == 0 || r.Results[0].ID != returns || r.Results[0].Heading != "Проверка" {
		t.Fatalf("semantic hit: %+v", r.Results)
	}
	top := r.Results[0]
	if strings.Join(top.MatchedBy, ",") != "semantic" || top.Similarity == nil || *top.Similarity <= 0 || top.ReadArgs["id"] != returns {
		t.Fatalf("semantic-only hit fields: %+v", top)
	}
	if r.Coverage == nil || *r.Coverage != 1 {
		t.Fatalf("coverage %v", r.Coverage)
	}

	// A full lexical match is fused and marked as both.
	r = searchIndexed(t, s, map[string]any{"q": "rollout batches"})
	if len(r.Results) == 0 || r.Results[0].ID != deploy || strings.Join(r.Results[0].MatchedBy, ",") != "lexical,semantic" {
		t.Fatalf("fused hit: %+v", r.Results)
	}

	// semantic=false is the old lexical search.
	r = semanticSearch(t, s, map[string]any{"q": "torn parcel", "semantic": false})
	if r.Semantic != "off" || len(r.Results) != 0 {
		t.Fatalf("lexical only: %+v", r)
	}

	// After an edit the stored vectors are exactly those of the current text.
	var stale []string
	rows, _ := s.DB.Query(`SELECT text_key FROM article_embeddings WHERE article_id=?`, returns)
	for rows.Next() {
		var k string
		rows.Scan(&k)
		stale = append(stale, k)
	}
	rows.Close()
	if len(stale) != 1 {
		t.Fatalf("stored keys before edit: %v", stale)
	}
	mustOK(t, s, "wiki_edit", map[string]any{"id": returns, "base_revision": rev, "op": "replace_text", "old": "ручную проверку", "text": "фото и ручную проверку"}, nil)
	r = searchIndexed(t, s, map[string]any{"q": "damaged box"})
	if len(r.Results) == 0 || r.Results[0].ID != returns {
		t.Fatalf("after edit: %+v", r.Results)
	}
	var keys []string
	rows, _ = s.DB.Query(`SELECT text_key FROM article_embeddings WHERE article_id=?`, returns)
	for rows.Next() {
		var k string
		rows.Scan(&k)
		keys = append(keys, k)
	}
	rows.Close()
	if len(keys) != 1 || keys[0] == stale[0] {
		t.Fatalf("stored keys after edit: %v (before %v)", keys, stale)
	}

	// A restart reuses stored vectors instead of embedding again.
	before := f.calls.Load()
	s.semantic.close()
	s.semantic = nil
	if err := s.EnableSemantic(SemanticConfig{URL: f.srv.URL, Model: "fake"}); err != nil {
		t.Fatal(err)
	}
	r = semanticSearch(t, s, map[string]any{"q": "torn parcel"})
	if r.Semantic != "on" || f.calls.Load() != before+1 {
		t.Fatalf("restart: status=%s embedding calls=%d (only the query expected)", r.Semantic, f.calls.Load()-before)
	}

	// The endpoint changes its output dimension: stored vectors are embedded
	// again instead of silently matching nothing.
	f.dims.Store(32)
	r = searchIndexed(t, s, map[string]any{"q": "torn parcel again"})
	if len(r.Results) == 0 || r.Results[0].ID != returns {
		t.Fatalf("after dimension change: %+v", r.Results)
	}
	var dims int
	if err := s.DB.QueryRow(`SELECT min(dims) FROM article_embeddings`).Scan(&dims); err != nil || dims != 32 {
		t.Fatalf("stored dims=%d err=%v", dims, err)
	}

	// With the embedding service down the search falls back to lexical results.
	f.srv.Close()
	r = semanticSearch(t, s, map[string]any{"q": "rollout steps unseen"})
	if r.Semantic != "unavailable" || len(r.Results) == 0 || r.Results[0].ID != deploy || r.Mode != "any" {
		t.Fatalf("fallback: %+v", r)
	}
}

func TestSemanticEndpointRedirectIsNotFollowed(t *testing.T) {
	target := newFakeEmbedder(t)
	redirect := httptest.NewServer(http.RedirectHandler(target.srv.URL, http.StatusTemporaryRedirect))
	t.Cleanup(redirect.Close)
	s := testServer(t)
	if err := s.EnableSemantic(SemanticConfig{URL: redirect.URL, Model: "fake"}); err != nil {
		t.Fatal(err)
	}
	writeArticle(t, s, map[string]any{"space_id": "general", "title": "Release", "body": "## Steps\n\nrollout"})
	r := semanticSearch(t, s, map[string]any{"q": "rollout"})
	if r.Semantic != "unavailable" || target.calls.Load() != 0 {
		t.Fatalf("redirect followed: status=%s calls=%d", r.Semantic, target.calls.Load())
	}
}

func TestSemanticSearchPostgresVisibility(t *testing.T) {
	owner, root := corporateFixture(t)
	f := newFakeEmbedder(t)
	if err := root.EnableSemantic(SemanticConfig{URL: f.srv.URL, Model: "fake"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(root.semantic.close)
	if err := root.DB.validateSemanticSchema(); err != nil {
		t.Fatal(err)
	}
	alice := corporateAs(t, root, "alice")
	bob := corporateAs(t, root, "bob")
	space := corporateCreateSpace(t, alice)
	rr := corporateHTTP(alice, "POST", "/api/articles", `{"space_id":"`+space+`","title":"Private returns","body":"## Проверка\n\nЕсли упаковка повреждена, назначьте ручную проверку."}`)
	if rr.Code != 201 {
		t.Fatalf("create %d %s", rr.Code, rr.Body)
	}
	var a Article
	json.Unmarshal(rr.Body.Bytes(), &a)

	r := searchIndexed(t, alice, map[string]any{"q": "torn parcel"})
	if len(r.Results) == 0 || r.Results[0].ID != a.ID {
		t.Fatalf("alice: %+v", r.Results)
	}
	// Bob's own query never returns the article, although its vectors are in
	// the shared process index; his own similar article is found.
	bobSpace := corporateCreateSpace(t, bob)
	rr = corporateHTTP(bob, "POST", "/api/articles", `{"space_id":"`+bobSpace+`","title":"Bob returns","body":"## Box\n\nA damaged box goes to manual review."}`)
	if rr.Code != 201 {
		t.Fatalf("bob create %d %s", rr.Code, rr.Body)
	}
	var mine Article
	json.Unmarshal(rr.Body.Bytes(), &mine)
	r = searchIndexed(t, bob, map[string]any{"q": "torn parcel", "limit": 20})
	if len(r.Results) == 0 || r.Results[0].ID != mine.ID {
		t.Fatalf("bob semantic results: %+v", r.Results)
	}
	for _, h := range r.Results {
		if h.ID == a.ID {
			t.Fatal("semantic search leaked a private article")
		}
	}
	var n int
	if err := alice.DB.QueryRow(`SELECT count(*) FROM article_embeddings WHERE article_id=?`, a.ID).Scan(&n); err != nil || n != 1 {
		t.Fatalf("alice rows=%d err=%v", n, err)
	}
	if err := bob.DB.QueryRow(`SELECT count(*) FROM article_embeddings WHERE article_id=?`, a.ID).Scan(&n); err != nil || n != 0 {
		t.Fatalf("bob sees %d vector rows err=%v", n, err)
	}
	if _, err := bob.DB.Exec(`INSERT INTO article_embeddings(article_id,model,text_key,dims,vector,created_at) VALUES(?,?,?,?,?,?)`, a.ID, "fake", strings.Repeat("a", 64), 1, []byte{0, 0, 128, 63}, "x"); err == nil {
		t.Fatal("bob inserted a vector for a hidden article")
	}
	if _, err := alice.DB.Exec(`UPDATE article_embeddings SET created_at='y' WHERE article_id=?`, a.ID); err == nil {
		var changed int
		alice.DB.QueryRow(`SELECT count(*) FROM article_embeddings WHERE created_at='y'`).Scan(&changed)
		if changed != 0 {
			t.Fatal("vector rows are updatable")
		}
	}
	// The startup check notices a widened policy.
	if _, err := owner.DB.Exec(`ALTER POLICY article_embeddings_read ON article_embeddings USING (true)`); err != nil {
		t.Fatal(err)
	}
	if err := root.DB.validateSemanticSchema(); err == nil {
		t.Fatal("widened policy passed the startup check")
	}
}
