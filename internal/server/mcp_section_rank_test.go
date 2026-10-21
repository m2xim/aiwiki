package server

import (
	"reflect"
	"strings"
	"testing"
)

func headingsOf(hits []sectionSearchHit) []string {
	out := make([]string, len(hits))
	for i, h := range hits {
		out[i] = h.Title + "/" + h.Heading
	}
	return out
}

func TestSectionRankMetadataJoinsLocalSection(t *testing.T) {
	s := testServer(t)
	id, _ := writeArticle(t, s, map[string]any{"space_id": "general", "title": "Zebra gateway",
		"body": "# Setup\nToken rotation steps.\n"})
	writeArticle(t, s, map[string]any{"space_id": "general", "title": "Unrelated notes",
		"body": "# Setup\nToken rotation steps.\n"})

	_, _, r := callSectionSearch(t, s, map[string]any{"q": "gateway token", "limit": 20})
	if r.Mode != "all" || r.Total != 1 || r.Results[0].ID != id {
		t.Fatalf("title + section text should satisfy all terms: %#v", r)
	}
	if got, want := r.Results[0].MatchedIn, []string{"body", "title"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("matched_in = %v, want %v", got, want)
	}
}

func TestSectionRankCommonWordsAreNoise(t *testing.T) {
	s := testServer(t)
	writeArticle(t, s, map[string]any{"space_id": "general", "title": "Chatter",
		"body": "# Notes\n" + strings.Repeat("how to find the thing. ", 30)})
	want, _ := writeArticle(t, s, map[string]any{"space_id": "general", "title": "Keys",
		"body": "# Keys\nRotation happens monthly.\n"})

	_, _, r := callSectionSearch(t, s, map[string]any{"q": "how to find the rotation"})
	if r.Mode != "all" || r.Total != 1 || r.Results[0].ID != want {
		t.Fatalf("stopwords must not become required terms: %#v", r)
	}
	// Negation and a query made only of function words are kept.
	writeArticle(t, s, map[string]any{"space_id": "general", "title": "Rules", "body": "# Rule\nNot allowed here.\n"})
	writeArticle(t, s, map[string]any{"space_id": "general", "title": "Rules2", "body": "# Rule\nAllowed only here.\n"})
	_, _, r = callSectionSearch(t, s, map[string]any{"q": "not allowed"})
	if r.Mode != "all" || r.Total != 1 || r.Results[0].Title != "Rules" {
		t.Fatalf("negation word dropped: %#v", r)
	}
	_, _, r = callSectionSearch(t, s, map[string]any{"q": "how to"})
	if r.Total == 0 {
		t.Fatalf("all-stopword query should fall back to the typed terms")
	}

	for _, tc := range []struct {
		q    string
		want []string
	}{
		{"покажи как найти backup", []string{"backup"}},
		{"find the not-found page", []string{"not-found", "page"}},
		{"без пароля", []string{"без", "пароля"}},
	} {
		if got := effectiveMatcher(newMatcher(tc.q)).terms; !reflect.DeepEqual(got, tc.want) {
			t.Errorf("effective terms for %q = %v, want %v", tc.q, got, tc.want)
		}
	}
}

func TestSectionRankRepeatedCommonBodyLosesToRareIdentity(t *testing.T) {
	s := testServer(t)
	for i := 0; i < 6; i++ {
		writeArticle(t, s, map[string]any{"space_id": "general", "title": "Filler",
			"body": "# Ops\n" + strings.Repeat("service ", 40)})
	}
	rare, _ := writeArticle(t, s, map[string]any{"space_id": "general", "title": "Quasar",
		"body": "# Ops\nunrelated words only\n"})

	_, _, r := callSectionSearch(t, s, map[string]any{"q": "service quasar"})
	if r.Mode != "any" || len(r.Results) == 0 || r.Results[0].ID != rare {
		t.Fatalf("rare identity term should beat repeated common word: %#v", r)
	}
	if got := r.Results[0].MatchedIn; !reflect.DeepEqual(got, []string{"title"}) {
		t.Fatalf("context-only hit must be reported as title only: %v", got)
	}
}

func TestSectionRankOwnHeadingBeatsInheritedAncestor(t *testing.T) {
	s := testServer(t)
	writeArticle(t, s, map[string]any{"space_id": "general", "title": "Guide",
		"body": "# Reports\nx\n## Overview\ny\n# Billing\nz\n"})
	_, _, r := callSectionSearch(t, s, map[string]any{"q": "billing", "limit": 20})
	if r.Total != 1 || r.Results[0].Heading != "Billing" {
		t.Fatalf("only the section holding the heading matches: %#v", r)
	}

	writeArticle(t, s, map[string]any{"space_id": "general", "title": "Manual",
		"body": "# Billing\nintro\n## Overview\ndetails\n# Other\n## Billing\nmore\n"})
	_, _, r = callSectionSearch(t, s, map[string]any{"q": "billing", "limit": 20})
	var overview int = -1
	for i, h := range r.Results {
		if h.Title == "Manual" && h.Heading == "Overview" {
			overview = i
			if !reflect.DeepEqual(h.MatchedIn, []string{"ancestor_heading"}) {
				t.Fatalf("ancestor evidence must be labelled: %v", h.MatchedIn)
			}
		}
	}
	if overview != 3 || r.Total != 4 {
		t.Fatalf("inherited ancestor match must rank after own-heading matches: %v", headingsOf(r.Results))
	}
}

func TestSectionRankContextOnlyAmbiguity(t *testing.T) {
	s := testServer(t)
	writeArticle(t, s, map[string]any{"space_id": "general", "title": "Orbit",
		"body": "# One\na\n# Two\nb\n# Three\nc\n"})
	writeArticle(t, s, map[string]any{"space_id": "general", "title": "Other",
		"body": "# Facts\nThe orbit is elliptical.\n"})

	_, _, r := callSectionSearch(t, s, map[string]any{"q": "orbit", "limit": 20})
	if want := []string{"Orbit/One", "Orbit/Two", "Orbit/Three", "Other/Facts"}; !reflect.DeepEqual(headingsOf(r.Results), want) {
		t.Fatalf("named article first, context-only siblings in document order: %v", headingsOf(r.Results))
	}
	for _, h := range r.Results[:3] {
		if !reflect.DeepEqual(h.MatchedIn, []string{"title"}) || h.Snippet != "" {
			t.Fatalf("context-only hit must not borrow text evidence: %#v", h)
		}
	}
	if !strings.Contains(r.Results[3].Snippet, "orbit") || !reflect.DeepEqual(r.Results[3].MatchedIn, []string{"body"}) {
		t.Fatalf("snippet or local evidence missing for body hit: %+v", r.Results[3])
	}
}

func TestSectionRankExplicitHistoricalIntent(t *testing.T) {
	s := testServer(t)
	writeArticle(t, s, map[string]any{"space_id": "general", "title": "Credentials",
		"body": "# Old procedure\nThe previous password rotation was manual.\n# Current procedure\nPassword rotation is automatic.\n"})

	_, _, r := callSectionSearch(t, s, map[string]any{"q": "previous password rotation"})
	if r.Mode != "all" || r.Total != 1 || r.Results[0].Heading != "Old procedure" {
		t.Fatalf("historical query must retrieve historical material: %#v", r)
	}
	_, _, r = callSectionSearch(t, s, map[string]any{"q": "current password rotation"})
	if r.Total != 1 || r.Results[0].Heading != "Current procedure" {
		t.Fatalf("current query: %#v", r)
	}
	_, _, r = callSectionSearch(t, s, map[string]any{"q": "password rotation", "limit": 20})
	if r.Total != 2 {
		t.Fatalf("historical sections must not be penalised: %#v", r)
	}
}

func TestSectionRankPartialPrefersRareTermOverCommonCount(t *testing.T) {
	s := testServer(t)
	for i := 0; i < 6; i++ {
		writeArticle(t, s, map[string]any{"space_id": "general", "title": "Note",
			"body": "# Log\n" + strings.Repeat("common ", 10)})
	}
	rare, _ := writeArticle(t, s, map[string]any{"space_id": "general", "title": "Note",
		"body": "# Log\nrarething once\n"})
	_, _, r := callSectionSearch(t, s, map[string]any{"q": "common rarething"})
	if r.Mode != "any" || r.Results[0].ID != rare {
		t.Fatalf("idf should rank the rare term first: %#v", r)
	}
}
