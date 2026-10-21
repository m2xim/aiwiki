package server

import (
	"encoding/json"
	"net/http"
	"reflect"
	"testing"
)

func TestSpaceAuditTracksRevisionChangesAndMoves(t *testing.T) {
	s := testServer(t)
	code, body := request(t, s, http.MethodPost, "/api/articles", `{"space_id":"general","title":"Audit parent","summary":"one","body":"first","kind":"article","status":"draft","tags":["one"],"properties":{"x":"1"},"author":"alice","message":"created"}`)
	if code != 201 {
		t.Fatalf("create: %d %s", code, body)
	}
	var parent Article
	if err := json.Unmarshal(body, &parent); err != nil {
		t.Fatal(err)
	}
	assertChanges(t, findAuditEvent(t, readAudit(t, s, "general", ""), parent.ID, 1), []string{"created"})
	code, body = request(t, s, http.MethodPost, "/api/articles", `{"space_id":"general","parent_id":"`+parent.ID+`","title":"Audit child","kind":"article","status":"draft","author":"alice"}`)
	if code != 201 {
		t.Fatalf("child create: %d %s", code, body)
	}
	var child Article
	if err := json.Unmarshal(body, &child); err != nil {
		t.Fatal(err)
	}

	code, body = request(t, s, http.MethodPut, "/api/articles/"+parent.ID, `{"space_id":"general","title":"Audit parent edited","summary":"two","body":"second","kind":"research","status":"active","pinned":true,"tags":["two"],"properties":{"x":"2"},"base_revision":1,"author":"bob","message":"edited"}`)
	if code != 200 {
		t.Fatalf("edit: %d %s", code, body)
	}
	editEvents := readAudit(t, s, "general", "")
	assertChanges(t, findAuditEvent(t, editEvents, parent.ID, 2), []string{"title", "summary", "body", "kind", "status", "pinned", "tags", "properties"})
	code, body = request(t, s, http.MethodPost, "/api/articles/"+parent.ID+"/restore", `{"revision":1,"base_revision":2,"author":"cara","message":"restore"}`)
	if code != 200 {
		t.Fatalf("restore: %d %s", code, body)
	}
	restoreEvents := readAudit(t, s, "general", "")
	assertChanges(t, findAuditEvent(t, restoreEvents, parent.ID, 3), []string{"title", "summary", "body", "kind", "status", "pinned", "tags", "properties"})
	code, body = request(t, s, http.MethodDelete, "/api/articles/"+child.ID, `{"base_revision":1,"author":"bob","message":"delete child"}`)
	if code != 200 {
		t.Fatalf("delete child: %d %s", code, body)
	}
	childDeleteEvents := readAudit(t, s, "general", "")
	childDeleted := findAuditEvent(t, childDeleteEvents, child.ID, 2)
	assertChanges(t, childDeleted, []string{"deleted_at"})
	if !childDeleted.Deleted {
		t.Fatalf("child delete event not marked deleted: %#v", childDeleted)
	}
	code, body = request(t, s, http.MethodDelete, "/api/articles/"+parent.ID, `{"base_revision":3,"author":"bob","message":"delete"}`)
	if code != 200 {
		t.Fatalf("delete: %d %s", code, body)
	}
	parentDeleteEvents := readAudit(t, s, "general", "")
	parentDeleted := findAuditEvent(t, parentDeleteEvents, parent.ID, 4)
	assertChanges(t, parentDeleted, []string{"deleted_at"})
	if !parentDeleted.Deleted {
		t.Fatalf("parent delete event not marked deleted: %#v", parentDeleted)
	}
	code, body = request(t, s, http.MethodPost, "/api/articles/"+parent.ID+"/undelete", `{"base_revision":4,"author":"cara","message":"undelete"}`)
	if code != 200 {
		t.Fatalf("undelete: %d %s", code, body)
	}
	undeleteEvents := readAudit(t, s, "general", "")
	parentRestored := findAuditEvent(t, undeleteEvents, parent.ID, 5)
	assertChanges(t, parentRestored, []string{"deleted_at"})
	if parentRestored.Deleted {
		t.Fatalf("undelete event still marked deleted: %#v", parentRestored)
	}
	code, body = request(t, s, http.MethodPut, "/api/articles/"+parent.ID, `{"space_id":"engineering","title":"Audit parent edited","summary":"two","body":"second","kind":"research","status":"active","pinned":true,"tags":["two"],"properties":{"x":"2"},"base_revision":5,"author":"dave","message":"move"}`)
	if code != 200 {
		t.Fatalf("move parent: %d %s", code, body)
	}

	generalEvents := readAudit(t, s, "general", "")
	parentMove := findAuditEvent(t, generalEvents, parent.ID, 6)
	assertChanges(t, parentMove, []string{"title", "summary", "body", "kind", "status", "pinned", "tags", "properties", "space_id"})
	if parentMove.PreviousSpaceID != "general" || parentMove.SpaceID != "engineering" {
		t.Fatalf("parent move attribution: %#v", parentMove)
	}
	childMove := findAuditEvent(t, generalEvents, child.ID, 3)
	assertChanges(t, childMove, []string{"space_id"})
	if !childMove.Deleted || childMove.PreviousSpaceID != "general" || childMove.SpaceID != "engineering" {
		t.Fatalf("deleted descendant move attribution: %#v", childMove)
	}
	engineeringEvents := readAudit(t, s, "engineering", "")
	if hasAuditEvent(engineeringEvents, parent.ID, 1) || hasAuditEvent(engineeringEvents, parent.ID, 2) || hasAuditEvent(engineeringEvents, parent.ID, 3) || hasAuditEvent(engineeringEvents, parent.ID, 4) || hasAuditEvent(engineeringEvents, parent.ID, 5) {
		t.Fatalf("destination contains source-only parent history: %#v", engineeringEvents)
	}
	if !hasAuditEvent(engineeringEvents, parent.ID, 6) || !hasAuditEvent(engineeringEvents, child.ID, 3) {
		t.Fatalf("destination move entries missing: %#v", engineeringEvents)
	}
	if _, err := s.DB.Exec(`UPDATE spaces SET archived_at=? WHERE id='engineering'`, stamp()); err != nil {
		t.Fatal(err)
	}
	if archived := readAudit(t, s, "engineering", ""); len(archived) == 0 {
		t.Fatal("audit unavailable for archived space")
	}
	code, body = request(t, s, http.MethodGet, "/api/spaces/general/audit?author=bob&limit=1&offset=1", "")
	if code != 200 {
		t.Fatalf("filtered audit: %d %s", code, body)
	}
	var filtered []AuditEvent
	if err := json.Unmarshal(body, &filtered); err != nil {
		t.Fatal(err)
	}
	if len(filtered) != 1 || filtered[0].Author != "bob" {
		t.Fatalf("author/pagination: %#v", filtered)
	}
	if filtered[0].ArticleID != child.ID || filtered[0].Revision != 2 {
		t.Fatalf("expected second newest bob revision, got %#v", filtered[0])
	}
	allBob := readAudit(t, s, "general", "?author=bob")
	if len(allBob) < 3 {
		t.Fatalf("expected multiple bob events: %#v", allBob)
	}
	for i := 1; i < len(allBob); i++ {
		if allBob[i-1].ID <= allBob[i].ID {
			t.Fatalf("events are not stably newest first: %#v", allBob)
		}
	}
	if len(allBob) < 2 || allBob[1].ID != filtered[0].ID {
		t.Fatalf("limit/offset does not match stable full ordering: page=%#v all=%#v", filtered, allBob)
	}
	if hasAuthor := readAudit(t, s, "general", "?author=unknown"); len(hasAuthor) != 0 || hasAuthor == nil {
		t.Fatalf("unknown author must return empty array: %#v", hasAuthor)
	}
}

func readAudit(t *testing.T, s *Server, spaceID, query string) []AuditEvent {
	t.Helper()
	code, body := request(t, s, http.MethodGet, "/api/spaces/"+spaceID+"/audit"+query, "")
	if code != http.StatusOK {
		t.Fatalf("audit %s%s: %d %s", spaceID, query, code, body)
	}
	var events []AuditEvent
	if err := json.Unmarshal(body, &events); err != nil {
		t.Fatal(err)
	}
	return events
}

func findAuditEvent(t *testing.T, events []AuditEvent, articleID string, revision int) AuditEvent {
	t.Helper()
	for _, event := range events {
		if event.ArticleID == articleID && event.Revision == revision {
			return event
		}
	}
	t.Fatalf("audit event missing for %s revision %d: %#v", articleID, revision, events)
	return AuditEvent{}
}

func hasAuditEvent(events []AuditEvent, articleID string, revision int) bool {
	for _, event := range events {
		if event.ArticleID == articleID && event.Revision == revision {
			return true
		}
	}
	return false
}

func assertChanges(t *testing.T, event AuditEvent, want []string) {
	t.Helper()
	if !reflect.DeepEqual(event.Changes, want) {
		t.Fatalf("%s revision %d changes=%#v want %#v", event.ArticleID, event.Revision, event.Changes, want)
	}
}

func TestSpaceAuditValidatesSpaceAndPagination(t *testing.T) {
	s := testServer(t)
	for _, tc := range []struct {
		path string
		want int
	}{
		{"/api/spaces/missing/audit", 404},
		{"/api/spaces/general/audit?limit=0", 400},
		{"/api/spaces/general/audit?limit=201", 400},
		{"/api/spaces/general/audit?limit=no", 400},
		{"/api/spaces/general/audit?offset=-1", 400},
		{"/api/spaces/general/audit?offset=x", 400},
	} {
		code, body := request(t, s, http.MethodGet, tc.path, "")
		if code != tc.want {
			t.Errorf("%s: got %d want %d: %s", tc.path, code, tc.want, body)
		}
	}
}
