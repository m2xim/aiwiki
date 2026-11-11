package server

import "strings"

// authorInfo is the read-time display data of a stored author id.
type authorInfo struct{ Name, Kind string }

// authorKind classifies a stored author id by its prefix.
func authorKind(id string) string {
	switch {
	case strings.HasPrefix(id, "svc_"):
		return "service"
	case strings.HasPrefix(id, "u_"):
		return "user"
	}
	return ""
}

// authorInfos resolves display names for the distinct ids in one query on PostgreSQL
// (corporate_actor_name is the only way to see service-account names across owners).
// In SQLite/personal mode the author is free text and is its own name.
func (s *Server) authorInfos(ids ...string) map[string]authorInfo {
	out := make(map[string]authorInfo, len(ids))
	var distinct []string
	for _, id := range ids {
		if _, ok := out[id]; ok || id == "" {
			continue
		}
		out[id] = authorInfo{Name: id}
		distinct = append(distinct, id)
	}
	if s == nil || s.DB == nil || !s.DB.pg || len(distinct) == 0 {
		return out
	}
	for _, id := range distinct {
		out[id] = authorInfo{Kind: authorKind(id)}
	}
	rows, err := s.DB.Query(`SELECT a, public.corporate_actor_name(a) FROM unnest(?::text[]) AS a`, distinct)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id, name string
		if rows.Scan(&id, &name) == nil {
			out[id] = authorInfo{Name: name, Kind: authorKind(id)}
		}
	}
	return out
}

func (s *Server) fillArticleAuthors(list ...*Article) {
	ids := make([]string, 0, len(list))
	for _, a := range list {
		ids = append(ids, a.Author)
	}
	m := s.authorInfos(ids...)
	for _, a := range list {
		a.AuthorName, a.AuthorKind = m[a.Author].Name, m[a.Author].Kind
	}
}

func (s *Server) fillRevisionAuthors(list []Revision) {
	ids := make([]string, 0, 2*len(list))
	for i := range list {
		ids = append(ids, list[i].Author, list[i].Snapshot.Author)
	}
	m := s.authorInfos(ids...)
	for i := range list {
		r := &list[i]
		r.AuthorName, r.AuthorKind = m[r.Author].Name, m[r.Author].Kind
		r.Snapshot.AuthorName, r.Snapshot.AuthorKind = m[r.Snapshot.Author].Name, m[r.Snapshot.Author].Kind
	}
}

func (s *Server) fillCommentAuthors(list []Comment) {
	ids := make([]string, 0, len(list))
	for i := range list {
		ids = append(ids, list[i].Author)
	}
	m := s.authorInfos(ids...)
	for i := range list {
		list[i].AuthorName, list[i].AuthorKind = m[list[i].Author].Name, m[list[i].Author].Kind
	}
}
