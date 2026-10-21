package server

// corporateVisibleSpaces includes safe navigation groups for separately granted
// projects. A limited group deliberately contains no metadata from its hidden space.
func (s *Server) corporateVisibleSpaces(includeArchived bool) ([]Space, error) {
	query := "SELECT id,name,description,archived_at FROM spaces"
	if !includeArchived {
		query += " WHERE archived_at IS NULL"
	}
	rows, err := s.DB.Query(query + " ORDER BY name,id")
	if err != nil {
		return nil, err
	}
	out := []Space{}
	seen := map[string]bool{}
	for rows.Next() {
		x, e := scanSpace(rows)
		if e != nil {
			rows.Close()
			return nil, e
		}
		out = append(out, x)
		seen[x.ID] = true
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	query = "SELECT DISTINCT space_id FROM projects WHERE corporate_role_rank(corporate_effective_role(current_setting('app.user_id',true),space_id,id))>=1"
	if !includeArchived {
		query += " AND corporate_container_writable(space_id,id)"
	}
	rows, err = s.DB.Query(query + " ORDER BY space_id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			return nil, err
		}
		if !seen[id] {
			out = append(out, Space{ID: id, Name: "Проекты с отдельным доступом", Limited: true})
			seen[id] = true
		}
	}
	return out, rows.Err()
}
