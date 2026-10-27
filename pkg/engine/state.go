// SPDX-License-Identifier: Apache-2.0
package engine

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/caleralabs/omnicoder-icx/pkg/canonical"
	"github.com/caleralabs/omnicoder-icx/pkg/devops"
	"github.com/caleralabs/omnicoder-icx/pkg/icx"
	"github.com/caleralabs/omnicoder-icx/pkg/llm"
	"github.com/caleralabs/omnicoder-icx/pkg/storage"
)

// EngineState encapsulates multi-turn state across an autonomous execution session.
type EngineState struct {
	WorkspaceDir    string               `json:"workspace_dir"`
	Task            string               `json:"task"`
	MaxTurns        int                  `json:"max_turns"`
	Unbounded       bool                 `json:"unbounded,omitempty"`
	MaxHealAttempts int                  `json:"max_heal_attempts"`
	CurrentTurn     int                  `json:"current_turn"`
	TotalHeals      int                  `json:"total_heals"`
	FilesTouched    map[string]bool      `json:"files_touched"`
	Commits         []string             `json:"commits"`
	MerkleLeaves    []string             `json:"merkle_leaves"`
	Milestones      []*icx.TurnMilestone `json:"milestones,omitempty"`
	LatticeSpaceID  string               `json:"lattice_space_id"`
	Status          string               `json:"status"`
	StartTime       time.Time            `json:"start_time"`
	EndTime         time.Time            `json:"end_time"`
	Turns           []*TurnState         `json:"turns"`
	Vault           *storage.Vault       `json:"-"`
	TargetSymbols   []string             `json:"target_symbols,omitempty"`
	Diagnostics     any                  `json:"diagnostics,omitempty"`
	PatchesProposed int                  `json:"patches_proposed,omitempty"`
	mu              sync.RWMutex
}

// TurnState models the execution state and artifacts of a single autonomous turn.
type TurnState struct {
	TurnNumber        int                              `json:"turn_number"`
	Task              string                           `json:"task"`
	FocusFiles        []string                         `json:"focus_files,omitempty"`
	TargetFiles       []string                         `json:"target_files,omitempty"`
	RepoMapSummary    string                           `json:"repo_map_summary,omitempty"`
	ASTNeighborhood   []string                         `json:"ast_neighborhood,omitempty"`
	ASTRibbon         string                           `json:"ast_ribbon,omitempty"`
	QuotedSlots       []string                         `json:"quoted_slots,omitempty"`
	Intent            *llm.IntentDecision              `json:"intent,omitempty"`
	Edits             []*llm.EditDecision              `json:"edits,omitempty"`
	Patches           []*devops.EditBlockResult        `json:"patches,omitempty"`
	ActuationReceipts []*icx.ActuationReceipt          `json:"actuation_receipts,omitempty"`
	CompilerReports   []*devops.CompilerFeedbackReport `json:"compiler_reports,omitempty"`
	HealDecisions     []*llm.HealDecision              `json:"heal_decisions,omitempty"`
	HealAttempts      int                              `json:"heal_attempts"`
	Finalize          *llm.FinalizeDecision            `json:"finalize,omitempty"`
	CommitHash        string                           `json:"commit_hash,omitempty"`
	TurnMerkleSeal    string                           `json:"turn_merkle_seal,omitempty"`
	ActivePlanPhase   *PlanPhase                       `json:"active_plan_phase,omitempty"`
	SandboxID         string                           `json:"sandbox_id,omitempty"`
	SandboxCheckpointID string                         `json:"sandbox_checkpoint_id,omitempty"`
	Success           bool                             `json:"success"`
	ErrorMessage      string                           `json:"error_message,omitempty"`
	StartTime         time.Time                        `json:"start_time"`
	EndTime           time.Time                        `json:"end_time"`
	CallerFiles          []string          `json:"caller_files,omitempty"`           // Files that call functions in FocusFiles (from icx_multihop_walk)
	TargetFunctionQuotes map[string]string `json:"target_function_quotes,omitempty"` // file -> quoted function body (from icx_quote_slot with range)
	TrajectoryRibbon     string            `json:"trajectory_ribbon,omitempty"`      // Recalled trajectory from prior turns (from icx_recall_scoped)
	Transaction          *PatchTransaction `json:"transaction,omitempty"`
	FileEdits            []FileEdit        `json:"file_edits,omitempty"`
	PreTurnSnapshots     map[string][]byte `json:"-"`
	TargetSymbols        []string          `json:"target_symbols,omitempty"`
	Diagnostics          any               `json:"diagnostics,omitempty"`
	ProposedPatchesCount int               `json:"proposed_patches_count,omitempty"`
}

// NewEngineState creates a new EngineState initialized with task and config defaults.
func NewEngineState(task string, cfg EngineConfig) *EngineState {
	ws := cfg.WorkspaceDir
	if ws == "" {
		ws = "."
	}
	unbounded := cfg.Unbounded || cfg.MaxTurns <= 0
	maxTurns := cfg.MaxTurns
	if maxTurns <= 0 && !unbounded {
		maxTurns = 10
	}
	maxHeals := cfg.MaxHealAttempts
	if maxHeals <= 0 {
		maxHeals = 6
	}

	spaceID := cfg.LatticeSpaceID
	if spaceID == "" {
		scope := detectProjectScope(task, nil, ws)
		if scope != "" {
			spaceID = "space_" + strings.ReplaceAll(filepath.Base(scope), "-", "_")
		} else if task != "" {
			spaceID = fmt.Sprintf("space_%s", canonical.SHA256Bytes([]byte(task))[:8])
		} else {
			spaceID = "default"
		}
	}

	return &EngineState{
		WorkspaceDir:    ws,
		Task:            task,
		MaxTurns:        maxTurns,
		Unbounded:       unbounded,
		MaxHealAttempts: maxHeals,
		CurrentTurn:     0,
		TotalHeals:      0,
		FilesTouched:    make(map[string]bool),
		Commits:         make([]string, 0),
		MerkleLeaves:    make([]string, 0),
		Milestones:      make([]*icx.TurnMilestone, 0),
		LatticeSpaceID:  spaceID,
		Status:          "INITIALIZED",
		StartTime:       time.Now(),
		Turns:           make([]*TurnState, 0),
		Vault:           cfg.Vault,
	}
}

// AddMilestone records a completed turn milestone into EngineState.
func (s *EngineState) AddMilestone(m *icx.TurnMilestone) {
	if m == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Milestones = append(s.Milestones, m)
	if s.Vault != nil {
		if mBytes, err := json.Marshal(m); err == nil {
			_ = s.Vault.SaveMilestone(s.Task, m.TurnNumber, mBytes)
		}
	}
}

// GetMilestones returns a snapshot copy of all recorded milestones.
func (s *EngineState) GetMilestones() []*icx.TurnMilestone {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*icx.TurnMilestone, len(s.Milestones))
	copy(out, s.Milestones)
	return out
}

// GetRecentMilestones returns the most recent n milestones.
func (s *EngineState) GetRecentMilestones(n int) []*icx.TurnMilestone {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if n <= 0 || len(s.Milestones) == 0 {
		return nil
	}
	start := len(s.Milestones) - n
	if start < 0 {
		start = 0
	}
	out := make([]*icx.TurnMilestone, len(s.Milestones[start:]))
	copy(out, s.Milestones[start:])
	return out
}

// GetMilestone retrieves the milestone for a specific turn number (1-indexed).
func (s *EngineState) GetMilestone(turnNum int) *icx.TurnMilestone {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, m := range s.Milestones {
		if m != nil && m.TurnNumber == turnNum {
			return m
		}
	}
	return nil
}

// BeginTurn initializes a new TurnState for the next turn iteration.
func (s *EngineState) BeginTurn(task string, focusFiles []string) *TurnState {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.CurrentTurn++
	turn := &TurnState{
		TurnNumber:  s.CurrentTurn,
		Task:        task,
		FocusFiles:  focusFiles,
		StartTime:   time.Now(),
		TargetFiles:       make([]string, 0),
		QuotedSlots:          make([]string, 0),
		TargetFunctionQuotes: make(map[string]string),
		Edits:                make([]*llm.EditDecision, 0),
		Patches:           make([]*devops.EditBlockResult, 0),
		ActuationReceipts: make([]*icx.ActuationReceipt, 0),
		PreTurnSnapshots:  make(map[string][]byte),
	}
	s.Turns = append(s.Turns, turn)
	s.Status = "RUNNING"
	return turn
}

// CompleteTurn finalizes the TurnState, recording end time and appending turn seal to leaves.
func (s *EngineState) CompleteTurn(turn *TurnState) {
	s.mu.Lock()
	defer s.mu.Unlock()

	turn.EndTime = time.Now()
	if turn.TurnMerkleSeal != "" {
		s.MerkleLeaves = append(s.MerkleLeaves, turn.TurnMerkleSeal)
	}

	if s.Vault != nil && turn != nil {
		_ = storage.SaveTurnState(s.Vault, s.Task, turn.TurnNumber, turn)
	}
}

// Close closes the attached Vault if present.
func (s *EngineState) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Vault != nil {
		return s.Vault.Close()
	}
	return nil
}

// RecordTouch tracks a modified file path.
func (s *EngineState) RecordTouch(filePath string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.FilesTouched[filePath] = true
}

// RecordCommit records a new Git commit hash.
func (s *EngineState) RecordCommit(hash string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Commits = append(s.Commits, hash)
}

// AddMerkleLeaf registers a leaf hash for tree aggregation.
func (s *EngineState) AddMerkleLeaf(leaf string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if leaf != "" {
		s.MerkleLeaves = append(s.MerkleLeaves, leaf)
	}
}

// ComputeMerkleSeal calculates the deterministic SHA-256 Merkle root across all leaves.
func (s *EngineState) ComputeMerkleSeal() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return canonical.MerkleRoot(s.MerkleLeaves)
}

// TouchedList returns a sorted slice of touched file paths.
func (s *EngineState) TouchedList() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.touchedListLocked()
}

func (s *EngineState) touchedListLocked() []string {
	files := make([]string, 0, len(s.FilesTouched))
	for f := range s.FilesTouched {
		files = append(files, f)
	}
	sort.Strings(files)
	return files
}

// BuildReport constructs the final RunReport.
func (s *EngineState) BuildReport(success bool, finalOutput string) *RunReport {
	s.mu.RLock()
	defer s.mu.RUnlock()

	errMsg := ""
	if !success {
		errMsg = finalOutput
	}

	return &RunReport{
		Success:         success,
		Task:            s.Task,
		TurnsCount:      s.CurrentTurn,
		HealCount:       s.TotalHeals,
		FilesTouched:    s.touchedListLocked(),
		Commits:         append([]string(nil), s.Commits...),
		MerkleSeal:      canonical.MerkleRoot(s.MerkleLeaves),
		FinalOutput:     finalOutput,
		Duration:        time.Since(s.StartTime),
		ErrorMessage:    errMsg,
		TargetSymbols:   append([]string(nil), s.TargetSymbols...),
		Diagnostics:     s.Diagnostics,
		PatchesProposed: s.PatchesProposed,
		ExecutionMS:     time.Since(s.StartTime).Milliseconds(),
	}
}
