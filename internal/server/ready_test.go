package server

import (
	"bytes"
	"context"
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *fakeClock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	var mu sync.Mutex
	log.SetOutput(writerFunc(func(p []byte) (int, error) { mu.Lock(); defer mu.Unlock(); return buf.Write(p) }))
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	return &buf
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

func TestReadinessCacheSingleFlightAndLogThrottle(t *testing.T) {
	logs := captureLog(t)
	var calls atomic.Int32
	var failing atomic.Bool
	release := make(chan struct{})
	rd := newReadiness(map[string]func(context.Context) error{
		"database": func(ctx context.Context) error {
			calls.Add(1)
			<-release
			if failing.Load() {
				return errors.New("dial tcp 192.0.2.7:5432: connection refused")
			}
			return nil
		},
	})
	clk := &fakeClock{t: time.Unix(1000, 0)}
	rd.now = clk.now

	// 20 concurrent callers share one check.
	var wg sync.WaitGroup
	results := make([]bool, 20)
	for i := range results {
		wg.Add(1)
		go func() { defer wg.Done(); results[i] = rd.ok(context.Background()) }()
	}
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()
	for _, r := range results {
		if !r {
			t.Fatal("expected ready")
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("single-flight broken: %d checks", calls.Load())
	}
	// Cached for 5 s even if the dependency broke meanwhile.
	failing.Store(true)
	clk.add(4 * time.Second)
	if !rd.ok(context.Background()) || calls.Load() != 1 {
		t.Fatal("result not cached for 5 s")
	}
	clk.add(2 * time.Second)
	if rd.ok(context.Background()) || calls.Load() != 2 {
		t.Fatalf("expired cache must re-check (calls=%d)", calls.Load())
	}
	// More failures within 30 s are logged once.
	for i := 0; i < 4; i++ {
		clk.add(6 * time.Second)
		if rd.ok(context.Background()) {
			t.Fatal("should be failing")
		}
	}
	if n := strings.Count(logs.String(), "readiness: database check failed"); n != 1 {
		t.Fatalf("logged %d times within 30 s:\n%s", n, logs.String())
	}
	clk.add(31 * time.Second)
	rd.ok(context.Background())
	if n := strings.Count(logs.String(), "readiness: database check failed"); n != 2 {
		t.Fatalf("expected a second log line after 30 s, got %d", n)
	}
}

func TestReadinessTimeoutAndCallerCancel(t *testing.T) {
	rd := newReadiness(map[string]func(context.Context) error{
		"storage": func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() },
	})
	captureLog(t)
	start := time.Now()
	if rd.ok(context.Background()) {
		t.Fatal("hung dependency reported ready")
	}
	if d := time.Since(start); d < 1900*time.Millisecond || d > 4*time.Second {
		t.Fatalf("check took %v, want about 2 s", d)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rd2 := newReadiness(map[string]func(context.Context) error{
		"storage": func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() },
	})
	if rd2.ok(ctx) {
		t.Fatal("cancelled caller got true")
	}
}

func TestReadyHandlerBodyHasNoDetails(t *testing.T) {
	captureLog(t)
	s := &Server{}
	s.ready = newReadiness(map[string]func(context.Context) error{
		"database": func(context.Context) error {
			return errors.New("password authentication failed for user aiwiki_app at db.internal.example.org")
		},
	})
	rr := httptest.NewRecorder()
	s.readyHandler(rr, httptest.NewRequest("GET", "/api/ready", nil))
	if rr.Code != http.StatusServiceUnavailable || strings.TrimSpace(rr.Body.String()) != `{"ok":false}` {
		t.Fatalf("%d %q", rr.Code, rr.Body.String())
	}
	for _, h := range rr.Header() {
		for _, v := range h {
			if strings.Contains(v, "example.org") || strings.Contains(v, "password") {
				t.Fatalf("header leaks detail: %q", v)
			}
		}
	}
}

func TestReadyTrustedDatabaseDown(t *testing.T) {
	captureLog(t)
	s := testServer(t)
	h := s.Handler()
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("GET", "http://127.0.0.1/api/ready", nil))
	if rr.Code != 200 {
		t.Fatalf("%d", rr.Code)
	}
	s.DB.pool.Close()
	s.ready = s.newReadiness() // drop the cache
	rr = httptest.NewRecorder()
	s.readyHandler(rr, httptest.NewRequest("GET", "/api/ready", nil))
	if rr.Code != 503 || strings.TrimSpace(rr.Body.String()) != `{"ok":false}` {
		t.Fatalf("%d %q", rr.Code, rr.Body.String())
	}
}

// Corporate mode: reachable without login, covers PostgreSQL and the bucket.
func TestCorporateReadyWithoutLogin(t *testing.T) {
	captureLog(t)
	s3Env(t)
	_, root := corporateFixture(t)
	fake, endpoint := newFakeS3(t)
	_ = fake
	store, err := newS3Store(context.Background(), S3Config{Bucket: "wiki", Region: "us-east-1", Endpoint: endpoint.URL, PathStyle: true, AllowInsecureLoopback: true})
	if err != nil {
		t.Fatal(err)
	}
	auth := newAuthHarness(t, nil)
	s := &Server{DB: root.DB, DataDir: root.DataDir, objects: store, auth: auth.a}
	h := s.Handler()
	get := func(path string) *httptest.ResponseRecorder {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest("GET", "http://127.0.0.1"+path, nil))
		return rr
	}
	if rr := get("/api/ready"); rr.Code != 200 || strings.TrimSpace(rr.Body.String()) != `{"ok":true}` {
		t.Fatalf("ready: %d %q", rr.Code, rr.Body.String())
	}
	if rr := get("/api/spaces"); rr.Code != 401 {
		t.Fatalf("protected route must still need login: %d", rr.Code)
	}
	// An unreachable bucket makes the instance not ready.
	endpoint.Close()
	s.ready = s.newReadiness()
	rr := get("/api/ready")
	if rr.Code != 503 || strings.TrimSpace(rr.Body.String()) != `{"ok":false}` {
		t.Fatalf("storage down: %d %q", rr.Code, rr.Body.String())
	}
}

func TestReadinessStuckCheckDoesNotWedgeFlight(t *testing.T) {
	captureLog(t)
	block := make(chan struct{})
	defer close(block)
	rd := newReadiness(map[string]func(context.Context) error{
		"database": func(context.Context) error { <-block; return nil }, // ignores its context
	})
	start := time.Now()
	if rd.ok(context.Background()) {
		t.Fatal("stuck check reported ready")
	}
	if d := time.Since(start); d > 4*time.Second {
		t.Fatalf("took %v", d)
	}
	rd.mu.Lock()
	stuck := rd.flight != nil
	rd.mu.Unlock()
	if stuck {
		t.Fatal("flight left open")
	}
}
