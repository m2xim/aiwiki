package server

import (
	"context"
	"errors"
	"strings"
)

// SearchParams is the shared input of REST GET /api/articles and MCP wiki_search.
// Tags and Properties ("key:value") are exact AND filters; Q is free text.
type SearchParams struct {
	ProjectID  string
	Q          string
	SpaceID    string
	Kind       string
	Pinned     bool
	Tags       []string
	Properties []string
	Limit      int // 1..200, already clamped by the caller
	Offset     int
	// AllowAny lets a text query fall back to partial matches (mode "any") when no
	// article contains every term. false (REST): the result is then empty, mode "all".
	AllowAny bool
}

// SearchHit is one result. Article.Body is always empty. Snippet and MatchedIn
// are set only for text queries.
type SearchHit struct {
	Article   Article
	Score     float64
	Snippet   string   // short excerpt around the best match, may be empty
	MatchedIn []string // subset of: title, tags, summary, properties, body
}

// SearchResult carries one page of hits and the number of all matches.
type SearchResult struct {
	Hits  []SearchHit
	Total int
	// Mode is "all" when every query term matched, "any" when no article
	// matched all terms and partial matches are returned instead; "" without Q.
	Mode string
}

// searchArticles applies filters and the text query. Without Q the order is
// updated_at DESC; with Q hits are ordered by relevance. On error it returns an
// HTTP status and a client-facing message.
func (s *Server) searchArticles(ctx context.Context, p SearchParams) (SearchResult, int, error) {
	where := []string{"a.deleted_at IS NULL"}
	if p.SpaceID == "" {
		if s.DB.pg {
			where = append(where, "corporate_container_writable(a.space_id,a.project_id)")
		} else {
			where = append(where, "a.space_id IN (SELECT id FROM spaces WHERE archived_at IS NULL)")
		}
	} else {
		where = append(where, "a.space_id=?")
	}
	args := []any{}
	if p.SpaceID != "" {
		args = append(args, p.SpaceID)
	}
	if p.ProjectID != "" {
		where = append(where, "a.project_id=?")
		args = append(args, p.ProjectID)
	}
	if p.Kind != "" {
		where = append(where, "a.kind=?")
		args = append(args, p.Kind)
	}
	if p.Pinned {
		where = append(where, "a.pinned=1")
	}
	for _, v := range p.Tags {
		if v != "" {
			where = append(where, s.DB.jsonArrayHasSQL("a.tags"))
			args = append(args, v)
		}
	}
	for _, v := range p.Properties {
		if v == "" {
			continue
		}
		kv := strings.SplitN(v, ":", 2)
		if len(kv) != 2 {
			return SearchResult{}, 400, errors.New("property must be key:value")
		}
		where = append(where, s.DB.jsonObjectHasSQL("a.properties"))
		args = append(args, kv[0], kv[1])
	}
	query := "SELECT " + articleColumns + " FROM articles a WHERE " + strings.Join(where, " AND ") + " ORDER BY a.updated_at DESC,a.id"
	rows, e := s.DB.QueryContext(ctx, query, args...)
	if e != nil {
		return SearchResult{}, 400, errors.New("invalid search or filter")
	}
	defer rows.Close()
	var m *matcher
	if q := strings.TrimSpace(p.Q); q != "" {
		m = newMatcher(q)
		if len(m.terms) == 0 {
			m = nil
		}
	}
	all := []SearchHit{}
	var cands []scored
	for rows.Next() {
		a, e := scanArticle(rows)
		if e != nil {
			return SearchResult{}, 500, errors.New("database error")
		}
		if m != nil {
			if sc, ok := m.rankArticle(a); ok {
				cands = append(cands, sc)
			}
			continue
		}
		a.Body = ""
		all = append(all, SearchHit{Article: a})
	}
	if err := rows.Err(); err != nil {
		return SearchResult{}, 500, errors.New("database error")
	}
	if m != nil {
		return m.rankAndPage(cands, p), 200, nil
	}
	res := SearchResult{Total: len(all), Hits: []SearchHit{}}
	if p.Offset < len(all) {
		end := p.Offset + p.Limit
		if end > len(all) {
			end = len(all)
		}
		res.Hits = all[p.Offset:end]
	}
	return res, 200, nil
}
