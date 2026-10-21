package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Exercise the complete middleware with real PostgreSQL and a signed OIDC issuer.
func TestCorporateOIDCEndToEnd(t *testing.T) {
	_, s := corporateFixture(t)
	x := newAuthHarness(t, nil)
	x.a.users = authSQLUsers{db: s.DB}
	s.auth = x.a
	cookie := authCookie(x.login(authUserGrant("employee", "aiwiki-access")), x.a.sessionCookie)
	if cookie == nil {
		t.Fatal("OIDC login did not create session")
	}
	call := func(method, path, body, csrf string, c *http.Cookie) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, "http://127.0.0.1:8090"+path, strings.NewReader(body))
		if c != nil {
			req.AddCookie(c)
		}
		if csrf != "" {
			req.Header.Set("X-CSRF-Token", csrf)
		}
		rr := httptest.NewRecorder()
		s.Handler().ServeHTTP(rr, req)
		return rr
	}
	if rr := call("GET", "/api/spaces", "", "", nil); rr.Code != 401 {
		t.Fatalf("anonymous %d", rr.Code)
	}
	me := call("GET", "/api/me", "", "", cookie)
	if me.Code != 200 {
		t.Fatal(me.Body.String())
	}
	var identity struct {
		CSRF string    `json:"csrf_token"`
		User Principal `json:"user"`
	}
	json.Unmarshal(me.Body.Bytes(), &identity)
	if identity.User.ID == "" || identity.CSRF == "" {
		t.Fatal("missing identity/CSRF")
	}
	if rr := call("POST", "/api/corporate/spaces", `{"name":"Mine","personal":true}`, "", cookie); rr.Code != 403 {
		t.Fatalf("CSRF bypass %d", rr.Code)
	}
	rr := call("POST", "/api/corporate/spaces", `{"name":"Mine","personal":true}`, identity.CSRF, cookie)
	if rr.Code != 201 {
		t.Fatalf("space %d %s", rr.Code, rr.Body.String())
	}
	var space Space
	json.Unmarshal(rr.Body.Bytes(), &space)
	rr = call("POST", "/api/articles", `{"space_id":"`+space.ID+`","title":"Corporate secret","body":"# Hidden\nconfidential","author":"spoof"}`, identity.CSRF, cookie)
	if rr.Code != 201 {
		t.Fatalf("article %d %s", rr.Code, rr.Body.String())
	}
	var a Article
	json.Unmarshal(rr.Body.Bytes(), &a)
	if a.Author != identity.User.ID {
		t.Fatal("author not server-derived")
	}
	if rr := call("POST", "/mcp", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, identity.CSRF, cookie); rr.Code != 401 {
		t.Fatal("MCP accepted cookie")
	}
	mcp := func(token, tool, args string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "http://127.0.0.1:8090/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"`+tool+`","arguments":`+args+`}}`))
		req.Header.Set("Authorization", "Bearer "+token)
		rr := httptest.NewRecorder()
		s.Handler().ServeHTTP(rr, req)
		return rr
	}
	access := x.iss.bearer(authUserGrant("employee", "aiwiki-access"))
	if rr := mcp(access, "wiki_read", `{"id":"`+a.ID+`","max_chars":20}`); !strings.Contains(rr.Body.String(), "confidentia") {
		t.Fatalf("MCP cannot read own article %s", rr.Body.String())
	}
	other := x.iss.bearer(authUserGrant("other", "aiwiki-access"))
	for _, tool := range []string{"wiki_search", "wiki_search_sections"} {
		if rr := mcp(other, tool, `{"q":"confidential"}`); strings.Contains(rr.Body.String(), a.ID) || strings.Contains(rr.Body.String(), "Corporate secret") {
			t.Fatalf("%s leaked: %s", tool, rr.Body.String())
		}
	}
	if rr := mcp(other, "wiki_read", `{"id":"`+a.ID+`","max_chars":10,"revision":1}`); strings.Contains(rr.Body.String(), "confidential") {
		t.Fatal("bounded history leaked")
	}
	// Revocation goes through the actual admin endpoint, including a sole owner.
	adminCookie := authCookie(x.login(authUserGrant("sysadmin", "aiwiki-access", "aiwiki-admin")), x.a.sessionCookie)
	var adminIdentity struct {
		CSRF string `json:"csrf_token"`
	}
	json.Unmarshal(call("GET", "/api/me", "", "", adminCookie).Body.Bytes(), &adminIdentity)
	if rr := call("GET", "/api/articles/"+a.ID, "", "", adminCookie); rr.Code != 404 {
		t.Fatal("system admin read private content")
	}
	rr = call("PATCH", "/api/admin/users/"+identity.User.ID, `{"disabled":true}`, adminIdentity.CSRF, adminCookie)
	if rr.Code != 200 {
		t.Fatalf("disable owner %d %s", rr.Code, rr.Body.String())
	}
	if rr := mcp(access, "wiki_read", `{"id":"`+a.ID+`"}`); rr.Code != 403 {
		t.Fatalf("disabled bearer %d %s", rr.Code, rr.Body.String())
	}
	if rr := call("GET", "/api/articles/"+a.ID, "", "", cookie); rr.Code != 403 {
		t.Fatalf("disabled browser %d", rr.Code)
	}
}
