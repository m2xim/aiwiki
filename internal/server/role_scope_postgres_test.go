package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"sort"
	"strings"
	"testing"
)

// corporate_role_scope() must reproduce corporate_effective_role (the reference) for
// every container. These tests build random fixtures, then compare the set function
// and the replaced SELECT policies with the reference evaluated row by row as the
// runtime role. Fixtures are written as the table owner inside one transaction that
// lifts FORCE RLS and the project triggers (which forbid cross-space parents) and puts
// both back before commit, so only the owner and app DSNs are needed.

type rsProject struct {
	id, space, parent string
	inherit           int
}
type rsMember struct{ rtype, rid, stype, sid, role string }
type rsArticle struct{ id, space, project string }
type rsRevision struct {
	article, space, project string
	rev                     int
}
type rsChild struct{ id, article string }
type rsCand struct{ space, project string }

type rsFixture struct {
	spaces, users, teams  []string
	disabled, archived    map[string]bool
	teamMembers           [][2]string
	projects              []rsProject
	members               []rsMember
	articles              []rsArticle
	revisions             []rsRevision
	comments, attachments []rsChild
	favorites             [][2]string // user, article
}

var rsRoles = []string{"owner", "manager", "editor", "commenter", "reader"}

func rsPick(r *rand.Rand, list []string, missing string, missingPercent int) string {
	if len(list) == 0 || r.Intn(100) < missingPercent {
		return missing
	}
	return list[r.Intn(len(list))]
}

func rsGenerate(seed int64, content bool) *rsFixture {
	r := rand.New(rand.NewSource(seed))
	f := &rsFixture{disabled: map[string]bool{}, archived: map[string]bool{}}
	for i, n := 0, 2+r.Intn(3); i < n; i++ {
		f.spaces = append(f.spaces, fmt.Sprintf("rs_sp%d", i))
	}
	for i, n := 0, 5+r.Intn(3); i < n; i++ {
		id := fmt.Sprintf("rs_u%d", i)
		f.users = append(f.users, id)
		f.disabled[id] = r.Intn(4) == 0
	}
	for i, n := 0, 2+r.Intn(3); i < n; i++ {
		id := fmt.Sprintf("rs_t%d", i)
		f.teams = append(f.teams, id)
		f.archived[id] = r.Intn(10) < 3
		for _, u := range f.users {
			if r.Intn(100) < 40 {
				f.teamMembers = append(f.teamMembers, [2]string{id, u})
			}
		}
	}
	nProj := 8 + r.Intn(8)
	ids := make([]string, nProj)
	for i := range ids {
		ids[i] = fmt.Sprintf("rs_p%d", i)
	}
	ids[3] = "rs_p3\"q\\ü" // quote, backslash and non-ASCII must survive the JSON scope list
	for i, id := range ids {
		sp := f.spaces[r.Intn(len(f.spaces))]
		parent := ""
		switch x := r.Intn(100); {
		case x < 40:
		case x < 75:
			var same []string
			for _, p := range f.projects {
				if p.space == sp {
					same = append(same, p.id)
				}
			}
			if len(same) > 0 {
				parent = same[r.Intn(len(same))]
			}
		case x < 83:
			parent = ids[r.Intn(nProj)] // possibly another space, possibly a later project
		case x < 88:
			parent = id
		case x < 93:
			parent = "rs_missing"
		default:
			parent = ids[(i+1+r.Intn(nProj))%nProj]
		}
		inherit := 1
		if r.Intn(4) == 0 {
			inherit = 0
		}
		f.projects = append(f.projects, rsProject{id, sp, parent, inherit})
	}
	if seed%5 == 1 { // an explicit two-project cycle inside one space, both inheriting
		for i := range f.projects {
			for j := i + 1; j < len(f.projects); j++ {
				if f.projects[i].space == f.projects[j].space {
					f.projects[i].parent, f.projects[j].parent = f.projects[j].id, f.projects[i].id
					f.projects[i].inherit, f.projects[j].inherit = 1, 1
					j, i = len(f.projects), len(f.projects)
				}
			}
		}
	}
	if seed%10 == 0 { // a 70-deep chain: the chain walk stops 64 steps above its start
		for i := 0; i < 70; i++ {
			parent := ""
			if i > 0 {
				parent = fmt.Sprintf("rs_c%d", i-1)
			}
			f.projects = append(f.projects, rsProject{fmt.Sprintf("rs_c%d", i), f.spaces[0], parent, 1})
		}
		for _, m := range []rsMember{
			{"project", "rs_c0", "user", f.users[0], "editor"},
			{"project", "rs_c64", "user", f.users[1], "reader"},
			{"project", "rs_c65", "user", f.users[2], "manager"},
			{"project", "rs_c69", "user", f.users[0], "reader"},
		} {
			f.members = append(f.members, m)
		}
	}
	if seed%4 == 2 { // schema allows a project with id ''; it must not turn into a space-level row
		f.projects = append(f.projects, rsProject{"", f.spaces[0], "", 1})
		f.members = append(f.members, rsMember{"project", "", "user", f.users[0], "owner"}, rsMember{"project", "", "team", f.teams[0], "owner"})
	}
	var projectIDs []string
	for _, p := range f.projects {
		projectIDs = append(projectIDs, p.id)
	}
	seen := map[rsMember]bool{}
	for _, m := range f.members {
		seen[rsMember{m.rtype, m.rid, m.stype, m.sid, ""}] = true
	}
	for n := 8 + r.Intn(18); n > 0; n-- {
		var m rsMember
		if r.Intn(100) < 40 {
			m.rtype, m.rid = "space", rsPick(r, f.spaces, "rs_nosp", 12)
		} else {
			m.rtype, m.rid = "project", rsPick(r, projectIDs, "rs_nopr", 10)
		}
		if r.Intn(100) < 60 {
			m.stype, m.sid = "user", rsPick(r, f.users, "rs_nouser", 8)
		} else {
			m.stype, m.sid = "team", rsPick(r, f.teams, "rs_noteam", 8)
		}
		m.role = rsRoles[r.Intn(len(rsRoles))]
		key := rsMember{m.rtype, m.rid, m.stype, m.sid, ""}
		if !seen[key] {
			seen[key] = true
			f.members = append(f.members, m)
		}
	}
	if !content {
		return f
	}
	// Content: one or two articles per container, including containers that do not
	// match (project of another space, unknown project), with revisions whose snapshot
	// container may differ from the article's current one.
	var containers [][2]string
	for _, s := range f.spaces {
		containers = append(containers, [2]string{s, ""})
	}
	for _, p := range f.projects {
		if strings.HasPrefix(p.id, "rs_c") && r.Intn(4) != 0 {
			continue
		}
		containers = append(containers, [2]string{p.space, p.id})
	}
	for i := 0; i < 3; i++ {
		p := f.projects[r.Intn(len(f.projects))]
		for _, s := range f.spaces {
			if s != p.space {
				containers = append(containers, [2]string{s, p.id})
				break
			}
		}
	}
	for _, s := range f.spaces[:2] {
		containers = append(containers, [2]string{s, "rs_nopr"})
	}
	n := 0
	for _, c := range containers {
		for k, cnt := 0, 1+r.Intn(2); k < cnt; k++ {
			if k == 1 && r.Intn(100) < 60 {
				continue
			}
			a := rsArticle{fmt.Sprintf("rs_a%d", n), c[0], c[1]}
			n++
			f.articles = append(f.articles, a)
			for rev, nRev := 1, 1+r.Intn(3); rev <= nRev; rev++ {
				snap := c
				if r.Intn(100) >= 65 {
					snap = containers[r.Intn(len(containers))]
				}
				f.revisions = append(f.revisions, rsRevision{a.id, snap[0], snap[1], rev})
			}
			for j, m := 0, r.Intn(3); j < m; j++ {
				f.comments = append(f.comments, rsChild{fmt.Sprintf("%s_c%d", a.id, j), a.id})
			}
			if r.Intn(2) == 0 {
				f.attachments = append(f.attachments, rsChild{a.id + "_f", a.id})
			}
			for _, u := range f.users {
				if r.Intn(100) < 35 {
					f.favorites = append(f.favorites, [2]string{u, a.id})
				}
			}
		}
	}
	return f
}

var rsForcedTables = []string{"spaces", "articles", "revisions", "comments", "attachments", "favorites", "article_fts"}

// rsLoad replaces all data with the fixture as the table owner.
func rsLoad(t *testing.T, owner *Server, f *rsFixture) {
	t.Helper()
	ctx := context.Background()
	tx, err := owner.DB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := tx.Exec(q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	for _, tbl := range rsForcedTables {
		exec(`ALTER TABLE ` + tbl + ` NO FORCE ROW LEVEL SECURITY`)
	}
	exec(`ALTER TABLE projects DISABLE TRIGGER USER`)
	exec(`TRUNCATE spaces, users, teams, memberships, corporate_space_state, article_fts CASCADE`)
	for _, s := range f.spaces {
		exec(`INSERT INTO spaces(id,name,description,owner_id,personal) VALUES(?,?, '', '',0)`, s, s)
	}
	for _, u := range f.users {
		exec(`INSERT INTO users(id,issuer,subject,name,admin,disabled) VALUES(?,'https://identity.example.org',?,?,0,?)`, u, u, u, boolInt(f.disabled[u]))
	}
	for _, tm := range f.teams {
		var archived any
		if f.archived[tm] {
			archived = "2026-01-01T00:00:00Z"
		}
		exec(`INSERT INTO teams(id,name,archived_at) VALUES(?,?,?)`, tm, tm, archived)
	}
	for _, m := range f.teamMembers {
		exec(`INSERT INTO team_members(team_id,user_id) VALUES(?,?)`, m[0], m[1])
	}
	for _, p := range f.projects {
		exec(`INSERT INTO projects(id,space_id,parent_id,name,description,inherit_acl) VALUES(?,?,?,?, '',?)`, p.id, p.space, p.parent, p.id, p.inherit)
	}
	for _, m := range f.members {
		exec(`INSERT INTO memberships(resource_type,resource_id,subject_type,subject_id,role) VALUES(?,?,?,?,?)`, m.rtype, m.rid, m.stype, m.sid, m.role)
	}
	for _, a := range f.articles {
		exec(`INSERT INTO articles(id,space_id,parent_id,project_id,title,summary,body,kind,status,pinned,tags,properties,revision,created_at,updated_at,author,deleted_at) VALUES(?,?, '', ?, ?, '', '', 'article','active',0,'[]','{}',1,'now','now','rs_u0',NULL)`, a.id, a.space, a.project, a.id)
		exec(`INSERT INTO article_fts(article_id,title) VALUES(?,?)`, a.id, a.id)
	}
	for _, rv := range f.revisions {
		snap, _ := json.Marshal(map[string]string{"space_id": rv.space, "project_id": rv.project})
		exec(`INSERT INTO revisions(article_id,revision,author,message,created_at,snapshot) VALUES(?,?, 'rs_u0','','now',?)`, rv.article, rv.rev, string(snap))
	}
	for _, c := range f.comments {
		exec(`INSERT INTO comments(id,article_id,anchor,body,author,created_at,resolved) VALUES(?,?, '', '', 'rs_u0','now',0)`, c.id, c.article)
	}
	for _, c := range f.attachments {
		exec(`INSERT INTO attachments(id,article_id,name,mime,size,stored_name,created_at) VALUES(?,?,'file','text/plain',1,?,'now')`, c.id, c.article, c.id)
	}
	for _, fv := range f.favorites {
		exec(`INSERT INTO favorites(user_id,article_id,created_at) VALUES(?,?,'now')`, fv[0], fv[1])
	}
	exec(`ALTER TABLE projects ENABLE TRIGGER USER`)
	for _, tbl := range rsForcedTables {
		exec(`ALTER TABLE ` + tbl + ` FORCE ROW LEVEL SECURITY`)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

// rsCandidates are all containers a policy can ask about: every space (and the ones
// only memberships mention), every project in its own space, in other spaces and in a
// missing space, and project ids that exist only in memberships.
func (f *rsFixture) candidates(r *rand.Rand) []rsCand {
	set := map[rsCand]bool{}
	spaces := append([]string{"", "rs_nosp"}, f.spaces...)
	projects := map[string]bool{"rs_nopr": true}
	for _, m := range f.members {
		if m.rtype == "space" {
			spaces = append(spaces, m.rid)
		} else {
			projects[m.rid] = true
		}
	}
	for _, p := range f.projects {
		projects[p.id] = true
	}
	for _, s := range spaces {
		set[rsCand{s, ""}] = true
		set[rsCand{s, "rs_nopr"}] = true
	}
	for _, p := range f.projects {
		set[rsCand{p.space, p.id}] = true
		set[rsCand{"rs_nosp", p.id}] = true
		set[rsCand{"", p.id}] = true
		set[rsCand{f.spaces[r.Intn(len(f.spaces))], p.id}] = true
	}
	for id := range projects {
		set[rsCand{f.spaces[0], id}] = true
	}
	out := make([]rsCand, 0, len(set))
	for c := range set {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].space != out[j].space {
			return out[i].space < out[j].space
		}
		return out[i].project < out[j].project
	})
	return out
}

type rsVariant struct{ name, projects, mode string }

func rsVariants(f *rsFixture, r *rand.Rand) []rsVariant {
	var all, some []string
	for _, p := range f.projects {
		all = append(all, p.id)
		if r.Intn(2) == 0 {
			some = append(some, p.id)
		}
	}
	all = append(all, "rs_nopr")
	some = append(some, "rs_nopr", some[0], some[0])
	js := func(v any) string { b, _ := json.Marshal(v); return string(b) }
	mixed := fmt.Sprintf(`[1,null,true,[%s],{"id":%s},%s,%s]`, js(all[0]), js(all[0]), js(all[1]), js(all[2]))
	return []rsVariant{
		{"none", "", ""},
		{"none+write-mode", "", "write"},
		{"read-some", js(some), "read"},
		{"write-some", js(some), "write"},
		{"read-empty-list", "[]", "read"},
		{"read-all", js(all), "read"},
		{"write-all", js(all), "write"},
		{"bad-json", "{not json", "read"},
		{"object", `{"` + all[0] + `":"x"}`, "read"},
		{"string", js(all[0]), "write"},
		{"json-null", "null", "write"},
		{"mixed-types", mixed, "write"},
		{"bad-mode", js(all), "admin"},
		{"empty-mode", js(all), ""},
		{"upper-mode", js(all), "READ"},
		{"whitespace", " ", "read"},
	}
}

func rsDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("AIWIKI_TEST_POSTGRES")
	if dsn == "" {
		t.Skip("set AIWIKI_TEST_POSTGRES_OWNER and AIWIKI_TEST_POSTGRES to a disposable database")
	}
	return dsn
}

// rsSession pins one runtime connection with the given user (nil = the setting was
// never set, so current_setting returns NULL), MCP scope and move mode.
func rsSession(t *testing.T, root *Server, user *string, v rsVariant, move string) (*sql.Conn, func()) {
	t.Helper()
	ctx := context.Background()
	var conn *sql.Conn
	var closeAll func()
	var err error
	if user == nil {
		fresh, err := sql.Open("pgx", rsDSN(t))
		if err != nil {
			t.Fatal(err)
		}
		fresh.SetMaxOpenConns(1)
		if conn, err = fresh.Conn(ctx); err != nil {
			t.Fatal(err)
		}
		closeAll = func() { conn.Close(); fresh.Close() }
		var isNull bool
		if err := conn.QueryRowContext(ctx, `SELECT current_setting('app.user_id',true) IS NULL`).Scan(&isNull); err != nil || !isNull {
			t.Fatalf("fresh session should have no app.user_id: %v %v", isNull, err)
		}
		_, err = conn.ExecContext(ctx, `SELECT set_config('app.mcp_projects',$1,false), set_config('app.mcp_mode',$2,false), set_config('app.move',$3,false)`, v.projects, v.mode, move)
	} else {
		if conn, err = root.DB.pool.Conn(ctx); err != nil {
			t.Fatal(err)
		}
		closeAll = func() {
			conn.ExecContext(ctx, `SELECT set_config('app.user_id','',false), set_config('app.mcp_projects','',false), set_config('app.mcp_mode','',false), set_config('app.move','',false)`)
			conn.Close()
		}
		_, err = conn.ExecContext(ctx, `SELECT set_config('app.user_id',$1,false), set_config('app.mcp_projects',$2,false), set_config('app.mcp_mode',$3,false), set_config('app.move',$4,false)`, *user, v.projects, v.mode, move)
	}
	if err != nil {
		closeAll()
		t.Fatal(err)
	}
	return conn, closeAll
}

func rsLabel(user *string) string {
	if user == nil {
		return "NULL"
	}
	return fmt.Sprintf("%q", *user)
}

func rsUsers(f *rsFixture) []*string {
	var out []*string
	for _, u := range f.users {
		u := u
		out = append(out, &u)
	}
	ghost, empty := "rs_ghost", ""
	return append(out, &ghost, &empty, nil)
}

func rsSorted(t *testing.T, conn *sql.Conn, q string, args ...any) []string {
	t.Helper()
	rows, err := conn.QueryContext(context.Background(), q, args...)
	if err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	sort.Strings(out)
	return out
}

func rsDiff(want, got []string) string {
	w, g := map[string]bool{}, map[string]bool{}
	for _, s := range want {
		w[s] = true
	}
	for _, s := range got {
		g[s] = true
	}
	var missing, extra []string
	for s := range w {
		if !g[s] {
			missing = append(missing, s)
		}
	}
	for s := range g {
		if !w[s] {
			extra = append(extra, s)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)
	return fmt.Sprintf("missing %q extra %q", missing, extra)
}

const rsRefRankSQL = `SELECT v.s, v.p, public.corporate_role_rank(public.corporate_effective_role(current_setting('app.user_id',true), v.s, v.p))
	FROM unnest($1::text[], $2::text[]) AS v(s,p)`

// rsCompareScope returns how many containers the set function reported.
func rsCompareScope(t *testing.T, conn *sql.Conn, label string, cands []rsCand) int {
	t.Helper()
	ctx := context.Background()
	spaces, projects := make([]string, len(cands)), make([]string, len(cands))
	known := map[rsCand]bool{}
	for i, c := range cands {
		spaces[i], projects[i] = c.space, c.project
		known[c] = true
	}
	want := map[rsCand]int{}
	rows, err := conn.QueryContext(ctx, rsRefRankSQL, spaces, projects)
	if err != nil {
		t.Fatalf("%s: reference: %v", label, err)
	}
	for rows.Next() {
		var c rsCand
		var rank int
		if err := rows.Scan(&c.space, &c.project, &rank); err != nil {
			t.Fatal(err)
		}
		if rank >= 1 {
			want[c] = rank
		}
	}
	rows.Close()
	got := map[rsCand]int{}
	rows, err = conn.QueryContext(ctx, `SELECT space_id, project_id, role_rank FROM public.corporate_role_scope()`)
	if err != nil {
		t.Fatalf("%s: corporate_role_scope: %v", label, err)
	}
	for rows.Next() {
		var c rsCand
		var rank int
		if err := rows.Scan(&c.space, &c.project, &rank); err != nil {
			t.Fatal(err)
		}
		if _, dup := got[c]; dup {
			t.Fatalf("%s: duplicate row %v", label, c)
		}
		if !known[c] {
			t.Fatalf("%s: row %v outside the candidate set", label, c)
		}
		got[c] = rank
	}
	rows.Close()
	for c, rank := range want {
		if g, ok := got[c]; !ok || g != rank {
			t.Fatalf("%s: %v reference rank %d, scope %d (present %v)", label, c, rank, g, ok)
		}
	}
	for c, rank := range got {
		if want[c] != rank {
			t.Fatalf("%s: %v scope rank %d, reference %d", label, c, rank, want[c])
		}
	}
	return len(got)
}

func TestPostgresRoleScopeMatchesEffectiveRole(t *testing.T) {
	owner, root := corporateFixture(t)
	ctx := context.Background()
	combos, nonEmpty, rowsSeen, mcpNonEmpty, candTotal := 0, 0, 0, 0, 0
	for seed := int64(1); seed <= 30; seed++ {
		f := rsGenerate(seed, false)
		rsLoad(t, owner, f)
		r := rand.New(rand.NewSource(seed * 7919))
		cands := f.candidates(r)
		candTotal += len(cands)
		for vi, v := range rsVariants(f, r) {
			for ui, user := range rsUsers(f) {
				// Unknown, empty and NULL users have no grants under any scope: three
				// variants cover them.
				if ui >= len(f.users) && vi%5 != 0 {
					continue
				}
				conn, done := rsSession(t, root, user, v, "")
				n := rsCompareScope(t, conn, fmt.Sprintf("seed %d user %s variant %s", seed, rsLabel(user), v.name), cands)
				done()
				combos++
				rowsSeen += n
				if n > 0 {
					nonEmpty++
					if v.projects != "" {
						mcpNonEmpty++
					}
				}
			}
		}
	}
	// A vacuous run (nobody can read anything) must fail loudly.
	if nonEmpty < 200 || mcpNonEmpty < 20 {
		t.Fatalf("fixtures too empty: %d of %d combinations returned rows, %d under MCP scope", nonEmpty, combos, mcpNonEmpty)
	}
	t.Logf("compared %d (seed,user,variant) combinations over 30 seeds, %d candidate containers in total; %d returned rows, %d combinations non-empty (%d under MCP scope)", combos, candTotal, rowsSeen, nonEmpty, mcpNonEmpty)
	_ = ctx
}

type rsExpected struct {
	articles, revisions, comments, attachments, fts, favorites, projects, spaces []string
}

// rsExpectedRows evaluates the pre-migration predicates (the reference functions)
// row by row as the runtime role.
func rsExpectedRows(t *testing.T, conn *sql.Conn, f *rsFixture, user *string) rsExpected {
	t.Helper()
	var as, ap, aid []string
	for _, a := range f.articles {
		aid, as, ap = append(aid, a.id), append(as, a.space), append(ap, a.project)
	}
	readable := map[string]bool{}
	for _, id := range rsSorted(t, conn, `SELECT a.id FROM unnest($1::text[],$2::text[],$3::text[]) AS a(id,s,p) WHERE public.corporate_article_read_policy(a.s,a.p)`, aid, as, ap) {
		readable[id] = true
	}
	var e rsExpected
	// Old articles policy, plus the project-move read policy (only in app.move=project).
	e.articles = rsSorted(t, conn, `SELECT a.id FROM unnest($1::text[],$2::text[],$3::text[]) AS a(id,s,p)
		WHERE public.corporate_article_read_policy(a.s,a.p)
		   OR (coalesce(current_setting('app.move',true),'')='project' AND a.p<>'' AND EXISTS (
		        SELECT 1 FROM public.projects pr WHERE pr.id=a.p AND pr.space_id<>a.s
		          AND public.corporate_role_rank(public.corporate_effective_role(current_setting('app.user_id',true),pr.space_id,pr.id)) >= 3
		          AND public.corporate_container_writable(pr.space_id,pr.id)))`, aid, as, ap)
	// Old revisions policy: current article readable AND snapshot container readable.
	var ra, rs, rp, keys []string
	for _, rv := range f.revisions {
		ra, rs, rp = append(ra, rv.article), append(rs, rv.space), append(rp, rv.project)
		keys = append(keys, fmt.Sprintf("%s#%d", rv.article, rv.rev))
	}
	snapOK := map[string]bool{}
	for _, k := range rsSorted(t, conn, `SELECT v.k FROM unnest($1::text[],$2::text[],$3::text[]) AS v(k,s,p) WHERE public.corporate_article_read_policy(v.s,v.p)`, keys, rs, rp) {
		snapOK[k] = true
	}
	e.revisions = []string{}
	for i, k := range keys {
		if readable[ra[i]] && snapOK[k] {
			e.revisions = append(e.revisions, k)
		}
	}
	for _, c := range f.comments {
		if readable[c.article] {
			e.comments = append(e.comments, c.id)
		}
	}
	for _, c := range f.attachments {
		if readable[c.article] {
			e.attachments = append(e.attachments, c.id)
		}
	}
	for _, a := range f.articles {
		if readable[a.id] {
			e.fts = append(e.fts, a.id)
		}
	}
	for _, fv := range f.favorites {
		if user != nil && fv[0] == *user && readable[fv[1]] {
			e.favorites = append(e.favorites, fv[0]+"|"+fv[1])
		}
	}
	var pid, psp []string
	for _, p := range f.projects {
		pid, psp = append(pid, p.id), append(psp, p.space)
	}
	// mcp_scope_projects_default shows every project while no MCP scope is active.
	e.projects = rsSorted(t, conn, `SELECT p.id FROM unnest($1::text[],$2::text[]) AS p(id,s)
		WHERE NOT public.corporate_mcp_scope_active()
		   OR public.corporate_role_rank(public.corporate_effective_role(current_setting('app.user_id',true),p.s,p.id)) >= 1`, pid, psp)
	e.spaces = rsSorted(t, conn, `SELECT s.id FROM unnest($1::text[]) AS s(id)
		WHERE public.corporate_role_rank(public.corporate_effective_role(current_setting('app.user_id',true),s.id,'')) >= 1`, f.spaces)
	for _, s := range []*[]string{&e.revisions, &e.comments, &e.attachments, &e.fts, &e.favorites} {
		if *s == nil {
			*s = []string{}
		}
		sort.Strings(*s)
	}
	return e
}

func TestPostgresRoleScopePoliciesMatchReferencePredicates(t *testing.T) {
	owner, root := corporateFixture(t)
	checks, visible, hidden := 0, map[string]int{}, map[string]int{}
	for seed := int64(100); seed < 112; seed++ {
		f := rsGenerate(seed, true)
		rsLoad(t, owner, f)
		r := rand.New(rand.NewSource(seed * 104729))
		variants := rsVariants(f, r)
		for vi, v := range variants {
			for ui, user := range rsUsers(f) {
				if ui >= len(f.users) && vi%4 != 0 {
					continue
				}
				// The project-move read policy is checked without an MCP scope; other
				// move values must change nothing.
				moves := []string{""}
				if v.projects == "" {
					moves = []string{"", "project", "article", "probe"}
				}
				for _, move := range moves {
					label := fmt.Sprintf("seed %d user %s variant %s move %q", seed, rsLabel(user), v.name, move)
					conn, done := rsSession(t, root, user, v, move)
					want := rsExpectedRows(t, conn, f, user)
					for _, tc := range []struct {
						table, q string
						want     []string
						total    int
					}{
						{"articles", `SELECT id FROM articles`, want.articles, len(f.articles)},
						{"revisions", `SELECT article_id||'#'||revision FROM revisions`, want.revisions, len(f.revisions)},
						{"comments", `SELECT id FROM comments`, want.comments, len(f.comments)},
						{"attachments", `SELECT id FROM attachments`, want.attachments, len(f.attachments)},
						{"article_fts", `SELECT article_id FROM article_fts`, want.fts, len(f.articles)},
						{"favorites", `SELECT user_id||'|'||article_id FROM favorites`, want.favorites, len(f.favorites)},
						{"projects", `SELECT id FROM projects`, want.projects, len(f.projects)},
						{"spaces", `SELECT id FROM spaces`, want.spaces, len(f.spaces)},
					} {
						got := rsSorted(t, conn, tc.q)
						if strings.Join(got, "\n") != strings.Join(tc.want, "\n") {
							done()
							t.Fatalf("%s: %s differ: %s", label, tc.table, rsDiff(tc.want, got))
						}
						checks++
						visible[tc.table] += len(got)
						hidden[tc.table] += tc.total - len(got)
					}
					// Revisions must not be widened by the move policy: the old rule
					// (explicit readable-article check) is the expectation above, and
					// the article policy may show more rows than revisions allow.
					done()
				}
			}
		}
	}
	for _, tbl := range []string{"articles", "revisions", "comments", "attachments", "article_fts", "favorites", "projects", "spaces"} {
		if visible[tbl] == 0 || hidden[tbl] == 0 {
			t.Fatalf("%s check is vacuous: %d visible, %d hidden rows over all combinations", tbl, visible[tbl], hidden[tbl])
		}
	}
	t.Logf("%d table/session comparisons; visible rows %v, hidden rows %v", checks, visible, hidden)
}

// The move policy widens articles only (UPDATE needs SELECT on the old row); the
// dependants keep their explicit readable-article check. Pinned on a concrete case.
func TestPostgresRoleScopeMoveModeDoesNotWidenRevisions(t *testing.T) {
	owner, root := corporateFixture(t)
	f := &rsFixture{
		spaces: []string{"rs_sp0", "rs_sp1"}, users: []string{"rs_u0"},
		disabled: map[string]bool{}, archived: map[string]bool{},
		projects: []rsProject{{"rs_p0", "rs_sp1", "", 1}},
		members:  []rsMember{{"project", "rs_p0", "user", "rs_u0", "editor"}},
		// The article sits in sp0 (no grant) but its project lives in sp1 (editor).
		articles:  []rsArticle{{"rs_a0", "rs_sp0", "rs_p0"}},
		revisions: []rsRevision{{"rs_a0", "rs_sp0", "rs_p0", 1}},
		comments:  []rsChild{{"rs_a0_c0", "rs_a0"}},
	}
	rsLoad(t, owner, f)
	u := "rs_u0"
	for move, wantArticle := range map[string]int{"": 0, "project": 1} {
		conn, done := rsSession(t, root, &u, rsVariant{"none", "", ""}, move)
		var n [3]int
		for i, tbl := range []string{"articles", "revisions", "comments"} {
			if err := conn.QueryRowContext(context.Background(), `SELECT count(*) FROM `+tbl).Scan(&n[i]); err != nil {
				t.Fatal(err)
			}
		}
		done()
		if n[0] != wantArticle || n[1] != 0 || n[2] != 0 {
			t.Fatalf("move %q: articles/revisions/comments visible = %v, want [%d 0 0]", move, n, wantArticle)
		}
	}
}

func TestPostgresRoleScopeFunctionIsNotALeak(t *testing.T) {
	owner, root := corporateFixture(t)
	ctx := context.Background()
	f := rsGenerate(3, false)
	rsLoad(t, owner, f)

	var args int
	var secdef bool
	var ownerName, runtimeName string
	if err := owner.DB.QueryRow(`SELECT p.pronargs, p.prosecdef, pg_get_userbyid(p.proowner), current_user FROM pg_proc p WHERE p.oid='public.corporate_role_scope()'::regprocedure`).Scan(&args, &secdef, &ownerName, &runtimeName); err != nil {
		t.Fatal(err)
	}
	if args != 0 || !secdef || ownerName != runtimeName {
		t.Fatalf("corporate_role_scope: %d args, security definer %v, owner %s (migration role %s)", args, secdef, ownerName, runtimeName)
	}
	// PUBLIC must not hold EXECUTE (new functions get it by default); the runtime role
	// does, through the grant that runs at every start.
	var public int
	if err := owner.DB.QueryRow(`SELECT count(*) FROM pg_proc p, aclexplode(COALESCE(p.proacl,acldefault('f',p.proowner))) a WHERE p.oid='public.corporate_role_scope()'::regprocedure AND a.grantee=0`).Scan(&public); err != nil || public != 0 {
		t.Fatalf("PUBLIC EXECUTE entries on corporate_role_scope: %d %v", public, err)
	}
	var appCan bool
	if err := root.DB.QueryRow(`SELECT has_function_privilege(current_user,'public.corporate_role_scope()','EXECUTE')`).Scan(&appCan); err != nil || !appCan {
		t.Fatalf("runtime role cannot execute corporate_role_scope: %v %v", appCan, err)
	}

	// The function has no arguments: the subject is the session's app.user_id. A runtime
	// session chooses that setting freely, exactly as it can already pass any user to
	// corporate_effective_role in an unscoped session, so this adds no capability. What
	// the setting cannot do is widen an MCP scope: a scoped session sees only listed
	// projects and never space rows, whoever the user is.
	cands := f.candidates(rand.New(rand.NewSource(1)))
	for _, name := range f.users[:3] {
		name := name
		plain, done := rsSession(t, root, &name, rsVariant{"none", "", ""}, "")
		rsCompareScope(t, plain, "unscoped "+name, cands)
		var all []string
		for _, p := range f.projects {
			all = append(all, p.id)
		}
		list, _ := json.Marshal(all[:2])
		scoped, doneScoped := rsSession(t, root, &name, rsVariant{"scoped", string(list), "read"}, "")
		rows, err := scoped.QueryContext(ctx, `SELECT space_id, project_id, role_rank FROM public.corporate_role_scope()`)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var s, p string
			var rank int
			if err := rows.Scan(&s, &p, &rank); err != nil {
				t.Fatal(err)
			}
			if p != all[0] && p != all[1] || rank != 1 {
				t.Fatalf("%s: scoped session got %q/%q rank %d outside its two read projects", name, s, p, rank)
			}
		}
		rows.Close()
		doneScoped()
		done()
	}
}

func TestPostgresRoleScopeMigrationAndGuard(t *testing.T) {
	owner, root := corporateFixture(t)
	if err := root.DB.validateRuntimeSchema(); err != nil {
		t.Fatalf("runtime schema guard after migration: %v", err)
	}
	// Re-running the migrations is a no-op for the registered component.
	if err := owner.migrateCorporate(); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := owner.DB.QueryRow(`SELECT count(*) FROM schema_migrations WHERE component='role_scope' AND version=1`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("role_scope migration rows: %d %v", n, err)
	}
	var fn string
	for _, st := range splitCorporateSQL(roleScopeSQL) {
		if strings.Contains(st, "CREATE OR REPLACE FUNCTION public.corporate_role_scope") {
			fn = st
		}
	}
	if fn == "" || !strings.Contains(fn, "WHERE b.rank >= 1") {
		t.Fatal("function statement not found in role_scope.sql")
	}
	const spaces = `(id, ''::text) IN (SELECT s.space_id, s.project_id FROM public.corporate_role_scope() s)`
	const fnName = `FUNCTION public.corporate_role_scope()`
	for _, c := range []struct{ name, break_, fix string }{
		{"PUBLIC may execute", `GRANT EXECUTE ON ` + fnName + ` TO PUBLIC`, `REVOKE ALL ON ` + fnName + ` FROM PUBLIC`},
		{"runtime role may not execute", `REVOKE EXECUTE ON ` + fnName + ` FROM aiwiki_app`, `GRANT EXECUTE ON ` + fnName + ` TO aiwiki_app`},
		{"spaces policy back to true", `ALTER POLICY corporate_spaces_read ON spaces USING (true)`, `ALTER POLICY corporate_spaces_read ON spaces USING (` + spaces + `)`},
		{"true OR scope", `ALTER POLICY corporate_spaces_read ON spaces USING (true OR ` + spaces + `)`, `ALTER POLICY corporate_spaces_read ON spaces USING (` + spaces + `)`},
		{"SECURITY INVOKER", `ALTER ` + fnName + ` SECURITY INVOKER`, `ALTER ` + fnName + ` SECURITY DEFINER`},
		{"search_path dropped", `ALTER ` + fnName + ` RESET search_path`, `ALTER ` + fnName + ` SET search_path = pg_catalog, public, pg_temp`},
		{"restrictive swap", `DROP POLICY corporate_spaces_read ON spaces; CREATE POLICY corporate_spaces_read ON spaces AS RESTRICTIVE FOR SELECT USING (` + spaces + `)`,
			`DROP POLICY corporate_spaces_read ON spaces; CREATE POLICY corporate_spaces_read ON spaces FOR SELECT USING (` + spaces + `)`},
		{"extra permissive SELECT policy", `CREATE POLICY zz_extra ON articles FOR SELECT USING (true)`, `DROP POLICY zz_extra ON articles`},
		{"altered function body", strings.Replace(fn, "WHERE b.rank >= 1", "WHERE b.rank >= 0", 1), fn},
	} {
		if _, err := owner.DB.Exec(c.break_); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		err := root.DB.validateRuntimeSchema()
		if _, ferr := owner.DB.Exec(c.fix); ferr != nil {
			t.Fatalf("%s: restore: %v", c.name, ferr)
		}
		if err == nil || err != errRoleScope {
			t.Fatalf("%s: guard returned %v, want the role scope error", c.name, err)
		}
		if err := root.DB.validateRuntimeSchema(); err != nil {
			t.Fatalf("%s: guard after restore: %v", c.name, err)
		}
	}
}
