package server

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCorporateDisableLastOwnerRevokesAndReportsOrphan(t *testing.T) {
	_, root := corporateFixture(t)
	db := root.DB
	ctx := context.Background()
	var err error
	suffix := randomSuffix()
	adminID, ownerID, spaceID := "acl-admin-"+suffix, "acl-owner-"+suffix, "acl-space-"+suffix
	issuer := "https://example.org/" + suffix
	if _, err = db.Exec(`INSERT INTO users(id,issuer,subject,name,admin,disabled) VALUES(?,?,?,? ,1,0)`, adminID, issuer, "admin", "Admin"); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO users(id,issuer,subject,name,admin,disabled) VALUES(?,?,?,? ,0,0)`, ownerID, issuer, "owner", "Owner"); err != nil {
		_, _ = db.Exec(`DELETE FROM users WHERE id=?`, adminID)
		t.Fatal(err)
	}
	cleanup := func() {
		_, _ = db.Exec(`UPDATE users SET disabled=0 WHERE id=?`, ownerID)
		ownerDB, done, scopeErr := db.scoped(ctx, ownerID)
		if scopeErr == nil {
			_, _ = ownerDB.Exec(`DELETE FROM spaces WHERE id=?`, spaceID)
			_, _ = ownerDB.Exec(`DELETE FROM memberships WHERE resource_type='space' AND resource_id=?`, spaceID)
			done()
		}
		_, _ = db.Exec(`DELETE FROM corporate_space_state WHERE space_id=?`, spaceID)
		_, _ = db.Exec(`DELETE FROM security_audit WHERE actor_id IN (?,?) OR resource_id=?`, adminID, ownerID, ownerID)
		_, _ = db.Exec(`DELETE FROM users WHERE id IN (?,?)`, adminID, ownerID)
	}
	defer cleanup()

	ownerDB, ownerCleanup, err := db.scoped(ctx, ownerID)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := ownerDB.Begin()
	if err != nil {
		ownerCleanup()
		t.Fatal(err)
	}
	if _, err = tx.Exec(`INSERT INTO spaces(id,name,description,owner_id,personal) VALUES(?, 'Private test space','',?,1)`, spaceID, ownerID); err == nil {
		_, err = tx.Exec(`INSERT INTO memberships(resource_type,resource_id,subject_type,subject_id,role) VALUES('space',?,'user',?,'owner')`, spaceID, ownerID)
	}
	if err == nil {
		err = tx.Commit()
	} else {
		_ = tx.Rollback()
	}
	ownerCleanup()
	if err != nil {
		t.Fatal(err)
	}

	adminDB, adminCleanup, err := db.scoped(ctx, adminID)
	if err != nil {
		t.Fatal(err)
	}
	defer adminCleanup()
	api := &Server{DB: adminDB, principal: &Principal{ID: adminID, Admin: true}}
	request := httptest.NewRequest("PATCH", "/api/admin/users/"+ownerID, strings.NewReader(`{"disabled":true}`))
	request.SetPathValue("id", ownerID)
	response := httptest.NewRecorder()
	api.corporateUserDisable(response, request)
	if response.Code != 200 {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var body struct {
		Disabled          bool `json:"disabled"`
		OrphanedResources int  `json:"orphaned_resources"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if !body.Disabled || body.OrphanedResources != 1 {
		t.Fatalf("unexpected response: %+v", body)
	}
	var disabled, grants int
	if err = db.QueryRow(`SELECT disabled FROM users WHERE id=?`, ownerID).Scan(&disabled); err != nil {
		t.Fatal(err)
	}
	if err = db.QueryRow(`SELECT count(*) FROM memberships WHERE resource_type='space' AND resource_id=? AND subject_id=? AND role='owner'`, spaceID, ownerID).Scan(&grants); err != nil {
		t.Fatal(err)
	}
	if disabled != 1 || grants != 1 {
		t.Fatalf("disable must revoke eligibility and preserve the owner grant: disabled=%d grants=%d", disabled, grants)
	}
}
