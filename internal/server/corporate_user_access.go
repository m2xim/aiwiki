package server

import (
	"net/http"
)

type corporateUserAccessResource struct {
	ResourceType string `json:"resource_type"`
	ResourceID   string `json:"resource_id"`
	Name         string `json:"name"`
	Role         string `json:"role"`
	Archived     bool   `json:"archived"`
	Personal     bool   `json:"personal"`
}

// corporateUserAccess lists the spaces and projects that the target user can
// read and that the calling system administrator may manage. Both roles come
// from corporate_effective_role on the request-scoped connection, so a
// resource appears only when the actor holds manager/owner on it and nothing
// is derived from other resources the actor cannot manage.
func (s *Server) corporateUserAccess(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.corporateActor(w, r, true)
	if !ok {
		return
	}
	target := r.PathValue("id")
	if target == "" {
		apiErr(w, http.StatusNotFound, "user not found")
		return
	}
	var found bool
	if err := s.DB.QueryRow(`SELECT EXISTS(SELECT 1 FROM users WHERE id=?)`, target).Scan(&found); err != nil {
		apiErr(w, http.StatusInternalServerError, "database error")
		return
	}
	if !found {
		apiErr(w, http.StatusNotFound, "user not found")
		return
	}
	rows, err := s.DB.Query(`SELECT resource_type,resource_id,name,target_role,archived,personal FROM (
SELECT 'space'::text AS resource_type,sp.id AS resource_id,sp.name AS name,
 corporate_effective_role(?,sp.id,'') AS target_role,
 corporate_effective_role(?,sp.id,'') AS actor_role,
 sp.archived_at IS NOT NULL AS archived,sp.personal=1 AS personal
FROM spaces sp
UNION ALL
SELECT 'project'::text,p.id,p.name,
 corporate_effective_role(?,p.space_id,p.id),
 corporate_effective_role(?,p.space_id,p.id),
 p.archived_at IS NOT NULL,false
FROM projects p
) access
WHERE corporate_role_rank(actor_role)>=4 AND corporate_role_rank(target_role)>=1
ORDER BY resource_type,name,resource_id`, target, actor, target, actor)
	if err != nil {
		apiErr(w, http.StatusInternalServerError, "database error")
		return
	}
	defer rows.Close()
	out := make([]corporateUserAccessResource, 0)
	for rows.Next() {
		var item corporateUserAccessResource
		if rows.Scan(&item.ResourceType, &item.ResourceID, &item.Name, &item.Role, &item.Archived, &item.Personal) != nil {
			apiErr(w, http.StatusInternalServerError, "database error")
			return
		}
		out = append(out, item)
	}
	if rows.Err() != nil {
		apiErr(w, http.StatusInternalServerError, "database error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"user_id": target, "resources": out})
}
