package server

import (
	"encoding/json"
	"net/http"
	"sort"
	"time"
)

type catalogArticleStatsRow struct {
	spaceID string
	updated any
	tags    []byte
}

func (s *Server) corporateCatalog(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.corporateActor(w, r, false)
	if !ok {
		return
	}
	type catalogSpace struct {
		ID            string   `json:"id"`
		Name          string   `json:"name"`
		Description   string   `json:"description"`
		Personal      bool     `json:"personal"`
		ArticleCount  int      `json:"article_count"`
		LastUpdated   string   `json:"last_updated,omitempty"`
		Tags          []string `json:"tags"`
		EffectiveRole string   `json:"effective_role,omitempty"`
	}
	type spaceRow struct {
		id, name, description string
		role                  string
	}
	rows, err := s.DB.Query(`SELECT id,name,description FROM spaces WHERE archived_at IS NULL ORDER BY name,id`)
	if err != nil {
		apiErr(w, 500, "database error")
		return
	}
	spaceRows := make([]spaceRow, 0)
	for rows.Next() {
		var item spaceRow
		if rows.Scan(&item.id, &item.name, &item.description) != nil {
			rows.Close()
			apiErr(w, 500, "database error")
			return
		}
		spaceRows = append(spaceRows, item)
	}
	rowsErr := rows.Err()
	closeErr := rows.Close()
	if rowsErr != nil || closeErr != nil {
		apiErr(w, 500, "database error")
		return
	}
	personal := map[string]bool{}
	if s.outsideMCP() {
		personal = s.personalSpaceSet()
	}
	spaces := make([]catalogSpace, 0, len(spaceRows))
	spaceIndex := make(map[string]int, len(spaceRows))
	for _, item := range spaceRows {
		item.role, err = corporateRole(s.DB, actor, "space", item.id)
		if err != nil {
			apiErr(w, 500, "database error")
			return
		}
		if item.role == "" {
			continue
		}
		spaceIndex[item.id] = len(spaces)
		spaces = append(spaces, catalogSpace{ID: item.id, Name: item.name, Description: item.description, Personal: personal[item.id], EffectiveRole: item.role, Tags: []string{}})
	}
	// Article RLS applies to this query before counts and tags are accumulated.
	articleRows, err := s.DB.Query(`SELECT space_id,updated_at,tags FROM articles WHERE deleted_at IS NULL`)
	if err != nil {
		apiErr(w, 500, "database error")
		return
	}
	articleItems := make([]catalogArticleStatsRow, 0)
	for articleRows.Next() {
		var item catalogArticleStatsRow
		if articleRows.Scan(&item.spaceID, &item.updated, &item.tags) != nil {
			articleRows.Close()
			apiErr(w, 500, "database error")
			return
		}
		articleItems = append(articleItems, item)
	}
	rowsErr = articleRows.Err()
	closeErr = articleRows.Close()
	if rowsErr != nil || closeErr != nil {
		apiErr(w, 500, "database error")
		return
	}
	for _, article := range articleItems {
		if idx, visible := spaceIndex[article.spaceID]; visible {
			spaces[idx].ArticleCount++
			spaces[idx].LastUpdated = laterCatalogDate(spaces[idx].LastUpdated, catalogDate(article.updated))
			addCatalogTags(&spaces[idx].Tags, article.tags)
		}
	}
	for i := range spaces {
		sort.Strings(spaces[i].Tags)
	}
	out := make([]any, 0, len(spaces))
	for _, sp := range spaces {
		out = append(out, sp)
	}
	// A system administrator also gets the spaces they hold no role in, as bare directory entries:
	// id, name, owner and the personal flag only (no description, article count, tags or content).
	if s.adminOutsideMCP(s.DB) {
		limited, lerr := s.DB.Query(`SELECT id,name,owner_name,personal FROM corporate_admin_space_directory()`)
		if lerr != nil {
			apiErr(w, 500, "database error")
			return
		}
		for limited.Next() {
			var id, name, owner string
			var personal bool
			if limited.Scan(&id, &name, &owner, &personal) != nil {
				limited.Close()
				apiErr(w, 500, "database error")
				return
			}
			out = append(out, map[string]any{"id": id, "name": name, "owner_name": owner, "personal": personal, "limited": true})
		}
		lerr = limited.Err()
		limited.Close()
		if lerr != nil {
			apiErr(w, 500, "database error")
			return
		}
	}
	writeJSON(w, 200, out)
}

func catalogDate(value any) string {
	switch v := value.(type) {
	case time.Time:
		return v.UTC().Format(time.RFC3339Nano)
	case string:
		return v
	case []byte:
		return string(v)
	default:
		return ""
	}
}

func laterCatalogDate(current, candidate string) string {
	if candidate > current {
		return candidate
	}
	return current
}

func addCatalogTags(target *[]string, raw []byte) {
	var tags []string
	if json.Unmarshal(raw, &tags) != nil {
		return
	}
	seen := make(map[string]bool, len(*target))
	for _, tag := range *target {
		seen[tag] = true
	}
	for _, tag := range tags {
		if tag != "" && !seen[tag] {
			*target = append(*target, tag)
			seen[tag] = true
		}
	}
}

func addCatalogTagSet(target map[string]bool, raw []byte) {
	var tags []string
	if json.Unmarshal(raw, &tags) == nil {
		for _, tag := range tags {
			if tag != "" {
				target[tag] = true
			}
		}
	}
}
