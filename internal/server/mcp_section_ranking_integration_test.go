package server

import (
	"strings"
	"testing"
)

func TestMCPSectionSearchCombinesArticleIdentityWithSection(t *testing.T) {
	s := testServer(t)
	id, revision := writeArticle(t, s, map[string]any{
		"space_id": "engineering", "title": "gamma-service operations", "summary": "Release and recovery procedures.",
		"body": "## Deployment\nStart with a canary and confirm health.\n\n## Recovery\nRestore the prior release.\n",
	})
	writeArticle(t, s, map[string]any{"space_id": "general", "title": "Other product", "body": "## Deployment\nThis mentions gamma-service only as an unrelated example.\n"})
	var result sectionSearchResponse
	mustOK(t, s, "wiki_search_sections", map[string]any{"q": "gamma-service Deployment"}, &result)
	if len(result.Results) == 0 || result.Results[0].ID != id || result.Results[0].Anchor != "deployment" {
		t.Fatalf("identity + heading did not select target: %+v", result)
	}
	hit := result.Results[0]
	var read struct {
		Text     string `json:"text"`
		Revision int    `json:"revision"`
	}
	mustOK(t, s, "wiki_read", hit.ReadArgs, &read)
	if read.Revision != revision || !strings.Contains(read.Text, "Start with a canary") {
		t.Fatalf("address did not read matched section: %+v", read)
	}
}

func TestMCPSectionSearchDoesNotInheritSiblingBody(t *testing.T) {
	s := testServer(t)
	id, _ := writeArticle(t, s, map[string]any{"space_id": "engineering", "title": "Operational guide", "body": "## First\nOrdinary procedure.\n\n## Second\nZEBRA-UNIQUE-922 guidance.\n"})
	var result sectionSearchResponse
	mustOK(t, s, "wiki_search_sections", map[string]any{"q": "ZEBRA-UNIQUE-922"}, &result)
	if result.Total != 1 || result.Results[0].ID != id || result.Results[0].Anchor != "second" {
		t.Fatalf("sibling body leaked into inherited context: %+v", result)
	}
}

func TestMCPSectionSearchIdentityOutranksCrossReference(t *testing.T) {
	s := testServer(t)
	id, _ := writeArticle(t, s, map[string]any{"space_id": "engineering", "title": "Current glacier delivery procedure", "summary": "Approved workflow for glacier deliveries.", "body": "## Steps\nConfirm the receipt before notifying the owner.\n"})
	writeArticle(t, s, map[string]any{"space_id": "engineering", "title": "Historical glacier procedure", "body": "## Historical note\nFor current work refer to Current glacier delivery procedure.\n"})
	var result sectionSearchResponse
	mustOK(t, s, "wiki_search_sections", map[string]any{"q": "current glacier delivery procedure"}, &result)
	if len(result.Results) == 0 || result.Results[0].ID != id {
		t.Fatalf("reference to target outranked target identity: %+v", result)
	}
}

func TestMCPSectionSearchLocalHeadingBreaksContextTie(t *testing.T) {
	s := testServer(t)
	id, _ := writeArticle(t, s, map[string]any{"space_id": "engineering", "title": "Deployment manual", "body": "## Overview\nOrdinary introduction.\n\n## Deployment\nOrdinary steps.\n"})
	var result sectionSearchResponse
	mustOK(t, s, "wiki_search_sections", map[string]any{"q": "deployment"}, &result)
	if len(result.Results) < 2 || result.Results[0].ID != id || result.Results[0].Anchor != "deployment" {
		t.Fatalf("local heading must break equal context score: %+v", result)
	}
}
