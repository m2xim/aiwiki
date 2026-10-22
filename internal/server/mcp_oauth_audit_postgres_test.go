package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Needs AIWIKI_TEST_POSTGRES_OWNER / AIWIKI_TEST_POSTGRES (skipped otherwise).
func TestMCPOAuthRequestAuditPostgres(t *testing.T) {
	owner, root := corporateFixture(t)
	if err := root.DB.validateRuntimeSchema(); err != nil {
		t.Fatal(err)
	}
	x := newMCPOAuthHarness(t)
	x.a.users = authSQLUsers{db: root.DB}
	root.auth = x.a
	g := oauthResourceGrant("audit-user")
	g.claims["azp"] = "audit-client"
	token := x.iss.bearer(g)
	userID := authUserID(x.iss.srv.URL, "audit-user")
	rpc := func(method string, params any) *httptest.ResponseRecorder {
		b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
		r := httptest.NewRequest("POST", "http://127.0.0.1:8090/mcp", strings.NewReader(string(b)))
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		root.Handler().ServeHTTP(w, r)
		return w
	}
	count := func() int {
		var n int
		if err := owner.DB.QueryRow(`SELECT count(*) FROM security_audit WHERE action='mcp.oauth.request'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if w := rpc("tools/call", map[string]any{"name": "wiki_search", "arguments": map[string]any{"limit": 1}}); w.Code != 200 {
		t.Fatalf("status=%d %s", w.Code, w.Body)
	}
	var actor, rtype, rid, details string
	if err := owner.DB.QueryRow(`SELECT actor_id,resource_type,resource_id,details::text FROM security_audit WHERE action='mcp.oauth.request'`).Scan(&actor, &rtype, &rid, &details); err != nil {
		t.Fatalf("audit row missing: %v", err)
	}
	var d map[string]string
	if actor != userID || rtype != "mcp_oauth_client" || rid != "audit-client" || json.Unmarshal([]byte(details), &d) != nil || d["method"] != "tools/call" || d["tool"] != "wiki_search" || len(d) != 2 {
		t.Fatalf("audit row wrong: %s %s %s %s", actor, rtype, rid, details)
	}
	var leaked int
	if err := owner.DB.QueryRow(`SELECT count(*) FROM security_audit WHERE position(? in details::text)>0 OR position(? in resource_id)>0 OR position(? in actor_id)>0`, token, token, token).Scan(&leaked); err != nil || leaked != 0 {
		t.Fatalf("token leaked into audit: %d %v", leaked, err)
	}
	// Notifications do no work and are not audited.
	before := count()
	notif := httptest.NewRequest("POST", "http://127.0.0.1:8090/mcp", strings.NewReader(`{"jsonrpc":"2.0","method":"notifications/initialized"}`))
	notif.Header.Set("Authorization", "Bearer "+token)
	nw := httptest.NewRecorder()
	root.Handler().ServeHTTP(nw, notif)
	if nw.Code != http.StatusAccepted || count() != before {
		t.Fatalf("notification: %d audited=%v", nw.Code, count() != before)
	}
	// The audit function is hardened: PUBLIC cannot execute it and it refuses bad input.
	var publicExec bool
	if err := owner.DB.QueryRow(`SELECT EXISTS(SELECT 1 FROM pg_proc p, aclexplode(COALESCE(p.proacl,acldefault('f',p.proowner))) a WHERE p.oid='public.corporate_mcp_oauth_audit(text,text,text)'::regprocedure AND a.grantee=0)`).Scan(&publicExec); err != nil || publicExec {
		t.Fatalf("PUBLIC can execute the audit function: %v %v", publicExec, err)
	}
	scoped := corporateAs(t, root, userID)
	for _, bad := range [][]string{{"", "m", "t"}, {strings.Repeat("c", 101), "m", "t"}, {"c", strings.Repeat("m", 101), "t"}, {"c", "m", strings.Repeat("t", 101)}} {
		if _, err := scoped.DB.Exec(`SELECT corporate_mcp_oauth_audit(?,?,?)`, bad[0], bad[1], bad[2]); err == nil {
			t.Fatalf("invalid metadata accepted: %v", len(bad[0]))
		}
	}
	if _, err := root.DB.Exec(`SELECT corporate_mcp_oauth_audit('c','m','t')`); err == nil {
		t.Fatal("audit accepted without app.user_id")
	}
	if _, err := owner.DB.Exec(`UPDATE users SET disabled=1 WHERE id=?`, userID); err != nil {
		t.Fatal(err)
	}
	if _, err := scoped.DB.Exec(`SELECT corporate_mcp_oauth_audit('c','m','t')`); err == nil {
		t.Fatal("audit accepted for a disabled user")
	}
	if _, err := owner.DB.Exec(`UPDATE users SET disabled=0 WHERE id=?`, userID); err != nil {
		t.Fatal(err)
	}
	// Fail closed: without the audit function the method does not run.
	if _, err := owner.DB.Exec(`DROP FUNCTION corporate_mcp_oauth_audit(text,text,text)`); err != nil {
		t.Fatal(err)
	}
	before = count()
	w := rpc("tools/call", map[string]any{"name": "wiki_search", "arguments": map[string]any{"limit": 1}})
	if w.Code != 503 || !strings.Contains(w.Body.String(), "audit unavailable") || strings.Contains(w.Body.String(), `"result"`) || count() != before {
		t.Fatalf("not fail-closed: %d %s", w.Code, w.Body)
	}
	if err := root.DB.validateRuntimeSchema(); err == nil {
		t.Fatal("startup guard accepted a schema without the OAuth audit function")
	}
}
