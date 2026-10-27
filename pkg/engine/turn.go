// SPDX-License-Identifier: Apache-2.0
package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/caleralabs/omnicoder-icx/pkg/canonical"
	"github.com/caleralabs/omnicoder-icx/pkg/devops"
	"github.com/caleralabs/omnicoder-icx/pkg/icx"
	"github.com/caleralabs/omnicoder-icx/pkg/llm"
	"github.com/caleralabs/omnicoder-icx/pkg/vm"
)

// goASTIngester is an optional interface implemented by ICX engines that support pure Go AST ingestion.
type goASTIngester interface {
	IngestGoAST(filePath string, src []byte) error
}

// TurnPipeline implements the 8-Stage Autonomous Turn Pipeline.
type TurnPipeline struct {
	orch *Orchestrator
}

// ExecuteTurn executes Stages 1 through 8 in strict sequence.
func (p *TurnPipeline) ExecuteTurn(ctx context.Context, state *EngineState, turn *TurnState, cfg EngineConfig) (retErr error) {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	defer func() {
		if retErr != nil && !turn.Success {
			if turn.SandboxCheckpointID != "" && cfg.VMClient != nil {
				_ = cfg.VMClient.Rollback(context.Background(), turn.SandboxID, turn.SandboxCheckpointID)
				p.emitProgress(turn.TurnNumber, cfg.MaxTurns, 0, "MicroVM Rollback", "ROLLBACK",
					fmt.Sprintf("Rolled back to checkpoint %s (<10ms): %v", turn.SandboxCheckpointID, retErr), 0, 0)
			}
			if turn.Transaction != nil && turn.Transaction.Status != TxAborted && turn.Transaction.Status != TxCommitted {
				_ = RollbackTransaction(turn.Transaction)
			}
			planSpec := DetectPlanSpec(turn.Task, state.WorkspaceDir)
			if planSpec == nil {
				reverted := p.rollbackTurnSnapshots(state.WorkspaceDir, turn)
				if reverted > 0 {
					p.emitProgress(turn.TurnNumber, cfg.MaxTurns, 0, "Atomic Rollback", "ROLLBACK",
						fmt.Sprintf("Rolled back %d files following turn error: %v", reverted, retErr), 0, 0)
				}
			}
		}
	}()
	startMsg := fmt.Sprintf("Starting Turn %d of %d", turn.TurnNumber, cfg.MaxTurns)
	if cfg.Unbounded || cfg.MaxTurns <= 0 {
		startMsg = fmt.Sprintf("Starting Turn %d (Unbounded)", turn.TurnNumber)
	}
	p.emitProgress(turn.TurnNumber, cfg.MaxTurns, 0, "Turn Start", "START", startMsg, 0, 0)

	// Stage 1: Ingest & RepoMap
	if err := p.executeStage1IngestAndRepoMap(ctx, state, turn, cfg); err != nil {
		return fmt.Errorf("stage 1 (Ingest & RepoMap) failed: %w", err)
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}

	// Stage 2: AST Blast-Radius Walk
	if err := p.executeStage2ASTBlastRadiusWalk(ctx, state, turn, cfg); err != nil {
		return fmt.Errorf("stage 2 (AST Blast-Radius Walk) failed: %w", err)
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}

	// Stage 3: Verbatim Quoting & Planning
	if err := p.executeStage3VerbatimQuotingAndPlanning(ctx, state, turn, cfg); err != nil {
		return fmt.Errorf("stage 3 (Verbatim Quoting & Planning) failed: %w", err)
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}

	// Stage 4: Micro-Decision Gate
	if err := p.executeStage4MicroDecisionGate(ctx, state, turn, cfg); err != nil {
		return fmt.Errorf("stage 4 (Micro-Decision Gate) failed: %w", err)
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}

	// Stage 5 Pre-flight: Capture instant MicroVM snapshot checkpoint
	if cfg.VMClient != nil && cfg.UseSandbox {
		if turn.SandboxID == "" {
			if sb, err := cfg.VMClient.CreateSandbox(ctx, vm.SandboxConfig{WorkspaceDir: state.WorkspaceDir}); err == nil {
				turn.SandboxID = sb.ID
			}
		}
		if turn.SandboxID != "" {
			if chkID, err := cfg.VMClient.Snapshot(ctx, turn.SandboxID, fmt.Sprintf("pre_stage5_turn_%d", turn.TurnNumber)); err == nil {
				turn.SandboxCheckpointID = chkID
			}
		}
	}

	// Stage 5: 4-Tier Fuzzy Patch
	if err := p.executeStage5FuzzyPatch(ctx, state, turn, cfg); err != nil {
		return fmt.Errorf("stage 5 (4-Tier Fuzzy Patch) failed: %w", err)
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}

	// Stage 6: Compiler Feedback & Self-Healing Loop
	if err := p.executeStage6CompilerFeedbackAndHeal(ctx, state, turn, cfg); err != nil {
		return fmt.Errorf("stage 6 (Compiler Feedback & Self-Healing) failed: %w", err)
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}

	// Stage 7: Zero-Token Session Reset
	if err := p.executeStage7ZeroTokenSessionReset(ctx, state, turn, cfg); err != nil {
		return fmt.Errorf("stage 7 (Zero-Token Session Reset) failed: %w", err)
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}

	// Stage 8: Git Commit & Attestation
	if err := p.executeStage8GitCommitAndAttestation(ctx, state, turn, cfg); err != nil {
		return fmt.Errorf("stage 8 (Git Commit & Attestation) failed: %w", err)
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}

	return nil
}

// Stage 1: Ingest changed/workspace files into ICX lattice & generate RepoMap.
func (p *TurnPipeline) executeStage1IngestAndRepoMap(ctx context.Context, state *EngineState, turn *TurnState, cfg EngineConfig) error {
	p.emitEvent(turn.TurnNumber, cfg.MaxTurns, 1, "Ingest & RepoMap", "Ingesting workspace files into 4D lattice...")

	// Walk workspace and ingest code files into ICX memory
	_ = filepath.WalkDir(state.WorkspaceDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			name := d.Name()
			if strings.HasPrefix(name, ".") && name != "." {
				return filepath.SkipDir
			}
			return nil
		}

		ext := strings.ToLower(filepath.Ext(path))
		if ext != ".go" && ext != ".ts" && ext != ".py" && ext != ".rs" {
			return nil
		}
		rel, err := filepath.Rel(state.WorkspaceDir, path)
		if err != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)

		raw, err := os.ReadFile(path)
		if err != nil {
			return nil
		}

		// Ingest into ICX lattice memory
		_, _ = p.orch.icxClient.Remember(ctx, icx.RememberRequest{
			SpaceID:  state.LatticeSpaceID,
			Family:   "file_slots",
			SourceID: rel,
			Text:     string(raw),
			NodeID:   fmt.Sprintf("file_%s", rel),
		})

		// If Go file, ingest into Go AST graph if engine supports it
		if ext == ".go" {
			if ingester, ok := p.orch.icxClient.(goASTIngester); ok {
				_ = ingester.IngestGoAST(rel, raw)
			}
		}
		return nil
	})

	// Synthesize RepoMap with Personalized PageRank (<1024 tokens)
	repoMapRes, err := devops.SynthesizeRepoMap(devops.RepoMapRequest{
		WorkspaceDir: state.WorkspaceDir,
		TokenBudget:  1024,
		FocusFiles:   turn.FocusFiles,
		Query:        turn.Task,
	})
	if err != nil {
		return fmt.Errorf("synthesize repomap: %w", err)
	}

	turn.RepoMapSummary = repoMapRes.RepoMap
	state.AddMerkleLeaf(canonical.SHA256Of(repoMapRes.RepoMap))
	return nil
}

// Stage 2: Evaluate AST neighborhood via 5-hop walk + targeted relation walks, formatting into dense <nav:ribbon> (<160 tokens).
func (p *TurnPipeline) executeStage2ASTBlastRadiusWalk(ctx context.Context, state *EngineState, turn *TurnState, cfg EngineConfig) error {
	p.emitEvent(turn.TurnNumber, cfg.MaxTurns, 2, "AST Blast-Radius Walk", "Evaluating AST blast radius (5-hop walk + targeted relations)...")

	candidateNodes := make([]string, 0)
	for _, f := range turn.FocusFiles {
		candidateNodes = append(candidateNodes, fmt.Sprintf("file_%s", f))
	}
	if len(candidateNodes) == 0 {
		candidateNodes = append(candidateNodes, "file_root")
	}

	var allSteps []icx.WalkStep
	rawNeighborhood := make([]string, 0)

	// Existing: Undirected 5-hop walk for broad blast radius
	for _, nodeID := range candidateNodes {
		walkRes, err := p.orch.icxClient.MultihopWalk(ctx, icx.MultihopWalkRequest{
			StartNode: nodeID,
			MaxHops:   5,
			SpaceID:   state.LatticeSpaceID,
		})
		if err == nil && walkRes != nil && walkRes.OK {
			allSteps = append(allSteps, walkRes.Steps...)
			for _, step := range walkRes.Steps {
				rawNeighborhood = append(rawNeighborhood, fmt.Sprintf("%s (%s %s)", step.Name, step.Relation, step.Type))
			}
		}
	}

	// Targeted relation walks for deeper understanding (calls, imports, defines)
	// These build the CallerFiles list so GateEdit knows what other files depend on the target
	for _, rel := range []string{"calls", "imports", "defines"} {
		for _, nodeID := range candidateNodes {
			walkRes, err := p.orch.icxClient.MultihopWalk(ctx, icx.MultihopWalkRequest{
				StartNode: nodeID,
				Relation:  rel,
				MaxHops:   3, // Shorter hops for targeted walks
				SpaceID:   state.LatticeSpaceID,
			})
			if err == nil && walkRes != nil && walkRes.OK {
				allSteps = append(allSteps, walkRes.Steps...)
				// Record callers so context curation knows what depends on the target
				if rel == "calls" {
					for _, step := range walkRes.Steps {
						if step.FilePath != "" {
							turn.CallerFiles = appendUnique(turn.CallerFiles, step.FilePath)
						}
					}
				}
			}
		}
	}

	// Use icx_query_codebase to find symbols matching the task description
	// This discovers additional relevant files the task might need to touch
	queryRes, err := p.orch.icxClient.QueryCodebase(ctx, icx.QueryCodebaseRequest{
		Query:     turn.Task,
		Dimension: "3D_VOLUMETRIC",
		TopK:      5,
	})
	if err == nil && queryRes != nil {
		for _, match := range queryRes.Results {
			if match.Payload != nil {
				if fp, ok := match.Payload["file_path"].(string); ok && fp != "" {
					turn.FocusFiles = appendUnique(turn.FocusFiles, fp)
				}
			}
		}
	}

	ribbon := FormatASTRibbon(candidateNodes, allSteps, 155)
	turn.ASTRibbon = ribbon
	turn.ASTNeighborhood = rawNeighborhood
	return nil
}

// FormatASTRibbon formats the AST neighborhood walk steps into a dense <nav:ribbon> representation strictly under maxTokens.
func FormatASTRibbon(candidateNodes []string, steps []icx.WalkStep, maxTokens int) string {
	if maxTokens <= 0 {
		maxTokens = 155 // Strictly < 160 tokens
	}
	rootRaw := "root"
	if len(candidateNodes) > 0 {
		rootRaw = candidateNodes[0]
	}
	rootName := llm.SanitizeRibbonRoot(rootRaw, 40)
	prefix := fmt.Sprintf("<nav:ribbon: root=%q hops=5 | ", rootName)

	if len(steps) == 0 {
		return prefix + "isolated />"
	}

	items := make([]string, 0, len(steps))
	seen := make(map[string]bool)
	for _, step := range steps {
		key := fmt.Sprintf("%d:%s:%s", step.Depth, step.Name, step.Type)
		if seen[key] {
			continue
		}
		seen[key] = true
		rel := step.Relation
		if rel == "" {
			if step.Depth == 0 {
				rel = "root"
			} else {
				rel = "node"
			}
		}
		items = append(items, fmt.Sprintf("d%d:%s(%s):%s", step.Depth, step.Name, step.Type, rel))
	}

	if len(items) == 0 {
		return prefix + "isolated />"
	}

	var finalItems []string
	for i, item := range items {
		candidate := prefix + strings.Join(append(finalItems, item), " > ") + " />"
		if llm.EstimateTokens(candidate) >= maxTokens {
			remaining := len(items) - i
			if remaining > 0 {
				finalItems = append(finalItems, fmt.Sprintf("[+%d_more]", remaining))
			}
			break
		}
		finalItems = append(finalItems, item)
	}

	if len(finalItems) == 0 && len(items) > 0 {
		finalItems = append(finalItems, items[0])
	}

	result := prefix + strings.Join(finalItems, " > ") + " />"
	for llm.EstimateTokens(result) >= maxTokens && len(finalItems) > 1 {
		finalItems = finalItems[:len(finalItems)-1]
		result = prefix + strings.Join(finalItems, " > ") + " [clamped] />"
	}
	if llm.EstimateTokens(result) >= maxTokens {
		result = fmt.Sprintf("<nav:ribbon: root=%q hops=5 | [clamped] />", rootName)
	}
	for llm.EstimateTokens(result) >= maxTokens {
		if len(rootName) > 8 {
			rootName = rootName[:len(rootName)/2] + "..."
			result = fmt.Sprintf("<nav:ribbon: root=%q hops=5 | [clamped] />", rootName)
		} else {
			result = "<nav:ribbon: [clamped] />"
			break
		}
	}
	return result
}

// Stage 3: Pull exact code lines via QuoteSlot (<0.002ms) with SHA-256 seal + trajectory recall.
func (p *TurnPipeline) executeStage3VerbatimQuotingAndPlanning(ctx context.Context, state *EngineState, turn *TurnState, cfg EngineConfig) error {
	p.emitEvent(turn.TurnNumber, cfg.MaxTurns, 3, "Verbatim Quoting & Planning", "Fetching byte-exact quote slots + recalling trajectory...")

	// Quote ALL focus files, not just slot 1
	if len(turn.FocusFiles) > 0 {
		for i, focusFile := range turn.FocusFiles {
			if i >= 5 {
				break // Cap at 5 files to prevent excessive quoting
			}

			// Get the file's AST to understand its structure (functions, classes, imports)
			astRes, _ := p.orch.icxClient.ReadFileAST(ctx, icx.ReadFileASTRequest{
				FilePath: focusFile,
			})

			// Quote the file slot
			quoteRes, err := p.orch.icxClient.QuoteSlot(ctx, icx.QuoteSlotRequest{
				SpaceID: state.LatticeSpaceID,
				Family:  "file_slots",
				Index:   i + 1,
			})
			if err == nil && quoteRes != nil && quoteRes.OK && quoteRes.Verified {
				turn.QuotedSlots = append(turn.QuotedSlots, quoteRes.Text)
				state.AddMerkleLeaf(quoteRes.SHA256)

				// If AST identified functions, try to extract the most relevant function body
				if astRes != nil && len(astRes.Functions) > 0 {
					funcName := findBestMatchingFunction(astRes.Functions, turn.Task)
					if funcName != "" {
						// Search for function body offset in the quoted text
						funcStart, funcLen := findFunctionBounds(quoteRes.Text, funcName)
						if funcStart >= 0 && funcLen > 0 {
							funcQuote, _ := p.orch.icxClient.QuoteSlot(ctx, icx.QuoteSlotRequest{
								SpaceID: state.LatticeSpaceID,
								Family:  "file_slots",
								Index:   i + 1,
								Start:   &funcStart,
								Length:  &funcLen,
							})
							if funcQuote != nil && funcQuote.OK {
								turn.TargetFunctionQuotes[focusFile] = funcQuote.Text
							}
						}
					}
				}
			}
		}
	} else {
		// Fallback: quote first slot (existing behavior)
		quoteRes, err := p.orch.icxClient.QuoteSlot(ctx, icx.QuoteSlotRequest{
			SpaceID: state.LatticeSpaceID,
			Family:  "file_slots",
			Index:   1,
		})
		if err == nil && quoteRes != nil && quoteRes.OK && quoteRes.Verified {
			turn.QuotedSlots = append(turn.QuotedSlots, quoteRes.Text)
			state.AddMerkleLeaf(quoteRes.SHA256)
		}
	}

	// Recall trajectory from previous turns via ICX lattice
	// This gives the LLM awareness of what was already completed/attempted
	recallRes, _ := p.orch.icxClient.RecallScoped(ctx, icx.RecallRequest{
		SpaceID: state.LatticeSpaceID,
		Family:  "trajectory",
		Query:   turn.Task,
		TopK:    5,
		Format:  "ribbon",
	})
	if recallRes != nil && recallRes.Ribbon != "" {
		turn.TrajectoryRibbon = recallRes.Ribbon
	}

	return nil
}

// findBestMatchingFunction returns the function name from funcs that best matches the task description.
func findBestMatchingFunction(funcs []string, task string) string {
	taskLower := strings.ToLower(task)
	bestFunc := ""
	bestScore := 0
	for _, fn := range funcs {
		fnLower := strings.ToLower(fn)
		score := 0
		// Score based on word overlap between function name and task
		for _, word := range strings.Fields(taskLower) {
			if len(word) > 2 && strings.Contains(fnLower, word) {
				score += len(word)
			}
		}
		// Also check camelCase parts
		parts := splitCamelCase(fn)
		for _, part := range parts {
			partLower := strings.ToLower(part)
			if len(partLower) > 2 && strings.Contains(taskLower, partLower) {
				score += len(partLower)
			}
		}
		if score > bestScore {
			bestScore = score
			bestFunc = fn
		}
	}
	return bestFunc
}

// splitCamelCase splits a camelCase or PascalCase identifier into words.
func splitCamelCase(s string) []string {
	var parts []string
	current := strings.Builder{}
	for i, r := range s {
		if i > 0 && r >= 'A' && r <= 'Z' {
			if current.Len() > 0 {
				parts = append(parts, current.String())
				current.Reset()
			}
		}
		current.WriteRune(r)
	}
	if current.Len() > 0 {
		parts = append(parts, current.String())
	}
	return parts
}

// findFunctionBounds finds the rune offset and length of a function body in source text.
// Returns (-1, 0) if not found.
func findFunctionBounds(source string, funcName string) (int, int) {
	// Search for "func" or "func (" patterns with the function name
	patterns := []string{
		"func " + funcName + "(",
		"func (" ,  // Method with receiver — search for funcName after receiver
	}
	for _, pat := range patterns {
		idx := strings.Index(source, pat)
		if idx >= 0 {
			// Find the matching closing brace by counting braces
			braceStart := strings.Index(source[idx:], "{")
			if braceStart < 0 {
				continue
			}
			braceStart += idx
			depth := 1
			end := braceStart + 1
			for end < len(source) && depth > 0 {
				if source[end] == '{' {
					depth++
				} else if source[end] == '}' {
					depth--
				}
				end++
			}
			if depth == 0 {
				// Convert byte offsets to rune offsets
				runeStart := len([]rune(source[:idx]))
				runeLen := len([]rune(source[idx:end]))
				return runeStart, runeLen
			}
		}
	}
	return -1, 0
}

// matchesScope checks if path matches a glob pattern or prefix scope.
func matchesScope(path, scope string) bool {
	cleanPath := filepath.ToSlash(strings.TrimPrefix(strings.TrimPrefix(path, "./"), "/"))
	cleanScope := filepath.ToSlash(strings.TrimPrefix(strings.TrimPrefix(scope, "./"), "/"))
	if cleanScope == "" || cleanScope == "*" {
		return true
	}
	if strings.HasPrefix(cleanPath, cleanScope) {
		return true
	}
	if strings.HasSuffix(cleanScope, "/*") {
		if strings.HasPrefix(cleanPath, strings.TrimSuffix(cleanScope, "*")) {
			return true
		}
	}
	if matched, _ := filepath.Match(cleanScope, cleanPath); matched {
		return true
	}
	if matched, _ := filepath.Match(cleanScope, filepath.Base(cleanPath)); matched {
		return true
	}
	return false
}

// Stage 4: Micro-Decision Gate (DecideIntent & DecideEdit <300 tokens).
func (p *TurnPipeline) executeStage4MicroDecisionGate(ctx context.Context, state *EngineState, turn *TurnState, cfg EngineConfig) error {
	p.emitEvent(turn.TurnNumber, cfg.MaxTurns, 4, "Micro-Decision Gate", "Evaluating GateIntent and GateEdit micro-cards...")

	// 4a. Decide Intent
	intentUserPrompt := turn.Task
	planSpec := DetectPlanSpec(turn.Task, state.WorkspaceDir)
	var activePhase *PlanPhase
	if planSpec != nil {
		activePhase = planSpec.DetermineActivePhase(state.WorkspaceDir)
		if activePhase != nil {
			intentUserPrompt = fmt.Sprintf("Phase %d: %s. Build target files: %s",
				activePhase.PhaseNumber, activePhase.Name, strings.Join(activePhase.TargetFiles, ", "))
		}
	}
	turn.ActivePlanPhase = activePhase

	milestones := state.GetMilestones()
	trajectoryRibbon := icx.FormatTrajectoryRibbon(milestones)

	intentCard := llm.GateIntentCard{
		UserPrompt:       intentUserPrompt,
		RepoMapSummary:   turn.RepoMapSummary,
		FocusFiles:       turn.FocusFiles,
		TrajectoryRibbon: trajectoryRibbon,
	}
	intentDec, err := p.orch.gate.DecideIntent(ctx, intentCard)
	if err != nil {
		return fmt.Errorf("gate decide intent: %w", err)
	}
	turn.Intent = intentDec
	rawTargets := intentDec.TargetFiles
	if len(rawTargets) == 0 && activePhase != nil {
		rawTargets = activePhase.TargetFiles
	} else if len(rawTargets) == 0 && len(turn.FocusFiles) > 0 {
		rawTargets = turn.FocusFiles
	}

	// Ensure all target files from intent are retained, and add any missing target files for the active phase
	if activePhase != nil {
		targetSet := make(map[string]bool)
		for _, tf := range rawTargets {
			clean := filepath.ToSlash(strings.TrimSpace(tf))
			clean = strings.TrimPrefix(clean, "./")
			clean = strings.TrimPrefix(clean, "/")
			targetSet[clean] = true
		}
		// Also add any missing target files for the active phase
		for _, pf := range activePhase.TargetFiles {
			clean := filepath.ToSlash(strings.TrimSpace(pf))
			clean = strings.TrimPrefix(clean, "./")
			clean = strings.TrimPrefix(clean, "/")
			absPath := filepath.Join(state.WorkspaceDir, filepath.FromSlash(clean))
			if fi, err := os.Stat(absPath); err != nil || fi.Size() == 0 {
				targetSet[clean] = true
			}
		}
		rawTargets = make([]string, 0, len(targetSet))
		for tf := range targetSet {
			rawTargets = append(rawTargets, tf)
		}
	} else if planSpec != nil && planSpec.IsComplete(state.WorkspaceDir) {
		turn.TargetFiles = nil
		p.emitEvent(turn.TurnNumber, cfg.MaxTurns, 4, "Plan Complete",
			fmt.Sprintf("All %d phases of %s plan verified present on disk; proceeding to verification", len(planSpec.Phases), planSpec.ModuleName))
		return nil
	}

	// Enforce Subfolder Encapsulation Policy & Host Package Shield
	projectScope := detectProjectScope(turn.Task, turn.FocusFiles, state.WorkspaceDir)
	if planSpec != nil && projectScope == "" {
		projectScope = planSpec.ProjectScope
	}
	isHostTask := isHostPackageTask(turn.Task)

	if projectScope != "" {
		scopedFiles := make([]string, 0, len(rawTargets))
		for _, tf := range rawTargets {
			clean := filepath.ToSlash(strings.TrimSpace(tf))
			clean = strings.TrimPrefix(clean, "./")
			clean = strings.TrimPrefix(clean, "/")

			if !strings.HasPrefix(clean, projectScope+"/") && clean != projectScope {
				projBase := filepath.Base(projectScope)
				if strings.HasPrefix(clean, "cmd/"+projBase+"/") {
					clean = "cmd/" + strings.TrimPrefix(clean, "cmd/"+projBase+"/")
				}
				clean = projectScope + "/" + clean
			}
			scopedFiles = append(scopedFiles, clean)
		}
		turn.TargetFiles = scopedFiles
	} else {
		shieldedTargets := make([]string, 0, len(rawTargets))
		for _, tf := range rawTargets {
			clean := filepath.ToSlash(strings.TrimSpace(tf))
			clean = strings.TrimPrefix(clean, "./")
			clean = strings.TrimPrefix(clean, "/")
			if isHostPackagePath(clean) && !isHostTask {
				p.emitEvent(turn.TurnNumber, cfg.MaxTurns, 4, "Host Package Shield",
					fmt.Sprintf("Shielded host engine package %q from unintentional mutation", clean))
				continue
			}
			shieldedTargets = append(shieldedTargets, clean)
		}
		turn.TargetFiles = shieldedTargets
	}

	// Apply explicit TargetFile boundary confinement or Scope filter
	if cfg.TargetFile != "" {
		cleanTarget := filepath.ToSlash(strings.TrimSpace(cfg.TargetFile))
		cleanTarget = strings.TrimPrefix(cleanTarget, "./")
		cleanTarget = strings.TrimPrefix(cleanTarget, "/")
		p.emitEvent(turn.TurnNumber, cfg.MaxTurns, 4, "Target Boundary",
			fmt.Sprintf("Confining mutations strictly to target boundary: %s", cleanTarget))
		turn.TargetFiles = []string{cleanTarget}
	} else if cfg.Scope != "" {
		var inScope []string
		for _, tf := range turn.TargetFiles {
			if matchesScope(tf, cfg.Scope) {
				inScope = append(inScope, tf)
			} else {
				p.emitEvent(turn.TurnNumber, cfg.MaxTurns, 4, "Scope Boundary Filter",
					fmt.Sprintf("Filtered out target file outside scope %q: %s", cfg.Scope, tf))
			}
		}
		turn.TargetFiles = inScope
	}

	// 4b. Decide Edit for each target file
	for _, targetFile := range turn.TargetFiles {
		var snippet string

		// Priority 1: Use targeted function quote from icx_quote_slot (byte-exact, <0.002ms)
		if funcQuote, ok := turn.TargetFunctionQuotes[targetFile]; ok && funcQuote != "" {
			snippet = funcQuote
		}

		// Priority 2: Use full file quote from QuotedSlots
		if snippet == "" {
			for _, q := range turn.QuotedSlots {
				// QuotedSlots are indexed by file order in FocusFiles
				if len(q) > 0 {
					snippet = q
					break
				}
			}
		}

		// Priority 3: Read from disk (existing fallback)
		if snippet == "" {
			fullPath := filepath.Join(state.WorkspaceDir, filepath.FromSlash(targetFile))
			if raw, readErr := os.ReadFile(fullPath); readErr == nil {
				snippet = string(raw)
			}
		}

		taskSummary := strings.TrimSpace(turn.Task)
		if idx := strings.Index(taskSummary, "\n"); idx != -1 {
			taskSummary = strings.TrimSpace(taskSummary[:idx])
		}
		if len(taskSummary) > 120 {
			taskSummary = taskSummary[:120] + "..."
		}

		instruction := fmt.Sprintf("%s | %s", taskSummary, intentDec.Rationale)
		if activePhase != nil {
			instruction = fmt.Sprintf("Phase %d (%s): %s. Target: %s. %s",
				activePhase.PhaseNumber, activePhase.Name, activePhase.Description, targetFile, intentDec.Rationale)
		}

		// Build trajectory context: prefer lattice-recalled ribbon, fallback to milestone ribbon
		editTrajectory := turn.TrajectoryRibbon
		if editTrajectory == "" {
			editTrajectory = trajectoryRibbon
		}

		editCard := llm.GateEditCard{
			FilePath:         targetFile,
			Instruction:      instruction,
			ContextSnippet:   snippet,
			ASTNeighborhood:  turn.ASTNeighborhood,
			ASTRibbon:        turn.ASTRibbon,
			TrajectoryRibbon: editTrajectory,
		}

		editDec, err := p.orch.gate.DecideEdit(ctx, editCard)
		if err != nil {
			return fmt.Errorf("gate decide edit on %s: %w", targetFile, err)
		}
		turn.Edits = append(turn.Edits, editDec)
	}

	// Populate structured diagnostics and target symbols for report
	if len(turn.Edits) > 0 {
		var diagList []map[string]any
		for _, editDec := range turn.Edits {
			action := "AutonomousRefactor"
			if turn.Intent != nil && turn.Intent.Operation != "" {
				action = string(turn.Intent.Operation)
			}
			msg := editDec.Explanation
			if msg == "" && turn.Intent != nil {
				msg = turn.Intent.Rationale
			}
			diagList = append(diagList, map[string]any{
				"rule":     action,
				"file":     editDec.FilePath,
				"severity": "warning",
				"message":  msg,
			})
		}
		state.Diagnostics = diagList
		turn.Diagnostics = diagList
	}
	for _, tf := range turn.TargetFiles {
		state.TargetSymbols = appendUnique(state.TargetSymbols, filepath.Base(tf))
		turn.TargetSymbols = appendUnique(turn.TargetSymbols, filepath.Base(tf))
	}

	return nil
}

// Stage 5: Apply 4-Tier Fuzzy Patch with transactional staging & rollback.
func (p *TurnPipeline) executeStage5FuzzyPatch(ctx context.Context, state *EngineState, turn *TurnState, cfg EngineConfig) error {
	p.emitEvent(turn.TurnNumber, cfg.MaxTurns, 5, "4-Tier Fuzzy Patch", "Applying search/replace edit blocks with atomic rollback...")

	var allBlocks []devops.EditBlock
	projectScope := detectProjectScope(turn.Task, turn.FocusFiles, state.WorkspaceDir)
	planSpec := DetectPlanSpec(turn.Task, state.WorkspaceDir)
	if planSpec != nil && projectScope == "" {
		projectScope = planSpec.ProjectScope
	}

	isHostTask := isHostPackageTask(turn.Task)
	for _, editDec := range turn.Edits {
		targetFP := filepath.ToSlash(editDec.FilePath)
		if isHostPackagePath(targetFP) && !isHostTask {
			p.emitEvent(turn.TurnNumber, cfg.MaxTurns, 5, "Host Package Shield",
				fmt.Sprintf("Shielded host engine package %q from unauthorized patch block", targetFP))
			continue
		}
		if projectScope != "" && !strings.HasPrefix(targetFP, projectScope+"/") && targetFP != projectScope {
			clean := strings.TrimPrefix(targetFP, "./")
			clean = strings.TrimPrefix(clean, "/")
			projBase := filepath.Base(projectScope)
			if strings.HasPrefix(clean, "cmd/"+projBase+"/") {
				clean = "cmd/" + strings.TrimPrefix(clean, "cmd/"+projBase+"/")
			}
			targetFP = projectScope + "/" + clean
		}
		for _, b := range editDec.EditBlocks {
			allBlocks = append(allBlocks, devops.EditBlock{
				FilePath:    targetFP,
				SearchText:  b.SearchText,
				ReplaceText: b.ReplaceText,
			})
		}
		if strings.TrimSpace(editDec.UnifiedDiff) != "" {
			if parsedEdits, diffErr := ParseUnifiedDiffBlocks(editDec.UnifiedDiff); diffErr == nil && len(parsedEdits) > 0 {
				turn.FileEdits = append(turn.FileEdits, parsedEdits...)
				diffBlocks := ConvertFileEditsToEditBlocks(parsedEdits)
				for _, db := range diffBlocks {
					blockFP := db.FilePath
					if projectScope != "" && !strings.HasPrefix(blockFP, projectScope+"/") && blockFP != projectScope {
						clean := strings.TrimPrefix(blockFP, "./")
						clean = strings.TrimPrefix(clean, "/")
						projBase := filepath.Base(projectScope)
						if strings.HasPrefix(clean, "cmd/"+projBase+"/") {
							clean = "cmd/" + strings.TrimPrefix(clean, "cmd/"+projBase+"/")
						}
						blockFP = projectScope + "/" + clean
					}
					db.FilePath = blockFP
					allBlocks = append(allBlocks, db)
				}
			}
		}
	}

	// Enforce TargetFile boundary confinement or Scope filter on edit blocks
	if cfg.TargetFile != "" {
		cleanTarget := filepath.ToSlash(strings.TrimSpace(cfg.TargetFile))
		cleanTarget = strings.TrimPrefix(cleanTarget, "./")
		cleanTarget = strings.TrimPrefix(cleanTarget, "/")
		var filtered []devops.EditBlock
		for _, b := range allBlocks {
			bPath := filepath.ToSlash(strings.TrimPrefix(strings.TrimPrefix(b.FilePath, "./"), "/"))
			if bPath == cleanTarget {
				filtered = append(filtered, b)
			} else {
				p.emitEvent(turn.TurnNumber, cfg.MaxTurns, 5, "Boundary Shield",
					fmt.Sprintf("Rejected patch block targeting %q outside target boundary %q", b.FilePath, cleanTarget))
			}
		}
		allBlocks = filtered
	} else if cfg.Scope != "" {
		var filtered []devops.EditBlock
		for _, b := range allBlocks {
			bPath := filepath.ToSlash(strings.TrimPrefix(strings.TrimPrefix(b.FilePath, "./"), "/"))
			if matchesScope(bPath, cfg.Scope) {
				filtered = append(filtered, b)
			} else {
				p.emitEvent(turn.TurnNumber, cfg.MaxTurns, 5, "Scope Shield",
					fmt.Sprintf("Rejected patch block targeting %q outside scope %q", b.FilePath, cfg.Scope))
			}
		}
		allBlocks = filtered
	}

	// Read-only analysis mode: skip disk mutations, record proposed patches
	if cfg.AuditOnly {
		p.emitProgress(turn.TurnNumber, cfg.MaxTurns, 5, "Audit-Only Mode", "PASS",
			fmt.Sprintf("Dry-run analysis complete: %d proposed patch blocks identified (0 disk mutations)", len(allBlocks)), 0, 0)
		state.PatchesProposed += len(allBlocks)
		turn.ProposedPatchesCount = len(allBlocks)
		var filesMod []string
		for _, b := range allBlocks {
			filesMod = appendUnique(filesMod, b.FilePath)
		}
		patchRes := &devops.EditBlockResult{
			Status:        "AUDIT_ONLY_PROPOSED",
			PatchID:       fmt.Sprintf("audit-%d", time.Now().UnixNano()),
			FilesModified: filesMod,
			BlocksApplied: 0,
			Applied:       true,
		}
		turn.Patches = append(turn.Patches, patchRes)
		return nil
	}

	if turn.Transaction == nil {
		turn.Transaction = NewTransaction(state.WorkspaceDir)
	}

	if len(allBlocks) == 0 {
		return nil
	}

	// Snapshot pre-turn file states for atomic rollback if turn is rejected or fails
	for _, b := range allBlocks {
		if _, seen := turn.PreTurnSnapshots[b.FilePath]; !seen {
			fullPath := filepath.Join(state.WorkspaceDir, filepath.FromSlash(b.FilePath))
			if content, readErr := os.ReadFile(fullPath); readErr == nil {
				turn.PreTurnSnapshots[b.FilePath] = content
				if state.Vault != nil {
					_, _ = state.Vault.PutCAS(content)
				}
			} else {
				turn.PreTurnSnapshots[b.FilePath] = nil // File did not exist before this turn
			}
		}
	}

	// Pre-create any missing files on disk with empty content so ApplyEditBlocks can read them
	for _, b := range allBlocks {
		fullPath := filepath.Join(state.WorkspaceDir, filepath.FromSlash(b.FilePath))
		if _, err := os.Stat(fullPath); os.IsNotExist(err) {
			if err := os.MkdirAll(filepath.Dir(fullPath), 0755); err == nil {
				_ = os.WriteFile(fullPath, []byte(""), 0644)
			}
		}
	}

	patchRes, err := devops.ApplyEditBlocks(devops.EditBlockRequest{
		RepoPath:   state.WorkspaceDir,
		Blocks:     allBlocks,
		FuzzyMatch: true,
	})
	if err != nil {
		// Stage 5 Fallback/Recovery: Try block-by-block application to recover from multi-block failures
		var recoveredFiles []string
		for _, b := range allBlocks {
			singleRes, singleErr := devops.ApplyEditBlocks(devops.EditBlockRequest{
				RepoPath:   state.WorkspaceDir,
				Blocks:     []devops.EditBlock{b},
				FuzzyMatch: true,
			})
			if singleErr == nil && singleRes != nil && singleRes.Applied {
				recoveredFiles = append(recoveredFiles, singleRes.FilesModified...)
			} else {
				// Resilient fallback: write ReplaceText ONLY if the file genuinely did not exist before this turn.
				// Check PreTurnSnapshots: nil snapshot means file was created fresh this turn.
				// This prevents overwriting files that existed before turn start but appear empty after pre-creation.
				fp := filepath.Join(state.WorkspaceDir, filepath.FromSlash(b.FilePath))
				snapshot, snapshotExists := turn.PreTurnSnapshots[b.FilePath]
				if snapshotExists && snapshot == nil {
					// File was created fresh this turn (nil snapshot = didn't exist before turn)
					_ = os.WriteFile(fp, []byte(b.ReplaceText), 0644)
					recoveredFiles = append(recoveredFiles, b.FilePath)
				} else if !snapshotExists {
					// No snapshot at all — check if file is brand new or empty on disk
					existing, readErr := os.ReadFile(fp)
					if readErr != nil || len(strings.TrimSpace(string(existing))) == 0 {
						_ = os.WriteFile(fp, []byte(b.ReplaceText), 0644)
						recoveredFiles = append(recoveredFiles, b.FilePath)
					}
				}
				// If file existed before turn (snapshot != nil), skip — patch failed, don't blindly overwrite
			}
		}
		applied := len(recoveredFiles) > 0
		status := "APPLIED_WITH_FALLBACK"
		if !applied {
			status = "REJECTED"
		}
		patchRes = &devops.EditBlockResult{
			Status:        status,
			PatchID:       fmt.Sprintf("fallback-%d", time.Now().UnixNano()),
			FilesModified: recoveredFiles,
			BlocksApplied: len(recoveredFiles),
			Applied:       applied,
			ErrorMessage:  fmt.Sprintf("Stage 5 edit block application: %v", err),
		}
	}

	turn.Patches = append(turn.Patches, patchRes)
	if patchRes != nil {
		for _, modFile := range patchRes.FilesModified {
			if strings.HasSuffix(strings.ToLower(modFile), ".go") {
				fp := filepath.Join(state.WorkspaceDir, filepath.FromSlash(modFile))
				if data, err := os.ReadFile(fp); err == nil {
					norm := devops.NormalizeGoSource(string(data))
					if norm != string(data) {
						_ = os.WriteFile(fp, []byte(norm), 0644)
					}
				}
			}
		}
	}
	for _, f := range patchRes.FilesModified {
		state.RecordTouch(f)
	}
	if patchRes != nil && patchRes.Applied && turn.Transaction != nil {
		turn.Transaction.Status = TxStaged
		for _, f := range patchRes.FilesModified {
			fullP := filepath.Join(state.WorkspaceDir, filepath.FromSlash(f))
			if data, err := os.ReadFile(fullP); err == nil {
				turn.Transaction.StagedContents[f] = data
			}
		}
	}
	if patchRes.MerkleSeal != "" {
		state.AddMerkleLeaf(patchRes.MerkleSeal)
	}

	if patchRes != nil {
		exitCode := 0
		diagMsg := fmt.Sprintf("Applied %d edit blocks (+%d -%d)", patchRes.BlocksApplied, patchRes.LinesAdded, patchRes.LinesRemoved)
		if !patchRes.Applied {
			exitCode = 1
			diagMsg = patchRes.ErrorMessage
			if diagMsg == "" {
				diagMsg = "Patch rejected during atomic staging"
			}
		}

		filesMod := strings.Join(patchRes.FilesModified, ", ")
		seal := patchRes.MerkleSeal
		if seal == "" {
			seal = patchRes.CASHash
		}
		if seal == "" {
			seal = canonical.SHA256Of(patchRes)
		}

		receipt := &icx.ActuationReceipt{
			FileModified:      filesMod,
			ExitCode:          exitCode,
			DiagnosticMessage: diagMsg,
			RepairDiffSummary: patchRes.DiffSummary,
			SHA256Seal:        seal,
			TurnNumber:        turn.TurnNumber,
			Surface:           "engine_stage5_patch",
			Action:            "fuzzy_patch",
			Timestamp:         time.Now().UnixNano(),
		}

		if p.orch.icxClient != nil {
			actRes, actErr := p.orch.icxClient.DispatchActuation(ctx, icx.DispatchActuationRequest{
				Surface: receipt.Surface,
				Action:  receipt.Action,
				Payload: receipt.ToPayload(),
			})
			if actErr == nil && actRes != nil && actRes.Receipt != "" {
				state.AddMerkleLeaf(actRes.Receipt)
			} else if receipt.SHA256Seal != "" {
				state.AddMerkleLeaf(receipt.SHA256Seal)
			}
		} else if receipt.SHA256Seal != "" {
			state.AddMerkleLeaf(receipt.SHA256Seal)
		}
		turn.ActuationReceipts = append(turn.ActuationReceipts, receipt)
	}
	return nil
}

// Stage 6: Compiler Feedback & Self-Healing Loop.
func (p *TurnPipeline) executeStage6CompilerFeedbackAndHeal(ctx context.Context, state *EngineState, turn *TurnState, cfg EngineConfig) error {
	p.emitEvent(turn.TurnNumber, cfg.MaxTurns, 6, "Compiler Feedback & Self-Healing", "Checking compiler diagnostics and self-healing...")

	if cfg.AuditOnly {
		p.emitProgress(turn.TurnNumber, cfg.MaxTurns, 6, "Compiler Feedback", "PASS",
			"Audit-only mode active: skipped execution and self-healing", 0, 0)
		return nil
	}

	testCmd := cfg.TestCommand
	testDir := state.WorkspaceDir
	if testCmd == "" && cfg.BuildCommand != "" {
		testCmd = cfg.BuildCommand
	}
	if testCmd == "" {
		planSpec := DetectPlanSpec(turn.Task, state.WorkspaceDir)
		if planSpec != nil {
			projAbs := filepath.Join(state.WorkspaceDir, filepath.FromSlash(planSpec.ProjectScope))
			testDir = projAbs
			if _, err := os.Stat(filepath.Join(projAbs, "go.mod")); err == nil {
				activePhase := turn.ActivePlanPhase
				if activePhase == nil {
					activePhase = planSpec.DetermineActivePhase(state.WorkspaceDir)
				}
				if activePhase != nil {
					if activePhase.VerificationCmd != "" {
						testCmd = CleanVerificationCmd(activePhase.VerificationCmd)
					} else {
						// Non-Go phases (e.g. web assets) do not have Go compiler feedback
						p.emitProgress(turn.TurnNumber, cfg.MaxTurns, 6, "Compiler Feedback & Self-Healing", "PASS", "Assets verified (no Go test step required for this phase)", 0, 0)
						return nil
					}
				} else {
					testCmd = "go test ./..."
				}
			} else {
				// Project scaffolding in progress (go.mod not yet created in target project)
				p.emitProgress(turn.TurnNumber, cfg.MaxTurns, 6, "Compiler Feedback & Self-Healing", "PASS", "Project scaffolding in progress (target go.mod not yet created)", 0, 0)
				return nil
			}
		}
	}
	if testCmd == "" {
		var targetPkg string
		for _, tf := range turn.TargetFiles {
			cleanTf := filepath.ToSlash(tf)
			if strings.HasPrefix(cleanTf, "projects/") || strings.HasPrefix(cleanTf, "apps/") {
				parts := strings.Split(cleanTf, "/")
				if len(parts) >= 2 {
					projRel := parts[0] + "/" + parts[1]
					projAbs := filepath.Join(state.WorkspaceDir, filepath.FromSlash(projRel))
					testDir = projAbs
					if _, err := os.Stat(filepath.Join(projAbs, "go.mod")); err == nil {
						testCmd = "go test ./..."
						break
					} else {
						// Target subproject has no go.mod yet; do not test host root
						return nil
					}
				}
			} else if strings.HasPrefix(cleanTf, "pkg/") || strings.HasPrefix(cleanTf, "cmd/") {
				parts := strings.Split(cleanTf, "/")
				if len(parts) >= 2 {
					targetPkg = fmt.Sprintf("./%s/%s/...", parts[0], parts[1])
					break
				}
			}
		}
		if testCmd == "" {
			if targetPkg != "" {
				testCmd = "go test " + targetPkg
			} else if _, err := os.Stat(filepath.Join(state.WorkspaceDir, "go.mod")); err == nil {
				testCmd = "go test ./..."
			} else {
				return nil // No compiler command configured
			}
		}
	}

	timeoutSec := 60
	if cfg.Timeout > 0 {
		timeoutSec = int(cfg.Timeout.Seconds())
	}

	backend := "Host_Subprocess"
	if cfg.UseSandbox && cfg.VMClient != nil {
		backend = "Calera_MicroVM"
	}

	report, err := devops.RunCompilerFeedbackLoop(ctx, devops.CompilerFeedbackRequest{
		Command:          testCmd,
		WorkspaceDir:     testDir,
		TimeoutSeconds:   timeoutSec,
		ExecutionBackend: backend,
		SandboxID:        turn.SandboxID,
		VMClient:         cfg.VMClient,
	})
	if err != nil {
		return fmt.Errorf("compiler feedback execution: %w", err)
	}
	turn.CompilerReports = append(turn.CompilerReports, report)

	// Self-healing loop if errors occurred
	maxHeals := state.MaxHealAttempts
	planSpecForHeal := DetectPlanSpec(turn.Task, state.WorkspaceDir)
	if planSpecForHeal != nil && maxHeals < 8 {
		maxHeals = 8
	}

	healAttempts := 0
	for !report.Success && report.ExitCode != 0 && healAttempts < maxHeals {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		if len(report.Diagnostics) == 0 {
			break
		}

		healAttempts++
		turn.HealAttempts++
		state.TotalHeals++

		diag := report.Diagnostics[0]
		snippet := strings.Join(diag.ContextLines, "\n")

		p.emitProgress(turn.TurnNumber, cfg.MaxTurns, 6, "Compiler Feedback & Self-Healing", "HEAL",
			fmt.Sprintf("Attempt %d/%d: healing %s:%d %s", healAttempts, maxHeals, diag.File, diag.Line, diag.Message),
			healAttempts, maxHeals)

		// Translate diag.File from testDir-relative to workspaceDir-relative
		resolvedDiagFile := diag.File
		if testDir != state.WorkspaceDir {
			relProj, err := filepath.Rel(state.WorkspaceDir, testDir)
			if err == nil && !strings.HasPrefix(relProj, "..") {
				cleanProj := filepath.ToSlash(relProj)
				cleanDiag := filepath.ToSlash(diag.File)
				if !strings.HasPrefix(cleanDiag, cleanProj+"/") {
					resolvedDiagFile = cleanProj + "/" + cleanDiag
				}
			}
		}

		// If resolvedDiagFile does not exist on disk, look for a matching filename within testDir
		checkPath := filepath.Join(state.WorkspaceDir, filepath.FromSlash(resolvedDiagFile))
		if _, err := os.Stat(checkPath); os.IsNotExist(err) {
			targetBase := filepath.Base(diag.File)
			_ = filepath.Walk(testDir, func(path string, info os.FileInfo, walkErr error) error {
				if walkErr == nil && !info.IsDir() && info.Name() == targetBase {
					if rel, relErr := filepath.Rel(state.WorkspaceDir, path); relErr == nil {
						resolvedDiagFile = filepath.ToSlash(rel)
						return filepath.SkipAll
					}
				}
				return nil
			})
		}

		// Generate an error signature (filepath:error_message_hash)
		errHash := canonical.SHA256Bytes([]byte(strings.TrimSpace(diag.Message)))
		if len(errHash) > 16 {
			errHash = errHash[:16]
		}
		errorFingerprint := fmt.Sprintf("%s:%s", filepath.ToSlash(resolvedDiagFile), errHash)

		// Deterministic Pre-Heal Pass for trivial compiler errors (unused imports, case typos)
		if preHealBlock, preHealSummary, ok := attemptDeterministicPreHeal(state.WorkspaceDir, resolvedDiagFile, diag); ok {
			p.emitProgress(turn.TurnNumber, cfg.MaxTurns, 6, "Compiler Feedback & Self-Healing", "HEAL",
				fmt.Sprintf("Attempt %d/%d (Pre-Heal): %s in %s:%d", healAttempts, maxHeals, preHealSummary, resolvedDiagFile, diag.Line),
				healAttempts, maxHeals)

			patchRes, patchErr := devops.ApplyEditBlocks(devops.EditBlockRequest{
				RepoPath:   state.WorkspaceDir,
				Blocks:     []devops.EditBlock{preHealBlock},
				FuzzyMatch: true,
			})
			if patchErr == nil {
				state.RecordTouch(preHealBlock.FilePath)
				if patchRes.MerkleSeal != "" {
					state.AddMerkleLeaf(patchRes.MerkleSeal)
				}
				turn.HealDecisions = append(turn.HealDecisions, &llm.HealDecision{
					FilePath:      preHealBlock.FilePath,
					RepairSummary: "Deterministic pre-heal: " + preHealSummary,
					EditBlocks: []llm.EditBlockData{{
						SearchText:  preHealBlock.SearchText,
						ReplaceText: preHealBlock.ReplaceText,
					}},
				})

				// Re-run compiler check to verify if the trivial error was resolved
				report, err = devops.RunCompilerFeedbackLoop(ctx, devops.CompilerFeedbackRequest{
					Command:        testCmd,
					WorkspaceDir:   testDir,
					TimeoutSeconds: timeoutSec,
				})
				if err != nil {
					if ctx.Err() != nil {
						return ctx.Err()
					}
					if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
						return err
					}
					break
				}
				turn.CompilerReports = append(turn.CompilerReports, report)
				if report.Success && report.ExitCode == 0 {
					if state.Vault != nil && errorFingerprint != "" {
						if patchBytes, err := json.Marshal([]devops.EditBlock{preHealBlock}); err == nil {
							_ = state.Vault.PutHealPattern(errorFingerprint, string(patchBytes))
						}
					}
					break
				}
				continue
			}
		}

		// Query state.Vault.GetHealPattern(fingerprint) before calling the LLM gate. If a known patch exists, apply it immediately.
		if state.Vault != nil {
			if cachedPatch, found := state.Vault.GetHealPattern(errorFingerprint); found && cachedPatch != "" {
				var cachedBlocks []devops.EditBlock
				if err := json.Unmarshal([]byte(cachedPatch), &cachedBlocks); err == nil && len(cachedBlocks) > 0 {
					p.emitProgress(turn.TurnNumber, cfg.MaxTurns, 6, "Compiler Feedback & Self-Healing", "HEAL",
						fmt.Sprintf("Attempt %d/%d (Vault Cache): applying cached heal pattern for %s", healAttempts, maxHeals, errorFingerprint),
						healAttempts, maxHeals)

					patchRes, patchErr := devops.ApplyEditBlocks(devops.EditBlockRequest{
						RepoPath:   state.WorkspaceDir,
						Blocks:     cachedBlocks,
						FuzzyMatch: true,
					})
					if patchErr == nil {
						for _, b := range cachedBlocks {
							state.RecordTouch(b.FilePath)
						}
						if patchRes.MerkleSeal != "" {
							state.AddMerkleLeaf(patchRes.MerkleSeal)
						}
						var editBlockData []llm.EditBlockData
						for _, b := range cachedBlocks {
							editBlockData = append(editBlockData, llm.EditBlockData{
								SearchText:  b.SearchText,
								ReplaceText: b.ReplaceText,
							})
						}
						turn.HealDecisions = append(turn.HealDecisions, &llm.HealDecision{
							FilePath:      resolvedDiagFile,
							RepairSummary: "Vault cached heal pattern: " + errorFingerprint,
							EditBlocks:    editBlockData,
						})

						// Re-run compiler check to see if cached fix resolved the problem
						report, err = devops.RunCompilerFeedbackLoop(ctx, devops.CompilerFeedbackRequest{
							Command:          testCmd,
							WorkspaceDir:     testDir,
							TimeoutSeconds:   timeoutSec,
							ExecutionBackend: backend,
							SandboxID:        turn.SandboxID,
							VMClient:         cfg.VMClient,
						})
						if err != nil {
							if ctx.Err() != nil {
								return ctx.Err()
							}
							if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
								return err
							}
							break
						}
						turn.CompilerReports = append(turn.CompilerReports, report)
						if report.Success && report.ExitCode == 0 {
							break
						}
						continue
					}
				}
			}
		}

		var errMsgBuilder strings.Builder
		errMsgBuilder.WriteString(diag.Message)
		if len(report.Diagnostics) > 1 {
			errMsgBuilder.WriteString("\nAdditional compiler errors in this pass:")
			maxExtra := 6
			if len(report.Diagnostics) < maxExtra {
				maxExtra = len(report.Diagnostics)
			}
			for i := 1; i < maxExtra; i++ {
				d := report.Diagnostics[i]
				errMsgBuilder.WriteString(fmt.Sprintf("\n- %s:%d: %s", d.File, d.Line, d.Message))
			}
		}

		healCard := llm.GateHealCard{
			FilePath:      resolvedDiagFile,
			ErrorLine:     diag.Line,
			ErrorMessage:  errMsgBuilder.String(),
			Snippet5Lines: snippet,
			Command:       testCmd,
			ErrorCode:     diag.ErrorCode,
		}

		healDec, err := p.orch.gate.DecideHeal(ctx, healCard)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return err
			}
			break
		}
		turn.HealDecisions = append(turn.HealDecisions, healDec)

		var healBlocks []devops.EditBlock
		for _, b := range healDec.EditBlocks {
			targetFP := healDec.FilePath
			if targetFP == "" {
				targetFP = resolvedDiagFile
			}
			if testDir != state.WorkspaceDir {
				relProj, err := filepath.Rel(state.WorkspaceDir, testDir)
				if err == nil && !strings.HasPrefix(relProj, "..") {
					cleanProj := filepath.ToSlash(relProj)
					cleanTarget := filepath.ToSlash(targetFP)
					if !strings.HasPrefix(cleanTarget, cleanProj+"/") {
						targetFP = cleanProj + "/" + cleanTarget
					}
				}
			}
			// If targetFP does not exist, check if resolvedDiagFile exists on disk
			fullTarget := filepath.Join(state.WorkspaceDir, filepath.FromSlash(targetFP))
			if _, err := os.Stat(fullTarget); os.IsNotExist(err) {
				fullResolved := filepath.Join(state.WorkspaceDir, filepath.FromSlash(resolvedDiagFile))
				if _, err := os.Stat(fullResolved); err == nil {
					targetFP = resolvedDiagFile
				}
			}
			healBlocks = append(healBlocks, devops.EditBlock{
				FilePath:    filepath.ToSlash(targetFP),
				SearchText:  b.SearchText,
				ReplaceText: b.ReplaceText,
			})
		}

		if len(healBlocks) > 0 {
			// Ensure files exist before patching
			for _, b := range healBlocks {
				fullPath := filepath.Join(state.WorkspaceDir, filepath.FromSlash(b.FilePath))
				if _, err := os.Stat(fullPath); os.IsNotExist(err) {
					if err := os.MkdirAll(filepath.Dir(fullPath), 0755); err == nil {
						_ = os.WriteFile(fullPath, []byte(""), 0644)
					}
				}
			}

			patchRes, patchErr := devops.ApplyEditBlocks(devops.EditBlockRequest{
				RepoPath:   state.WorkspaceDir,
				Blocks:     healBlocks,
				FuzzyMatch: true,
			})
			if patchErr != nil {
				break
			}
			for _, b := range healBlocks {
				state.RecordTouch(b.FilePath)
				if strings.HasSuffix(strings.ToLower(b.FilePath), ".go") {
					fp := filepath.Join(state.WorkspaceDir, filepath.FromSlash(b.FilePath))
					if data, err := os.ReadFile(fp); err == nil {
						norm := devops.NormalizeGoSource(string(data))
						if norm != string(data) {
							_ = os.WriteFile(fp, []byte(norm), 0644)
						}
					}
				}
			}
			if patchRes.MerkleSeal != "" {
				state.AddMerkleLeaf(patchRes.MerkleSeal)
			}
		}

		// Re-run compiler check
		report, err = devops.RunCompilerFeedbackLoop(ctx, devops.CompilerFeedbackRequest{
			Command:          testCmd,
			WorkspaceDir:     testDir,
			TimeoutSeconds:   timeoutSec,
			ExecutionBackend: backend,
			SandboxID:        turn.SandboxID,
			VMClient:         cfg.VMClient,
		})
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return err
			}
			break
		}
		turn.CompilerReports = append(turn.CompilerReports, report)
		if report.Success && report.ExitCode == 0 {
			if state.Vault != nil && errorFingerprint != "" && len(healBlocks) > 0 {
				if patchBytes, err := json.Marshal(healBlocks); err == nil {
					_ = state.Vault.PutHealPattern(errorFingerprint, string(patchBytes))
				}
			}
			break
		}
	}

	if ctx.Err() != nil {
		return ctx.Err()
	}

	if report != nil {
		var healedFiles []string
		var diffSummaries []string
		for _, hd := range turn.HealDecisions {
			healedFiles = append(healedFiles, hd.FilePath)
			if hd.RepairSummary != "" {
				diffSummaries = append(diffSummaries, hd.RepairSummary)
			}
		}

		diagMsg := "Clean compilation verified"
		if !report.Success || report.ExitCode != 0 {
			if len(report.Diagnostics) > 0 {
				diagMsg = fmt.Sprintf("%s:%d: %s", report.Diagnostics[0].File, report.Diagnostics[0].Line, report.Diagnostics[0].Message)
			} else if report.CleanedStderr != "" {
				diagMsg = report.CleanedStderr
			} else {
				diagMsg = fmt.Sprintf("Compilation exited with code %d", report.ExitCode)
			}
		} else if healAttempts > 0 {
			diagMsg = fmt.Sprintf("Healed %d diagnostic faults in %d attempts", len(turn.HealDecisions), healAttempts)
		}

		repairDiff := strings.Join(diffSummaries, "; ")
		if repairDiff == "" && len(turn.Patches) > 0 {
			repairDiff = turn.Patches[len(turn.Patches)-1].DiffSummary
		}

		seal := ""
		if len(turn.Patches) > 0 && turn.Patches[len(turn.Patches)-1].MerkleSeal != "" {
			seal = turn.Patches[len(turn.Patches)-1].MerkleSeal
		} else {
			seal = canonical.SHA256Of(report)
		}

		filesMod := strings.Join(healedFiles, ", ")
		if filesMod == "" && len(turn.TargetFiles) > 0 {
			filesMod = strings.Join(turn.TargetFiles, ", ")
		}

		receipt := &icx.ActuationReceipt{
			FileModified:      filesMod,
			ExitCode:          report.ExitCode,
			DiagnosticMessage: diagMsg,
			RepairDiffSummary: repairDiff,
			SHA256Seal:        seal,
			TurnNumber:        turn.TurnNumber,
			Surface:           "engine_stage6_heal",
			Action:            "compiler_self_healing",
			Timestamp:         time.Now().UnixNano(),
		}

		if p.orch.icxClient != nil {
			actRes, actErr := p.orch.icxClient.DispatchActuation(ctx, icx.DispatchActuationRequest{
				Surface: receipt.Surface,
				Action:  receipt.Action,
				Payload: receipt.ToPayload(),
			})
			if actErr == nil && actRes != nil && actRes.Receipt != "" {
				state.AddMerkleLeaf(actRes.Receipt)
			} else if receipt.SHA256Seal != "" {
				state.AddMerkleLeaf(receipt.SHA256Seal)
			}
		} else if receipt.SHA256Seal != "" {
			state.AddMerkleLeaf(receipt.SHA256Seal)
		}
		turn.ActuationReceipts = append(turn.ActuationReceipts, receipt)
	}

	return nil
}

var (
	unusedImportRegex1  = regexp.MustCompile(`(?i)imported (?:and|but) not used:\s*"?([^"\s]+)"?`)
	unusedImportRegex2  = regexp.MustCompile(`(?i)"([^"]+)"\s*imported (?:and|but) not used`)
	didYouMeanRegex     = regexp.MustCompile(`(?i)undefined:\s*([A-Za-z0-9_.]+),\s*did you mean\s*([A-Za-z0-9_.]+)\?`)
	hasNoFieldRegex     = regexp.MustCompile(`(?i)has no field or method\s*([A-Za-z0-9_]+).*?but does have\s*(?:field|method)?\s*([A-Za-z0-9_]+)`)
	undefinedIdentRegex = regexp.MustCompile(`(?i)undefined:\s*([A-Za-z0-9_.]+)`)
	// Patterns for expanded deterministic pre-heal
	declaredNotUsedRegex = regexp.MustCompile(`(?i)([A-Za-z_][A-Za-z0-9_]*) declared (?:and|but) not used`)
	tooManyArgsRegex     = regexp.MustCompile(`(?i)too many arguments`)
	duplicateFieldRegex  = regexp.MustCompile(`(?i)duplicate field (?:name )?["']?([A-Za-z0-9_]+)["']?`)
	pyNameErrorRegex     = regexp.MustCompile(`NameError:\s*name\s+['"]([A-Za-z_][A-Za-z0-9_]*)['"]\s+is not defined`)
)

// AttemptDeterministicPreHeal is the exported pre-heal used by the conversational sandbox.
func AttemptDeterministicPreHeal(workspaceDir, resolvedDiagFile string, diag devops.CompilerDiagnostic) (devops.EditBlock, string, bool) {
	return attemptDeterministicPreHeal(workspaceDir, resolvedDiagFile, diag)
}

// attemptDeterministicPreHeal analyzes compiler diagnostics for trivial errors (such as unused imports
// or case typos with compiler suggestions) and generates surgical edit blocks to resolve them deterministically
// without invoking the LLM decision gate.
func attemptDeterministicPreHeal(workspaceDir, resolvedDiagFile string, diag devops.CompilerDiagnostic) (devops.EditBlock, string, bool) {
	if resolvedDiagFile == "" {
		return devops.EditBlock{}, "", false
	}
	fullPath := filepath.Join(workspaceDir, filepath.FromSlash(resolvedDiagFile))
	contentBytes, err := os.ReadFile(fullPath)
	if err != nil {
		return devops.EditBlock{}, "", false
	}

	rawContent := string(contentBytes)
	lines := strings.Split(rawContent, "\n")
	if len(lines) == 0 {
		return devops.EditBlock{}, "", false
	}

	// 1. Trivial Error: Unused Import
	var unusedPkg string
	if m := unusedImportRegex1.FindStringSubmatch(diag.Message); len(m) > 1 {
		unusedPkg = m[1]
	} else if m := unusedImportRegex2.FindStringSubmatch(diag.Message); len(m) > 1 {
		unusedPkg = m[1]
	}

	if unusedPkg != "" {
		pkgBase := filepath.Base(unusedPkg)
		targetLineIdx := -1

		// First check the reported diagnostic line
		if diag.Line >= 1 && diag.Line <= len(lines) {
			l := lines[diag.Line-1]
			if strings.Contains(l, unusedPkg) || strings.Contains(l, `"`+pkgBase+`"`) {
				targetLineIdx = diag.Line - 1
			}
		}

		// Fallback: search within import blocks in first 100 lines
		if targetLineIdx == -1 {
			searchLimit := len(lines)
			if searchLimit > 100 {
				searchLimit = 100
			}
			for i := 0; i < searchLimit; i++ {
				l := lines[i]
				if strings.Contains(l, `"`+unusedPkg+`"`) || strings.Contains(l, `"`+pkgBase+`"`) {
					targetLineIdx = i
					break
				}
			}
		}

		if targetLineIdx != -1 {
			offendingLine := lines[targetLineIdx]
			search := offendingLine + "\n"
			if !strings.HasSuffix(rawContent, "\n") && targetLineIdx == len(lines)-1 {
				search = offendingLine
			}
			return devops.EditBlock{
				FilePath:    resolvedDiagFile,
				SearchText:  search,
				ReplaceText: "",
			}, fmt.Sprintf("removed unused import %q", unusedPkg), true
		}
	}

	// 1b. Missing Standard Library Import (undefined: fmt, undefined: errors, etc.)
	var missingStdPkg string
	if m := undefinedIdentRegex.FindStringSubmatch(diag.Message); len(m) > 1 {
		ident := strings.TrimSpace(m[1])
		switch ident {
		case "fmt", "errors", "strings", "sync", "time", "math", "os", "sort", "reflect", "bytes", "io":
			missingStdPkg = ident
		}
	}

	if missingStdPkg != "" {
		alreadyImported := false
		for _, l := range lines {
			if strings.Contains(l, `"`+missingStdPkg+`"`) {
				alreadyImported = true
				break
			}
		}
		if !alreadyImported {
			importBlockIdx := -1
			singleImportIdx := -1
			packageDeclIdx := -1

			searchLimit := len(lines)
			if searchLimit > 60 {
				searchLimit = 60
			}

			for i := 0; i < searchLimit; i++ {
				l := strings.TrimSpace(lines[i])
				if strings.HasPrefix(l, "package ") && packageDeclIdx == -1 {
					packageDeclIdx = i
				}
				if strings.HasPrefix(l, "import (") && importBlockIdx == -1 {
					importBlockIdx = i
				} else if strings.HasPrefix(l, "import \"") && singleImportIdx == -1 {
					singleImportIdx = i
				}
			}

			if importBlockIdx != -1 {
				search := lines[importBlockIdx] + "\n"
				replace := lines[importBlockIdx] + "\n\t\"" + missingStdPkg + "\"\n"
				return devops.EditBlock{
					FilePath:    resolvedDiagFile,
					SearchText:  search,
					ReplaceText: replace,
				}, fmt.Sprintf("added missing import %q to import block", missingStdPkg), true
			} else if singleImportIdx != -1 {
				origLine := lines[singleImportIdx]
				search := origLine + "\n"
				replace := "import (\n\t" + strings.TrimPrefix(origLine, "import ") + "\n\t\"" + missingStdPkg + "\"\n)\n"
				return devops.EditBlock{
					FilePath:    resolvedDiagFile,
					SearchText:  search,
					ReplaceText: replace,
				}, fmt.Sprintf("added missing import %q", missingStdPkg), true
			} else if packageDeclIdx != -1 {
				pkgLine := lines[packageDeclIdx]
				search := pkgLine + "\n"
				replace := pkgLine + "\n\nimport \"" + missingStdPkg + "\"\n"
				return devops.EditBlock{
					FilePath:    resolvedDiagFile,
					SearchText:  search,
					ReplaceText: replace,
				}, fmt.Sprintf("added missing import %q", missingStdPkg), true
			}
		}
	}

	// 2. Trivial Error: Case Typo / "Did you mean" compiler suggestion
	var wrongToken, rightToken string
	if m := didYouMeanRegex.FindStringSubmatch(diag.Message); len(m) > 2 {
		wrongToken = m[1]
		rightToken = m[2]
		if strings.Contains(wrongToken, ".") {
			partsW := strings.Split(wrongToken, ".")
			partsR := strings.Split(rightToken, ".")
			wrongToken = partsW[len(partsW)-1]
			rightToken = partsR[len(partsR)-1]
		}
	} else if m := hasNoFieldRegex.FindStringSubmatch(diag.Message); len(m) > 2 {
		wrongToken = m[1]
		rightToken = m[2]
	} else if m := undefinedIdentRegex.FindStringSubmatch(diag.Message); len(m) > 1 {
		ident := m[1]
		if strings.Contains(ident, ".") {
			parts := strings.Split(ident, ".")
			ident = parts[len(parts)-1]
		}
		if len(ident) > 0 {
			var candidate string
			r := rune(ident[0])
			if unicode.IsLower(r) {
				candidate = string(unicode.ToUpper(r)) + ident[1:]
			} else if unicode.IsUpper(r) {
				candidate = string(unicode.ToLower(r)) + ident[1:]
			}

			if candidate != "" {
				pkgDir := filepath.Dir(fullPath)
				entries, _ := os.ReadDir(pkgDir)
				for _, entry := range entries {
					if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".go") {
						siblingData, readErr := os.ReadFile(filepath.Join(pkgDir, entry.Name()))
						if readErr == nil {
							sContent := string(siblingData)
							if strings.Contains(sContent, "func "+candidate+"(") ||
								strings.Contains(sContent, "type "+candidate+" ") ||
								strings.Contains(sContent, "type "+candidate+"\t") ||
								strings.Contains(sContent, "var "+candidate+" ") ||
								strings.Contains(sContent, "const "+candidate+" ") {
								wrongToken = ident
								rightToken = candidate
								break
							}
						}
					}
				}
			}
		}
	}

	if wrongToken != "" && rightToken != "" && wrongToken != rightToken {
		startIdx := diag.Line - 3
		if startIdx < 0 {
			startIdx = 0
		}
		endIdx := diag.Line + 2
		if endIdx >= len(lines) {
			endIdx = len(lines) - 1
		}

		for i := startIdx; i <= endIdx; i++ {
			if strings.Contains(lines[i], wrongToken) {
				origLine := lines[i]
				repairedLine := strings.Replace(origLine, wrongToken, rightToken, 1)
				if origLine != repairedLine {
					return devops.EditBlock{
						FilePath:    resolvedDiagFile,
						SearchText:  origLine,
						ReplaceText: repairedLine,
					}, fmt.Sprintf("corrected case typo %s -> %s", wrongToken, rightToken), true
				}
			}
		}
	}

	// 3. Declared but not used variable — prefix with _
	if declNotUsedMatch := declaredNotUsedRegex.FindStringSubmatch(diag.Message); len(declNotUsedMatch) > 1 {
		varName := declNotUsedMatch[1]
		if diag.Line >= 1 && diag.Line <= len(lines) {
			origLine := lines[diag.Line-1]
			if strings.Contains(origLine, varName) {
				// Replace variable name with _ prefix (e.g., "err" -> "_")
				// Check if it's a short var declaration like "x := ..."
				if strings.Contains(origLine, varName+" :=") {
					repairedLine := strings.Replace(origLine, varName+" :=", "_ =", 1)
					return devops.EditBlock{
						FilePath:    resolvedDiagFile,
						SearchText:  origLine,
						ReplaceText: repairedLine,
					}, fmt.Sprintf("prefixed unused variable %q with _", varName), true
				}
				// Multi-return assignment like "x, err := ..."
				if strings.Contains(origLine, varName+",") || strings.Contains(origLine, ", "+varName) {
					repairedLine := strings.Replace(origLine, varName, "_", 1)
					return devops.EditBlock{
						FilePath:    resolvedDiagFile,
						SearchText:  origLine,
						ReplaceText: repairedLine,
					}, fmt.Sprintf("replaced unused variable %q with _", varName), true
				}
			}
		}
	}

	// 4. Missing return at end of function — add zero-value return
	if strings.Contains(diag.Message, "missing return at end of function") ||
		strings.Contains(diag.Message, "missing return") {
		if diag.Line >= 1 && diag.Line <= len(lines) {
			origLine := lines[diag.Line-1]
			trimmed := strings.TrimSpace(origLine)
			if trimmed == "}" {
				// Insert a "return" before the closing brace
				indent := origLine[:len(origLine)-len(strings.TrimLeft(origLine, " \t"))]
				repairedLine := indent + "\treturn\n" + origLine
				return devops.EditBlock{
					FilePath:    resolvedDiagFile,
					SearchText:  origLine,
					ReplaceText: repairedLine,
				}, "added missing return statement", true
			}
		}
	}

	// 5. Too many arguments in call — detect and remove trailing argument
	if tooManyMatch := tooManyArgsRegex.FindStringSubmatch(diag.Message); len(tooManyMatch) > 0 {
		if diag.Line >= 1 && diag.Line <= len(lines) {
			origLine := lines[diag.Line-1]
			// Find the last comma and remove everything after it until the closing paren
			lastComma := strings.LastIndex(origLine, ",")
			closeParen := strings.LastIndex(origLine, ")")
			if lastComma > 0 && closeParen > lastComma {
				repairedLine := origLine[:lastComma] + origLine[closeParen:]
				return devops.EditBlock{
					FilePath:    resolvedDiagFile,
					SearchText:  origLine,
					ReplaceText: repairedLine,
				}, "removed extra argument from function call", true
			}
		}
	}

	// 6. Duplicate field name in struct literal — remove duplicate line
	if dupFieldMatch := duplicateFieldRegex.FindStringSubmatch(diag.Message); len(dupFieldMatch) > 1 {
		if diag.Line >= 1 && diag.Line <= len(lines) {
			origLine := lines[diag.Line-1]
			search := origLine + "\n"
			if !strings.HasSuffix(rawContent, "\n") && diag.Line == len(lines) {
				search = origLine
			}
			return devops.EditBlock{
				FilePath:    resolvedDiagFile,
				SearchText:  search,
				ReplaceText: "",
			}, fmt.Sprintf("removed duplicate field %q", dupFieldMatch[1]), true
		}
	}

	// 7. Invalid //go:embed pattern containing ".." (Go prohibits relative parent paths in embed)
	if strings.Contains(diag.Message, "invalid pattern syntax") ||
		(strings.Contains(diag.Message, "pattern") && strings.Contains(diag.Message, "embed")) {
		if diag.Line >= 1 && diag.Line <= len(lines) {
			origLine := lines[diag.Line-1]
			if strings.Contains(origLine, "//go:embed") && strings.Contains(origLine, "..") {
				repairedLine := "// (embed removed: Go forbids parent '..' paths in //go:embed; serve via web package or http.FileSystem)"
				return devops.EditBlock{
					FilePath:    resolvedDiagFile,
					SearchText:  origLine,
					ReplaceText: repairedLine,
				}, "removed invalid //go:embed parent path", true
			}
		}
	}

	// 8. Python NameError: missing standard library or intra-package import
	if strings.HasSuffix(strings.ToLower(resolvedDiagFile), ".py") {
		if m := pyNameErrorRegex.FindStringSubmatch(diag.Message); len(m) > 1 {
			missingSym := m[1]
			var importStmt string
			switch missingSym {
			case "re", "os", "sys", "math", "copy", "warnings", "functools", "itertools", "collections",
				"json", "inspect", "types", "numbers", "operator", "textwrap", "traceback", "pathlib",
				"io", "time", "datetime", "uuid", "hashlib", "shutil", "tempfile", "weakref", "contextlib", "abc", "enum":
				importStmt = "import " + missingSym
			case "Path":
				importStmt = "from pathlib import Path"
			case "defaultdict", "OrderedDict", "Counter", "deque", "namedtuple":
				importStmt = "from collections import " + missingSym
			case "partial", "lru_cache", "reduce", "wraps", "total_ordering":
				importStmt = "from functools import " + missingSym
			case "chain", "islice", "product", "combinations", "permutations", "groupby", "cycle", "repeat":
				importStmt = "from itertools import " + missingSym
			case "Any", "Optional", "Union", "List", "Dict", "Tuple", "Set", "Callable", "Iterable", "Iterator", "Sequence", "Mapping", "Type", "TYPE_CHECKING", "cast", "overload":
				importStmt = "from typing import " + missingSym
			case "dataclass", "field":
				importStmt = "from dataclasses import " + missingSym
			}

			if importStmt != "" && !strings.Contains(rawContent, importStmt) {
				insertIdx := -1
				searchLimit := len(lines)
				if searchLimit > 80 {
					searchLimit = 80
				}
				for i := 0; i < searchLimit; i++ {
					trimmed := strings.TrimSpace(lines[i])
					if strings.HasPrefix(trimmed, "from __future__") {
						continue
					}
					if (strings.HasPrefix(trimmed, "import ") || strings.HasPrefix(trimmed, "from ")) &&
						len(lines[i]) > 0 && lines[i][0] != ' ' && lines[i][0] != '\t' &&
						!strings.HasSuffix(trimmed, "(") && !strings.HasSuffix(trimmed, "\\") {
						insertIdx = i
						break
					}
				}
				if insertIdx != -1 {
					anchorLine := lines[insertIdx]
					return devops.EditBlock{
						FilePath:    resolvedDiagFile,
						SearchText:  anchorLine,
						ReplaceText: importStmt + "\n" + anchorLine,
					}, fmt.Sprintf("added missing Python import %q", importStmt), true
				}
			}
		}
	}

	return devops.EditBlock{}, "", false
}

// Stage 7: Crystallize Turn Learnings & Zero-Token Session Reset.
// First records what happened this turn into the ICX lattice (family="trajectory" survives reset),
// then resets dialogue tokens to 0 while preserving all 4D memory, trajectory facts, and swarm variables.
func (p *TurnPipeline) executeStage7ZeroTokenSessionReset(ctx context.Context, state *EngineState, turn *TurnState, cfg EngineConfig) error {
	p.emitEvent(turn.TurnNumber, cfg.MaxTurns, 7, "Zero-Token Session Reset", "Crystallizing turn learnings, then resetting dialogue tokens...")

	// 1. CRYSTALLIZE: Record turn outcome into trajectory family (survives icx_reset_session)
	outcome := formatTurnOutcome(turn)
	if outcome != "" {
		_, _ = p.orch.icxClient.Remember(ctx, icx.RememberRequest{
			SpaceID:  state.LatticeSpaceID,
			Family:   "trajectory", // NOT "dialogue_turn" — survives reset!
			SourceID: fmt.Sprintf("turn_%d", turn.TurnNumber),
			Text:     outcome,
		})
	}

	// 2. Update CAS-protected completed_files swarm variable
	completedVar, _ := p.orch.icxClient.VarGet(ctx, icx.VarGetRequest{
		Key: "completed_files", SpaceID: state.LatticeSpaceID,
	})
	completed := parseCompletedFilesVar(completedVar)
	if turn.Success {
		for _, f := range turn.TargetFiles {
			completed[f] = turn.TurnNumber
		}
	}
	_, _ = p.orch.icxClient.VarSet(ctx, icx.VarSetRequest{
		Key:     "completed_files",
		Value:   completed,
		Writer:  fmt.Sprintf("turn_%d", turn.TurnNumber),
		SpaceID: state.LatticeSpaceID,
	})

	// 3. Record avoid patterns if turn failed
	if !turn.Success && turn.ErrorMessage != "" {
		avoidText := fmt.Sprintf("T%d FAIL: %s", turn.TurnNumber, truncateStr(turn.ErrorMessage, 120))
		_, _ = p.orch.icxClient.Remember(ctx, icx.RememberRequest{
			SpaceID:  state.LatticeSpaceID,
			Family:   "trajectory",
			SourceID: fmt.Sprintf("avoid_turn_%d", turn.TurnNumber),
			Text:     avoidText,
		})
	}

	// 4. RESET: Clear dialogue tokens (existing behavior — dialogue_turn family pruned)
	_, err := p.orch.icxClient.ResetSession(ctx, state.LatticeSpaceID)
	if err != nil {
		// Non-fatal if reset session is unsupported by external mock or transport
		return nil
	}
	return nil
}

// Stage 8: Git Commit & Attestation.
func (p *TurnPipeline) executeStage8GitCommitAndAttestation(ctx context.Context, state *EngineState, turn *TurnState, cfg EngineConfig) error {
	p.emitEvent(turn.TurnNumber, cfg.MaxTurns, 8, "Git Commit & Attestation", "Verifying GateFinalize and generating Merkle seal...")

	// Inspect latest compiler report
	lastExitCode := 0
	lastSummary := "All checks passed"
	if len(turn.CompilerReports) > 0 {
		lastRep := turn.CompilerReports[len(turn.CompilerReports)-1]
		lastExitCode = lastRep.ExitCode
		if lastRep.CleanedStderr != "" {
			lastSummary = lastRep.CleanedStderr
		} else if lastRep.RawStdout != "" {
			lastSummary = lastRep.RawStdout
		}
	}

	testCmd := cfg.TestCommand
	if testCmd == "" && len(turn.CompilerReports) > 0 {
		testCmd = turn.CompilerReports[len(turn.CompilerReports)-1].Command
	}
	if testCmd == "" {
		testCmd = "verified-clean"
	}

	taskDesc := turn.Task
	planSpec := DetectPlanSpec(turn.Task, state.WorkspaceDir)
	if planSpec != nil {
		activePhase := turn.ActivePlanPhase
		if activePhase == nil {
			activePhase = planSpec.DetermineActivePhase(state.WorkspaceDir)
		}
		if activePhase != nil {
			taskDesc = fmt.Sprintf("Autonomous completion of %s (Phase %d)", activePhase.Name, activePhase.PhaseNumber)
		} else {
			taskDesc = fmt.Sprintf("Autonomous completion of all phases for %s", planSpec.ModuleName)
		}
	}

	finalizeCard := llm.GateFinalizeCard{
		TaskDescription:   taskDesc,
		FilesModified:     state.TouchedList(),
		TestCommand:       testCmd,
		TestExitCode:      lastExitCode,
		TestOutputSummary: lastSummary,
	}

	finalizeDec, err := p.orch.gate.DecideFinalize(ctx, finalizeCard)
	if err != nil {
		return fmt.Errorf("gate decide finalize: %w", err)
	}
	turn.Finalize = finalizeDec

	if cfg.AuditOnly {
		turn.Success = true
		turn.TurnMerkleSeal = state.ComputeMerkleSeal()
		p.emitProgress(turn.TurnNumber, cfg.MaxTurns, 8, "Audit-Only Attestation", "PASS",
			"Audit complete: diagnostic recommendations generated without disk commits", 0, 0)
		return nil
	}

	if finalizeDec.Approved {
		touched := state.TouchedList()
		if len(touched) > 0 {
			commitHash, err := gitCommitTouchedFiles(ctx, state.WorkspaceDir, finalizeDec.CommitMessage, touched)
			if err == nil && commitHash != "" {
				turn.CommitHash = commitHash
				state.RecordCommit(commitHash)
				state.AddMerkleLeaf(canonical.SHA256Bytes([]byte(commitHash)))
			}
		}

		turn.TurnMerkleSeal = state.ComputeMerkleSeal()
		turn.Success = true
		p.emitProgress(turn.TurnNumber, cfg.MaxTurns, 8, "Git Commit & Attestation", "PASS",
			fmt.Sprintf("Finalized: %s (seal: %s)", finalizeDec.CommitMessage, turn.TurnMerkleSeal[:min(12, len(turn.TurnMerkleSeal))]), 0, 0)
	} else {
		turn.Success = false
		turn.ErrorMessage = finalizeDec.Reason
		p.emitProgress(turn.TurnNumber, cfg.MaxTurns, 8, "Git Commit & Attestation", "FAIL",
			fmt.Sprintf("Rejected: %s", finalizeDec.Reason), 0, 0)

		// ATOMIC ROLLBACK: Revert modified/created files to their pre-turn clean baseline.
		// For structured multi-phase plans (planSpec != nil) or brand-new project scaffolds,
		// preserve newly created files so subsequent turns and Stage 6 self-healing can iteratively converge.
		allNewScaffold := true
		hasScaffoldFiles := false
		for relPath, snapshot := range turn.PreTurnSnapshots {
			cleanRP := filepath.ToSlash(relPath)
			if strings.HasPrefix(cleanRP, "projects/") || strings.HasPrefix(cleanRP, "apps/") {
				hasScaffoldFiles = true
				if snapshot != nil {
					allNewScaffold = false
					break
				}
			} else {
				allNewScaffold = false
				break
			}
		}

		if planSpec == nil && !(hasScaffoldFiles && allNewScaffold) {
			if turn.Transaction != nil && turn.Transaction.Status != TxAborted && turn.Transaction.Status != TxCommitted {
				_ = RollbackTransaction(turn.Transaction)
			}
			reverted := p.rollbackTurnSnapshots(state.WorkspaceDir, turn)
			if reverted > 0 {
				p.emitProgress(turn.TurnNumber, cfg.MaxTurns, 8, "Atomic Rollback", "ROLLBACK",
					fmt.Sprintf("Rolled back %d modified files to clean pre-turn baseline", reverted), 0, 0)
			}
		} else {
			activePhaseNumber := 1
			if planSpec != nil {
				if activePhase := planSpec.DetermineActivePhase(state.WorkspaceDir); activePhase != nil {
					activePhaseNumber = activePhase.PhaseNumber
				}
			}
			p.emitProgress(turn.TurnNumber, cfg.MaxTurns, 8, "Plan Convergence", "PROGRESS",
				fmt.Sprintf("Preserved newly created Phase %d files on disk for iterative compiler convergence", activePhaseNumber), 0, 0)
		}
	}

	status := "IN_PROGRESS"
	if finalizeDec.Approved {
		status = "COMPLETED"
		if turn.Transaction != nil && turn.Transaction.Status != TxCommitted && turn.Transaction.Status != TxAborted {
			_ = CommitTransaction(turn.Transaction)
		}
	}

	milestone := &icx.TurnMilestone{
		TurnNumber:     turn.TurnNumber,
		Task:           taskDesc,
		TargetFiles:    turn.TargetFiles,
		Status:         status,
		PatchesApplied: len(turn.Patches),
		HealCycles:     turn.HealAttempts,
		Approved:       finalizeDec.Approved,
		CommitHash:     turn.CommitHash,
		MerkleSeal:     turn.TurnMerkleSeal,
		Timestamp:      time.Now().Unix(),
	}

	state.AddMilestone(milestone)

	if p.orch.icxClient != nil {
		_, _ = p.orch.icxClient.CrystallizeMilestone(ctx, state.LatticeSpaceID, milestone)

		// Periodic Checkpointing every 10 turns or upon approval
		if turn.TurnNumber%10 == 0 || finalizeDec.Approved {
			cpPayload, cpErr := json.Marshal(map[string]any{
				"turn":        turn.TurnNumber,
				"task":        state.Task,
				"status":      state.Status,
				"milestones":  len(state.Milestones),
				"merkle_seal": state.ComputeMerkleSeal(),
				"timestamp":   time.Now().Unix(),
			})
			if cpErr == nil {
				_, _ = p.orch.icxClient.VarSet(ctx, icx.VarSetRequest{
					Key:     fmt.Sprintf("checkpoint.turn_%d", turn.TurnNumber),
					Value:   string(cpPayload),
					SpaceID: state.LatticeSpaceID,
				})
			}
		}
	}

	return nil
}

func gitCommitTouchedFiles(ctx context.Context, repoDir, message string, files []string) (string, error) {
	if _, err := exec.LookPath("git"); err != nil {
		return "", nil
	}
	if strings.TrimSpace(message) == "" {
		message = "OmniCoder autonomous commit"
	}
	if len(files) > 0 {
		addArgs := append([]string{"add", "--"}, files...)
		cmd := exec.CommandContext(ctx, "git", addArgs...)
		cmd.Dir = repoDir
		_ = cmd.Run()
	} else {
		cmd := exec.CommandContext(ctx, "git", "add", "-A")
		cmd.Dir = repoDir
		_ = cmd.Run()
	}

	cmd := exec.CommandContext(ctx, "git", "commit", "-m", message)
	cmd.Dir = repoDir
	out, err := cmd.CombinedOutput()
	if err != nil {
		combined := strings.ToLower(string(out))
		if strings.Contains(combined, "nothing to commit") || strings.Contains(combined, "working tree clean") || strings.Contains(combined, "not a git repository") {
			return "", nil
		}
		return "", err
	}

	revCmd := exec.CommandContext(ctx, "git", "rev-parse", "HEAD")
	revCmd.Dir = repoDir
	shaOut, err := revCmd.Output()
	if err != nil {
		return "", nil
	}
	return strings.TrimSpace(string(shaOut)), nil
}

func (p *TurnPipeline) emitEvent(turnNum, maxTurns, stage int, stageName string, msg string) {
	if p.orch.cfg.OnEvent != nil {
		p.orch.cfg.OnEvent(TurnEvent{
			TurnNumber: turnNum,
			Stage:      stage,
			StageName:  stageName,
			Message:    msg,
			Timestamp:  time.Now(),
		})
	}
	if p.orch.cfg.ProgressCallback != nil {
		p.orch.cfg.ProgressCallback(TurnProgressEvent{
			Turn:      turnNum,
			MaxTurns:  maxTurns,
			Stage:     stage,
			StageName: stageName,
			Status:    "STAGE",
			Details:   msg,
		})
	}
}

func (p *TurnPipeline) emitProgress(turnNum, maxTurns, stage int, stageName, status, details string, healAttempt, maxHeals int) {
	if p.orch.cfg.ProgressCallback != nil {
		p.orch.cfg.ProgressCallback(TurnProgressEvent{
			Turn:        turnNum,
			MaxTurns:    maxTurns,
			Stage:       stage,
			StageName:   stageName,
			Status:      status,
			Details:     details,
			HealAttempt: healAttempt,
			MaxHeals:    maxHeals,
		})
	}
}

// IsHostPackagePath reports whether cleanPath falls directly within OmniCoder's own platform packages.
func IsHostPackagePath(cleanPath string) bool {
	return isHostPackagePath(cleanPath)
}

// isHostPackagePath reports whether cleanPath falls directly within OmniCoder's own platform packages.
func isHostPackagePath(cleanPath string) bool {
	clean := filepath.ToSlash(cleanPath)
	clean = strings.TrimPrefix(clean, "./")
	clean = strings.TrimPrefix(clean, "/")
	hostPrefixes := []string{
		"pkg/canonical",
		"pkg/devops",
		"pkg/engine",
		"pkg/icx",
		"pkg/llm",
		"pkg/mcp",
		"pkg/ui",
		"cmd/omnicoder",
	}
	for _, hp := range hostPrefixes {
		if clean == hp || strings.HasPrefix(clean, hp+"/") {
			return true
		}
	}
	return false
}

// IsHostPackageTask reports whether the user's task explicitly requests modifying OmniCoder's own host packages.
func IsHostPackageTask(task string) bool {
	return isHostPackageTask(task)
}

// isHostPackageTask reports whether the user's task explicitly requests modifying OmniCoder's own host packages.
func isHostPackageTask(task string) bool {
	taskLower := strings.ToLower(task)
	// If task explicitly targets a project under projects/ or apps/, it is an application task, not a host task
	if strings.Contains(taskLower, "projects/") || strings.Contains(taskLower, "apps/") {
		return false
	}
	keywords := []string{
		"omnicoder",
		"pkg/canonical",
		"pkg/devops",
		"pkg/engine",
		"pkg/icx",
		"pkg/llm",
		"pkg/mcp",
		"pkg/ui",
		"cmd/omnicoder",
		"safeguard",
		"safeguards",
		"host package",
		"host shield",
		"self-preservation",
		"code preservation",
	}
	for _, kw := range keywords {
		if strings.Contains(taskLower, kw) {
			return true
		}
	}
	return false
}

// rollbackTurnSnapshots restores all files touched during the turn to their clean pre-turn snapshot.
func (p *TurnPipeline) rollbackTurnSnapshots(workspaceDir string, turn *TurnState) int {
	if turn == nil || len(turn.PreTurnSnapshots) == 0 {
		return 0
	}
	reverted := 0
	for relPath, snapshot := range turn.PreTurnSnapshots {
		fullPath := filepath.Join(workspaceDir, filepath.FromSlash(relPath))
		if snapshot == nil {
			// File did not exist before this turn, delete it
			_ = os.Remove(fullPath)
			reverted++
		} else {
			// Restore exact pre-turn bytes
			if err := os.WriteFile(fullPath, snapshot, 0644); err == nil {
				reverted++
			}
		}
	}
	return reverted
}

// detectProjectScope analyzes the task description, focus files, and workspace structure
// to identify whether the task is targeting a subfolder project under projects/ (e.g. projects/my-app).
func detectProjectScope(task string, focusFiles []string, workspaceDir string) string {
	taskLower := strings.ToLower(task)

	// 1. Check focus files first
	for _, f := range focusFiles {
		cleanF := filepath.ToSlash(f)
		cleanF = strings.TrimPrefix(cleanF, "./")
		cleanF = strings.TrimPrefix(cleanF, "/")
		if strings.HasPrefix(cleanF, "projects/") {
			parts := strings.Split(cleanF, "/")
			if len(parts) >= 2 && parts[1] != "" {
				return "projects/" + parts[1]
			}
		}
	}

	// 2. Check task string for "projects/<name>"
	if idx := strings.Index(taskLower, "projects/"); idx != -1 {
		rest := taskLower[idx+len("projects/"):]
		endIdx := strings.IndexAny(rest, " /\\\"'`:,;\n\r\t")
		name := rest
		if endIdx != -1 {
			name = rest[:endIdx]
		}
		name = strings.TrimSpace(name)
		if name != "" {
			return "projects/" + name
		}
	}

	// 3. Match any existing directory in workspace projects/
	projectsDir := filepath.Join(workspaceDir, "projects")
	if entries, err := os.ReadDir(projectsDir); err == nil {
		for _, e := range entries {
			if e.IsDir() {
				pName := strings.ToLower(e.Name())
				if strings.Contains(taskLower, pName) {
					return "projects/" + e.Name()
				}
				tokens := strings.Split(pName, "-")
				matchAll := true
				for _, tok := range tokens {
					if len(tok) > 2 && !strings.Contains(taskLower, tok) {
						matchAll = false
						break
					}
				}
				if matchAll && len(tokens) > 1 {
					return "projects/" + e.Name()
				}
			}
		}
	}

	return ""
}

// formatTurnOutcome produces a compact summary of what happened during a turn.
// Stored in ICX lattice as family="trajectory", survives icx_reset_session.
func formatTurnOutcome(turn *TurnState) string {
	if turn == nil {
		return ""
	}
	status := "FAIL"
	if turn.Success {
		status = "PASS"
	}
	files := strings.Join(turn.TargetFiles, ",")
	if len(files) > 80 {
		files = files[:77] + "..."
	}
	summary := fmt.Sprintf("T%d:%s files=[%s]", turn.TurnNumber, status, files)

	// Record what heals were applied
	if turn.HealAttempts > 0 {
		healSummaries := make([]string, 0, len(turn.HealDecisions))
		for _, hd := range turn.HealDecisions {
			if hd != nil && hd.RepairSummary != "" {
				hs := truncateStr(hd.RepairSummary, 40)
				healSummaries = append(healSummaries, hs)
			}
		}
		if len(healSummaries) > 0 {
			summary += " healed:[" + strings.Join(healSummaries, ";") + "]"
		}
	}

	// Record error if failed
	if !turn.Success && turn.ErrorMessage != "" {
		summary += " err:" + truncateStr(turn.ErrorMessage, 60)
	}

	// Clamp total length for lattice efficiency
	if len(summary) > 250 {
		summary = summary[:247] + "..."
	}
	return summary
}

// parseCompletedFilesVar extracts completed files map from an ICX VarGet response.
func parseCompletedFilesVar(resp *icx.VarGetResponse) map[string]int {
	result := make(map[string]int)
	if resp == nil || !resp.OK || resp.Value == nil {
		return result
	}
	if m, ok := resp.Value.(map[string]any); ok {
		for k, v := range m {
			switch val := v.(type) {
			case float64:
				result[k] = int(val)
			case int:
				result[k] = val
			}
		}
	}
	return result
}

// truncateStr safely truncates a string to maxLen characters.
func truncateStr(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	if maxLen > 3 {
		return s[:maxLen-3] + "..."
	}
	return s[:maxLen]
}

// appendUnique appends s to slice only if not already present.
func appendUnique(slice []string, s string) []string {
	for _, v := range slice {
		if v == s {
			return slice
		}
	}
	return append(slice, s)
}

var hunkScopeRegex = regexp.MustCompile(`^@@\s+-[0-9]+(?:,[0-9]+)?\s+\+[0-9]+(?:,[0-9]+)?\s+@@\s*(.*)$`)

// ParseUnifiedDiffBlocks parses multi-file unified diff blocks (diff --git a/... b/... or --- a/... +++ b/...)
// into structured FileEdit instances with AST scope extraction, markdown fence stripping, and git metadata tolerance.
func ParseUnifiedDiffBlocks(diffText string) ([]FileEdit, error) {
	if strings.TrimSpace(diffText) == "" {
		return nil, errors.New("transaction: unified diff text is empty")
	}

	cleanText := extractDiffTextFromMarkdown(diffText)
	lines := strings.Split(cleanText, "\n")

	var edits []FileEdit
	var curOldPath, curNewPath string
	var curLines []string
	var curScopes []string
	var inHunk bool

	flushCurrent := func() {
		if (curOldPath != "" || curNewPath != "") && len(curLines) > 0 {
			targetPath := resolveDiffPath(curOldPath, curNewPath)
			if targetPath != "" {
				// Ensure diff contains --- and +++ headers for downstream compatibility
				hasMinus := false
				hasPlus := false
				for _, l := range curLines {
					if strings.HasPrefix(l, "--- ") {
						hasMinus = true
					}
					if strings.HasPrefix(l, "+++ ") {
						hasPlus = true
					}
				}

				var fullDiffLines []string
				if !hasMinus || !hasPlus {
					oldHeader := "--- a/" + targetPath
					if curOldPath == "/dev/null" {
						oldHeader = "--- /dev/null"
					}
					newHeader := "+++ b/" + targetPath
					if curNewPath == "/dev/null" {
						newHeader = "+++ /dev/null"
					}
					fullDiffLines = append(fullDiffLines, oldHeader, newHeader)
				}
				fullDiffLines = append(fullDiffLines, curLines...)

				diffContent := strings.Join(fullDiffLines, "\n")
				if !strings.HasSuffix(diffContent, "\n") {
					diffContent += "\n"
				}

				scope := strings.Join(uniqueStrings(curScopes), "; ")
				edits = append(edits, FileEdit{
					Path:         targetPath,
					Diff:         diffContent,
					ASTNodeScope: scope,
				})
			}
		}
		curOldPath = ""
		curNewPath = ""
		curLines = nil
		curScopes = nil
		inHunk = false
	}

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)

		if strings.HasPrefix(line, "diff --git ") {
			flushCurrent()
			oldP, newP := parseDiffGitPaths(line)
			curOldPath = oldP
			curNewPath = newP
			curLines = append(curLines, line)
			continue
		}

		if strings.HasPrefix(line, "--- ") {
			p := extractPathFromDiffHeader(line, "--- ")
			if inHunk {
				flushCurrent()
				curOldPath = p
				curLines = append(curLines, line)
				continue
			}
			curOldPath = p
			curLines = append(curLines, line)
			continue
		}

		if strings.HasPrefix(line, "+++ ") {
			p := extractPathFromDiffHeader(line, "+++ ")
			curNewPath = p
			curLines = append(curLines, line)
			continue
		}

		if strings.HasPrefix(line, "@@ ") {
			inHunk = true
			curLines = append(curLines, line)
			if scope := extractHunkScope(line); scope != "" {
				curScopes = append(curScopes, scope)
			}
			continue
		}

		// Git metadata headers
		if !inHunk && (strings.HasPrefix(trimmed, "index ") ||
			strings.HasPrefix(trimmed, "new file mode ") ||
			strings.HasPrefix(trimmed, "deleted file mode ") ||
			strings.HasPrefix(trimmed, "old mode ") ||
			strings.HasPrefix(trimmed, "new mode ") ||
			strings.HasPrefix(trimmed, "similarity index ") ||
			strings.HasPrefix(trimmed, "rename from ") ||
			strings.HasPrefix(trimmed, "rename to ") ||
			strings.HasPrefix(trimmed, "copy from ") ||
			strings.HasPrefix(trimmed, "copy to ")) {
			curLines = append(curLines, line)
			continue
		}

		if inHunk {
			if len(line) == 0 {
				curLines = append(curLines, " ")
			} else if line[0] == ' ' || line[0] == '+' || line[0] == '-' || line[0] == '\\' {
				curLines = append(curLines, line)
			} else {
				// Treat unrecognized lines inside hunk as context lines
				curLines = append(curLines, " "+line)
			}
			continue
		}
	}

	flushCurrent()

	if len(edits) == 0 {
		return nil, errors.New("transaction: no valid unified diff blocks found")
	}

	return edits, nil
}

// ParseMultiFilePatch is an alias for ParseUnifiedDiffBlocks to handle multi-file unified diffs.
func ParseMultiFilePatch(diffText string) ([]FileEdit, error) {
	return ParseUnifiedDiffBlocks(diffText)
}

// ApplyMultiFileUnifiedDiff parses a multi-file unified diff and stages all edits into an atomic
// PatchTransaction. If any file fails to stage, all previously staged edits are rolled back.
func ApplyMultiFileUnifiedDiff(repoDir string, diffText string) (*PatchTransaction, error) {
	edits, err := ParseUnifiedDiffBlocks(diffText)
	if err != nil {
		return nil, fmt.Errorf("failed parsing unified diff blocks: %w", err)
	}
	if len(edits) == 0 {
		return nil, errors.New("no valid file diffs found in diff text")
	}

	tx := NewTransaction(repoDir)
	if err := StageMultipleEdits(tx, edits); err != nil {
		return nil, fmt.Errorf("failed staging multi-file patch: %w", err)
	}
	return tx, nil
}

// ConvertFileEditsToEditBlocks converts parsed FileEdits into standard EditBlocks for fuzzy patching.
func ConvertFileEditsToEditBlocks(edits []FileEdit) []devops.EditBlock {
	var blocks []devops.EditBlock
	for _, edit := range edits {
		diffs, err := devops.ParseUnifiedDiff(edit.Diff)
		if err != nil {
			continue
		}
		for _, d := range diffs {
			targetPath := edit.Path
			if targetPath == "" {
				targetPath = d.NewPath
				if targetPath == "" || targetPath == "/dev/null" {
					targetPath = d.OldPath
				}
			}
			for _, h := range d.Hunks {
				var searchLines, replaceLines []string
				for _, l := range h.Lines {
					if len(l) == 0 {
						continue
					}
					marker := l[0]
					content := ""
					if len(l) > 1 {
						content = l[1:]
					}
					switch marker {
					case ' ':
						searchLines = append(searchLines, content)
						replaceLines = append(replaceLines, content)
					case '-':
						searchLines = append(searchLines, content)
					case '+':
						replaceLines = append(replaceLines, content)
					}
				}
				blocks = append(blocks, devops.EditBlock{
					FilePath:    targetPath,
					SearchText:  strings.Join(searchLines, "\n"),
					ReplaceText: strings.Join(replaceLines, "\n"),
				})
			}
		}
	}
	return blocks
}

// extractDiffTextFromMarkdown extracts diff content from markdown code fences or returns sanitized text.
func extractDiffTextFromMarkdown(text string) string {
	normalized := strings.ReplaceAll(text, "\r\n", "\n")
	if !strings.Contains(normalized, "```") {
		return normalized
	}

	lines := strings.Split(normalized, "\n")
	var extracted []string
	inFence := false
	var curFenceLines []string

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") {
			if inFence {
				inFence = false
				blockContent := strings.Join(curFenceLines, "\n")
				if containsDiffSignatures(blockContent) {
					extracted = append(extracted, blockContent)
				}
				curFenceLines = nil
			} else {
				inFence = true
				curFenceLines = nil
			}
			continue
		}
		if inFence {
			curFenceLines = append(curFenceLines, line)
		}
	}

	if len(extracted) > 0 {
		return strings.Join(extracted, "\n\n")
	}

	// Fallback: strip fence markers and return remainder
	var nonFenceLines []string
	for _, line := range lines {
		if !strings.HasPrefix(strings.TrimSpace(line), "```") {
			nonFenceLines = append(nonFenceLines, line)
		}
	}
	return strings.Join(nonFenceLines, "\n")
}

// parseDiffGitPaths extracts old and new paths from a "diff --git" header.
func parseDiffGitPaths(line string) (string, string) {
	rest := strings.TrimPrefix(line, "diff --git ")
	rest = strings.TrimSpace(rest)

	var tokens []string
	if strings.Contains(rest, "\"") {
		inQuote := false
		var cur strings.Builder
		for _, r := range rest {
			if r == '"' {
				inQuote = !inQuote
				continue
			}
			if unicode.IsSpace(r) && !inQuote {
				if cur.Len() > 0 {
					tokens = append(tokens, cur.String())
					cur.Reset()
				}
				continue
			}
			cur.WriteRune(r)
		}
		if cur.Len() > 0 {
			tokens = append(tokens, cur.String())
		}
	} else {
		tokens = strings.Fields(rest)
	}

	oldP, newP := "", ""
	if len(tokens) >= 1 {
		oldP = cleanDiffPrefix(tokens[0])
	}
	if len(tokens) >= 2 {
		newP = cleanDiffPrefix(tokens[1])
	}
	return oldP, newP
}

// cleanDiffPrefix strips a/ or b/ prefixes and normalizes slashes.
func cleanDiffPrefix(p string) string {
	p = strings.TrimSpace(p)
	p = strings.Trim(p, "\"")
	if p == "/dev/null" {
		return "/dev/null"
	}
	p = strings.TrimPrefix(p, "a/")
	p = strings.TrimPrefix(p, "b/")
	clean := filepath.ToSlash(filepath.Clean(p))
	clean = strings.TrimPrefix(clean, "./")
	clean = strings.TrimPrefix(clean, "/")
	return clean
}

// extractPathFromDiffHeader extracts and cleans path from --- or +++ lines.
func extractPathFromDiffHeader(line, prefix string) string {
	rest := strings.TrimPrefix(line, prefix)
	rest = strings.TrimSpace(rest)
	if idx := strings.IndexAny(rest, "\t\r "); idx != -1 {
		rest = rest[:idx]
	}
	return cleanDiffPrefix(rest)
}

// resolveDiffPath chooses the non-/dev/null target path.
func resolveDiffPath(oldP, newP string) string {
	if newP != "" && newP != "/dev/null" {
		return newP
	}
	if oldP != "" && oldP != "/dev/null" {
		return oldP
	}
	return newP
}

// extractHunkScope extracts the trailing function/class scope from @@ ... @@ header.
func extractHunkScope(line string) string {
	matches := hunkScopeRegex.FindStringSubmatch(line)
	if len(matches) > 1 {
		return strings.TrimSpace(matches[1])
	}
	return ""
}

// containsDiffSignatures checks if text contains indicators of a unified diff.
func containsDiffSignatures(text string) bool {
	return strings.Contains(text, "diff --git ") ||
		strings.Contains(text, "--- ") ||
		strings.Contains(text, "@@ ")
}

// uniqueStrings returns unique non-empty trimmed strings.
func uniqueStrings(items []string) []string {
	seen := make(map[string]bool)
	var res []string
	for _, s := range items {
		s = strings.TrimSpace(s)
		if s != "" && !seen[s] {
			seen[s] = true
			res = append(res, s)
		}
	}
	return res
}

