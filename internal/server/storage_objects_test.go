package server

import (
	"bytes"
	"context"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

type fakeS3 struct {
	mu      sync.Mutex
	objects map[string][]byte
	types   map[string]string
	auths   []string
	heads   int
	failPut bool
}

func newFakeS3(t *testing.T) (*fakeS3, *httptest.Server) {
	t.Helper()
	f := &fakeS3{objects: map[string][]byte{}, types: map[string]string{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.auths = append(f.auths, r.Header.Get("Authorization"))
		path := strings.TrimPrefix(r.URL.Path, "/")
		switch {
		case r.Method == http.MethodHead && path == "wiki":
			f.heads++
		case r.Method == http.MethodPut && strings.HasPrefix(path, "wiki/"):
			if f.failPut {
				http.Error(w, "<Error><Code>InternalError</Code></Error>", 500)
				return
			}
			b, _ := io.ReadAll(r.Body)
			f.objects[path] = b
			f.types[path] = r.Header.Get("Content-Type")
		case r.Method == http.MethodGet && strings.HasPrefix(path, "wiki/"):
			b, ok := f.objects[path]
			if !ok {
				w.WriteHeader(404)
				_, _ = io.WriteString(w, `<?xml version="1.0"?><Error><Code>NoSuchKey</Code><Message>x</Message></Error>`)
				return
			}
			_, _ = w.Write(b)
		case r.Method == http.MethodDelete && strings.HasPrefix(path, "wiki/"):
			delete(f.objects, path)
			w.WriteHeader(204)
		default:
			w.WriteHeader(400)
		}
	}))
	t.Cleanup(srv.Close)
	return f, srv
}

func s3Env(t *testing.T) {
	t.Helper()
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIDTEST")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")
	t.Setenv("AWS_SESSION_TOKEN", "")
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	t.Setenv("AWS_PROFILE", "")
	t.Setenv("AWS_CONFIG_FILE", filepath.Join(t.TempDir(), "none"))
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(t.TempDir(), "none"))
}

func TestS3StoreRoundTrip(t *testing.T) {
	s3Env(t)
	fake, srv := newFakeS3(t)
	st, err := newS3Store(context.Background(), S3Config{Bucket: "wiki", Region: "us-east-1", Endpoint: srv.URL, Prefix: "team/a", PathStyle: true, AllowInsecureLoopback: true})
	if err != nil {
		t.Fatal(err)
	}
	if fake.heads != 1 {
		t.Fatalf("expected one HEAD bucket, got %d", fake.heads)
	}
	data := bytes.Repeat([]byte("0123456789"), 1000)
	if err = st.Put(context.Background(), "obj1", bytes.NewReader(data), int64(len(data)), "text/plain"); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(fake.objects["wiki/team/a/obj1"], data) || fake.types["wiki/team/a/obj1"] != "text/plain" {
		t.Fatalf("stored objects: %v", fake.types)
	}
	for _, a := range fake.auths {
		if !strings.HasPrefix(a, "AWS4-HMAC-SHA256 ") || !strings.Contains(a, "Credential=AKIDTEST/") || !strings.Contains(a, "/us-east-1/s3/") {
			t.Fatalf("request not signed correctly: %q", a)
		}
	}
	rc, err := st.Get(context.Background(), "obj1")
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if !bytes.Equal(got, data) {
		t.Fatal("get body mismatch")
	}
	if _, err = st.Get(context.Background(), "missing"); err != errObjectNotFound {
		t.Fatalf("expected not found, got %v", err)
	}
	if err = st.Delete(context.Background(), "obj1"); err != nil {
		t.Fatal(err)
	}
	if len(fake.objects) != 0 {
		t.Fatal("object not deleted")
	}
	for _, bad := range []string{"", "..", "a/b", "../x", "a\x00b"} {
		if st.Put(context.Background(), bad, strings.NewReader("x"), 1, "") == nil {
			t.Fatalf("key %q accepted", bad)
		}
	}
}

func TestS3StoreFailsClosed(t *testing.T) {
	s3Env(t)
	_, srv := newFakeS3(t)
	cases := map[string]S3Config{
		"no bucket":      {Region: "us-east-1"},
		"no region":      {Bucket: "wiki"},
		"http not opted": {Bucket: "wiki", Region: "us-east-1", Endpoint: srv.URL, PathStyle: true},
		"http non-lo":    {Bucket: "wiki", Region: "us-east-1", Endpoint: "http://example.com", AllowInsecureLoopback: true},
		"userinfo":       {Bucket: "wiki", Region: "us-east-1", Endpoint: "https://u:p@example.com"},
		"ftp":            {Bucket: "wiki", Region: "us-east-1", Endpoint: "ftp://example.com"},
		"dotdot prefix":  {Bucket: "wiki", Region: "us-east-1", Prefix: "a/../b"},
		"leading slash":  {Bucket: "wiki", Region: "us-east-1", Prefix: "/a"},
		"control prefix": {Bucket: "wiki", Region: "us-east-1", Prefix: "a\nb"},
		"bucket missing": {Bucket: "other", Region: "us-east-1", Endpoint: srv.URL, PathStyle: true, AllowInsecureLoopback: true},
	}
	for name, cfg := range cases {
		if st, err := newS3Store(context.Background(), cfg); err == nil || st != nil {
			t.Errorf("%s: expected failure", name)
		}
	}
}

func TestLocalStore(t *testing.T) {
	st := localStore{dir: t.TempDir()}
	ctx := context.Background()
	if err := st.Put(ctx, "a.txt", strings.NewReader("hello"), 5, "text/plain"); err != nil {
		t.Fatal(err)
	}
	if st.Put(ctx, "a.txt", strings.NewReader("hello"), 5, "") == nil {
		t.Fatal("overwrite allowed")
	}
	rc, err := st.Get(ctx, "a.txt")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(rc)
	rc.Close()
	if string(b) != "hello" {
		t.Fatal(string(b))
	}
	if _, err = st.Get(ctx, "../a.txt"); err == nil {
		t.Fatal("traversal accepted")
	}
	if err = st.Delete(ctx, "a.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err = st.Get(ctx, "a.txt"); err != errObjectNotFound {
		t.Fatal(err)
	}
}

func TestCorporateWithoutObjectsFailsClosed(t *testing.T) {
	s := &Server{DB: &database{pg: true}, DataDir: t.TempDir()}
	if _, err := s.objectStorage(); err == nil {
		t.Fatal("corporate server fell back to local storage")
	}
}

func uploadBody(t *testing.T, name string, content []byte) (*bytes.Buffer, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("file", name)
	_, _ = fw.Write(content)
	_ = mw.Close()
	return &buf, mw.FormDataContentType()
}

func TestAttachmentUploadAndDownloadViaS3(t *testing.T) {
	s3Env(t)
	fake, srv := newFakeS3(t)
	st, err := newS3Store(context.Background(), S3Config{Bucket: "wiki", Region: "us-east-1", Endpoint: srv.URL, Prefix: "att", PathStyle: true, AllowInsecureLoopback: true})
	if err != nil {
		t.Fatal(err)
	}
	s := testServer(t)
	s.objects = st
	body, ct := uploadBody(t, "note.html", []byte("<html><script>alert(1)</script></html>"))
	req := httptest.NewRequest(http.MethodPost, "/api/articles/start-here/attachments", body)
	req.Header.Set("Content-Type", ct)
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, req)
	if rec.Code != 201 {
		t.Fatalf("upload: %d %s", rec.Code, rec.Body)
	}
	if len(fake.objects) != 1 {
		t.Fatalf("objects: %d", len(fake.objects))
	}
	for k := range fake.objects {
		if !strings.HasPrefix(k, "wiki/att/") || strings.Contains(k, "note") {
			t.Fatalf("unexpected key %q", k)
		}
	}
	var id string
	if err = s.DB.QueryRow("SELECT id FROM attachments").Scan(&id); err != nil {
		t.Fatal(err)
	}
	rec = httptest.NewRecorder()
	s.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/files/"+id, nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "alert(1)") {
		t.Fatalf("download: %d %s", rec.Code, rec.Body)
	}
	if rec.Header().Get("X-Content-Type-Options") != "nosniff" || !strings.HasPrefix(rec.Header().Get("Content-Disposition"), "attachment") {
		t.Fatalf("headers: %v", rec.Header())
	}
	// Soft delete keeps the stored bytes.
	rec = httptest.NewRecorder()
	s.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/api/articles/start-here/attachments/"+id, nil))
	if rec.Code != 204 || len(fake.objects) != 1 {
		t.Fatalf("soft delete: %d objects=%d", rec.Code, len(fake.objects))
	}
}

func TestAttachmentDBFailureDeletesObject(t *testing.T) {
	s3Env(t)
	fake, srv := newFakeS3(t)
	st, err := newS3Store(context.Background(), S3Config{Bucket: "wiki", Region: "us-east-1", Endpoint: srv.URL, PathStyle: true, AllowInsecureLoopback: true})
	if err != nil {
		t.Fatal(err)
	}
	s := testServer(t)
	s.objects = st
	if _, err = s.DB.Exec("DROP TABLE attachments"); err != nil {
		t.Fatal(err)
	}
	body, ct := uploadBody(t, "a.txt", []byte("data"))
	req := httptest.NewRequest(http.MethodPost, "/api/articles/start-here/attachments", body)
	req.Header.Set("Content-Type", ct)
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, req)
	if rec.Code != 500 {
		t.Fatalf("expected 500, got %d", rec.Code)
	}
	if len(fake.objects) != 0 {
		t.Fatal("orphaned object left behind")
	}
}

func TestLocalAttachmentUploadAndDownload(t *testing.T) {
	s := testServer(t)
	body, ct := uploadBody(t, "../evil.txt", []byte("plain text"))
	req := httptest.NewRequest(http.MethodPost, "/api/articles/start-here/attachments", body)
	req.Header.Set("Content-Type", ct)
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, req)
	if rec.Code != 201 {
		t.Fatalf("upload: %d %s", rec.Code, rec.Body)
	}
	var id, stored string
	if err := s.DB.QueryRow("SELECT id,stored_name FROM attachments").Scan(&id, &stored); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(s.DataDir, "files", stored)); err != nil {
		t.Fatal(err)
	}
	rec = httptest.NewRecorder()
	s.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/files/"+id, nil))
	if rec.Code != 200 || rec.Body.String() != "plain text" || rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("download: %d %q %v", rec.Code, rec.Body, rec.Header())
	}
}

func TestStorageSniffRewindsFromEnd(t *testing.T) {
	png := []byte{137, 80, 78, 71, 13, 10, 26, 10, 0, 0, 0, 13}
	reader := bytes.NewReader(png)
	reader.Seek(0, io.SeekEnd)
	mime, err := sniffMIME(reader)
	if err != nil || mime != "image/png" {
		t.Fatalf("sniff %s %v", mime, err)
	}
	pos, _ := reader.Seek(0, io.SeekCurrent)
	if pos != 0 {
		t.Fatal("sniff did not rewind upload")
	}
}
