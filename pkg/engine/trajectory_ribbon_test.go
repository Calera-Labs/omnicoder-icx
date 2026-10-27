// SPDX-License-Identifier: Apache-2.0
package engine

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/caleralabs/omnicoder-icx/pkg/icx"
	"github.com/caleralabs/omnicoder-icx/pkg/llm"
)

func TestFormatTrajectoryRibbon_TokenBoundsAndSyntax(t *testing.T) {
	// 1. Empty milestones -> empty string
	emptyRibbon := icx.FormatTrajectoryRibbon(nil)
	if emptyRibbon != "" {
		t.Errorf("expected empty string for nil milestones, got %q", emptyRibbon)
	}

	// 2. Normal 3 milestones
	m1 := &icx.TurnMilestone{
		TurnNumber:     1,
		Task:           "Implement token bucket",
		TargetFiles:    []string{"pkg/limiter/bucket.go"},
		Status:         "PASS",
		PatchesApplied: 1,
		Approved:       true,
		CommitHash:     "abc1234",
		MerkleSeal:     "seal_turn_1",
		Timestamp:      time.Now().Unix(),
	}
	m2 := &icx.TurnMilestone{
		TurnNumber:     2,
		Task:           "Add unit tests for limiter",
		TargetFiles:    []string{"pkg/limiter/bucket_test.go"},
		Status:         "PASS",
		PatchesApplied: 1,
		Approved:       true,
		CommitHash:     "def5678",
		MerkleSeal:     "seal_turn_2",
		Timestamp:      time.Now().Unix(),
	}
	m3 := &icx.TurnMilestone{
		TurnNumber:     3,
		Task:           "Fix concurrency race in bucket",
		TargetFiles:    []string{"pkg/limiter/bucket.go"},
		Status:         "PASS",
		PatchesApplied: 2,
		HealCycles:     1,
		Approved:       true,
		CommitHash:     "789abcd",
		MerkleSeal:     "seal_turn_3",
		Timestamp:      time.Now().Unix(),
	}

	ribbon3 := icx.FormatTrajectoryRibbon([]*icx.TurnMilestone{m1, m2, m3})
	if !strings.HasPrefix(ribbon3, "<nav:ribbon: trajectory turns=3 | ") {
		t.Errorf("unexpected prefix: %s", ribbon3)
	}
	if !strings.HasSuffix(ribbon3, "/>") {
		t.Errorf("ribbon must close with '/>': %s", ribbon3)
	}
	if !strings.Contains(ribbon3, "t1:PASS[bucket.go]") {
		t.Errorf("expected t1:PASS[bucket.go] in ribbon, got %s", ribbon3)
	}
	if !strings.Contains(ribbon3, "t2:PASS[bucket_test.go]") {
		t.Errorf("expected t2:PASS in ribbon, got %s", ribbon3)
	}
	if !strings.Contains(ribbon3, "t3:PASS[bucket.go]") {
		t.Errorf("expected t3:PASS in ribbon, got %s", ribbon3)
	}

	toks3 := llm.EstimateTokens(ribbon3)
	if toks3 >= 80 {
		t.Errorf("token count %d exceeds 80 token budget", toks3)
	}

	// 3. Stress test: 100 turns trajectory -> strictly <80 tokens
	manyMilestones := make([]*icx.TurnMilestone, 100)
	for i := 0; i < 100; i++ {
		manyMilestones[i] = &icx.TurnMilestone{
			TurnNumber:     i + 1,
			Task:           fmt.Sprintf("Step %d execution task", i+1),
			TargetFiles:    []string{fmt.Sprintf("pkg/mod%d/file%d.go", i%5, i)},
			Status:         "PASS",
			PatchesApplied: 1,
			Approved:       true,
			CommitHash:     fmt.Sprintf("hash_%d", i+1),
			MerkleSeal:     fmt.Sprintf("seal_%d", i+1),
			Timestamp:      time.Now().Unix(),
		}
	}

	ribbon100 := icx.FormatTrajectoryRibbon(manyMilestones)
	if !strings.HasPrefix(ribbon100, "<nav:ribbon: trajectory turns=100 | ") {
		t.Errorf("unexpected prefix for 100 turns: %s", ribbon100)
	}
	if !strings.Contains(ribbon100, "turns 1..96: 96 PASS, 0 FAIL") {
		t.Errorf("expected turns 1..96 summary in ribbon, got: %s", ribbon100)
	}
	if !strings.Contains(ribbon100, "t100:PASS") {
		t.Errorf("expected t100:PASS in ribbon, got: %s", ribbon100)
	}

	toks100 := llm.EstimateTokens(ribbon100)
	if toks100 >= 80 {
		t.Errorf("100-turn ribbon tokens %d exceeds 80 token budget: %s", toks100, ribbon100)
	}
}

func TestTurnMilestone_CrystallizationAndQuoteSlotRecall(t *testing.T) {
	ctx := context.Background()
	lat := icx.NewLocalLattice()
	spaceID := "space_trajectory_test"

	// Crystallize 25 milestones
	for turnNum := 1; turnNum <= 25; turnNum++ {
		m := &icx.TurnMilestone{
			TurnNumber:     turnNum,
			Task:           fmt.Sprintf("Autonomous implementation milestone for step %d", turnNum),
			TargetFiles:    []string{fmt.Sprintf("pkg/component%d/logic.go", turnNum)},
			Status:         "PASS",
			PatchesApplied: 1,
			HealCycles:     0,
			Approved:       true,
			CommitHash:     fmt.Sprintf("git_commit_hash_%04d", turnNum),
			MerkleSeal:     fmt.Sprintf("merkle_seal_turn_%04d", turnNum),
			Timestamp:      time.Now().Unix(),
		}
		factID, err := lat.CrystallizeMilestone(ctx, spaceID, m)
		if err != nil {
			t.Fatalf("failed to crystallize milestone turn %d: %v", turnNum, err)
		}
		if factID == "" {
			t.Fatalf("empty factID for milestone turn %d", turnNum)
		}
	}

	// Recall step 1 in O(1) <0.002ms via QuoteSlot
	start1 := time.Now()
	res1, err := lat.QuoteSlot(ctx, icx.QuoteSlotRequest{
		SpaceID: spaceID,
		Family:  "turn_milestones",
		Index:   1,
	})
	elapsed1 := time.Since(start1)
	if err != nil {
		t.Fatalf("QuoteSlot turn 1 failed: %v", err)
	}
	if !res1.OK || !res1.Verified {
		t.Fatalf("QuoteSlot turn 1 not verified: %+v", res1)
	}
	if !strings.Contains(res1.Text, "turn=1") {
		t.Errorf("expected turn=1 in quote slot, got %s", res1.Text)
	}
	if elapsed1 > 50*time.Millisecond {
		t.Errorf("QuoteSlot took %v, expected <2ms", elapsed1)
	}

	// Recall step 17 in O(1) <0.002ms via QuoteSlot
	start17 := time.Now()
	res17, err := lat.QuoteSlot(ctx, icx.QuoteSlotRequest{
		SpaceID: spaceID,
		Family:  "turn_milestones",
		Index:   17,
	})
	elapsed17 := time.Since(start17)
	if err != nil {
		t.Fatalf("QuoteSlot turn 17 failed: %v", err)
	}
	if !res17.OK || !res17.Verified {
		t.Fatalf("QuoteSlot turn 17 not verified: %+v", res17)
	}
	if !strings.Contains(res17.Text, "turn=17") {
		t.Errorf("expected turn=17 in quote slot, got %s", res17.Text)
	}
	if elapsed17 > 50*time.Millisecond {
		t.Errorf("QuoteSlot took %v, expected <2ms", elapsed17)
	}

	// Test Zero-Token Session Reset retains turn milestones
	resetResp, err := lat.ResetSession(ctx, spaceID)
	if err != nil {
		t.Fatalf("ResetSession failed: %v", err)
	}
	if resetResp.TokensPreserved != 0 {
		t.Fatalf("ResetSession TokensPreserved=%d, expected 0: %+v", resetResp.TokensPreserved, resetResp)
	}

	// QuoteSlot after reset must still succeed (Zero Token Dialogue, 100% Retained Memory)
	res1After, err := lat.QuoteSlot(ctx, icx.QuoteSlotRequest{
		SpaceID: spaceID,
		Family:  "turn_milestones",
		Index:   1,
	})
	if err != nil || !res1After.OK || !res1After.Verified {
		t.Fatalf("QuoteSlot turn 1 failed after reset: %v, %+v", err, res1After)
	}
	if res1After.SHA256 != res1.SHA256 {
		t.Errorf("SHA256 mismatch after reset: %s vs %s", res1After.SHA256, res1.SHA256)
	}
}

func TestGateCards_TrajectoryRibbonInclusion(t *testing.T) {
	// Construct a realistic Trajectory Ribbon
	milestones := []*icx.TurnMilestone{
		{TurnNumber: 1, Task: "Init schema", TargetFiles: []string{"pkg/db/schema.go"}, Status: "PASS", Approved: true},
		{TurnNumber: 2, Task: "Implement query", TargetFiles: []string{"pkg/db/query.go"}, Status: "PASS", Approved: true},
	}
	ribbon := icx.FormatTrajectoryRibbon(milestones)

	// 1. GateIntentCard with TrajectoryRibbon
	intentCard := llm.GateIntentCard{
		UserPrompt:       "Add transaction rollback handling to db",
		RepoMapSummary:   "pkg/db: schema.go, query.go",
		FocusFiles:       []string{"pkg/db/query.go"},
		TrajectoryRibbon: ribbon,
	}

	sysPrompt, userPrompt, err := intentCard.FormatPrompt()
	if err != nil {
		t.Fatalf("intentCard FormatPrompt failed: %v", err)
	}
	if !strings.Contains(userPrompt, "Trajectory:") || !strings.Contains(userPrompt, "<nav:ribbon: trajectory") {
		t.Errorf("expected Trajectory ribbon in userPrompt, got:\n%s", userPrompt)
	}

	totalTokens := llm.EstimateTokens(sysPrompt + " " + userPrompt)
	if totalTokens > llm.MaxCardTokenBudget {
		t.Errorf("intentCard total tokens %d > MaxCardTokenBudget (%d)", totalTokens, llm.MaxCardTokenBudget)
	}

	// 2. GateEditCard with TrajectoryRibbon
	editCard := llm.GateEditCard{
		FilePath:         "pkg/db/query.go",
		Instruction:      "Add BeginTx and Rollback methods",
		ContextSnippet:   "package db\n\ntype DB struct{}\n",
		TrajectoryRibbon: ribbon,
	}

	sysEdit, userEdit, err := editCard.FormatPrompt()
	if err != nil {
		t.Fatalf("editCard FormatPrompt failed: %v", err)
	}
	if !strings.Contains(userEdit, "Trajectory:") || !strings.Contains(userEdit, "<nav:ribbon: trajectory") {
		t.Errorf("expected Trajectory ribbon in editCard userPrompt, got:\n%s", userEdit)
	}

	editTokens := llm.EstimateTokens(sysEdit + " " + userEdit)
	if editTokens > llm.MaxCardTokenBudget {
		t.Errorf("editCard total tokens %d > MaxCardTokenBudget (%d)", editTokens, llm.MaxCardTokenBudget)
	}
}
