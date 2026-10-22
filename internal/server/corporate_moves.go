package server

import (
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
)

// Safe moves of articles and projects between containers in corporate mode.
// The rules and wire format are in docs/MOVES.md. Access impact is computed by
// the SQL function corporate_move_impact (the same chain recursion as
// corporate_effective_role); this file never re-implements inheritance.

const (
	moveMaxArticles = 500
	moveMaxProjects = 100
)

type moveTarget struct {
	SpaceID   string `json:"space_id"`
	ProjectID string `json:"project_id"`
	ParentID  string `json:"parent_id"`
}

type moveRequest struct {
	Kind         string     `json:"kind"`
	ID           string     `json:"id"`
	BaseRevision int        `json:"base_revision"`
	Target       moveTarget `json:"target"`
	Fingerprint  string     `json:"fingerprint"`
}

type moveContainer struct {
	SpaceID     string `json:"space_id"`
	SpaceName   string `json:"space_name"`
	ProjectID   string `json:"project_id"`
	ProjectName string `json:"project_name"`
}

type moveAccess struct {
	SubjectType string  `json:"subject_type"`
	SubjectID   string  `json:"subject_id"`
	Name        string  `json:"name"`
	Before      *string `json:"before"`
	After       *string `json:"after"`
	Users       int     `json:"users"`
}

type movePreview struct {
	Fingerprint  string        `json:"fingerprint"`
	Articles     int           `json:"articles"`
	Projects     int           `json:"projects"`
	Source       moveContainer `json:"source"`
	Target       moveContainer `json:"target"`
	Access       []moveAccess  `json:"access"`
	UsersGaining int           `json:"users_gaining"`
	UsersLosing  int           `json:"users_losing"`
	Warnings     []string      `json:"warnings"`
}

type moveError struct {
	code int
	msg  string
}

func (e *moveError) Error() string { return e.msg }

func moveFail(code int, msg string) *moveError { return &moveError{code, msg} }

// moveDBFail maps a database failure to a client-facing error. Authorization
// failures raised by the database guards are reported as 403.
func moveDBFail(err error) *moveError {
	text := err.Error()
	if strings.Contains(text, "SQLSTATE 42501") || strings.Contains(text, "row-level security") {
		return moveFail(http.StatusForbidden, "move is not permitted")
	}
	return moveFail(500, "database error")
}

type moveArticle struct {
	ID       string
	Revision int
	Parent   string
	Deleted  bool
}

type moveProject struct {
	ID       string
	ParentID string
	SpaceID  string
}

type movePlan struct {
	req      moveRequest
	actor    string
	articles []moveArticle // article kind: root first; project kind: articles whose space changes
	projects []moveProject // project kind: parents before children
	preview  movePreview
	root     Article
	project  corporateProject
	access   []moveAccess
}

var moveRoleRank = map[string]int{"": 0, "reader": 1, "commenter": 2, "editor": 3, "manager": 4, "owner": 5}

func moveRank(q interface{ QueryRow(string, ...any) *sql.Row }, actor, space, project string) int {
	var rank int
	if q.QueryRow(`SELECT corporate_role_rank(corporate_effective_role(?,?,?))`, actor, space, project).Scan(&rank) != nil {
		return 0
	}
	return rank
}

func moveNamePtr(role string) *string {
	if role == "" {
		return nil
	}
	return &role
}

func moveWritable(q interface{ QueryRow(string, ...any) *sql.Row }, space, project string) bool {
	var ok bool
	return q.QueryRow(`SELECT corporate_container_writable(?,?)`, space, project).Scan(&ok) == nil && ok
}

func moveContainerInfo(q interface{ QueryRow(string, ...any) *sql.Row }, space, project string) moveContainer {
	c := moveContainer{SpaceID: space, ProjectID: project}
	_ = q.QueryRow(`SELECT name FROM spaces WHERE id=?`, space).Scan(&c.SpaceName)
	if project != "" {
		_ = q.QueryRow(`SELECT name FROM projects WHERE id=?`, project).Scan(&c.ProjectName)
	}
	return c
}

func (s *Server) registerMoveRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/moves/preview", func(w http.ResponseWriter, r *http.Request) { s.moveHandler(w, r, false) })
	mux.HandleFunc("POST /api/moves/apply", func(w http.ResponseWriter, r *http.Request) { s.moveHandler(w, r, true) })
}

func (s *Server) moveHandler(w http.ResponseWriter, r *http.Request, apply bool) {
	actor, ok := s.corporateActor(w, r, false)
	if !ok {
		return
	}
	// Delegated MCP sessions never move content.
	if s.principal.MCPMode != "" || s.principal.MCPProjects != nil {
		apiErr(w, http.StatusForbidden, "forbidden")
		return
	}
	var req moveRequest
	if corporateDecode(r, &req) != nil || (req.Kind != "article" && req.Kind != "project") || req.ID == "" ||
		len(req.ID) > 200 || len(req.Target.SpaceID) > 200 || len(req.Target.ProjectID) > 200 || len(req.Target.ParentID) > 200 || len(req.Fingerprint) > 200 {
		apiErr(w, 400, "invalid input")
		return
	}
	if apply && req.Fingerprint == "" {
		apiErr(w, 400, "fingerprint required")
		return
	}
	tx, err := s.DB.Begin()
	if err != nil {
		apiErr(w, 500, "database error")
		return
	}
	defer tx.Rollback()
	if apply {
		if err = corporateLockOwners(tx); err != nil {
			apiErr(w, 500, "database error")
			return
		}
	}
	plan, merr := s.movePlan(tx, actor, req)
	if merr != nil {
		apiErr(w, merr.code, merr.msg)
		return
	}
	if !apply {
		writeJSON(w, 200, plan.preview)
		return
	}
	if subtle.ConstantTimeCompare([]byte(plan.preview.Fingerprint), []byte(req.Fingerprint)) != 1 {
		writeJSON(w, http.StatusConflict, map[string]any{"error": "access changed", "preview": plan.preview})
		return
	}
	var out map[string]any
	if req.Kind == "article" {
		out, merr = s.moveApplyArticle(tx, plan)
	} else {
		out, merr = s.moveApplyProject(tx, plan)
	}
	if merr != nil {
		apiErr(w, merr.code, merr.msg)
		return
	}
	if err = tx.Commit(); err != nil {
		apiErr(w, 500, "database error")
		return
	}
	writeJSON(w, 200, out)
}

// movePlan validates every rule of docs/MOVES.md against the current database
// state (inside tx) and builds the preview. It is used by preview and apply.
func (s *Server) movePlan(tx *transaction, actor string, req moveRequest) (*movePlan, *moveError) {
	plan := &movePlan{req: req, actor: actor}
	var tcontainer string
	var sourceInfo, targetInfo moveContainer
	var warnings []string
	t := req.Target
	if t.SpaceID == "" {
		return nil, moveFail(400, "target space required")
	}
	if req.Kind == "article" {
		if req.BaseRevision < 1 {
			return nil, moveFail(400, "base_revision required")
		}
		if t.ProjectID != "" && t.ParentID == t.ProjectID {
			return nil, moveFail(400, "invalid input")
		}
		root, err := moveLoadArticle(tx, req.ID)
		if err != nil {
			return nil, moveFail(404, "article not found")
		}
		if root.DeletedAt != "" {
			return nil, moveFail(409, "article is deleted; use undelete first")
		}
		if root.Revision != req.BaseRevision {
			return nil, moveFail(409, fmt.Sprintf("revision conflict: current revision is %d", root.Revision))
		}
		if root.SpaceID == t.SpaceID && root.ProjectID == t.ProjectID {
			return nil, moveFail(400, "target is the same container; change the parent with a normal article update")
		}
		if moveRank(tx, actor, root.SpaceID, root.ProjectID) < 4 {
			return nil, moveFail(403, "moving an article requires the manager role on the source container")
		}
		if moveRank(tx, actor, t.SpaceID, t.ProjectID) < 1 {
			return nil, moveFail(404, "target not found")
		}
		if moveRank(tx, actor, t.SpaceID, t.ProjectID) < 4 {
			return nil, moveFail(403, "moving an article requires the manager role on the target container")
		}
		if t.ProjectID != "" {
			var one int
			if tx.QueryRow(`SELECT 1 FROM projects WHERE id=? AND space_id=?`, t.ProjectID, t.SpaceID).Scan(&one) != nil {
				return nil, moveFail(404, "target not found")
			}
		}
		if !moveWritable(tx, root.SpaceID, root.ProjectID) || !moveWritable(tx, t.SpaceID, t.ProjectID) {
			return nil, moveFail(409, "source or target container is archived")
		}
		if t.ParentID != "" {
			parent, err := moveLoadArticle(tx, t.ParentID)
			if err != nil || parent.SpaceID != t.SpaceID || parent.ProjectID != t.ProjectID {
				return nil, moveFail(400, "target parent must be in the target container")
			}
			if parent.DeletedAt != "" {
				return nil, moveFail(409, "parent article is deleted")
			}
		}
		rows, err := tx.Query(`WITH RECURSIVE sub(id,path) AS (
			SELECT id,ARRAY[id]::text[] FROM articles WHERE id=?
			UNION ALL
			SELECT a.id,sub.path||a.id FROM articles a JOIN sub ON a.parent_id=sub.id WHERE NOT a.id=ANY(sub.path)
		) SELECT a.id,a.revision,a.parent_id,a.space_id,a.project_id,coalesce(a.deleted_at,'')<>'' FROM articles a WHERE a.id IN (SELECT id FROM sub) ORDER BY a.id LIMIT ?`, req.ID, moveMaxArticles+1)
		if err != nil {
			return nil, moveDBFail(err)
		}
		deleted := 0
		for rows.Next() {
			var item moveArticle
			var space, project string
			if err = rows.Scan(&item.ID, &item.Revision, &item.Parent, &space, &project, &item.Deleted); err != nil {
				rows.Close()
				return nil, moveFail(500, "database error")
			}
			if space != root.SpaceID || project != root.ProjectID {
				rows.Close()
				return nil, moveFail(409, "article subtree spans several containers")
			}
			if item.Deleted {
				deleted++
			}
			plan.articles = append(plan.articles, item)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, moveFail(500, "database error")
		}
		if len(plan.articles) > moveMaxArticles {
			return nil, moveFail(400, "move too large")
		}
		// The root first, then the descendants in id order.
		sort.SliceStable(plan.articles, func(i, j int) bool {
			if (plan.articles[i].ID == req.ID) != (plan.articles[j].ID == req.ID) {
				return plan.articles[i].ID == req.ID
			}
			return plan.articles[i].ID < plan.articles[j].ID
		})
		if deleted > 0 {
			warnings = append(warnings, fmt.Sprintf("В поддереве есть удалённые статьи (%d): они переедут вместе с остальными.", deleted))
		}
		plan.root = root
		tcontainer = t.ProjectID
		sourceInfo = moveContainerInfo(tx, root.SpaceID, root.ProjectID)
		targetInfo = moveContainerInfo(tx, t.SpaceID, t.ProjectID)
	} else {
		if t.ProjectID != "" {
			return nil, moveFail(400, "a project moves to target.parent_id, not target.project_id")
		}
		var p corporateProject
		var inherit int
		if err := tx.QueryRow(`SELECT id,space_id,parent_id,name,description,inherit_acl,coalesce(archived_at,'') FROM projects WHERE id=?`, req.ID).Scan(&p.ID, &p.SpaceID, &p.ParentID, &p.Name, &p.Description, &inherit, &p.ArchivedAt); err != nil {
			return nil, moveFail(404, "project not found")
		}
		p.InheritACL = inherit != 0
		switch rank := moveRank(tx, actor, p.SpaceID, p.ID); {
		case rank < 1:
			return nil, moveFail(404, "project not found")
		case rank < 5:
			return nil, moveFail(403, "moving a project requires the owner role")
		}
		if p.SpaceID == t.SpaceID && p.ParentID == t.ParentID {
			return nil, moveFail(400, "project is already there")
		}
		if t.ParentID != "" {
			var one int
			if tx.QueryRow(`SELECT 1 FROM projects WHERE id=? AND space_id=?`, t.ParentID, t.SpaceID).Scan(&one) != nil {
				return nil, moveFail(404, "target not found")
			}
		}
		if moveRank(tx, actor, t.SpaceID, t.ParentID) < 1 {
			return nil, moveFail(404, "target not found")
		}
		if moveRank(tx, actor, t.SpaceID, t.ParentID) < 4 {
			return nil, moveFail(403, "moving a project requires the manager role on the target")
		}
		rows, err := tx.Query(`WITH RECURSIVE sub(id,depth,path) AS (
			SELECT id,0,ARRAY[id]::text[] FROM projects WHERE id=?
			UNION ALL
			SELECT c.id,sub.depth+1,sub.path||c.id FROM projects c JOIN sub ON c.parent_id=sub.id WHERE NOT c.id=ANY(sub.path) AND sub.depth<64
		) SELECT DISTINCT p.id,p.parent_id,p.space_id,sub.depth,coalesce(p.archived_at,'')<>'',corporate_role_rank(corporate_effective_role(?,p.space_id,p.id)) FROM sub JOIN projects p ON p.id=sub.id ORDER BY sub.depth,p.id LIMIT ?`, req.ID, actor, moveMaxProjects+1)
		if err != nil {
			return nil, moveDBFail(err)
		}
		for rows.Next() {
			var item moveProject
			var depth, rank int
			var archived bool
			if err = rows.Scan(&item.ID, &item.ParentID, &item.SpaceID, &depth, &archived, &rank); err != nil {
				rows.Close()
				return nil, moveFail(500, "database error")
			}
			if item.ID == t.ParentID {
				rows.Close()
				return nil, moveFail(400, "a project cannot be moved into itself or its descendants")
			}
			if archived {
				rows.Close()
				return nil, moveFail(409, "the project subtree contains an archived project")
			}
			if rank < 4 {
				rows.Close()
				return nil, moveFail(403, "moving a project requires the manager role on every sub-project")
			}
			plan.projects = append(plan.projects, item)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, moveFail(500, "database error")
		}
		if len(plan.projects) > moveMaxProjects {
			return nil, moveFail(400, "move too large")
		}
		// A target inside the subtree can only be detected after the loop above
		// has seen every id (the check inside the loop covers the rows read so far).
		for _, item := range plan.projects {
			if item.ID == t.ParentID {
				return nil, moveFail(400, "a project cannot be moved into itself or its descendants")
			}
		}
		if !moveWritable(tx, p.SpaceID, p.ID) || !moveWritable(tx, t.SpaceID, t.ParentID) {
			return nil, moveFail(409, "source or target container is archived")
		}
		arows, err := tx.Query(`WITH RECURSIVE sub(id,depth,path) AS (
			SELECT id,0,ARRAY[id]::text[] FROM projects WHERE id=?
			UNION ALL
			SELECT c.id,sub.depth+1,sub.path||c.id FROM projects c JOIN sub ON c.parent_id=sub.id WHERE NOT c.id=ANY(sub.path) AND sub.depth<64
		) SELECT a.id,a.revision,a.parent_id,coalesce(a.deleted_at,'')<>'' FROM articles a WHERE a.project_id IN (SELECT id FROM sub) AND a.space_id<>? ORDER BY a.id LIMIT ?`, req.ID, t.SpaceID, moveMaxArticles+1)
		if err != nil {
			return nil, moveDBFail(err)
		}
		for arows.Next() {
			var item moveArticle
			if err = arows.Scan(&item.ID, &item.Revision, &item.Parent, &item.Deleted); err != nil {
				arows.Close()
				return nil, moveFail(500, "database error")
			}
			plan.articles = append(plan.articles, item)
		}
		err = arows.Err()
		arows.Close()
		if err != nil {
			return nil, moveFail(500, "database error")
		}
		if len(plan.articles) > moveMaxArticles {
			return nil, moveFail(400, "move too large")
		}
		plan.project = p
		tcontainer = t.ParentID
		sourceInfo = moveContainerInfo(tx, p.SpaceID, p.ParentID)
		targetInfo = moveContainerInfo(tx, t.SpaceID, t.ParentID)
	}

	// Access impact, computed and authorized by the database.
	rows, err := tx.Query(`SELECT r_kind,r_subject_type,r_subject_id,r_name,coalesce(r_before,''),coalesce(r_after,''),r_users FROM corporate_move_impact(?,?,?,?)`, req.Kind, req.ID, t.SpaceID, tcontainer)
	if err != nil {
		return nil, moveDBFail(err)
	}
	access := make([]moveAccess, 0)
	gaining, losing := 0, 0
	selfAfter := -1
	for rows.Next() {
		var kind, stype, sid, name, before, after string
		var users int
		if err = rows.Scan(&kind, &stype, &sid, &name, &before, &after, &users); err != nil {
			rows.Close()
			return nil, moveFail(500, "database error")
		}
		if kind == "user" {
			switch {
			case moveRoleRank[after] > moveRoleRank[before]:
				gaining++
			case moveRoleRank[after] < moveRoleRank[before]:
				losing++
			}
			if sid == actor {
				selfAfter = moveRoleRank[after]
			}
			continue
		}
		access = append(access, moveAccess{SubjectType: stype, SubjectID: sid, Name: name, Before: moveNamePtr(before), After: moveNamePtr(after), Users: users})
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, moveDBFail(err)
	}
	if req.Kind == "project" && selfAfter >= 0 && selfAfter < 3 {
		return nil, moveFail(403, "the move would remove your own edit access to the moved content")
	}
	sort.Slice(access, func(i, j int) bool {
		if access[i].SubjectType != access[j].SubjectType {
			return access[i].SubjectType < access[j].SubjectType
		}
		return access[i].SubjectID < access[j].SubjectID
	})
	if losing > 0 {
		warnings = append(warnings, fmt.Sprintf("Доступ потеряют пользователей: %d.", losing))
	}
	if gaining > 0 {
		warnings = append(warnings, fmt.Sprintf("Доступ получат пользователей: %d.", gaining))
	}
	if warnings == nil {
		warnings = []string{}
	}
	plan.access = access
	plan.preview = movePreview{Articles: len(plan.articles), Projects: len(plan.projects), Source: sourceInfo, Target: targetInfo,
		Access: access, UsersGaining: gaining, UsersLosing: losing, Warnings: warnings}
	plan.preview.Fingerprint = moveFingerprint(plan)
	return plan, nil
}

// moveFingerprint hashes a canonical encoding of the request, the actor, the
// moved objects with their versions and the sorted access diff. The slices are
// sorted before encoding, so the digest is stable.
func moveFingerprint(plan *movePlan) string {
	type item struct {
		ID string `json:"i"`
		A  string `json:"a,omitempty"`
		B  string `json:"b,omitempty"`
	}
	type grant struct {
		T string `json:"t"`
		I string `json:"i"`
		B string `json:"b"`
		A string `json:"a"`
		U int    `json:"u"`
	}
	articles := make([]item, 0, len(plan.articles))
	for _, a := range plan.articles {
		articles = append(articles, item{ID: a.ID, A: fmt.Sprint(a.Revision), B: a.Parent})
	}
	sort.Slice(articles, func(i, j int) bool { return articles[i].ID < articles[j].ID })
	projects := make([]item, 0, len(plan.projects))
	for _, p := range plan.projects {
		projects = append(projects, item{ID: p.ID, A: p.ParentID, B: p.SpaceID})
	}
	sort.Slice(projects, func(i, j int) bool { return projects[i].ID < projects[j].ID })
	grants := make([]grant, 0, len(plan.access))
	for _, g := range plan.access {
		b, a := "", ""
		if g.Before != nil {
			b = *g.Before
		}
		if g.After != nil {
			a = *g.After
		}
		grants = append(grants, grant{g.SubjectType, g.SubjectID, b, a, g.Users})
	}
	sort.Slice(grants, func(i, j int) bool {
		if grants[i].T != grants[j].T {
			return grants[i].T < grants[j].T
		}
		return grants[i].I < grants[j].I
	})
	raw, _ := json.Marshal(struct {
		V        int        `json:"v"`
		Kind     string     `json:"kind"`
		ID       string     `json:"id"`
		Target   moveTarget `json:"target"`
		Actor    string     `json:"actor"`
		Base     int        `json:"base"`
		Articles []item     `json:"articles"`
		Projects []item     `json:"projects"`
		Grants   []grant    `json:"grants"`
		Gaining  int        `json:"gaining"`
		Losing   int        `json:"losing"`
	}{1, plan.req.Kind, plan.req.ID, plan.req.Target, plan.actor, plan.req.BaseRevision, articles, projects, grants, plan.preview.UsersGaining, plan.preview.UsersLosing})
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func moveSetMarker(tx *transaction, mode, root string) error {
	var v string
	if err := tx.QueryRow(`SELECT set_config('app.move',?,true)`, mode).Scan(&v); err != nil {
		return err
	}
	return tx.QueryRow(`SELECT set_config('app.move_root',?,true)`, root).Scan(&v)
}

func moveAuditDetails(plan *movePlan) map[string]any {
	type change struct {
		SubjectType string  `json:"subject_type"`
		SubjectID   string  `json:"subject_id"`
		Before      *string `json:"before"`
		After       *string `json:"after"`
	}
	changes := make([]change, 0, len(plan.access))
	for _, g := range plan.access {
		changes = append(changes, change{g.SubjectType, g.SubjectID, g.Before, g.After})
	}
	return map[string]any{
		"source": plan.preview.Source, "target": plan.req.Target,
		"articles": plan.preview.Articles, "projects": plan.preview.Projects,
		"access": changes, "users_gaining": plan.preview.UsersGaining, "users_losing": plan.preview.UsersLosing,
	}
}

// moveArticleRevision stores the moved article as a new revision.
func (s *Server) moveArticleRevision(tx *transaction, a Article, space, project string, parent *string, message, now, author string) *moveError {
	item := moveArticle{ID: a.ID, Revision: a.Revision}
	a.SpaceID, a.ProjectID = space, project
	if parent != nil {
		a.ParentID = *parent
	}
	a.Revision++
	a.UpdatedAt = now
	a.Author = author
	res, err := tx.Exec(`UPDATE articles SET space_id=?,project_id=?,parent_id=?,revision=?,updated_at=?,author=? WHERE id=? AND revision=?`, a.SpaceID, a.ProjectID, a.ParentID, a.Revision, a.UpdatedAt, a.Author, a.ID, item.Revision)
	if err != nil {
		return moveDBFail(err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return moveFail(409, "article changed during the move")
	}
	if _, err = tx.Exec(`INSERT INTO revisions(article_id,revision,author,message,created_at,snapshot) VALUES(?,?,?,?,?,?)`, a.ID, a.Revision, a.Author, message, a.UpdatedAt, encode(a)); err != nil {
		return moveDBFail(err)
	}
	if a.DeletedAt != "" {
		_, err = tx.Exec(`DELETE FROM article_fts WHERE article_id=?`, a.ID)
	} else {
		err = indexArticle(tx, a)
	}
	if err != nil {
		return moveDBFail(err)
	}
	return nil
}

func (s *Server) moveApplyArticle(tx *transaction, plan *movePlan) (map[string]any, *moveError) {
	if err := moveSetMarker(tx, "article", plan.req.ID); err != nil {
		return nil, moveFail(500, "database error")
	}
	loaded, merr := moveLoadPlanned(tx, plan.articles)
	if merr != nil {
		return nil, merr
	}
	now, author := stamp(), s.changeAuthor(plan.actor)
	t := plan.req.Target
	for _, item := range loaded {
		var parent *string
		message := "Перенос вместе с родительской статьёй"
		if item.ID == plan.req.ID {
			parent = &t.ParentID
			message = "Перенос"
		}
		if merr := s.moveArticleRevision(tx, item, t.SpaceID, t.ProjectID, parent, message, now, author); merr != nil {
			return nil, merr
		}
	}
	if err := corporateAudit(tx, plan.actor, "move.article", "article", plan.req.ID, moveAuditDetails(plan)); err != nil {
		return nil, moveFail(500, "database error")
	}
	root, err := moveLoadArticle(tx, plan.req.ID)
	if err != nil {
		return nil, moveFail(500, "database error")
	}
	return map[string]any{"articles": len(plan.articles), "projects": 0, "root": root}, nil
}

func (s *Server) moveApplyProject(tx *transaction, plan *movePlan) (map[string]any, *moveError) {
	if err := moveSetMarker(tx, "project", plan.req.ID); err != nil {
		return nil, moveFail(500, "database error")
	}
	// The articles are read before the projects move: afterwards their stored
	// space no longer matches their project and the read policy hides them.
	loaded, merr := moveLoadPlanned(tx, plan.articles)
	if merr != nil {
		return nil, merr
	}
	t := plan.req.Target
	for _, p := range plan.projects {
		var err error
		if p.ID == plan.req.ID {
			_, err = tx.Exec(`UPDATE projects SET parent_id=?,space_id=? WHERE id=?`, t.ParentID, t.SpaceID, p.ID)
		} else if p.SpaceID != t.SpaceID {
			_, err = tx.Exec(`UPDATE projects SET space_id=? WHERE id=?`, t.SpaceID, p.ID)
		}
		if err != nil {
			return nil, moveDBFail(err)
		}
	}
	now, author := stamp(), s.changeAuthor(plan.actor)
	for _, item := range loaded {
		if merr := s.moveArticleRevision(tx, item, t.SpaceID, item.ProjectID, nil, "Перенос вместе с проектом", now, author); merr != nil {
			return nil, merr
		}
	}
	if err := corporateAudit(tx, plan.actor, "move.project", "project", plan.req.ID, moveAuditDetails(plan)); err != nil {
		return nil, moveFail(500, "database error")
	}
	var root corporateProject
	var inherit int
	if err := tx.QueryRow(`SELECT id,space_id,parent_id,name,description,inherit_acl,coalesce(archived_at,'') FROM projects WHERE id=?`, plan.req.ID).Scan(&root.ID, &root.SpaceID, &root.ParentID, &root.Name, &root.Description, &inherit, &root.ArchivedAt); err != nil {
		return nil, moveFail(500, "database error")
	}
	root.InheritACL = inherit != 0
	return map[string]any{"articles": len(plan.articles), "projects": len(plan.projects), "root": root}, nil
}

// moveLoadArticle reads an article without FOR UPDATE: the row lock would need
// the UPDATE policy and hide articles of archived containers, which the move
// rules must report as a conflict rather than as "not found".
func moveLoadArticle(tx *transaction, id string) (Article, error) {
	return scanArticle(tx.QueryRow("SELECT "+articleColumns+" FROM articles WHERE id=?", id))
}

// moveLoadPlanned loads the planned articles and verifies their revisions.
func moveLoadPlanned(tx *transaction, items []moveArticle) ([]Article, *moveError) {
	out := make([]Article, 0, len(items))
	for _, item := range items {
		a, err := moveLoadArticle(tx, item.ID)
		if err != nil || a.Revision != item.Revision {
			return nil, moveFail(409, "article changed during the move")
		}
		out = append(out, a)
	}
	return out, nil
}
