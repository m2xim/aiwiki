package server

import (
	"math"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/kljensen/snowball/english"
	"github.com/kljensen/snowball/russian"
)

// Field weights of the relevance score.
const (
	weightTitle      = 10.0
	weightTags       = 6.0
	weightSummary    = 4.0
	weightProperties = 3.0
	weightBody       = 1.0
	weakFactor       = 0.15 // substring-only match relative to a word/stem match
	bodyTFCap        = 4.0  // saturation of the body term-frequency factor
	titleAllBonus    = 5.0  // every term strongly matched in the title
	snippetWindow    = 200  // runes
)

const (
	fTitle = iota
	fTags
	fSummary
	fProps
	fBody
	numFields
)

var fieldNames = [numFields]string{"title", "tags", "summary", "properties", "body"}
var fieldWeights = [numFields]float64{weightTitle, weightTags, weightSummary, weightProperties, weightBody}

// normRune lower-cases a rune and folds yo into ye. It is a 1:1 rune mapping,
// so rune indexes of the normalized text equal those of the original.
func normRune(r rune) rune {
	r = unicode.ToLower(r)
	if r == 'ё' {
		return 'е'
	}
	return r
}

func normText(s string) string { return strings.Map(normRune, s) }

func isWordRune(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }

// scanWords calls fn for every word of the normalized text s (byte offsets).
// A hyphenated word such as "wg-quick" is reported whole and then by parts.
func scanWords(s string, fn func(start, end int)) {
	n := len(s)
	i := 0
	var hy [8]int
	for i < n {
		r, w := rune(s[i]), 1
		if r >= utf8.RuneSelf {
			r, w = utf8.DecodeRuneInString(s[i:])
		}
		if !isWordRune(r) {
			i += w
			continue
		}
		start := i
		hyphens := hy[:0]
		i += w
		for i < n {
			r, w = rune(s[i]), 1
			if r >= utf8.RuneSelf {
				r, w = utf8.DecodeRuneInString(s[i:])
			}
			if isWordRune(r) {
				i += w
				continue
			}
			if r == '-' && i+1 < n {
				r2, _ := utf8.DecodeRuneInString(s[i+1:])
				if isWordRune(r2) {
					hyphens = append(hyphens, i)
					i++
					continue
				}
			}
			break
		}
		fn(start, i)
		if len(hyphens) > 0 {
			ps := start
			for _, h := range hyphens {
				fn(ps, h)
				ps = h + 1
			}
			fn(ps, i)
		}
	}
}

// matcher holds the query terms and a per-query stem cache.
type matcher struct {
	terms   []string
	stems   []string
	isWord  []bool // false: substring-only term (paths, IPs, punctuation)
	runeLen []int
	cache   map[string]string
}

func newMatcher(q string) *matcher {
	m := &matcher{cache: map[string]string{}}
	seen := map[string]bool{}
	for _, chunk := range strings.Fields(normText(q)) {
		t := strings.TrimFunc(chunk, func(r rune) bool { return !isWordRune(r) })
		if t == "" {
			t = chunk
		}
		if seen[t] {
			continue
		}
		seen[t] = true
		word := true
		for i, r := range t {
			if !isWordRune(r) && !(r == '-' && i > 0 && i < len(t)-1) {
				word = false
				break
			}
		}
		m.terms = append(m.terms, t)
		m.isWord = append(m.isWord, word)
		m.runeLen = append(m.runeLen, utf8.RuneCountInString(t))
		if word {
			m.stems = append(m.stems, m.stem(t))
		} else {
			m.stems = append(m.stems, "")
		}
	}
	return m
}

// stem returns the Snowball stem of an already normalized word; the language
// is chosen by alphabet. Hyphenated words, digits and mixed words stay as is.
func (m *matcher) stem(w string) string {
	if st, ok := m.cache[w]; ok {
		return st
	}
	st := w
	cyr, lat, other := false, false, false
	for _, r := range w {
		switch {
		case r >= 'a' && r <= 'z':
			lat = true
		case unicode.Is(unicode.Cyrillic, r):
			cyr = true
		default:
			other = true
		}
	}
	if !other {
		if cyr && !lat {
			st = russian.Stem(w, true)
		} else if lat && !cyr {
			st = english.Stem(w, true)
		}
	}
	m.cache[w] = st
	return st
}

// wordMatches reports whether document word w (stem ws) matches term j strongly.
func (m *matcher) wordMatches(w, ws string, j int) bool {
	if ws == m.stems[j] {
		return true
	}
	return m.runeLen[j] >= 3 && strings.HasPrefix(w, m.terms[j])
}

// scanField counts strong matches of every term in the normalized field text.
func (m *matcher) scanField(norm string, tf []int) {
	scanWords(norm, func(a, b int) {
		w := norm[a:b]
		ws := m.stem(w)
		for j := range m.terms {
			if m.isWord[j] && m.wordMatches(w, ws, j) {
				tf[j]++
			}
		}
	})
}

type scored struct {
	hit     SearchHit
	matched int     // number of terms matched somewhere
	art     Article // full article, body kept for the snippet
}

func strongScore(f, tf int) float64 {
	if f == fBody {
		return math.Min(1+math.Log(1+float64(tf)), bodyTFCap)
	}
	return fieldWeights[f] * (1 + 0.2*math.Log(float64(tf)))
}

// rankArticle scores one article against the query. ok is false when no term matched.
func (m *matcher) rankArticle(a Article) (sc scored, ok bool) {
	var props strings.Builder
	for k, v := range a.Properties {
		props.WriteString(k)
		props.WriteByte(' ')
		props.WriteString(v)
		props.WriteByte('\n')
	}
	texts := [numFields]string{
		normText(a.Title), normText(join(a.Tags, "\n")), normText(a.Summary),
		normText(props.String()), normText(a.Body),
	}
	nt := len(m.terms)
	tf := make([]int, nt)
	termMatched := make([]bool, nt)
	titleStrong := 0
	var score float64
	var matchedIn []string
	for f := 0; f < numFields; f++ {
		if texts[f] == "" {
			continue
		}
		for j := range tf {
			tf[j] = 0
		}
		m.scanField(texts[f], tf)
		fieldHit := false
		for j := 0; j < nt; j++ {
			if tf[j] > 0 {
				score += strongScore(f, tf[j])
				termMatched[j] = true
				fieldHit = true
				if f == fTitle {
					titleStrong++
				}
			} else if strings.Contains(texts[f], m.terms[j]) {
				score += weakFactor * fieldWeights[f]
				termMatched[j] = true
				fieldHit = true
			}
		}
		if fieldHit {
			matchedIn = append(matchedIn, fieldNames[f])
		}
	}
	cnt := 0
	for _, b := range termMatched {
		if b {
			cnt++
		}
	}
	if cnt == 0 {
		return sc, false
	}
	if titleStrong == nt {
		score += titleAllBonus
	}
	sc.art = a
	a.Body = ""
	sc.hit = SearchHit{Article: a, Score: score, MatchedIn: matchedIn}
	sc.matched = cnt
	return sc, true
}

// rankAndPage picks the mode, sorts candidates (given in updated_at DESC, id
// order, which the stable sort keeps for ties) and returns the requested page.
func (m *matcher) rankAndPage(cands []scored, p SearchParams) SearchResult {
	nterms := len(m.terms)
	mode := "all"
	var list []scored
	for _, c := range cands {
		if c.matched == nterms {
			list = append(list, c)
		}
	}
	if len(list) == 0 && nterms >= 2 && p.AllowAny {
		mode = "any"
		list = cands
	}
	res := SearchResult{Hits: []SearchHit{}, Mode: mode}
	if len(list) == 0 {
		return res
	}
	sort.SliceStable(list, func(i, j int) bool {
		if list[i].matched != list[j].matched {
			return list[i].matched > list[j].matched
		}
		return list[i].hit.Score > list[j].hit.Score
	})
	res.Total = len(list)
	if p.Offset < len(list) {
		end := p.Offset + p.Limit
		if end > len(list) {
			end = len(list)
		}
		for _, c := range list[p.Offset:end] {
			h := c.hit
			h.Snippet = m.snippet(c.art)
			res.Hits = append(res.Hits, h)
		}
	}
	return res
}

type matchPos struct{ pos, length, term int }

// snippet builds an excerpt of the body (or, failing that, the summary).
func (m *matcher) snippet(a Article) string {
	if s := m.excerpt(a.Body); s != "" {
		return s
	}
	return m.excerpt(a.Summary)
}

func (m *matcher) excerpt(orig string) string {
	if orig == "" {
		return ""
	}
	norm := normText(orig)
	var ms []matchPos
	has := make([]bool, len(m.terms))
	lastByte, lastRune := 0, 0 // rune position is accumulated: word offsets ascend
	scanWords(norm, func(a, b int) {
		if len(ms) >= 1000 {
			return
		}
		w := norm[a:b]
		ws := m.stem(w)
		for j := range m.terms {
			if m.isWord[j] && m.wordMatches(w, ws, j) {
				lastRune += utf8.RuneCountInString(norm[lastByte:a])
				lastByte = a
				ms = append(ms, matchPos{lastRune, utf8.RuneCountInString(w), j})
				has[j] = true
			}
		}
	})
	for j, t := range m.terms {
		if has[j] {
			continue
		}
		if i := strings.Index(norm, t); i >= 0 {
			ms = append(ms, matchPos{utf8.RuneCountInString(norm[:i]), m.runeLen[j], j})
		}
	}
	if len(ms) == 0 {
		return ""
	}
	sort.SliceStable(ms, func(i, j int) bool { return ms[i].pos < ms[j].pos })
	best, bestN := 0, -1
	for i := range ms {
		seen := map[int]bool{}
		for k := i; k < len(ms) && ms[k].pos < ms[i].pos+snippetWindow*3/4; k++ {
			seen[ms[k].term] = true
		}
		if len(seen) > bestN {
			best, bestN = i, len(seen)
		}
	}
	rs := []rune(orig)
	mp := ms[best]
	mEnd := mp.pos + mp.length
	start := mp.pos - 60
	if start < 0 {
		start = 0
	}
	end := start + snippetWindow
	if end > len(rs) {
		end = len(rs)
	}
	if mEnd > end {
		end = mEnd
	}
	if start > 0 {
		for i := start; i < mp.pos; i++ {
			if unicode.IsSpace(rs[i]) {
				start = i + 1
				break
			}
		}
	}
	if end < len(rs) {
		for i := end; i > mEnd; i-- {
			if unicode.IsSpace(rs[i]) {
				end = i
				break
			}
		}
	}
	text := strings.Join(strings.Fields(string(rs[start:end])), " ")
	if start > 0 {
		text = "…" + text
	}
	if end < len(rs) {
		text += "…"
	}
	return text
}
