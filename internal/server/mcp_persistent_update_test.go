package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestMCPPersistentUpdateScope(t *testing.T) {
	e := newPersistentEnv(t)
	alice, aliceID := e.user("alice")
	bob, bobID := e.user("bob")
	e.seed(aliceID, bobID)
	// seed: pa, pb, pe editor; pr reader; pn nothing; parch archived; pbob is bob's.
	id, token := e.mintToken(alice, `{"name":"agent","project_ids":["pa"],"mode":"read"}`)
	audits := func() int {
		return e.count(`SELECT count(*) FROM security_audit WHERE action='mcp_credential.update' AND resource_id=?`, id)
	}

	// Projects and mode change in place; the same secret picks them up at once.
	rec := e.call(alice, persistentCredentialR, "PATCH", `{"project_ids":["pe","pr"],"mode":"read"}`, id)
	e.expect(rec, 200)
	out := persistentJSON(t, rec)
	if out["id"] != id || out["mode"] != nil || out["name"] != "agent" || strings.Contains(rec.Body.String(), "token") {
		t.Fatalf("update response: %s", rec.Body)
	}
	p := e.mustAuth(token)
	if mcpScopeHasWrite(p.MCPScope) || len(p.MCPScope) != 2 || p.MCPScope[0].ID != "pe" || p.MCPScope[1].ID != "pr" {
		t.Fatalf("credential does not use the new scope: %+v", p)
	}
	if mcpScopedTool(&p, "wiki_write") {
		t.Fatal("read mode must not permit writes")
	}
	e.expect(e.call(alice, persistentCredentialR, "PATCH", `{"project_ids":["pa","pe"],"mode":"write"}`, id), 200)
	if p = e.mustAuth(token); !mcpScopeHasWrite(p.MCPScope) || !mcpScopedTool(&p, "wiki_write") {
		t.Fatalf("write mode not applied: %+v", p)
	}
	if n := audits(); n != 2 {
		t.Fatalf("each update is audited: %d", n)
	}
	if !strings.Contains(e.text(`SELECT details::text FROM security_audit WHERE action='mcp_credential.update' AND resource_id=? ORDER BY id DESC LIMIT 1`, id), `"old_scope"`) {
		t.Fatal("audit must record the previous scope")
	}

	// The new scope is checked against the owner's current rights, like issuance.
	before := audits()
	for _, body := range []string{
		`{"project_ids":["pr"],"mode":"write"}`,      // reader cannot write
		`{"project_ids":["pa","pr"],"mode":"write"}`, // one weak project spoils the set
		`{"project_ids":["pn"],"mode":"read"}`,       // no rights
		`{"project_ids":["parch"],"mode":"read"}`,    // archived
		`{"project_ids":["pbob"],"mode":"read"}`,     // someone else's project
		`{"project_ids":["missing"],"mode":"read"}`,  // unknown
	} {
		e.expect(e.call(alice, persistentCredentialR, "PATCH", body, id), http.StatusBadRequest)
	}
	for _, body := range []string{
		`{"project_ids":[],"mode":"read"}`,
		`{"project_ids":["pa"],"mode":"admin"}`,
		`{"project_ids":["pa"]}`,
		`{"project_ids":["pa","pa"],"mode":"read"}`,
		`{"project_ids":["pa"],"mode":"read","expires_at":"2099-01-01T00:00:00Z"}`, // expiry is not editable
		`{"project_ids":["pa"],"mode":"read","name":"renamed"}`,
		`not json`,
	} {
		e.expect(e.call(alice, persistentCredentialR, "PATCH", body, id), http.StatusBadRequest)
	}
	if audits() != before {
		t.Fatal("refused updates must not change or audit anything")
	}
	if p = e.mustAuth(token); !mcpScopeHasWrite(p.MCPScope) || len(p.MCPScope) != 2 || p.MCPScope[0].ID != "pa" {
		t.Fatalf("refused updates changed the scope: %+v", p)
	}

	// Only the owner, only live credentials.
	e.expect(e.call(bob, persistentCredentialR, "PATCH", `{"project_ids":["pbob"],"mode":"read"}`, id), http.StatusNotFound)
	e.expect(e.call(alice, persistentCredentialR, "PATCH", `{"project_ids":["pa"],"mode":"read"}`, "pat_missing"), http.StatusNotFound)
	short, _ := e.mintToken(alice, `{"name":"short","project_ids":["pa"],"ttl_seconds":60}`)
	e.clk.advance(time.Minute)
	e.expect(e.call(alice, persistentCredentialR, "PATCH", `{"project_ids":["pb"],"mode":"read"}`, short), http.StatusNotFound)
	e.expect(e.call(alice, persistentCredentialR, "DELETE", "", id), 204)
	e.expect(e.call(alice, persistentCredentialR, "PATCH", `{"project_ids":["pb"],"mode":"read"}`, id), http.StatusNotFound)
	e.mustFail(token, http.StatusUnauthorized)

	// A disabled owner cannot edit.
	id2, _ := e.mintToken(alice, `{"name":"n","project_ids":["pa"]}`)
	e.exec(`UPDATE users SET disabled=1 WHERE id=?`, aliceID)
	e.expect(e.call(alice, persistentCredentialR, "PATCH", `{"project_ids":["pb"],"mode":"read"}`, id2), http.StatusForbidden)
	e.exec(`UPDATE users SET disabled=0 WHERE id=?`, aliceID)
}

func TestMCPPersistentUpdateOnlyThroughCheckedFunction(t *testing.T) {
	e := newPersistentEnv(t)
	alice, aliceID := e.user("alice")
	_, bobID := e.user("bob")
	e.seed(aliceID, bobID)
	id, _ := e.mintToken(alice, `{"name":"n","project_ids":["pa"]}`)

	// The runtime role (alice's browser-scoped connection) cannot widen a
	// scope directly: only the checked function may change projects/mode.
	scope, done, err := e.root.DB.scoped(context.Background(), aliceID)
	if err != nil {
		t.Fatal(err)
	}
	defer done()
	for _, q := range []string{
		`UPDATE mcp_credentials SET scope='[{"type":"project","id":"pn","mode":"read"}]' WHERE id=?`,
		`UPDATE mcp_credentials SET mode='write' WHERE id=?`,
	} {
		if _, err := scope.Exec(q, id); err == nil || !strings.Contains(err.Error(), "immutable") {
			t.Fatalf("runtime role widened a scope with %q: %v", q, err)
		}
	}
	// The function enforces the owner's current rights itself.
	call := func(scopeText string) error {
		_, err := scope.Exec(`SELECT corporate_mcp_credential_set_scope(?,?::jsonb)`, id, scopeText)
		return err
	}
	el := func(t, id, m string) string { return `{"type":"` + t + `","id":"` + id + `","mode":"` + m + `"}` }
	for _, c := range []string{
		`[` + el("project", "pn", "read") + `]`,                                     // no rights
		`[` + el("project", "pr", "write") + `]`,                                    // reader cannot write
		`[` + el("project", "parch", "read") + `]`,                                  // archived
		`[` + el("project", "pbob", "read") + `]`,                                   // someone else's
		`[` + el("project", "pa", "read") + `,` + el("project", "pa", "read") + `]`, // duplicate
		`[]`,                                     // empty
		`[` + el("project", "pa", "admin") + `]`, // bad mode
		`[` + el("team", "pa", "read") + `]`,     // bad type
		`["pa"]`,                                 // not an object
		`[` + el("space", "nospace", "read") + `]`, // unknown space
		`[` + el("space", "s1", "write") + `]`,     // not editor on the space root
	} {
		if err := call(c); err == nil {
			t.Fatalf("function accepted %s", c)
		}
	}
	if err := call(`[` + el("project", "pe", "read") + `,` + el("project", "pr", "read") + `]`); err != nil {
		t.Fatalf("allowed scope refused: %v", err)
	}
	if got := e.text(`SELECT scope::text FROM mcp_credentials WHERE id=?`, id); !strings.Contains(got, `"id": "pe"`) || !strings.Contains(got, `"id": "pr"`) {
		t.Fatalf("scope not stored: %s", got)
	}
	// Someone else's key, and a revoked key, are refused.
	bobScope, bobDone, err := e.root.DB.scoped(context.Background(), bobID)
	if err != nil {
		t.Fatal(err)
	}
	defer bobDone()
	if _, err := bobScope.Exec(`SELECT corporate_mcp_credential_set_scope(?,?::jsonb)`, id, `[`+el("project", "pbob", "read")+`]`); err == nil {
		t.Fatal("another user changed alice's credential")
	}
	e.expect(e.call(alice, persistentCredentialR, "DELETE", "", id), 204)
	if err := call(`[` + el("project", "pa", "read") + `]`); err == nil {
		t.Fatal("revoked credential changed")
	}
	// An MCP-scoped session (a key itself) can never change scopes.
	id2, _ := e.mintToken(alice, `{"name":"n2","project_ids":["pa"]}`)
	mcp, mcpDone, err := e.root.DB.scopedMCP(context.Background(), aliceID, projScope("write", "pa"))
	if err != nil {
		t.Fatal(err)
	}
	defer mcpDone()
	if _, err := mcp.Exec(`SELECT corporate_mcp_credential_set_scope(?,?::jsonb)`, id2, `[`+el("project", "pa", "write")+`]`); err == nil {
		t.Fatal("an MCP session changed a credential scope")
	}
}

func TestMCPPersistentUpdateTriggerKeepsIdentityImmutable(t *testing.T) {
	e := newPersistentEnv(t)
	alice, aliceID := e.user("alice")
	_, bobID := e.user("bob")
	e.seed(aliceID, bobID)
	id, _ := e.mintToken(alice, `{"name":"n","project_ids":["pa"]}`)
	// Even the migration owner cannot rewrite identity, secret or times.
	for _, q := range []string{
		`UPDATE mcp_credentials SET owner_id='` + bobID + `' WHERE id=?`,
		`UPDATE mcp_credentials SET secret_hash=repeat('f',64) WHERE id=?`,
		`UPDATE mcp_credentials SET expires_at=expires_at+interval '1 day' WHERE id=?`,
		`UPDATE mcp_credentials SET name='x' WHERE id=?`,
		`UPDATE mcp_credentials SET service_account_id='svc_x' WHERE id=?`,
	} {
		if _, err := e.admin.Exec(q, id); err == nil || !strings.Contains(err.Error(), "immutable") {
			t.Fatalf("%s must be refused: %v", q, err)
		}
	}
	// The table owner (migrations, the checked function) may change scope of a
	// live credential, never of a revoked one; revocation stays final.
	e.exec(`UPDATE mcp_credentials SET scope='[{"type":"project","id":"pb","mode":"write"}]' WHERE id=?`, id)
	e.exec(`UPDATE mcp_credentials SET revoked_at=now() WHERE id=?`, id)
	if _, err := e.admin.Exec(`UPDATE mcp_credentials SET scope='[{"type":"project","id":"pa","mode":"read"}]' WHERE id=?`, id); err == nil {
		t.Fatal("a revoked credential's scope must stay frozen")
	}
	if _, err := e.admin.Exec(`UPDATE mcp_credentials SET revoked_at=NULL WHERE id=?`, id); err == nil {
		t.Fatal("revocation must stay final")
	}
}

func TestMCPPersistentScopeElementsAPI(t *testing.T) {
	e := newPersistentEnv(t)
	alice, aliceID := e.user("alice")
	_, bobID := e.user("bob")
	e.seed(aliceID, bobID)
	// Contract: scope body with a space element; legacy body still works; both is refused.
	e.seedExec(`INSERT INTO memberships(resource_type,resource_id,subject_type,subject_id,role) VALUES('space','s1','user',?,'reader')`, aliceID)
	out := e.mint(alice, `{"name":"sp","scope":[{"type":"space","id":"s1","mode":"read"},{"type":"project","id":"pe","mode":"write"}]}`, http.StatusCreated)
	sc, _ := json.Marshal(out["scope"])
	if !strings.Contains(string(sc), `"type":"space"`) || !strings.Contains(string(sc), `"space_id":"s1"`) || out["project_ids"] != nil || out["mode"] != nil {
		t.Fatalf("scope response: %s", sc)
	}
	e.mint(alice, `{"name":"both","scope":[{"type":"project","id":"pe","mode":"read"}],"project_ids":["pe"]}`, http.StatusBadRequest)
	e.mint(alice, `{"name":"bad","scope":[{"type":"team","id":"pe","mode":"read"}]}`, http.StatusBadRequest)
	e.mint(alice, `{"name":"bad","scope":[]}`, http.StatusBadRequest)
	var many []string
	for i := 0; i < 21; i++ {
		many = append(many, fmt.Sprintf(`{"type":"project","id":"p%d","mode":"read"}`, i))
	}
	rec := e.call(alice, persistentCredentials, "POST", `{"name":"many","scope":[`+strings.Join(many, ",")+`]}`, "")
	e.expect(rec, http.StatusBadRequest)
	if !strings.Contains(rec.Body.String(), "too many scope elements") {
		t.Fatalf("message: %s", rec.Body)
	}
	rec = e.call(alice, persistentCredentials, "POST", `{"name":"x","scope":[{"type":"project","id":"nonexistent","mode":"read"}]}`, "")
	e.expect(rec, http.StatusBadRequest)
	if !strings.Contains(rec.Body.String(), "scope access denied") {
		t.Fatalf("message: %s", rec.Body)
	}
	id, _ := out["id"].(string)
	rec = e.call(alice, persistentCredentialR, "PATCH", `{"scope":[{"type":"space","id":"s1","mode":"read"}]}`, id)
	e.expect(rec, http.StatusOK)
	// Reading general never needs rights; writing the general space does.
	e.mint(alice, `{"name":"g","scope":[{"type":"space","id":"general","mode":"read"}]}`, http.StatusCreated)
	rec = e.call(alice, persistentCredentials, "GET", "", "")
	if !strings.Contains(rec.Body.String(), `"scope"`) || strings.Contains(rec.Body.String(), `"project_ids"`) {
		t.Fatalf("list: %s", rec.Body)
	}
}
