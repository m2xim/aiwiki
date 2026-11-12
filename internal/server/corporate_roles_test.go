package server

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"
)

func TestCorporateRolesInheritanceAndAtomicGrants(t *testing.T) {
	_, root := corporateFixture(t)
	alice := corporateAs(t, root, "alice")
	bob := corporateAs(t, root, "bob")
	reader := corporateAs(t, root, "reader")
	commenter := corporateAs(t, root, "commenter")
	editor := corporateAs(t, root, "editor")
	space := corporateCreateSpace(t, alice)
	grant := func(s *Server, kind, id, user, role string) int {
		r := corporateHTTP(s, "PUT", "/api/access?resource_type="+kind+"&resource_id="+id, `{"grant":{"subject_type":"user","subject_id":"`+user+`","role":"`+role+`"}}`)
		if r.Code != 200 {
			t.Logf("grant %s %s %d %s", user, role, r.Code, r.Body.String())
		}
		return r.Code
	}
	if grant(alice, "space", space, "bob", "manager") != 200 {
		t.Fatal("owner cannot grant manager")
	}
	if grant(bob, "space", space, "alice", "reader") != 403 {
		t.Fatal("manager downgraded owner")
	}
	if grant(bob, "space", space, "reader", "reader") != 200 {
		t.Fatal("manager cannot grant reader")
	}
	// Independent changes to different principals never replace the ACL snapshot.
	var wg sync.WaitGroup
	codes := make(chan int, 2)
	for _, user := range []string{"commenter", "editor"} {
		cp := corporateAs(t, root, "alice")
		wg.Add(1)
		go func(s *Server, user string) {
			defer wg.Done()
			codes <- corporateHTTP(s, "PUT", "/api/access?resource_type=space&resource_id="+space, `{"grant":{"subject_type":"user","subject_id":"`+user+`","role":"commenter"}}`).Code
		}(cp, user)
	}
	wg.Wait()
	close(codes)
	for code := range codes {
		if code != 200 {
			t.Fatalf("concurrent grant %d", code)
		}
	}
	grants := corporateHTTP(alice, "GET", "/api/access?resource_type=space&resource_id="+space, "")
	if !strings.Contains(grants.Body.String(), "commenter") || !strings.Contains(grants.Body.String(), "editor") {
		t.Fatal("lost concurrent grants")
	}
	rr := corporateHTTP(alice, "POST", "/api/articles", `{"space_id":"`+space+`","title":"Nested article","body":"nested needle","tags":["team.a"],"properties":{"key.with.dots":"v"}}`)
	if rr.Code != 201 {
		t.Fatalf("article %d %s", rr.Code, rr.Body.String())
	}
	var a Article
	json.Unmarshal(rr.Body.Bytes(), &a)
	if rr := corporateHTTP(reader, "GET", "/api/articles/"+a.ID, ""); rr.Code != 200 {
		t.Fatal("space grant does not reach the article")
	}
	if rr := corporateHTTP(reader, "GET", "/api/search?tag=team.a&property=key.with.dots:v", ""); rr.Code != 200 || !strings.Contains(rr.Body.String(), a.ID) {
		t.Fatalf("PG JSON filtering %d %s", rr.Code, rr.Body.String())
	}
	if rr := corporateHTTP(reader, "POST", "/api/articles/"+a.ID+"/comments", `{"body":"forbidden"}`); rr.Code < 400 {
		t.Fatal("reader posted comment")
	}
	rr = corporateHTTP(commenter, "POST", "/api/articles/"+a.ID+"/comments", `{"body":"mine","author":"spoof"}`)
	if rr.Code != 201 {
		t.Fatalf("commenter %d %s", rr.Code, rr.Body.String())
	}
	var c Comment
	json.Unmarshal(rr.Body.Bytes(), &c)
	if c.Author != "commenter" {
		t.Fatal("comment author spoof")
	}
	rr = corporateHTTP(editor, "PATCH", "/api/comments/"+c.ID, `{"body":"other comment"}`)
	if rr.Code < 400 {
		t.Fatal("commenter edited someone else's comment")
	}
	if grant(alice, "space", space, "editor", "editor") != 200 {
		t.Fatal("editor grant")
	}
	if rr := corporateHTTP(editor, "PATCH", "/api/comments/"+c.ID, `{"body":"moderated"}`); rr.Code != 200 {
		t.Fatalf("editor moderation %d %s", rr.Code, rr.Body.String())
	}
	// Revoking the space grant revokes the article.
	if rr := corporateHTTP(alice, "DELETE", "/api/access?resource_type=space&resource_id="+space+"&subject_type=user&subject_id=reader", ""); rr.Code != 204 {
		t.Fatalf("revoke %d %s", rr.Code, rr.Body.String())
	}
	if rr := corporateHTTP(reader, "GET", "/api/articles/"+a.ID, ""); rr.Code != 404 {
		t.Fatal("revoking the space grant did not revoke the article")
	}
	if rr := corporateHTTP(alice, "DELETE", "/api/access?resource_type=space&resource_id="+space+"&subject_type=user&subject_id=alice", ""); rr.Code != 409 {
		t.Fatalf("removed last owner %d", rr.Code)
	}
}
