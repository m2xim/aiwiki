package server

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestMCPPersistentUpdateProjects(t *testing.T) {
	e := newPersistentEnv(t)
	alice, aliceID := e.user("alice")
	bob, bobID := e.user("bob")
	e.seed(aliceID, bobID)
	// seed: pa, pb, pe, pr are alice's; pn and pbob are bob's private; pub is bob's public.
	id, token := e.mintToken(alice, `{"name":"agent","projects":["pa"],"read_only":true}`)
	audits := func() int {
		return e.count(`SELECT count(*) FROM security_audit WHERE action='mcp_credential.update' AND resource_id=?`, id)
	}

	// Projects and the switch change in place; the same secret picks them up at once.
	rec := e.call(alice, persistentCredentialR, "PATCH", `{"projects":["pe","pr"],"read_only":false}`, id)
	e.expect(rec, 200)
	out := persistentJSON(t, rec)
	if out["id"] != id || out["name"] != "agent" || out["read_only"] != false || strings.Contains(rec.Body.String(), "token") {
		t.Fatalf("update response: %s", rec.Body)
	}
	p := e.mustAuth(token)
	if p.MCPReadOnly || len(p.MCPKeyProjects) != 2 || p.MCPKeyProjects[0] != "pe" || p.MCPKeyProjects[1] != "pr" {
		t.Fatalf("credential does not use the new projects: %+v", p)
	}
	e.expect(e.call(alice, persistentCredentialR, "PATCH", `{"read_only":true}`, id), 200)
	if p = e.mustAuth(token); !p.MCPReadOnly || len(p.MCPKeyProjects) != 2 {
		t.Fatalf("read_only not applied or projects lost: %+v", p)
	}
	if n := audits(); n != 2 {
		t.Fatalf("each update is audited: %d", n)
	}
	if !strings.Contains(e.text(`SELECT details::text FROM security_audit WHERE action='mcp_credential.update' AND resource_id=? ORDER BY id DESC LIMIT 1`, id), `"old_projects"`) {
		t.Fatal("audit must record the previous projects")
	}

	// New projects must be visible to the owner; refusals change and audit nothing.
	before := audits()
	for _, body := range []string{
		`{"projects":["pn"]}`,      // bob's private project
		`{"projects":["pa","pn"]}`, // one hidden project spoils the set
		`{"projects":["pbob"]}`,    // bob's private project
		`{"projects":["missing"]}`, // unknown
		`{"projects":["sa"]}`,      // a space is not a project
	} {
		e.expect(e.call(alice, persistentCredentialR, "PATCH", body, id), http.StatusBadRequest)
	}
	for _, body := range []string{
		`{"projects":[]}`, `{"projects":["pa","pa"]}`, `{}`, `{"read_only":"x"}`,
		`{"projects":["pa"],"expires_at":"2099-01-01T00:00:00Z"}`, // expiry is not editable
		`{"projects":["pa"],"name":"renamed"}`,
		`{"scope":[]}`, `{"project_ids":["pa"]}`, `{"mode":"write"}`,
		`not json`,
	} {
		e.expect(e.call(alice, persistentCredentialR, "PATCH", body, id), http.StatusBadRequest)
	}
	if audits() != before {
		t.Fatal("refused updates must not change or audit anything")
	}
	if p = e.mustAuth(token); len(p.MCPKeyProjects) != 2 || p.MCPKeyProjects[0] != "pe" {
		t.Fatalf("refused updates changed the projects: %+v", p)
	}
	// A project already in the key stays even once it is no longer visible, so the owner can still trim the list.
	pubKey, _ := e.mintToken(alice, `{"name":"pubkey","projects":["pub","pa"]}`)
	e.exec(`UPDATE agent_projects SET visibility='private' WHERE id='pub'`)
	e.expect(e.call(alice, persistentCredentialR, "PATCH", `{"projects":["pub","pa","pb"]}`, pubKey), 200)
	e.expect(e.call(alice, persistentCredentialR, "PATCH", `{"projects":["pa"]}`, pubKey), 200)
	e.expect(e.call(alice, persistentCredentialR, "PATCH", `{"projects":["pa","pub"]}`, pubKey), 400) // not back in

	// Only the owner, only live credentials.
	e.expect(e.call(bob, persistentCredentialR, "PATCH", `{"projects":["pbob"]}`, id), http.StatusNotFound)
	e.expect(e.call(alice, persistentCredentialR, "PATCH", `{"projects":["pa"]}`, "pat_missing"), http.StatusNotFound)
	short, _ := e.mintToken(alice, `{"name":"short","projects":["pa"],"ttl_seconds":60}`)
	e.clk.advance(time.Minute)
	e.expect(e.call(alice, persistentCredentialR, "PATCH", `{"projects":["pb"]}`, short), http.StatusNotFound)
	e.expect(e.call(alice, persistentCredentialR, "DELETE", "", id), 204)
	e.expect(e.call(alice, persistentCredentialR, "PATCH", `{"projects":["pb"]}`, id), http.StatusNotFound)
	e.mustFail(token, http.StatusUnauthorized)

	// A disabled owner cannot edit.
	id2, _ := e.mintToken(alice, `{"name":"n","projects":["pa"]}`)
	e.exec(`UPDATE users SET disabled=1 WHERE id=?`, aliceID)
	e.expect(e.call(alice, persistentCredentialR, "PATCH", `{"projects":["pb"]}`, id2), http.StatusForbidden)
	e.exec(`UPDATE users SET disabled=0 WHERE id=?`, aliceID)
}

func TestMCPPersistentUpdateOnlyThroughCheckedFunction(t *testing.T) {
	e := newPersistentEnv(t)
	alice, aliceID := e.user("alice")
	_, bobID := e.user("bob")
	e.seed(aliceID, bobID)
	id, _ := e.mintToken(alice, `{"name":"n","projects":["pa"]}`)

	// The runtime role (alice's browser-scoped connection) cannot change the authority directly.
	scope, done, err := e.root.DB.scoped(context.Background(), aliceID)
	if err != nil {
		t.Fatal(err)
	}
	defer done()
	for _, q := range []string{
		`UPDATE mcp_credentials SET agent_project_ids='["pn"]'::jsonb WHERE id=?`,
		`UPDATE mcp_credentials SET read_only=true WHERE id=?`,
		`UPDATE mcp_credentials SET scope='[{"type":"space","id":"sn","mode":"write"}]'::jsonb WHERE id=?`,
		`UPDATE mcp_credentials SET mode='write' WHERE id=?`,
	} {
		if _, err := scope.Exec(q, id); err == nil || !strings.Contains(err.Error(), "immutable") {
			t.Fatalf("runtime role changed the authority with %q: %v", q, err)
		}
	}
	// The function enforces visibility itself.
	call := func(projects string, readOnly bool) error {
		_, err := scope.Exec(`SELECT corporate_mcp_credential_set_projects(?,?::jsonb,?)`, id, projects, readOnly)
		return err
	}
	for _, c := range []string{`["pn"]`, `["pbob"]`, `["missing"]`, `["pa","pa"]`, `[]`, `["pa",1]`, `[""]`, `"pa"`, `null`} {
		if err := call(c, false); err == nil {
			t.Fatalf("function accepted %s", c)
		}
	}
	if err := call(`["pe","pub"]`, true); err != nil {
		t.Fatalf("allowed projects refused: %v", err)
	}
	if got := e.text(`SELECT agent_project_ids::text||read_only::text FROM mcp_credentials WHERE id=?`, id); got != `["pe", "pub"]true` {
		t.Fatalf("projects not stored: %s", got)
	}
	// Someone else's key, and a revoked key, are refused.
	bobScope, bobDone, err := e.root.DB.scoped(context.Background(), bobID)
	if err != nil {
		t.Fatal(err)
	}
	defer bobDone()
	if _, err := bobScope.Exec(`SELECT corporate_mcp_credential_set_projects(?,?::jsonb,false)`, id, `["pbob"]`); err == nil {
		t.Fatal("another user changed alice's credential")
	}
	e.expect(e.call(alice, persistentCredentialR, "DELETE", "", id), 204)
	if err := call(`["pa"]`, false); err == nil {
		t.Fatal("revoked credential changed")
	}
	// A session scoped to a project (a key itself) can never change a key.
	id2, _ := e.mintToken(alice, `{"name":"n2","projects":["pa"]}`)
	mcp, mcpDone, err := e.root.DB.scopedMCP(context.Background(), aliceID, spaceScope("write", "sa"))
	if err != nil {
		t.Fatal(err)
	}
	defer mcpDone()
	if _, err := mcp.Exec(`SELECT corporate_mcp_credential_set_projects(?,?::jsonb,false)`, id2, `["pa"]`); err == nil {
		t.Fatal("an MCP session changed a credential")
	}
}

func TestMCPPersistentUpdateTriggerKeepsIdentityImmutable(t *testing.T) {
	e := newPersistentEnv(t)
	alice, aliceID := e.user("alice")
	_, bobID := e.user("bob")
	e.seed(aliceID, bobID)
	id, _ := e.mintToken(alice, `{"name":"n","projects":["pa"]}`)
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
	// The table owner (migrations, the checked function) may change the projects of a live credential,
	// never of a revoked one; revocation stays final.
	e.exec(`UPDATE mcp_credentials SET agent_project_ids='["pb"]'::jsonb WHERE id=?`, id)
	e.exec(`UPDATE mcp_credentials SET revoked_at=now() WHERE id=?`, id)
	if _, err := e.admin.Exec(`UPDATE mcp_credentials SET agent_project_ids='["pa"]'::jsonb WHERE id=?`, id); err == nil {
		t.Fatal("a revoked credential's projects must stay frozen")
	}
	if _, err := e.admin.Exec(`UPDATE mcp_credentials SET revoked_at=NULL WHERE id=?`, id); err == nil {
		t.Fatal("revocation must stay final")
	}
}
