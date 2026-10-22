package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestMCPOAuthPostgresResourceBoundary(t *testing.T) {
	owner, root := corporateFixture(t)
	x := newMCPOAuthHarness(t)
	x.a.users = authSQLUsers{db: root.DB}
	root.auth = x.a
	cookie := authCookie(x.login(authUserGrant("oauth-alice", "aiwiki-access")), x.a.sessionCookie)
	if cookie == nil {
		t.Fatal("browser login failed")
	}
	bobCookie := authCookie(x.login(authUserGrant("oauth-bob", "aiwiki-access")), x.a.sessionCookie)
	if bobCookie == nil {
		t.Fatal("second browser login failed")
	}
	aliceID := authUserID(x.iss.srv.URL, "oauth-alice")
	bobID := authUserID(x.iss.srv.URL, "oauth-bob")
	alice, bob := corporateAs(t, root, aliceID), corporateAs(t, root, bobID)
	aSpace, bSpace := corporateCreateSpace(t, alice), corporateCreateSpace(t, bob)
	create := func(s *Server, space, title string) {
		t.Helper()
		w := corporateHTTP(s, "POST", "/api/articles", `{"space_id":"`+space+`","title":"`+title+`","body":"Search fixture"}`)
		if w.Code != 201 {
			t.Fatalf("seed status=%d", w.Code)
		}
	}
	create(alice, aSpace, "AliceOAuthVisible")
	create(bob, bSpace, "BobOAuthSecret")
	call := func(method, path, body, token, csrf string, c *http.Cookie) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "http://127.0.0.1:8090"+path, strings.NewReader(body))
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		if csrf != "" {
			r.Header.Set("X-CSRF-Token", csrf)
		}
		if c != nil {
			r.AddCookie(c)
		}
		r.Header.Set("X-Forwarded-Host", "attacker.example.org")
		r.Header.Set("Forwarded", `host=attacker.example.org;proto=https`)
		w := httptest.NewRecorder()
		root.Handler().ServeHTTP(w, r)
		return w
	}
	for _, path := range []string{"/.well-known/oauth-protected-resource", "/.well-known/oauth-protected-resource/mcp"} {
		w := call("GET", path, "", "", "", nil)
		var m mcpOAuthMetadata
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &m) != nil || m.Resource != oauthTestResource || len(w.Result().Cookies()) != 0 {
			t.Fatalf("public metadata invalid status=%d", w.Code)
		}
		if strings.Contains(w.Body.String(), "attacker.example.org") || strings.Contains(w.Body.String(), authTestSecret) {
			t.Fatal("metadata exposed request authority or secret")
		}
	}
	token := x.iss.bearer(oauthResourceGrant("oauth-alice"))
	rpc := func(token, method string, params any) *httptest.ResponseRecorder {
		b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
		return call("POST", "/mcp", string(b), token, "", nil)
	}
	w := rpc(token, "tools/call", map[string]any{"name": "wiki_search", "arguments": map[string]any{"limit": 100}})
	if w.Code != 200 || !strings.Contains(w.Body.String(), "AliceOAuthVisible") || strings.Contains(w.Body.String(), "BobOAuthSecret") {
		t.Fatalf("OAuth ACL search failed: %d %s", w.Code, w.Body)
	}
	legacy := x.iss.bearer(authUserGrant("oauth-alice", "aiwiki-access"))
	if w = rpc(legacy, "ping", nil); w.Code != 401 {
		t.Fatalf("legacy audience fallback: %d", w.Code)
	}
	var me struct {
		CSRF string `json:"csrf_token"`
	}
	w = call("GET", "/api/me", "", "", "", cookie)
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &me) != nil {
		t.Fatal("browser session lost")
	}
	// Native credentials coexist with discovery without inheriting its broad scope.
	p := corporateHTTP(alice, "POST", "/api/projects", `{"name":"Native subset","space_id":"`+aSpace+`"}`)
	var project corporateProject
	if p.Code != 201 || json.Unmarshal(p.Body.Bytes(), &project) != nil {
		t.Fatal("seed project failed")
	}
	x.a.persistent = &mcpPersistentAccess{db: root.DB, now: time.Now, admission: leadAdmissionCheck(func(context.Context, string, string) error { return nil })}
	for _, endpoint := range []string{"credentials", "delegations"} {
		w = call("POST", "/api/mcp/"+endpoint, `{"name":"Native read","project_ids":["`+project.ID+`"]}`, "", me.CSRF, cookie)
		var native struct {
			Token string `json:"token"`
		}
		if w.Code != 201 || json.Unmarshal(w.Body.Bytes(), &native) != nil || native.Token == "" {
			t.Fatalf("native mint failed: %d %s", w.Code, w.Body)
		}
		w = rpc(native.Token, "tools/list", nil)
		if w.Code != 200 || strings.Contains(w.Body.String(), `"wiki_write"`) {
			t.Fatalf("native scope escaped: %d %s", w.Code, w.Body)
		}
	}
	if _, err := owner.DB.Exec(`DELETE FROM memberships WHERE resource_type='space' AND resource_id=? AND subject_id=?`, aSpace, aliceID); err != nil {
		t.Fatal(err)
	}
	w = rpc(token, "tools/call", map[string]any{"name": "wiki_search", "arguments": map[string]any{"q": "AliceOAuthVisible"}})
	if w.Code != 200 || strings.Contains(w.Body.String(), "AliceOAuthVisible") {
		t.Fatal("removed ACL still visible")
	}
	if _, err := owner.DB.Exec(`UPDATE users SET disabled=1 WHERE id=?`, aliceID); err != nil {
		t.Fatal(err)
	}
	if w = rpc(token, "ping", nil); w.Code != 403 {
		t.Fatalf("disabled OAuth user status=%d", w.Code)
	}
}

func TestMCPOAuthTrustedModeHasNoDiscovery(t *testing.T) {
	s, err := Open(t.TempDir(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, path := range []string{"/.well-known/oauth-protected-resource", "/.well-known/oauth-protected-resource/mcp"} {
		r := httptest.NewRequest("GET", "http://127.0.0.1:8090"+path, nil)
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		if w.Code != 404 {
			t.Fatalf("trusted mode metadata status=%d", w.Code)
		}
	}
}
