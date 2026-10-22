package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"github.com/jackc/pgx/v5/pgconn"
	"strings"
)

func artifactToolList() []map[string]any {
	fileSchema := objectSchema(map[string]any{
		"path":     prop("string", "File path, e.g. index.html or js/app.js (letters, digits, . _ -; at most 4 levels; extensions html css js mjs json csv txt md svg png jpg jpeg gif webp)."),
		"content":  prop("string", "File content (UTF-8 text, or base64 when encoding is base64)."),
		"encoding": map[string]any{"type": "string", "enum": []string{"utf8", "base64"}, "description": "utf8 (default) or base64 for binary files."},
		"delete":   prop("boolean", "true removes the file (update only); do not send content."),
	}, "path")
	return []map[string]any{
		{"name": "wiki_artifact_list", "description": "List interactive artifacts (HTML/JS applications with files) you can see, newest first. q searches title, description and the visible text of the files.", "inputSchema": objectSchema(map[string]any{
			"space_id":   prop("string", "Only artifacts of this space."),
			"project_id": prop("string", "Only artifacts directly in this project."),
			"q":          prop("string", "Search words (all must match)."),
			"limit":      prop("integer", "Page size, default 20, maximum 100."),
			"offset":     prop("integer", "Number of results to skip."),
		})},
		{"name": "wiki_artifact_read", "description": "Read an artifact. Without path returns the manifest (files with size, sha256, mime) of the revision. With path returns one text file in bounded chunks of Unicode code points (max_chars, default 4000, at most 16000) pinned to a revision: follow next_read verbatim to continue; offset greater than 0 requires an explicit revision.", "inputSchema": objectSchema(map[string]any{
			"id":        prop("string", "Artifact id."),
			"path":      prop("string", "File to read; omit for the manifest."),
			"revision":  map[string]any{"type": "integer", "minimum": 1, "description": "Revision to read; default the current one."},
			"max_chars": map[string]any{"type": "integer", "minimum": 1, "maximum": 16000, "description": "Chunk size in Unicode code points."},
			"offset":    map[string]any{"type": "integer", "minimum": 0, "description": "Code point offset within the file; nonzero requires revision."},
		}, "id")},
		{"name": "wiki_artifact_write", "description": "Create an artifact (no id; space_id, title and an index.html file required) or change one (id + base_revision). An update is partial: only the listed files are written or deleted, other files, title and description keep their values. A stale base_revision returns a revision conflict. Visibility, sharing and deletion are not available through MCP. The request is limited to about 3 MB.", "inputSchema": objectSchema(map[string]any{
			"id":            prop("string", "Id of the artifact to change; omit to create."),
			"space_id":      prop("string", "Create only: space of the artifact."),
			"project_id":    prop("string", "Create only: project within the space."),
			"title":         prop("string", "Title, at most 300 characters (required on create)."),
			"description":   prop("string", "Short description, at most 2000 characters."),
			"base_revision": map[string]any{"type": "integer", "minimum": 1, "description": "Revision you last read; required when updating."},
			"files":         map[string]any{"type": "array", "items": fileSchema, "description": "Files to write or delete."},
			"message":       prop("string", "Short change description stored in history."),
		})},
	}
}

func (s *Server) toolArtifactList(args map[string]json.RawMessage) any {
	ar := newArgs(args)
	space, project, q := ar.str("space_id"), ar.str("project_id"), ar.str("q")
	limit, offset := ar.integer("limit"), ar.integer("offset")
	if bad := ar.fail(); bad != nil {
		return bad
	}
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}
	if offset < 0 {
		offset = 0
	}
	items, err := s.listArtifacts(space, project, q, limit, offset)
	if err != nil {
		return toolErr("database error")
	}
	return toolOK(map[string]any{"total_returned": len(items), "offset": offset, "artifacts": items})
}

func (s *Server) toolArtifactRead(args map[string]json.RawMessage) any {
	id, bad := toolID(args)
	if bad != nil {
		return bad
	}
	path := ""
	if raw, ok := args["path"]; ok {
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) || json.Unmarshal(raw, &path) != nil {
			return toolErr("path must be a string")
		}
	}
	maxChars, hasMax, e1 := readChunkInt(args, "max_chars")
	offset, hasOffset, e2 := readChunkInt(args, "offset")
	rev, hasRev, e3 := readChunkInt(args, "revision")
	for _, e := range []string{e1, e2, e3} {
		if e != "" {
			return toolErr(e)
		}
	}
	if hasRev && rev < 1 {
		return toolErr("revision must be a positive integer")
	}
	if !hasMax {
		maxChars = readChunkDefault
	} else if maxChars < 1 || maxChars > readChunkMax {
		return toolErr("max_chars must be between 1 and 16000")
	}
	if hasOffset && offset < 0 {
		return toolErr("offset must be 0 or greater")
	}
	if offset > 0 && !hasRev {
		return toolErr("offset greater than 0 requires an explicit positive revision")
	}
	if path == "" && (hasMax || hasOffset) {
		return toolErr("max_chars and offset apply to a file: give path")
	}
	a, err := s.loadArtifact(s.DB, id)
	if err != nil {
		return toolErr("artifact not found")
	}
	if !hasRev {
		rev = a.Revision
	}
	if rev > a.Revision {
		return toolErr("revision not found")
	}
	files, found, err := loadManifest(s.DB, id, rev)
	if err != nil || !found {
		return toolErr("revision not found")
	}
	if path == "" {
		out := map[string]any{"id": a.ID, "title": a.Title, "description": a.Description, "space_id": a.SpaceID, "project_id": a.ProjectID, "visibility": a.Visibility, "revision": rev, "current_revision": a.Revision, "updated_at": a.UpdatedAt, "files": publicManifest(files)}
		if a.DeletedAt != "" {
			out["deleted_at"] = a.DeletedAt
		}
		return toolOK(out)
	}
	var file artifactFile
	ok := false
	for _, f := range files {
		if f.Path == path {
			file, ok = f, true
		}
	}
	if !ok {
		return toolErr("file not found")
	}
	if !strings.HasPrefix(file.MIME, "text/") && file.MIME != "application/json" && file.MIME != "image/svg+xml" {
		return toolErr("binary file: only the manifest entry is available")
	}
	store, err := s.objectStorage()
	if err != nil {
		return toolErr("object storage unavailable")
	}
	s.DB.releaseForTransfer()
	data, err := readArtifactBlob(s.DB.context(), store, file)
	if err != nil {
		if errors.Is(err, errObjectNotFound) {
			return toolErr("file not found")
		}
		return toolErr("could not read file")
	}
	runes := []rune(strings.ToValidUTF8(string(data), "�"))
	total := len(runes)
	if offset > total {
		return toolErr("offset is past the end of the text")
	}
	end := offset + maxChars
	if end > total {
		end = total
	}
	out := map[string]any{"id": a.ID, "path": path, "mime": file.MIME, "revision": rev, "partial": true, "offset": offset, "chars": end - offset, "total_chars": total, "has_more": end < total, "text": string(runes[offset:end])}
	if rev != a.Revision {
		out["historical"] = true
	}
	if end < total {
		out["next_read"] = map[string]any{"id": a.ID, "path": path, "revision": rev, "offset": end, "max_chars": maxChars}
	}
	return toolOK(out)
}

func (s *Server) toolArtifactWrite(args map[string]json.RawMessage) any {
	if _, ok := args["visibility"]; ok {
		return toolErr("visibility cannot be set through MCP")
	}
	id := ""
	if _, has := args["id"]; has {
		var bad any
		if id, bad = toolID(args); bad != nil {
			return bad
		}
	}
	var in struct {
		SpaceID      string            `json:"space_id"`
		ProjectID    string            `json:"project_id"`
		Title        *string           `json:"title"`
		Description  *string           `json:"description"`
		BaseRevision int               `json:"base_revision"`
		Files        *[]artifactFileIn `json:"files"`
		Message      string            `json:"message"`
	}
	delete2 := map[string]json.RawMessage{}
	for k, v := range args {
		if k != "id" {
			delete2[k] = v
		}
	}
	raw, _ := json.Marshal(delete2)
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		return toolErr("invalid arguments: " + clip(err.Error(), 200))
	}
	var detail artifactDetail
	var err error
	ctx := s.DB.context()
	if id == "" {
		c := artifactCreateIn{SpaceID: in.SpaceID, ProjectID: in.ProjectID, Description: "", Message: in.Message}
		if in.Title != nil {
			c.Title = *in.Title
		}
		if in.Description != nil {
			c.Description = *in.Description
		}
		if in.Files != nil {
			c.Files = *in.Files
		}
		detail, err = s.createArtifact(ctx, c)
	} else {
		detail, err = s.updateArtifact(ctx, id, artifactUpdateIn{BaseRevision: in.BaseRevision, Title: in.Title, Description: in.Description, Files: in.Files, Message: in.Message})
	}
	if err != nil {
		var ae *artifactError
		if errors.As(err, &ae) {
			return toolErr(ae.Msg)
		}
		var pg *pgconn.PgError
		if errors.As(err, &pg) && pg.Code == "42501" {
			return toolErr("forbidden")
		}
		return toolErr("database error")
	}
	return toolOK(map[string]any{"id": detail.ID, "revision": detail.Revision, "updated_at": detail.UpdatedAt, "title": detail.Title, "space_id": detail.SpaceID, "file_count": detail.FileCount})
}
