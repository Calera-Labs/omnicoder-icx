// SPDX-License-Identifier: Apache-2.0
package engine_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/caleralabs/omnicoder-icx/pkg/canonical"
	"github.com/caleralabs/omnicoder-icx/pkg/icx"
	"github.com/caleralabs/omnicoder-icx/pkg/llm"
)

// TestHighConcurrency_Race_LatticeSlotsReceipts stresses the LocalMemoryCache
// under extreme concurrent load (150 goroutines) performing slots quoting,
// actuation receipt crystallization, swarm variable updates with TTL, ribbon recall,
// and Merkle tree root computations simultaneously.
func TestHighConcurrency_Race_LatticeSlotsReceipts(t *testing.T) {
	ctx := context.Background()
	lat := icx.NewLocalLattice()

	// Seed some initial AST nodes for ribbon queries
	for i := 0; i < 20; i++ {
		nodeID := fmt.Sprintf("node_%03d", i)
		lat.AddASTNode(&icx.LatticeNode{
			ID:        nodeID,
			Name:      fmt.Sprintf("Symbol_%d", i),
			Type:      "function",
			Dimension: "3D_VOLUMETRIC",
		})
		if i > 0 {
			lat.AddASTEdge(fmt.Sprintf("node_%03d", i-1), "calls", nodeID)
		}
	}

	// Seed initial quote slots in "file_slots" family
	const seededSlots = 10
	for i := 1; i <= seededSlots; i++ {
		_, err := lat.Remember(ctx, icx.RememberRequest{
			Family:   "file_slots",
			SourceID: fmt.Sprintf("file_%d.go:1-10", i),
			Text:     fmt.Sprintf("package seeded\nfunc Seed%d() int { return %d }\n", i, i),
		})
		if err != nil {
			t.Fatalf("failed seeding slot %d: %v", i, err)
		}
	}

	const numWorkersPerGroup = 25
	const iterations = 30

	var wg sync.WaitGroup
	var errCount int64

	// Group 1: Concurrent Slot Remembers & Quotes
	for g := 0; g < numWorkersPerGroup; g++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			for it := 0; it < iterations; it++ {
				// Quote existing slots
				targetIdx := (it % seededSlots) + 1
				quoteRes, quoteErr := lat.QuoteSlot(ctx, icx.QuoteSlotRequest{
					Family: "file_slots",
					Index:  targetIdx,
				})
				if quoteErr != nil || quoteRes == nil || !quoteRes.Verified || quoteRes.SHA256 == "" {
					atomic.AddInt64(&errCount, 1)
				}

				// Append new slot
				text := fmt.Sprintf("slot_content_worker_%d_iter_%d", workerID, it)
				_, err := lat.Remember(ctx, icx.RememberRequest{
					Family:   "file_slots",
					SourceID: fmt.Sprintf("worker_%d.go:%d", workerID, it),
					Text:     text,
				})
				if err != nil {
					atomic.AddInt64(&errCount, 1)
				}
			}
		}(g)
	}

	// Group 2: Concurrent Actuation Receipt Dispatches & Reads
	for g := 0; g < numWorkersPerGroup; g++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			for it := 0; it < iterations; it++ {
				receipt := icx.ActuationReceipt{
					FileModified:      fmt.Sprintf("worker_%d_file.go", workerID),
					ExitCode:          it % 2,
					DiagnosticMessage: fmt.Sprintf("diag_%d_%d", workerID, it),
					RepairDiffSummary: fmt.Sprintf("diff_%d_%d", workerID, it),
					SHA256Seal:        canonical.SHA256Of(fmt.Sprintf("receipt_%d_%d", workerID, it)),
					TurnNumber:        it,
					Surface:           "concurrency_stress",
					Action:            "concurrent_actuation",
				}

				req := icx.DispatchActuationRequest{
					Surface: "concurrency_stress",
					Action:  "concurrent_actuation",
					Payload: receipt.ToPayload(),
				}

				res, err := lat.DispatchActuation(ctx, req)
				if err != nil || res == nil || !res.Success {
					atomic.AddInt64(&errCount, 1)
				}

				compact := receipt.FormatCompact()
				if llm.EstimateTokens(compact) >= 170 {
					atomic.AddInt64(&errCount, 1)
				}
			}
		}(g)
	}

	// Group 3: Swarm Variable Set with TTL & Get
	for g := 0; g < numWorkersPerGroup; g++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			for it := 0; it < iterations; it++ {
				key := fmt.Sprintf("swarm.worker_%d.var_%d", workerID, it%5)
				val := fmt.Sprintf("val_%d_%d", workerID, it)
				ttl := (rand.Intn(100) + 50) // 50-150ms TTL

				res, err := lat.VarSet(ctx, icx.VarSetRequest{
					Key:    key,
					Value:  val,
					Writer: fmt.Sprintf("worker_%d", workerID),
					TTLMs:  ttl,
				})
				if err != nil || res == nil || !res.OK {
					atomic.AddInt64(&errCount, 1)
					continue
				}

				getRes, getErr := lat.VarGet(ctx, icx.VarGetRequest{Key: key})
				if getErr != nil || getRes == nil || !getRes.OK {
					// Might have expired if delayed under race detector, ignore expiration
					continue
				}
				if getRes.Value == nil {
					atomic.AddInt64(&errCount, 1)
				}
			}
		}(g)
	}

	// Group 4: Swarm Variable Prefix Listing & ResetSession durability
	for g := 0; g < numWorkersPerGroup; g++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			for it := 0; it < iterations; it++ {
				listRes, err := lat.VarList(ctx, icx.VarListRequest{
					Prefix: "swarm.",
				})
				if err != nil || listRes == nil || !listRes.OK {
					atomic.AddInt64(&errCount, 1)
				}

				if it == iterations/2 && workerID == 0 {
					// Occasional session reset to challenge durability of receipts
					_, _ = lat.ResetSession(ctx, "")
				}
			}
		}(g)
	}

	// Group 5: Ribbon Recall & Merkle Root Calculations
	for g := 0; g < numWorkersPerGroup; g++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			for it := 0; it < iterations; it++ {
				recRes, err := lat.RecallScoped(ctx, icx.RecallRequest{
					Query:  "Symbol",
					Format: "ribbon",
				})
				if err != nil || recRes == nil {
					atomic.AddInt64(&errCount, 1)
					continue
				}
				if recRes.Ribbon == "" || !strings.HasPrefix(recRes.Ribbon, "<nav:ribbon:") {
					atomic.AddInt64(&errCount, 1)
				}

				// Concurrent Merkle root calculations on random leaf sets
				leafCount := (workerID % 15) + 1
				leaves := make([]string, leafCount)
				for l := 0; l < leafCount; l++ {
					leaves[l] = canonical.SHA256Of(fmt.Sprintf("leaf_%d_%d_%d", workerID, it, l))
				}
				root := canonical.MerkleRoot(leaves)
				if len(root) != 64 {
					atomic.AddInt64(&errCount, 1)
				}
			}
		}(g)
	}

	wg.Wait()

	if errs := atomic.LoadInt64(&errCount); errs > 0 {
		t.Fatalf("encountered %d errors during high concurrency race stress", errs)
	}

	// Final verification: receipts are still present and queryable
	finalReceipts := lat.GetActuationReceipts("")
	if len(finalReceipts) == 0 {
		t.Fatalf("expected crystallized receipts to survive, found 0")
	}
	t.Logf("PASS: High-concurrency race stress completed cleanly with %d receipts crystallized and 0 errors", len(finalReceipts))
}

// computeBinaryMerkleProof calculates the inclusion sibling hashes for leaf at index leafIdx.
func computeBinaryMerkleProof(leafHashes []string, leafIdx int) ([]string, error) {
	if leafIdx < 0 || leafIdx >= len(leafHashes) {
		return nil, fmt.Errorf("leafIdx %d out of bounds for %d leaves", leafIdx, len(leafHashes))
	}

	var proof []string
	layer := make([]string, len(leafHashes))
	copy(layer, leafHashes)
	idx := leafIdx

	for len(layer) > 1 {
		var sibling string
		if idx%2 == 0 {
			// Sibling is to the right
			if idx+1 < len(layer) {
				sibling = layer[idx+1]
			} else {
				sibling = layer[idx] // duplicate odd node
			}
		} else {
			// Sibling is to the left
			sibling = layer[idx-1]
		}
		proof = append(proof, sibling)

		// Next layer reduction
		next := make([]string, 0, (len(layer)+1)/2)
		for i := 0; i < len(layer); i += 2 {
			left := layer[i]
			right := left
			if i+1 < len(layer) {
				right = layer[i+1]
			}
			sum := sha256.Sum256([]byte(left + right))
			next = append(next, hex.EncodeToString(sum[:]))
		}
		idx = idx / 2
		layer = next
	}

	return proof, nil
}

// verifyProofPath verifies a leaf hash against a root using sibling hashes and the index path.
func verifyProofPath(leafHash string, leafIdx int, proof []string, expectedRoot string) bool {
	curr := leafHash
	idx := leafIdx
	for _, sibling := range proof {
		var combined string
		if idx%2 == 0 {
			combined = curr + sibling
		} else {
			combined = sibling + curr
		}
		sum := sha256.Sum256([]byte(combined))
		curr = hex.EncodeToString(sum[:])
		idx = idx / 2
	}
	return curr == expectedRoot
}

// TestAdversarialMerkleInclusion_ArbitraryDepthsAndTamper
// thoroughly stress-tests Merkle inclusion proof paths and tamper detection
// across diverse leaf set cardinalities (1, 2, 3, 4, 7, 8, 15, 16, 25, 32, 63, 64, 100).
func TestAdversarialMerkleInclusion_ArbitraryDepthsAndTamper(t *testing.T) {
	testSizes := []int{1, 2, 3, 4, 5, 7, 8, 9, 15, 16, 25, 31, 32, 63, 64, 100}

	for _, count := range testSizes {
		leaves := make([]string, count)
		for i := 0; i < count; i++ {
			leaves[i] = canonical.SHA256Of(fmt.Sprintf("dataset_leaf_%d_of_%d", i, count))
		}

		expectedRoot := canonical.MerkleRoot(leaves)
		if len(expectedRoot) != 64 {
			t.Fatalf("invalid root length for count=%d: %s", count, expectedRoot)
		}

		// Verify inclusion proof for every single leaf in the set
		for idx := 0; idx < count; idx++ {
			proof, err := computeBinaryMerkleProof(leaves, idx)
			if err != nil {
				t.Fatalf("failed computing proof for count=%d idx=%d: %v", count, idx, err)
			}

			// 1. Verify valid proof path
			if !verifyProofPath(leaves[idx], idx, proof, expectedRoot) {
				t.Fatalf("inclusion proof failed for count=%d leafIdx=%d", count, idx)
			}

			// 2. Tamper leaf hash: verify proof rejection
			tamperedLeaf := canonical.SHA256Of(fmt.Sprintf("tampered_leaf_%d", idx))
			if verifyProofPath(tamperedLeaf, idx, proof, expectedRoot) {
				t.Fatalf("SECURITY VIOLATION: Tampered leaf hash accepted by proof for count=%d idx=%d", count, idx)
			}

			// 3. Tamper proof sibling: verify proof rejection if any sibling is modified
			if len(proof) > 0 {
				tamperedProof := make([]string, len(proof))
				copy(tamperedProof, proof)
				tamperedProof[0] = canonical.SHA256Of("tampered_sibling")
				if verifyProofPath(leaves[idx], idx, tamperedProof, expectedRoot) {
					t.Fatalf("SECURITY VIOLATION: Tampered proof sibling accepted for count=%d idx=%d", count, idx)
				}
			}
		}
	}
	t.Logf("PASS: Verified mathematical inclusion proofs and tamper resistance across %d tree configurations", len(testSizes))
}

// TestDynamicPortIsolation_MultiCommand verifies that running multiple
// subcommands (version, --help, map --offline, repl --offline) never opens any listening TCP ports.
func TestDynamicPortIsolation_MultiCommand(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("skipping Windows Get-NetTCPConnection hygiene test on non-windows platform")
	}

	binCandidates := []string{
		"../../omnicoder.exe",
		"../omnicoder.exe",
		"omnicoder.exe",
		filepath.Join(os.Getenv("OMNICODER_WORKSPACE"), "omnicoder.exe"),
	}
	var binPath string
	for _, c := range binCandidates {
		if c == "" {
			continue
		}
		if abs, err := filepath.Abs(c); err == nil {
			if fi, err := os.Stat(abs); err == nil && !fi.IsDir() {
				binPath = abs
				break
			}
		}
	}
	if binPath == "" {
		t.Skip("omnicoder.exe binary not found; skipping dynamic binary hygiene test")
	}

	subcommands := [][]string{
		{"version"},
		{"--help"},
		{"repl", "--offline"},
		{"chat", "--offline"},
	}

	for _, sub := range subcommands {
		t.Run(strings.Join(sub, "_"), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			cmd := exec.CommandContext(ctx, binPath, sub...)
			cmd.Stdin = strings.NewReader("/exit\nexit\nquit\n")
			if err := cmd.Start(); err != nil {
				t.Fatalf("failed starting %v: %v", sub, err)
			}
			pid := cmd.Process.Pid

			// Inspect listening ports via PowerShell
			psScript := fmt.Sprintf(`Get-NetTCPConnection -OwningProcess %d -ErrorAction SilentlyContinue | Where-Object { $_.State -eq 'Listen' } | Measure-Object | Select-Object -ExpandProperty Count`, pid)
			psCmd := exec.Command("powershell", "-NoProfile", "-Command", psScript)
			out, _ := psCmd.CombinedOutput()
			countStr := strings.TrimSpace(string(out))

			_ = cmd.Wait()

			if countStr != "" && countStr != "0" {
				t.Fatalf("Dynamic port isolation failure: %v opened %s listening ports", sub, countStr)
			}
			t.Logf("PASS: %v (PID %d) listening ports: %q", sub, pid, countStr)
		})
	}
}
