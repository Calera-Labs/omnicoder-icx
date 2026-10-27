// SPDX-License-Identifier: Apache-2.0
// Package engine provides core execution pipeline, turn management, and transaction primitives.
// transaction.go implements the Multi-File Transactional Patch Engine:
// - Two-phase commit semantics for multi-file patches
// - Atomic staging and rollback harnesses for isolated candidate evaluations
// - SHA-256 Merkle audit seals for cryptographically attested changes
// - Zero floating-point drift and deterministic cross-file rollbacks

package engine

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/caleralabs/omnicoder-icx/pkg/canonical"
	"github.com/caleralabs/omnicoder-icx/pkg/devops"
)

// TransactionStatus models the finite lifecycle states of a patch transaction.
type TransactionStatus int

const (
	TxPending TransactionStatus = iota
	TxStaged
	TxPreHealed
	TxVerified
	TxCommitted
	TxAborted
)

// String returns the canonical uppercase status name.
func (s TransactionStatus) String() string {
	switch s {
	case TxPending:
		return "PENDING"
	case TxStaged:
		return "STAGED"
	case TxPreHealed:
		return "PRE_HEALED"
	case TxVerified:
		return "VERIFIED"
	case TxCommitted:
		return "COMMITTED"
	case TxAborted:
		return "ABORTED"
	default:
		return fmt.Sprintf("UNKNOWN(%d)", int(s))
	}
}

// Standard transaction errors.
var (
	ErrNilTransaction         = errors.New("transaction: nil transaction")
	ErrTransactionCommitted    = errors.New("transaction: cannot modify committed transaction")
	ErrTransactionAborted      = errors.New("transaction: cannot modify aborted transaction")
	ErrEmptyFilePath           = errors.New("transaction: file path cannot be empty")
	ErrEmptyDiff               = errors.New("transaction: diff cannot be empty")
	ErrIllegalPathTraversal    = errors.New("transaction: illegal path traversal outside repository")
	ErrPatchApplicationFailed = errors.New("transaction: patch application failed")
)

// FileEdit represents an atomic edit target within a multi-file transaction.
type FileEdit struct {
	Path         string `json:"path"`
	OriginalHash string `json:"original_hash,omitempty"`
	PatchedHash  string `json:"patched_hash,omitempty"`
	Diff         string `json:"diff"`
	ASTNodeScope string `json:"ast_node_scope,omitempty"`
}

// PatchTransaction encapsulates a multi-file atomic transaction with two-phase commit semantics.
type PatchTransaction struct {
	ID               string            `json:"id"`
	RepoDir          string            `json:"repo_dir"`
	Edits            []FileEdit        `json:"edits"`
	BranchName       string            `json:"branch_name,omitempty"`
	Status           TransactionStatus `json:"status"`
	RollbackFn       func() error      `json:"-"`
	OriginalContents map[string][]byte `json:"-"` // clean relative path -> pre-tx snapshot (nil if file was created)
	StagedContents   map[string][]byte `json:"-"` // clean relative path -> staged content
	CreatedAt        time.Time         `json:"created_at"`
	CommittedAt      *time.Time        `json:"committed_at,omitempty"`
	MerkleSeal       string            `json:"merkle_seal,omitempty"`
	mu               sync.Mutex        `json:"-"`
}

// randUint32 generates a cryptographically secure random uint32.
func randUint32() uint32 {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return uint32(time.Now().UnixNano() & 0xffffffff)
	}
	return binary.LittleEndian.Uint32(b[:])
}

// NewTransaction initializes a new PatchTransaction bound to repoDir in TxPending status.
func NewTransaction(repoDir string) *PatchTransaction {
	if repoDir == "" {
		repoDir = "."
	}
	cleanRepo := filepath.Clean(repoDir)
	id := fmt.Sprintf("tx_%d_%08x", time.Now().UnixNano(), randUint32())
	tx := &PatchTransaction{
		ID:               id,
		RepoDir:          cleanRepo,
		Edits:            make([]FileEdit, 0),
		Status:           TxPending,
		OriginalContents: make(map[string][]byte),
		StagedContents:   make(map[string][]byte),
		CreatedAt:        time.Now(),
	}
	tx.RollbackFn = func() error {
		return RollbackTransaction(tx)
	}
	return tx
}

// StageFileEdit stages a single file modification into the transaction.
// The file's pre-transaction content is snapshotted for instant rollback,
// the diff is validated and applied to disk/staging, and the transaction transitions to TxStaged.
func StageFileEdit(tx *PatchTransaction, edit FileEdit) error {
	if tx == nil {
		return ErrNilTransaction
	}

	tx.mu.Lock()
	defer tx.mu.Unlock()

	if tx.Status == TxCommitted {
		return ErrTransactionCommitted
	}
	if tx.Status == TxAborted {
		return ErrTransactionAborted
	}

	rawPath := strings.TrimSpace(edit.Path)
	if rawPath == "" {
		return ErrEmptyFilePath
	}

	cleanPath := filepath.ToSlash(filepath.Clean(rawPath))
	cleanPath = strings.TrimPrefix(cleanPath, "./")
	cleanPath = strings.TrimPrefix(cleanPath, "/")

	if cleanPath == ".." || strings.HasPrefix(cleanPath, "../") {
		return fmt.Errorf("%w: %s", ErrIllegalPathTraversal, edit.Path)
	}

	fullPath := filepath.Join(tx.RepoDir, filepath.FromSlash(cleanPath))

	// Snapshot original content before ANY edits occur in this transaction
	if _, recorded := tx.OriginalContents[cleanPath]; !recorded {
		if data, err := os.ReadFile(fullPath); err == nil {
			tx.OriginalContents[cleanPath] = data
			if edit.OriginalHash == "" {
				edit.OriginalHash = canonical.SHA256Bytes(data)
			}
		} else {
			// File did not exist on disk prior to transaction
			tx.OriginalContents[cleanPath] = nil
			if edit.OriginalHash == "" {
				edit.OriginalHash = ""
			}
		}
	}

	// Base content to patch: use latest staged content if already touched, otherwise original content
	var baseContent string
	if staged, exists := tx.StagedContents[cleanPath]; exists {
		baseContent = string(staged)
	} else if orig := tx.OriginalContents[cleanPath]; orig != nil {
		baseContent = string(orig)
	} else {
		baseContent = ""
	}

	if strings.TrimSpace(edit.Diff) == "" {
		return ErrEmptyDiff
	}

	var patchedText string
	var applyErr error

	// Determine diff application strategy
	diffTrimmed := strings.TrimSpace(edit.Diff)
	if strings.Contains(diffTrimmed, "@@ ") || strings.HasPrefix(diffTrimmed, "diff --git ") || strings.HasPrefix(diffTrimmed, "--- ") {
		// Unified diff format
		patchedText, _, _, applyErr = devops.ApplyPatchToText(baseContent, edit.Diff)
		if applyErr != nil {
			// Resilient fuzzy fallback
			if fuzzyPatched, fuzzyErr := applyUnifiedDiffFuzzy(baseContent, edit.Diff); fuzzyErr == nil {
				patchedText = fuzzyPatched
				applyErr = nil
			}
		}
	} else if strings.Contains(diffTrimmed, "<<<<<<< SEARCH") {
		// Aider search/replace block
		blocks, parseErr := devops.ParseAiderEditBlocks(edit.Diff, cleanPath)
		if parseErr == nil && len(blocks) > 0 {
			resText, modErr := applyEditBlocksToMemory(baseContent, blocks)
			if modErr == nil {
				patchedText = resText
				applyErr = nil
			} else {
				applyErr = fmt.Errorf("failed applying aider blocks: %w", modErr)
			}
		} else {
			applyErr = fmt.Errorf("failed parsing aider blocks: %w", parseErr)
		}
	} else {
		// Raw replacement text
		patchedText = edit.Diff
	}

	if applyErr != nil {
		return fmt.Errorf("%w for %s: %v", ErrPatchApplicationFailed, cleanPath, applyErr)
	}

	patchedBytes := []byte(patchedText)
	edit.PatchedHash = canonical.SHA256Bytes(patchedBytes)
	edit.Path = cleanPath

	tx.StagedContents[cleanPath] = patchedBytes

	// Write staged file to workspace
	if err := os.MkdirAll(filepath.Dir(fullPath), 0755); err != nil {
		return fmt.Errorf("transaction: failed creating directory for %s: %w", cleanPath, err)
	}
	if err := os.WriteFile(fullPath, patchedBytes, 0644); err != nil {
		return fmt.Errorf("transaction: failed writing staged file %s: %w", cleanPath, err)
	}

	tx.Edits = append(tx.Edits, edit)
	tx.Status = TxStaged
	tx.RollbackFn = func() error {
		return RollbackTransaction(tx)
	}

	return nil
}

// StageMultipleEdits stages multiple file edits in sequence with atomic rollback on any failure.
func StageMultipleEdits(tx *PatchTransaction, edits []FileEdit) error {
	if tx == nil {
		return ErrNilTransaction
	}
	for i, edit := range edits {
		if err := StageFileEdit(tx, edit); err != nil {
			_ = RollbackTransaction(tx)
			return fmt.Errorf("transaction: staging failed at edit %d/%d (%s): %w (rolled back all changes)", i+1, len(edits), edit.Path, err)
		}
	}
	return nil
}

// CommitTransaction finalizes the transaction, verifies all staged files match on disk,
// computes the RFC 8785 Canonical Merkle Seal, and transitions to TxCommitted.
func CommitTransaction(tx *PatchTransaction) error {
	if tx == nil {
		return ErrNilTransaction
	}

	tx.mu.Lock()
	defer tx.mu.Unlock()

	if tx.Status == TxCommitted {
		return nil // idempotent
	}
	if tx.Status == TxAborted {
		return ErrTransactionAborted
	}

	// Verify all staged files on disk match their recorded PatchedHash
	for _, edit := range tx.Edits {
		fullPath := filepath.Join(tx.RepoDir, filepath.FromSlash(edit.Path))
		data, err := os.ReadFile(fullPath)
		if err != nil {
			return fmt.Errorf("transaction: verification failed for %s: %w", edit.Path, err)
		}
		hash := canonical.SHA256Bytes(data)
		if edit.PatchedHash != "" && hash != edit.PatchedHash {
			return fmt.Errorf("transaction: hash verification mismatch for %s: expected %s, got %s", edit.Path, edit.PatchedHash, hash)
		}
	}

	editsSummary := make([]map[string]any, len(tx.Edits))
	for i, e := range tx.Edits {
		editsSummary[i] = map[string]any{
			"path":          e.Path,
			"original_hash": e.OriginalHash,
			"patched_hash":  e.PatchedHash,
			"ast_scope":     e.ASTNodeScope,
		}
	}

	now := time.Now()
	tx.MerkleSeal = canonical.SHA256Of(map[string]any{
		"tx_id":        tx.ID,
		"repo_dir":     filepath.ToSlash(tx.RepoDir),
		"edits_count": len(tx.Edits),
		"edits":        editsSummary,
		"committed_at": now.UTC().Format(time.RFC3339Nano),
	})

	tx.Status = TxCommitted
	tx.CommittedAt = &now
	tx.RollbackFn = nil

	return nil
}

// RollbackTransaction reverts all staged modifications, restoring original files or removing
// newly created files, and transitions the transaction to TxAborted.
func RollbackTransaction(tx *PatchTransaction) error {
	if tx == nil {
		return ErrNilTransaction
	}

	tx.mu.Lock()
	defer tx.mu.Unlock()

	if tx.Status == TxAborted {
		return nil // idempotent
	}
	if tx.Status == TxCommitted {
		return ErrTransactionCommitted
	}

	var rollbackErrors []string
	for relPath, original := range tx.OriginalContents {
		fullPath := filepath.Join(tx.RepoDir, filepath.FromSlash(relPath))
		if original == nil {
			// File was created during transaction, remove it
			if err := os.Remove(fullPath); err != nil && !os.IsNotExist(err) {
				rollbackErrors = append(rollbackErrors, fmt.Sprintf("failed removing %s: %v", relPath, err))
			}
		} else {
			// Restore original content
			if err := os.MkdirAll(filepath.Dir(fullPath), 0755); err != nil {
				rollbackErrors = append(rollbackErrors, fmt.Sprintf("failed creating dir for %s: %v", relPath, err))
				continue
			}
			if err := os.WriteFile(fullPath, original, 0644); err != nil {
				rollbackErrors = append(rollbackErrors, fmt.Sprintf("failed restoring %s: %v", relPath, err))
			}
		}
	}

	tx.Status = TxAborted
	tx.StagedContents = make(map[string][]byte)

	if len(rollbackErrors) > 0 {
		return fmt.Errorf("transaction: rollback errors: %s", strings.Join(rollbackErrors, "; "))
	}
	return nil
}

// TouchFiles returns a list of unique relative file paths modified in this transaction.
func (tx *PatchTransaction) TouchFiles() []string {
	if tx == nil {
		return nil
	}
	tx.mu.Lock()
	defer tx.mu.Unlock()

	seen := make(map[string]bool)
	var paths []string
	for _, e := range tx.Edits {
		if !seen[e.Path] {
			seen[e.Path] = true
			paths = append(paths, e.Path)
		}
	}
	return paths
}

// applyUnifiedDiffFuzzy converts diff hunks into search/replace blocks and applies them.
func applyUnifiedDiffFuzzy(originalText string, diffText string) (string, error) {
	diffs, err := devops.ParseUnifiedDiff(diffText)
	if err != nil || len(diffs) == 0 {
		return "", fmt.Errorf("fuzzy diff parse error: %w", err)
	}

	current := originalText
	for _, fd := range diffs {
		for _, hunk := range fd.Hunks {
			var searchLines, replaceLines []string
			for _, l := range hunk.Lines {
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

			if len(searchLines) == 0 {
				// Pure addition, append or prepend
				current = current + "\n" + strings.Join(replaceLines, "\n")
				continue
			}

			searchBlock := strings.Join(searchLines, "\n")
			replaceBlock := strings.Join(replaceLines, "\n")

			if strings.Contains(current, searchBlock) {
				current = strings.Replace(current, searchBlock, replaceBlock, 1)
			} else {
				// Try normalized search
				curNorm := strings.ReplaceAll(current, "\r\n", "\n")
				searchNorm := strings.ReplaceAll(searchBlock, "\r\n", "\n")
				if strings.Contains(curNorm, searchNorm) {
					current = strings.Replace(curNorm, searchNorm, replaceBlock, 1)
				} else {
					return "", fmt.Errorf("fuzzy context match failed for hunk @@ -%d,%d +%d,%d @@",
						hunk.OldStart, hunk.OldCount, hunk.NewStart, hunk.NewCount)
				}
			}
		}
	}
	return current, nil
}

// applyEditBlocksToMemory applies Aider EditBlocks in-memory to text.
func applyEditBlocksToMemory(text string, blocks []devops.EditBlock) (string, error) {
	current := text
	for _, b := range blocks {
		search := strings.TrimRight(b.SearchText, "\r\n")
		replace := strings.TrimRight(b.ReplaceText, "\r\n")

		if search == "" {
			current = current + "\n" + replace
			continue
		}

		if strings.Contains(current, search) {
			current = strings.Replace(current, search, replace, 1)
			continue
		}

		// Normalize line endings
		curNorm := strings.ReplaceAll(current, "\r\n", "\n")
		searchNorm := strings.ReplaceAll(search, "\r\n", "\n")
		if strings.Contains(curNorm, searchNorm) {
			current = strings.Replace(curNorm, searchNorm, replace, 1)
			continue
		}

		return "", fmt.Errorf("could not find search block in file content")
	}
	return current, nil
}
