package server

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"mime/multipart"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// PostgreSQL tests of the per-space storage quotas (docs/OPERATIONS.md section 5).

const quotaBody = `{"error":"space storage quota exceeded"}`

func rep(c byte, n int) string { return strings.Repeat(string(c), n) }

// quotaEnv is an artifact environment whose servers carry a default quota.
func newQuotaEnv(t *testing.T, def int64) *artEnv {
	t.Helper()
	e := newArtEnv(t)
	e.root.spaceQuotaDefault = def
	e.users = map[string]*Server{}
	return e
}

// setQuota sets a per-space override as the system administrator.
func (e *artEnv) setQuota(space string, bytes int64) {
	e.t.Helper()
	if code, raw := e.do("admin", "PATCH", "/api/spaces/"+space, fmt.Sprintf(`{"storage_quota_bytes":%d}`, bytes)); code != 200 {
		e.t.Fatalf("set quota: %d %s", code, raw)
	}
}

// upload posts a multipart attachment as user.
func (e *artEnv) upload(user, article, name, content string) (int, string) {
	e.t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("file", name)
	_, _ = fw.Write([]byte(content))
	_ = mw.Close()
	s := e.as(user)
	db, done, err := s.DB.resumeAfterTransfer(context.Background())
	if err != nil {
		e.t.Fatal(err)
	}
	defer done()
	cp := *s
	cp.DB = db
	req := httptest.NewRequest("POST", "/api/articles/"+article+"/attachments", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rr := httptest.NewRecorder()
	cp.routes().ServeHTTP(rr, req)
	return rr.Code, strings.TrimSpace(rr.Body.String())
}

func (e *artEnv) storage(user, space string) (int, string) {
	e.t.Helper()
	code, raw := e.do(user, "GET", "/api/spaces/"+space+"/storage", nil)
	return code, strings.TrimSpace(string(raw))
}

func (e *artEnv) usedBy(user, space string) int64 {
	e.t.Helper()
	code, raw := e.storage(user, space)
	if code != 200 {
		e.t.Fatalf("storage as %s: %d %s", user, code, raw)
	}
	var out spaceStorageOut
	decodeJSON(e.t, []byte(raw), &out)
	return out.Used
}

func (e *artEnv) artFile(user string, extra map[string]any, content string) (int, string, artifactDetail) {
	e.t.Helper()
	body := map[string]any{"space_id": e.space, "title": "Квота", "files": []fileSpec{{Path: "index.html", Content: content}}}
	for k, v := range extra {
		body[k] = v
	}
	code, raw := e.do(user, "POST", "/api/artifacts", body)
	var d artifactDetail
	if code == 201 {
		decodeJSON(e.t, raw, &d)
	}
	return code, strings.TrimSpace(string(raw)), d
}

func TestStorageQuotaAttachments(t *testing.T) {
	e := newQuotaEnv(t, 1000)
	art := e.article("alice", e.space, "", "", "С вложениями")

	code, body := e.upload("alice", art.ID, "a.bin", rep('a', 600))
	if code != 201 {
		t.Fatalf("first upload: %d %s", code, body)
	}
	var first Attachment
	decodeJSON(t, []byte(body), &first)
	code, body = e.upload("editor", art.ID, "b.bin", rep('b', 600))
	if code != 413 || body != quotaBody {
		t.Fatalf("second upload: %d %q", code, body)
	}
	if code, body = e.upload("alice", art.ID, "c.bin", rep('c', 400)); code != 201 {
		t.Fatalf("exactly at the limit: %d %s", code, body)
	}
	if code, body = e.upload("alice", art.ID, "d.bin", "x"); code != 413 || body != quotaBody {
		t.Fatalf("beyond the limit: %d %q", code, body)
	}
	// A refused upload leaves nothing behind in the object store.
	if n := len(e.root.objects.(*memStore).m); n != 2 {
		t.Errorf("objects after refusals: %d", n)
	}
	if got := e.usedBy("alice", e.space); got != 1000 {
		t.Errorf("used %d", got)
	}
	// A deleted attachment is restorable and keeps counting.
	if e.status("alice", "DELETE", "/api/articles/"+art.ID+"/attachments/"+first.ID, nil) != 204 {
		t.Fatal("delete attachment")
	}
	if code, body = e.upload("alice", art.ID, "e.bin", "x"); code != 413 || body != quotaBody {
		t.Errorf("after deleting an attachment: %d %q", code, body)
	}
	if got := e.usedBy("alice", e.space); got != 1000 {
		t.Errorf("used after delete %d", got)
	}
	// A reader cannot upload at all; the answer is not a quota answer.
	if code, _ = e.upload("reader", art.ID, "r.bin", "x"); code != 403 {
		t.Errorf("reader upload: %d", code)
	}
}

func TestStorageQuotaArtifacts(t *testing.T) {
	e := newQuotaEnv(t, 3000)
	code, body, a1 := e.artFile("alice", nil, rep('a', 1000))
	if code != 201 {
		t.Fatalf("create: %d %s", code, body)
	}
	// an artifact inside a project, for the MCP key below
	ap := e.artEnvArtifact(e.space, e.proj, "alice", rep('z', 10))
	// A hash counts once per artifact, not once per space: the same content in a
	// second artifact is paid for again (no credit for what another artifact holds).
	if code, body, _ = e.artFile("alice", nil, rep('a', 1000)); code != 201 {
		t.Fatalf("same content again: %d %s", code, body)
	}
	if got := e.usedBy("alice", e.space); got != 2010 {
		t.Fatalf("used after duplicate: %d", got)
	}
	if code, body, _ = e.artFile("alice", nil, rep('b', 1000)); code != 413 || body != quotaBody {
		t.Fatalf("over the limit: %d %q", code, body)
	}
	if n := len(e.root.objects.(*memStore).m); n != 2 {
		t.Errorf("a refused artifact left %d objects", n-2)
	}
	if code, body, _ = e.artFile("alice", nil, rep('b', 990)); code != 201 {
		t.Fatalf("exactly at the limit: %d %s", code, body)
	}
	path := "/api/artifacts/" + a1.ID
	// Growth is refused, a title change and a reuse of content the same artifact already stores are not.
	code, raw := e.do("alice", "PUT", path, map[string]any{"base_revision": 1, "files": []fileSpec{{Path: "n.txt", Content: "1"}}})
	if code != 413 || strings.TrimSpace(string(raw)) != quotaBody {
		t.Fatalf("update with new bytes: %d %s", code, raw)
	}
	if code, raw = e.do("alice", "PUT", path, map[string]any{"base_revision": 1, "title": "Другое"}); code != 200 {
		t.Fatalf("title only: %d %s", code, raw)
	}
	if code, raw = e.do("alice", "PUT", path, map[string]any{"base_revision": 2, "files": []fileSpec{{Path: "copy.txt", Content: rep('a', 1000)}}}); code != 200 {
		t.Fatalf("reuse of stored content: %d %s", code, raw)
	}
	// Deleting is always possible; the deleted artifact (restorable) still counts.
	if code, raw = e.do("alice", "DELETE", path, map[string]any{"base_revision": 3}); code != 200 {
		t.Fatalf("delete: %d %s", code, raw)
	}
	if got := e.usedBy("alice", e.space); got != 3000 {
		t.Errorf("used after delete: %d", got)
	}
	if code, body, _ = e.artFile("alice", nil, rep('c', 10)); code != 413 || body != quotaBody {
		t.Errorf("create after delete: %d %q", code, body)
	}
	if code, raw = e.do("alice", "POST", path+"/restore", map[string]any{"base_revision": 4}); code != 200 {
		t.Fatalf("restore: %d %s", code, raw)
	}

	// MCP: the tool error says the same, with no numbers.
	rw := e.mcpAs("alice", "write", e.proj)
	isErr, text := mcpCall(rw, "wiki_artifact_write", map[string]any{"space_id": e.space, "project_id": e.proj, "title": "mcp", "files": []map[string]any{{"path": "index.html", "content": rep('m', 50)}}})
	if !isErr || !strings.Contains(text, spaceQuotaMessage) || strings.ContainsAny(text, "0123456789") {
		t.Errorf("MCP create: %v %q", isErr, text)
	}
	isErr, text = mcpCall(rw, "wiki_artifact_write", map[string]any{"id": ap.ID, "base_revision": 1, "files": []map[string]any{{"path": "more.txt", "content": rep('m', 50)}}})
	if !isErr || !strings.Contains(text, spaceQuotaMessage) || strings.ContainsAny(text, "0123456789") {
		t.Errorf("MCP update: %v %q", isErr, text)
	}
}

func TestStorageQuotaPrivateArtifactsCountAndVisibility(t *testing.T) {
	e := newQuotaEnv(t, 2000)
	if code, body, _ := e.artFile("editor", map[string]any{"visibility": "private"}, rep('p', 1500)); code != 201 {
		t.Fatalf("private create: %d %s", code, body)
	}
	// alice cannot see the private artifact, but it counts against the space.
	if code, body, _ := e.artFile("alice", nil, rep('q', 600)); code != 413 || body != quotaBody {
		t.Fatalf("create over a hidden artifact: %d %q", code, body)
	}
	code, body, mine := e.artFile("alice", nil, rep('r', 400))
	if code != 201 {
		t.Fatalf("create within the limit: %d %s", code, body)
	}
	// No oracle: the exact content of the hidden private artifact earns no credit,
	// neither in a new artifact nor as an added file of an existing one.
	if code, body, _ := e.artFile("alice", nil, rep('p', 1500)); code != 413 || body != quotaBody {
		t.Errorf("credit for another artifact's content on create: %d %q", code, body)
	}
	if code, raw := e.do("alice", "PUT", "/api/artifacts/"+mine.ID, map[string]any{"base_revision": 1, "files": []fileSpec{{Path: "copy.txt", Content: rep('p', 1500)}}}); code != 413 || strings.TrimSpace(string(raw)) != quotaBody {
		t.Errorf("credit for another artifact's content on update: %d %s", code, raw)
	}
	for _, u := range []string{"alice", "bob", "admin"} {
		if got := e.usedBy(u, e.space); got != 1900 {
			t.Errorf("%s sees used %d", u, got)
		}
	}
	code, raw := e.storage("alice", e.space)
	var out spaceStorageOut
	decodeJSON(t, []byte(raw), &out)
	if code != 200 || out.Limit != 2000 || out.Default != 2000 || out.Override != nil {
		t.Errorf("limit fields: %d %s", code, raw)
	}
	// Everybody else gets the same plain refusal, existing space or not.
	for _, u := range []string{"editor", "commenter", "reader", "u1"} {
		for _, sp := range []string{e.space, "no-such-space"} {
			if code, raw := e.storage(u, sp); code != 403 || raw != `{"error":"forbidden"}` {
				t.Errorf("%s on %s: %d %s", u, sp, code, raw)
			}
		}
	}
	if code, raw := e.storage("admin", "no-such-space"); code != 403 || raw != `{"error":"forbidden"}` {
		t.Errorf("admin on a missing space: %d %s", code, raw)
	}
	// A key limited to projects never sees usage, even of its owner who manages the space.
	if code := corporateHTTP(e.mcpAs("alice", "write", e.proj), "GET", "/api/spaces/"+e.space+"/storage", "").Code; code != 403 {
		t.Errorf("MCP-scoped usage: %d", code)
	}

	// The same holds at the SQL level: the functions answer nothing to the unentitled.
	var used sql.NullInt64
	if err := e.as("editor").DB.QueryRow(`SELECT space_storage_used(?)`, e.space).Scan(&used); err != nil || used.Valid {
		t.Errorf("editor space_storage_used: %v %v", used, err)
	}
	if err := e.as("u1").DB.QueryRow(`SELECT space_storage_used(?)`, e.space).Scan(&used); err != nil || used.Valid {
		t.Errorf("outsider space_storage_used: %v %v", used, err)
	}
	var ov sql.NullInt64
	if err := e.as("reader").DB.QueryRow(`SELECT space_storage_override(?)`, e.space).Scan(&ov); err != nil || ov.Valid {
		t.Errorf("reader space_storage_override: %v %v", ov, err)
	}
	var ok bool
	for _, u := range []string{"reader", "commenter", "u1"} {
		err := e.as(u).DB.QueryRow(`SELECT space_storage_admit(?,?,?,?,?)`, e.space, "", int64(1), []string{"a:x"}, []int64{1}).Scan(&ok)
		var pg *pgconn.PgError
		if err == nil || !asPg(err, &pg) || pg.Code != "42501" {
			t.Errorf("%s space_storage_admit: %v %v", u, ok, err)
		}
	}
	var n sql.NullInt64
	err := e.as("alice").DB.QueryRow(`SELECT count(*) FROM space_storage_objects(?)`, e.space).Scan(&n)
	var pg *pgconn.PgError
	if err == nil || !asPg(err, &pg) || pg.Code != "42501" {
		t.Errorf("space_storage_objects callable by the runtime role: %v", err)
	}
	// A role that can read nothing of a space still gets no usage by calling as the user.
	if err := e.as("admin").DB.QueryRow(`SELECT space_storage_used(?)`, e.space).Scan(&used); err != nil || !used.Valid || used.Int64 != 1900 {
		t.Errorf("admin space_storage_used: %v %v", used, err)
	}
}

func asPg(err error, target **pgconn.PgError) bool {
	for err != nil {
		if p, ok := err.(*pgconn.PgError); ok {
			*target = p
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

func TestStorageQuotaAdministration(t *testing.T) {
	e := newQuotaEnv(t, 1000)
	art := e.article("alice", e.space, "", "", "Админ")
	if code, body := e.upload("alice", art.ID, "a.bin", rep('a', 900)); code != 201 {
		t.Fatalf("upload: %d %s", code, body)
	}
	if code, _ := e.upload("alice", art.ID, "b.bin", rep('b', 200)); code != 413 {
		t.Fatal("default limit not applied")
	}
	patch := func(user string, body string) (int, string) {
		code, raw := e.do(user, "PATCH", "/api/spaces/"+e.space, body)
		return code, strings.TrimSpace(string(raw))
	}
	// Managers and owners cannot change the quota, alone or with other fields.
	for _, u := range []string{"alice", "bob", "editor", "u1"} {
		for _, b := range []string{`{"storage_quota_bytes":0}`, `{"storage_quota_bytes":null}`, `{"name":"Имя","storage_quota_bytes":5000}`} {
			if code, raw := patch(u, b); code != 403 {
				t.Errorf("%s %s: %d %s", u, b, code, raw)
			}
		}
	}
	// The administrator check comes before the validation of the value.
	for _, u := range []string{"alice", "u1"} {
		if code, raw := patch(u, `{"storage_quota_bytes":-1}`); code != 403 {
			t.Errorf("%s invalid value: %d %s", u, code, raw)
		}
	}
	// Not through SQL either: the guard trigger refuses the runtime role.
	for _, u := range []string{"alice", "bob"} {
		_, err := e.as(u).DB.Exec(`UPDATE spaces SET storage_quota_bytes=? WHERE id=?`, int64(5000), e.space)
		var pg *pgconn.PgError
		if err == nil || !asPg(err, &pg) || pg.Code != "42501" {
			t.Errorf("%s changed the quota directly: %v", u, err)
		}
	}
	// A rename by the manager still works and leaves the quota alone.
	if code, raw := patch("alice", `{"name":"Новое имя"}`); code != 200 {
		t.Fatalf("rename: %d %s", code, raw)
	}
	for _, b := range []string{`{"storage_quota_bytes":-1}`, `{"storage_quota_bytes":"7"}`, `{"storage_quota_bytes":1.5}`, `{"storage_quota_bytes":true}`} {
		if code, raw := patch("admin", b); code != 400 {
			t.Errorf("admin %s: %d %s", b, code, raw)
		}
	}
	// The administrator needs no role in the space.
	if code, raw := patch("admin", `{"storage_quota_bytes":2000}`); code != 200 {
		t.Fatalf("admin set: %d %s", code, raw)
	}
	if code, body := e.upload("alice", art.ID, "b.bin", rep('b', 200)); code != 201 {
		t.Fatalf("after raising the limit: %d %s", code, body)
	}
	code, raw := e.storage("alice", e.space)
	var out spaceStorageOut
	decodeJSON(t, []byte(raw), &out)
	if code != 200 || out.Limit != 2000 || out.Override == nil || *out.Override != 2000 || out.Used != 1100 {
		t.Errorf("storage after override: %d %s", code, raw)
	}
	if code, raw := patch("admin", `{"storage_quota_bytes":0}`); code != 200 {
		t.Fatalf("unlimited: %d %s", code, raw)
	}
	if code, body := e.upload("alice", art.ID, "c.bin", rep('c', 5000)); code != 201 {
		t.Fatalf("unlimited upload: %d %s", code, body)
	}
	if code, raw := patch("admin", `{"storage_quota_bytes":null}`); code != 200 {
		t.Fatalf("back to default: %d %s", code, raw)
	}
	if code, _ := e.upload("alice", art.ID, "d.bin", "x"); code != 413 {
		t.Error("default not restored")
	}
	if code, raw := patch("admin", `{"storage_quota_bytes":0,"name":"x"}`); code != 404 {
		t.Errorf("admin without a role in the space combined with a rename: %d %s", code, raw)
	}
	if got := e.auditCount("space.storage_quota", e.space); got != 3 {
		t.Errorf("quota audit records: %d", got)
	}
	// A bad default in the environment is refused at startup.
	if _, err := OpenCorporate(context.Background(), CorporateConfig{SpaceQuotaBytes: -1, S3: S3Config{Bucket: "b"}, DatabaseURL: "x", IssuerURL: "x", ClientID: "x", ClientSecret: "x", RedirectURL: "x"}, t.TempDir(), ""); err == nil || !strings.Contains(err.Error(), "negative") {
		t.Errorf("negative default accepted: %v", err)
	}
}

// Two transactions race for the last bytes of a space: the second admission
// waits for the first to commit and then sees its insert.
func TestStorageQuotaLockOrdering(t *testing.T) {
	e := newQuotaEnv(t, 1000)
	art := e.article("alice", e.space, "", "", "Гонка")
	e.as("editor")
	tx1, err := e.as("alice").DB.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx1.Rollback()
	var ok bool
	if err = tx1.QueryRow(`SELECT space_storage_admit(?,?,?,?,?)`, e.space, "", int64(1000), []string{"a:one"}, []int64{600}).Scan(&ok); err != nil || !ok {
		t.Fatalf("first admission: %v %v", ok, err)
	}
	type result struct {
		ok  bool
		err error
		at  time.Time
	}
	second := make(chan result, 1)
	go func() {
		tx2, err := e.as("editor").DB.Begin()
		if err != nil {
			second <- result{err: err}
			return
		}
		defer tx2.Rollback()
		var ok bool
		err = tx2.QueryRow(`SELECT space_storage_admit(?,?,?,?,?)`, e.space, "", int64(1000), []string{"a:two"}, []int64{600}).Scan(&ok)
		second <- result{ok, err, time.Now()}
	}()
	select {
	case r := <-second:
		t.Fatalf("the second admission did not wait for the first transaction: %+v", r)
	case <-time.After(400 * time.Millisecond):
	}
	if _, err = tx1.Exec(`INSERT INTO attachments(id,article_id,name,mime,size,stored_name,created_at) VALUES(?,?,?,?,?,?,?)`, "att-one", art.ID, "one", "text/plain", int64(600), "one", stamp()); err != nil {
		t.Fatal(err)
	}
	committed := time.Now()
	if err = tx1.Commit(); err != nil {
		t.Fatal(err)
	}
	r := <-second
	if r.err != nil || r.ok || r.at.Before(committed) {
		t.Fatalf("second admission: %+v (committed %v)", r, committed)
	}
}

// Parallel uploads that together exceed the limit: exactly one passes.
func TestStorageQuotaConcurrentUploads(t *testing.T) {
	e := newQuotaEnv(t, 0)
	e.as("alice")
	e.as("editor")
	for round := 0; round < 6; round++ {
		other := corporateCreateSpace(t, e.as("alice"))
		e.grant("space", other, "user", "editor", "editor")
		e.setQuota(other, 1000)
		art2 := e.article("alice", other, "", "", fmt.Sprintf("Параллельно %d", round))
		start := make(chan struct{})
		var wg sync.WaitGroup
		codes := make([]int, 2)
		bodies := make([]string, 2)
		for i, user := range []string{"alice", "editor"} {
			wg.Add(1)
			go func(i int, user string) {
				defer wg.Done()
				<-start
				codes[i], bodies[i] = e.upload(user, art2.ID, fmt.Sprintf("p%d.bin", i), rep(byte('a'+i), 600))
			}(i, user)
		}
		close(start)
		wg.Wait()
		created, refused := 0, 0
		for i, c := range codes {
			switch c {
			case 201:
				created++
			case 413:
				if bodies[i] != quotaBody {
					t.Errorf("round %d: refusal body %q", round, bodies[i])
				}
				refused++
			}
		}
		if created != 1 || refused != 1 {
			t.Fatalf("round %d: codes %v", round, codes)
		}
		var n int
		if err := e.as("alice").DB.QueryRow(`SELECT count(*) FROM attachments at JOIN articles a ON a.id=at.article_id WHERE a.space_id=?`, other).Scan(&n); err != nil || n != 1 {
			t.Fatalf("round %d: %d attachments stored (%v)", round, n, err)
		}
		// the artifact path serializes the same way
		resp := make([]int, 2)
		start = make(chan struct{})
		for i, user := range []string{"alice", "editor"} {
			wg.Add(1)
			go func(i int, user string) {
				defer wg.Done()
				<-start
				body := map[string]any{"space_id": other, "title": "Гонка", "files": []fileSpec{{Path: "index.html", Content: rep(byte('A'+i), 300)}}}
				resp[i], _ = e.do(user, "POST", "/api/artifacts", body)
			}(i, user)
		}
		close(start)
		wg.Wait()
		// 600 attachment bytes are stored; of two 300-byte artifacts only one fits.
		if (resp[0] == 201) == (resp[1] == 201) {
			t.Fatalf("round %d: artifact codes %v", round, resp)
		}
	}
}

func TestStorageQuotaMovesMayOvershoot(t *testing.T) {
	e := newQuotaEnv(t, 0)
	a, b := e.twoSpaces()
	e.setQuota(b, 500)
	mv := func(req map[string]any) movesCall {
		pv := e.call("alice", "/api/moves/preview", req)
		if pv.Code != 200 {
			t.Fatalf("preview: %d %s", pv.Code, pv.Raw)
		}
		return e.call("alice", "/api/moves/apply", withFingerprint(req, pv.preview(t).Fingerprint))
	}
	x := e.article("alice", a, "", "", "X")
	y := e.article("alice", a, "", "", "Y")
	for _, art := range []Article{x, y} {
		if code, body := e.upload("alice", art.ID, "f.bin", rep(strings.ToLower(art.Title)[0], 400)); code != 201 {
			t.Fatalf("upload in the open space: %d %s", code, body)
		}
	}
	if r := mv(articleMove(x.ID, 1, b, "", "")); r.Code != 200 {
		t.Fatalf("move within the limit: %d %s", r.Code, r.Raw)
	}
	if code, body := e.upload("alice", x.ID, "more.bin", rep('z', 200)); code != 413 || body != quotaBody {
		t.Fatalf("upload beyond the limit after the move: %d %q", code, body)
	}
	// The second move pushes the space past its limit: it is not blocked.
	if r := mv(articleMove(y.ID, 1, b, "", "")); r.Code != 200 {
		t.Fatalf("move beyond the limit: %d %s", r.Code, r.Raw)
	}
	if got := e.usedBy("bob", b); got != 800 {
		t.Fatalf("used after the moves: %d", got)
	}
	// Over its limit the space accepts no new bytes whatever their size ...
	if code, body := e.upload("alice", x.ID, "tiny.bin", "t"); code != 413 || body != quotaBody {
		t.Errorf("upload into an overfull space: %d %q", code, body)
	}
	body := map[string]any{"space_id": b, "title": "Новый", "files": []fileSpec{{Path: "index.html", Content: "t"}}}
	if code, raw := e.do("alice", "POST", "/api/artifacts", body); code != 413 || strings.TrimSpace(string(raw)) != quotaBody {
		t.Errorf("artifact into an overfull space: %d %s", code, raw)
	}
	// ... while deletions stay possible.
	var list []Attachment
	code, raw := e.do("alice", "GET", "/api/articles/"+y.ID+"/attachments", nil)
	decodeJSON(t, raw, &list)
	if code != 200 || len(list) != 1 {
		t.Fatalf("attachments: %d %s", code, raw)
	}
	if e.status("alice", "DELETE", "/api/articles/"+y.ID+"/attachments/"+list[0].ID, nil) != 204 {
		t.Error("deleting an attachment in an overfull space")
	}
	cur, _ := e.get("alice", y.ID)
	if e.status("alice", "DELETE", "/api/articles/"+y.ID, map[string]any{"base_revision": cur.Revision}) != 200 {
		t.Error("deleting an article in an overfull space")
	}

	// An artifact moves with its project, too.
	proj := e.project(a, "", "С артефактом", true)
	d := e.artEnvArtifact(a, proj, "alice", rep('k', 100))
	r := mv(projectMove(proj, b, ""))
	if r.Code != 200 {
		t.Fatalf("project move: %d %s", r.Code, r.Raw)
	}
	if got := e.usedBy("bob", b); got != 900 {
		t.Errorf("used after the project move: %d", got)
	}
	// An artifact can still be deleted and its title changed there.
	path := "/api/artifacts/" + d.ID
	if code, raw := e.do("alice", "PUT", path, map[string]any{"base_revision": 1, "title": "Переименован"}); code != 200 {
		t.Errorf("title in an overfull space: %d %s", code, raw)
	}
	if code, raw := e.do("alice", "DELETE", path, map[string]any{"base_revision": 2}); code != 200 {
		t.Errorf("delete in an overfull space: %d %s", code, raw)
	}
}

func (e *artEnv) artEnvArtifact(space, project, user, content string) artifactDetail {
	e.t.Helper()
	body := map[string]any{"space_id": space, "project_id": project, "title": "В проекте", "files": []fileSpec{{Path: "index.html", Content: content}}}
	code, raw := e.do(user, "POST", "/api/artifacts", body)
	if code != 201 {
		e.t.Fatalf("create in project: %d %s", code, raw)
	}
	var d artifactDetail
	decodeJSON(e.t, raw, &d)
	return d
}

func TestStorageQuotaSchemaGuard(t *testing.T) {
	e := newQuotaEnv(t, 0)
	if err := e.root.DB.validateRuntimeSchema(); err != nil {
		t.Fatalf("fresh schema: %v", err)
	}
	const admit = `space_storage_admit(text,text,bigint,text[],bigint[])`
	probe := `current_user<>session_user AND current_setting('app.quota_probe',true)='1'`
	for _, c := range []struct{ break_, fix string }{
		{`GRANT EXECUTE ON FUNCTION ` + admit + ` TO PUBLIC`, `REVOKE ALL ON FUNCTION ` + admit + ` FROM PUBLIC`},
		{`GRANT EXECUTE ON FUNCTION space_storage_used(text) TO PUBLIC`, `REVOKE ALL ON FUNCTION space_storage_used(text) FROM PUBLIC`},
		{`GRANT EXECUTE ON FUNCTION space_storage_objects(text) TO PUBLIC`, `REVOKE ALL ON FUNCTION space_storage_objects(text) FROM PUBLIC`},
		{`GRANT EXECUTE ON FUNCTION space_set_storage_quota(text,bigint) TO PUBLIC`, `REVOKE ALL ON FUNCTION space_set_storage_quota(text,bigint) FROM PUBLIC`},
		{`ALTER FUNCTION ` + admit + ` SECURITY INVOKER`, `ALTER FUNCTION ` + admit + ` SECURITY DEFINER`},
		{`ALTER FUNCTION space_storage_used(text) RESET search_path`, `ALTER FUNCTION space_storage_used(text) SET search_path=pg_catalog,public,pg_temp`},
		{`ALTER FUNCTION space_storage_used(text) SET search_path=pg_catalog,public`, `ALTER FUNCTION space_storage_used(text) SET search_path=pg_catalog,public,pg_temp`},
		{`ALTER FUNCTION space_set_storage_quota(text,bigint) SET search_path=public`, `ALTER FUNCTION space_set_storage_quota(text,bigint) SET search_path=pg_catalog,public,pg_temp`},
		{`ALTER FUNCTION storage_quota_guard() SET search_path=pg_catalog,public`, `ALTER FUNCTION storage_quota_guard() SET search_path=pg_catalog,public,pg_temp`},
		{`ALTER FUNCTION space_storage_objects(text) RESET search_path`, `ALTER FUNCTION space_storage_objects(text) SET search_path=pg_catalog,public,pg_temp`},
		{`ALTER POLICY storage_quota_probe_spaces_update ON spaces WITH CHECK (true)`, `ALTER POLICY storage_quota_probe_spaces_update ON spaces WITH CHECK (` + probe + `)`},
		{`REVOKE EXECUTE ON FUNCTION space_storage_override(text) FROM aiwiki_app`, `GRANT EXECUTE ON FUNCTION space_storage_override(text) TO aiwiki_app`},
		{`ALTER TABLE spaces DISABLE TRIGGER spaces_storage_quota_guard`, `ALTER TABLE spaces ENABLE TRIGGER spaces_storage_quota_guard`},
		{`ALTER POLICY storage_quota_probe_articles ON articles USING (true)`, `ALTER POLICY storage_quota_probe_articles ON articles USING (` + probe + `)`},
		{`ALTER POLICY storage_quota_probe_spaces_update ON spaces USING (true)`, `ALTER POLICY storage_quota_probe_spaces_update ON spaces USING (` + probe + `)`},
		{`DROP POLICY storage_quota_probe_attachments ON attachments`, `CREATE POLICY storage_quota_probe_attachments ON attachments FOR SELECT USING (` + probe + `)`},
	} {
		e.exec(c.break_)
		if err := e.root.DB.validateRuntimeSchema(); err == nil {
			t.Errorf("guard missed: %s", c.break_)
		}
		e.exec(c.fix)
		if err := e.root.DB.validateRuntimeSchema(); err != nil {
			t.Fatalf("after restoring %q: %v", c.break_, err)
		}
	}
	// A body that lost the lock or the probe is caught too.
	var def string
	if err := e.owner.DB.QueryRow(`SELECT pg_get_functiondef('public.` + admit + `'::regprocedure)`).Scan(&def); err != nil {
		t.Fatal(err)
	}
	e.exec(strings.Replace(def, "pg_advisory_xact_lock", "pg_try_advisory_xact_lock", 1))
	if err := e.root.DB.validateRuntimeSchema(); err == nil {
		t.Error("guard missed an admission without the advisory lock")
	}
	e.exec(def)
	if err := e.root.DB.validateRuntimeSchema(); err != nil {
		t.Fatalf("after restoring the admission function: %v", err)
	}
	// The migration is recorded once and can be re-run.
	var versions int
	if err := e.owner.DB.QueryRow(`SELECT count(*) FROM schema_migrations WHERE component='storage_quotas' AND version=1`).Scan(&versions); err != nil || versions != 1 {
		t.Errorf("schema_migrations: %d %v", versions, err)
	}
	if err := e.owner.migrateStorageQuotas(context.Background()); err != nil {
		t.Errorf("second run: %v", err)
	}
}

// A temporary table named like a catalog table must not change what the
// guard trigger or the definer functions see.
func TestStorageQuotaIgnoresTempShadowing(t *testing.T) {
	e := newQuotaEnv(t, 0)
	alice := e.as("alice").DB
	for _, q := range []string{
		`CREATE TEMP TABLE pg_class (oid oid, relowner oid)`,
		`INSERT INTO pg_temp.pg_class SELECT 'public.spaces'::regclass::oid, (SELECT oid FROM pg_catalog.pg_roles WHERE rolname=current_user)`,
	} {
		if _, err := alice.Exec(q); err != nil {
			t.Skipf("temporary tables unavailable to the runtime role: %v", err)
		}
	}
	_, err := alice.Exec(`UPDATE spaces SET storage_quota_bytes=77 WHERE id=?`, e.space)
	var pg *pgconn.PgError
	if err == nil || !asPg(err, &pg) || pg.Code != "42501" {
		t.Fatalf("a shadowing pg_class let the runtime role change the quota: %v", err)
	}
}
