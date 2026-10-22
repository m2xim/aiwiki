package server

import (
	"net/http"
	"sort"
)

const trustedCatalogMaxTags = 20

// trustedCatalog serves the same JSON contract as corporateCatalog for the
// single-user mode: every active space, full access, no projects.
func (s *Server) trustedCatalog(w http.ResponseWriter, r *http.Request) {
	type catalogSpace struct {
		ID            string           `json:"id"`
		Name          string           `json:"name"`
		Description   string           `json:"description"`
		Personal      bool             `json:"personal"`
		ArticleCount  int              `json:"article_count"`
		LastUpdated   string           `json:"last_updated,omitempty"`
		Tags          []string         `json:"tags"`
		EffectiveRole string           `json:"effective_role,omitempty"`
		Projects      []catalogProject `json:"projects"`
	}
	rows, err := s.DB.Query(`SELECT id,name,description FROM spaces WHERE archived_at IS NULL ORDER BY name,id`)
	if err != nil {
		apiErr(w, 500, "database error")
		return
	}
	spaces := make([]catalogSpace, 0)
	index := make(map[string]int)
	for rows.Next() {
		var item catalogSpace
		if rows.Scan(&item.ID, &item.Name, &item.Description) != nil {
			rows.Close()
			apiErr(w, 500, "database error")
			return
		}
		item.EffectiveRole = "owner"
		item.Tags = []string{}
		item.Projects = []catalogProject{}
		index[item.ID] = len(spaces)
		spaces = append(spaces, item)
	}
	rowsErr := rows.Err()
	closeErr := rows.Close()
	if rowsErr != nil || closeErr != nil {
		apiErr(w, 500, "database error")
		return
	}
	articleRows, err := s.DB.Query(`SELECT space_id,updated_at,tags FROM articles WHERE deleted_at IS NULL`)
	if err != nil {
		apiErr(w, 500, "database error")
		return
	}
	for articleRows.Next() {
		var spaceID string
		var updated any
		var tags []byte
		if articleRows.Scan(&spaceID, &updated, &tags) != nil {
			articleRows.Close()
			apiErr(w, 500, "database error")
			return
		}
		if i, ok := index[spaceID]; ok {
			spaces[i].ArticleCount++
			spaces[i].LastUpdated = laterCatalogDate(spaces[i].LastUpdated, catalogDate(updated))
			addCatalogTags(&spaces[i].Tags, tags)
		}
	}
	rowsErr = articleRows.Err()
	closeErr = articleRows.Close()
	if rowsErr != nil || closeErr != nil {
		apiErr(w, 500, "database error")
		return
	}
	for i := range spaces {
		sort.Strings(spaces[i].Tags)
		if len(spaces[i].Tags) > trustedCatalogMaxTags {
			spaces[i].Tags = spaces[i].Tags[:trustedCatalogMaxTags]
		}
	}
	writeJSON(w, 200, spaces)
}
