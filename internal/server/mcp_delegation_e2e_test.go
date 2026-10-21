package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCorporateMCPDelegationEndToEnd(t *testing.T) {
	owner, root := corporateFixture(t)
	x := newAuthHarness(t, nil)
	x.a.users = authSQLUsers{db: root.DB}
	root.auth = x.a
	cookie := authCookie(x.login(authUserGrant("delegation-owner", "aiwiki-access", "aiwiki-admin")), x.a.sessionCookie)
	call := func(method, path, body, csrf string, c *http.Cookie) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "http://127.0.0.1:8090"+path, strings.NewReader(body))
		if c != nil {
			r.AddCookie(c)
		}
		if csrf != "" {
			r.Header.Set("X-CSRF-Token", csrf)
		}
		w := httptest.NewRecorder()
		root.Handler().ServeHTTP(w, r)
		return w
	}
	var me struct {
		CSRF string `json:"csrf_token"`
		User struct {
			ID string `json:"id"`
		} `json:"user"`
	}
	if err := json.Unmarshal(call("GET", "/api/me", "", "", cookie).Body.Bytes(), &me); err != nil {
		t.Fatal(err)
	}
	spaceResponse := call("POST", "/api/corporate/spaces", `{"name":"Private scope parent","personal":true}`, me.CSRF, cookie)
	if spaceResponse.Code != 201 {
		t.Fatal(spaceResponse.Body.String())
	}
	var space Space
	if err := json.Unmarshal(spaceResponse.Body.Bytes(), &space); err != nil {
		t.Fatal(err)
	}
	project := func(name string) string {
		t.Helper()
		b, _ := json.Marshal(map[string]any{"space_id": space.ID, "name": name})
		r := call("POST", "/api/projects", string(b), me.CSRF, cookie)
		if r.Code != 201 {
			t.Fatal(r.Body.String())
		}
		var p corporateProject
		if err := json.Unmarshal(r.Body.Bytes(), &p); err != nil {
			t.Fatal(err)
		}
		return p.ID
	}
	aProject, bProject := project("Selected project"), project("Private sibling project")
	article := func(p, title string) Article {
		t.Helper()
		b, _ := json.Marshal(map[string]any{"space_id": space.ID, "project_id": p, "title": title, "body": "# Deployment\nneedle " + title, "tags": []string{title}})
		r := call("POST", "/api/articles", string(b), me.CSRF, cookie)
		if r.Code != 201 {
			t.Fatal(r.Body.String())
		}
		var a Article
		if err := json.Unmarshal(r.Body.Bytes(), &a); err != nil {
			t.Fatal(err)
		}
		return a
	}
	a, b := article(aProject, "Selected material"), article(bProject, "Forbidden material")
	type issued struct {
		Token string `json:"token"`
		ID    string `json:"id"`
	}
	mint := func() issued {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"name": "agent test", "project_ids": []string{aProject}, "ttl_seconds": 300})
		r := call("POST", "/api/mcp/delegations", string(body), me.CSRF, cookie)
		if r.Code != 201 {
			t.Fatalf("mint %d %s", r.Code, r.Body.String())
		}
		var out issued
		if err := json.Unmarshal(r.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(out.Token, mcpDelegationPrefix) {
			t.Fatal("missing one-time secret")
		}
		return out
	}
	key := mint()
	mcp := func(token, method, tool string, args any) *httptest.ResponseRecorder {
		t.Helper()
		params := map[string]any{}
		if tool != "" {
			params["name"] = tool
			params["arguments"] = args
		}
		body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
		r := httptest.NewRequest("POST", "http://127.0.0.1:8090/mcp", strings.NewReader(string(body)))
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		root.Handler().ServeHTTP(w, r)
		return w
	}
	if r := mcp(key.Token, "tools/call", "wiki_read", map[string]any{"id": a.ID, "max_chars": 4000}); r.Code != 200 || !strings.Contains(r.Body.String(), "Selected material") {
		t.Fatalf("selected read %d %s", r.Code, r.Body.String())
	}
	for _, tc := range []struct {
		tool string
		args any
	}{
		{"wiki_read", map[string]any{"id": b.ID}},
		{"wiki_history", map[string]any{"id": b.ID}},
		{"wiki_search", map[string]any{"q": "needle"}},
		{"wiki_search_sections", map[string]any{"q": "Deployment"}},
		{"wiki_bootstrap", map[string]any{}},
	} {
		r := mcp(key.Token, "tools/call", tc.tool, tc.args)
		if r.Code != 200 || strings.Contains(r.Body.String(), "Forbidden material") || strings.Contains(r.Body.String(), "Private sibling project") || strings.Contains(r.Body.String(), "Private scope parent") {
			t.Fatalf("scope leak %s: %d %s", tc.tool, r.Code, r.Body.String())
		}
	}
	if r := mcp(key.Token, "tools/list", "", nil); strings.Contains(r.Body.String(), `"wiki_write"`) || strings.Contains(r.Body.String(), `"wiki_comment"`) {
		t.Fatal("write tools advertised")
	}
	if r := mcp(key.Token, "tools/call", "wiki_write", map[string]any{"space_id": space.ID, "project_id": aProject, "title": "unauthorized"}); !strings.Contains(r.Body.String(), "read-only") {
		t.Fatal("mutation not rejected")
	}
	if r := call("GET", "/api/mcp/delegations", "", "", cookie); strings.Contains(r.Body.String(), key.Token) || strings.Contains(r.Body.String(), "SessionKey") {
		t.Fatal("secret shown again")
	}
	if r := call("POST", "/api/mcp/delegations", `{}`, "", cookie); r.Code != 403 {
		t.Fatal("mint missing CSRF accepted")
	}
	// Bearer credentials are never browser/API credentials.
	r := httptest.NewRequest("GET", "http://127.0.0.1:8090/api/articles/"+a.ID, nil)
	r.Header.Set("Authorization", "Bearer "+key.Token)
	w := httptest.NewRecorder()
	root.Handler().ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatalf("delegation authorized REST: %d", w.Code)
	}
	// Removing current owner access affects an already-issued key without cache.
	if rr := call("PATCH", "/api/projects/"+aProject, `{"inherit_acl":false}`, me.CSRF, cookie); rr.Code != 200 {
		t.Fatal(rr.Body.String())
	}
	path := "/api/access?resource_type=project&resource_id=" + aProject
	if rr := call("PUT", path, `{"grant":{"subject_type":"user","subject_id":"bob","role":"owner"}}`, me.CSRF, cookie); rr.Code != 200 {
		t.Fatal(rr.Body.String())
	}
	if rr := call("DELETE", path+"&subject_type=user&subject_id="+me.User.ID, "", me.CSRF, cookie); rr.Code != 204 {
		t.Fatal(rr.Body.String())
	}
	if rr := mcp(key.Token, "tools/call", "wiki_read", map[string]any{"id": a.ID}); strings.Contains(rr.Body.String(), "Selected material") {
		t.Fatal("owner ACL revocation did not restrict delegation")
	}
	if _, err := owner.DB.Exec(`INSERT INTO memberships(resource_type,resource_id,subject_type,subject_id,role) VALUES('project',?,'user',?,'owner')`, aProject, me.User.ID); err != nil {
		t.Fatal(err)
	}
	// Explicit revocation is effective on the next request.
	var listed []mcpDelegation
	if err := json.Unmarshal(call("GET", "/api/mcp/delegations", "", "", cookie).Body.Bytes(), &listed); err != nil || len(listed) != 1 {
		t.Fatal("cannot list delegation")
	}
	if rr := call("DELETE", "/api/mcp/delegations/"+listed[0].ID, "", me.CSRF, cookie); rr.Code != 204 {
		t.Fatal(rr.Body.String())
	}
	if rr := mcp(key.Token, "tools/call", "wiki_read", map[string]any{"id": a.ID}); rr.Code != 401 {
		t.Fatalf("revoked key %d", rr.Code)
	}
	key = mint()
	if rr := call("POST", "/auth/logout", "", me.CSRF, cookie); rr.Code != 204 {
		t.Fatal(rr.Body.String())
	}
	if rr := mcp(key.Token, "tools/call", "wiki_read", map[string]any{"id": a.ID}); rr.Code != 401 {
		t.Fatalf("logout key %d", rr.Code)
	}
}
