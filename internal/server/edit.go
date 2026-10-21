package server

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"
)

// toolEdit implements wiki_edit: replace_section, append and replace_text.
func (s *Server) toolEdit(args map[string]json.RawMessage) any {
	id, bad := toolID(args)
	if bad != nil {
		return bad
	}
	ar := newArgs(args)
	base := ar.integer("base_revision")
	op, anchor := ar.str("op"), ar.str("anchor")
	text, old := ar.str("text"), ar.str("old")
	author, message := ar.str("author"), ar.str("message")
	if bad := ar.fail(); bad != nil {
		return bad
	}
	if base < 1 {
		return toolErr("base_revision is required (the revision you last read)")
	}
	switch op {
	case "replace_section":
		if anchor == "" {
			return toolErr("anchor is required for replace_section")
		}
		if strings.TrimSpace(text) == "" {
			return toolErr("text is required for replace_section")
		}
	case "append":
		if strings.TrimSpace(text) == "" {
			return toolErr("text is required for append")
		}
	case "replace_text":
		if old == "" {
			return toolErr("old is required for replace_text")
		}
	default:
		return toolErr("op must be one of: replace_section, append, replace_text")
	}
	a, _, err := s.commitUpdate(id, base, author, message, func(cur Article) (Article, error) {
		body, e := applyEdit(cur.Body, op, anchor, text, old)
		if e != nil {
			return cur, e
		}
		cur.Body = body
		return cur, nil
	})
	if err != nil {
		return toolErr(err.Error())
	}
	return toolOK(map[string]any{"id": a.ID, "revision": a.Revision, "updated_at": a.UpdatedAt, "chars": utf8.RuneCountInString(a.Body)})
}

func applyEdit(body, op, anchor, text, old string) (string, error) {
	switch op {
	case "replace_text":
		n := strings.Count(body, old)
		if n != 1 {
			return "", fmt.Errorf("old text must occur exactly once in the body, found %d occurrences", n)
		}
		return strings.Replace(body, old, text, 1), nil
	case "append":
		if anchor == "" {
			trimmed := strings.TrimRight(body, "\n")
			if trimmed == "" {
				return strings.TrimRight(text, "\n") + "\n", nil
			}
			return trimmed + "\n\n" + strings.TrimRight(text, "\n") + "\n", nil
		}
		lines := strings.Split(body, "\n")
		h, ok := findHeading(lines, anchor)
		if !ok {
			return "", fmt.Errorf("heading not found: %s", anchor)
		}
		sec := lines[h.start:h.end]
		for len(sec) > 0 && strings.TrimSpace(sec[len(sec)-1]) == "" {
			sec = sec[:len(sec)-1]
		}
		res := append([]string{}, lines[:h.start]...)
		res = append(res, sec...)
		res = append(res, "")
		res = append(res, strings.Split(strings.TrimRight(text, "\n"), "\n")...)
		if h.end < len(lines) {
			res = append(res, "")
		} else if strings.HasSuffix(body, "\n") {
			res = append(res, "")
		}
		return strings.Join(append(res, lines[h.end:]...), "\n"), nil
	default: // replace_section
		lines := strings.Split(body, "\n")
		h, ok := findHeading(lines, anchor)
		if !ok {
			return "", fmt.Errorf("heading not found: %s", anchor)
		}
		newLines := strings.Split(strings.TrimRight(strings.TrimLeft(text, "\r\n"), "\n"), "\n")
		hs := parseMarkdownHeadings(newLines)
		if len(hs) == 0 || hs[0].start != 0 {
			return "", fmt.Errorf("text must start with the section heading line (e.g. %q) because replace_section replaces the heading too", strings.Repeat("#", h.level)+" "+h.title)
		}
		if hs[0].level != h.level {
			return "", fmt.Errorf("heading level changed from %d to %d; use wiki_write to restructure", h.level, hs[0].level)
		}
		res := append([]string{}, lines[:h.start]...)
		res = append(res, newLines...)
		if h.end < len(lines) || strings.HasSuffix(body, "\n") {
			res = append(res, "")
		}
		return strings.Join(append(res, lines[h.end:]...), "\n"), nil
	}
}

func findHeading(lines []string, anchor string) (markdownHeading, bool) {
	for _, h := range parseMarkdownHeadings(lines) {
		if h.slug == anchor {
			return h, true
		}
	}
	return markdownHeading{}, false
}
