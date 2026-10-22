package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Semantic section search (docs/SEMANTIC-SEARCH.md). Sections are split into
// chunks, each chunk is embedded once by an OpenAI-compatible endpoint and the
// vectors are kept per article, keyed by a hash of the model and the exact text.
// A search scores only the sections of articles that the request's own,
// row-level-secured query returned, so the index never widens access.

const (
	semanticChunkRunes   = 1200
	semanticQueryTimeout = 2 * time.Second
	semanticBatchTimeout = 60 * time.Second
	semanticBatchSize    = 32
	semanticFuseDepth    = 20
	semanticRRFK         = 60
	semanticQueueSize    = 4096
	semanticQueryCache   = 256
	semanticMaxDims      = 8192
)

// SemanticConfig selects the embedding endpoint. An empty URL disables
// semantic search.
type SemanticConfig struct {
	URL           string
	Model         string
	QueryPrefix   string
	DocPrefix     string
	MinSimilarity float64
	AllowRemote   bool
}

// SemanticConfigFromEnv reads the AIWIKI_EMBEDDINGS_* variables.
func SemanticConfigFromEnv() (SemanticConfig, error) {
	c := SemanticConfig{
		URL:         strings.TrimSpace(os.Getenv("AIWIKI_EMBEDDINGS_URL")),
		Model:       strings.TrimSpace(os.Getenv("AIWIKI_EMBEDDINGS_MODEL")),
		QueryPrefix: unescapeNewlines(os.Getenv("AIWIKI_EMBEDDINGS_QUERY_PREFIX")),
		DocPrefix:   unescapeNewlines(os.Getenv("AIWIKI_EMBEDDINGS_DOC_PREFIX")),
		AllowRemote: os.Getenv("AIWIKI_EMBEDDINGS_ALLOW_REMOTE") == "1",
	}
	if v := strings.TrimSpace(os.Getenv("AIWIKI_EMBEDDINGS_MIN_SIMILARITY")); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil || f < -1 || f > 1 {
			return c, errors.New("AIWIKI_EMBEDDINGS_MIN_SIMILARITY must be a number from -1 to 1")
		}
		c.MinSimilarity = f
	}
	return c, c.validate()
}

// unescapeNewlines lets an environment file carry a multi-line instruction
// prefix as a literal \n.
func unescapeNewlines(v string) string { return strings.ReplaceAll(v, `\n`, "\n") }

func (c SemanticConfig) validate() error {
	if c.URL == "" {
		return nil
	}
	if c.Model == "" {
		return errors.New("AIWIKI_EMBEDDINGS_MODEL is required with AIWIKI_EMBEDDINGS_URL")
	}
	u, err := url.Parse(c.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
		return errors.New("AIWIKI_EMBEDDINGS_URL must be an http(s) URL without credentials")
	}
	if !c.AllowRemote {
		ip := net.ParseIP(strings.Trim(u.Hostname(), "[]"))
		if ip == nil || !ip.IsLoopback() {
			// Article text is sent to this endpoint: closed content must not reach
			// an external provider by accident.
			return errors.New("AIWIKI_EMBEDDINGS_URL must be a literal loopback address unless AIWIKI_EMBEDDINGS_ALLOW_REMOTE=1")
		}
	}
	return nil
}

type semanticJob struct {
	articleID string
	userID    string
	chunks    []semanticChunk
}

type semanticChunk struct {
	key  string
	text string
}

type semanticIndex struct {
	cfg    SemanticConfig
	root   *database
	client *http.Client

	mu      sync.RWMutex
	vectors map[string]map[string][]float32 // article id -> chunk key -> unit vector
	loaded  map[string]bool                 // article ids whose stored vectors were read
	pending map[string]bool                 // article ids queued for embedding
	queries map[string][]float32
	qorder  []string

	queue chan semanticJob
	stop  chan struct{}
	done  chan struct{}
}

// EnableSemantic turns on semantic section search for this server. It must be
// called before Handler. A zero config leaves search lexical.
func (s *Server) EnableSemantic(c SemanticConfig) error {
	if err := c.validate(); err != nil {
		return err
	}
	if c.URL == "" {
		return nil
	}
	idx := &semanticIndex{
		cfg: c, root: s.DB, client: &http.Client{Timeout: semanticBatchTimeout},
		vectors: map[string]map[string][]float32{}, loaded: map[string]bool{}, pending: map[string]bool{},
		queries: map[string][]float32{}, queue: make(chan semanticJob, semanticQueueSize),
		stop: make(chan struct{}), done: make(chan struct{}),
	}
	go idx.worker()
	s.semantic = idx
	return nil
}

func (idx *semanticIndex) close() {
	if idx == nil {
		return
	}
	close(idx.stop)
	<-idx.done
}

// chunkKey identifies one embedded text for one model.
func (idx *semanticIndex) chunkKey(text string) string {
	sum := sha256.Sum256([]byte(idx.cfg.Model + "\x00" + idx.cfg.DocPrefix + text))
	return hex.EncodeToString(sum[:])
}

// sectionChunks returns the texts embedded for one section: the article title
// and heading path as context, then the section text in bounded pieces split on
// paragraph boundaries.
func (idx *semanticIndex) sectionChunks(a Article, sec searchableSection) []semanticChunk {
	head := a.Title
	if sec.path != "" {
		head += " > " + sec.path
	}
	var out []semanticChunk
	for _, piece := range splitChunks(sec.text, semanticChunkRunes) {
		text := head + "\n" + piece
		out = append(out, semanticChunk{key: idx.chunkKey(text), text: text})
	}
	return out
}

func splitChunks(text string, limit int) []string {
	var out []string
	cur := ""
	flush := func() {
		if strings.TrimSpace(cur) != "" {
			out = append(out, cur)
		}
		cur = ""
	}
	for _, p := range strings.Split(text, "\n\n") {
		if cur != "" && utf8.RuneCountInString(cur)+2+utf8.RuneCountInString(p) > limit {
			flush()
		}
		if cur == "" {
			cur = p
		} else {
			cur += "\n\n" + p
		}
		for utf8.RuneCountInString(cur) > limit {
			rs := []rune(cur)
			out = append(out, string(rs[:limit]))
			cur = string(rs[limit:])
		}
	}
	flush()
	return out
}

// ensureLoaded reads stored vectors of the given articles through db, which is
// the request's own connection, so row-level security applies to the read.
func (idx *semanticIndex) ensureLoaded(ctx context.Context, db *database, ids []string) {
	idx.mu.RLock()
	var missing []string
	for _, id := range ids {
		if !idx.loaded[id] {
			missing = append(missing, id)
		}
	}
	idx.mu.RUnlock()
	for len(missing) > 0 {
		n := len(missing)
		if n > 500 {
			n = 500
		}
		batch := missing[:n]
		missing = missing[n:]
		marks := strings.TrimSuffix(strings.Repeat("?,", len(batch)), ",")
		args := []any{idx.cfg.Model}
		for _, id := range batch {
			args = append(args, id)
		}
		rows, err := db.QueryContext(ctx, `SELECT article_id,text_key,vector FROM article_embeddings WHERE model=? AND article_id IN (`+marks+`)`, args...)
		if err != nil {
			log.Printf("semantic search: reading stored vectors failed: %v", err)
			return
		}
		got := map[string]map[string][]float32{}
		for rows.Next() {
			var id, key string
			var raw []byte
			if rows.Scan(&id, &key, &raw) != nil {
				continue
			}
			if v := decodeVector(raw); v != nil {
				if got[id] == nil {
					got[id] = map[string][]float32{}
				}
				got[id][key] = v
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			log.Printf("semantic search: reading stored vectors failed: %v", err)
			return
		}
		idx.mu.Lock()
		for _, id := range batch {
			if idx.loaded[id] {
				continue
			}
			idx.loaded[id] = true
			if idx.vectors[id] == nil {
				idx.vectors[id] = map[string][]float32{}
			}
			for k, v := range got[id] {
				idx.vectors[id][k] = v
			}
		}
		idx.mu.Unlock()
	}
}

// lookup returns the vectors of the given chunk keys of one article and whether
// all of them are present.
func (idx *semanticIndex) lookup(articleID string, chunks []semanticChunk) ([][]float32, bool) {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	m := idx.vectors[articleID]
	out := make([][]float32, 0, len(chunks))
	complete := true
	for _, c := range chunks {
		if v, ok := m[c.key]; ok {
			out = append(out, v)
		} else {
			complete = false
		}
	}
	return out, complete
}

// enqueue schedules embedding of an article's current chunks. It never blocks a
// search: when the queue is full the article is retried by a later search.
func (idx *semanticIndex) enqueue(articleID, userID string, chunks []semanticChunk) {
	idx.mu.Lock()
	if idx.pending[articleID] {
		idx.mu.Unlock()
		return
	}
	idx.pending[articleID] = true
	idx.mu.Unlock()
	select {
	case idx.queue <- semanticJob{articleID: articleID, userID: userID, chunks: chunks}:
	default:
		idx.mu.Lock()
		delete(idx.pending, articleID)
		idx.mu.Unlock()
	}
}

func (idx *semanticIndex) worker() {
	defer close(idx.done)
	for {
		select {
		case <-idx.stop:
			return
		case job := <-idx.queue:
			if err := idx.index(job); err != nil {
				log.Printf("semantic search: indexing an article failed: %v", err)
			}
			idx.mu.Lock()
			delete(idx.pending, job.articleID)
			idx.mu.Unlock()
		}
	}
}

// index embeds the missing chunks of one article, then replaces the article's
// stored vectors by exactly its current chunks: vectors of removed text do not
// outlive the text. Storage goes through a connection scoped to the user whose
// search saw the article, so row-level security checks the write.
func (idx *semanticIndex) index(job semanticJob) error {
	idx.mu.RLock()
	known := idx.vectors[job.articleID]
	var todo []semanticChunk
	current := map[string][]float32{}
	for _, c := range job.chunks {
		if v, ok := known[c.key]; ok {
			current[c.key] = v
		} else if _, dup := current[c.key]; !dup {
			todo = append(todo, c)
			current[c.key] = nil
		}
	}
	idx.mu.RUnlock()
	for i := 0; i < len(todo); i += semanticBatchSize {
		end := i + semanticBatchSize
		if end > len(todo) {
			end = len(todo)
		}
		texts := make([]string, 0, end-i)
		for _, c := range todo[i:end] {
			texts = append(texts, idx.cfg.DocPrefix+c.text)
		}
		ctx, cancel := context.WithTimeout(context.Background(), semanticBatchTimeout)
		vecs, err := idx.embed(ctx, texts)
		cancel()
		if err != nil {
			return err
		}
		for j, c := range todo[i:end] {
			current[c.key] = vecs[j]
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), semanticBatchTimeout)
	defer cancel()
	db, release, err := idx.root.scoped(ctx, job.userID)
	if err != nil {
		return err
	}
	defer release()
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rows, err := tx.Query(`SELECT text_key FROM article_embeddings WHERE article_id=? AND model=?`, job.articleID, idx.cfg.Model)
	if err != nil {
		return err
	}
	stored := map[string]bool{}
	for rows.Next() {
		var k string
		if rows.Scan(&k) == nil {
			stored[k] = true
		}
	}
	rows.Close()
	for k := range stored {
		if _, keep := current[k]; !keep {
			if _, err := tx.Exec(`DELETE FROM article_embeddings WHERE article_id=? AND model=? AND text_key=?`, job.articleID, idx.cfg.Model, k); err != nil {
				return err
			}
		}
	}
	now := time.Now().UTC().Format(time.RFC3339)
	for k, v := range current {
		if stored[k] || v == nil {
			continue
		}
		if _, err := tx.Exec(`INSERT INTO article_embeddings(article_id,model,text_key,dims,vector,created_at) VALUES(?,?,?,?,?,?)`, job.articleID, idx.cfg.Model, k, len(v), encodeVector(v), now); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	idx.mu.Lock()
	idx.vectors[job.articleID] = current
	idx.loaded[job.articleID] = true
	idx.mu.Unlock()
	return nil
}

// queryVector embeds a search query, with a small cache of recent queries.
func (idx *semanticIndex) queryVector(ctx context.Context, q string) ([]float32, error) {
	text := idx.cfg.QueryPrefix + q
	idx.mu.RLock()
	v, ok := idx.queries[text]
	idx.mu.RUnlock()
	if ok {
		return v, nil
	}
	ctx, cancel := context.WithTimeout(ctx, semanticQueryTimeout)
	defer cancel()
	vecs, err := idx.embed(ctx, []string{text})
	if err != nil {
		return nil, err
	}
	idx.mu.Lock()
	if _, ok := idx.queries[text]; !ok {
		idx.queries[text] = vecs[0]
		idx.qorder = append(idx.qorder, text)
		if len(idx.qorder) > semanticQueryCache {
			delete(idx.queries, idx.qorder[0])
			idx.qorder = idx.qorder[1:]
		}
	}
	idx.mu.Unlock()
	return vecs[0], nil
}

// embed calls the OpenAI-compatible /v1/embeddings endpoint and returns unit
// vectors in input order.
func (idx *semanticIndex) embed(ctx context.Context, texts []string) ([][]float32, error) {
	body, _ := json.Marshal(map[string]any{"model": idx.cfg.Model, "input": texts})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, idx.cfg.URL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if key := os.Getenv("AIWIKI_EMBEDDINGS_API_KEY"); key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	res, err := idx.client.Do(req)
	if err != nil {
		return nil, errors.New("embedding endpoint unavailable")
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, 64<<20))
	if err != nil {
		return nil, errors.New("embedding endpoint read failed")
	}
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("embedding endpoint returned HTTP %d", res.StatusCode)
	}
	var out struct {
		Data []struct {
			Index     int       `json:"index"`
			Embedding []float64 `json:"embedding"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || len(out.Data) != len(texts) {
		return nil, errors.New("embedding endpoint returned an invalid response")
	}
	vecs := make([][]float32, len(texts))
	dims := -1
	for _, d := range out.Data {
		if d.Index < 0 || d.Index >= len(texts) || vecs[d.Index] != nil {
			return nil, errors.New("embedding endpoint returned an invalid response")
		}
		if dims == -1 {
			dims = len(d.Embedding)
		}
		if len(d.Embedding) != dims || dims == 0 || dims > semanticMaxDims {
			return nil, errors.New("embedding endpoint returned an invalid vector")
		}
		v := normalize(d.Embedding)
		if v == nil {
			return nil, errors.New("embedding endpoint returned an invalid vector")
		}
		vecs[d.Index] = v
	}
	return vecs, nil
}

func normalize(in []float64) []float32 {
	var n float64
	for _, x := range in {
		if math.IsNaN(x) || math.IsInf(x, 0) {
			return nil
		}
		n += x * x
	}
	n = math.Sqrt(n)
	if n == 0 {
		return nil
	}
	out := make([]float32, len(in))
	for i, x := range in {
		out[i] = float32(x / n)
	}
	return out
}

func dot(a, b []float32) float64 {
	if len(a) != len(b) {
		return -1
	}
	var s float32
	for i := range a {
		s += a[i] * b[i]
	}
	return float64(s)
}

func encodeVector(v []float32) []byte {
	out := make([]byte, 4*len(v))
	for i, x := range v {
		binary.LittleEndian.PutUint32(out[4*i:], math.Float32bits(x))
	}
	return out
}

func decodeVector(raw []byte) []float32 {
	if len(raw) == 0 || len(raw)%4 != 0 || len(raw)/4 > semanticMaxDims {
		return nil
	}
	out := make([]float32, len(raw)/4)
	for i := range out {
		out[i] = math.Float32frombits(binary.LittleEndian.Uint32(raw[4*i:]))
	}
	return out
}
