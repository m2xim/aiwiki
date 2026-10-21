package server

import (
	"database/sql"
	"net/http"
	"strings"
	"unicode/utf8"
)

func (s *Server) spaces(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		if s.principal != nil {
			s.corporateSpaceCreate(w, r)
			return
		}
		var in struct {
			Name        string `json:"name"`
			Description string `json:"description"`
		}
		if decode(r, &in) != nil || !validSpaceName(in.Name) {
			apiErr(w, 400, "name must contain 1 to 300 characters")
			return
		}
		x := Space{ID: newID(), Name: strings.TrimSpace(in.Name), Description: in.Description}
		if _, err := s.DB.Exec("INSERT INTO spaces(id,name,description) VALUES(?,?,?)", x.ID, x.Name, x.Description); err != nil {
			apiErr(w, 400, "could not create space")
			return
		}
		writeJSON(w, 201, x)
		return
	}
	if r.Method != http.MethodGet {
		apiErr(w, 405, "method not allowed")
		return
	}
	if s.DB.pg {
		spaces, err := s.corporateVisibleSpaces(r.URL.Query().Get("include_archived") == "true")
		if err != nil {
			apiErr(w, 500, "database error")
			return
		}
		writeJSON(w, 200, spaces)
		return
	}
	query := "SELECT id,name,description,archived_at FROM spaces"
	if r.URL.Query().Get("include_archived") != "true" {
		query += " WHERE archived_at IS NULL"
	}
	rows, e := s.DB.Query(query + " ORDER BY name")
	if e != nil {
		apiErr(w, 500, "database error")
		return
	}
	defer rows.Close()
	out := []Space{}
	for rows.Next() {
		x, err := scanSpace(rows)
		if err != nil {
			apiErr(w, 500, "database error")
			return
		}
		out = append(out, x)
	}
	if rows.Err() != nil {
		apiErr(w, 500, "database error")
		return
	}
	writeJSON(w, 200, out)
}

func validSpaceName(name string) bool {
	name = strings.TrimSpace(name)
	return name != "" && utf8.RuneCountInString(name) <= 300
}

func scanSpace(row interface{ Scan(...any) error }) (Space, error) {
	var x Space
	var archived sql.NullString
	err := row.Scan(&x.ID, &x.Name, &x.Description, &archived)
	x.ArchivedAt = archived.String
	return x, err
}

func (s *Server) space(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPatch {
		apiErr(w, 405, "method not allowed")
		return
	}
	var in struct {
		Name        *string `json:"name"`
		Description *string `json:"description"`
		Archived    *bool   `json:"archived"`
	}
	if decode(r, &in) != nil || (in.Name != nil && !validSpaceName(*in.Name)) || (in.Name == nil && in.Description == nil && in.Archived == nil) {
		apiErr(w, 400, "provide name (1 to 300 characters), description, or archived")
		return
	}
	tx, err := s.DB.Begin()
	if err != nil {
		apiErr(w, 500, "database error")
		return
	}
	defer tx.Rollback()
	x, err := scanSpace(tx.QueryRow("SELECT id,name,description,archived_at FROM spaces WHERE id=?", r.PathValue("id")))
	if err == sql.ErrNoRows {
		apiErr(w, 404, "space not found")
		return
	}
	if err != nil {
		apiErr(w, 500, "database error")
		return
	}
	if s.principal != nil {
		var role int
		if err = tx.QueryRow("SELECT corporate_role_rank(corporate_effective_role(?,?,''))", s.principal.ID, x.ID).Scan(&role); err != nil || role < 4 {
			apiErr(w, 403, "space management is not permitted")
			return
		}
	}
	original := x
	if in.Name != nil {
		x.Name = strings.TrimSpace(*in.Name)
	}
	if in.Description != nil {
		x.Description = *in.Description
	}
	if in.Archived != nil {
		if *in.Archived && x.ArchivedAt == "" {
			x.ArchivedAt = stamp()
		}
		if !*in.Archived {
			x.ArchivedAt = ""
		}
	}
	result, err := tx.Exec("UPDATE spaces SET name=?,description=?,archived_at=? WHERE id=?", x.Name, x.Description, nullString(x.ArchivedAt), x.ID)
	if err != nil {
		apiErr(w, 500, "database error")
		return
	}
	if n, err := result.RowsAffected(); err != nil || n != 1 {
		apiErr(w, 403, "space management is not permitted")
		return
	}
	if s.principal != nil {
		if err = corporateAudit(tx, s.principal.ID, "space.update", "space", x.ID, map[string]any{"renamed": original.Name != x.Name, "description_changed": original.Description != x.Description, "archived": x.ArchivedAt != ""}); err != nil {
			apiErr(w, 500, "database error")
			return
		}
	}
	if tx.Commit() != nil {
		apiErr(w, 500, "database error")
		return
	}
	writeJSON(w, 200, x)
}
