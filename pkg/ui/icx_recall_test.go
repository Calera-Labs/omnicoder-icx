package ui

import (
	"context"
	"strings"
	"testing"

	"github.com/caleralabs/omnicoder-icx/pkg/icx"
)

func TestSpaceIDForProject(t *testing.T) {
	cases := map[string]string{
		"":                             "default",
		".":                            "default",
		"projects/analytics-service":   "omni_projects_analytics-service",
		"projects\\Analytics Service/": "omni_projects_analytics_service",
	}
	for in, want := range cases {
		if got := spaceIDForProject(in); got != want {
			t.Errorf("spaceIDForProject(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestBuildSession_ProjectSpacesAreIsolated(t *testing.T) {
	ctx := context.Background()
	lat := icx.NewLocalLattice()

	a := NewBuildSession(lat, t.TempDir())
	a.SpaceID = spaceIDForProject("projects/alpha-app")
	_ = a.Open(ctx, "build alpha", nil)
	if _, err := a.CrystallizeFile(ctx, "db.py", "def connect_alpha_database():\n    return 1\n", ""); err != nil {
		t.Fatal(err)
	}

	b := NewBuildSession(lat, t.TempDir())
	b.SpaceID = spaceIDForProject("projects/beta-app")
	_ = b.Open(ctx, "build beta", nil)

	if got := b.PromptRecall(ctx, "connect_alpha_database"); got != "" {
		t.Errorf("project beta must not recall project alpha's files: %s", got)
	}
	if got := a.PromptRecall(ctx, "connect_alpha_database"); !strings.Contains(got, "db.py") || !strings.Contains(got, icxRecallHeader) {
		t.Errorf("project alpha should recall its own file, got %q", got)
	}
}

func TestBuildSession_SearchContextHonestWhenEmpty(t *testing.T) {
	ctx := context.Background()
	b := NewBuildSession(icx.NewLocalLattice(), t.TempDir())
	b.SpaceID = spaceIDForProject("projects/empty")
	_ = b.Open(ctx, "empty", nil)
	out := b.SearchContext(ctx, "nonexistent_symbol_zzz", 5)
	if !strings.Contains(out, "No matching facts") {
		t.Errorf("expected honest empty result, got %q", out)
	}
	var nilSess *BuildSession
	if !strings.Contains(nilSess.SearchContext(ctx, "x", 3), "[unavailable]") {
		t.Error("nil session must report unavailable, not panic or invent hits")
	}
}

func TestParseSearchAction(t *testing.T) {
	in := ParseAllActionIntents(`[ACTION:SEARCH query="where are records inserted"]`)
	if len(in) != 1 || in[0].Type != ActionSearch || in[0].Query() != "where are records inserted" {
		t.Fatalf("bad keyed parse: %+v", in)
	}
	in = ParseAllActionIntents(`[ACTION:SEARCH insert_record]`)
	if len(in) != 1 || in[0].Type != ActionSearch || in[0].Query() != "insert_record" {
		t.Fatalf("bad unkeyed parse: %+v", in)
	}
}
