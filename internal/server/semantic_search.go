package server

import (
	"math"
	"sort"
	"strconv"
	"strings"
)

type visibleArticle struct {
	article  Article
	sections []searchableSection
}

type semanticHit struct {
	article    Article
	section    searchableSection
	similarity float64
}

type fusedHit struct {
	lexical  *sectionSearchCandidate
	semantic *semanticHit
	score    float64
}

// semanticRank scores the sections of the articles that the request's own query
// returned. It returns the hits ordered by similarity, a status ("on",
// "indexing" while some sections have no vector yet, or "unavailable") and the
// share of the visible chunks that have a vector.
func (s *Server) semanticRank(q string, visible []visibleArticle) ([]semanticHit, string, float64) {
	idx := s.semantic
	ctx := s.DB.context()
	qv, err := idx.queryVector(ctx, q)
	if err != nil {
		return nil, "unavailable", -1
	}
	ids := make([]string, 0, len(visible))
	for _, v := range visible {
		ids = append(ids, v.article.ID)
	}
	idx.ensureLoaded(ctx, s.DB, ids)
	userID := ""
	if s.principal != nil {
		userID = s.principal.ID
	}
	var hits []semanticHit
	total, have := 0, 0
	for _, v := range visible {
		var all []semanticChunk
		articleComplete := true
		for _, sec := range v.sections {
			chunks := idx.sectionChunks(v.article, sec)
			all = append(all, chunks...)
			vecs, complete := idx.lookup(v.article.ID, chunks)
			total += len(chunks)
			have += len(vecs)
			if !complete {
				articleComplete = false
			}
			if len(vecs) == 0 {
				continue
			}
			best := -1.0
			for _, vec := range vecs {
				if d := dot(qv, vec); d > best {
					best = d
				}
			}
			if best >= idx.cfg.MinSimilarity {
				hits = append(hits, semanticHit{article: v.article, section: sec, similarity: best})
			}
		}
		if !articleComplete {
			idx.enqueue(v.article.ID, userID, all)
		}
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].similarity > hits[j].similarity })
	status, coverage := "on", 1.0
	if total > 0 {
		coverage = float64(have) / float64(total)
	}
	if have < total {
		status = "indexing"
	}
	return hits, status, coverage
}

// fuseSectionHits merges the lexical ranking (only full matches are passed in)
// and the semantic ranking by reciprocal rank fusion over the top of each list.
func fuseSectionHits(lexical []sectionSearchCandidate, semantic []semanticHit) []fusedHit {
	byKey := map[string]*fusedHit{}
	var order []string
	key := func(a Article, sec searchableSection) string { return a.ID + "#" + strconv.Itoa(sec.order) }
	for i := range lexical {
		if i >= semanticFuseDepth {
			break
		}
		c := &lexical[i]
		k := key(c.article, c.section)
		if byKey[k] == nil {
			byKey[k] = &fusedHit{}
			order = append(order, k)
		}
		byKey[k].lexical = c
		byKey[k].score += 1 / float64(semanticRRFK+i+1)
	}
	for i := range semantic {
		if i >= semanticFuseDepth {
			break
		}
		h := &semantic[i]
		k := key(h.article, h.section)
		if byKey[k] == nil {
			byKey[k] = &fusedHit{}
			order = append(order, k)
		}
		byKey[k].semantic = h
		byKey[k].score += 1 / float64(semanticRRFK+i+1)
	}
	out := make([]fusedHit, 0, len(order))
	for _, k := range order {
		out = append(out, *byKey[k])
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].score > out[j].score })
	return out
}

func (f fusedHit) hit(m *matcher) sectionSearchHit {
	var a Article
	var sec searchableSection
	var h sectionSearchHit
	if f.lexical != nil {
		a, sec = f.lexical.article, f.lexical.section
		pos, matched := firstSectionMatch(m, sec.text)
		h.Snippet = sectionSnippet(sec.text, pos, matched)
		h.MatchedIn = f.lexical.matchedIn
		h.ReadArgs = sectionReadArgs(a, sec, pos, matched)
		h.MatchedBy = append(h.MatchedBy, "lexical")
	} else {
		a, sec = f.semantic.article, f.semantic.section
		h.Snippet = semanticSnippet(sec.text)
		h.MatchedIn = []string{}
		h.ReadArgs = sectionReadArgs(a, sec, 0, false)
	}
	if f.semantic != nil {
		h.MatchedBy = append(h.MatchedBy, "semantic")
		sim := math.Round(f.semantic.similarity*1000) / 1000
		h.Similarity = &sim
	}
	h.ID, h.Title, h.SpaceID, h.Revision = a.ID, a.Title, a.SpaceID, a.Revision
	h.Anchor, h.Heading, h.HeadingPath = sec.anchor, sec.heading, sec.path
	h.Score = math.Round(f.score*100000) / 100000
	return h
}

// semanticSnippet is the start of the section text without its heading line.
func semanticSnippet(text string) string {
	if strings.HasPrefix(strings.TrimSpace(text), "#") {
		if i := strings.IndexByte(text, '\n'); i >= 0 {
			text = text[i+1:]
		} else {
			text = ""
		}
	}
	rs := []rune(strings.Join(strings.Fields(text), " "))
	if len(rs) > 240 {
		rs = rs[:240]
	}
	return string(rs)
}
