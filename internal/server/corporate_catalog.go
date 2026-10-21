package server

import (
	"encoding/json"
	"net/http"
	"sort"
	"time"
)

type catalogArticleStatsRow struct {
	spaceID, projectID string
	updated            any
	tags               []byte
}

func (s *Server) corporateCatalog(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.corporateActor(w, r, false)
	if !ok {
		return
	}
	type catalogSpace struct {
		Limited       bool             `json:"limited,omitempty"`
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
	type spaceRow struct {
		id, name, description string
		personal              int
		role                  string
	}
	rows, err := s.DB.Query(`SELECT id,name,description,personal FROM spaces WHERE archived_at IS NULL ORDER BY name,id`)
	if err != nil {
		apiErr(w, 500, "database error")
		return
	}
	spaceRows := make([]spaceRow, 0)
	for rows.Next() {
		var item spaceRow
		if rows.Scan(&item.id, &item.name, &item.description, &item.personal) != nil {
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
		spaces = append(spaces, catalogSpace{ID: item.id, Name: item.name, Description: item.description, Personal: item.personal != 0, EffectiveRole: item.role, Tags: []string{}, Projects: []catalogProject{}})
	}
	projectRows, err := s.DB.Query(`SELECT id,space_id,parent_id,name,description,inherit_acl FROM projects WHERE archived_at IS NULL ORDER BY name,id`)
	if err != nil {
		apiErr(w, 500, "database error")
		return
	}
	type projectRow struct {
		item    catalogProject
		inherit int
	}
	projectItems := make([]projectRow, 0)
	for projectRows.Next() {
		var item projectRow
		if projectRows.Scan(&item.item.ID, &item.item.SpaceID, &item.item.ParentID, &item.item.Name, &item.item.Description, &item.inherit) != nil {
			projectRows.Close()
			apiErr(w, 500, "database error")
			return
		}
		projectItems = append(projectItems, item)
	}
	rowsErr = projectRows.Err()
	closeErr = projectRows.Close()
	if rowsErr != nil || closeErr != nil {
		apiErr(w, 500, "database error")
		return
	}
	visibleProjects := make([]catalogProject, 0, len(projectItems))
	projectIndex := make(map[string]int, len(projectItems))
	projectSpace := make(map[string]string, len(projectItems))
	for _, row := range projectItems {
		role, roleErr := corporateRole(s.DB, actor, "project", row.item.ID)
		if roleErr != nil {
			apiErr(w, 500, "database error")
			return
		}
		if role == "" {
			continue
		}
		if row.item.ParentID != "" {
			parentRole, parentErr := corporateRole(s.DB, actor, "project", row.item.ParentID)
			if parentErr != nil {
				apiErr(w, 500, "database error")
				return
			}
			if parentRole == "" {
				row.item.ParentID = ""
			}
		}
		row.item.InheritACL = row.inherit != 0
		row.item.EffectiveRole = role
		row.item.Tags = []string{}
		projectIndex[row.item.ID] = len(visibleProjects)
		projectSpace[row.item.ID] = row.item.SpaceID
		visibleProjects = append(visibleProjects, row.item)
	}
	for _, project := range visibleProjects {
		idx, exists := spaceIndex[project.SpaceID]
		if !exists {
			idx = len(spaces)
			spaceIndex[project.SpaceID] = idx
			spaces = append(spaces, catalogSpace{ID: project.SpaceID, Name: "Проекты с отдельным доступом", Limited: true, Tags: []string{}, Projects: []catalogProject{}})
		}
		spaces[idx].Projects = append(spaces[idx].Projects, project)
	}
	// Article RLS applies to this query before counts and tags are accumulated.
	articleRows, err := s.DB.Query(`SELECT space_id,project_id,updated_at,tags FROM articles WHERE deleted_at IS NULL`)
	if err != nil {
		apiErr(w, 500, "database error")
		return
	}
	articleItems := make([]catalogArticleStatsRow, 0)
	for articleRows.Next() {
		var item catalogArticleStatsRow
		if articleRows.Scan(&item.spaceID, &item.projectID, &item.updated, &item.tags) != nil {
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
	projectCounts := make(map[string]int)
	projectLatest := make(map[string]string)
	projectTags := make(map[string]map[string]bool)
	projectParents := make(map[string]string, len(visibleProjects))
	for _, project := range visibleProjects {
		projectParents[project.ID] = project.ParentID
	}
	for _, article := range articleItems {
		spaceIdx, visibleSpace := spaceIndex[article.spaceID]
		if visibleSpace && (!spaces[spaceIdx].Limited || projectSpace[article.projectID] == article.spaceID) {
			spaces[spaceIdx].ArticleCount++
			spaces[spaceIdx].LastUpdated = laterCatalogDate(spaces[spaceIdx].LastUpdated, catalogDate(article.updated))
			addCatalogTags(&spaces[spaceIdx].Tags, article.tags)
		}
		if article.projectID == "" || projectSpace[article.projectID] != article.spaceID {
			continue
		}
		seenProjects := make(map[string]bool)
		for projectID := article.projectID; projectID != ""; projectID = projectParents[projectID] {
			if seenProjects[projectID] {
				break
			}
			seenProjects[projectID] = true
			if _, visible := projectIndex[projectID]; !visible || projectSpace[projectID] != article.spaceID {
				break
			}
			projectCounts[projectID]++
			projectLatest[projectID] = laterCatalogDate(projectLatest[projectID], catalogDate(article.updated))
			if projectTags[projectID] == nil {
				projectTags[projectID] = make(map[string]bool)
			}
			addCatalogTagSet(projectTags[projectID], article.tags)
		}
	}
	for i := range spaces {
		for j := range spaces[i].Projects {
			project := &spaces[i].Projects[j]
			project.ArticleCount = projectCounts[project.ID]
			project.LastUpdated = projectLatest[project.ID]
			for tag := range projectTags[project.ID] {
				project.Tags = append(project.Tags, tag)
			}
			sort.Strings(project.Tags)
		}
		sort.Strings(spaces[i].Tags)
	}
	writeJSON(w, 200, spaces)
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

type catalogProject struct {
	ID            string   `json:"id"`
	SpaceID       string   `json:"space_id"`
	ParentID      string   `json:"parent_id"`
	Name          string   `json:"name"`
	Description   string   `json:"description"`
	InheritACL    bool     `json:"inherit_acl"`
	ArticleCount  int      `json:"article_count"`
	LastUpdated   string   `json:"last_updated,omitempty"`
	Tags          []string `json:"tags"`
	EffectiveRole string   `json:"effective_role,omitempty"`
}
