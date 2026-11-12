package server

// personalSpaceSet is the set of spaces that are personal: exactly one membership, a user with the
// owner role, and not the general space. Memberships are readable outside MCP sessions.
func (s *Server) personalSpaceSet() map[string]bool {
	out := map[string]bool{}
	// A bound connection runs one statement at a time: read the general space first.
	general := s.generalSpaceID(s.DB)
	rows, err := s.DB.Query(`SELECT m.resource_id FROM memberships m WHERE m.resource_type='space'
GROUP BY m.resource_id HAVING count(*)=1 AND bool_or(m.subject_type='user' AND m.role='owner')`)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil && id != general {
			out[id] = true
		}
	}
	return out
}

// corporateVisibleSpaces lists the spaces the caller has a role in (RLS), with that role and the
// derived personal flag. Spaces without a role are never listed here.
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
	for rows.Next() {
		x, e := scanSpace(rows)
		if e != nil {
			rows.Close()
			return nil, e
		}
		out = append(out, x)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	personal := map[string]bool{}
	if s.principal != nil && s.outsideMCP() {
		personal = s.personalSpaceSet()
	}
	for i := range out {
		if s.principal != nil {
			role, rerr := corporateRole(s.DB, s.principal.ID, "space", out[i].ID)
			if rerr != nil {
				return nil, rerr
			}
			out[i].EffectiveRole = role
			if s.outsideMCP() {
				p := personal[out[i].ID]
				out[i].Personal = &p
			}
		}
	}
	return out, nil
}
