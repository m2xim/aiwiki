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

func TestMCPPersistentEndToEnd(t *testing.T) {
	owner, root := corporateFixture(t)
	x := newAuthHarness(t, nil)
	x.a.users = authSQLUsers{db: root.DB}
	x.a.persistent = &mcpPersistentAccess{db: root.DB, now: time.Now}
	root.auth = x.a
	cookie := authCookie(x.login(authUserGrant("persistent-e2e", "aiwiki-access", "aiwiki-admin")), x.a.sessionCookie)
	call := func(method, path, body, csrf, bearer string, c *http.Cookie) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "http://127.0.0.1:8090"+path, strings.NewReader(body))
		if c != nil {
			r.AddCookie(c)
		}
		if csrf != "" {
			r.Header.Set("X-CSRF-Token", csrf)
		}
		if bearer != "" {
			r.Header.Set("Authorization", "Bearer "+bearer)
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
	if err := json.Unmarshal(call("GET", "/api/me", "", "", "", cookie).Body.Bytes(), &me); err != nil {
		t.Fatal(err)
	}
	mustJSON := func(w *httptest.ResponseRecorder, code int, out any) {
		t.Helper()
		if w.Code != code {
			t.Fatalf("status=%d want=%d body=%s", w.Code, code, w.Body)
		}
		if out != nil {
			if err := json.Unmarshal(w.Body.Bytes(), out); err != nil {
				t.Fatal(err)
			}
		}
	}
	var space, other Space
	mustJSON(call("POST", "/api/corporate/spaces", `{"name":"PAT fixture"}`, me.CSRF, "", cookie), 201, &space)
	mustJSON(call("POST", "/api/corporate/spaces", `{"name":"PAT sibling"}`, me.CSRF, "", cookie), 201, &other)
	var project, sibling AgentProjectForTest
	mustJSON(call("POST", "/api/agent-projects", `{"name":"Selected","visibility":"private","spaces":[{"space_id":"`+space.ID+`","mode":"write"}]}`, me.CSRF, "", cookie), 201, &project)
	mustJSON(call("POST", "/api/agent-projects", `{"name":"Sibling","visibility":"private","spaces":[{"space_id":"`+other.ID+`","mode":"write"}]}`, me.CSRF, "", cookie), 201, &sibling)
	var account mcpServiceAccount
	mustJSON(call("POST", "/api/mcp/service-accounts", `{"name":"Background reviewer"}`, me.CSRF, "", cookie), 201, &account)
	var issued struct {
		mcpCredential
		Token string `json:"token"`
	}
	mustJSON(call("POST", "/api/mcp/credentials", `{"name":"Scoped writer","projects":["`+project.ID+`"],"service_account_id":"`+account.ID+`"}`, me.CSRF, "", cookie), 201, &issued)
	if issued.Token == "" {
		t.Fatal("missing secret")
	}
	rpcAt := func(path, method string, params any) *httptest.ResponseRecorder {
		b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
		return call("POST", path, string(b), "", issued.Token, nil)
	}
	rpc := func(method string, params any) *httptest.ResponseRecorder {
		return rpcAt("/mcp?project="+project.ID, method, params)
	}
	// Without a project only the introductory tools exist.
	if w := rpcAt("/mcp", "tools/list", map[string]any{}); w.Code != 200 || strings.Contains(w.Body.String(), `"wiki_write"`) || strings.Contains(w.Body.String(), `"wiki_search"`) || !strings.Contains(w.Body.String(), `"wiki_projects"`) {
		t.Fatalf("introspection tools: %d %s", w.Code, w.Body)
	}
	if w := rpcAt("/mcp", "tools/call", map[string]any{"name": "wiki_search", "arguments": map[string]any{}}); w.Code != 200 || !strings.Contains(w.Body.String(), `"isError":true`) {
		t.Fatalf("introspection search: %d %s", w.Code, w.Body)
	}
	// A project outside the key's list is refused, and the answer does not say whether it exists.
	for _, path := range []string{"/mcp?project=" + sibling.ID, "/mcp?project=nonexistent", "/mcp?project=a/b", "/mcp?project=" + project.ID + "&project=" + sibling.ID} {
		if w := rpcAt(path, "tools/list", map[string]any{}); w.Code != 403 && w.Code != 400 || strings.Contains(w.Body.String(), "Sibling") {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body)
		}
	}
	if w := rpcAt("/mcp?project="+sibling.ID, "ping", nil); w.Code != 403 || !strings.Contains(w.Body.String(), "project is not available for this key") {
		t.Fatalf("foreign project: %d %s", w.Code, w.Body)
	}
	w := rpc("tools/list", map[string]any{})
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"wiki_edit"`) {
		t.Fatalf("writer discovery failed: %d %s", w.Code, w.Body)
	}
	// The agent learns where it may write from bootstrap alone: the project, its spaces and effective rights.
	w = rpc("tools/call", map[string]any{"name": "wiki_bootstrap", "arguments": map[string]any{}})
	var boot struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &boot) != nil || len(boot.Result.Content) != 1 {
		t.Fatalf("bootstrap failed: %d %s", w.Code, w.Body)
	}
	var bootOut struct {
		Project AgentProjectForTest `json:"project"`
	}
	if err := json.Unmarshal([]byte(boot.Result.Content[0].Text), &bootOut); err != nil {
		t.Fatal(err)
	}
	if bootOut.Project.ID != project.ID || len(bootOut.Project.Spaces) != 1 || bootOut.Project.Spaces[0].SpaceID != space.ID || bootOut.Project.Spaces[0].Effective != "write" || strings.Contains(boot.Result.Content[0].Text, "PAT sibling") {
		t.Fatalf("bootstrap must show exactly its project: %s", boot.Result.Content[0].Text)
	}
	w = rpc("tools/call", map[string]any{"name": "wiki_write", "arguments": map[string]any{"article": map[string]any{"title": "Service authorship", "space_id": space.ID, "body": "# Scoped\ncontent", "author": "spoof"}}})
	if w.Code != 200 || strings.Contains(w.Body.String(), `"isError":true`) {
		t.Fatalf("write failed: %d %s", w.Code, w.Body)
	}
	// Changing the project applies to the very next request: the space leaves it and writes stop.
	var rowID string
	for _, r := range project.Spaces {
		rowID = r.RowID
	}
	mustJSON(call("PATCH", "/api/agent-projects/"+project.ID, `{"spaces":[{"row_id":"`+rowID+`","mode":"read"}]}`, me.CSRF, "", cookie), 200, nil)
	w = rpc("tools/call", map[string]any{"name": "wiki_write", "arguments": map[string]any{"article": map[string]any{"title": "Now read only", "space_id": space.ID}}})
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"isError":true`) {
		t.Fatalf("a project switched to read still wrote: %d %s", w.Code, w.Body)
	}
	mustJSON(call("PATCH", "/api/agent-projects/"+project.ID, `{"spaces":[{"row_id":"`+rowID+`","mode":"write"}]}`, me.CSRF, "", cookie), 200, nil)
	scoped, done, err := owner.DB.scoped(context.Background(), me.User.ID)
	if err != nil {
		t.Fatal(err)
	}
	var author, articleID string
	err = scoped.QueryRow(`SELECT id,author FROM articles WHERE title='Service authorship'`).Scan(&articleID, &author)
	if err != nil || author != account.ID {
		t.Fatalf("author=%q want=%q err=%v", author, account.ID, err)
	}
	done()
	// Updating an existing article rewrites its search index row: a write key
	// must be able to edit, not only create.
	w = rpc("tools/call", map[string]any{"name": "wiki_edit", "arguments": map[string]any{"id": articleID, "base_revision": 1, "op": "append", "text": "\n\nAppended by key"}})
	if w.Code != 200 || strings.Contains(w.Body.String(), `"isError":true`) || !strings.Contains(w.Body.String(), `\"revision\":2`) {
		t.Fatalf("edit through write key failed: %d %s", w.Code, w.Body)
	}
	w = rpc("tools/call", map[string]any{"name": "wiki_search", "arguments": map[string]any{"q": "Appended"}})
	if w.Code != 200 || !strings.Contains(w.Body.String(), articleID) {
		t.Fatalf("edited text is not searchable: %d %s", w.Code, w.Body)
	}
	w = rpc("tools/call", map[string]any{"name": "wiki_write", "arguments": map[string]any{"article": map[string]any{"title": "Out of scope", "space_id": other.ID}}})
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"isError":true`) {
		t.Fatalf("sibling write allowed: %d %s", w.Code, w.Body)
	}
	// Native secrets never authenticate browser APIs, even with an otherwise valid key.
	if w = call("GET", "/api/mcp/credentials", "", "", issued.Token, nil); w.Code != 401 {
		t.Fatalf("native credential authenticated browser path: %d", w.Code)
	}
	var actors int
	if err = owner.DB.QueryRow(`SELECT count(*) FROM security_audit WHERE actor_id=? AND action='mcp.credential.request' AND details::jsonb->>'owner_id'=?`, account.ID, me.User.ID).Scan(&actors); err != nil || actors < 8 { // every request of the project connection is audited
		t.Fatalf("audit actors=%d err=%v", actors, err)
	}
	// Local admission: no browser login for longer than the window stops the key.
	if _, err = owner.DB.Exec(`UPDATE users SET last_login_at=? WHERE id=?`, time.Now().UTC().Add(-mcpPersistentLoginWindow-time.Minute), me.User.ID); err != nil {
		t.Fatal(err)
	}
	if w = rpc("ping", map[string]any{}); w.Code != 403 {
		t.Fatalf("stale login still allowed: %d %s", w.Code, w.Body)
	}
	// A fresh browser login re-activates the same credential.
	x.login(authUserGrant("persistent-e2e", "aiwiki-access", "aiwiki-admin"))
	if w = rpc("ping", map[string]any{}); w.Code != 200 {
		t.Fatalf("fresh login did not re-admit: %d %s", w.Code, w.Body)
	}
	// A locally disabled owner is refused.
	if _, err = owner.DB.Exec(`UPDATE users SET disabled=1 WHERE id=?`, me.User.ID); err != nil {
		t.Fatal(err)
	}
	if w = rpc("ping", map[string]any{}); w.Code != 403 {
		t.Fatalf("disabled owner still allowed: %d %s", w.Code, w.Body)
	}
	if _, err = owner.DB.Exec(`UPDATE users SET disabled=0 WHERE id=?`, me.User.ID); err != nil {
		t.Fatal(err)
	}
	mustJSON(call("DELETE", "/api/mcp/service-accounts/"+account.ID, "", me.CSRF, "", cookie), 204, nil)
	w = rpc("ping", map[string]any{})
	if w.Code != 403 && w.Code != 401 {
		t.Fatalf("disabled service still allowed: %d %s", w.Code, w.Body)
	}
}
