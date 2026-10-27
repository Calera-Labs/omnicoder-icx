// SPDX-License-Identifier: Apache-2.0
package engine_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/caleralabs/omnicoder-icx/pkg/engine"
	"github.com/shopspring/decimal"
)

// setupGitRepo initializes a temporary git repository with an initial commit.
func setupGitRepo(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()

	runGit := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = tmp
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s failed: %s (%v)", strings.Join(args, " "), string(out), err)
		}
	}

	runGit("init")
	runGit("config", "user.name", "OmniCoder Test")
	runGit("config", "user.email", "support@caleralabs.com")

	// Create initial file
	initialFile := filepath.Join(tmp, "calculator.py")
	initialContent := "def add(a, b):\n    return a - b  # bug\n"
	if err := os.WriteFile(initialFile, []byte(initialContent), 0644); err != nil {
		t.Fatalf("failed to write initial file: %v", err)
	}

	runGit("add", ".")
	runGit("commit", "-m", "initial commit")

	return tmp
}

func TestSpawnCandidateSandboxes_DefaultCountAndProfiles(t *testing.T) {
	repoDir := setupGitRepo(t)
	tm := engine.NewTournamentManager(repoDir)

	sandboxes, err := tm.SpawnCandidateSandboxes(3)
	if err != nil {
		t.Fatalf("SpawnCandidateSandboxes(3) failed: %v", err)
	}
	defer func() {
		_ = tm.TeardownAll()
	}()

	if len(sandboxes) != 3 {
		t.Fatalf("expected 3 sandboxes, got %d", len(sandboxes))
	}

	// Verify Alpha
	alpha := sandboxes[0]
	if alpha.Name != "candidate-alpha" {
		t.Errorf("expected candidate-alpha, got %s", alpha.Name)
	}
	if alpha.Temperature != 0.1 {
		t.Errorf("expected T=0.1, got %f", alpha.Temperature)
	}
	if !strings.Contains(alpha.Strategy, "conservative") {
		t.Errorf("expected conservative strategy, got %s", alpha.Strategy)
	}
	if !alpha.IsWorktree {
		t.Errorf("expected git worktree for git repo, got IsWorktree=false")
	}
	if _, err := os.Stat(alpha.Path); err != nil {
		t.Errorf("sandbox path does not exist on disk: %s", alpha.Path)
	}

	// Verify Beta
	beta := sandboxes[1]
	if beta.Name != "candidate-beta" || beta.Temperature != 0.4 {
		t.Errorf("unexpected beta profile: %+v", beta)
	}

	// Verify Gamma
	gamma := sandboxes[2]
	if gamma.Name != "candidate-gamma" || gamma.Temperature != 0.7 {
		t.Errorf("unexpected gamma profile: %+v", gamma)
	}

	// Verify teardown removes worktrees
	if err := tm.TeardownAll(); err != nil {
		t.Fatalf("TeardownAll failed: %v", err)
	}
	for _, sb := range sandboxes {
		if _, err := os.Stat(sb.Path); !os.IsNotExist(err) {
			t.Errorf("expected sandbox path to be deleted after teardown: %s", sb.Path)
		}
	}
}

func TestSpawnCandidateSandboxes_CustomK(t *testing.T) {
	repoDir := setupGitRepo(t)
	tm := engine.NewTournamentManager(repoDir)

	sandboxes, err := tm.SpawnCandidateSandboxes(5)
	if err != nil {
		t.Fatalf("SpawnCandidateSandboxes(5) failed: %v", err)
	}
	defer func() {
		_ = tm.TeardownAll()
	}()

	if len(sandboxes) != 5 {
		t.Fatalf("expected 5 sandboxes, got %d", len(sandboxes))
	}
	if sandboxes[3].Name != "candidate-delta" {
		t.Errorf("expected candidate-delta, got %s", sandboxes[3].Name)
	}
	if sandboxes[4].Name != "candidate-epsilon" {
		t.Errorf("expected candidate-epsilon, got %s", sandboxes[4].Name)
	}
}

func TestSpawnCandidateSandboxes_FallbackNonGit(t *testing.T) {
	nonGitDir := t.TempDir()
	sampleFile := filepath.Join(nonGitDir, "hello.txt")
	_ = os.WriteFile(sampleFile, []byte("hello world"), 0644)

	tm := engine.NewTournamentManager(nonGitDir)
	sandboxes, err := tm.SpawnCandidateSandboxes(2)
	if err != nil {
		t.Fatalf("SpawnCandidateSandboxes on non-git directory failed: %v", err)
	}
	defer func() {
		_ = tm.TeardownAll()
	}()

	if len(sandboxes) != 2 {
		t.Fatalf("expected 2 sandboxes, got %d", len(sandboxes))
	}
	for _, sb := range sandboxes {
		if sb.IsWorktree {
			t.Errorf("expected IsWorktree=false for non-git repo")
		}
		copiedFile := filepath.Join(sb.Path, "hello.txt")
		data, err := os.ReadFile(copiedFile)
		if err != nil || string(data) != "hello world" {
			t.Errorf("file not copied properly to sandbox: %v, content=%s", err, string(data))
		}
	}
}

func TestCalculateCandidateScore_Formula(t *testing.T) {
	// Formula: Score = 100 * II(Pass) + 50 * (Pass/Tot) - 20 * Lines - 50 * II(Lint)

	// Case 1: Pass, 0 regressions, 2 lines changed, 0 linter errors
	// Score: 100 + 50 - 40 - 0 = 110
	score1, elim1, _ := engine.CalculateCandidateScore(true, 10, 10, 2, 0)
	if elim1 {
		t.Error("expected candidate 1 not eliminated")
	}
	if !score1.Equal(decimal.NewFromInt(110)) {
		t.Errorf("expected score 110, got %s", score1)
	}

	// Case 2: Pass, 0 regressions, 5 lines changed, 0 linter errors
	// Score: 100 + 50 - 100 - 0 = 50
	score2, elim2, _ := engine.CalculateCandidateScore(true, 10, 10, 5, 0)
	if elim2 {
		t.Error("expected candidate 2 not eliminated")
	}
	if !score2.Equal(decimal.NewFromInt(50)) {
		t.Errorf("expected score 50, got %s", score2)
	}
	if !score1.GreaterThan(score2) {
		t.Errorf("expected candidate 1 (2 lines) to outscore candidate 2 (5 lines)")
	}

	// Case 3: Pass, 0 regressions, 2 lines changed, 1 linter error
	// Score: 100 + 50 - 40 - 50 = 60
	score3, elim3, _ := engine.CalculateCandidateScore(true, 10, 10, 2, 1)
	if elim3 {
		t.Error("expected candidate 3 not eliminated")
	}
	if !score3.Equal(decimal.NewFromInt(60)) {
		t.Errorf("expected score 60, got %s", score3)
	}

	// Case 4: Fail reproducer -> Eliminated
	score4, elim4, reason4 := engine.CalculateCandidateScore(false, 0, 0, 1, 0)
	if !elim4 {
		t.Error("expected candidate 4 eliminated due to reproducer fail")
	}
	if !strings.Contains(reason4, "verification failed") {
		t.Errorf("unexpected elimination reason: %s", reason4)
	}
	if score4.GreaterThan(decimal.Zero) {
		t.Errorf("expected score <= 0 on failed reproducer, got %s", score4)
	}

	// Case 5: Pass reproducer, but regression tests fail (8/10) -> Eliminated
	_, elim5, reason5 := engine.CalculateCandidateScore(true, 8, 10, 1, 0)
	if !elim5 {
		t.Error("expected candidate 5 eliminated due to regression")
	}
	if !strings.Contains(reason5, "Regression tests failed") {
		t.Errorf("unexpected elimination reason: %s", reason5)
	}
}

func TestEvaluateCandidate_PassAndFail(t *testing.T) {
	repoDir := setupGitRepo(t)
	tm := engine.NewTournamentManager(repoDir)

	sandboxes, err := tm.SpawnCandidateSandboxes(2)
	if err != nil {
		t.Fatalf("failed to spawn sandboxes: %v", err)
	}
	defer func() {
		_ = tm.TeardownAll()
	}()

	// Sandbox 0: Candidate Alpha fixes the bug
	fixedCode := "def add(a, b):\n    return a + b\n"
	if err := os.WriteFile(filepath.Join(sandboxes[0].Path, "calculator.py"), []byte(fixedCode), 0644); err != nil {
		t.Fatalf("failed to write fixed code: %v", err)
	}

	// Reproducer script: tests that add(2, 3) == 5.
	// Protocol: exit 0 on pass, exit 1 on fail.
	reproducerScript := `
import sys
from calculator import add

if add(2, 3) == 5:
    print("REPRODUCTION_PASS")
    sys.exit(0)
else:
    print("REPRODUCTION_FAIL")
    sys.exit(1)
`

	// Evaluate Sandbox 0 (Fixed)
	scoreAlpha := tm.EvaluateCandidate(sandboxes[0], reproducerScript)
	if !scoreAlpha.ReproducerPassed {
		t.Errorf("expected Alpha reproducer to pass, output: %s", scoreAlpha.ReproducerOutput)
	}
	if scoreAlpha.Eliminated {
		t.Errorf("expected Alpha not eliminated, reason: %s", scoreAlpha.EliminationReason)
	}
	if scoreAlpha.Score.LessThanOrEqual(decimal.Zero) {
		t.Errorf("expected Alpha score > 0, got %s", scoreAlpha.Score)
	}

	// Evaluate Sandbox 1 (Unfixed)
	scoreBeta := tm.EvaluateCandidate(sandboxes[1], reproducerScript)
	if scoreBeta.ReproducerPassed {
		t.Errorf("expected Beta reproducer to fail")
	}
	if !scoreBeta.Eliminated {
		t.Errorf("expected Beta to be eliminated")
	}
}

func TestSelectWinner_HighestScoreAndPruning(t *testing.T) {
	repoDir := setupGitRepo(t)
	tm := engine.NewTournamentManager(repoDir)

	sbAlpha := engine.Sandbox{ID: "sb_alpha", Name: "candidate-alpha", Temperature: 0.1, Path: repoDir}
	sbBeta := engine.Sandbox{ID: "sb_beta", Name: "candidate-beta", Temperature: 0.4, Path: repoDir}
	sbGamma := engine.Sandbox{ID: "sb_gamma", Name: "candidate-gamma", Temperature: 0.7, Path: repoDir}

	scores := []engine.CandidateScore{
		{
			Sandbox:            sbAlpha,
			ReproducerPassed:   true,
			LinesChanged:       2,
			Score:              decimal.NewFromInt(110),
			ScoreFloat:         110.0,
			Eliminated:         false,
			ReproducerDuration: 100 * time.Millisecond,
		},
		{
			Sandbox:            sbBeta,
			ReproducerPassed:   false,
			LinesChanged:       1,
			Score:              decimal.NewFromInt(-20),
			ScoreFloat:         -20.0,
			Eliminated:         true,
			EliminationReason:  "reproducer failed",
			ReproducerDuration: 90 * time.Millisecond,
		},
		{
			Sandbox:            sbGamma,
			ReproducerPassed:   true,
			LinesChanged:       8,
			Score:              decimal.NewFromInt(50),
			ScoreFloat:         50.0,
			Eliminated:         false,
			ReproducerDuration: 120 * time.Millisecond,
		},
	}

	winner, err := tm.SelectWinner(scores)
	if err != nil {
		t.Fatalf("SelectWinner failed: %v", err)
	}
	if winner.ID != sbAlpha.ID {
		t.Errorf("expected winner to be %s (Score 110), got %s", sbAlpha.ID, winner.ID)
	}
}

func TestSelectWinner_AllEliminated(t *testing.T) {
	tm := engine.NewTournamentManager(".")

	scores := []engine.CandidateScore{
		{
			Sandbox:          engine.Sandbox{ID: "sb1"},
			ReproducerPassed: false,
			Eliminated:       true,
		},
		{
			Sandbox:          engine.Sandbox{ID: "sb2"},
			ReproducerPassed: false,
			Eliminated:       true,
		},
	}

	_, err := tm.SelectWinner(scores)
	if err == nil {
		t.Fatal("expected error when all candidates are eliminated")
	}
	if !strings.Contains(err.Error(), "eliminated") {
		t.Errorf("unexpected error message: %v", err)
	}
}

func TestSelectWinner_Empty(t *testing.T) {
	tm := engine.NewTournamentManager(".")
	_, err := tm.SelectWinner(nil)
	if err == nil {
		t.Fatal("expected error on empty scores")
	}
}

func TestSandbox_ApplyFileEditsAndCountLines(t *testing.T) {
	repoDir := setupGitRepo(t)
	tm := engine.NewTournamentManager(repoDir)

	sandboxes, err := tm.SpawnCandidateSandboxes(1)
	if err != nil {
		t.Fatalf("failed to spawn sandbox: %v", err)
	}
	defer func() {
		_ = tm.TeardownAll()
	}()

	sb := sandboxes[0]
	diff := `--- a/calculator.py
+++ b/calculator.py
@@ -1,2 +1,2 @@
 def add(a, b):
-    return a - b  # bug
+    return a + b
`
	edit := engine.FileEdit{
		Path: "calculator.py",
		Diff: diff,
	}

	if err := sb.ApplyFileEdits([]engine.FileEdit{edit}); err != nil {
		t.Fatalf("ApplyFileEdits failed: %v", err)
	}

	// Verify file was patched
	patched, rErr := os.ReadFile(filepath.Join(sb.Path, "calculator.py"))
	if rErr != nil || !strings.Contains(string(patched), "return a + b") {
		t.Fatalf("patch not applied cleanly: %v\n%s", rErr, string(patched))
	}

	// Verify line count detection
	lines, countErr := sb.CountLinesChanged()
	if countErr != nil {
		t.Fatalf("CountLinesChanged error: %v", countErr)
	}
	if lines <= 0 {
		t.Errorf("expected lines changed > 0, got %d", lines)
	}
}

func TestSandbox_CheckLinters(t *testing.T) {
	repoDir := setupGitRepo(t)
	tm := engine.NewTournamentManager(repoDir)

	sandboxes, err := tm.SpawnCandidateSandboxes(1)
	if err != nil {
		t.Fatalf("failed to spawn sandbox: %v", err)
	}
	defer func() {
		_ = tm.TeardownAll()
	}()

	sb := sandboxes[0]

	// Write code with unclosed triple quote
	brokenPy := filepath.Join(sb.Path, "broken.py")
	_ = os.WriteFile(brokenPy, []byte(`"""unclosed docstring`), 0644)

	errs, _ := sb.CheckLinters()
	if len(errs) == 0 {
		t.Error("expected linter error for unclosed triple quote")
	}
}

func TestGlobalTournamentAPI(t *testing.T) {
	repoDir := setupGitRepo(t)
	engine.SetDefaultTournamentRepo(repoDir)

	sandboxes, err := engine.SpawnCandidateSandboxes(2)
	if err != nil {
		t.Fatalf("global SpawnCandidateSandboxes failed: %v", err)
	}
	defer func() {
		_ = engine.TeardownSandboxes(sandboxes)
	}()

	if len(sandboxes) != 2 {
		t.Fatalf("expected 2 sandboxes, got %d", len(sandboxes))
	}

	// Fix code in sandbox 0
	_ = os.WriteFile(filepath.Join(sandboxes[0].Path, "calculator.py"), []byte("def add(a, b):\n    return a + b\n"), 0644)
	reproducer := "import sys\nfrom calculator import add\nsys.exit(0 if add(1, 2) == 3 else 1)\n"

	var scores []engine.CandidateScore
	for _, sb := range sandboxes {
		sc := engine.EvaluateCandidate(sb, reproducer)
		scores = append(scores, sc)
	}

	winner, err := engine.SelectWinner(scores)
	if err != nil {
		t.Fatalf("global SelectWinner failed: %v", err)
	}
	if winner.Name != "candidate-alpha" {
		t.Errorf("expected candidate-alpha, got %s", winner.Name)
	}
}
