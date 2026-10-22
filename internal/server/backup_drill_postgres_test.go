package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

// Backup and restore drill (see docs/BACKUP.md and scripts/backup-drill.mjs).
//
// It destroys the database and the bucket prefix it is pointed at, so it runs
// only when every variable below is set AND the PostgreSQL container name
// starts with "aiwiki-drill-". It needs a real PostgreSQL (with podman to run
// pg_dump/pg_restore inside the container) and a real S3-compatible bucket.
//
//	AIWIKI_TEST_POSTGRES_OWNER   migration owner  (aiwiki_owner)
//	AIWIKI_TEST_POSTGRES         runtime role     (aiwiki_app)
//	AIWIKI_TEST_DRILL_BACKUP     backup role URL  (BYPASSRLS, pg_read_all_data)
//	AIWIKI_TEST_DRILL_ADMIN      superuser URL of the maintenance database "postgres"
//	AIWIKI_TEST_DRILL_PG         PostgreSQL container name (aiwiki-drill-*)
//	AIWIKI_TEST_S3_ENDPOINT, AIWIKI_TEST_S3_BUCKET (+ AWS_* credentials)
//	AIWIKI_TEST_DRILL_BINARY     optional: built aiwiki binary for exit-code checks

func drillPodman(t *testing.T, container string, env []string, args ...string) string {
	t.Helper()
	full := []string{"exec"}
	for _, e := range env {
		full = append(full, "-e", e)
	}
	full = append(full, container)
	full = append(full, args...)
	cmd := exec.Command("podman", full...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		t.Fatalf("podman %s failed: %v\n%s", args[0], err, errb.String())
	}
	return out.String()
}

type drillClient struct {
	t      *testing.T
	s      *Server
	cookie *http.Cookie
	csrf   string
}

func (c *drillClient) do(method, path string, body []byte, contentType string) (int, []byte, http.Header) {
	c.t.Helper()
	req := httptest.NewRequest(method, "http://127.0.0.1:8080"+path, bytes.NewReader(body))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	} else if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.cookie != nil {
		req.AddCookie(c.cookie)
		req.Header.Set("X-CSRF-Token", c.csrf)
	}
	rr := httptest.NewRecorder()
	c.s.Handler().ServeHTTP(rr, req)
	return rr.Code, rr.Body.Bytes(), rr.Header()
}

func (c *drillClient) mustJSON(method, path string, body any, want int, into any) {
	c.t.Helper()
	var raw []byte
	if body != nil {
		raw, _ = json.Marshal(body)
	}
	code, resp, _ := c.do(method, path, raw, "")
	if code != want {
		c.t.Fatalf("%s %s: %d (want %d) %s", method, path, code, want, resp)
	}
	if into != nil {
		if err := json.Unmarshal(resp, into); err != nil {
			c.t.Fatalf("%s %s: bad JSON: %v", method, path, err)
		}
	}
}

func (c *drillClient) upload(articleID, name string, content []byte) Attachment {
	c.t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("file", name)
	_, _ = fw.Write(content)
	_ = mw.Close()
	code, resp, _ := c.do("POST", "/api/articles/"+articleID+"/attachments", buf.Bytes(), mw.FormDataContentType())
	if code != 201 {
		c.t.Fatalf("upload: %d %s", code, resp)
	}
	var a Attachment
	if err := json.Unmarshal(resp, &a); err != nil {
		c.t.Fatal(err)
	}
	return a
}

// drillLogin signs in through the real OIDC callback of s against the mock issuer.
func drillLogin(t *testing.T, s *Server, iss *authMockIssuer, sub string) *drillClient {
	t.Helper()
	c := &drillClient{t: t, s: s}
	req := httptest.NewRequest("GET", "http://127.0.0.1:8080/auth/login", nil)
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusFound {
		t.Fatalf("login start: %d", rr.Code)
	}
	loc, _ := url.Parse(rr.Header().Get("Location"))
	q := loc.Query()
	g := authUserGrant(sub, "aiwiki-access")
	g.nonce = q.Get("nonce")
	code := iss.issueCode(q.Get("code_challenge"), g)
	cb := httptest.NewRequest("GET", "http://127.0.0.1:8080/auth/callback?code="+code+"&state="+url.QueryEscape(q.Get("state")), nil)
	cb.AddCookie(authCookie(rr, s.auth.loginCookie))
	cr := httptest.NewRecorder()
	s.Handler().ServeHTTP(cr, cb)
	c.cookie = authCookie(cr, s.auth.sessionCookie)
	if cr.Code != http.StatusSeeOther || c.cookie == nil {
		t.Fatalf("login callback: %d", cr.Code)
	}
	var me struct {
		CSRF string `json:"csrf_token"`
	}
	c.mustJSON("GET", "/api/me", nil, 200, &me)
	c.csrf = me.CSRF
	return c
}

func drillRowCounts(t *testing.T, backupURL string) map[string]int {
	t.Helper()
	cfg, err := pgx.ParseConfig(backupURL)
	if err != nil {
		t.Fatal(err)
	}
	db := stdlib.OpenDB(*cfg)
	defer db.Close()
	rows, err := db.Query(`SELECT tablename FROM pg_tables WHERE schemaname='public' ORDER BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for rows.Next() {
		var n string
		_ = rows.Scan(&n)
		names = append(names, n)
	}
	rows.Close()
	out := map[string]int{}
	for _, n := range names {
		var c int
		// table names come from pg_tables, not from input
		if err = db.QueryRow(`SELECT count(*) FROM public."` + n + `"`).Scan(&c); err != nil {
			t.Fatalf("count %s: %v", n, err)
		}
		out[n] = c
	}
	return out
}

func drillRun(t *testing.T, binary string, env []string) (int, string) {
	t.Helper()
	cmd := exec.Command(binary, "check-storage", "-verify-hashes")
	cmd.Env = append(os.Environ(), env...)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	if err == nil {
		return 0, out.String()
	}
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.ExitCode(), out.String()
	}
	t.Fatal(err)
	return -1, ""
}

func TestBackupDrillPostgresS3(t *testing.T) {
	ownerURL, appURL := os.Getenv("AIWIKI_TEST_POSTGRES_OWNER"), os.Getenv("AIWIKI_TEST_POSTGRES")
	backupURL, adminURL := os.Getenv("AIWIKI_TEST_DRILL_BACKUP"), os.Getenv("AIWIKI_TEST_DRILL_ADMIN")
	container := os.Getenv("AIWIKI_TEST_DRILL_PG")
	endpoint, bucket := os.Getenv("AIWIKI_TEST_S3_ENDPOINT"), os.Getenv("AIWIKI_TEST_S3_BUCKET")
	if ownerURL == "" || appURL == "" || backupURL == "" || adminURL == "" || container == "" || endpoint == "" || bucket == "" {
		t.Skip("set the AIWIKI_TEST_DRILL_* , AIWIKI_TEST_POSTGRES* and AIWIKI_TEST_S3_* variables to run the backup drill (see scripts/backup-drill.mjs)")
	}
	if !strings.HasPrefix(container, "aiwiki-drill-") {
		t.Fatal("the drill destroys its database: the container name must start with aiwiki-drill-")
	}
	ctx := context.Background()
	const prefix = "drill/"
	s3cfg := S3Config{Bucket: bucket, Region: "us-east-1", Endpoint: endpoint, Prefix: prefix, PathStyle: true, AllowInsecureLoopback: true}
	store, err := newS3Store(ctx, s3cfg)
	if err != nil {
		t.Fatal(err)
	}
	wipeBucket := func() {
		objs, err := store.List(ctx, "")
		if err != nil {
			t.Fatal(err)
		}
		for _, o := range objs {
			if err = store.Delete(ctx, o.Key); err != nil {
				t.Fatal(err)
			}
		}
	}
	wipeBucket()
	iss := newAuthMockIssuer(t)
	open := func() *Server {
		t.Helper()
		oc, cancel := context.WithTimeout(ctx, 60*time.Second)
		defer cancel()
		s, err := OpenCorporate(oc, CorporateConfig{
			S3: s3cfg, DatabaseURL: appURL, MigrationDatabaseURL: ownerURL, RuntimeRole: "aiwiki_app",
			IssuerURL: iss.srv.URL, ClientID: authTestClientID, ClientSecret: authTestSecret, RedirectURL: authTestRedirect,
			AllowInsecureHTTP: true,
		}, t.TempDir(), "")
		if err != nil {
			t.Fatalf("OpenCorporate: %v", err)
		}
		return s
	}
	checkCfg := StorageCheckConfig{DatabaseURL: backupURL, S3: s3cfg, VerifyHashes: true}
	check := func() *StorageReport {
		t.Helper()
		var out bytes.Buffer
		cfg := checkCfg
		cfg.Out = &out
		rep, err := CheckStorage(ctx, cfg)
		if err != nil {
			t.Fatalf("CheckStorage: %v", err)
		}
		t.Logf("check-storage report:\n%s", out.String())
		return rep
	}

	// ---- 1. build a workload -------------------------------------------------
	// The destination database may hold leftovers from a previous run.
	{
		odb, err := openPostgres(ctx, ownerURL)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = odb.Exec(`DROP SCHEMA public CASCADE; CREATE SCHEMA public`); err != nil {
			t.Fatal(err)
		}
		odb.Close()
	}
	s1 := open()
	alice := drillLogin(t, s1, iss, "alice")
	var space Space
	alice.mustJSON("POST", "/api/corporate/spaces", map[string]any{"name": "Drill", "personal": true}, 201, &space)
	var art Article
	alice.mustJSON("POST", "/api/articles", map[string]any{"space_id": space.ID, "title": "Drill article", "body": "# First\nrevision one"}, 201, &art)
	alice.mustJSON("PUT", "/api/articles/"+art.ID, map[string]any{"body": "# Second\nrevision two, Привет", "base_revision": 1, "message": "second"}, 200, nil)
	attBytes := make([]byte, 40000)
	_, _ = rand.Read(attBytes)
	att := alice.upload(art.ID, "data.bin", attBytes)
	att2Bytes := []byte("second attachment, soft-deleted afterwards")
	att2 := alice.upload(art.ID, "gone.txt", att2Bytes)
	if code, resp, _ := alice.do("DELETE", "/api/articles/"+art.ID+"/attachments/"+att2.ID, nil, ""); code >= 300 {
		t.Fatalf("delete attachment: %d %s", code, resp)
	}
	shared := make([]byte, 3000) // identical bytes in two artifacts: one content-addressed object
	_, _ = rand.Read(shared)
	sharedB64 := base64.StdEncoding.EncodeToString(shared)
	mkFiles := func(extra ...map[string]any) []map[string]any {
		files := []map[string]any{
			{"path": "index.html", "content": "<!doctype html><title>drill</title><h1 id=m>DRILL-MARKER-A</h1><script src=app.js></script>"},
			{"path": "shared.png", "content": sharedB64, "encoding": "base64"},
		}
		return append(files, extra...)
	}
	var a1, a2 artifactDetail
	alice.mustJSON("POST", "/api/artifacts", map[string]any{"space_id": space.ID, "title": "Drill artifact one", "files": mkFiles(map[string]any{"path": "app.js", "content": "window.v=1"})}, 201, &a1)
	alice.mustJSON("POST", "/api/artifacts", map[string]any{"space_id": space.ID, "title": "Drill artifact two", "files": []map[string]any{
		{"path": "index.html", "content": "<h1>DRILL-MARKER-B</h1>"}, {"path": "copy.png", "content": sharedB64, "encoding": "base64"}}}, 201, &a2)
	alice.mustJSON("PUT", "/api/artifacts/"+a1.ID, map[string]any{"base_revision": 1, "message": "v2", "files": []map[string]any{{"path": "app.js", "content": "window.v=2"}}}, 200, nil)
	var share struct{ URL string }
	alice.mustJSON("POST", "/api/artifacts/"+a1.ID+"/shares", map[string]any{}, 201, &share)
	if share.URL == "" {
		t.Fatal("no public link")
	}

	before := drillRowCounts(t, backupURL)
	rep := check()
	// 2 attachments + blobs: index A, shared (once), app v1, app v2, index B = 5... counted from the database
	if rep.Failed() || rep.References < 6 || rep.References != rep.Objects || len(rep.Orphans) != 0 {
		t.Fatalf("workload check: %+v", rep)
	}
	refsBefore, objsBefore := rep.References, rep.Objects
	var artifactObjects int
	{
		objs, _ := store.List(ctx, "art-")
		artifactObjects = len(objs)
		sharedHash := shaHex(shared)
		n := 0
		for _, o := range objs {
			if o.Key == "art-"+sharedHash {
				n++
			}
		}
		if n != 1 {
			t.Fatalf("shared bytes stored %d times", n)
		}
	}

	// ---- 2. backup: database FIRST, then objects ------------------------------
	dumpPath := "/tmp/aiwiki-drill.dump"
	drillPodman(t, container, []string{"PGPASSWORD=backup-ci-only"}, "pg_dump", "-h", "127.0.0.1", "-U", "aiwiki_backup", "-d", "aiwiki_ci", "-Fc", "-f", dumpPath)
	// A write that lands after the dump but before the object copy: its object
	// is in the copy, its row is not (the safe direction of the ordering rule).
	late := alice.upload(art.ID, "late.txt", []byte("uploaded between dump and object copy"))
	objectCopy := map[string][]byte{}
	{
		objs, err := store.List(ctx, "")
		if err != nil {
			t.Fatal(err)
		}
		for _, o := range objs {
			rc, err := store.Get(ctx, o.Key)
			if err != nil {
				t.Fatal(err)
			}
			b, _ := io.ReadAll(rc)
			rc.Close()
			objectCopy[o.Key] = b
		}
	}
	if _, ok := objectCopy[late.URL[len("/files/"):]]; !ok {
		t.Fatal("late object missing from the object copy")
	}

	// ---- 3. disaster: drop the database and wipe the bucket --------------------
	s1.Close()
	{
		cfg, _ := pgx.ParseConfig(adminURL)
		adm := stdlib.OpenDB(*cfg)
		for _, q := range []string{`DROP DATABASE aiwiki_ci WITH (FORCE)`, `CREATE DATABASE aiwiki_ci OWNER aiwiki_owner`} {
			if _, err = adm.Exec(q); err != nil {
				t.Fatalf("%s: %v", q, err)
			}
		}
		adm.Close()
	}
	wipeBucket()
	if objs, _ := store.List(ctx, ""); len(objs) != 0 {
		t.Fatal("bucket not empty")
	}
	// A check against the destroyed database must fail loudly, never report "OK".
	if _, err = CheckStorage(ctx, StorageCheckConfig{DatabaseURL: backupURL, S3: s3cfg}); err == nil {
		t.Fatal("check-storage succeeded against an empty database")
	}
	{
		cfg, _ := pgx.ParseConfig(ownerURL)
		odb := stdlib.OpenDB(*cfg)
		var n sql.NullString
		_ = odb.QueryRow(`SELECT to_regclass('public.articles')::text`).Scan(&n)
		odb.Close()
		if n.Valid {
			t.Fatal("database was not destroyed")
		}
	}

	// ---- 4. restore ------------------------------------------------------------
	// Roles are cluster objects and survive (documented). Restore as the owner.
	drillPodman(t, container, []string{"PGPASSWORD=owner-ci-only"}, "pg_restore", "-h", "127.0.0.1", "-U", "aiwiki_owner", "-d", "aiwiki_ci", "--no-owner", "--exit-on-error", dumpPath)
	keys := make([]string, 0, len(objectCopy))
	for k := range objectCopy {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		b := objectCopy[k]
		if err = store.Put(ctx, k, bytes.NewReader(b), int64(len(b)), "application/octet-stream"); err != nil {
			t.Fatal(err)
		}
	}
	after := drillRowCounts(t, backupURL)
	if fmt.Sprint(before) != fmt.Sprint(after) {
		t.Fatalf("row counts differ after restore:\nbefore %v\nafter  %v", before, after)
	}

	// ---- 5. start the server on the restored data and verify through HTTP --------
	s2 := open() // applies missing migrations, startup checks, new random artifact secret
	defer s2.Close()
	if code, _, _ := (&drillClient{t: t, s: s2, cookie: alice.cookie, csrf: alice.csrf}).do("GET", "/api/me", nil, ""); code != 401 {
		t.Fatalf("a pre-disaster session survived the restore: %d", code)
	}
	alice2 := drillLogin(t, s2, iss, "alice")
	var got Article
	alice2.mustJSON("GET", "/api/articles/"+art.ID, nil, 200, &got)
	if got.Revision != 2 || !strings.Contains(got.Body, "revision two, Привет") {
		t.Fatalf("article after restore: rev %d", got.Revision)
	}
	var hist []map[string]any
	alice2.mustJSON("GET", "/api/articles/"+art.ID+"/history", nil, 200, &hist)
	if len(hist) < 2 {
		t.Fatalf("revision history after restore: %d entries", len(hist))
	}
	if code, body, _ := alice2.do("GET", att.URL, nil, ""); code != 200 || !bytes.Equal(body, attBytes) {
		t.Fatalf("attachment after restore: %d, bytes equal=%v", code, bytes.Equal(body, attBytes))
	}
	if code, body, _ := alice2.do("GET", att2.URL, nil, ""); code != 404 && code != 410 {
		t.Fatalf("soft-deleted attachment is served again: %d %s", code, body)
	}
	for _, f := range []struct {
		id, path string
		want     []byte
	}{{a1.ID, "shared.png", shared}, {a2.ID, "copy.png", shared}, {a1.ID, "app.js", []byte("window.v=2")}} {
		if code, body, _ := alice2.do("GET", "/api/artifacts/"+f.id+"/files/"+f.path, nil, ""); code != 200 || !bytes.Equal(body, f.want) {
			t.Fatalf("artifact file %s after restore: %d", f.path, code)
		}
	}
	if code, body, _ := alice2.do("GET", "/api/artifacts/"+a1.ID+"/files/app.js?revision=1", nil, ""); code != 200 || string(body) != "window.v=1" {
		t.Fatalf("old artifact revision after restore: %d %s", code, body)
	}
	var view struct {
		FrameURL string `json:"frame_url"`
	}
	alice2.mustJSON("POST", "/api/artifacts/"+a1.ID+"/view", map[string]any{}, 200, &view)
	if code, body, _ := alice2.do("GET", view.FrameURL, nil, ""); code != 200 || !strings.Contains(string(body), "DRILL-MARKER-A") {
		t.Fatalf("artifact frame after restore: %d", code)
	}
	anon := &drillClient{t: t, s: s2}
	code, page, _ := anon.do("GET", share.URL, nil, "")
	if code != 200 {
		t.Fatalf("public link after restore: %d", code)
	}
	if code, body, _ := anon.do("GET", publicFrame(t, s2, page), nil, ""); code != 200 || !strings.Contains(string(body), "DRILL-MARKER-A") {
		t.Fatalf("public frame after restore: %d", code)
	}

	// ---- 6. storage consistency after restore ---------------------------------------
	rep = check()
	if rep.Failed() || len(rep.Missing) != 0 || rep.References != refsBefore || rep.Objects != objsBefore+1 || len(rep.Orphans) != 1 {
		t.Fatalf("after restore (expected the one late upload as orphan): %+v", rep)
	}
	if !strings.HasSuffix(late.URL, rep.Orphans[0].Key) {
		t.Fatalf("unexpected orphan %q", rep.Orphans[0].Key)
	}
	_ = artifactObjects
	binary := os.Getenv("AIWIKI_TEST_DRILL_BINARY")
	cliEnv := []string{"AIWIKI_BACKUP_DATABASE_URL=" + backupURL, "AIWIKI_S3_BUCKET=" + bucket, "AIWIKI_S3_REGION=us-east-1", "AIWIKI_S3_ENDPOINT=" + endpoint, "AIWIKI_S3_PREFIX=" + prefix, "AIWIKI_S3_PATH_STYLE=1", "AIWIKI_ALLOW_INSECURE_LOOPBACK=1"}
	if binary != "" {
		if code, out := drillRun(t, binary, cliEnv); code != 0 {
			t.Fatalf("aiwiki check-storage exit %d on a healthy restore:\n%s", code, out)
		}
	}

	// ---- 7. negative: lose one object ------------------------------------------------
	lostKey := att.URL[len("/files/"):]
	if err = store.Delete(ctx, lostKey); err != nil {
		t.Fatal(err)
	}
	rep = check()
	if !rep.Failed() || len(rep.Missing) != 1 || rep.Missing[0].Key != lostKey {
		t.Fatalf("a lost object was not reported: %+v", rep)
	}
	if code, _, _ := alice2.do("GET", att.URL, nil, ""); code < 400 {
		t.Fatalf("the lost attachment still downloads: %d", code)
	}
	if binary != "" {
		code, out := drillRun(t, binary, cliEnv)
		if code != 1 || !strings.Contains(out, lostKey) || !strings.Contains(out, "RESULT: FAILED") {
			t.Fatalf("aiwiki check-storage exit %d for a lost object:\n%s", code, out)
		}
	}
	if err = store.Put(ctx, lostKey, bytes.NewReader(attBytes), int64(len(attBytes)), ""); err != nil {
		t.Fatal(err)
	}
	if rep = check(); rep.Failed() {
		t.Fatalf("restoring the object did not heal the report: %+v", rep)
	}

	// ---- 8. negative: same-size corruption is caught by -verify-hashes ------------------
	blobKey := "art-" + shaHex(shared)
	bad := bytes.Repeat([]byte{'x'}, len(shared))
	if err = store.Put(ctx, blobKey, bytes.NewReader(bad), int64(len(bad)), ""); err != nil {
		t.Fatal(err)
	}
	cheap := checkCfg
	cheap.VerifyHashes = false
	cheap.Out = io.Discard
	if r, err := CheckStorage(ctx, cheap); err != nil || r.Failed() {
		t.Fatalf("size-only check should not see same-size corruption: %+v %v", r, err)
	}
	rep = check()
	if !rep.Failed() || len(rep.HashMismatch) != 1 || rep.HashMismatch[0] != blobKey {
		t.Fatalf("corruption not reported: %+v", rep)
	}
	if err = store.Put(ctx, blobKey, bytes.NewReader(shared), int64(len(shared)), ""); err != nil {
		t.Fatal(err)
	}
	if rep = check(); rep.Failed() {
		t.Fatalf("report did not heal: %+v", rep)
	}
	t.Logf("drill OK: %d table row counts identical, %d references, %d objects", len(after), refsBefore, objsBefore)
	wipeBucket()
}
