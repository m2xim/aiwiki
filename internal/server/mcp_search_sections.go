package server

import (
	"encoding/json"
	"math"
	"sort"
	"strings"
	"unicode/utf8"
)

type sectionSearchHit struct {
	ID          string         `json:"id"`
	Title       string         `json:"title"`
	SpaceID     string         `json:"space_id"`
	Revision    int            `json:"revision"`
	Anchor      string         `json:"anchor"`
	Heading     string         `json:"heading"`
	HeadingPath string         `json:"heading_path"`
	Snippet     string         `json:"snippet"`
	Score       float64        `json:"score"`
	MatchedIn   []string       `json:"matched_in"`
	MatchedBy   []string       `json:"matched_by,omitempty"`
	Similarity  *float64       `json:"similarity,omitempty"`
	ReadArgs    map[string]any `json:"read_args"`
}

type sectionSearchCandidate struct {
	article    Article
	section    searchableSection
	matched    int
	matchedIn  []string
	sortScore  float64
	localScore float64
	order      int
}

type searchableSection struct {
	anchor     string
	heading    string
	path       string
	text       string
	order      int
	bodyOffset int
}

// toolSearchSections searches each heading's own text independently. Ancestor
// sections stop at the next heading of any level, so child text is indexed once.
func (s *Server) toolSearchSections(args map[string]json.RawMessage) any {
	ar := newArgs(args)
	q := ar.str("q")
	spaceID, kind := ar.str("space_id"), ar.str("kind")
	pinned := ar.boolean("pinned")
	useSemantic := true
	ar.value("semantic", "boolean", &useSemantic)
	limit, offset := 5, 0
	tags, properties := ar.strings("tag"), ar.strings("property")
	if bad := ar.fail(); bad != nil {
		return bad
	}
	var intErr string
	if v, present, errMsg := readChunkInt(args, "limit"); errMsg != "" {
		intErr = errMsg
	} else if present {
		limit = v
	}
	if v, present, errMsg := readChunkInt(args, "offset"); errMsg != "" && intErr == "" {
		intErr = errMsg
	} else if errMsg == "" && present {
		offset = v
	}
	if intErr != "" {
		return toolErr(intErr)
	}
	if strings.TrimSpace(q) == "" {
		return toolErr("q must contain searchable terms")
	}
	m := newMatcher(q)
	if len(m.terms) == 0 {
		return toolErr("q must contain searchable terms")
	}
	if limit < 1 || limit > 20 {
		return toolErr("limit must be between 1 and 20")
	}
	if offset < 0 {
		return toolErr("offset must be 0 or greater")
	}

	where := []string{"a.deleted_at IS NULL"}
	queryArgs := []any{}
	if spaceID == "" {
		if s.DB.pg {
			where = append(where, "corporate_container_writable(a.space_id,a.project_id)")
		} else {
			where = append(where, "a.space_id IN (SELECT id FROM spaces WHERE archived_at IS NULL)")
		}
	} else {
		where = append(where, "a.space_id=?")
		queryArgs = append(queryArgs, spaceID)
	}
	if projectID := ar.str("project_id"); projectID != "" {
		where = append(where, "a.project_id=?")
		queryArgs = append(queryArgs, projectID)
	}
	if bad := ar.fail(); bad != nil {
		return bad
	}
	if kind != "" {
		where = append(where, "a.kind=?")
		queryArgs = append(queryArgs, kind)
	}
	if pinned {
		where = append(where, "a.pinned=1")
	}
	for _, tag := range tags {
		if tag != "" {
			where = append(where, s.DB.jsonArrayHasSQL("a.tags"))
			queryArgs = append(queryArgs, tag)
		}
	}
	for _, property := range properties {
		if property == "" {
			continue
		}
		kv := strings.SplitN(property, ":", 2)
		if len(kv) != 2 {
			return toolErr("property must be key:value")
		}
		where = append(where, s.DB.jsonObjectHasSQL("a.properties"))
		queryArgs = append(queryArgs, kv[0], kv[1])
	}
	query := "SELECT " + articleColumns + " FROM articles a WHERE " + strings.Join(where, " AND ") + " ORDER BY a.updated_at DESC,a.id"
	rows, err := s.DB.QueryContext(s.DB.context(), query, queryArgs...)
	if err != nil {
		return toolErr("invalid search or filter")
	}
	defer rows.Close()

	m = effectiveMatcher(m)
	ranker := newSectionRanker(m)
	order := 0
	var visible []visibleArticle
	for rows.Next() {
		a, scanErr := scanArticle(rows)
		if scanErr != nil {
			return toolErr("database error")
		}
		sections := splitSearchSections(a.Body)
		ranker.addArticle(a, sections, &order)
		visible = append(visible, visibleArticle{article: a, sections: sections})
	}
	if err := rows.Err(); err != nil {
		return toolErr("database error")
	}
	candidates, mode := ranker.results()
	semanticStatus, coverage := "", -1.0
	var fused []fusedHit
	if s.semantic != nil {
		semanticStatus = "off"
		if useSemantic {
			var hits []semanticHit
			hits, semanticStatus, coverage = s.semanticRank(q, visible)
			if semanticStatus != "unavailable" && len(hits) > 0 {
				lexical := candidates
				if mode != "all" && semanticStatus == "on" {
					// Partial lexical matches are noise next to a complete semantic
					// ranking; while sections are still being embedded they stay.
					lexical = nil
					mode = "semantic"
				}
				fused = fuseSectionHits(lexical, hits)
			}
		}
	}
	total := len(candidates)
	if fused != nil {
		total = len(fused)
	}
	results := make([]sectionSearchHit, 0, limit)
	if fused != nil && offset < total {
		end := offset + limit
		if end > total {
			end = total
		}
		for _, h := range fused[offset:end] {
			results = append(results, h.hit(m))
		}
	} else if offset < total {
		end := offset + limit
		if end > total {
			end = total
		}
		for _, c := range candidates[offset:end] {
			pos, matched := firstSectionMatch(m, c.section.text)
			results = append(results, sectionSearchHit{
				ID: c.article.ID, Title: c.article.Title, SpaceID: c.article.SpaceID, Revision: c.article.Revision,
				Anchor: c.section.anchor, Heading: c.section.heading, HeadingPath: c.section.path,
				Snippet: sectionSnippet(c.section.text, pos, matched),
				Score:   math.Round(c.sortScore*1000) / 1000, MatchedIn: c.matchedIn,
				ReadArgs: sectionReadArgs(c.article, c.section, pos, matched),
			})
		}
	}
	out := struct {
		Total      int                `json:"total"`
		Offset     int                `json:"offset"`
		Mode       string             `json:"mode"`
		Results    []sectionSearchHit `json:"results"`
		NextOffset *int               `json:"next_offset,omitempty"`
		Semantic   string             `json:"semantic,omitempty"`
		Coverage   *float64           `json:"semantic_coverage,omitempty"`
	}{Total: total, Offset: offset, Mode: mode, Results: results, Semantic: semanticStatus}
	if coverage >= 0 {
		c := math.Round(coverage*1000) / 1000
		out.Coverage = &c
	}
	if len(results) > 0 && offset+len(results) < total {
		next := offset + len(results)
		out.NextOffset = &next
	}
	return toolOK(out)
}

func splitSearchSections(body string) []searchableSection {
	lines := strings.Split(body, "\n")
	headings := parseMarkdownHeadings(lines)
	sections := make([]searchableSection, 0, len(headings)+1)
	if len(headings) == 0 {
		if strings.TrimSpace(body) != "" {
			sections = append(sections, searchableSection{text: body})
		}
		return sections
	}
	if headings[0].start > 0 {
		preamble := strings.Join(lines[:headings[0].start], "\n")
		if strings.TrimSpace(preamble) != "" {
			sections = append(sections, searchableSection{text: preamble, order: 0})
		}
	}
	path := []string{}
	levels := []int{}
	lineOffsets := make([]int, len(lines))
	runeOffset := 0
	for i, line := range lines {
		lineOffsets[i] = runeOffset
		runeOffset += utf8.RuneCountInString(line) + 1
	}
	for i, h := range headings {
		for len(path) > 0 && levels[len(levels)-1] >= h.level {
			path = path[:len(path)-1]
			levels = levels[:len(levels)-1]
		}
		path = append(path, h.title)
		levels = append(levels, h.level)
		end := len(lines)
		if i+1 < len(headings) {
			end = headings[i+1].start
		}
		sections = append(sections, searchableSection{
			anchor: h.slug, heading: h.title, path: strings.Join(path, " > "),
			text: strings.Join(lines[h.start:end], "\n"), order: len(sections), bodyOffset: lineOffsets[h.start],
		})
	}
	anchors := map[string]int{}
	for _, section := range sections {
		if section.anchor != "" {
			anchors[section.anchor]++
		}
	}
	for i := range sections {
		if sections[i].anchor != "" && anchors[sections[i].anchor] > 1 {
			// The legacy reader resolves only the first matching slug. Drop the
			// anchor for every colliding section and read by full-body offset.
			sections[i].anchor = ""
		}
	}
	return sections
}

func sectionSnippet(text string, matchPos int, matched bool) string {
	if !matched {
		return ""
	}
	rs := []rune(text)
	start := matchPos - 40
	if start < 0 {
		start = 0
	}
	end := start + 240
	if end > len(rs) {
		end = len(rs)
	}
	return strings.Join(strings.Fields(string(rs[start:end])), " ")
}

func sectionReadArgs(a Article, section searchableSection, matchPos int, matched bool) map[string]any {
	offset := 0
	args := map[string]any{"id": a.ID, "revision": a.Revision, "max_chars": readChunkDefault}
	if section.anchor != "" {
		args["anchor"] = section.anchor
	}
	if matched {
		offset = matchPos - 40
		if offset < 0 {
			offset = 0
		}
	}
	if section.anchor == "" {
		offset += section.bodyOffset
	}
	args["offset"] = offset
	return args
}

func firstSectionMatch(m *matcher, text string) (int, bool) {
	norm := normText(text)
	positions := []matchPos{}
	has := make([]bool, len(m.terms))
	lastStartByte, lastStartRune := 0, 0
	scanWords(norm, func(start, end int) {
		word := norm[start:end]
		stem := m.stem(word)
		if start > lastStartByte {
			lastStartRune += utf8.RuneCountInString(norm[lastStartByte:start])
			lastStartByte = start
		}
		for j := range m.terms {
			if m.isWord[j] && m.wordMatches(word, stem, j) {
				positions = append(positions, matchPos{pos: lastStartRune, length: utf8.RuneCountInString(word), term: j})
				has[j] = true
			}
		}
	})
	var substrings []matchPos
	for j, term := range m.terms {
		if !has[j] {
			if i := strings.Index(norm, term); i >= 0 {
				substrings = append(substrings, matchPos{pos: i, length: m.runeLen[j], term: j})
			}
		}
	}
	if len(substrings) > 0 {
		sort.Slice(substrings, func(i, j int) bool { return substrings[i].pos < substrings[j].pos })
		cursorByte, cursorRune := 0, 0
		for _, match := range substrings {
			bytePos := match.pos
			cursorRune += utf8.RuneCountInString(norm[cursorByte:bytePos])
			match.pos = cursorRune
			positions = append(positions, match)
			cursorByte = bytePos
		}
	}
	if len(positions) == 0 {
		return 0, false
	}
	sort.SliceStable(positions, func(i, j int) bool { return positions[i].pos < positions[j].pos })
	best, bestTerms := 0, -1
	for i := range positions {
		seen := map[int]bool{}
		for k := i; k < len(positions) && positions[k].pos < positions[i].pos+snippetWindow*3/4; k++ {
			seen[positions[k].term] = true
		}
		if len(seen) > bestTerms {
			best, bestTerms = i, len(seen)
		}
	}
	return positions[best].pos, true
}
