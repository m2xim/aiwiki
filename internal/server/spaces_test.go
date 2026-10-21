package server

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSpaceManagement(t *testing.T) {
	s := testServer(t)
	code, body := request(t, s, "PATCH", "/api/spaces/general", `{"name":"  Новое имя  ","description":"new description"}`)
	var updated Space
	if code != 200 || json.Unmarshal(body, &updated) != nil || updated.Name != "Новое имя" || updated.Description != "new description" {
		t.Fatalf("rename: %d %s", code, body)
	}
	for _, payload := range []string{`{}`, `null`, `{"name":" "}`, `{"archived":"yes"}`, `{"name":"` + strings.Repeat("я", 301) + `"}`} {
		if code, body := request(t, s, "PATCH", "/api/spaces/general", payload); code != 400 {
			t.Fatalf("invalid patch %s: %d %s", payload, code, body)
		}
	}
	if code, _ := request(t, s, "PATCH", "/api/spaces/missing", `{"archived":true}`); code != 404 {
		t.Fatalf("missing: %d", code)
	}
	if code, _ := request(t, s, "DELETE", "/api/spaces/general", ""); code != 405 {
		t.Fatalf("method: %d", code)
	}
	code, body = request(t, s, "PATCH", "/api/spaces/general", `{"archived":true}`)
	if code != 200 || json.Unmarshal(body, &updated) != nil || updated.ArchivedAt == "" {
		t.Fatalf("archive: %d %s", code, body)
	}
	firstArchive := updated.ArchivedAt
	code, body = request(t, s, "PATCH", "/api/spaces/general", `{"archived":true}`)
	if code != 200 || json.Unmarshal(body, &updated) != nil || updated.ArchivedAt != firstArchive {
		t.Fatalf("idempotent archive: %d %s", code, body)
	}
	_, body = request(t, s, "GET", "/api/spaces", "")
	if strings.Contains(string(body), `"id":"general"`) {
		t.Fatalf("archive in active list: %s", body)
	}
	_, body = request(t, s, "GET", "/api/spaces?include_archived=true", "")
	if !strings.Contains(string(body), `"archived_at"`) || !strings.Contains(string(body), `"id":"general"`) {
		t.Fatalf("archive missing: %s", body)
	}
	code, body = request(t, s, "PATCH", "/api/spaces/general", `{"archived":false}`)
	if code != 200 || strings.Contains(string(body), "archived_at") {
		t.Fatalf("restore: %d %s", code, body)
	}
	_, body = request(t, s, "GET", "/api/spaces", "")
	if !strings.Contains(string(body), `"id":"general"`) {
		t.Fatalf("restored missing: %s", body)
	}
}

func TestSpaceArchivePreservesContentAndExport(t *testing.T) {
	s := testServer(t)
	file := filepath.Join(s.DataDir, "files", "fixture")
	if err := os.WriteFile(file, []byte("retained bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec(`INSERT INTO attachments(id,article_id,name,mime,size,stored_name,created_at) VALUES('space-file','start-here','fixture','text/plain',14,'fixture',?)`, stamp()); err != nil {
		t.Fatal(err)
	}
	_, before := request(t, s, "GET", "/api/articles/start-here", "")
	_, history := request(t, s, "GET", "/api/articles/start-here/history", "")
	for _, id := range []string{"general", "genealogy", "engineering"} {
		if code, body := request(t, s, "PATCH", "/api/spaces/"+id, `{"archived":true}`); code != 200 {
			t.Fatalf("archive all: %d %s", code, body)
		}
	}
	_, after := request(t, s, "GET", "/api/articles/start-here", "")
	_, afterHistory := request(t, s, "GET", "/api/articles/start-here/history", "")
	if string(before) != string(after) || string(history) != string(afterHistory) {
		t.Fatal("archive changed article or revisions")
	}
	if bytes, err := os.ReadFile(file); err != nil || string(bytes) != "retained bytes" {
		t.Fatalf("file changed: %s %v", bytes, err)
	}
	for _, path := range []string{"/api/bootstrap", "/api/bootstrap?space_id=general", "/api/bootstrap?space_id=engineering"} {
		code, body := request(t, s, "GET", path, "")
		var result struct {
			Spaces       []Space   `json:"spaces"`
			Instructions []Article `json:"instructions"`
			Recent       []Article `json:"recent"`
		}
		if code != 200 || json.Unmarshal(body, &result) != nil || len(result.Spaces)+len(result.Instructions)+len(result.Recent) != 0 {
			t.Fatalf("bootstrap includes archives: %d %s", code, body)
		}
	}
	code, body := request(t, s, "GET", "/api/export", "")
	var exported struct {
		Spaces      []Space      `json:"spaces"`
		Articles    []Article    `json:"articles"`
		Revisions   []Revision   `json:"revisions"`
		Attachments []Attachment `json:"attachments"`
	}
	if code != 200 || json.Unmarshal(body, &exported) != nil || len(exported.Spaces) != 3 || len(exported.Articles) == 0 || len(exported.Revisions) == 0 || len(exported.Attachments) != 1 {
		t.Fatalf("export lost content: %d %s", code, body)
	}
	for _, space := range exported.Spaces {
		if space.ArchivedAt == "" {
			t.Fatal("export lost archived_at")
		}
	}
}

func TestSpaceArchiveMigratesOldDatabase(t *testing.T) {
	dir := t.TempDir()
	db, err := sql.Open("sqlite", filepath.Join(dir, "wiki.db"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`CREATE TABLE spaces(id TEXT PRIMARY KEY,name TEXT NOT NULL,description TEXT NOT NULL DEFAULT ''); INSERT INTO spaces VALUES('legacy','Legacy','kept')`)
	if err != nil {
		t.Fatal(err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		s, err := Open(dir, "")
		if err != nil {
			t.Fatal(err)
		}
		space, err := scanSpace(s.DB.QueryRow("SELECT id,name,description,archived_at FROM spaces WHERE id='legacy'"))
		if err != nil || space.Name != "Legacy" || space.Description != "kept" || (i == 1 && space.ArchivedAt == "") {
			t.Fatalf("migration lost data: %#v %v", space, err)
		}
		if i == 0 {
			if code, body := request(t, s, "PATCH", "/api/spaces/legacy", `{"archived":true}`); code != 200 {
				t.Fatalf("archive migrated: %d %s", code, body)
			}
		}
		if err = s.Close(); err != nil {
			t.Fatal(err)
		}
	}
}
