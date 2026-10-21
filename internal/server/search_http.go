package server

import (
	"math"
	"net/http"
	"strconv"
	"strings"
)

type searchHTTPItem struct {
	compactArticle
	ParentID  string   `json:"parent_id"`
	Tags      []string `json:"tags"`    // shadows compactArticle.Tags: always an array
	Summary   string   `json:"summary"` // shadows compactArticle.Summary: always present
	Score     *float64 `json:"score,omitempty"`
	Snippet   string   `json:"snippet,omitempty"`
	MatchedIn []string `json:"matched_in,omitempty"`
}

// searchHTTP handles GET /api/search for the UI.
func (s *Server) searchHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		apiErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	q := r.URL.Query()
	p := SearchParams{ProjectID: q.Get("project_id"), Q: q.Get("q"), SpaceID: q.Get("space_id"), Kind: q.Get("kind"), Pinned: q.Get("pinned") == "true", Tags: q["tag"], Properties: q["property"], Limit: 30, AllowAny: true}
	if n, e := strconv.Atoi(q.Get("limit")); e == nil && n > 0 {
		p.Limit = n
	}
	if p.Limit > 100 {
		p.Limit = 100
	}
	if n, e := strconv.Atoi(q.Get("offset")); e == nil && n > 0 {
		p.Offset = n
	}
	res, code, err := s.searchArticles(r.Context(), p)
	if err != nil {
		apiErr(w, code, err.Error())
		return
	}
	hasQ := strings.TrimSpace(p.Q) != ""
	items := make([]searchHTTPItem, 0, len(res.Hits))
	for _, h := range res.Hits {
		it := searchHTTPItem{compactArticle: compact(h.Article), ParentID: h.Article.ParentID, Summary: h.Article.Summary, Tags: h.Article.Tags, Snippet: h.Snippet, MatchedIn: h.MatchedIn}
		if it.Tags == nil {
			it.Tags = []string{}
		}
		if hasQ {
			sc := math.Round(h.Score*100) / 100
			it.Score = &sc
		}
		items = append(items, it)
	}
	out := struct {
		Total      int              `json:"total"`
		Offset     int              `json:"offset"`
		Mode       string           `json:"mode,omitempty"`
		Results    []searchHTTPItem `json:"results"`
		NextOffset *int             `json:"next_offset,omitempty"`
	}{res.Total, p.Offset, res.Mode, items, nil}
	if res.Total > p.Offset+len(items) && len(items) > 0 {
		n := p.Offset + len(items)
		out.NextOffset = &n
	}
	writeJSON(w, http.StatusOK, out)
}
