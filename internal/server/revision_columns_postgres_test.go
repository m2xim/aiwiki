package server

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// oldAuditSQL is the pre-column audit query (snapshot JSON, OR across the join),
// kept as the reference the candidate-based query must reproduce exactly.
const oldAuditSQL = `
	SELECT r.id FROM revisions r
	LEFT JOIN revisions p ON p.article_id=r.article_id AND p.revision=r.revision-1
	WHERE ((aiwiki_json_object(r.snapshot)->>'space_id')=? OR (aiwiki_json_object(p.snapshot)->>'space_id')=?)
	  AND (?=0 OR r.author=?)
	ORDER BY r.id DESC LIMIT ? OFFSET ?`

func TestPostgresRevisionColumnsAuditMatchesOldQuery(t *testing.T) {
	owner, root := corporateFixture(t)
	alice := corporateAs(t, root, "alice")
	bob := corporateAs(t, root, "bob")
	spaces := map[string]string{}
	for _, name := range []string{"one", "two", "hidden"} {
		r := corporateHTTP(alice, "POST", "/api/corporate/spaces", `{"name":"Audit `+name+`","personal":true}`)
		if r.Code != 201 {
			t.Fatalf("space %s: %d %s", name, r.Code, r.Body.String())
		}
		var v map[string]any
		json.Unmarshal(r.Body.Bytes(), &v)
		spaces[name] = v["id"].(string)
	}
	for _, name := range []string{"one", "two"} {
		if _, err := owner.DB.Exec(`INSERT INTO memberships(resource_type,resource_id,subject_type,subject_id,role) VALUES('space',?,'user','bob','editor')`, spaces[name]); err != nil {
			t.Fatal(err)
		}
	}
	type art struct {
		ID    string
		Rev   int
		Space string
	}
	var move func(u *Server, a *art, space string)
	put := func(u *Server, a *art, space, body string) {
		t.Helper()
		if space != a.Space {
			move(u, a, space)
		}
		r := corporateHTTP(u, "PUT", "/api/articles/"+a.ID, fmt.Sprintf(`{"space_id":%q,"title":"T","body":%q,"base_revision":%d}`, space, body, a.Rev))
		if r.Code != 200 {
			t.Fatalf("put %s: %d %s", space, r.Code, r.Body.String())
		}
		a.Rev++
	}
	move = func(u *Server, a *art, space string) {
		t.Helper()
		req := articleMove(a.ID, a.Rev, space, "", "")
		raw, _ := json.Marshal(req)
		pv := corporateHTTP(u, "POST", "/api/moves/preview", string(raw))
		if pv.Code != 200 {
			t.Fatalf("preview: %d %s", pv.Code, pv.Body.String())
		}
		var p movePreview
		json.Unmarshal(pv.Body.Bytes(), &p)
		raw, _ = json.Marshal(withFingerprint(req, p.Fingerprint))
		if r := corporateHTTP(u, "POST", "/api/moves/apply", string(raw)); r.Code != 200 {
			t.Fatalf("apply: %d %s", r.Code, r.Body.String())
		}
		a.Rev++
		a.Space = space
	}
	create := func(u *Server, space string) *art {
		t.Helper()
		r := corporateHTTP(u, "POST", "/api/articles", fmt.Sprintf(`{"space_id":%q,"title":"T","body":"b"}`, space))
		if r.Code != 201 {
			t.Fatalf("create: %d %s", r.Code, r.Body.String())
		}
		var a Article
		json.Unmarshal(r.Body.Bytes(), &a)
		return &art{a.ID, a.Revision, space}
	}
	one, two := spaces["one"], spaces["two"]
	a1, a2, a3 := create(alice, one), create(bob, two), create(alice, one)
	put(bob, a1, one, "edit-b")
	put(alice, a1, two, "moved out") // one -> two
	put(bob, a1, two, "edit in two") // stays in two
	put(alice, a2, one, "moved in")  // two -> one
	put(bob, a2, one, "edit in one")
	put(alice, a2, two, "back") // one -> two
	put(alice, a3, one, "edit a3")
	put(bob, a3, one, "edit a3 by bob")
	hidden := create(alice, spaces["hidden"])
	put(alice, hidden, spaces["hidden"], "secret edit")
	a4 := create(alice, spaces["hidden"])
	put(alice, a4, one, "moved from hidden")

	oldIDs := func(u *Server, space string, author *string, limit, offset int) []int64 {
		t.Helper()
		has := 0
		a := ""
		if author != nil {
			has, a = 1, *author
		}
		rows, err := u.DB.Query(oldAuditSQL, space, space, has, a, limit, offset)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var ids []int64
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				t.Fatal(err)
			}
			ids = append(ids, id)
		}
		return ids
	}
	// a4 moves from a space bob cannot read into one he can: its newest snapshots are
	// readable for bob, the older (hidden) one is not.
	var moveIn int64
	if err := alice.DB.QueryRow(`SELECT id FROM revisions WHERE article_id=? AND revision=2`, a4.ID).Scan(&moveIn); err != nil {
		t.Fatal(err)
	}
	compared := 0
	for _, user := range []*Server{alice, bob} {
		for _, space := range []string{one, two} {
			for _, au := range []*string{nil, ptr("alice"), ptr("bob"), ptr("nobody")} {
				for _, limit := range []int{1, 2, 3, 7, 50} {
					for _, offset := range []int{0, 1, 2, 5, 40} {
						path := fmt.Sprintf("/api/spaces/%s/audit?limit=%d&offset=%d", space, limit, offset)
						if au != nil {
							path += "&author=" + *au
						}
						want := oldIDs(user, space, au, limit, offset)
						r := corporateHTTP(user, "GET", path, "")
						hidesPrev := false
						for _, id := range want {
							hidesPrev = hidesPrev || user == bob && id == moveIn
						}
						if hidesPrev {
							// the previous snapshot is invisible to bob: same failure as with the old query
							if r.Code != 500 || !strings.Contains(r.Body.String(), "missing previous revision snapshot") {
								t.Errorf("bob %s: %d %s", path, r.Code, r.Body.String())
							}
							compared++
							continue
						}
						if r.Code != 200 {
							t.Fatalf("%s: %d %s", path, r.Code, r.Body.String())
						}
						var events []AuditEvent
						json.Unmarshal(r.Body.Bytes(), &events)
						got := make([]int64, 0, len(events))
						for _, e := range events {
							got = append(got, e.ID)
						}
						if fmt.Sprint(got) != fmt.Sprint(want) {
							t.Errorf("%s: new %v, old %v", path, got, want)
						}
						compared++
					}
				}
			}
		}
	}
	if compared != 2*2*4*5*5 {
		t.Fatalf("compared %d combinations", compared)
	}
	if full := oldIDs(alice, one, nil, 200, 0); len(full) < 8 {
		t.Fatalf("fixture too small: %v", full)
	}

	// Policy semantics: bob has no access to the hidden space.
	if r := corporateHTTP(bob, "GET", "/api/spaces/"+spaces["hidden"]+"/audit", ""); r.Code != 404 {
		t.Fatalf("hidden audit for bob: %d %s", r.Code, r.Body.String())
	}
	var n int
	if err := bob.DB.QueryRow(`SELECT count(*) FROM revisions WHERE space_id=?`, spaces["hidden"]).Scan(&n); err != nil || n != 0 {
		t.Fatalf("bob sees %d hidden revisions (err %v)", n, err)
	}
	if err := alice.DB.QueryRow(`SELECT count(*) FROM revisions WHERE space_id=?`, spaces["hidden"]).Scan(&n); err != nil || n != 3 {
		t.Fatalf("alice sees %d hidden revisions (err %v)", n, err)
	}

	// Backfill: wipe the columns as the owner, forget the migration and rerun it.
	for _, q := range []string{
		`ALTER TABLE revisions NO FORCE ROW LEVEL SECURITY`,
		`ALTER TABLE revisions ALTER COLUMN space_id DROP NOT NULL`,
		`UPDATE revisions SET space_id=NULL, project_id='x'`,
		`ALTER TABLE revisions FORCE ROW LEVEL SECURITY`,
		`DELETE FROM schema_migrations WHERE component='revision_columns'`,
	} {
		if _, err := owner.DB.Exec(q); err != nil {
			t.Fatal(q, err)
		}
	}
	if err := owner.migrateCorporate(); err != nil {
		t.Fatal(err)
	}
	if _, err := owner.DB.Exec(`ALTER TABLE revisions NO FORCE ROW LEVEL SECURITY`); err != nil {
		t.Fatal(err)
	}
	defer owner.DB.Exec(`ALTER TABLE revisions FORCE ROW LEVEL SECURITY`)
	var total, bad int
	if err := owner.DB.QueryRow(`SELECT count(*), count(*) FILTER (WHERE space_id IS DISTINCT FROM snapshot::jsonb->>'space_id' OR project_id IS DISTINCT FROM coalesce(snapshot::jsonb->>'project_id',''))
		FROM revisions`).Scan(&total, &bad); err != nil || total < 8 || bad != 0 {
		t.Fatalf("backfill: total %d bad %d err %v", total, bad, err)
	}
	// A snapshot without space_id must abort the migration.
	if _, err := owner.DB.Exec(`ALTER TABLE revisions ALTER COLUMN space_id DROP NOT NULL`); err != nil {
		t.Fatal(err)
	}
	if _, err := owner.DB.Exec(`ALTER TABLE revisions DISABLE TRIGGER revisions_fill_scope`); err != nil {
		t.Fatal(err)
	}
	if _, err := owner.DB.Exec(`DELETE FROM schema_migrations WHERE component='revision_columns'`); err != nil {
		t.Fatal(err)
	}
	// A snapshot with no space_id cannot be backfilled.
	if res, err := owner.DB.Exec(`UPDATE revisions SET snapshot='{}' WHERE id=(SELECT min(id) FROM revisions)`); err != nil {
		t.Fatal(err)
	} else if n, _ := res.RowsAffected(); n != 1 {
		t.Fatalf("snapshot update touched %d rows", n)
	}
	if err := owner.migrateRevisionColumns(context.Background()); err == nil || !strings.Contains(err.Error(), "without space_id") {
		t.Fatalf("migration accepted a revision without space_id: %v", err)
	}
}

func ptr(s string) *string { return &s }
