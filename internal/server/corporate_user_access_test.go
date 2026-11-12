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
	if !ok || len(got.Resources) != 1 || item.Role != "reader" || item.Personal || item.Archived || item.Name != "Private Alice" {
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
