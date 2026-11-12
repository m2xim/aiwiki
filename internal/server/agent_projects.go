package server

import (
	"database/sql"
	"errors"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"
)

// Agent projects ("Проекты"): named sets of spaces with a read or write mode. A project grants no
// rights; it only limits an agent. People in a browser are not limited by projects.
//
// All reads go through two SECURITY DEFINER functions that apply the visibility rules in the database
// (own projects, public projects, and for an MCP session only the keys' projects). A space the viewer
// cannot read is returned without id, name or role. Writes use ordinary statements under the caller's
// row-level security policies; nothing here is reachable from a scoped MCP session.

const (
	agentProjectMaxSpaces = 20
	agentProjectMaxName   = 120
	agentProjectMaxDesc   = 2000
)

type agentProjectSpace struct {
	RowID      string `json:"row_id"`
	Mode       string `json:"mode"`
	Accessible bool   `json:"accessible"`
	SpaceID    string `json:"space_id,omitempty"`
	Name       string `json:"name,omitempty"`
	MyRole     string `json:"my_role,omitempty"`
	Effective  string `json:"effective"`
}

type agentProject struct {
	ID          string              `json:"id"`
	Name        string              `json:"name"`
	Description string              `json:"description"`
	OwnerID     string              `json:"owner_id"`
	OwnerName   string              `json:"owner_name"`
	Visibility  string              `json:"visibility"`
	CanEdit     bool                `json:"can_edit"`
	CreatedAt   string              `json:"created_at"`
	UpdatedAt   string              `json:"updated_at"`
	Spaces      []agentProjectSpace `json:"spaces"`
}

// agentEffective is min(project mode, own role): write needs editor+, comment needs commenter, any
// readable space reads. An unreadable space yields "".
func agentEffective(mode, role string, readOnly bool) string {
	rank := map[string]int{"reader": 1, "commenter": 2, "editor": 3, "manager": 3, "owner": 3}[role]
	switch {
	case rank == 0:
		return ""
	case mode == "write" && !readOnly && rank >= 3:
		return "write"
	case mode == "write" && !readOnly && rank == 2:
		return "comment"
	}
	return "read"
}

type agentQuery interface {
	Query(string, ...any) (*sql.Rows, error)
	QueryRow(string, ...any) *sql.Row
}

// loadAgentProjects returns the visible projects (all when ids is nil) with their rows, ordered by name.
func loadAgentProjects(q agentQuery, ids []string, readOnly bool) ([]agentProject, error) {
	var idArg any
	if ids != nil {
		idArg = ids
	}
	rows, err := q.Query(`SELECT id,name,description,owner_id,owner_name,visibility,can_edit,created_at,updated_at FROM corporate_agent_projects_visible(?::text[])`, idArg)
	if err != nil {
		return nil, err
	}
	out := make([]agentProject, 0)
	index := map[string]int{}
	for rows.Next() {
		var p agentProject
		var created, updated time.Time
		if err = rows.Scan(&p.ID, &p.Name, &p.Description, &p.OwnerID, &p.OwnerName, &p.Visibility, &p.CanEdit, &created, &updated); err != nil {
			rows.Close()
			return nil, err
		}
		p.CreatedAt, p.UpdatedAt, p.Spaces = created.UTC().Format(time.RFC3339), updated.UTC().Format(time.RFC3339), []agentProjectSpace{}
		index[p.ID] = len(out)
		out = append(out, p)
	}
	err = rows.Err()
	rows.Close()
	if err != nil || len(out) == 0 {
		return out, err
	}
	have := make([]string, 0, len(out))
	for _, p := range out {
		have = append(have, p.ID)
	}
	rows, err = q.Query(`SELECT project_id,row_id,mode,accessible,coalesce(space_id,''),coalesce(name,''),coalesce(my_role,'') FROM corporate_agent_project_rows(?::text[])`, have)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var pid string
		var r agentProjectSpace
		if err = rows.Scan(&pid, &r.RowID, &r.Mode, &r.Accessible, &r.SpaceID, &r.Name, &r.MyRole); err != nil {
			return nil, err
		}
		r.Effective = agentEffective(r.Mode, r.MyRole, readOnly)
		if i, ok := index[pid]; ok {
			out[i].Spaces = append(out[i].Spaces, r)
		}
	}
	return out, rows.Err()
}

func (s *Server) registerAgentProjectRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/api/agent-projects", s.agentProjects)
	mux.HandleFunc("/api/agent-projects/{id}", s.agentProject)
}

// agentProjectActor admits only a human browser session of a live, local user.
func (s *Server) agentProjectActor(w http.ResponseWriter, r *http.Request) (string, bool) {
	actor, ok := s.corporateActor(w, r, false)
	if !ok {
		return "", false
	}
	if !s.outsideMCP() {
		apiErr(w, http.StatusForbidden, "access denied")
		return "", false
	}
	return actor, true
}

func (s *Server) agentProjects(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.agentProjectActor(w, r)
	if !ok {
		return
	}
	switch r.Method {
	case http.MethodGet:
		list, err := loadAgentProjects(s.DB, nil, false)
		if err != nil {
			apiErr(w, 500, "database error")
			return
		}
		writeJSON(w, 200, map[string]any{"projects": list, "general_space_id": s.generalSpaceID(s.DB)})
	case http.MethodPost:
		s.agentProjectCreate(w, r, actor)
	default:
		apiErr(w, 405, "method not allowed")
	}
}

type agentProjectSpaceIn struct {
	RowID   string `json:"row_id"`
	SpaceID string `json:"space_id"`
	Mode    string `json:"mode"`
}

type agentProjectInput struct {
	Name        *string                `json:"name"`
	Description *string                `json:"description"`
	Visibility  *string                `json:"visibility"`
	Spaces      *[]agentProjectSpaceIn `json:"spaces"`
}

func agentProjectText(in agentProjectInput) (name, desc string, ok bool) {
	if in.Name != nil {
		name, ok = corporateName(*in.Name, agentProjectMaxName)
		if !ok {
			return "", "", false
		}
	}
	if in.Description != nil {
		desc = *in.Description
		if utf8.RuneCountInString(desc) > agentProjectMaxDesc || strings.ContainsRune(desc, 0) {
			return "", "", false
		}
	}
	if in.Name != nil && strings.ContainsRune(name, 0) {
		return "", "", false
	}
	return name, desc, true
}

func (s *Server) agentProjectCreate(w http.ResponseWriter, r *http.Request, actor string) {
	var in agentProjectInput
	if corporateDecode(r, &in) != nil || in.Name == nil {
		apiErr(w, 400, "invalid project")
		return
	}
	name, desc, ok := agentProjectText(in)
	if !ok {
		apiErr(w, 400, "invalid project")
		return
	}
	visibility := "private"
	if in.Visibility != nil {
		visibility = *in.Visibility
	}
	if visibility != "private" && visibility != "public" {
		apiErr(w, 400, "invalid project")
		return
	}
	var spaces []agentProjectSpaceIn
	if in.Spaces != nil {
		spaces = *in.Spaces
	}
	if len(spaces) > agentProjectMaxSpaces {
		apiErr(w, 400, "too many spaces")
		return
	}
	seen := map[string]bool{}
	for _, sp := range spaces {
		if sp.RowID != "" || !validPathID(sp.SpaceID) || (sp.Mode != "read" && sp.Mode != "write") || seen[sp.SpaceID] {
			apiErr(w, 400, "invalid project")
			return
		}
		seen[sp.SpaceID] = true
	}
	id := newID()
	tx, err := s.DB.Begin()
	if err != nil {
		apiErr(w, 500, "database error")
		return
	}
	defer tx.Rollback()
	for _, sp := range spaces {
		if !agentSpaceVisible(tx, sp.SpaceID) {
			apiErr(w, 400, "space not visible")
			return
		}
	}
	if _, err = tx.Exec(`INSERT INTO agent_projects(id,name,description,owner_id,visibility) VALUES(?,?,?,?,?)`, id, name, desc, actor, visibility); err != nil {
		apiErr(w, 500, "database error")
		return
	}
	added := make([]map[string]string, 0, len(spaces))
	for _, sp := range spaces {
		if _, err = tx.Exec(`INSERT INTO agent_project_spaces(project_id,row_id,space_id,mode) VALUES(?,?,?,?)`, id, newID(), sp.SpaceID, sp.Mode); err != nil {
			agentWriteErr(w, err)
			return
		}
		added = append(added, map[string]string{"space_id": sp.SpaceID, "mode": sp.Mode})
	}
	if err = corporateAudit(tx, actor, "agent_project.create", "agent_project", id, map[string]any{"name": name, "visibility": visibility, "spaces": added}); err != nil {
		apiErr(w, 500, "database error")
		return
	}
	list, err := loadAgentProjects(tx, []string{id}, false)
	if err != nil || len(list) != 1 {
		apiErr(w, 500, "database error")
		return
	}
	if err = tx.Commit(); err != nil {
		apiErr(w, 500, "database error")
		return
	}
	writeJSON(w, 201, list[0])
}

// agentSpaceVisible: the caller reads the space (any role; the general space counts).
func agentSpaceVisible(q agentQuery, spaceID string) bool {
	var ok bool
	return q.QueryRow(`SELECT corporate_ap_space_addable(?)`, spaceID).Scan(&ok) == nil && ok
}

func agentWriteErr(w http.ResponseWriter, err error) {
	text := err.Error()
	switch {
	case strings.Contains(text, "SQLSTATE 42501"), strings.Contains(text, "row-level security"):
		apiErr(w, 403, "access denied")
	case strings.Contains(text, "too many spaces"):
		apiErr(w, 400, "too many spaces")
	case strings.Contains(text, "SQLSTATE 23505"), strings.Contains(text, "SQLSTATE 23503"), strings.Contains(text, "SQLSTATE 23514"):
		apiErr(w, 400, "invalid project")
	default:
		apiErr(w, 500, "database error")
	}
}

func (s *Server) agentProject(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.agentProjectActor(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if !validPathID(id) {
		apiErr(w, 404, "not found")
		return
	}
	switch r.Method {
	case http.MethodGet:
		list, err := loadAgentProjects(s.DB, []string{id}, false)
		if err != nil {
			apiErr(w, 500, "database error")
			return
		}
		if len(list) != 1 {
			apiErr(w, 404, "not found")
			return
		}
		writeJSON(w, 200, list[0])
	case http.MethodPatch:
		s.agentProjectPatch(w, r, actor, id)
	case http.MethodDelete:
		s.agentProjectDelete(w, actor, id)
	default:
		apiErr(w, 405, "method not allowed")
	}
}

func (s *Server) agentProjectPatch(w http.ResponseWriter, r *http.Request, actor, id string) {
	var in agentProjectInput
	if corporateDecode(r, &in) != nil || in.Name == nil && in.Description == nil && in.Visibility == nil && in.Spaces == nil {
		apiErr(w, 400, "invalid project")
		return
	}
	name, desc, ok := agentProjectText(in)
	if !ok {
		apiErr(w, 400, "invalid project")
		return
	}
	if in.Visibility != nil && *in.Visibility != "private" && *in.Visibility != "public" {
		apiErr(w, 400, "invalid project")
		return
	}
	var spaces []agentProjectSpaceIn
	if in.Spaces != nil {
		spaces = *in.Spaces
		if len(spaces) > agentProjectMaxSpaces {
			apiErr(w, 400, "too many spaces")
			return
		}
		seenRow, seenSpace := map[string]bool{}, map[string]bool{}
		for _, sp := range spaces {
			if (sp.Mode != "read" && sp.Mode != "write") || (sp.RowID == "") == (sp.SpaceID == "") ||
				sp.RowID != "" && (!validPathID(sp.RowID) || seenRow[sp.RowID]) ||
				sp.SpaceID != "" && (!validPathID(sp.SpaceID) || seenSpace[sp.SpaceID]) {
				apiErr(w, 400, "invalid project")
				return
			}
			seenRow[sp.RowID], seenSpace[sp.SpaceID] = true, true
		}
	}
	tx, err := s.DB.Begin()
	if err != nil {
		apiErr(w, 500, "database error")
		return
	}
	defer tx.Rollback()
	// The row is visible to the caller (own or public) or the project does not exist for them.
	var curName, curDesc, owner, vis string
	if err = tx.QueryRow(`SELECT name,description,owner_id,visibility FROM agent_projects WHERE id=? FOR UPDATE`, id).Scan(&curName, &curDesc, &owner, &vis); err != nil {
		if errors.Is(err, sql.ErrNoRows) || strings.Contains(err.Error(), "row-level security") {
			// FOR UPDATE on a row the UPDATE policy hides: it is visible to read but not to edit.
			var visible bool
			if q := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM agent_projects WHERE id=?)`, id).Scan(&visible); q == nil && visible {
				apiErr(w, 403, "access denied")
				return
			}
			apiErr(w, 404, "not found")
			return
		}
		apiErr(w, 500, "database error")
		return
	}
	if in.Visibility != nil && *in.Visibility != vis && owner != actor {
		apiErr(w, 403, "access denied")
		return
	}
	// Editing is the owner's, or an administrator's for a public project; the policies enforce it too.
	if owner != actor && !(vis == "public" && s.adminOutsideMCP(tx)) {
		apiErr(w, 403, "access denied")
		return
	}
	diff := map[string]any{}
	newName, newDesc, newVis := curName, curDesc, vis
	if in.Name != nil && name != curName {
		newName, diff["name"] = name, map[string]string{"old": curName, "new": name}
	}
	if in.Description != nil && desc != curDesc {
		newDesc, diff["description_changed"] = desc, true
	}
	if in.Visibility != nil && *in.Visibility != vis {
		newVis, diff["visibility"] = *in.Visibility, map[string]string{"old": vis, "new": *in.Visibility}
	}
	if in.Spaces != nil {
		type existing struct{ spaceID, mode string }
		have := map[string]existing{}
		rows, qerr := tx.Query(`SELECT row_id,space_id,mode FROM agent_project_spaces WHERE project_id=?`, id)
		if qerr != nil {
			apiErr(w, 500, "database error")
			return
		}
		for rows.Next() {
			var rid string
			var e existing
			if rows.Scan(&rid, &e.spaceID, &e.mode) != nil {
				rows.Close()
				apiErr(w, 500, "database error")
				return
			}
			have[rid] = e
		}
		qerr = rows.Err()
		rows.Close()
		if qerr != nil {
			apiErr(w, 500, "database error")
			return
		}
		keep := map[string]string{}
		var fresh []agentProjectSpaceIn
		for _, sp := range spaces {
			if sp.RowID != "" {
				if _, ok := have[sp.RowID]; !ok {
					apiErr(w, 400, "invalid project") // a row of another project (or none)
					return
				}
				keep[sp.RowID] = sp.Mode
			} else {
				fresh = append(fresh, sp)
			}
		}
		for _, sp := range fresh {
			if !agentSpaceVisible(tx, sp.SpaceID) {
				apiErr(w, 400, "space not visible")
				return
			}
		}
		var removed, added, changed []map[string]string
		for rid, e := range have {
			mode, kept := keep[rid]
			switch {
			case !kept:
				if _, err = tx.Exec(`DELETE FROM agent_project_spaces WHERE project_id=? AND row_id=?`, id, rid); err != nil {
					agentWriteErr(w, err)
					return
				}
				removed = append(removed, map[string]string{"space_id": e.spaceID, "mode": e.mode})
			case mode != e.mode:
				if _, err = tx.Exec(`UPDATE agent_project_spaces SET mode=? WHERE project_id=? AND row_id=?`, mode, id, rid); err != nil {
					agentWriteErr(w, err)
					return
				}
				changed = append(changed, map[string]string{"space_id": e.spaceID, "old": e.mode, "new": mode})
			}
		}
		for _, sp := range fresh {
			if _, err = tx.Exec(`INSERT INTO agent_project_spaces(project_id,row_id,space_id,mode) VALUES(?,?,?,?)`, id, newID(), sp.SpaceID, sp.Mode); err != nil {
				agentWriteErr(w, err)
				return
			}
			added = append(added, map[string]string{"space_id": sp.SpaceID, "mode": sp.Mode})
		}
		if len(removed)+len(added)+len(changed) > 0 {
			diff["spaces_removed"], diff["spaces_added"], diff["spaces_changed"] = removed, added, changed
		}
	}
	res, err := tx.Exec(`UPDATE agent_projects SET name=?,description=?,visibility=?,updated_at=now() WHERE id=?`, newName, newDesc, newVis, id)
	if err != nil {
		agentWriteErr(w, err)
		return
	}
	if n, _ := res.RowsAffected(); n != 1 {
		apiErr(w, 403, "access denied")
		return
	}
	if err = corporateAudit(tx, actor, "agent_project.update", "agent_project", id, diff); err != nil {
		apiErr(w, 500, "database error")
		return
	}
	list, err := loadAgentProjects(tx, []string{id}, false)
	if err != nil || len(list) != 1 {
		apiErr(w, 500, "database error")
		return
	}
	if err = tx.Commit(); err != nil {
		agentWriteErr(w, err)
		return
	}
	writeJSON(w, 200, list[0])
}

func (s *Server) agentProjectDelete(w http.ResponseWriter, actor, id string) {
	tx, err := s.DB.Begin()
	if err != nil {
		apiErr(w, 500, "database error")
		return
	}
	defer tx.Rollback()
	var name, owner, vis string
	if err = tx.QueryRow(`SELECT name,owner_id,visibility FROM agent_projects WHERE id=?`, id).Scan(&name, &owner, &vis); err != nil {
		apiErr(w, 404, "not found")
		return
	}
	if owner != actor && !(vis == "public" && s.adminOutsideMCP(tx)) {
		apiErr(w, 403, "access denied")
		return
	}
	res, err := tx.Exec(`DELETE FROM agent_projects WHERE id=?`, id)
	if err != nil {
		agentWriteErr(w, err)
		return
	}
	if n, _ := res.RowsAffected(); n != 1 {
		apiErr(w, 403, "access denied")
		return
	}
	if err = corporateAudit(tx, actor, "agent_project.delete", "agent_project", id, map[string]any{"name": name, "visibility": vis, "owner_id": owner}); err != nil {
		apiErr(w, 500, "database error")
		return
	}
	if err = tx.Commit(); err != nil {
		apiErr(w, 500, "database error")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
