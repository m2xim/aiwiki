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
	// moveMaxArtifacts bounds the artifacts visible to the actor in a moved subtree.
	moveMaxArtifacts = 2000
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
	Artifacts    int           `json:"artifacts"`
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

// moveArtifact is an artifact of a moved project subtree that the actor can see.
type moveArtifact struct {
	ID       string
	Revision int
}

type moveProject struct {
	ID       string
	ParentID string
	SpaceID  string
	archived bool
}

type movePlan struct {
	req      moveRequest
	actor    string
	articles []moveArticle // article kind: root first; project kind: articles whose space changes
	projects []moveProject // project kind: parents before children
	// artifacts: project kind, space change only; visible to the actor (their RLS)
	artifacts []moveArtifact
	preview   movePreview
	root      Article
	project   corporateProject
	access    []moveAccess
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

func moveContainerInfo(q interface{ QueryRow(string, ...any) *sql.Row }, actor, space, project string) moveContainer {
	c := moveContainer{SpaceID: space, ProjectID: project}
	// Names are shown only to a reader of that very space or project.
	if moveRank(q, actor, space, "") >= 1 {
		_ = q.QueryRow(`SELECT name FROM spaces WHERE id=?`, space).Scan(&c.SpaceName)
	}
	if project != "" && moveRank(q, actor, space, project) >= 1 {
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
	plan, merr := s.movePlan(tx, actor, req, apply)
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
		// The deferred artifact checks run at commit: a concurrent change (for example an
		// artifact created in a moved project meanwhile) rolls this move back.
		if strings.Contains(err.Error(), "SQLSTATE 42501") {
			apiErr(w, http.StatusConflict, "concurrent change, retry")
			return
		}
		apiErr(w, 500, "database error")
		return
	}
	writeJSON(w, 200, out)
}

// movePlan validates every rule of docs/MOVES.md against the current database
// state (inside tx) and builds the preview. It is used by preview and apply.
func (s *Server) movePlan(tx *transaction, actor string, req moveRequest, lock bool) (*movePlan, *moveError) {
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
			parent, err := moveLoadArticleFor(tx, t.ParentID, lockSuffix(lock, " FOR SHARE"))
			if err != nil || parent.SpaceID != t.SpaceID || parent.ProjectID != t.ProjectID {
				return nil, moveFail(400, "target parent must be in the target container")
			}
			if parent.DeletedAt != "" {
				return nil, moveFail(409, "parent article is deleted")
			}
		}
		load := func() ([]moveArticle, *moveError) {
			rows, err := tx.Query(`WITH RECURSIVE sub(id,path) AS (
			SELECT id,ARRAY[id]::text[] FROM articles WHERE id=?
			UNION ALL
			SELECT a.id,sub.path||a.id FROM articles a JOIN sub ON a.parent_id=sub.id WHERE NOT a.id=ANY(sub.path)
		) SELECT a.id,a.revision,a.parent_id,a.space_id,a.project_id,coalesce(a.deleted_at,'')<>'' FROM articles a WHERE a.id IN (SELECT id FROM sub) ORDER BY a.id LIMIT ?`+lockSuffix(lock, " FOR NO KEY UPDATE OF a"), req.ID, moveMaxArticles+1)
			if err != nil {
				return nil, moveDBFail(err)
			}
			defer rows.Close()
			var items []moveArticle
			for rows.Next() {
				var item moveArticle
				var space, project string
				if err = rows.Scan(&item.ID, &item.Revision, &item.Parent, &space, &project, &item.Deleted); err != nil {
					return nil, moveFail(500, "database error")
				}
				if space != root.SpaceID || project != root.ProjectID {
					return nil, moveFail(409, "article subtree spans several containers")
				}
				items = append(items, item)
			}
			if rows.Err() != nil {
				return nil, moveFail(500, "database error")
			}
			return items, nil
		}
		items, merr := load()
		if merr != nil {
			return nil, merr
		}
		// Locking pass: a concurrent child creation holds a share lock on its
		// parent, so the lock above may have waited for it. Read the subtree again
		// (new snapshot) until it is stable; afterwards every row is locked and no
		// new child can be attached to a locked parent.
		for pass := 0; lock; pass++ {
			again, merr := load()
			if merr != nil {
				return nil, merr
			}
			same := len(again) == len(items)
			for i := 0; same && i < len(items); i++ {
				same = items[i].ID == again[i].ID && items[i].Revision == again[i].Revision
			}
			items = again
			if same {
				break
			}
			if pass >= 3 {
				return nil, moveFail(409, "article subtree is changing; retry")
			}
		}
		deleted := 0
		for _, item := range items {
			if item.Deleted {
				deleted++
			}
			if lock && item.ID == req.ID && item.Revision != req.BaseRevision {
				return nil, moveFail(409, fmt.Sprintf("revision conflict: current revision is %d", item.Revision))
			}
		}
		plan.articles = items
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
		sourceInfo = moveContainerInfo(tx, actor, root.SpaceID, root.ProjectID)
		targetInfo = moveContainerInfo(tx, actor, t.SpaceID, t.ProjectID)
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
			return nil, moveFail(403, movePermProject)
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
			return nil, moveFail(403, movePermProject)
		}
		loadProjects := func() ([]moveProject, bool, *moveError) {
			rows, err := tx.Query(`WITH RECURSIVE sub(id,depth,path) AS (
			SELECT id,0,ARRAY[id]::text[] FROM projects WHERE id=?
			UNION ALL
			SELECT c.id,sub.depth+1,sub.path||c.id FROM projects c JOIN sub ON c.parent_id=sub.id WHERE NOT c.id=ANY(sub.path) AND sub.depth<63
		) SELECT p.id,p.parent_id,p.space_id,coalesce(p.archived_at,'')<>'',corporate_role_rank(corporate_effective_role(?,p.space_id,p.id)) FROM sub JOIN projects p ON p.id=sub.id ORDER BY sub.depth,p.id LIMIT ?`+lockSuffix(lock, " FOR NO KEY UPDATE OF p"), req.ID, actor, moveMaxProjects+1)
			if err != nil {
				return nil, false, moveDBFail(err)
			}
			defer rows.Close()
			var items []moveProject
			seen := map[string]bool{}
			denied := false
			for rows.Next() {
				var item moveProject
				var archived bool
				var rank int
				if err = rows.Scan(&item.ID, &item.ParentID, &item.SpaceID, &archived, &rank); err != nil {
					return nil, false, moveFail(500, "database error")
				}
				if seen[item.ID] {
					continue
				}
				seen[item.ID] = true
				if rank < 4 {
					denied = true
				}
				if archived {
					item.archived = true
				}
				items = append(items, item)
			}
			if rows.Err() != nil {
				return nil, false, moveFail(500, "database error")
			}
			return items, denied, nil
		}
		projects, denied, merr := loadProjects()
		if merr != nil {
			return nil, merr
		}
		for pass := 0; lock; pass++ {
			again, againDenied, merr := loadProjects()
			if merr != nil {
				return nil, merr
			}
			same := len(again) == len(projects)
			for i := 0; same && i < len(projects); i++ {
				same = projects[i].ID == again[i].ID && projects[i].ParentID == again[i].ParentID
			}
			projects, denied = again, againDenied
			if same {
				break
			}
			if pass >= 3 {
				return nil, moveFail(409, "project subtree is changing; retry")
			}
		}
		if len(projects) > moveMaxProjects {
			return nil, moveFail(400, "move too large")
		}
		// One answer for every permission refusal, so a hidden sub-project cannot be probed.
		if denied {
			return nil, moveFail(403, movePermProject)
		}
		for _, item := range projects {
			if item.archived {
				return nil, moveFail(409, "the project subtree contains an archived project")
			}
			if item.ID == t.ParentID {
				return nil, moveFail(400, "a project cannot be moved into itself or its descendants")
			}
		}
		plan.projects = projects
		if !moveWritable(tx, p.SpaceID, p.ID) || !moveWritable(tx, t.SpaceID, t.ParentID) {
			return nil, moveFail(409, "source or target container is archived")
		}
		arows, err := tx.Query(`WITH RECURSIVE sub(id,depth,path) AS (
			SELECT id,0,ARRAY[id]::text[] FROM projects WHERE id=?
			UNION ALL
			SELECT c.id,sub.depth+1,sub.path||c.id FROM projects c JOIN sub ON c.parent_id=sub.id WHERE NOT c.id=ANY(sub.path) AND sub.depth<63
		) SELECT a.id,a.revision,a.parent_id,coalesce(a.deleted_at,'')<>'' FROM articles a WHERE a.project_id IN (SELECT id FROM sub) AND a.space_id<>? ORDER BY a.id LIMIT ?`+lockSuffix(lock, " FOR NO KEY UPDATE OF a"), req.ID, t.SpaceID, moveMaxArticles+1)
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
		if p.SpaceID != t.SpaceID {
			// Artifacts follow their project. Only those the actor can see are listed
			// (ordinary RLS), so hidden private ones are neither counted nor disclosed.
			// The warning does not depend on what the actor sees.
			xrows, err := tx.Query(`WITH RECURSIVE sub(id,depth,path) AS (
			SELECT id,0,ARRAY[id]::text[] FROM projects WHERE id=?
			UNION ALL
			SELECT c.id,sub.depth+1,sub.path||c.id FROM projects c JOIN sub ON c.parent_id=sub.id WHERE NOT c.id=ANY(sub.path) AND sub.depth<63
		) SELECT a.id,a.revision FROM artifacts a WHERE a.project_id IN (SELECT id FROM sub) ORDER BY a.id LIMIT ?`, req.ID, moveMaxArtifacts+1)
			if err != nil {
				return nil, moveDBFail(err)
			}
			for xrows.Next() {
				var item moveArtifact
				if err = xrows.Scan(&item.ID, &item.Revision); err != nil {
					xrows.Close()
					return nil, moveFail(500, "database error")
				}
				plan.artifacts = append(plan.artifacts, item)
			}
			err = xrows.Err()
			xrows.Close()
			if err != nil {
				return nil, moveFail(500, "database error")
			}
			if len(plan.artifacts) > moveMaxArtifacts {
				return nil, moveFail(400, "move too large")
			}
			warnings = append(warnings, "Артефакты проекта переедут вместе с ним. Авторы личных артефактов сохранят к ним доступ, только если у них есть права в новом месте; их публичные ссылки перестанут работать, если прав не будет.")
		}
		plan.project = p
		tcontainer = t.ParentID
		sourceInfo = moveContainerInfo(tx, actor, p.SpaceID, p.ParentID)
		targetInfo = moveContainerInfo(tx, actor, t.SpaceID, t.ParentID)
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
		if kind == "self" {
			selfAfter = moveRoleRank[after]
			continue
		}
		if kind == "user" {
			switch {
			case moveRoleRank[after] > moveRoleRank[before]:
				gaining++
			case moveRoleRank[after] < moveRoleRank[before]:
				losing++
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
		if (access[i].SubjectType == "other") != (access[j].SubjectType == "other") {
			return access[j].SubjectType == "other"
		}
		if access[i].SubjectType != access[j].SubjectType {
			return access[i].SubjectType < access[j].SubjectType
		}
		if access[i].SubjectID != access[j].SubjectID {
			return access[i].SubjectID < access[j].SubjectID
		}
		bi, bj := moveRoleRank[derefRole(access[i].Before)], moveRoleRank[derefRole(access[j].Before)]
		if bi != bj {
			return bi < bj
		}
		return moveRoleRank[derefRole(access[i].After)] < moveRoleRank[derefRole(access[j].After)]
	})
	// Counts are shown by the client from users_gaining/users_losing; warn only
	// when someone who had no access before can now read the moved content.
	newReaders := false
	for _, row := range access {
		if row.Before == nil && row.After != nil {
			newReaders = true
		}
	}
	if newReaders {
		warnings = append(warnings, "Новым участникам станут видны комментарии и вложения перемещаемых статей; прежние версии останутся видны только тем, у кого был доступ к прежнему месту.")
	}
	if warnings == nil {
		warnings = []string{}
	}
	plan.access = access
	plan.preview = movePreview{Articles: len(plan.articles), Projects: len(plan.projects), Artifacts: len(plan.artifacts), Source: sourceInfo, Target: targetInfo,
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
	artifacts := make([]item, 0, len(plan.artifacts))
	for _, a := range plan.artifacts {
		artifacts = append(artifacts, item{ID: a.ID, A: fmt.Sprint(a.Revision)})
	}
	sort.Slice(artifacts, func(i, j int) bool { return artifacts[i].ID < artifacts[j].ID })
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
		V         int        `json:"v"`
		Kind      string     `json:"kind"`
		ID        string     `json:"id"`
		Target    moveTarget `json:"target"`
		Actor     string     `json:"actor"`
		Base      int        `json:"base"`
		Articles  []item     `json:"articles"`
		Projects  []item     `json:"projects"`
		Artifacts []item     `json:"artifacts"`
		Grants    []grant    `json:"grants"`
		Gaining   int        `json:"gaining"`
		Losing    int        `json:"losing"`
	}{1, plan.req.Kind, plan.req.ID, plan.req.Target, plan.actor, plan.req.BaseRevision, articles, projects, artifacts, grants, plan.preview.UsersGaining, plan.preview.UsersLosing})
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
	// Artifacts of the whole subtree (inherit, private and deleted ones, also
	// those the actor cannot see) follow in the same transaction. The database
	// function reads the target space from the projects and counts what it moved.
	var artifactsMoved int
	if err := tx.QueryRow(`SELECT artifact_follow_project_move(?)`, plan.req.ID).Scan(&artifactsMoved); err != nil {
		return nil, moveDBFail(err)
	}
	details := moveAuditDetails(plan)
	details["artifacts"] = artifactsMoved
	if err := corporateAudit(tx, plan.actor, "move.project", "project", plan.req.ID, details); err != nil {
		return nil, moveFail(500, "database error")
	}
	var root corporateProject
	var inherit int
	if err := tx.QueryRow(`SELECT id,space_id,parent_id,name,description,inherit_acl,coalesce(archived_at,'') FROM projects WHERE id=?`, plan.req.ID).Scan(&root.ID, &root.SpaceID, &root.ParentID, &root.Name, &root.Description, &inherit, &root.ArchivedAt); err != nil {
		return nil, moveFail(500, "database error")
	}
	root.InheritACL = inherit != 0
	// "artifacts" is the number visible to the actor, as in the preview.
	return map[string]any{"articles": len(plan.articles), "projects": len(plan.projects), "artifacts": len(plan.artifacts), "root": root}, nil
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

// movePermProject is the single answer to every permission refusal of a project
// move (owner, target, sub-project roles), so the reason cannot be probed.
const movePermProject = "move is not permitted: a project move needs the owner role on the project and the manager role on the target and on all its sub-projects"

func lockSuffix(lock bool, suffix string) string {
	if lock {
		return suffix
	}
	return ""
}

// moveLoadArticleFor reads an article with an optional locking suffix.
func moveLoadArticleFor(tx *transaction, id, suffix string) (Article, error) {
	return scanArticle(tx.QueryRow("SELECT "+articleColumns+" FROM articles WHERE id=?"+suffix, id))
}

func derefRole(v *string) string {
	if v == nil {
		return ""
	}
	return *v
}
