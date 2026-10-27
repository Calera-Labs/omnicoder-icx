// SPDX-License-Identifier: Apache-2.0
// Package engine provides core execution pipeline, turn management, and transaction primitives.
// tournament.go implements Speculative Tournament & Sandboxing:
// - Sub-second Git worktree sandbox allocation, switching, and pruning
// - Multi-candidate speculative patch evaluation (Candidate Alpha, Beta, Gamma)
// - Objective scoring matrix with zero floating-point drift (shopspring/decimal)
// - Deterministic tournament winner selection with regression and linter penalty gates

package engine

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/caleralabs/omnicoder-icx/pkg/canonical"
	"github.com/caleralabs/omnicoder-icx/pkg/devops"
	"github.com/shopspring/decimal"
)

// Standard tournament errors.
var (
	ErrNoViableCandidates  = errors.New("tournament: all candidate sandboxes eliminated")
	ErrEmptyScores         = errors.New("tournament: candidate scores slice cannot be empty")
	ErrInvalidCandidateK   = errors.New("tournament: candidate count k must be positive")
	ErrSandboxNotFound     = errors.New("tournament: sandbox path does not exist")
	ErrReproducerFailed    = errors.New("tournament: candidate reproducer failed")
	ErrReproducerMissing   = errors.New("tournament: reproducer script or specification is empty")
)

// CandidateProfile defines sampling parameters for speculative branches.
type CandidateProfile struct {
	Name        string  `json:"name"`
	Temperature float64 `json:"temperature"`
	Strategy    string  `json:"strategy"`
}

// DefaultCandidateProfiles specifies the canonical multi-temperature distribution.
// - Candidate Alpha (T=0.1): Conservative minimal patch (exact bug fix).
// - Candidate Beta  (T=0.4): Structural refactor (handling broader type checks/edge cases).
// - Candidate Gamma (T=0.7): Alternative AST approach (defensive guard at caller level).
var DefaultCandidateProfiles = []CandidateProfile{
	{Name: "candidate-alpha", Temperature: 0.1, Strategy: "conservative minimal patch"},
	{Name: "candidate-beta", Temperature: 0.4, Strategy: "structural refactor"},
	{Name: "candidate-gamma", Temperature: 0.7, Strategy: "alternative AST defensive guard"},
	{Name: "candidate-delta", Temperature: 0.8, Strategy: "exploratory alternative"},
	{Name: "candidate-epsilon", Temperature: 0.9, Strategy: "diversity branch"},
}

// Sandbox represents an isolated execution environment for a tournament candidate.
type Sandbox struct {
	ID          string       `json:"id"`
	Name        string       `json:"name"`        // e.g. "candidate-alpha", "candidate-beta", "candidate-gamma"
	Path        string       `json:"path"`        // absolute path to worktree/directory
	Branch      string       `json:"branch"`      // git branch name (empty if non-git)
	RepoDir     string       `json:"repo_dir"`    // source git repository root
	IsWorktree  bool         `json:"is_worktree"` // true if managed via git worktree
	Temperature float64      `json:"temperature"` // sampling temperature (0.1, 0.4, 0.7...)
	Strategy    string       `json:"strategy"`    // strategy description
	CreatedAt   time.Time    `json:"created_at"`
	CleanedUp   bool         `json:"cleaned_up"`
	CleanupFn   func() error `json:"-"`
}

// sandboxTeardownMu serializes Teardown. It is package-level (not a Sandbox field)
// because Sandbox is copied by value throughout the tournament code and must not
// embed a lock; teardown is infrequent so contention is negligible.
var sandboxTeardownMu sync.Mutex

// IsEmpty reports whether the sandbox is uninitialized.
func (sb Sandbox) IsEmpty() bool {
	return sb.ID == "" && sb.Path == ""
}

// Teardown cleans up the sandbox environment. If managed via git worktree,
// it forcefully removes the worktree and prunes the temporary branch.
func (sb *Sandbox) Teardown() error {
	if sb == nil || sb.CleanedUp {
		return nil
	}
	sandboxTeardownMu.Lock()
	defer sandboxTeardownMu.Unlock()

	if sb.CleanedUp {
		return nil
	}

	var errs []string
	if sb.CleanupFn != nil {
		if err := sb.CleanupFn(); err != nil {
			errs = append(errs, err.Error())
		}
	} else if sb.IsWorktree && sb.RepoDir != "" {
		if err := removeWorktree(sb.RepoDir, sb.Path, sb.Branch); err != nil {
			errs = append(errs, err.Error())
		}
	} else if sb.Path != "" {
		if err := os.RemoveAll(sb.Path); err != nil && !os.IsNotExist(err) {
			errs = append(errs, err.Error())
		}
	}

	sb.CleanedUp = true
	if len(errs) > 0 {
		return fmt.Errorf("sandbox teardown error for %s: %s", sb.Name, strings.Join(errs, "; "))
	}
	return nil
}

// ApplyPatch applies a unified diff or raw patch text to this sandbox using transactional two-phase commit.
func (sb *Sandbox) ApplyPatch(patchText string) error {
	if sb == nil || sb.Path == "" {
		return ErrSandboxNotFound
	}
	tx := NewTransaction(sb.Path)

	diffs, err := devops.ParseUnifiedDiff(patchText)
	if err == nil && len(diffs) > 0 {
		var edits []FileEdit
		for _, d := range diffs {
			targetPath := d.NewPath
			if targetPath == "" {
				targetPath = d.OldPath
			}
			edits = append(edits, FileEdit{
				Path: targetPath,
				Diff: patchText,
			})
		}
		if err := StageMultipleEdits(tx, edits); err != nil {
			return err
		}
		return CommitTransaction(tx)
	}

	// Fallback to single-file edit or direct transaction stage
	return fmt.Errorf("sandbox: unable to parse unified diff for patch application")
}

// ApplyFileEdits applies a slice of structured file edits atomically to the sandbox.
func (sb *Sandbox) ApplyFileEdits(edits []FileEdit) error {
	if sb == nil || sb.Path == "" {
		return ErrSandboxNotFound
	}
	tx := NewTransaction(sb.Path)
	if err := StageMultipleEdits(tx, edits); err != nil {
		return err
	}
	return CommitTransaction(tx)
}

// CountLinesChanged calculates the total added and removed lines in the sandbox relative to base.
func (sb *Sandbox) CountLinesChanged() (int, error) {
	if sb == nil || sb.Path == "" {
		return 0, ErrSandboxNotFound
	}

	if sb.IsWorktree {
		// Use git diff --numstat HEAD
		cmd := exec.Command("git", "diff", "--numstat", "HEAD")
		cmd.Dir = sb.Path
		out, err := cmd.Output()
		if err == nil {
			totalLines := 0
			scanner := bufio.NewScanner(strings.NewReader(string(out)))
			for scanner.Scan() {
				fields := strings.Fields(scanner.Text())
				if len(fields) >= 2 {
					added, _ := strconv.Atoi(fields[0])
					deleted, _ := strconv.Atoi(fields[1])
					totalLines += added + deleted
				}
			}

			// Include untracked files (excluding reproducer scripts)
			statusCmd := exec.Command("git", "status", "--porcelain")
			statusCmd.Dir = sb.Path
			statusOut, sErr := statusCmd.Output()
			if sErr == nil {
				statusScanner := bufio.NewScanner(strings.NewReader(string(statusOut)))
				for statusScanner.Scan() {
					line := statusScanner.Text()
					if strings.HasPrefix(line, "?? ") {
						rel := strings.TrimSpace(strings.TrimPrefix(line, "?? "))
						if rel == devops.DefaultPythonScript || rel == devops.DefaultGoScript || strings.HasPrefix(rel, "reproduce_") {
							continue
						}
						untrackedFile := filepath.Join(sb.Path, rel)
						if data, rErr := os.ReadFile(untrackedFile); rErr == nil {
							totalLines += strings.Count(string(data), "\n") + 1
						}
					}
				}
			}
			return totalLines, nil
		}
	}

	// Non-git fallback: count differences against sb.RepoDir
	if sb.RepoDir == "" || sb.RepoDir == sb.Path {
		return 0, nil
	}

	total := 0
	_ = filepath.Walk(sb.Path, func(path string, info os.FileInfo, err error) error {
		if err != nil || info == nil || info.IsDir() {
			return nil
		}
		rel, rErr := filepath.Rel(sb.Path, path)
		if rErr != nil || strings.HasPrefix(rel, ".git") {
			return nil
		}
		if rel == devops.DefaultPythonScript || rel == devops.DefaultGoScript || strings.HasPrefix(rel, "reproduce_") {
			return nil
		}

		baseFile := filepath.Join(sb.RepoDir, rel)
		newData, errNew := os.ReadFile(path)
		if errNew != nil {
			return nil
		}

		origData, errOrig := os.ReadFile(baseFile)
		if errOrig != nil {
			// Newly created file
			total += strings.Count(string(newData), "\n") + 1
			return nil
		}

		if canonical.SHA256Bytes(newData) != canonical.SHA256Bytes(origData) {
			// Approximate line changes by diffing lines
			linesNew := strings.Split(string(newData), "\n")
			linesOrig := strings.Split(string(origData), "\n")
			diffCount := len(linesNew) - len(linesOrig)
			if diffCount < 0 {
				diffCount = -diffCount
			}
			if diffCount == 0 {
				diffCount = 2 // minimal edit
			}
			total += diffCount
		}
		return nil
	})

	return total, nil
}

// CheckLinters scans modified source files in the sandbox for syntax, unclosed literal, or linter errors.
func (sb *Sandbox) CheckLinters() ([]string, error) {
	if sb == nil || sb.Path == "" {
		return nil, ErrSandboxNotFound
	}

	var linterErrors []string

	_ = filepath.Walk(sb.Path, func(path string, info os.FileInfo, err error) error {
		if err != nil || info == nil || info.IsDir() {
			return nil
		}
		rel, rErr := filepath.Rel(sb.Path, path)
		if rErr != nil || strings.HasPrefix(rel, ".git") {
			return nil
		}

		ext := strings.ToLower(filepath.Ext(path))
		if ext != ".go" && ext != ".py" {
			return nil
		}

		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil
		}
		content := string(data)

		// 1. Unclosed quotes/delimiters check across Python and Go
		if unclosed, msg := devops.DetectUnclosedLiterals(content, ext); unclosed {
			linterErrors = append(linterErrors, fmt.Sprintf("%s: %s", rel, msg))
		}

		// 2. Go preflight check
		if ext == ".go" {
			if goErr := devops.ValidateGoPreflight(sb.Path, rel, content); goErr != nil {
				linterErrors = append(linterErrors, fmt.Sprintf("%s: %v", rel, goErr))
			}
		}

		return nil
	})

	return linterErrors, nil
}

// CandidateScore captures the multi-factor objective evaluation for a tournament candidate.
type CandidateScore struct {
	Sandbox            Sandbox         `json:"sandbox"`
	ReproducerPassed   bool            `json:"reproducer_passed"`
	ReproducerOutput   string          `json:"reproducer_output,omitempty"`
	ReproducerDuration time.Duration   `json:"reproducer_duration"`
	RegressionPassed   int             `json:"regression_passed"`
	RegressionTotal    int             `json:"regression_total"`
	RegressionRatio    decimal.Decimal `json:"regression_ratio"`
	LinesChanged       int             `json:"lines_changed"`
	LinterErrorsCount  int             `json:"linter_errors_count"`
	LinterErrors       []string        `json:"linter_errors,omitempty"`
	Score              decimal.Decimal `json:"score"`
	ScoreFloat         float64         `json:"score_float"`
	Eliminated         bool            `json:"eliminated"`
	EliminationReason  string          `json:"elimination_reason,omitempty"`
	EvaluatedAt        time.Time       `json:"evaluated_at"`
}

// CalculateCandidateScore implements the canonical scoring formula:
// Score = 100 * II(ReproducerPass) + 50 * (N_passed_regression / N_total_regression) - 20 * LinesChanged - 50 * II(LinterErrors)
// All arithmetic is evaluated with exact decimal precision (shopspring/decimal).
func CalculateCandidateScore(reproducerPass bool, regPassed, regTotal int, linesChanged int, linterErrors int) (decimal.Decimal, bool, string) {
	if !reproducerPass {
		// Reproducer failed: candidate is eliminated
		score := decimal.Zero
		if regTotal > 0 && regPassed > 0 {
			ratio := decimal.NewFromInt(int64(regPassed)).Div(decimal.NewFromInt(int64(regTotal)))
			score = score.Add(decimal.NewFromInt(50).Mul(ratio))
		}
		if linesChanged > 0 {
			score = score.Sub(decimal.NewFromInt(int64(20 * linesChanged)))
		}
		if linterErrors > 0 {
			score = score.Sub(decimal.NewFromInt(50))
		}
		return score, true, "Reproducer verification failed (exit code != 0)"
	}

	// 100 * II(ReproducerPass)
	score := decimal.NewFromInt(100)

	// Regression ratio: 50 * (N_passed / N_total)
	eliminated := false
	var eliminationReason string
	if regTotal > 0 {
		if regPassed < regTotal {
			eliminated = true
			eliminationReason = fmt.Sprintf("Regression tests failed: %d/%d passed", regPassed, regTotal)
		}
		ratio := decimal.NewFromInt(int64(regPassed)).Div(decimal.NewFromInt(int64(regTotal)))
		score = score.Add(decimal.NewFromInt(50).Mul(ratio))
	} else {
		// If no separate regression suite was specified, full regression credit is awarded
		score = score.Add(decimal.NewFromInt(50))
	}

	// Lines changed penalty: - 20 * LinesChanged
	if linesChanged > 0 {
		score = score.Sub(decimal.NewFromInt(int64(20 * linesChanged)))
	}

	// Linter errors penalty: - 50 * II(LinterErrors)
	if linterErrors > 0 {
		score = score.Sub(decimal.NewFromInt(50))
	}

	return score, eliminated, eliminationReason
}

// TournamentManager coordinates Git worktree pools, speculative candidate evaluations,
// and objective winner selection.
type TournamentManager struct {
	RepoDir           string
	WorktreeBaseDir   string
	Timeout           time.Duration
	RegressionCommand string
	Profiles          []CandidateProfile
	mu                sync.Mutex
	sandboxes         map[string]*Sandbox
}

// NewTournamentManager initializes a TournamentManager bound to a base repository.
func NewTournamentManager(repoDir string) *TournamentManager {
	if repoDir == "" {
		repoDir = "."
	}
	absRepo, err := filepath.Abs(repoDir)
	if err == nil {
		repoDir = absRepo
	}

	return &TournamentManager{
		RepoDir:         repoDir,
		WorktreeBaseDir: filepath.Join(os.TempDir(), "omnicoder_tournaments"),
		Timeout:         devops.DefaultReproducerTimeout,
		Profiles:        DefaultCandidateProfiles,
		sandboxes:       make(map[string]*Sandbox),
	}
}

// SpawnCandidateSandboxes allocates k isolated candidate sandboxes (Candidate Alpha, Beta, Gamma, etc.).
// If the underlying repo is a Git repository, it utilizes Git worktrees for sub-second switching (<50ms).
// Otherwise, it transparently falls back to fast directory cloning.
func (tm *TournamentManager) SpawnCandidateSandboxes(k int) ([]Sandbox, error) {
	if k <= 0 {
		k = 3 // default speculative branching factor
	}

	tm.mu.Lock()
	defer tm.mu.Unlock()

	if err := os.MkdirAll(tm.WorktreeBaseDir, 0755); err != nil {
		return nil, fmt.Errorf("tournament: failed to create worktree base directory: %w", err)
	}

	isGit := isGitRepo(tm.RepoDir)
	var sandboxes []Sandbox
	tournamentID := fmt.Sprintf("%d_%08x", time.Now().UnixNano(), randUint32())

	for i := 0; i < k; i++ {
		var profile CandidateProfile
		if i < len(tm.Profiles) {
			profile = tm.Profiles[i]
		} else {
			profile = CandidateProfile{
				Name:        fmt.Sprintf("candidate-%d", i+1),
				Temperature: 0.5,
				Strategy:    "speculative alternative",
			}
		}

		sbID := fmt.Sprintf("%s_%s", profile.Name, tournamentID)
		worktreePath := filepath.Join(tm.WorktreeBaseDir, sbID)
		branchName := fmt.Sprintf("omnicoder-%s", sbID)

		sb := Sandbox{
			ID:          sbID,
			Name:        profile.Name,
			Path:        worktreePath,
			Branch:      branchName,
			RepoDir:     tm.RepoDir,
			Temperature: profile.Temperature,
			Strategy:    profile.Strategy,
			CreatedAt:   time.Now(),
		}

		if isGit {
			if err := addWorktree(tm.RepoDir, worktreePath, branchName); err == nil {
				sb.IsWorktree = true
				sb.CleanupFn = func() error {
					return removeWorktree(tm.RepoDir, worktreePath, branchName)
				}
			} else {
				// Git worktree failed (e.g. empty repository with no commits), fallback to copy
				if copyErr := copyDirShallow(tm.RepoDir, worktreePath); copyErr != nil {
					tm.teardownSlice(sandboxes)
					return nil, fmt.Errorf("tournament: worktree and fallback copy failed for %s: %w", profile.Name, copyErr)
				}
				sb.IsWorktree = false
				sb.CleanupFn = func() error {
					return os.RemoveAll(worktreePath)
				}
			}
		} else {
			// Fast directory clone fallback
			if err := copyDirShallow(tm.RepoDir, worktreePath); err != nil {
				tm.teardownSlice(sandboxes)
				return nil, fmt.Errorf("tournament: failed creating directory sandbox for %s: %w", profile.Name, err)
			}
			sb.IsWorktree = false
			sb.CleanupFn = func() error {
				return os.RemoveAll(worktreePath)
			}
		}

		sandboxes = append(sandboxes, sb)
		tm.sandboxes[sb.ID] = &sandboxes[len(sandboxes)-1]
	}

	return sandboxes, nil
}

// EvaluateCandidate assesses a candidate sandbox against the target reproducer and test oracle.
func (tm *TournamentManager) EvaluateCandidate(sb Sandbox, reproducer string) CandidateScore {
	start := time.Now()
	score := CandidateScore{
		Sandbox:     sb,
		EvaluatedAt: start,
	}

	if _, err := os.Stat(sb.Path); err != nil {
		score.Eliminated = true
		score.EliminationReason = fmt.Sprintf("sandbox workspace missing: %s", sb.Path)
		score.Score = decimal.NewFromInt(-1000)
		score.ScoreFloat = -1000.0
		return score
	}

	reproducer = strings.TrimSpace(reproducer)
	if reproducer == "" {
		score.Eliminated = true
		score.EliminationReason = ErrReproducerMissing.Error()
		score.Score = decimal.NewFromInt(-1000)
		score.ScoreFloat = -1000.0
		return score
	}

	// 1. Resolve or write reproducer script
	scriptPath := resolveOrWriteReproducer(sb.Path, reproducer)
	if scriptPath == "" {
		score.Eliminated = true
		score.EliminationReason = "failed to resolve reproducer script path"
		score.Score = decimal.NewFromInt(-1000)
		score.ScoreFloat = -1000.0
		return score
	}

	// 2. Execute reproducer under strict execution budget (<2.5s)
	timeout := tm.Timeout
	if timeout <= 0 {
		timeout = devops.DefaultReproducerTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	runRes, runErr := devops.RunReproducerScript(ctx, scriptPath, timeout)
	score.ReproducerDuration = time.Since(start)

	if runErr != nil || runRes == nil {
		score.ReproducerPassed = false
		if runRes != nil {
			score.ReproducerOutput = runRes.Stdout + "\n" + runRes.Stderr
		}
		score.Eliminated = true
		score.EliminationReason = fmt.Sprintf("reproducer execution error: %v", runErr)
	} else {
		score.ReproducerOutput = runRes.Stdout + "\n" + runRes.Stderr
		// Protocol: exit code 0 indicates bug is resolved!
		score.ReproducerPassed = (runRes.ExitCode == 0 && !runRes.TimedOut)
		if !score.ReproducerPassed {
			score.Eliminated = true
			if runRes.TimedOut {
				score.EliminationReason = "reproducer script timed out"
			} else {
				score.EliminationReason = fmt.Sprintf("reproducer failed with exit code %d", runRes.ExitCode)
			}
		}
	}

	// 3. Optional targeted regression test suite
	regPassed := 0
	regTotal := 0
	if tm.RegressionCommand != "" && score.ReproducerPassed {
		p, tot := tm.runRegressionCommand(sb.Path, tm.RegressionCommand)
		regPassed = p
		regTotal = tot
	}
	score.RegressionPassed = regPassed
	score.RegressionTotal = regTotal
	if regTotal > 0 {
		score.RegressionRatio = decimal.NewFromInt(int64(regPassed)).Div(decimal.NewFromInt(int64(regTotal)))
	} else {
		score.RegressionRatio = decimal.NewFromInt(1)
	}

	// 4. Lines changed metric
	linesChanged, _ := sb.CountLinesChanged()
	score.LinesChanged = linesChanged

	// 5. Linters check
	linterErrs, _ := sb.CheckLinters()
	score.LinterErrors = linterErrs
	score.LinterErrorsCount = len(linterErrs)

	// 6. Compute objective score
	calcScore, eliminated, elimReason := CalculateCandidateScore(
		score.ReproducerPassed,
		score.RegressionPassed,
		score.RegressionTotal,
		score.LinesChanged,
		score.LinterErrorsCount,
	)
	score.Score = calcScore
	score.ScoreFloat, _ = calcScore.Float64()
	if eliminated && !score.Eliminated {
		score.Eliminated = true
		score.EliminationReason = elimReason
	}

	return score
}

// SelectWinner determines the Pareto-optimal winning candidate from candidate scores.
// If all candidates are eliminated, an error is returned.
func (tm *TournamentManager) SelectWinner(scores []CandidateScore) (Sandbox, error) {
	if len(scores) == 0 {
		return Sandbox{}, ErrEmptyScores
	}

	var viable []CandidateScore
	for _, sc := range scores {
		if !sc.Eliminated && sc.ReproducerPassed {
			viable = append(viable, sc)
		}
	}

	if len(viable) == 0 {
		return Sandbox{}, fmt.Errorf("%w: 0 of %d candidates passed reproducer", ErrNoViableCandidates, len(scores))
	}

	// Select candidate with the highest objective score.
	// Tie-breaking:
	// 1. Higher score
	// 2. Fewer lines changed (minimal patch discipline)
	// 3. Shorter reproducer duration
	// 4. Lower sampling temperature (Candidate Alpha preferred)
	best := viable[0]
	for i := 1; i < len(viable); i++ {
		cur := viable[i]
		if cur.Score.GreaterThan(best.Score) {
			best = cur
		} else if cur.Score.Equal(best.Score) {
			if cur.LinesChanged < best.LinesChanged {
				best = cur
			} else if cur.LinesChanged == best.LinesChanged {
				if cur.ReproducerDuration < best.ReproducerDuration {
					best = cur
				} else if cur.Sandbox.Temperature < best.Sandbox.Temperature {
					best = cur
				}
			}
		}
	}

	return best.Sandbox, nil
}

// TeardownAll forcefully cleans up all active candidate sandboxes.
func (tm *TournamentManager) TeardownAll() error {
	tm.mu.Lock()
	defer tm.mu.Unlock()

	var errs []string
	for _, sb := range tm.sandboxes {
		if err := sb.Teardown(); err != nil {
			errs = append(errs, err.Error())
		}
	}
	tm.sandboxes = make(map[string]*Sandbox)
	if len(errs) > 0 {
		return fmt.Errorf("tournament teardown errors: %s", strings.Join(errs, "; "))
	}
	return nil
}

func (tm *TournamentManager) teardownSlice(sandboxes []Sandbox) {
	for i := range sandboxes {
		_ = sandboxes[i].Teardown()
	}
}

func (tm *TournamentManager) runRegressionCommand(workDir, cmdStr string) (passed, total int) {
	cmd := exec.Command("cmd.exe", "/c", cmdStr)
	if _, err := exec.LookPath("bash"); err == nil && !strings.Contains(cmdStr, ".exe") {
		cmd = exec.Command("sh", "-c", cmdStr)
	}
	cmd.Dir = workDir
	err := cmd.Run()
	if err == nil {
		return 1, 1
	}
	return 0, 1
}

// --- Default Global Tournament Manager Integration ---

var (
	defaultTournamentManager   *TournamentManager
	defaultTournamentManagerMu sync.RWMutex
)

func init() {
	defaultTournamentManager = NewTournamentManager(".")
}

// SetDefaultTournamentRepo configures the repository root for package-level tournament calls.
func SetDefaultTournamentRepo(repoDir string) {
	defaultTournamentManagerMu.Lock()
	defer defaultTournamentManagerMu.Unlock()
	defaultTournamentManager = NewTournamentManager(repoDir)
}

// GetDefaultTournamentManager returns the active global TournamentManager.
func GetDefaultTournamentManager() *TournamentManager {
	defaultTournamentManagerMu.RLock()
	defer defaultTournamentManagerMu.RUnlock()
	return defaultTournamentManager
}

// SpawnCandidateSandboxes spawns k isolated candidate sandboxes using the default repository.
func SpawnCandidateSandboxes(k int) ([]Sandbox, error) {
	return GetDefaultTournamentManager().SpawnCandidateSandboxes(k)
}

// EvaluateCandidate evaluates a single candidate sandbox against a reproducer script.
func EvaluateCandidate(sb Sandbox, reproducer string) CandidateScore {
	return GetDefaultTournamentManager().EvaluateCandidate(sb, reproducer)
}

// SelectWinner determines the optimal candidate from candidate evaluation scores.
func SelectWinner(scores []CandidateScore) (Sandbox, error) {
	return GetDefaultTournamentManager().SelectWinner(scores)
}

// TeardownSandboxes cleans up a slice of candidate sandboxes.
func TeardownSandboxes(sandboxes []Sandbox) error {
	var errs []string
	for i := range sandboxes {
		if err := sandboxes[i].Teardown(); err != nil {
			errs = append(errs, err.Error())
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("teardown errors: %s", strings.Join(errs, "; "))
	}
	return nil
}

// --- Helper Functions for Git Worktree & Filesystem Operations ---

func isGitRepo(dir string) bool {
	if dir == "" {
		return false
	}
	cmd := exec.Command("git", "-C", dir, "rev-parse", "--is-inside-work-tree")
	out, err := cmd.Output()
	return err == nil && strings.TrimSpace(string(out)) == "true"
}

func addWorktree(repoDir, worktreePath, branchName string) error {
	// Verify HEAD exists
	headCmd := exec.Command("git", "-C", repoDir, "rev-parse", "--verify", "HEAD")
	if err := headCmd.Run(); err != nil {
		return fmt.Errorf("git repository has no HEAD commit: %w", err)
	}

	cleanPath := filepath.Clean(worktreePath)
	cmd := exec.Command("git", "-C", repoDir, "worktree", "add", "-b", branchName, cleanPath, "HEAD")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git worktree add failed: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

func removeWorktree(repoDir, worktreePath, branchName string) error {
	cleanPath := filepath.Clean(worktreePath)

	// 1. git worktree remove --force <path>
	cmd := exec.Command("git", "-C", repoDir, "worktree", "remove", "--force", cleanPath)
	_ = cmd.Run()

	// 2. git branch -D <branchName>
	if branchName != "" {
		branchCmd := exec.Command("git", "-C", repoDir, "branch", "-D", branchName)
		_ = branchCmd.Run()
	}

	// 3. Ensure path is cleaned up if git left residual directories
	_ = os.RemoveAll(cleanPath)

	// 4. Prune worktree metadata
	pruneCmd := exec.Command("git", "-C", repoDir, "worktree", "prune")
	_ = pruneCmd.Run()

	return nil
}

func resolveOrWriteReproducer(sandboxPath, reproducer string) string {
	// Check if reproducer is an existing file path
	if filepath.IsAbs(reproducer) {
		if _, err := os.Stat(reproducer); err == nil {
			return reproducer
		}
	}
	relPath := filepath.Join(sandboxPath, reproducer)
	if _, err := os.Stat(relPath); err == nil {
		return relPath
	}

	// If reproducer contains script source code, write it into the sandbox
	if strings.Contains(reproducer, "\n") || strings.HasPrefix(reproducer, "#!") || strings.Contains(reproducer, "def ") || strings.Contains(reproducer, "package ") {
		filename := devops.DefaultPythonScript
		if strings.Contains(reproducer, "package ") || strings.Contains(reproducer, "func Test") {
			filename = devops.DefaultGoScript
		}
		target := filepath.Join(sandboxPath, filename)
		if err := os.WriteFile(target, []byte(reproducer), 0755); err == nil {
			return target
		}
	}

	return ""
}

func copyDirShallow(src, dst string) error {
	if err := os.MkdirAll(dst, 0755); err != nil {
		return err
	}

	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}

	for _, entry := range entries {
		name := entry.Name()
		if name == ".git" {
			continue // skip copying internal git metadata
		}
		srcPath := filepath.Join(src, name)
		dstPath := filepath.Join(dst, name)

		if entry.IsDir() {
			if err := copyDirShallow(srcPath, dstPath); err != nil {
				return err
			}
		} else {
			if err := copyFile(srcPath, dstPath); err != nil {
				return err
			}
		}
	}
	return nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		return err
	}

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()

	if _, err = io.Copy(out, in); err != nil {
		return err
	}
	return out.Sync()
}
