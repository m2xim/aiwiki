package server

import (
	"database/sql"
	"net/http"
)

// validPathID reports whether id consists only of [A-Za-z0-9_-] and is not empty.
func validPathID(id string) bool {
	if id == "" || len(id) > 200 {
		return false
	}
	for _, c := range id {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

type favoriteItem struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	SpaceID     string `json:"space_id"`
	ParentID    string `json:"parent_id"`
	Kind        string `json:"kind"`
	Status      string `json:"status"`
	Summary     string `json:"summary"`
	UpdatedAt   string `json:"updated_at"`
	FavoritedAt string `json:"favorited_at"`
}

// favorites handles GET /api/favorites.
func (s *Server) favorites(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		apiErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	rows, err := s.DB.QueryContext(r.Context(), `SELECT a.id,a.title,a.space_id,a.parent_id,a.kind,a.status,a.summary,a.updated_at,f.created_at
FROM favorites f JOIN articles a ON a.id=f.article_id
WHERE a.deleted_at IS NULL AND a.space_id IN (SELECT id FROM spaces WHERE archived_at IS NULL)
ORDER BY f.created_at DESC,a.id`)
	if err != nil {
		apiErr(w, http.StatusInternalServerError, "database error")
		return
	}
	defer rows.Close()
	out := []favoriteItem{}
	for rows.Next() {
		var f favoriteItem
		if err := rows.Scan(&f.ID, &f.Title, &f.SpaceID, &f.ParentID, &f.Kind, &f.Status, &f.Summary, &f.UpdatedAt, &f.FavoritedAt); err != nil {
			apiErr(w, http.StatusInternalServerError, "database error")
			return
		}
		out = append(out, f)
	}
	if rows.Err() != nil {
		apiErr(w, http.StatusInternalServerError, "database error")
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// favorite handles PUT and DELETE /api/favorites/{id}.
func (s *Server) favorite(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut && r.Method != http.MethodDelete {
		apiErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	id := r.PathValue("id")
	if !validPathID(id) {
		apiErr(w, http.StatusBadRequest, "invalid article id")
		return
	}
	if r.Method == http.MethodDelete {
		if _, err := s.DB.ExecContext(r.Context(), "DELETE FROM favorites WHERE article_id=?", id); err != nil {
			apiErr(w, http.StatusInternalServerError, "database error")
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	var deleted sql.NullString
	err := s.DB.QueryRowContext(r.Context(), "SELECT deleted_at FROM articles WHERE id=?", id).Scan(&deleted)
	if err == sql.ErrNoRows {
		apiErr(w, http.StatusNotFound, "article not found")
		return
	}
	if err != nil {
		apiErr(w, http.StatusInternalServerError, "database error")
		return
	}
	if deleted.Valid {
		apiErr(w, http.StatusConflict, "article is deleted")
		return
	}
	insert := "INSERT OR IGNORE INTO favorites(article_id,created_at) VALUES(?,?)"
	if s.DB.pg {
		insert = "INSERT INTO favorites(article_id,created_at) VALUES(?,?) ON CONFLICT DO NOTHING"
	}
	if _, err := s.DB.ExecContext(r.Context(), insert, id, stamp()); err != nil {
		apiErr(w, http.StatusInternalServerError, "database error")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// exportFavorites returns all favorites rows for /api/export (never nil).
func exportFavorites(tx *transaction) []map[string]string {
	out := []map[string]string{}
	rows, err := tx.Query("SELECT article_id,created_at FROM favorites ORDER BY created_at,article_id")
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id, at string
		if rows.Scan(&id, &at) == nil {
			out = append(out, map[string]string{"article_id": id, "created_at": at})
		}
	}
	return out
}
