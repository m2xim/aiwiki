package server

import (
	"math"
	"sort"
	"strings"
)

// Section ranking is a weighted lexical score. It is not semantic search: it
// only knows word/stem/prefix/substring overlap, so synonyms, cross-language
// matches and negation are not understood.
//
// For every effective query term t and section s:
//
//		score(s) = sum_t idf(t) * max(own heading + ancestor heading + own body, article context)
//
//	  - own heading: weight 6, log-damped term frequency.
//	  - ancestor heading: weight 2, counted only when the own heading lacks the term.
//	  - own body: weight 4 times a BM25 saturation with length normalization.
//	    Other sections' body text is never inherited.
//	  - article context: best of title/tags/summary/property values (weights below),
//	    the same for every section of the article.
//	  - idf is BM25-style and computed per query over the filtered articles, so a
//	    word present in most articles contributes little however often it repeats.
//	  - context and local evidence for the same term are not added twice; an
//	    incidental cross-reference must not win by repeating identity metadata.
const (
	secWeightHeading  = 6.0
	secWeightAncestor = 2.0
	secWeightBody     = 4.0
	secBM25K1         = 1.2
	secBM25B          = 0.75
	secMinAvgWords    = 40.0 // keeps tiny corpora from over-rewarding short sections
)

// Context fields, in matched_in order.
const (
	ctxTitle = iota
	ctxTags
	ctxSummary
	ctxProps
	numCtx
)

var ctxNames = [numCtx]string{"title", "tags", "summary", "properties"}

// Identity is stronger than an incidental mention in a different article's body.
// Context-only hits cannot prove a fact, even when identity makes them rank first.
var ctxWeights = [numCtx]float64{8.0, 5.0, 4.0, 3.0}

// sectionStopwords are function words and retrieval imperatives that carry no
// topic. Negation (not/no/без/не/нет), temporal words and domain nouns stay.
var sectionStopwords = func() map[string]bool {
	set := map[string]bool{}
	for _, w := range strings.Fields(`
		a an the of in on at to for from by with and or is are was were be been it this that these those as
		how what which when where who why do does did can could should would i we you they my our your
		find show give tell please retrieve locate lookup
		и в во на с со к ко по о об от до из за у для при про что как это этот эта эти какой какая какие какое
		где когда кто почему ли или а но же бы то мы вы я он она они чем
		найди найдите найти покажи покажите показать дай дайте ищи искать расскажи пожалуйста`) {
		set[w] = true
	}
	return set
}()

// effectiveMatcher drops stopword terms. If nothing would remain, the query is
// kept as typed.
func effectiveMatcher(m *matcher) *matcher {
	kept := make([]string, 0, len(m.terms))
	for j, term := range m.terms {
		if m.isWord[j] && sectionStopwords[term] {
			continue
		}
		kept = append(kept, term)
	}
	if len(kept) == 0 || len(kept) == len(m.terms) {
		return m
	}
	return newMatcher(strings.Join(kept, " "))
}

// termScan holds per-term evidence in one piece of text.
type termScan struct {
	tf    []int
	weak  []bool // substring-only match
	words int
}

func (ts termScan) hit(j int) bool { return ts.tf[j] > 0 || ts.weak[j] }

type articleContext struct {
	weight []float64 // per term, best context field
	fields [numCtx]bool
	any    bool
}

type sectionUnit struct {
	article        Article
	section        searchableSection
	head, anc, doc termScan // doc = own body text
	ctx            *articleContext
	order          int
}

type sectionRanker struct {
	m          *matcher
	articles   int
	sections   int
	totalWords int
	df         []int // articles in which each term appears anywhere
	units      []sectionUnit
}

func newSectionRanker(m *matcher) *sectionRanker {
	return &sectionRanker{m: m, df: make([]int, len(m.terms))}
}

func (r *sectionRanker) scan(text string) termScan {
	norm := normText(text)
	n := len(r.m.terms)
	ts := termScan{tf: make([]int, n), weak: make([]bool, n)}
	r.m.scanField(norm, ts.tf)
	scanWords(norm, func(int, int) { ts.words++ })
	for j, term := range r.m.terms {
		if ts.tf[j] == 0 && strings.Contains(norm, term) {
			ts.weak[j] = true
		}
	}
	return ts
}

func fieldStrength(tf int, weak bool) float64 {
	switch {
	case tf > 0:
		return 1 + 0.2*math.Log(float64(tf))
	case weak:
		return weakFactor
	}
	return 0
}

// bodyStrength is BM25 term saturation with length normalization, at most k1+1.
func bodyStrength(tf int, weak bool, words, avgWords float64) float64 {
	t := float64(tf)
	if tf == 0 {
		if !weak {
			return 0
		}
		t = weakFactor
	}
	norm := secBM25K1 * (1 - secBM25B + secBM25B*words/avgWords)
	return t * (secBM25K1 + 1) / (t + norm)
}

func (r *sectionRanker) context(a Article) *articleContext {
	values := make([]string, 0, len(a.Properties))
	for _, v := range a.Properties {
		values = append(values, v)
	}
	sort.Strings(values)
	texts := [numCtx]string{a.Title, join(a.Tags, "\n"), a.Summary, strings.Join(values, "\n")}
	ctx := &articleContext{weight: make([]float64, len(r.m.terms))}
	for f, text := range texts {
		if text == "" {
			continue
		}
		ts := r.scan(text)
		for j := range r.m.terms {
			if w := ctxWeights[f] * fieldStrength(ts.tf[j], ts.weak[j]); w > 0 {
				ctx.fields[f], ctx.any = true, true
				if w > ctx.weight[j] {
					ctx.weight[j] = w
				}
			}
		}
	}
	return ctx
}

// addArticle scans one article's sections and keeps those with any evidence.
func (r *sectionRanker) addArticle(a Article, sections []searchableSection, order *int) {
	if len(sections) == 0 {
		return
	}
	r.articles++
	ctx := r.context(a)
	seen := make([]bool, len(r.m.terms))
	for j, w := range ctx.weight {
		seen[j] = w > 0
	}
	var kept []sectionUnit
	for _, sec := range sections {
		body := sec.text
		if sec.heading != "" {
			if i := strings.IndexByte(body, '\n'); i >= 0 {
				body = body[i+1:]
			} else {
				body = ""
			}
		}
		ancestors := strings.TrimSuffix(strings.TrimSuffix(sec.path, sec.heading), " > ")
		u := sectionUnit{article: a, section: sec, ctx: ctx, order: *order,
			head: r.scan(sec.heading), anc: r.scan(ancestors), doc: r.scan(body)}
		*order++
		r.sections++
		r.totalWords += u.doc.words
		local := false
		for j := range r.m.terms {
			if u.head.hit(j) || u.anc.hit(j) || u.doc.hit(j) {
				seen[j], local = true, true
			}
		}
		if local || ctx.any {
			kept = append(kept, u)
		}
	}
	for j, ok := range seen {
		if ok {
			r.df[j]++
		}
	}
	r.units = append(r.units, kept...)
}

// results scores and orders candidates: all-terms mode first, partial fallback
// only when no section covers every effective term.
func (r *sectionRanker) results() ([]sectionSearchCandidate, string) {
	n := len(r.m.terms)
	avg := secMinAvgWords
	if r.sections > 0 {
		avg = math.Max(avg, float64(r.totalWords)/float64(r.sections))
	}
	idf := make([]float64, n)
	for j := range idf {
		df := float64(r.df[j])
		idf[j] = math.Log(1 + (float64(r.articles)-df+0.5)/(df+0.5))
	}
	var all, partial []sectionSearchCandidate
	for _, u := range r.units {
		var score, localScore float64
		matched := 0
		for j := 0; j < n; j++ {
			h := secWeightHeading * fieldStrength(u.head.tf[j], u.head.weak[j])
			a := 0.0
			if h == 0 {
				a = secWeightAncestor * fieldStrength(u.anc.tf[j], u.anc.weak[j])
			}
			b := secWeightBody * bodyStrength(u.doc.tf[j], u.doc.weak[j], float64(u.doc.words), avg)
			local := h + a + b
			localScore += idf[j] * local
			if total := math.Max(local, u.ctx.weight[j]); total > 0 {
				matched++
				score += idf[j] * total
			}
		}
		if matched == 0 {
			continue
		}
		c := sectionSearchCandidate{article: u.article, section: u.section, matched: matched,
			matchedIn: sectionMatchedIn(r.m, u), sortScore: score, localScore: localScore, order: u.order}
		partial = append(partial, c)
		if matched == n {
			all = append(all, c)
		}
	}
	mode, candidates := "all", all
	if len(candidates) == 0 && n >= 2 {
		mode, candidates = "any", partial
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		x, y := candidates[i], candidates[j]
		if x.sortScore != y.sortScore {
			return x.sortScore > y.sortScore
		}
		// Context may give siblings the same main score. Prefer actual section
		// evidence before timestamps or document order in that case.
		if x.localScore != y.localScore {
			return x.localScore > y.localScore
		}
		if x.article.UpdatedAt != y.article.UpdatedAt {
			return x.article.UpdatedAt > y.article.UpdatedAt
		}
		if x.article.ID != y.article.ID {
			return x.article.ID < y.article.ID
		}
		return x.order < y.order // context-only siblings keep document order
	})
	return candidates, mode
}

// sectionMatchedIn lists local fields (heading, ancestor_heading, body) first,
// then article context fields (title, tags, summary, properties).
func sectionMatchedIn(m *matcher, u sectionUnit) []string {
	var head, anc, body bool
	for j := range m.terms {
		head = head || u.head.hit(j)
		anc = anc || u.anc.hit(j)
		body = body || u.doc.hit(j)
	}
	out := []string{}
	for _, f := range []struct {
		on   bool
		name string
	}{{head, "heading"}, {anc, "ancestor_heading"}, {body, "body"}} {
		if f.on {
			out = append(out, f.name)
		}
	}
	for f, on := range u.ctx.fields {
		if on {
			out = append(out, ctxNames[f])
		}
	}
	return out
}
