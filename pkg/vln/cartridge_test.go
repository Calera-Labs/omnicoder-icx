// SPDX-License-Identifier: Apache-2.0
package vln

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSWECartridge_DeterministicRecallAndLatency(t *testing.T) {
	cart := NewDefaultSWECartridge()

	start := time.Now()
	hint, boosts, coherence, matched := cart.RecallFromIssue(
		"In django.db.models.sql.query, combining two queries with Query.combine mutates rhs change_map and causes FieldError on join alias.",
		`File "django/db/models/sql/query.py", line 612, in combine`,
	)
	elapsed := time.Since(start)

	if !matched {
		t.Fatalf("expected VLN cartridge to match django-orm-alias-clone basin, got matched=false (coherence=%.3f)", coherence)
	}
	if elapsed > 5*time.Millisecond {
		t.Errorf("expected <5ms VLN recall latency, took %v", elapsed)
	}
	if !strings.Contains(hint, "change_map") {
		t.Errorf("unexpected hint: %q", hint)
	}
	if len(boosts) == 0 {
		t.Errorf("expected non-empty TargetSymbolBoosts")
	}
	words := len(strings.Fields(hint))
	if words > 40 {
		t.Errorf("expected VLN repair invariant <40 words, got %d (%q)", words, hint)
	}
}

func TestSWECartridge_ZeroHallucinationOutOfBasin(t *testing.T) {
	cart := NewDefaultSWECartridge()

	outOfBasinQueries := []struct {
		issue string
		trace string
	}{
		{
			issue: "Add a dark mode toggle button to the React navigation header component.",
			trace: "",
		},
		{
			issue: "Audio driver buffer underrun in ALSA PCM playback loop on Raspberry Pi.",
			trace: "alsa_pcm.c:142",
		},
		{
			issue: "",
			trace: "",
		},
	}

	for i, q := range outOfBasinQueries {
		hint, boosts, coherence, matched := cart.RecallFromIssue(q.issue, q.trace)
		if matched || hint != "" || len(boosts) != 0 {
			t.Errorf("case %d: expected NOT_REGISTERED hard refusal (0.00%% hallucination), got matched=%v hint=%q coherence=%.3f",
				i, matched, hint, coherence)
		}
	}
}

func TestSWECartridge_GobPersistenceRoundTrip(t *testing.T) {
	cart := NewDefaultSWECartridge()
	tmpPath := filepath.Join(t.TempDir(), "vln_swe_code.gob")

	if err := cart.SaveGob(tmpPath); err != nil {
		t.Fatalf("SaveGob failed: %v", err)
	}

	loaded, err := LoadGob(tmpPath)
	if err != nil {
		t.Fatalf("LoadGob failed: %v", err)
	}

	if len(loaded.Archetypes) != len(cart.Archetypes) {
		t.Fatalf("expected %d archetypes after round-trip, got %d", len(cart.Archetypes), len(loaded.Archetypes))
	}

	h1, _, c1, m1 := cart.RecallFromIssue("sympy BlockMatrix _entry IndexError when indexing blocks", "sympy/matrices/expressions/blockmatrix.py")
	h2, _, c2, m2 := loaded.RecallFromIssue("sympy BlockMatrix _entry IndexError when indexing blocks", "sympy/matrices/expressions/blockmatrix.py")

	if m1 != m2 || h1 != h2 || c1 != c2 {
		t.Fatalf("gob round-trip mismatch: (%v, %q, %.4f) vs (%v, %q, %.4f)", m1, h1, c1, m2, h2, c2)
	}

	// Materialize the default SWE cartridge at models/vln_swe_code.gob for runtime and benchmarks
	repoGob := filepath.Join("..", "..", "models", "vln_swe_code.gob")
	_ = cart.SaveGob(repoGob)
}
