package server

import (
	"encoding/json"
	"testing"
)

func TestCorporateProjectManagementRolesAcrossArchive(t *testing.T) {
	_, root := corporateFixture(t)
	alice, bob := corporateAs(t, root, "alice"), corporateAs(t, root, "bob")
	space := corporateCreateSpace(t, alice)
	r := corporateHTTP(alice, "POST", "/api/projects", `{"space_id":"`+space+`","name":"Restorable project"}`)
	if r.Code != 201 {
		t.Fatalf("create %d %s", r.Code, r.Body)
	}
	var project corporateProject
	if err := json.Unmarshal(r.Body.Bytes(), &project); err != nil {
		t.Fatal(err)
	}
	r = corporateHTTP(alice, "PUT", "/api/access?resource_type=project&resource_id="+project.ID, `{"grant":{"subject_type":"user","subject_id":"bob","role":"editor"}}`)
	if r.Code != 200 {
		t.Fatalf("grant %d %s", r.Code, r.Body)
	}
	verify := func(user *Server, role string, archived bool) {
		t.Helper()
		r := corporateHTTP(user, "GET", "/api/projects", "")
		var rows []corporateProject
		if r.Code != 200 {
			t.Fatal(r.Code)
		}
		if err := json.Unmarshal(r.Body.Bytes(), &rows); err != nil {
			t.Fatal(err)
		}
		for _, p := range rows {
			if p.ID == project.ID {
				if p.EffectiveRole != role || (p.ArchivedAt != "") != archived {
					t.Fatalf("role/state mismatch role=%s archived=%v", p.EffectiveRole, p.ArchivedAt != "")
				}
				return
			}
		}
		t.Fatal("project missing")
	}
	verify(alice, "owner", false)
	verify(bob, "editor", false)
	if r := corporateHTTP(bob, "PATCH", "/api/projects/"+project.ID, `{"archived":true}`); r.Code == 200 {
		t.Fatal("editor archived project")
	}
	if r := corporateHTTP(alice, "PATCH", "/api/projects/"+project.ID, `{"archived":true}`); r.Code != 200 {
		t.Fatalf("archive %d %s", r.Code, r.Body)
	}
	verify(alice, "owner", true)
	if r := corporateHTTP(alice, "PATCH", "/api/projects/"+project.ID, `{"archived":false}`); r.Code != 200 {
		t.Fatalf("restore %d %s", r.Code, r.Body)
	}
	verify(alice, "owner", false)
}
