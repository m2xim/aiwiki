package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Import tests that need PostgreSQL run only with the three roles of a
// disposable database (the public schema is reset):
//
//	AIWIKI_TEST_POSTGRES_OWNER   migration (owner) role
//	AIWIKI_TEST_POSTGRES         runtime role (aiwiki_app)
//	AIWIKI_TEST_POSTGRES_IMPORT  BYPASSRLS role, member of the owner role
//
// The tests of the source analysis and of the refusals need no database.

func fileSHA(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// treeHash hashes every file under dir (names and bytes).
func treeHash(t *testing.T, dir string) string {
	t.Helper()
	h := sha256.New()
	err := filepath.Walk(dir, func(p string, fi os.FileInfo, err error) error {
		if err != nil || fi.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		b, err := os.ReadFile(p)
		h.Write([]byte(rel + "\x00"))
		h.Write(b)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func fixtureUpdate(t *testing.T, s *Server, id, msg string, mutate func(*Article)) {
	t.Helper()
	a, err := s.articleByID(s.DB, id)
	if err != nil {
		t.Fatal(err)
	}
	mutate(&a)
	a.Revision++
	a.UpdatedAt = stamp()
	tx, err := s.DB.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`UPDATE articles SET title=?,summary=?,body=?,tags=?,properties=?,revision=?,updated_at=?,author=? WHERE id=?`, a.Title, a.Summary, a.Body, encode(a.Tags), encode(a.Properties), a.Revision, a.UpdatedAt, a.Author, a.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(`INSERT INTO revisions(article_id,revision,author,message,created_at,snapshot) VALUES(?,?,?,?,?,?)`, a.ID, a.Revision, a.Author, msg, a.UpdatedAt, encode(a)); err != nil {
		t.Fatal(err)
	}
	if err = indexArticle(tx, a); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

// buildCurrentFixture creates a data directory with the current trusted code.
func buildCurrentFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	s, err := Open(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := s.DB.Exec(q, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO spaces(id,name,description,archived_at) VALUES('old-space','Старый раздел','закрыт','2026-03-04T05:06:07Z')`)
	mk := func(id, space, parent, title, body string, tags []string) {
		a := Article{ID: id, SpaceID: space, ParentID: parent, Title: title, Summary: "Кратко " + title, Body: body, Kind: "research", Status: "active", Pinned: id == "child", Tags: tags, Properties: map[string]string{"k": "v", "ключ": "значение"}, Revision: 1, CreatedAt: "2026-01-02T03:04:05Z", UpdatedAt: "2026-01-02T03:04:05Z", Author: "claude-agent"}
		tx, err := s.DB.Begin()
		if err != nil {
			t.Fatal(err)
		}
		if err = insertArticle(tx, a, "Первая версия"); err != nil {
			t.Fatal(err)
		}
		if err = tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	mk("zz-parent", "general", "", "Родитель", "# Родитель\n\nтекст с «кавычками» и \\u00e9 и 'апострофом'", []string{"тег", "tag"})
	mk("aa-child", "general", "zz-parent", "Ребёнок", "дочерняя статья", nil)
	mk("child", "engineering", "", "Pinned", "pinned body", []string{"x"})
	mk("old-article", "old-space", "", "В архиве", "архивная статья", []string{"архив"})
	mk("gone", "general", "", "Удалена", "будет удалена", nil)
	fixtureUpdate(t, s, "zz-parent", "Правка 2", func(a *Article) { a.Body += "\n\nвторая правка"; a.Author = "maxim" })
	fixtureUpdate(t, s, "zz-parent", "Правка 3", func(a *Article) {
		a.Title = "Родитель 3"
		a.Tags = []string{"тег"}
		a.Author = "claude-agent"
	})
	fixtureUpdate(t, s, "child", "Правка", func(a *Article) { a.Summary = "новое" })
	// Delete an article the way the server does: soft delete, no search row.
	exec(`UPDATE articles SET deleted_at='2026-05-05T05:05:05Z' WHERE id='gone'`)
	exec(`DELETE FROM article_fts WHERE article_id='gone'`)
	exec(`INSERT INTO comments(id,article_id,anchor,body,author,created_at,resolved) VALUES('c1','zz-parent','h2','Комментарий','maxim','2026-02-02T02:02:02Z',0),('c2','aa-child','','Решён','agent','2026-02-03T02:02:02Z',1)`)
	exec(`INSERT INTO favorites(article_id,created_at) VALUES('zz-parent','2026-06-06T06:06:06Z')`)
	// One attachment with bytes that are not valid UTF-8, one deleted one.
	payload := append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte{0, 1, 2, 250, 251}, 5000)...)
	if err = os.WriteFile(filepath.Join(dir, "files", "att-1"), payload, 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, "files", "att-2"), []byte("deleted attachment"), 0600); err != nil {
		t.Fatal(err)
	}
	exec(`INSERT INTO attachments(id,article_id,name,mime,size,stored_name,created_at,deleted_at) VALUES('att-1','zz-parent','схема.png','image/png',?,'att-1','2026-02-04T02:02:02Z',NULL),('att-2','zz-parent','old.txt','text/plain',18,'att-2','2026-02-05T02:02:02Z','2026-02-06T02:02:02Z')`, len(payload))
	// One artifact with a blob and a public link.
	page := []byte("<!doctype html><title>A</title><p>hello")
	sum := sha256.Sum256(page)
	sha := hex.EncodeToString(sum[:])
	if err = os.WriteFile(filepath.Join(dir, "files", "art-"+sha), page, 0600); err != nil {
		t.Fatal(err)
	}
	manifest := encode([]artifactFile{{Path: "index.html", Size: int64(len(page)), SHA256: sha, MIME: "text/html"}})
	exec(`INSERT INTO artifacts(id,space_id,title,description,visibility,owner_id,revision,author,created_at,updated_at,file_count,projection,search_text) VALUES('art1','general','Страница','','private','',1,'maxim','2026-07-07T07:07:07Z','2026-07-07T07:07:07Z',1,'{}','hello')`)
	exec(`INSERT INTO artifact_revisions(artifact_id,revision,author,message,created_at,manifest,file_count) VALUES('art1',1,'maxim','','2026-07-07T07:07:07Z',?,1)`, manifest)
	exec(`INSERT INTO artifact_blobs(sha256,size,mime,stored_name,created_at) VALUES(?,?,'text/html',?,'2026-07-07T07:07:07Z')`, sha, len(page), "art-"+sha)
	exec(`INSERT INTO artifact_shares(id,artifact_id,token_hash,revision,created_by,created_at) VALUES('sh1','art1',?,NULL,'','2026-07-08T07:07:07Z')`, hashShareToken("token"))
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	return dir
}

// legacySchema is the real home installation's schema: no project_id, no
// artifact tables, FTS5 search table.
const legacySchema = `CREATE TABLE spaces(id TEXT PRIMARY KEY,name TEXT NOT NULL,description TEXT NOT NULL DEFAULT '',archived_at TEXT);
CREATE TABLE articles(id TEXT PRIMARY KEY,space_id TEXT NOT NULL REFERENCES spaces(id),parent_id TEXT NOT NULL DEFAULT '',title TEXT NOT NULL,summary TEXT NOT NULL DEFAULT '',body TEXT NOT NULL DEFAULT '',kind TEXT NOT NULL,status TEXT NOT NULL,pinned INTEGER NOT NULL DEFAULT 0,tags TEXT NOT NULL DEFAULT '[]',properties TEXT NOT NULL DEFAULT '{}',revision INTEGER NOT NULL,created_at TEXT NOT NULL,updated_at TEXT NOT NULL,author TEXT NOT NULL,deleted_at TEXT);
CREATE TABLE revisions(id INTEGER PRIMARY KEY AUTOINCREMENT,article_id TEXT NOT NULL REFERENCES articles(id),revision INTEGER NOT NULL,author TEXT NOT NULL,message TEXT NOT NULL,created_at TEXT NOT NULL,snapshot TEXT NOT NULL,UNIQUE(article_id,revision));
CREATE TABLE comments(id TEXT PRIMARY KEY,article_id TEXT NOT NULL REFERENCES articles(id),anchor TEXT NOT NULL,body TEXT NOT NULL,author TEXT NOT NULL,created_at TEXT NOT NULL,resolved INTEGER NOT NULL DEFAULT 0);
CREATE TABLE attachments(id TEXT PRIMARY KEY,article_id TEXT NOT NULL REFERENCES articles(id),name TEXT NOT NULL,mime TEXT NOT NULL,size INTEGER NOT NULL,stored_name TEXT NOT NULL,created_at TEXT NOT NULL,deleted_at TEXT);
CREATE TABLE favorites(article_id TEXT PRIMARY KEY REFERENCES articles(id),created_at TEXT NOT NULL);
CREATE VIRTUAL TABLE article_fts USING fts5(article_id UNINDEXED,title,summary,body,tags, tokenize='unicode61');`

// buildLegacyFixture writes the old schema with sqlite directly (the current
// code cannot create it) and leaves the database in WAL mode with the journal
// not yet checkpointed, like a copied live installation.
func buildLegacyFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "files"), 0755); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(dir, "wiki.db"))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.Exec(q, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(legacySchema)
	exec(`INSERT INTO spaces(id,name,description,archived_at) VALUES('general','Общее','д',NULL),('closed','Закрытый','',  '2026-01-01T00:00:00Z')`)
	snap := func(id, space, parent, title string, rev int, deleted string) string {
		a := map[string]any{"id": id, "space_id": space, "parent_id": parent, "title": title, "summary": "", "body": "тело " + title, "kind": "article", "status": "active", "pinned": false, "tags": []string{"t"}, "properties": map[string]string{}, "revision": rev, "created_at": "2025-01-01T00:00:00Z", "updated_at": "2025-01-0" + string(rune('0'+rev)) + "T00:00:00Z", "author": "maxim"}
		if deleted != "" {
			a["deleted_at"] = deleted
		}
		b, _ := json.Marshal(a)
		return string(b)
	}
	// Child sorts before its parent: the import must insert the parent first.
	exec(`INSERT INTO articles(id,space_id,parent_id,title,summary,body,kind,status,pinned,tags,properties,revision,created_at,updated_at,author,deleted_at) VALUES
('a-child','general','z-parent','Дитя','','тело Дитя','article','active',0,'["t"]','{}',1,'2025-01-01T00:00:00Z','2025-01-01T00:00:00Z','maxim',NULL),
('z-parent','general','','Родитель','','тело Родитель','article','active',1,'["t"]','{}',2,'2025-01-01T00:00:00Z','2025-01-02T00:00:00Z','maxim',NULL),
('dead','closed','','Мёртвая','','тело Мёртвая','article','draft',0,'[]','{}',1,'2025-01-01T00:00:00Z','2025-01-01T00:00:00Z','maxim','2025-02-02T00:00:00Z')`)
	exec(`INSERT INTO revisions(id,article_id,revision,author,message,created_at,snapshot) VALUES
(10,'z-parent',1,'maxim','создано','2025-01-01T00:00:00Z',?),
(11,'a-child',1,'maxim','создано','2025-01-01T00:00:00Z',?),
(25,'z-parent',2,'maxim','правка','2025-01-02T00:00:00Z',?),
(30,'dead',1,'maxim','создано','2025-01-01T00:00:00Z',?)`,
		snap("z-parent", "general", "", "Родитель", 1, ""), snap("a-child", "general", "z-parent", "Дитя", 1, ""), snap("z-parent", "general", "", "Родитель", 2, ""), snap("dead", "closed", "", "Мёртвая", 1, ""))
	exec(`INSERT INTO comments(id,article_id,anchor,body,author,created_at,resolved) VALUES('k1','a-child','','комментарий','maxim','2025-03-03T00:00:00Z',0)`)
	if err = os.WriteFile(filepath.Join(dir, "files", "legacy.bin"), []byte("legacy bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	exec(`INSERT INTO attachments(id,article_id,name,mime,size,stored_name,created_at) VALUES('f1','z-parent','legacy.bin','application/octet-stream',12,'legacy.bin','2025-04-04T00:00:00Z')`)
	exec(`INSERT INTO favorites(article_id,created_at) VALUES('a-child','2025-05-05T00:00:00Z')`)
	exec(`INSERT INTO article_fts(article_id,title,summary,body,tags) VALUES('z-parent','Родитель','','тело Родитель','t'),('a-child','Дитя','','тело Дитя','t')`)
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	return dir
}

type importFixture struct {
	owner, app, imp string
	cfg             SQLiteImportConfig
	fake            *fakeS3
	out             *bytes.Buffer
}

// newImportFixture resets the disposable database and starts a fake bucket.
func newImportFixture(t *testing.T, from string) *importFixture {
	t.Helper()
	f := &importFixture{owner: os.Getenv("AIWIKI_TEST_POSTGRES_OWNER"), app: os.Getenv("AIWIKI_TEST_POSTGRES"), imp: os.Getenv("AIWIKI_TEST_POSTGRES_IMPORT")}
	if f.owner == "" || f.app == "" || f.imp == "" {
		t.Skip("set AIWIKI_TEST_POSTGRES_OWNER, AIWIKI_TEST_POSTGRES and AIWIKI_TEST_POSTGRES_IMPORT to a disposable database")
	}
	owner, err := openPostgres(context.Background(), f.owner)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	if _, err = owner.Exec(`DROP SCHEMA public CASCADE; CREATE SCHEMA public`); err != nil {
		t.Fatal(err)
	}
	s3Env(t)
	fake, srv := newFakeS3(t)
	f.fake = fake
	f.out = &bytes.Buffer{}
	f.cfg = SQLiteImportConfig{
		From:        from,
		S3:          S3Config{Bucket: "wiki", Region: "us-east-1", Endpoint: srv.URL, PathStyle: true, AllowInsecureLoopback: true},
		DatabaseURL: f.app, MigrationDatabaseURL: f.owner, RuntimeRole: "aiwiki_app", ImportDatabaseURL: f.imp,
		OwnerName: "Тест Владелец", Out: f.out,
	}
	return f
}

// run imports and returns the report and the error.
func (f *importFixture) run(t *testing.T) (*SQLiteImportReport, error) {
	t.Helper()
	f.out.Reset()
	return ImportSQLite(context.Background(), f.cfg)
}

// count reads one number as the runtime role scoped as the owner.
func (f *importFixture) count(t *testing.T, query string, args ...any) int {
	t.Helper()
	rt, err := openPostgres(context.Background(), f.app)
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	db, done, err := rt.scoped(context.Background(), "owner")
	if err != nil {
		t.Fatal(err)
	}
	defer done()
	var n int
	if err = db.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return n
}

// rawCount counts as the import role (sees everything); -1 if the table is absent.
func (f *importFixture) rawCount(t *testing.T, table string) int {
	t.Helper()
	pool, err := openImportPostgres(context.Background(), f.imp)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	var n int
	if err = pool.QueryRow(`SELECT count(*) FROM ` + table).Scan(&n); err != nil {
		return -1
	}
	return n
}

func mustRefuse(t *testing.T, err error, contains string) {
	t.Helper()
	var r *ImportRefusal
	if !errors.As(err, &r) || !strings.Contains(r.Msg, contains) {
		t.Fatalf("expected a refusal containing %q, got %v", contains, err)
	}
}

func TestImportSQLiteCurrentSchema(t *testing.T) {
	dir := buildCurrentFixture(t)
	f := newImportFixture(t, dir)
	before := treeHash(t, dir)
	rep, err := f.run(t)
	if err != nil {
		t.Fatalf("import: %v\n%s", err, f.out)
	}
	if rep.Failed() {
		t.Fatalf("verification failed:\n%s", f.out)
	}
	if after := treeHash(t, dir); after != before {
		t.Fatal("the source data directory changed")
	}
	// 3 seeded spaces + old-space; 5 seeded + 5 articles.
	if n := f.count(t, `SELECT count(*) FROM spaces`); n != 4 {
		t.Fatalf("spaces %d", n)
	}
	if n := f.count(t, `SELECT count(*) FROM articles`); n != 10 {
		t.Fatalf("articles %d", n)
	}
	if n := f.count(t, `SELECT count(*) FROM articles WHERE deleted_at IS NOT NULL`); n != 1 {
		t.Fatalf("deleted %d", n)
	}
	if n := f.count(t, `SELECT count(*) FROM revisions WHERE article_id='zz-parent'`); n != 3 {
		t.Fatalf("revisions of zz-parent: %d", n)
	}
	if n := f.count(t, `SELECT count(*) FROM article_fts`); n != 9 {
		t.Fatalf("fts %d", n)
	}
	if n := f.count(t, `SELECT count(*) FROM comments`) + f.count(t, `SELECT count(*) FROM attachments`)*10 + f.count(t, `SELECT count(*) FROM favorites`)*100; n != 2+20+100 {
		t.Fatalf("comments/attachments/favorites %d", n)
	}
	if n := f.count(t, `SELECT count(*) FROM artifacts WHERE visibility='private' AND owner_id='owner'`); n != 1 {
		t.Fatalf("artifact %d", n)
	}
	if n := f.rawCount(t, "artifact_blobs"); n != 1 {
		t.Fatalf("blobs %d", n)
	}
	if n := f.rawCount(t, "(SELECT 1 FROM corporate_space_state WHERE space_id='old-space' AND archived) x"); n != 1 {
		t.Fatalf("archive state of old-space: %d", n)
	}
	if got := len(f.fake.objects); got != 3 { // att-1, att-2, art-<sha>
		t.Fatalf("%d objects in the bucket", got)
	}
	if !bytes.Equal(f.fake.objects["wiki/att-1"], mustRead(t, filepath.Join(dir, "files", "att-1"))) {
		t.Fatal("att-1 bytes differ")
	}
	// The sequence is above every imported id: a new revision gets a fresh id.
	rt, err := openPostgres(context.Background(), f.app)
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	db, done, err := rt.scoped(context.Background(), "owner")
	if err != nil {
		t.Fatal(err)
	}
	defer done()
	var maxBefore, id int64
	if err = db.QueryRow(`SELECT max(id) FROM revisions`).Scan(&maxBefore); err != nil {
		t.Fatal(err)
	}
	if err = db.QueryRow(`INSERT INTO revisions(article_id,revision,author,message,created_at,snapshot) VALUES('zz-parent',4,'t','m','x',(SELECT snapshot FROM revisions WHERE article_id='zz-parent' AND revision=3)) RETURNING id`).Scan(&id); err != nil || id <= maxBefore {
		t.Fatalf("new revision id %d after max %d: %v", id, maxBefore, err)
	}
	// A second import into the same database is refused and writes nothing.
	f.fake.objects = map[string][]byte{}
	_, err = f.run(t)
	mustRefuse(t, err, "already contains")
	if len(f.fake.objects) != 0 {
		t.Fatal("a refused import uploaded objects")
	}
	// Output never carries secrets.
	for _, secret := range []string{"owner-ci-only", "app-ci-only", "import-ci-only", "secret"} {
		if strings.Contains(f.out.String(), secret) {
			t.Fatalf("output contains %q", secret)
		}
	}
}

func mustRead(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestImportSQLiteLegacySchema(t *testing.T) {
	dir := buildLegacyFixture(t)
	f := newImportFixture(t, dir)
	before := treeHash(t, dir)
	rep, err := f.run(t)
	if err != nil {
		t.Fatalf("import: %v\n%s", err, f.out)
	}
	if rep.Failed() {
		t.Fatalf("verification failed:\n%s", f.out)
	}
	if treeHash(t, dir) != before {
		t.Fatal("the source data directory changed")
	}
	for q, want := range map[string]int{
		`SELECT count(*) FROM spaces`:                                               2,
		`SELECT count(*) FROM articles`:                                             3,
		`SELECT count(*) FROM revisions`:                                            4,
		`SELECT count(*) FROM revisions WHERE id IN (10,11,25,30)`:                  4,
		`SELECT count(*) FROM articles WHERE id='a-child' AND parent_id='z-parent'`: 1,
		`SELECT count(*) FROM articles WHERE deleted_at IS NOT NULL`:                1,
		`SELECT count(*) FROM article_fts`:                                          2,
		`SELECT count(*) FROM comments`:                                             1,
		`SELECT count(*) FROM attachments`:                                          1,
		`SELECT count(*) FROM favorites`:                                            1,
		`SELECT count(*) FROM artifacts`:                                            0,
	} {
		if n := f.count(t, q); n != want {
			t.Errorf("%s = %d, want %d", q, n, want)
		}
	}
	if string(f.fake.objects["wiki/legacy.bin"]) != "legacy bytes" || len(f.fake.objects) != 1 {
		t.Fatalf("bucket: %v", f.fake.objects)
	}
	// The next revision id continues after the largest imported id (30).
	if n := f.rawCount(t, "(SELECT last_value FROM revisions_id_seq WHERE last_value=30) x"); n != 1 {
		t.Fatalf("sequence not at 30")
	}
	for _, line := range []string{"ok   articles", "ok   revisions", "ok   bucket objects"} {
		if !strings.Contains(f.out.String(), line) {
			t.Errorf("report lacks %q:\n%s", line, f.out)
		}
	}
}

func TestImportSQLiteDryRunWritesNothing(t *testing.T) {
	for name, build := range map[string]func(*testing.T) string{"current": buildCurrentFixture, "legacy": buildLegacyFixture} {
		t.Run(name, func(t *testing.T) {
			dir := build(t)
			f := newImportFixture(t, dir)
			f.cfg.DryRun = true
			before := treeHash(t, dir)
			rep, err := f.run(t)
			if err != nil || rep.Failed() {
				t.Fatalf("dry run: %v failed=%v\n%s", err, rep.Failed(), f.out)
			}
			if !strings.Contains(f.out.String(), "target was left unchanged") {
				t.Fatalf("output:\n%s", f.out)
			}
			for _, tbl := range []string{"spaces", "articles", "revisions", "comments", "attachments", "favorites", "article_fts", "memberships", "users", "artifacts"} {
				if n := f.rawCount(t, tbl); n != 0 {
					t.Errorf("%s has %d rows after a dry run", tbl, n)
				}
			}
			if len(f.fake.objects) != 0 {
				t.Fatal("a dry run uploaded objects")
			}
			if treeHash(t, dir) != before {
				t.Fatal("the source changed")
			}
			// The sequence was not advanced either.
			if n := f.rawCount(t, "(SELECT 1 FROM revisions_id_seq WHERE NOT is_called) x"); n != 1 {
				t.Fatal("revision sequence advanced by a dry run")
			}
			// And the real import still works afterwards.
			f.cfg.DryRun = false
			if rep, err = f.run(t); err != nil || rep.Failed() {
				t.Fatalf("import after dry run: %v\n%s", err, f.out)
			}
		})
	}
}

func TestImportSQLiteUploadFailureAbortsTransaction(t *testing.T) {
	dir := buildLegacyFixture(t)
	f := newImportFixture(t, dir)
	f.fake.failPut = true
	if _, err := f.run(t); err == nil {
		t.Fatal("expected an upload failure")
	} else if errors.As(err, new(*ImportRefusal)) {
		t.Fatalf("an upload failure is not a refusal: %v", err)
	}
	for _, tbl := range []string{"spaces", "articles", "revisions", "users"} {
		if n := f.rawCount(t, tbl); n != 0 {
			t.Errorf("%s has %d rows after an aborted import", tbl, n)
		}
	}
	f.fake.failPut = false
	if rep, err := f.run(t); err != nil || rep.Failed() {
		t.Fatalf("retry: %v\n%s", err, f.out)
	}
}

func TestImportSQLiteRefusesTargetWithContent(t *testing.T) {
	dir := buildLegacyFixture(t)
	f := newImportFixture(t, dir)
	ctx := context.Background()
	prepare := func(t *testing.T, stmts ...string) {
		t.Helper()
		f = newImportFixture(t, dir)
		if err := migrateAndGrant(ctx, f.owner, "aiwiki_app", "", ""); err != nil {
			t.Fatal(err)
		}
		pool, err := openImportPostgres(ctx, f.imp)
		if err != nil {
			t.Fatal(err)
		}
		defer pool.Close()
		for _, s := range stmts {
			if _, err = pool.Exec(s); err != nil {
				t.Fatalf("%s: %v", s, err)
			}
		}
	}
	t.Run("already imported", func(t *testing.T) {
		f = newImportFixture(t, dir)
		if rep, err := f.run(t); err != nil || rep.Failed() {
			t.Fatalf("first import: %v", err)
		}
		f.fake.objects = map[string][]byte{}
		_, err := f.run(t)
		mustRefuse(t, err, "already contains")
		if len(f.fake.objects) != 0 {
			t.Fatal("a refused import uploaded objects")
		}
	})
	t.Run("pre-existing space", func(t *testing.T) {
		prepare(t, `INSERT INTO spaces(id,name) VALUES('general','x')`)
		_, err := f.run(t)
		mustRefuse(t, err, "spaces rows")
		if n := f.rawCount(t, "articles"); n != 0 {
			t.Fatalf("articles %d", n)
		}
	})
	t.Run("pre-existing setting", func(t *testing.T) {
		prepare(t, `INSERT INTO app_settings(key,value) VALUES('public_artifact_links','false')`)
		_, err := f.run(t)
		mustRefuse(t, err, "app_settings rows")
	})
	t.Run("membership of someone else", func(t *testing.T) {
		prepare(t, `INSERT INTO memberships(resource_type,resource_id,subject_type,subject_id,role) VALUES('space','x','user','bob','reader')`)
		_, err := f.run(t)
		mustRefuse(t, err, "memberships")
	})
	t.Run("another identity named owner", func(t *testing.T) {
		prepare(t, `INSERT INTO users(id,issuer,subject,name,admin) VALUES('owner','https://idp.example.org','owner','x',0)`)
		_, err := f.run(t)
		mustRefuse(t, err, "not the personal-mode owner")
	})
	t.Run("personal server started before", func(t *testing.T) {
		prepare(t, `INSERT INTO users(id,issuer,subject,name,admin,disabled) VALUES('owner','aiwiki:personal','owner','Старое имя',1,0)`)
		rep, err := f.run(t)
		if err != nil || rep.Failed() {
			t.Fatalf("import into a pre-started personal database: %v\n%s", err, f.out)
		}
		if n := f.count(t, `SELECT count(*) FROM users WHERE name='Тест Владелец'`); n != 1 {
			t.Fatalf("owner name not updated: %d", n)
		}
	})
}

func TestImportVerifyOnly(t *testing.T) {
	dir := buildLegacyFixture(t)
	f := newImportFixture(t, dir)
	f.cfg.VerifyOnly = true
	rep, err := f.run(t) // nothing imported yet: the verification must fail
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Failed() {
		t.Fatalf("verify-only passed on an empty target:\n%s", f.out)
	}
	f.cfg.VerifyOnly = false
	if rep, err = f.run(t); err != nil || rep.Failed() {
		t.Fatalf("import: %v\n%s", err, f.out)
	}
	f.cfg.VerifyOnly = true
	if rep, err = f.run(t); err != nil || rep.Failed() {
		t.Fatalf("verify-only: %v\n%s", err, f.out)
	}
}

func TestImportAbortDeletesUploadedObjects(t *testing.T) {
	ms := newMemStore()
	ctx := context.Background()
	for _, k := range []string{"a", "b"} {
		if err := ms.Put(ctx, k, strings.NewReader("x"), 1, ""); err != nil {
			t.Fatal(err)
		}
	}
	var out bytes.Buffer
	run := &importRun{out: &out, rep: &SQLiteImportReport{Orphans: []string{"a", "b"}}}
	if _, err := run.abort(ms, errors.New("boom")); err == nil || err.Error() != "boom" {
		t.Fatal(err)
	}
	if len(run.rep.Orphans) != 0 {
		t.Fatalf("left %v", run.rep.Orphans)
	}
	if _, err := ms.Get(ctx, "a"); err == nil {
		t.Fatal("object a was not deleted")
	}
}

func TestImportRefusesHotJournal(t *testing.T) {
	dir := buildLegacyFixture(t)
	if err := os.WriteFile(filepath.Join(dir, "wiki.db-journal"), []byte("pending"), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := loadImportSource(context.Background(), dir, t.TempDir())
	mustRefuse(t, err, "journal")
}

// ---- no database needed ----

func TestImportSourceRefusals(t *testing.T) {
	ctx := context.Background()
	t.Run("no wiki.db", func(t *testing.T) {
		_, err := loadImportSource(ctx, t.TempDir(), t.TempDir())
		mustRefuse(t, err, "no wiki.db")
	})
	t.Run("not sqlite", func(t *testing.T) {
		dir := t.TempDir()
		_ = os.WriteFile(filepath.Join(dir, "wiki.db"), []byte("this is not a database file at all"), 0600)
		_, err := loadImportSource(ctx, dir, t.TempDir())
		mustRefuse(t, err, "not a SQLite database")
	})
	t.Run("foreign schema", func(t *testing.T) {
		dir := t.TempDir()
		db, _ := sql.Open("sqlite", filepath.Join(dir, "wiki.db"))
		if _, err := db.Exec(`CREATE TABLE notes(id INTEGER PRIMARY KEY, text TEXT)`); err != nil {
			t.Fatal(err)
		}
		db.Close()
		_, err := loadImportSource(ctx, dir, t.TempDir())
		mustRefuse(t, err, `no "spaces" table`)
	})
	t.Run("unreadable columns", func(t *testing.T) {
		dir := t.TempDir()
		db, _ := sql.Open("sqlite", filepath.Join(dir, "wiki.db"))
		if _, err := db.Exec(`CREATE TABLE spaces(id TEXT PRIMARY KEY); CREATE TABLE articles(id TEXT PRIMARY KEY, title TEXT); CREATE TABLE revisions(id INTEGER PRIMARY KEY)`); err != nil {
			t.Fatal(err)
		}
		db.Close()
		_, err := loadImportSource(ctx, dir, t.TempDir())
		var r *ImportRefusal
		if !errors.As(err, &r) {
			t.Fatalf("expected a refusal, got %v", err)
		}
	})
	t.Run("missing attachment file", func(t *testing.T) {
		dir := buildLegacyFixture(t)
		if err := os.Remove(filepath.Join(dir, "files", "legacy.bin")); err != nil {
			t.Fatal(err)
		}
		_, err := loadImportSource(ctx, dir, t.TempDir())
		mustRefuse(t, err, "file legacy.bin is missing")
	})
	t.Run("wrong size", func(t *testing.T) {
		dir := buildLegacyFixture(t)
		_ = os.WriteFile(filepath.Join(dir, "files", "legacy.bin"), []byte("short"), 0600)
		_, err := loadImportSource(ctx, dir, t.TempDir())
		mustRefuse(t, err, "has 5 bytes")
	})
	t.Run("corrupt artifact blob", func(t *testing.T) {
		dir := buildCurrentFixture(t)
		entries, _ := filepath.Glob(filepath.Join(dir, "files", "art-*"))
		if len(entries) != 1 {
			t.Fatal(entries)
		}
		b := mustRead(t, entries[0])
		b[0] ^= 1
		_ = os.WriteFile(entries[0], b, 0600)
		_, err := loadImportSource(ctx, dir, t.TempDir())
		mustRefuse(t, err, "SHA-256")
	})
	t.Run("parent cycle", func(t *testing.T) {
		dir := buildLegacyFixture(t)
		db, _ := sql.Open("sqlite", filepath.Join(dir, "wiki.db"))
		if _, err := db.Exec(`UPDATE articles SET parent_id='a-child' WHERE id='z-parent'`); err != nil {
			t.Fatal(err)
		}
		db.Close()
		_, err := loadImportSource(ctx, dir, t.TempDir())
		mustRefuse(t, err, "cycle")
	})
	t.Run("parent in another space", func(t *testing.T) {
		dir := buildLegacyFixture(t)
		db, _ := sql.Open("sqlite", filepath.Join(dir, "wiki.db"))
		if _, err := db.Exec(`UPDATE articles SET parent_id='z-parent' WHERE id='dead'`); err != nil {
			t.Fatal(err)
		}
		db.Close()
		_, err := loadImportSource(ctx, dir, t.TempDir())
		mustRefuse(t, err, "its parent")
	})
}

func TestImportSourceIsNotModified(t *testing.T) {
	for name, build := range map[string]func(*testing.T) string{"current": buildCurrentFixture, "legacy": buildLegacyFixture} {
		t.Run(name, func(t *testing.T) {
			dir := build(t)
			before := treeHash(t, dir)
			src, err := loadImportSource(context.Background(), dir, t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			if treeHash(t, dir) != before {
				t.Fatal("the source changed")
			}
			if len(src.Ordered) != len(src.Data.Articles) {
				t.Fatal("article order lost rows")
			}
			seen := map[string]bool{}
			for _, a := range src.Ordered {
				if a.ParentID != "" && !seen[a.ParentID] {
					t.Fatalf("%s comes before its parent %s", a.ID, a.ParentID)
				}
				seen[a.ID] = true
			}
		})
	}
}

func TestImportRequiresConfiguration(t *testing.T) {
	_, err := ImportSQLite(context.Background(), SQLiteImportConfig{From: t.TempDir()})
	mustRefuse(t, err, "AIWIKI_S3_BUCKET")
	_, err = ImportSQLite(context.Background(), SQLiteImportConfig{})
	mustRefuse(t, err, "-from")
}

// The verification must really compare: tamper with the target and the bucket
// after a good import and run it again.
func TestImportVerificationDetectsDifferences(t *testing.T) {
	dir := buildLegacyFixture(t)
	f := newImportFixture(t, dir)
	if rep, err := f.run(t); err != nil || rep.Failed() {
		t.Fatalf("import: %v\n%s", err, f.out)
	}
	pool, err := openImportPostgres(context.Background(), f.imp)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	for _, q := range []string{
		`UPDATE articles SET body=body||'!' WHERE id='z-parent'`,
		`UPDATE revisions SET snapshot=replace(snapshot,'Родитель','Другой') WHERE id=25`,
		`DELETE FROM comments`,
		`UPDATE article_fts SET body='x' WHERE article_id='a-child'`,
	} {
		if _, err = pool.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	f.fake.objects["wiki/legacy.bin"] = []byte("tampered!!!!")
	src, err := loadImportSource(context.Background(), dir, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store, err := newS3Store(context.Background(), f.cfg.S3)
	if err != nil {
		t.Fatal(err)
	}
	f.out.Reset()
	run := &importRun{cfg: f.cfg, out: f.out, rep: &SQLiteImportReport{}}
	if err = run.verify(context.Background(), pool, store, src); err != nil {
		t.Fatal(err)
	}
	if !run.rep.Failed() || run.rep.Mismatches < 5 {
		t.Fatalf("tampering not detected enough (%d mismatches):\n%s", run.rep.Mismatches, f.out)
	}
	t.Log("\n" + f.out.String())
}
