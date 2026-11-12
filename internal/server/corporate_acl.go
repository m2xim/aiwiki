package server

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

//go:embed corporate_schema.sql
var corporateSchema embed.FS

const corporateSchemaVersion = 1

// migrateCorporate applies the ACL schema atomically. The database role used
// here is the migration owner; the runtime role is provisioned separately.
func (s *Server) migrateCorporate() error {
	if s == nil || s.DB == nil || !s.DB.pg {
		return errors.New("corporate migration requires PostgreSQL")
	}
	schema, err := corporateSchema.ReadFile("corporate_schema.sql")
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := s.DB.runMigrations(ctx, "corporate_acl", []pgMigration{{Version: corporateSchemaVersion, Name: "corporate ACL and forced RLS", Statements: splitCorporateSQL(string(schema))}}); err != nil {
		return err
	}
	if err := s.migrateMCPScope(ctx); err != nil {
		return err
	}
	if err := s.migrateCorporateMoves(ctx); err != nil {
		return err
	}
	if err := s.migrateArtifacts(ctx); err != nil {
		return err
	}
	if err := s.migrateStorageQuotas(ctx); err != nil {
		return err
	}
	if err := s.migrateRevisionColumns(ctx); err != nil {
		return err
	}
	if err := s.migrateSemantic(ctx); err != nil {
		return err
	}
	if err := s.migrateAgentProjects(ctx); err != nil {
		return err
	}
	return s.migrateRoleScope(ctx)
}

// splitCorporateSQL separates commands while preserving function bodies,
// literals, quoted identifiers, and comments for drivers that prepare one
// command per Exec call.
func splitCorporateSQL(src string) []string {
	statements := make([]string, 0, 32)
	start := 0
	for i := 0; i < len(src); {
		switch {
		case src[i] == '\'':
			i++
			for i < len(src) {
				if src[i] == '\\' && i+1 < len(src) {
					i += 2
					continue
				}
				if src[i] == '\'' {
					i++
					if i < len(src) && src[i] == '\'' {
						i++
						continue
					}
					break
				}
				i++
			}
		case src[i] == '"':
			i++
			for i < len(src) {
				if src[i] == '"' {
					i++
					if i < len(src) && src[i] == '"' {
						i++
						continue
					}
					break
				}
				i++
			}
		case src[i] == '-' && i+1 < len(src) && src[i+1] == '-':
			if n := strings.IndexByte(src[i:], '\n'); n >= 0 {
				i += n + 1
			} else {
				i = len(src)
			}
		case src[i] == '/' && i+1 < len(src) && src[i+1] == '*':
			depth := 1
			i += 2
			for i < len(src) && depth > 0 {
				if i+1 < len(src) && src[i] == '/' && src[i+1] == '*' {
					depth++
					i += 2
				} else if i+1 < len(src) && src[i] == '*' && src[i+1] == '/' {
					depth--
					i += 2
				} else {
					i++
				}
			}
		case src[i] == '$':
			j := i + 1
			for j < len(src) && (src[j] == '_' || src[j] >= 'a' && src[j] <= 'z' || src[j] >= 'A' && src[j] <= 'Z' || j > i+1 && src[j] >= '0' && src[j] <= '9') {
				j++
			}
			if j < len(src) && src[j] == '$' {
				delim := src[i : j+1]
				if end := strings.Index(src[j+1:], delim); end >= 0 {
					i = j + 1 + end + len(delim)
				} else {
					i = len(src)
				}
			} else {
				i++
			}
		case src[i] == ';':
			if statement := strings.TrimSpace(src[start:i]); statement != "" {
				statements = append(statements, statement)
			}
			i++
			start = i
		default:
			i++
		}
	}
	if statement := strings.TrimSpace(src[start:]); statement != "" {
		statements = append(statements, statement)
	}
	return statements
}

// corporateRoutes registers security and administration APIs. Callers must
// place these routes behind the request-scoped authenticated Server.
func (s *Server) corporateRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/api/mcp/credentials", s.mcpCredentials)
	mux.HandleFunc("/api/mcp/credentials/{id}", s.mcpCredential)
	mux.HandleFunc("/api/mcp/service-accounts", s.mcpServiceAccounts)
	mux.HandleFunc("/api/mcp/service-accounts/{id}", s.mcpServiceAccount)
	mux.HandleFunc("GET /api/admin/users", s.corporateUsers)
	mux.HandleFunc("GET /api/admin/users/{id}/access", s.corporateUserAccess)
	mux.HandleFunc("GET /api/users", s.corporateUserDirectory)
	mux.HandleFunc("PATCH /api/admin/users/{id}", s.corporateUserDisable)
	mux.HandleFunc("GET /api/admin/audit", s.corporateAudit)
	mux.HandleFunc("/api/teams", s.corporateTeams)
	mux.HandleFunc("/api/teams/{id}", s.corporateTeam)
	mux.HandleFunc("/api/teams/{id}/members", s.corporateTeamMembers)
	mux.HandleFunc("/api/projects", projectsRemoved)
	mux.HandleFunc("/api/projects/{id}", projectsRemoved)
	s.registerAgentProjectRoutes(mux)
	mux.HandleFunc("/api/access", s.corporateAccess)
	mux.HandleFunc("GET /api/access/effective", s.corporateEffectiveAccess)
	mux.HandleFunc("GET /api/catalog", s.corporateCatalog)
	mux.HandleFunc("POST /api/corporate/spaces", s.corporateSpaceCreate)
	mux.HandleFunc("GET /api/admin/settings", s.adminSettingsGet)
	mux.HandleFunc("PUT /api/admin/settings", s.adminSettingsPut)
	s.registerMoveRoutes(mux)
}

func (s *Server) corporateActor(w http.ResponseWriter, r *http.Request, admin bool) (string, bool) {
	if s == nil || s.DB == nil || s.principal == nil || s.principal.ID == "" {
		apiErr(w, http.StatusForbidden, "forbidden")
		return "", false
	}
	var disabled, isAdmin int
	err := s.DB.QueryRow(`SELECT disabled,admin FROM users WHERE id=?`, s.principal.ID).Scan(&disabled, &isAdmin)
	if err != nil || disabled != 0 || admin && (isAdmin != 1 || !s.principal.Admin) {
		apiErr(w, http.StatusForbidden, "forbidden")
		return "", false
	}
	return s.principal.ID, true
}

func corporateDecode(r *http.Request, dst any) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return errors.New("invalid trailing data")
	}
	return nil
}

func corporateName(value string, max int) (string, bool) {
	value = strings.TrimSpace(value)
	return value, value != "" && utf8.RuneCountInString(value) <= max
}

func corporateAudit(tx *transaction, actor, action, resourceType, resourceID string, details any) error {
	raw, err := json.Marshal(details)
	if err != nil {
		return err
	}
	_, err = tx.Exec(`INSERT INTO security_audit(actor_id,action,resource_type,resource_id,details,created_at) VALUES(?,?,?,?,?,?)`, actor, action, resourceType, resourceID, string(raw), time.Now().UTC().Format(time.RFC3339Nano))
	return err
}

func corporateLockOwners(tx *transaction) error {
	_, err := tx.Exec(`SELECT pg_advisory_xact_lock(742109333)`)
	return err
}

func corporateTeamOwnersRemain(tx *transaction, teamID, excludeUser string, deactivateTeam bool) error {
	rows, err := tx.Query(`SELECT DISTINCT resource_type,resource_id FROM memberships WHERE role='owner' AND subject_type='team' AND subject_id=?`, teamID)
	if err != nil {
		return err
	}
	type resource struct{ kind, id string }
	resources := make([]resource, 0)
	for rows.Next() {
		var item resource
		if err = rows.Scan(&item.kind, &item.id); err != nil {
			rows.Close()
			return err
		}
		resources = append(resources, item)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, resource := range resources {
		var owners int
		err = tx.QueryRow(`SELECT count(DISTINCT u.id) FROM memberships m
LEFT JOIN team_members tm ON m.subject_type='team' AND tm.team_id=m.subject_id
JOIN users u ON (m.subject_type='user' AND u.id=m.subject_id OR m.subject_type='team' AND u.id=tm.user_id)
WHERE m.resource_type=? AND m.resource_id=? AND m.role='owner' AND u.disabled=0
AND (m.subject_type='user' OR EXISTS(SELECT 1 FROM teams t WHERE t.id=m.subject_id AND t.archived_at IS NULL))
AND (NOT ? OR m.subject_type<>'team' OR m.subject_id<>?)
AND (?='' OR u.id<>?)`, resource.kind, resource.id, deactivateTeam, teamID, excludeUser, excludeUser).Scan(&owners)
		if err != nil {
			return err
		}
		if owners == 0 {
			return errors.New("last active owner")
		}
	}
	return nil
}

func corporateRole(q interface{ QueryRow(string, ...any) *sql.Row }, who, kind, id string) (string, error) {
	if kind != "space" {
		return "", errors.New("invalid resource type")
	}
	var role sql.NullString
	err := q.QueryRow(`SELECT corporate_effective_role(?,?,'')`, who, id).Scan(&role)
	if err != nil || !role.Valid {
		return "", err
	}
	return role.String, nil
}

// projectsRemoved answers every request that still addresses the removed folders inside spaces.
func projectsRemoved(w http.ResponseWriter, r *http.Request) {
	apiErr(w, http.StatusGone, projectsRemovedText)
}

// outsideMCP reports a browser-style session: not a persistent key and not an OAuth bearer.
func (s *Server) outsideMCP() bool {
	p := s.principal
	return p != nil && p.MCPScope == nil && p.MCPKeyProjects == nil && p.MCPDelegationID == "" && p.MCPActorID == "" && !p.MCPBearer && !p.MCPIntrospect
}

// adminOutsideMCP: a system administrator in a browser session, confirmed by the users table.
func (s *Server) adminOutsideMCP(q interface{ QueryRow(string, ...any) *sql.Row }) bool {
	if !s.outsideMCP() || !s.principal.Admin {
		return false
	}
	var admin, disabled int
	return q.QueryRow(`SELECT admin,disabled FROM users WHERE id=?`, s.principal.ID).Scan(&admin, &disabled) == nil && admin == 1 && disabled == 0
}

// corporateSpaceRole is the caller's effective rank name on a space. A system administrator may manage
// the roles of every existing space, with or without a role of their own: without one the result is
// "owner" for the purpose of the role-management endpoints only (the content stays closed, the
// database policies do not follow this).
func (s *Server) corporateSpaceRole(q interface{ QueryRow(string, ...any) *sql.Row }, actor, id string) (role string, viaAdmin bool, err error) {
	role, err = corporateRole(q, actor, "space", id)
	if err != nil {
		return "", false, err
	}
	if s.adminOutsideMCP(q) {
		var exists bool
		if q.QueryRow(`SELECT corporate_admin_space_exists(?)`, id).Scan(&exists) == nil && exists && !corporateAtLeast(role, "owner") {
			return "owner", true, nil
		}
	}
	return role, false, nil
}

func corporateAtLeast(role, minimum string) bool {
	return map[string]int{"reader": 1, "commenter": 2, "editor": 3, "manager": 4, "owner": 5}[role] >=
		map[string]int{"reader": 1, "commenter": 2, "editor": 3, "manager": 4, "owner": 5}[minimum]
}

func (s *Server) corporateManageResource(w http.ResponseWriter, r *http.Request, q interface{ QueryRow(string, ...any) *sql.Row }, actor, kind, id string, ownerOnly bool) bool {
	if kind == "project" {
		apiErr(w, http.StatusGone, projectsRemovedText)
		return false
	}
	if kind != "space" {
		apiErr(w, http.StatusNotFound, "resource not found")
		return false
	}
	role, _, err := s.corporateSpaceRole(q, actor, id)
	if err != nil || !corporateAtLeast(role, map[bool]string{true: "owner", false: "manager"}[ownerOnly]) {
		apiErr(w, http.StatusNotFound, "resource not found")
		return false
	}
	return true
}

type corporateTeam struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	ArchivedAt  string `json:"archived_at,omitempty"`
}

func (s *Server) corporateTeams(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.corporateActor(w, r, true)
	if !ok {
		return
	}
	switch r.Method {
	case http.MethodGet:
		rows, err := s.DB.Query(`SELECT id,name,description,coalesce(archived_at,'') FROM teams ORDER BY name,id`)
		if err != nil {
			apiErr(w, 500, "database error")
			return
		}
		defer rows.Close()
		out := make([]corporateTeam, 0)
		for rows.Next() {
			var team corporateTeam
			if rows.Scan(&team.ID, &team.Name, &team.Description, &team.ArchivedAt) != nil {
				apiErr(w, 500, "database error")
				return
			}
			out = append(out, team)
		}
		if rows.Err() != nil {
			apiErr(w, 500, "database error")
			return
		}
		writeJSON(w, 200, out)
	case http.MethodPost:
		var in struct {
			Name        string `json:"name"`
			Description string `json:"description"`
		}
		if corporateDecode(r, &in) != nil {
			apiErr(w, 400, "invalid input")
			return
		}
		in.Name, ok = corporateName(in.Name, 160)
		if !ok || utf8.RuneCountInString(in.Description) > 2000 {
			apiErr(w, 400, "invalid input")
			return
		}
		team := corporateTeam{ID: newID(), Name: in.Name, Description: in.Description}
		tx, err := s.DB.Begin()
		if err != nil {
			apiErr(w, 500, "database error")
			return
		}
		defer tx.Rollback()
		if _, err = tx.Exec(`INSERT INTO teams(id,name,description) VALUES(?,?,?)`, team.ID, team.Name, team.Description); err == nil {
			err = corporateAudit(tx, actor, "team.create", "team", team.ID, map[string]any{"name": team.Name})
		}
		if err == nil {
			err = tx.Commit()
		}
		if err != nil {
			apiErr(w, 500, "database error")
			return
		}
		writeJSON(w, 201, team)
	default:
		apiErr(w, 405, "method not allowed")
	}
}

func (s *Server) corporateTeam(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.corporateActor(w, r, true)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if id == "" {
		apiErr(w, 404, "team not found")
		return
	}
	switch r.Method {
	case http.MethodGet:
		var team corporateTeam
		if err := s.DB.QueryRow(`SELECT id,name,description,coalesce(archived_at,'') FROM teams WHERE id=?`, id).Scan(&team.ID, &team.Name, &team.Description, &team.ArchivedAt); err != nil {
			apiErr(w, 404, "team not found")
			return
		}
		writeJSON(w, 200, team)
	case http.MethodPatch:
		var in struct {
			Name        *string `json:"name"`
			Description *string `json:"description"`
			Archived    *bool   `json:"archived"`
		}
		if corporateDecode(r, &in) != nil || in.Name == nil && in.Description == nil && in.Archived == nil {
			apiErr(w, 400, "invalid input")
			return
		}
		tx, err := s.DB.Begin()
		if err != nil {
			apiErr(w, 500, "database error")
			return
		}
		defer tx.Rollback()
		if in.Archived != nil && *in.Archived {
			if err = corporateLockOwners(tx); err != nil {
				apiErr(w, 500, "database error")
				return
			}
			if err = corporateTeamOwnersRemain(tx, id, "", true); err != nil {
				apiErr(w, 409, "team change would remove the last active owner")
				return
			}
		}
		var current corporateTeam
		if err = tx.QueryRow(`SELECT id,name,description,coalesce(archived_at,'') FROM teams WHERE id=? FOR UPDATE`, id).Scan(&current.ID, &current.Name, &current.Description, &current.ArchivedAt); err != nil {
			apiErr(w, 404, "team not found")
			return
		}
		if in.Name != nil {
			current.Name, ok = corporateName(*in.Name, 160)
			if !ok {
				apiErr(w, 400, "invalid input")
				return
			}
		}
		if in.Description != nil {
			if utf8.RuneCountInString(*in.Description) > 2000 {
				apiErr(w, 400, "invalid input")
				return
			}
			current.Description = *in.Description
		}
		if in.Archived != nil {
			if *in.Archived && current.ArchivedAt == "" {
				current.ArchivedAt = time.Now().UTC().Format(time.RFC3339)
			} else if !*in.Archived {
				current.ArchivedAt = ""
			}
		}
		if _, err = tx.Exec(`UPDATE teams SET name=?,description=?,archived_at=? WHERE id=?`, current.Name, current.Description, nullString(current.ArchivedAt), id); err == nil {
			err = corporateAudit(tx, actor, "team.update", "team", id, map[string]any{"changed": true})
		}
		if err == nil {
			err = tx.Commit()
		}
		if err != nil {
			apiErr(w, 500, "database error")
			return
		}
		writeJSON(w, 200, current)
	case http.MethodDelete:
		tx, err := s.DB.Begin()
		if err != nil {
			apiErr(w, 500, "database error")
			return
		}
		defer tx.Rollback()
		if err = corporateLockOwners(tx); err != nil {
			apiErr(w, 500, "database error")
			return
		}
		if err = corporateTeamOwnersRemain(tx, id, "", true); err != nil {
			apiErr(w, 409, "team change would remove the last active owner")
			return
		}
		if _, err = tx.Exec(`DELETE FROM memberships WHERE subject_type='team' AND subject_id=?`, id); err != nil {
			apiErr(w, 500, "database error")
			return
		}
		res, err := tx.Exec(`DELETE FROM teams WHERE id=?`, id)
		if err != nil {
			apiErr(w, 500, "database error")
			return
		}
		n, err := res.RowsAffected()
		if err != nil || n == 0 {
			apiErr(w, 404, "team not found")
			return
		}
		if err = corporateAudit(tx, actor, "team.delete", "team", id, map[string]any{}); err == nil {
			err = tx.Commit()
		}
		if err != nil {
			apiErr(w, 500, "database error")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		apiErr(w, 405, "method not allowed")
	}
}

func (s *Server) corporateTeamMembers(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.corporateActor(w, r, true); !ok {
		return
	}
	teamID := r.PathValue("id")
	if teamID == "" {
		apiErr(w, 404, "team not found")
		return
	}
	switch r.Method {
	case http.MethodGet:
		rows, err := s.DB.Query(`SELECT u.id,u.name FROM team_members tm JOIN users u ON u.id=tm.user_id WHERE tm.team_id=? ORDER BY u.name,u.id`, teamID)
		if err != nil {
			apiErr(w, 404, "team not found")
			return
		}
		defer rows.Close()
		type member struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		}
		out := make([]member, 0)
		for rows.Next() {
			var v member
			if rows.Scan(&v.ID, &v.Name) != nil {
				apiErr(w, 500, "database error")
				return
			}
			out = append(out, v)
		}
		if rows.Err() != nil {
			apiErr(w, 500, "database error")
			return
		}
		writeJSON(w, 200, out)
	case http.MethodPut, http.MethodDelete:
		var in struct {
			UserID string `json:"user_id"`
		}
		if corporateDecode(r, &in) != nil || strings.TrimSpace(in.UserID) == "" {
			apiErr(w, 400, "invalid input")
			return
		}
		tx, err := s.DB.Begin()
		if err != nil {
			apiErr(w, 500, "database error")
			return
		}
		defer tx.Rollback()
		if r.Method == http.MethodDelete {
			if err = corporateLockOwners(tx); err != nil {
				apiErr(w, 500, "database error")
				return
			}
			if err = corporateTeamOwnersRemain(tx, teamID, in.UserID, false); err != nil {
				apiErr(w, 409, "team change would remove the last active owner")
				return
			}
		}
		var exists bool
		if tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM teams WHERE id=?) AND EXISTS(SELECT 1 FROM users WHERE id=?)`, teamID, in.UserID).Scan(&exists) != nil || !exists {
			apiErr(w, 404, "team or user not found")
			return
		}
		action := "team.member.add"
		if r.Method == http.MethodPut && s.principal.ID == in.UserID {
			action = "team.member.self_add"
		}
		if r.Method == http.MethodPut {
			_, err = tx.Exec(`INSERT INTO team_members(team_id,user_id) VALUES(?,?) ON CONFLICT DO NOTHING`, teamID, in.UserID)
		} else {
			action = "team.member.remove"
			_, err = tx.Exec(`DELETE FROM team_members WHERE team_id=? AND user_id=?`, teamID, in.UserID)
		}
		if err == nil {
			err = corporateAudit(tx, s.principal.ID, action, "team", teamID, map[string]any{"user_id": in.UserID, "self_change": s.principal.ID == in.UserID})
		}
		if err == nil {
			err = tx.Commit()
		}
		if err != nil {
			apiErr(w, 500, "database error")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		apiErr(w, 405, "method not allowed")
	}
}

func (s *Server) corporateUsers(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.corporateActor(w, r, true); !ok {
		return
	}
	limit := 200
	if n, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && n > 0 && n < limit {
		limit = n
	}
	rows, err := s.DB.Query(`SELECT id,name,admin,disabled FROM users ORDER BY name,id LIMIT ?`, limit)
	if err != nil {
		apiErr(w, 500, "database error")
		return
	}
	defer rows.Close()
	type user struct {
		ID       string `json:"id"`
		Name     string `json:"name"`
		Admin    bool   `json:"admin"`
		Disabled bool   `json:"disabled"`
	}
	out := make([]user, 0)
	for rows.Next() {
		var v user
		var admin, disabled int
		if rows.Scan(&v.ID, &v.Name, &admin, &disabled) != nil {
			apiErr(w, 500, "database error")
			return
		}
		v.Admin, v.Disabled = admin != 0, disabled != 0
		out = append(out, v)
	}
	if rows.Err() != nil {
		apiErr(w, 500, "database error")
		return
	}
	writeJSON(w, 200, out)
}

func (s *Server) corporateUserDisable(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.corporateActor(w, r, true)
	if !ok {
		return
	}
	var in struct {
		Disabled *bool `json:"disabled"`
	}
	if corporateDecode(r, &in) != nil || in.Disabled == nil {
		apiErr(w, 400, "invalid input")
		return
	}
	id := r.PathValue("id")
	if s.personal && id == PersonalOwnerID {
		// Disabling the only identity of a personal installation would lock
		// everyone out and stop the next start.
		apiErr(w, 403, "the owner of a personal installation cannot be disabled")
		return
	}
	tx, err := s.DB.Begin()
	if err != nil {
		apiErr(w, 500, "database error")
		return
	}
	defer tx.Rollback()
	if err = corporateLockOwners(tx); err != nil {
		apiErr(w, 500, "database error")
		return
	}
	var admin int
	var disabled int
	if err = tx.QueryRow(`SELECT admin,disabled FROM users WHERE id=? FOR UPDATE`, id).Scan(&admin, &disabled); err != nil {
		apiErr(w, 404, "user not found")
		return
	}
	if disabled == boolInt(*in.Disabled) {
		writeJSON(w, 200, map[string]any{"id": id, "disabled": *in.Disabled})
		return
	}
	orphanedResources := 0
	if *in.Disabled {
		rows, qerr := tx.Query(`SELECT DISTINCT m.resource_type,m.resource_id FROM memberships m
LEFT JOIN teams t ON m.subject_type='team' AND t.id=m.subject_id
WHERE m.role='owner' AND (m.subject_type='user' AND m.subject_id=? OR m.subject_type='team' AND m.subject_id IN (SELECT team_id FROM team_members WHERE user_id=?))
AND (m.subject_type='user' OR t.archived_at IS NULL)`, id, id)
		if qerr != nil {
			apiErr(w, 500, "database error")
			return
		}
		type resource struct{ kind, id string }
		resources := make([]resource, 0)
		for rows.Next() {
			var v resource
			if rows.Scan(&v.kind, &v.id) != nil {
				rows.Close()
				apiErr(w, 500, "database error")
				return
			}
			resources = append(resources, v)
		}
		qerr = rows.Err()
		rows.Close()
		if qerr != nil {
			apiErr(w, 500, "database error")
			return
		}
		for _, resource := range resources {
			var n int
			qerr = tx.QueryRow(`SELECT count(DISTINCT p.id) FROM memberships m
LEFT JOIN team_members tm ON m.subject_type='team' AND tm.team_id=m.subject_id
JOIN users p ON (m.subject_type='user' AND p.id=m.subject_id OR m.subject_type='team' AND p.id=tm.user_id)
WHERE m.resource_type=? AND m.resource_id=? AND m.role='owner' AND p.disabled=0 AND p.id<>?
AND (m.subject_type='user' OR EXISTS(SELECT 1 FROM teams t WHERE t.id=m.subject_id AND t.archived_at IS NULL))`, resource.kind, resource.id, id).Scan(&n)
			if qerr != nil {
				apiErr(w, 500, "database error")
				return
			}
			if n == 0 {
				orphanedResources++
			}
		}
	}
	if _, err = tx.Exec(`UPDATE users SET disabled=? WHERE id=?`, boolInt(*in.Disabled), id); err == nil {
		action := "user.enabled"
		if *in.Disabled {
			action = "user.disabled"
		}
		err = corporateAudit(tx, actor, action, "user", id, map[string]any{"disabled": *in.Disabled, "orphaned_resources": orphanedResources})
	}
	if err == nil {
		err = tx.Commit()
	}
	if err != nil {
		apiErr(w, 500, "database error")
		return
	}
	writeJSON(w, 200, map[string]any{"id": id, "disabled": *in.Disabled, "orphaned_resources": orphanedResources})
}

func (s *Server) corporateAudit(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.corporateActor(w, r, true); !ok {
		return
	}
	limit := 200
	if n, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && n > 0 && n < limit {
		limit = n
	}
	rows, err := s.DB.Query(`SELECT id,actor_id,action,resource_type,resource_id,details::text,created_at FROM security_audit ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		apiErr(w, 500, "database error")
		return
	}
	defer rows.Close()
	type entry struct {
		ID           int64           `json:"id"`
		ActorID      string          `json:"actor_id"`
		Action       string          `json:"action"`
		ResourceType string          `json:"resource_type"`
		ResourceID   string          `json:"resource_id"`
		Details      json.RawMessage `json:"details"`
		CreatedAt    string          `json:"created_at"`
	}
	out := make([]entry, 0)
	for rows.Next() {
		var v entry
		if rows.Scan(&v.ID, &v.ActorID, &v.Action, &v.ResourceType, &v.ResourceID, &v.Details, &v.CreatedAt) != nil {
			apiErr(w, 500, "database error")
			return
		}
		out = append(out, v)
	}
	if rows.Err() != nil {
		apiErr(w, 500, "database error")
		return
	}
	writeJSON(w, 200, out)
}

func (s *Server) corporateSpaceCreate(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.corporateActor(w, r, false)
	if !ok {
		return
	}
	if !s.outsideMCP() {
		apiErr(w, http.StatusForbidden, "forbidden")
		return
	}
	var in struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		// Personal is accepted and ignored: a space is personal while its owner is its only member.
		Personal *bool `json:"personal"`
	}
	if corporateDecode(r, &in) != nil {
		apiErr(w, 400, "invalid input")
		return
	}
	in.Name, ok = corporateName(in.Name, 160)
	if !ok || utf8.RuneCountInString(in.Description) > 4000 {
		apiErr(w, 400, "invalid input")
		return
	}
	id := newID()
	tx, err := s.DB.Begin()
	if err != nil {
		apiErr(w, 500, "database error")
		return
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`INSERT INTO spaces(id,name,description,owner_id) VALUES(?,?,?,?)`, id, in.Name, in.Description, actor); err == nil {
		_, err = tx.Exec(`INSERT INTO memberships(resource_type,resource_id,subject_type,subject_id,role) VALUES('space',?,'user',?,'owner') ON CONFLICT DO NOTHING`, id, actor)
	}
	if err == nil {
		err = corporateAudit(tx, actor, "space.create", "space", id, map[string]any{})
	}
	if err == nil {
		err = tx.Commit()
	}
	if err != nil {
		apiErr(w, 500, "database error")
		return
	}
	writeJSON(w, 201, map[string]any{"id": id, "name": in.Name, "description": in.Description, "personal": !s.personal, "owner_id": actor})
}

func (s *Server) corporateUserDirectory(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.corporateActor(w, r, false)
	if !ok {
		return
	}
	if !s.outsideMCP() {
		apiErr(w, http.StatusForbidden, "access denied")
		return
	}
	kind, id := r.URL.Query().Get("resource_type"), r.URL.Query().Get("resource_id")
	if id == "" || !s.corporateManageResource(w, r, s.DB, actor, kind, id, false) {
		return
	}
	rows, err := s.DB.Query(`SELECT id,name FROM users WHERE disabled=0 ORDER BY name,id LIMIT 200`)
	if err != nil {
		apiErr(w, 500, "database error")
		return
	}
	defer rows.Close()
	type item struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	out := make([]item, 0)
	for rows.Next() {
		var v item
		if rows.Scan(&v.ID, &v.Name) != nil {
			apiErr(w, 500, "database error")
			return
		}
		out = append(out, v)
	}
	if rows.Err() != nil {
		apiErr(w, 500, "database error")
		return
	}
	writeJSON(w, 200, out)
}

type corporateGrant struct {
	SubjectType string `json:"subject_type"`
	SubjectID   string `json:"subject_id"`
	Role        string `json:"role"`
}

func (s *Server) corporateAccess(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.corporateActor(w, r, false)
	if !ok {
		return
	}
	if !s.outsideMCP() {
		apiErr(w, http.StatusForbidden, "access denied")
		return
	}
	kind, id := r.URL.Query().Get("resource_type"), r.URL.Query().Get("resource_id")
	if id == "" || !s.corporateManageResource(w, r, s.DB, actor, kind, id, false) {
		return
	}
	if r.Method == http.MethodGet {
		rows, err := s.DB.Query("SELECT subject_type,subject_id,role FROM memberships WHERE resource_type=? AND resource_id=? ORDER BY subject_type,subject_id", kind, id)
		if err != nil {
			apiErr(w, 500, "database error")
			return
		}
		defer rows.Close()
		grants := []corporateGrant{}
		for rows.Next() {
			var g corporateGrant
			if rows.Scan(&g.SubjectType, &g.SubjectID, &g.Role) != nil {
				apiErr(w, 500, "database error")
				return
			}
			grants = append(grants, g)
		}
		if rows.Err() != nil {
			apiErr(w, 500, "database error")
			return
		}
		writeJSON(w, 200, map[string]any{"resource_type": kind, "resource_id": id, "grants": grants})
		return
	}
	if r.Method != http.MethodPut && r.Method != http.MethodDelete {
		apiErr(w, 405, "method not allowed")
		return
	}
	var grant corporateGrant
	if r.Method == http.MethodPut {
		var in struct {
			Grant *corporateGrant `json:"grant"`
		}
		if corporateDecode(r, &in) != nil || in.Grant == nil || !corporateValidRole(in.Grant.Role) {
			apiErr(w, 400, "provide one grant; bulk replacement is not supported")
			return
		}
		grant = *in.Grant
	} else {
		grant.SubjectType = r.URL.Query().Get("subject_type")
		grant.SubjectID = r.URL.Query().Get("subject_id")
	}
	if (grant.SubjectType != "user" && grant.SubjectType != "team") || !validPathID(grant.SubjectID) {
		apiErr(w, 400, "invalid grant subject")
		return
	}
	tx, err := s.DB.Begin()
	if err != nil {
		apiErr(w, 500, "database error")
		return
	}
	defer tx.Rollback()
	if err = corporateLockOwners(tx); err != nil {
		apiErr(w, 500, "database error")
		return
	}
	// Permissions are checked again after taking the ACL mutation lock.
	role, viaAdmin, err := s.corporateSpaceRole(tx, actor, id)
	if err != nil || (role != "owner" && role != "manager") {
		apiErr(w, 403, "access denied")
		return
	}
	var previous string
	err = tx.QueryRow("SELECT role FROM memberships WHERE resource_type=? AND resource_id=? AND subject_type=? AND subject_id=? FOR UPDATE", kind, id, grant.SubjectType, grant.SubjectID).Scan(&previous)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		apiErr(w, 500, "database error")
		return
	}
	if r.Method == http.MethodDelete && errors.Is(err, sql.ErrNoRows) {
		apiErr(w, 404, "grant not found")
		return
	}
	if (previous == "owner" || grant.Role == "owner") && role != "owner" {
		apiErr(w, 403, "only an owner may change ownership")
		return
	}
	if r.Method == http.MethodPut {
		var exists bool
		if grant.SubjectType == "user" {
			err = tx.QueryRow("SELECT EXISTS(SELECT 1 FROM users WHERE id=? AND disabled=0)", grant.SubjectID).Scan(&exists)
		} else {
			err = tx.QueryRow("SELECT EXISTS(SELECT 1 FROM teams WHERE id=? AND archived_at IS NULL)", grant.SubjectID).Scan(&exists)
		}
		if err != nil || !exists {
			apiErr(w, 400, "invalid grant subject")
			return
		}
		_, err = tx.Exec("INSERT INTO memberships(resource_type,resource_id,subject_type,subject_id,role) VALUES(?,?,?,?,?) ON CONFLICT(resource_type,resource_id,subject_type,subject_id) DO UPDATE SET role=EXCLUDED.role", kind, id, grant.SubjectType, grant.SubjectID, grant.Role)
	} else {
		_, err = tx.Exec("DELETE FROM memberships WHERE resource_type=? AND resource_id=? AND subject_type=? AND subject_id=?", kind, id, grant.SubjectType, grant.SubjectID)
	}
	if err != nil {
		apiErr(w, 500, "database error")
		return
	}
	if previous == "owner" {
		var owners int
		err = tx.QueryRow(`SELECT count(DISTINCT u.id) FROM memberships m LEFT JOIN team_members tm ON m.subject_type='team' AND tm.team_id=m.subject_id JOIN users u ON (m.subject_type='user' AND u.id=m.subject_id OR m.subject_type='team' AND u.id=tm.user_id) WHERE m.resource_type=? AND m.resource_id=? AND m.role='owner' AND u.disabled=0 AND (m.subject_type='user' OR EXISTS(SELECT 1 FROM teams t WHERE t.id=m.subject_id AND t.archived_at IS NULL))`, kind, id).Scan(&owners)
		if err != nil || owners == 0 {
			apiErr(w, 409, "resource must retain an active owner")
			return
		}
	}
	action := "access.upsert"
	if r.Method == http.MethodDelete {
		action = "access.remove"
	}
	if err = corporateAudit(tx, actor, action, kind, id, map[string]any{"subject_type": grant.SubjectType, "subject_id": grant.SubjectID, "previous_role": previous, "role": grant.Role, "via_admin": viaAdmin}); err == nil {
		err = tx.Commit()
	}
	if err != nil {
		apiErr(w, 500, "database error")
		return
	}
	if r.Method == http.MethodDelete {
		w.WriteHeader(204)
		return
	}
	writeJSON(w, 200, grant)
}

func corporateValidRole(role string) bool {
	switch role {
	case "owner", "manager", "editor", "commenter", "reader":
		return true
	default:
		return false
	}
}

func actorRoleForAccess(tx *transaction, actor, kind, id string) string {
	role, err := corporateRole(tx, actor, kind, id)
	if err != nil {
		return ""
	}
	return role
}

func (s *Server) corporateEffectiveAccess(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.corporateActor(w, r, false)
	if !ok {
		return
	}
	if !s.outsideMCP() {
		apiErr(w, http.StatusForbidden, "access denied")
		return
	}
	kind, id := r.URL.Query().Get("resource_type"), r.URL.Query().Get("resource_id")
	if kind == "project" {
		apiErr(w, http.StatusGone, projectsRemovedText)
		return
	}
	if id == "" || kind != "space" {
		apiErr(w, 400, "resource_type and resource_id are required")
		return
	}
	target := r.URL.Query().Get("user_id")
	if target == "" {
		target = actor
	}
	if target != actor && !s.corporateManageResource(w, r, s.DB, actor, kind, id, false) {
		return
	}
	role, err := corporateRole(s.DB, target, kind, id)
	if err != nil {
		apiErr(w, 404, "resource or user not found")
		return
	}
	if target == actor && role == "" {
		apiErr(w, 404, "resource not found")
		return
	}
	if target != actor {
		var found bool
		if err = s.DB.QueryRow(`SELECT EXISTS(SELECT 1 FROM users WHERE id=?)`, target).Scan(&found); err != nil || !found {
			apiErr(w, 404, "user not found")
			return
		}
	}
	rows, err := s.DB.Query(`WITH principals(subject_type,subject_id) AS (
SELECT 'user',u.id FROM users u WHERE u.id=? AND u.disabled=0
UNION SELECT 'team',tm.team_id FROM team_members tm JOIN teams t ON t.id=tm.team_id AND t.archived_at IS NULL JOIN users u ON u.id=tm.user_id AND u.disabled=0 WHERE tm.user_id=?
)
SELECT m.resource_type,m.resource_id,m.subject_type,m.subject_id,m.role FROM memberships m JOIN principals p USING(subject_type,subject_id) WHERE m.resource_type='space' AND m.resource_id=? ORDER BY m.subject_type,m.subject_id`, target, target, id)
	if err != nil {
		apiErr(w, 500, "database error")
		return
	}
	defer rows.Close()
	type source struct {
		ResourceType string `json:"resource_type"`
		ResourceID   string `json:"resource_id"`
		SubjectType  string `json:"subject_type"`
		SubjectID    string `json:"subject_id"`
		Role         string `json:"role"`
	}
	sources := make([]source, 0)
	for rows.Next() {
		var v source
		if rows.Scan(&v.ResourceType, &v.ResourceID, &v.SubjectType, &v.SubjectID, &v.Role) != nil {
			apiErr(w, 500, "database error")
			return
		}
		sources = append(sources, v)
	}
	if rows.Err() != nil {
		apiErr(w, 500, "database error")
		return
	}
	rows.Close()
	// Only managers (and administrators) of the space see which grants produce the role.
	if actorRole, _, rerr := s.corporateSpaceRole(s.DB, actor, id); rerr != nil || !corporateAtLeast(actorRole, "manager") {
		sources = sources[:0]
	}
	var result any
	if role != "" {
		result = role
	}
	writeJSON(w, 200, map[string]any{"resource_type": kind, "resource_id": id, "user_id": target, "role": result, "sources": sources})
}
