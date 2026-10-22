package server

import (
	"encoding/json"
	"strings"
	"testing"
)

type userAccessResponse struct {
	UserID    string                        `json:"user_id"`
	Resources []corporateUserAccessResource `json:"resources"`
}

func userAccessGet(t *testing.T, s *Server, target string) (int, userAccessResponse, string) {
	t.Helper()
	rr := corporateHTTP(s, "GET", "/api/admin/users/"+target+"/access", "")
	var out userAccessResponse
	if rr.Code == 200 {
		if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode %s: %v", rr.Body.String(), err)
		}
	}
	return rr.Code, out, rr.Body.String()
}

func userAccessProject(t *testing.T, s *Server, space, parent, name string, inherit bool) string {
	t.Helper()
	raw, _ := json.Marshal(map[string]any{"space_id": space, "parent_id": parent, "name": name, "inherit_acl": inherit})
	rr := corporateHTTP(s, "POST", "/api/projects", string(raw))
	if rr.Code != 201 {
		t.Fatalf("project %s: %d %s", name, rr.Code, rr.Body.String())
	}
	var p struct {
		ID string `json:"id"`
	}
	json.Unmarshal(rr.Body.Bytes(), &p)
	return p.ID
}

func userAccessGrant(t *testing.T, owner *Server, kind, id, subjectType, subject, role string) {
	t.Helper()
	if _, err := owner.DB.Exec(`INSERT INTO memberships(resource_type,resource_id,subject_type,subject_id,role) VALUES(?,?,?,?,?) ON CONFLICT (resource_type,resource_id,subject_type,subject_id) DO UPDATE SET role=EXCLUDED.role`, kind, id, subjectType, subject, role); err != nil {
		t.Fatal(err)
	}
}

func userAccessFind(items []corporateUserAccessResource, kind, id string) (corporateUserAccessResource, bool) {
	for _, item := range items {
		if item.ResourceType == kind && item.ResourceID == id {
			return item, true
		}
	}
	return corporateUserAccessResource{}, false
}

func TestCorporateUserAccessAuthorizationAndErrors(t *testing.T) {
	_, root := corporateFixture(t)
	admin := corporateAs(t, root, "admin")
	alice := corporateAs(t, root, "alice")
	bob := corporateAs(t, root, "bob")

	for _, caller := range []*Server{alice, bob} {
		for _, target := range []string{caller.principal.ID, "admin", "alice"} {
			if code, _, body := userAccessGet(t, caller, target); code != 403 {
				t.Fatalf("non-admin %s read %s: %d %s", caller.principal.ID, target, code, body)
			}
		}
	}
	if code, _, body := userAccessGet(t, admin, "no-such-user"); code != 404 {
		t.Fatalf("unknown target: %d %s", code, body)
	}
	code, got, body := userAccessGet(t, admin, "bob")
	if code != 200 || got.UserID != "bob" {
		t.Fatalf("admin read %d %s", code, body)
	}
	if !strings.Contains(body, `"resources":[]`) {
		t.Fatalf("empty result must be [] not null: %s", body)
	}
	if code, got, body = userAccessGet(t, admin, "admin"); code != 200 || got.UserID != "admin" || len(got.Resources) != 0 {
		t.Fatalf("admin self read %d %s", code, body)
	}
}

func TestCorporateUserAccessEnumeratesOnlyManagedResources(t *testing.T) {
	owner, root := corporateFixture(t)
	admin := corporateAs(t, root, "admin")
	alice := corporateAs(t, root, "alice")

	// A system administrator without a role cannot enumerate private resources.
	space := corporateCreateSpace(t, alice)
	userAccessGrant(t, owner, "space", space, "user", "bob", "reader")
	for _, target := range []string{"alice", "bob"} {
		if code, got, body := userAccessGet(t, admin, target); code != 200 || len(got.Resources) != 0 {
			t.Fatalf("admin without role enumerated %s: %d %s", target, code, body)
		}
	}

	// Read-only actor access is not enough.
	userAccessGrant(t, owner, "space", space, "user", "admin", "editor")
	if _, got, body := userAccessGet(t, admin, "bob"); len(got.Resources) != 0 {
		t.Fatalf("editor actor enumerated: %s", body)
	}

	// Managing actor sees the space with the target's role and flags.
	userAccessGrant(t, owner, "space", space, "user", "admin", "manager")
	_, got, body := userAccessGet(t, admin, "bob")
	item, ok := userAccessFind(got.Resources, "space", space)
	if !ok || len(got.Resources) != 1 || item.Role != "reader" || !item.Personal || item.Archived || item.Name != "Private Alice" {
		t.Fatalf("manager view: %s", body)
	}
	_, got, body = userAccessGet(t, admin, "alice")
	if item, ok = userAccessFind(got.Resources, "space", space); !ok || item.Role != "owner" {
		t.Fatalf("owner target: %s", body)
	}

	// Response carries no ACL sources or other metadata.
	var raw map[string]json.RawMessage
	_ = json.Unmarshal([]byte(body), &raw)
	if len(raw) != 2 {
		t.Fatalf("unexpected top-level fields: %s", body)
	}
	var resources []map[string]json.RawMessage
	if err := json.Unmarshal(raw["resources"], &resources); err != nil {
		t.Fatal(err)
	}
	allowed := map[string]bool{"resource_type": true, "resource_id": true, "name": true, "role": true, "archived": true, "personal": true}
	for _, resource := range resources {
		for key := range resource {
			if !allowed[key] {
				t.Fatalf("unexpected metadata field %q", key)
			}
		}
	}

	// Revocation of the target is reflected on the next request.
	if _, err := owner.DB.Exec(`DELETE FROM memberships WHERE resource_id=? AND subject_id='bob'`, space); err != nil {
		t.Fatal(err)
	}
	if _, got, body = userAccessGet(t, admin, "bob"); len(got.Resources) != 0 {
		t.Fatalf("revoked target still listed: %s", body)
	}
	// So is revocation of the actor.
	if _, err := owner.DB.Exec(`DELETE FROM memberships WHERE resource_id=? AND subject_id='admin'`, space); err != nil {
		t.Fatal(err)
	}
	if _, got, body = userAccessGet(t, admin, "alice"); len(got.Resources) != 0 {
		t.Fatalf("revoked actor still enumerates: %s", body)
	}
}

func TestCorporateUserAccessTeamInheritedAndArchivedProjects(t *testing.T) {
	owner, root := corporateFixture(t)
	admin := corporateAs(t, root, "admin")
	alice := corporateAs(t, root, "alice")

	space := corporateCreateSpace(t, alice)
	parent := userAccessProject(t, alice, space, "", "Parent project", true)
	child := userAccessProject(t, alice, space, parent, "Child project", true)
	if _, err := owner.DB.Exec(`INSERT INTO teams(id,name) VALUES('team-a','Team A')`); err != nil {
		t.Fatal(err)
	}
	if _, err := owner.DB.Exec(`INSERT INTO team_members(team_id,user_id) VALUES('team-a','bob')`); err != nil {
		t.Fatal(err)
	}
	userAccessGrant(t, owner, "space", space, "team", "team-a", "commenter")
	userAccessGrant(t, owner, "project", child, "user", "bob", "editor")
	userAccessGrant(t, owner, "space", space, "user", "admin", "owner")

	_, got, body := userAccessGet(t, admin, "bob")
	if len(got.Resources) != 3 {
		t.Fatalf("want space, parent, child: %s", body)
	}
	want := map[string]string{space: "commenter", parent: "commenter", child: "editor"}
	for id, role := range want {
		kind := "project"
		if id == space {
			kind = "space"
		}
		if item, ok := userAccessFind(got.Resources, kind, id); !ok || item.Role != role || item.Archived {
			t.Fatalf("%s %s: %s", kind, id, body)
		}
	}
	for i := 1; i < len(got.Resources); i++ {
		a, b := got.Resources[i-1], got.Resources[i]
		if a.ResourceType > b.ResourceType {
			t.Fatalf("not ordered by resource_type: %s", body)
		}
	}

	// Archive flag is reported; roles still apply to archived resources.
	if rr := corporateHTTP(alice, "PATCH", "/api/projects/"+parent, `{"archived":true}`); rr.Code != 200 {
		t.Fatalf("archive %d %s", rr.Code, rr.Body.String())
	}
	_, got, body = userAccessGet(t, admin, "bob")
	if item, ok := userAccessFind(got.Resources, "project", parent); !ok || !item.Archived || item.Personal {
		t.Fatalf("archived metadata: %s", body)
	}
	if item, ok := userAccessFind(got.Resources, "project", child); !ok || item.Archived {
		t.Fatalf("child archived flag must be its own: %s", body)
	}

	// Disabled target has no effective role anywhere.
	if _, err := owner.DB.Exec(`UPDATE users SET disabled=1 WHERE id='bob'`); err != nil {
		t.Fatal(err)
	}
	code, got, body := userAccessGet(t, admin, "bob")
	if code != 200 || len(got.Resources) != 0 || !strings.Contains(body, `"resources":[]`) {
		t.Fatalf("disabled target: %d %s", code, body)
	}

	// A disabled administrator is rejected.
	if _, err := owner.DB.Exec(`UPDATE users SET disabled=1 WHERE id='admin'`); err != nil {
		t.Fatal(err)
	}
	if code, _, body = userAccessGet(t, admin, "alice"); code != 403 {
		t.Fatalf("disabled admin: %d %s", code, body)
	}
}

func TestCorporateUserAccessChildWithoutParentMetadata(t *testing.T) {
	owner, root := corporateFixture(t)
	admin := corporateAs(t, root, "admin")
	alice := corporateAs(t, root, "alice")

	space := corporateCreateSpace(t, alice)
	hidden := userAccessProject(t, alice, space, "", "Hidden parent title", true)
	child := userAccessProject(t, alice, space, hidden, "Directly shared child", false)
	other := userAccessProject(t, alice, space, hidden, "Unmanaged sibling", false)
	userAccessGrant(t, owner, "project", child, "user", "admin", "manager")
	userAccessGrant(t, owner, "project", child, "user", "bob", "reader")
	userAccessGrant(t, owner, "project", other, "user", "admin", "reader")
	userAccessGrant(t, owner, "project", other, "user", "bob", "reader")

	code, got, body := userAccessGet(t, admin, "bob")
	if code != 200 || len(got.Resources) != 1 {
		t.Fatalf("want only the managed child: %d %s", code, body)
	}
	item := got.Resources[0]
	if item.ResourceType != "project" || item.ResourceID != child || item.Name != "Directly shared child" || item.Role != "reader" {
		t.Fatalf("child entry: %s", body)
	}
	for _, secret := range []string{space, hidden, other, "Hidden parent title", "Unmanaged sibling", "Private Alice"} {
		if strings.Contains(body, secret) {
			t.Fatalf("response leaks %q: %s", secret, body)
		}
	}
	// Target with no access to the managed child gets nothing.
	if _, got, body = userAccessGet(t, admin, "reader"); len(got.Resources) != 0 {
		t.Fatalf("unrelated target listed: %s", body)
	}
	// The target's access to a resource the actor cannot manage stays hidden.
	if _, got, body = userAccessGet(t, admin, "alice"); len(got.Resources) != 1 || got.Resources[0].ResourceID != child {
		t.Fatalf("alice view must be limited to managed resources: %s", body)
	}
}
