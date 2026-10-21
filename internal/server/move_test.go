package server

import (
	"encoding/json"
	"net/http"
	"reflect"
	"testing"
)

func createMoveArticle(t *testing.T, s *Server, space, parent, title string) Article {
	t.Helper()
	payload := map[string]any{"space_id": space, "parent_id": parent, "title": title, "body": "body " + title, "kind": "article", "status": "draft", "tags": []string{"move"}, "properties": map[string]string{"key": "value"}}
	code, body := request(t, s, http.MethodPost, "/api/articles", encode(payload))
	if code != 201 {
		t.Fatalf("create: %d %s", code, body)
	}
	var a Article
	if err := json.Unmarshal(body, &a); err != nil {
		t.Fatal(err)
	}
	return a
}

func movePayload(a Article, space, parent string) string {
	var payload map[string]any
	_ = json.Unmarshal([]byte(encode(a)), &payload)
	payload["space_id"] = space
	payload["parent_id"] = parent
	payload["base_revision"] = a.Revision
	payload["author"] = "mover"
	return encode(payload)
}

func TestMoveSubtreePreservesDataAndRevisions(t *testing.T) {
	s := testServer(t)
	root := createMoveArticle(t, s, "general", "", "root")
	child := createMoveArticle(t, s, "general", root.ID, "child")
	grandchild := createMoveArticle(t, s, "general", child.ID, "grandchild")
	sibling := createMoveArticle(t, s, "general", root.ID, "sibling")
	unrelated := createMoveArticle(t, s, "general", "", "unrelated")
	destination := createMoveArticle(t, s, "engineering", "", "destination")
	code, body := request(t, s, http.MethodDelete, "/api/articles/"+grandchild.ID, `{"base_revision":1}`)
	if code != 200 {
		t.Fatalf("delete: %d %s", code, body)
	}
	grandchild, _ = s.articleByID(s.DB, grandchild.ID)
	if _, err := s.DB.Exec(`INSERT INTO attachments(id,article_id,name,mime,size,stored_name,created_at) VALUES('move-attachment',?,'test','text/plain',4,'test',?)`, child.ID, stamp()); err != nil {
		t.Fatal(err)
	}
	code, body = request(t, s, http.MethodPut, "/api/articles/"+root.ID, movePayload(root, "engineering", destination.ID))
	if code != 200 {
		t.Fatalf("move: %d %s", code, body)
	}
	var movedRoot Article
	_ = json.Unmarshal(body, &movedRoot)
	for _, before := range []Article{child, grandchild, sibling} {
		after, err := s.articleByID(s.DB, before.ID)
		if err != nil {
			t.Fatal(err)
		}
		want := before
		want.SpaceID, want.Author, want.UpdatedAt = "engineering", "mover", movedRoot.UpdatedAt
		want.Revision++
		if !reflect.DeepEqual(after, want) {
			t.Fatalf("moved descendant: got %#v want %#v", after, want)
		}
		var raw string
		if err := s.DB.QueryRow(`SELECT snapshot FROM revisions WHERE article_id=? AND revision=?`, after.ID, after.Revision).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		var snapshot Article
		_ = json.Unmarshal([]byte(raw), &snapshot)
		if !reflect.DeepEqual(snapshot, after) {
			t.Fatalf("snapshot differs: %#v", snapshot)
		}
		var indexed int
		if err := s.DB.QueryRow(`SELECT count(*) FROM article_fts WHERE article_id=?`, after.ID).Scan(&indexed); err != nil {
			t.Fatal(err)
		}
		if (after.DeletedAt == "" && indexed != 1) || (after.DeletedAt != "" && indexed != 0) {
			t.Fatalf("index count %d for %#v", indexed, after)
		}
	}
	unchanged, _ := s.articleByID(s.DB, unrelated.ID)
	if !reflect.DeepEqual(unchanged, unrelated) {
		t.Fatal("unrelated article changed")
	}
	var attachmentOwner string
	if err := s.DB.QueryRow(`SELECT article_id FROM attachments WHERE id='move-attachment'`).Scan(&attachmentOwner); err != nil || attachmentOwner != child.ID {
		t.Fatalf("attachment changed: %q %v", attachmentOwner, err)
	}
	code, body = request(t, s, http.MethodPut, "/api/articles/"+child.ID, movePayload(child, "general", root.ID))
	if code != 409 {
		t.Fatalf("stale child edit: %d %s", code, body)
	}
	// A restore uses the current descendants, including children created after
	// the historic snapshot, and leaves their current content intact.
	newChild := createMoveArticle(t, s, "engineering", root.ID, "new child")
	code, body = request(t, s, http.MethodPost, "/api/articles/"+root.ID+"/restore", `{"revision":1,"base_revision":2,"author":"restorer"}`)
	if code != 200 {
		t.Fatalf("restore: %d %s", code, body)
	}
	for _, before := range []Article{child, grandchild, sibling, newChild} {
		after, err := s.articleByID(s.DB, before.ID)
		if err != nil || after.SpaceID != "general" || after.Body != before.Body || after.DeletedAt != before.DeletedAt || after.Author != "restorer" {
			t.Fatalf("restored descendant %#v: %v", after, err)
		}
	}
}

func TestMoveSubtreeRollback(t *testing.T) {
	for _, scenario := range []string{"invalid space", "invalid parent", "cycle", "child revision failure"} {
		t.Run(scenario, func(t *testing.T) {
			s := testServer(t)
			root := createMoveArticle(t, s, "general", "", "root")
			child := createMoveArticle(t, s, "general", root.ID, "child")
			destination, parent, wantCode := "engineering", "", 400
			switch scenario {
			case "invalid space":
				destination = "missing"
			case "invalid parent":
				parent = "missing"
			case "cycle":
				destination, parent = "general", child.ID
			case "child revision failure":
				// Fail after the root write and the child's UPDATE have succeeded.
				if _, err := s.DB.Exec(`CREATE TRIGGER fail_child_revision BEFORE INSERT ON revisions WHEN NEW.article_id='` + child.ID + `' AND NEW.revision=2 BEGIN SELECT RAISE(ABORT,'injected failure'); END`); err != nil {
					t.Fatal(err)
				}
				wantCode = 500
			}
			code, body := request(t, s, http.MethodPut, "/api/articles/"+root.ID, movePayload(root, destination, parent))
			if code != wantCode {
				t.Fatalf("move: %d %s", code, body)
			}
			for _, before := range []Article{root, child} {
				after, err := s.articleByID(s.DB, before.ID)
				if err != nil || !reflect.DeepEqual(before, after) {
					t.Fatalf("rollback article: %#v %v", after, err)
				}
				var count int
				if err := s.DB.QueryRow(`SELECT count(*) FROM revisions WHERE article_id=?`, before.ID).Scan(&count); err != nil || count != 1 {
					t.Fatalf("rollback history: %d %v", count, err)
				}
			}
		})
	}
}

func TestSameSpaceReparentLeavesDescendantsUnchanged(t *testing.T) {
	s := testServer(t)
	root := createMoveArticle(t, s, "general", "", "root")
	child := createMoveArticle(t, s, "general", root.ID, "child")
	destination := createMoveArticle(t, s, "general", "", "destination")
	code, body := request(t, s, http.MethodPut, "/api/articles/"+root.ID, movePayload(root, "general", destination.ID))
	if code != 200 {
		t.Fatalf("reparent: %d %s", code, body)
	}
	afterRoot, _ := s.articleByID(s.DB, root.ID)
	if afterRoot.ParentID != destination.ID || afterRoot.Revision != 2 {
		t.Fatalf("root not reparented: %#v", afterRoot)
	}
	afterChild, err := s.articleByID(s.DB, child.ID)
	if err != nil || !reflect.DeepEqual(afterChild, child) {
		t.Fatalf("descendant changed: %#v %v", afterChild, err)
	}
	var count int
	if err := s.DB.QueryRow(`SELECT count(*) FROM revisions WHERE article_id=?`, child.ID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("descendant history: %d %v", count, err)
	}
}

func TestMoveTraversesDeletedBranch(t *testing.T) {
	s := testServer(t)
	root := createMoveArticle(t, s, "general", "", "root")
	child := createMoveArticle(t, s, "general", root.ID, "child")
	grandchild := createMoveArticle(t, s, "general", child.ID, "grandchild")
	deleted := []Article{}
	for _, a := range []Article{grandchild, child} {
		code, body := request(t, s, http.MethodDelete, "/api/articles/"+a.ID, `{"base_revision":1}`)
		if code != 200 {
			t.Fatalf("delete: %d %s", code, body)
		}
		before, err := s.articleByID(s.DB, a.ID)
		if err != nil {
			t.Fatal(err)
		}
		deleted = append(deleted, before)
	}
	code, body := request(t, s, http.MethodPut, "/api/articles/"+root.ID, movePayload(root, "engineering", ""))
	if code != 200 {
		t.Fatalf("move: %d %s", code, body)
	}
	var movedRoot Article
	_ = json.Unmarshal(body, &movedRoot)
	for _, before := range deleted {
		after, err := s.articleByID(s.DB, before.ID)
		want := before
		want.SpaceID, want.Author, want.UpdatedAt = "engineering", "mover", movedRoot.UpdatedAt
		want.Revision++
		if err != nil || !reflect.DeepEqual(after, want) {
			t.Fatalf("deleted branch: got %#v want %#v error %v", after, want, err)
		}
	}
}
