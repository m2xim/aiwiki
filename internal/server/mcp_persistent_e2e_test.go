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

type leadAdmissionCheck func(context.Context, string, string) error

func (f leadAdmissionCheck) Check(c context.Context, i, s string) error { return f(c, i, s) }

func TestMCPPersistentEndToEnd(t *testing.T) {
	owner, root := corporateFixture(t)
	x := newAuthHarness(t, nil)
	x.a.users = authSQLUsers{db: root.DB}
	admitted := true
	checks := 0
	x.a.persistent = &mcpPersistentAccess{db: root.DB, now: time.Now, admission: leadAdmissionCheck(func(context.Context, string, string) error {
		checks++
		if !admitted {
			return errMCPAdmissionDenied
		}
		return nil
	})}
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
	var space Space
	mustJSON(call("POST", "/api/corporate/spaces", `{"name":"PAT fixture","personal":true}`, me.CSRF, "", cookie), 201, &space)
	var project, sibling corporateProject
	mustJSON(call("POST", "/api/projects", `{"space_id":"`+space.ID+`","name":"Selected"}`, me.CSRF, "", cookie), 201, &project)
	mustJSON(call("POST", "/api/projects", `{"space_id":"`+space.ID+`","name":"Sibling"}`, me.CSRF, "", cookie), 201, &sibling)
	var account mcpServiceAccount
	mustJSON(call("POST", "/api/mcp/service-accounts", `{"name":"Background reviewer"}`, me.CSRF, "", cookie), 201, &account)
	var issued struct {
		mcpCredential
		Token string `json:"token"`
	}
	mustJSON(call("POST", "/api/mcp/credentials", `{"name":"Scoped writer","mode":"write","project_ids":["`+project.ID+`"],"service_account_id":"`+account.ID+`"}`, me.CSRF, "", cookie), 201, &issued)
	if issued.Token == "" {
		t.Fatal("missing secret")
	}
	before := checks
	rpc := func(method string, params any) *httptest.ResponseRecorder {
		b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
		return call("POST", "/mcp", string(b), "", issued.Token, nil)
	}
	w := rpc("tools/list", map[string]any{})
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"wiki_edit"`) {
		t.Fatalf("writer discovery failed: %d %s", w.Code, w.Body)
	}
	w = rpc("tools/call", map[string]any{"name": "wiki_write", "arguments": map[string]any{"article": map[string]any{"title": "Service authorship", "space_id": space.ID, "project_id": project.ID, "body": "# Scoped\ncontent", "author": "spoof"}}})
	if w.Code != 200 || strings.Contains(w.Body.String(), `"isError":true`) {
		t.Fatalf("write failed: %d %s", w.Code, w.Body)
	}
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
	w = rpc("tools/call", map[string]any{"name": "wiki_write", "arguments": map[string]any{"article": map[string]any{"title": "Out of scope", "space_id": space.ID, "project_id": sibling.ID}}})
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"isError":true`) {
		t.Fatalf("sibling write allowed: %d %s", w.Code, w.Body)
	}
	if checks-before != 5 { // list, write, edit, search, sibling write
		t.Fatalf("admission called %d times, want5", checks-before)
	}
	// Native secrets never authenticate browser APIs, even with an otherwise valid key.
	if w = call("GET", "/api/mcp/credentials", "", "", issued.Token, nil); w.Code != 401 {
		t.Fatalf("native credential authenticated browser path: %d", w.Code)
	}
	var actors int
	if err = owner.DB.QueryRow(`SELECT count(*) FROM security_audit WHERE actor_id=? AND action='mcp.credential.request' AND details::jsonb->>'owner_id'=?`, account.ID, me.User.ID).Scan(&actors); err != nil || actors != 5 {
		t.Fatalf("audit actors=%d err=%v", actors, err)
	}
	admitted = false
	w = rpc("ping", map[string]any{})
	if w.Code != 403 && w.Code != 401 {
		t.Fatalf("removed admission still allowed: %d %s", w.Code, w.Body)
	}
	admitted = true
	mustJSON(call("DELETE", "/api/mcp/service-accounts/"+account.ID, "", me.CSRF, "", cookie), 204, nil)
	w = rpc("ping", map[string]any{})
	if w.Code != 403 && w.Code != 401 {
		t.Fatalf("disabled service still allowed: %d %s", w.Code, w.Body)
	}
}
