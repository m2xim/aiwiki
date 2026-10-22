package server

import (
	"database/sql"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"reflect"
	"strconv"
)

// AuditEvent is a compact entry describing a change captured by an immutable article revision.
type AuditEvent struct {
	ID              int64    `json:"id"`
	ArticleID       string   `json:"article_id"`
	Title           string   `json:"title"`
	Revision        int      `json:"revision"`
	Author          string   `json:"author"`
	Message         string   `json:"message"`
	CreatedAt       string   `json:"created_at"`
	Changes         []string `json:"changes"`
	SpaceID         string   `json:"space_id"`
	PreviousSpaceID string   `json:"previous_space_id"`
	Deleted         bool     `json:"deleted"`
}

func (s *Server) spaceAudit(w http.ResponseWriter, r *http.Request) {
	spaceID := r.PathValue("id")
	var exists int
	if err := s.DB.QueryRow(`SELECT 1 FROM spaces WHERE id=?`, spaceID).Scan(&exists); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			apiErr(w, http.StatusNotFound, "space not found")
		} else {
			apiErr(w, http.StatusInternalServerError, "database error")
		}
		return
	}

	limit, err := parseAuditInt(r.URL.Query().Get("limit"), 50, 200)
	if err != nil {
		apiErr(w, http.StatusBadRequest, "invalid limit")
		return
	}
	offset, err := parseAuditOffset(r.URL.Query().Get("offset"))
	if err != nil {
		apiErr(w, http.StatusBadRequest, "invalid offset")
		return
	}
	author := r.URL.Query().Get("author")
	hasAuthor := r.URL.Query().Has("author")
	// Candidates: the newest events of the space itself plus the newest moves out of it
	// (previous revision in the space, this one not). Each branch is capped at
	// limit+offset, enough for any page of the union, and uses the per-space index
	// instead of reading every revision.
	// int64 and clamped so 32-bit builds cannot overflow; a huge offset still returns [].
	window := int64(limit) + int64(min(offset, math.MaxInt32))
	cur, prev, next := s.DB.revisionSpaceSQL("r1"), s.DB.revisionSpaceSQL("p1"), s.DB.revisionSpaceSQL("n")
	rows, err := s.DB.Query(`
		WITH cand AS (
		  SELECT id FROM (SELECT id FROM revisions r1 WHERE `+cur+`=? AND (?=0 OR r1.author=?) ORDER BY id DESC LIMIT ?) a
		  UNION
		  SELECT id FROM (SELECT n.id FROM revisions p1
		    JOIN revisions n ON n.article_id=p1.article_id AND n.revision=p1.revision+1
		    WHERE `+prev+`=? AND (`+next+` IS NULL OR `+next+`<>?) AND (?=0 OR n.author=?)
		    ORDER BY n.id DESC LIMIT ?) b)
		SELECT r.id,r.article_id,r.revision,r.author,r.message,r.created_at,r.snapshot,
		       p.snapshot
		FROM cand JOIN revisions r ON r.id=cand.id
		LEFT JOIN revisions p ON p.article_id=r.article_id AND p.revision=r.revision-1
		ORDER BY r.id DESC LIMIT ? OFFSET ?`,
		spaceID, boolInt(hasAuthor), author, window, spaceID, spaceID, boolInt(hasAuthor), author, window, limit, offset)
	if err != nil {
		apiErr(w, http.StatusInternalServerError, "database error")
		return
	}
	defer rows.Close()
	out := []AuditEvent{}
	for rows.Next() {
		var event AuditEvent
		var currentRaw, previousRaw sql.NullString
		if err := rows.Scan(&event.ID, &event.ArticleID, &event.Revision, &event.Author, &event.Message, &event.CreatedAt, &currentRaw, &previousRaw); err != nil {
			apiErr(w, http.StatusInternalServerError, "database error")
			return
		}
		var current Article
		if !currentRaw.Valid || json.Unmarshal([]byte(currentRaw.String), &current) != nil {
			apiErr(w, http.StatusInternalServerError, "corrupt revision snapshot")
			return
		}
		event.Title = current.Title
		event.SpaceID = current.SpaceID
		event.Deleted = current.DeletedAt != ""
		if previousRaw.Valid {
			var previous Article
			if json.Unmarshal([]byte(previousRaw.String), &previous) != nil {
				apiErr(w, http.StatusInternalServerError, "corrupt revision snapshot")
				return
			}
			event.PreviousSpaceID = previous.SpaceID
			event.Changes = changedArticleFields(previous, current)
		} else if event.Revision == 1 {
			event.Changes = []string{"created"}
		} else {
			apiErr(w, http.StatusInternalServerError, "missing previous revision snapshot")
			return
		}
		out = append(out, event)
	}
	if err := rows.Err(); err != nil {
		apiErr(w, http.StatusInternalServerError, "database error")
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func parseAuditInt(raw string, fallback, max int) (int, error) {
	if raw == "" {
		return fallback, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 || n > max {
		return 0, strconv.ErrSyntax
	}
	return n, nil
}

func parseAuditOffset(raw string) (int, error) {
	if raw == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return 0, strconv.ErrSyntax
	}
	return n, nil
}

func changedArticleFields(previous, current Article) []string {
	fields := []struct {
		name string
		a, b any
	}{
		{"title", previous.Title, current.Title},
		{"summary", previous.Summary, current.Summary},
		{"body", previous.Body, current.Body},
		{"kind", previous.Kind, current.Kind},
		{"status", previous.Status, current.Status},
		{"pinned", previous.Pinned, current.Pinned},
		{"tags", previous.Tags, current.Tags},
		{"properties", previous.Properties, current.Properties},
		{"parent_id", previous.ParentID, current.ParentID},
		{"space_id", previous.SpaceID, current.SpaceID},
		{"deleted_at", previous.DeletedAt, current.DeletedAt},
	}
	changes := []string{}
	for _, field := range fields {
		if !reflect.DeepEqual(field.a, field.b) {
			changes = append(changes, field.name)
		}
	}
	return changes
}
