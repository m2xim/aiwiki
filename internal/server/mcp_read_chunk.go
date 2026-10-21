package server

import (
	"bytes"
	"encoding/json"
)

const (
	readChunkDefault = 4000
	readChunkMax     = 16000
)

// readChunkInt reads an optional strict integer argument. A present key must be
// a JSON integer (null, strings, fractions and overflowing numbers are errors).
func readChunkInt(args map[string]json.RawMessage, key string) (v int, present bool, errMsg string) {
	raw, ok := args[key]
	if !ok {
		return 0, false, ""
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) || json.Unmarshal(raw, &v) != nil {
		return 0, true, key + " must be an integer"
	}
	return v, true, ""
}

// toolReadChunk serves wiki_read in bounded mode: max_chars and/or offset count
// Unicode code points of the whole body, or of the section selected by anchor.
// The caller routes here when either argument is supplied.
func (s *Server) toolReadChunk(args map[string]json.RawMessage) any {
	id, bad := toolID(args)
	if bad != nil {
		return bad
	}
	maxChars, hasMax, e1 := readChunkInt(args, "max_chars")
	offset, hasOffset, e2 := readChunkInt(args, "offset")
	rev, hasRev, e3 := readChunkInt(args, "revision")
	for _, e := range []string{e1, e2, e3} {
		if e != "" {
			return toolErr(e)
		}
	}
	anchor, hasAnchor := "", false
	if raw, ok := args["anchor"]; ok {
		hasAnchor = true
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) || json.Unmarshal(raw, &anchor) != nil {
			return toolErr("anchor must be a string")
		}
		if anchor == "" {
			return toolErr("anchor must not be empty")
		}
	}
	if raw, ok := args["outline"]; ok {
		var outline bool
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) || json.Unmarshal(raw, &outline) != nil {
			return toolErr("outline must be a boolean")
		}
		if outline {
			return toolErr("outline cannot be combined with max_chars or offset")
		}
	}
	if !hasMax {
		maxChars = readChunkDefault
	} else if maxChars < 1 || maxChars > readChunkMax {
		return toolErr("max_chars must be between 1 and 16000")
	}
	if hasOffset && offset < 0 {
		return toolErr("offset must be 0 or greater")
	}
	if hasRev && rev < 1 {
		return toolErr("revision must be a positive integer")
	}
	if offset > 0 && !hasRev {
		return toolErr("offset greater than 0 requires an explicit positive revision")
	}

	a, err := s.articleByID(s.DB, id)
	if err != nil {
		return toolErr("article not found")
	}
	currentDeleted := a.DeletedAt
	historical := false
	if hasRev && rev != a.Revision {
		old, ok := s.snapshotRevision(id, rev)
		if !ok {
			return toolErr("revision not found")
		}
		a, historical = old, true
	}
	text := a.Body
	if hasAnchor {
		section, ok := markdownSection(a.Body, anchor)
		if !ok {
			return toolErr("heading not found")
		}
		text = section
	}
	runes := []rune(text)
	total := len(runes)
	if offset > total {
		return toolErr("offset is past the end of the text")
	}
	end := offset + maxChars
	if end > total {
		end = total
	}
	chunk := string(runes[offset:end])
	hasMore := end < total

	out := struct {
		ID         string         `json:"id"`
		Title      string         `json:"title"`
		Revision   int            `json:"revision"`
		Anchor     string         `json:"anchor,omitempty"`
		Partial    bool           `json:"partial"`
		Historical bool           `json:"historical,omitempty"`
		DeletedAt  string         `json:"deleted_at,omitempty"`
		Offset     int            `json:"offset"`
		Chars      int            `json:"chars"`
		TotalChars int            `json:"total_chars"`
		HasMore    bool           `json:"has_more"`
		Text       string         `json:"text"`
		NextRead   map[string]any `json:"next_read,omitempty"`
	}{a.ID, a.Title, a.Revision, anchor, true, historical, currentDeleted, offset, end - offset, total, hasMore, chunk, nil}
	if hasMore {
		next := map[string]any{"id": a.ID, "revision": a.Revision, "offset": end, "max_chars": maxChars}
		if hasAnchor {
			next["anchor"] = anchor
		}
		out.NextRead = next
	}
	return toolOK(out)
}
