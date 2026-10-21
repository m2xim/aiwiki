package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCorporateSeparateChildAndS3Isolation(t *testing.T) {
	s3Env(t)
	fake, endpoint := newFakeS3(t)
	objects, err := newS3Store(context.Background(), S3Config{Bucket: "wiki", Region: "us-east-1", Endpoint: endpoint.URL, Prefix: "corporate", PathStyle: true, AllowInsecureLoopback: true})
	if err != nil {
		t.Fatal(err)
	}
	_, root := corporateFixture(t)
	root.objects = objects
	alice := corporateAs(t, root, "alice")
	bob := corporateAs(t, root, "bob")
	space := corporateCreateSpace(t, alice)
	project := func(parent, name string) string {
		t.Helper()
		r := corporateHTTP(alice, "POST", "/api/projects", `{"space_id":"`+space+`","parent_id":"`+parent+`","name":"`+name+`"}`)
		if r.Code != 201 {
			t.Fatal(r.Body.String())
		}
		var p struct {
			ID string `json:"id"`
		}
		json.Unmarshal(r.Body.Bytes(), &p)
		return p.ID
	}
	parent := project("", "Secret Parent Name")
	child := project(parent, "Visible Child")
	if hidden := corporateHTTP(alice, "POST", "/api/articles", `{"space_id":"`+space+`","project_id":"`+parent+`","title":"Hidden parent document","tags":["secret-parent-tag"],"body":"private payload"}`); hidden.Code != 201 {
		t.Fatal(hidden.Body.String())
	}
	grant := corporateHTTP(alice, "PUT", "/api/access?resource_type=project&resource_id="+child, `{"grant":{"subject_type":"user","subject_id":"bob","role":"reader"}}`)
	if grant.Code != 200 {
		t.Fatal(grant.Body.String())
	}
	rr := corporateHTTP(alice, "POST", "/api/articles", `{"space_id":"`+space+`","project_id":"`+child+`","title":"Child Doc","tags":["visible-child-tag"],"body":"searchable payload"}`)
	if rr.Code != 201 {
		t.Fatal(rr.Body.String())
	}
	var a Article
	json.Unmarshal(rr.Body.Bytes(), &a)
	for _, path := range []string{"/api/catalog", "/api/spaces", "/api/projects"} {
		r := corporateHTTP(bob, "GET", path, "")
		if r.Code != 200 {
			t.Fatal(r.Body.String())
		}
		if strings.Contains(r.Body.String(), "Secret Parent Name") || strings.Contains(r.Body.String(), "Private Alice") {
			t.Fatalf("ancestor metadata leak %s", r.Body.String())
		}
		if path != "/api/spaces" && !strings.Contains(r.Body.String(), "Visible Child") {
			t.Fatalf("child missing %s", r.Body.String())
		}
		if path == "/api/catalog" {
			var catalog []struct {
				ArticleCount int              `json:"article_count"`
				Tags         []string         `json:"tags"`
				Projects     []catalogProject `json:"projects"`
			}
			if err := json.Unmarshal(r.Body.Bytes(), &catalog); err != nil {
				t.Fatal(err)
			}
			if len(catalog) != 1 || catalog[0].ArticleCount != 1 || len(catalog[0].Projects) != 1 || catalog[0].Projects[0].ArticleCount != 1 || strings.Contains(r.Body.String(), "secret-parent-tag") || !strings.Contains(r.Body.String(), "visible-child-tag") {
				t.Fatalf("catalog counts/tags include inaccessible documents: %s", r.Body.String())
			}
		}
	}
	if r := corporateHTTP(bob, "GET", "/api/search?q=payload", ""); !strings.Contains(r.Body.String(), a.ID) {
		t.Fatalf("global search omitted independent child %s", r.Body.String())
	}
	body, ct := uploadBody(t, "note.txt", []byte("private object"))
	req := httptest.NewRequest(http.MethodPost, "/api/articles/"+a.ID+"/attachments", body)
	req.Header.Set("Content-Type", ct)
	rr = httptest.NewRecorder()
	alice.routes().ServeHTTP(rr, req)
	if rr.Code != 201 {
		t.Fatalf("upload %d %s", rr.Code, rr.Body.String())
	}
	var file Attachment
	json.Unmarshal(rr.Body.Bytes(), &file)
	if r := corporateHTTP(bob, "GET", file.URL, ""); r.Code != 200 || r.Body.String() != "private object" {
		t.Fatalf("shared child file %d %s", r.Code, r.Body.String())
	}
	if r := corporateHTTP(bob, "DELETE", "/api/articles/"+a.ID+"/attachments/"+file.ID, ""); r.Code != 403 {
		t.Fatalf("reader attachment delete %d", r.Code)
	}
	calls := len(fake.auths)
	body, ct = uploadBody(t, "attack.txt", []byte("reader upload"))
	req = httptest.NewRequest("POST", "/api/articles/"+a.ID+"/attachments", body)
	req.Header.Set("Content-Type", ct)
	rr = httptest.NewRecorder()
	bob.routes().ServeHTTP(rr, req)
	if rr.Code != 403 || len(fake.auths) != calls {
		t.Fatal("reader upload reached S3")
	}
	if r := corporateHTTP(alice, "DELETE", "/api/access?resource_type=project&resource_id="+child+"&subject_type=user&subject_id=bob", ""); r.Code != 204 {
		t.Fatal(r.Body.String())
	}
	if r := corporateHTTP(bob, "GET", file.URL, ""); r.Code != 404 || len(fake.auths) != calls {
		t.Fatal("revoked file request reached object store")
	}
	if r := corporateHTTP(bob, "GET", "/api/catalog", ""); strings.Contains(r.Body.String(), child) {
		t.Fatal("revoked child remains in catalog")
	}
	// No corporate bytes are written to the legacy attachment directory.
	if len(fake.objects) != 1 {
		t.Fatal("missing S3 object")
	}
}
