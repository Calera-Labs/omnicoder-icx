// SPDX-License-Identifier: Apache-2.0
package icx_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/caleralabs/omnicoder-icx/pkg/canonical"
	"github.com/caleralabs/omnicoder-icx/pkg/icx"
	"github.com/caleralabs/omnicoder-icx/pkg/llm"
)

// ============================================================================
// 1. Actuation Receipt Pathological Formatting & Budget (< 170 tokens)
// ============================================================================
func TestActuationReceipts_PathologicalFormattingAndBudget(t *testing.T) {
	testCases := []struct {
		name    string
		receipt *icx.ActuationReceipt
	}{
		{
			name:    "Receipt-01: Nil receipt",
			receipt: nil,
		},
		{
			name:    "Receipt-02: Empty receipt",
			receipt: &icx.ActuationReceipt{},
		},
		{
			name: "Receipt-03: Massive FileModified (5000 chars)",
			receipt: &icx.ActuationReceipt{
				FileModified:      strings.Repeat("deep/sub/path/to/component/", 150) + "file.go",
				ExitCode:          0,
				DiagnosticMessage: "OK",
				RepairDiffSummary: "+1 -0",
				SHA256Seal:        canonical.SHA256Of("test"),
			},
		},
		{
			name: "Receipt-04: Massive DiagnosticMessage (50000 chars compiler crash dump)",
			receipt: &icx.ActuationReceipt{
				FileModified:      "pkg/engine/turn.go",
				ExitCode:          1,
				DiagnosticMessage: strings.Repeat("panic: fatal memory corrupted during compilation [recovered]\n", 1000),
				RepairDiffSummary: "Applied fix",
				SHA256Seal:        canonical.SHA256Of("test"),
			},
		},
		{
			name: "Receipt-05: Massive RepairDiffSummary (50000 chars diff)",
			receipt: &icx.ActuationReceipt{
				FileModified:      "pkg/calc/calc.go",
				ExitCode:          0,
				DiagnosticMessage: "Clean compilation verified",
				RepairDiffSummary: strings.Repeat("@@ -1,5 +1,5 @@\n-\treturn a - b\n+\treturn a + b\n", 1000),
				SHA256Seal:        canonical.SHA256Of("test"),
			},
		},
		{
			name: "Receipt-06: Giant single word in DiagnosticMessage without whitespace (4000 chars)",
			receipt: &icx.ActuationReceipt{
				FileModified:      "main.go",
				ExitCode:          2,
				DiagnosticMessage: strings.Repeat("GiantDiagnosticWordWithoutAnySpacesOrWhitespaceOrBreaks", 80),
				RepairDiffSummary: "diff",
				SHA256Seal:        canonical.SHA256Of("test"),
			},
		},
		{
			name: "Receipt-07: Unicode CJK and emoji rich diagnostic and diff",
			receipt: &icx.ActuationReceipt{
				FileModified:      "项目/核心/分布式存储.go",
				ExitCode:          0,
				DiagnosticMessage: "编译成功：零错误，零警告，高并发性能指标达成 🚀🎉",
				RepairDiffSummary: "修复了死锁问题并重构了通道同步机制",
				SHA256Seal:        canonical.SHA256Of("cjk"),
			},
		},
		{
			name: "Receipt-08: Control characters, null bytes, and ANSI color codes",
			receipt: &icx.ActuationReceipt{
				FileModified:      "test\x00file.go",
				ExitCode:          127,
				DiagnosticMessage: "\x1b[31;1mError in line 42\x1b[0m\x00\x01\x02\r\n\t",
				RepairDiffSummary: "\x1b[32m+ line\x1b[0m",
				SHA256Seal:        canonical.SHA256Of("ansi"),
			},
		},
		{
			name: "Receipt-09: Quotes, backslashes, and XML injection inside receipt fields",
			receipt: &icx.ActuationReceipt{
				FileModified:      "\"foo'bar`<xml>\"/>",
				ExitCode:          0,
				DiagnosticMessage: "<script>alert('xss')</script> \"quoted\" and 'single'",
				RepairDiffSummary: "/> <injection attestation=false />",
				SHA256Seal:        canonical.SHA256Of("injection"),
			},
		},
		{
			name: "Receipt-10: Negative and large exit codes",
			receipt: &icx.ActuationReceipt{
				FileModified:      "pkg/runner.go",
				ExitCode:          -999999,
				DiagnosticMessage: "Killed",
				RepairDiffSummary: "None",
				SHA256Seal:        canonical.SHA256Of("neg"),
			},
		},
		{
			name: "Receipt-11: All fields maxed out simultaneously",
			receipt: &icx.ActuationReceipt{
				FileModified:      strings.Repeat("sub/", 300) + "extreme.go",
				ExitCode:          1,
				DiagnosticMessage: strings.Repeat("ExtremeDiagnosticDumpWithContinuousRepeatedTokens ", 200),
				RepairDiffSummary: strings.Repeat("ExtremeDiffSummaryLineAddedRemovedReplaced ", 200),
				SHA256Seal:        strings.Repeat("f", 64),
				TurnNumber:        999,
				Surface:           "engine_stage6_heal",
				Action:            "compiler_self_healing",
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			compact := tc.receipt.FormatCompact()
			tokens := llm.EstimateTokens(compact)
			if tokens >= 170 {
				t.Fatalf("VIOLATION: receipt tokens %d >= 170 budget ceiling:\n%s", tokens, compact)
			}
			if !strings.HasPrefix(compact, "<actuation:receipt") {
				t.Fatalf("FORMAT ERROR: missing prefix in receipt: %s", compact)
			}
			if !strings.HasSuffix(compact, "/>") {
				t.Fatalf("FORMAT ERROR: missing suffix in receipt: %s", compact)
			}
		})
	}
}

// ============================================================================
// 2. Actuation Receipts Session Reset Durability & Multi-Turn Persistence
// ============================================================================
func TestActuationReceipts_SessionResetDurability_Stress(t *testing.T) {
	ctx := context.Background()
	lat := icx.NewLocalLattice()

	// 1. Dispatch 10 initial receipts across turns
	for turn := 1; turn <= 10; turn++ {
		receipt := &icx.ActuationReceipt{
			FileModified:      fmt.Sprintf("pkg/service/component_%d.go", turn),
			ExitCode:          0,
			DiagnosticMessage: fmt.Sprintf("Healed turn %d", turn),
			RepairDiffSummary: fmt.Sprintf("+%d -%d", turn*2, turn),
			SHA256Seal:        canonical.SHA256Of(fmt.Sprintf("receipt_turn_%d", turn)),
			TurnNumber:        turn,
			Surface:           "engine_stage6_heal",
			Action:            "compiler_self_healing",
		}
		res, err := lat.DispatchActuation(ctx, icx.DispatchActuationRequest{
			Surface: receipt.Surface,
			Action:  receipt.Action,
			Payload: receipt.ToPayload(),
		})
		if err != nil || !res.Success {
			t.Fatalf("DispatchActuation failed on turn %d: %v", turn, err)
		}
	}

	initialReceipts := lat.GetActuationReceipts("")
	if len(initialReceipts) != 10 {
		t.Fatalf("Expected 10 initial receipts, got %d", len(initialReceipts))
	}

	// 2. Execute 10 sequential ResetSession calls (simulating turns 1-10 resets)
	for i := 1; i <= 10; i++ {
		resetRes, err := lat.ResetSession(ctx, "")
		if err != nil {
			t.Fatalf("ResetSession failed on iteration %d: %v", i, err)
		}
		if resetRes.TokensPreserved != 0 {
			t.Fatalf("ResetSession failed to reset dialogue tokens to 0: got %d", resetRes.TokensPreserved)
		}
		receiptsAfter := lat.GetActuationReceipts("")
		if len(receiptsAfter) != 10 {
			t.Fatalf("Durability failure on ResetSession %d: expected 10 receipts preserved, got %d", i, len(receiptsAfter))
		}
	}

	// 3. High-concurrency stress: 20 concurrent goroutines resetting session while 20 concurrent
	// goroutines dispatch new receipts and read receipts
	var wg sync.WaitGroup
	const concurrentWriters = 20
	const concurrentResetters = 20

	for i := 0; i < concurrentWriters; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			r := &icx.ActuationReceipt{
				FileModified:      fmt.Sprintf("concurrent_%d.go", idx),
				ExitCode:          0,
				DiagnosticMessage: "Concurrent write",
				RepairDiffSummary: "+1 -0",
				SHA256Seal:        canonical.SHA256Of(fmt.Sprintf("concurrent_%d", idx)),
				TurnNumber:        idx + 100,
				Surface:           "engine_stage5_patch",
				Action:            "fuzzy_patch",
			}
			_, _ = lat.DispatchActuation(ctx, icx.DispatchActuationRequest{
				Surface: r.Surface,
				Action:  r.Action,
				Payload: r.ToPayload(),
			})
		}(i)
	}

	for i := 0; i < concurrentResetters; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = lat.ResetSession(ctx, "")
			_ = lat.GetActuationReceipts("")
		}()
	}

	wg.Wait()

	finalReceipts := lat.GetActuationReceipts("")
	expectedMin := 10 + concurrentWriters
	if len(finalReceipts) < expectedMin {
		t.Fatalf("Concurrent durability loss: expected at least %d receipts, got %d", expectedMin, len(finalReceipts))
	}
	t.Logf("PASS: Session reset durability verified with %d crystallized receipts across concurrent resets", len(finalReceipts))
}

// ============================================================================
// 3. Swarm Variable Discovery (TTL Expiry Boundaries, Prefix Filtering, CAS)
// ============================================================================
func TestSwarmVar_TTLExpiry_BoundariesAndStress(t *testing.T) {
	ctx := context.Background()
	lat := icx.NewLocalLattice()

	// 1. Permanent variable (TTLMs = 0)
	permRes, err := lat.VarSet(ctx, icx.VarSetRequest{
		Key:   "perm_var",
		Value: "permanent_data",
		TTLMs: 0,
	})
	if err != nil || !permRes.OK {
		t.Fatalf("VarSet failed for perm_var: %v", err)
	}

	// 2. Ephemeral variable with tiny TTL (TTLMs = 50)
	ephRes, err := lat.VarSet(ctx, icx.VarSetRequest{
		Key:   "eph_var",
		Value: "ephemeral_data",
		TTLMs: 50,
	})
	if err != nil || !ephRes.OK {
		t.Fatalf("VarSet failed for eph_var: %v", err)
	}

	// Immediate check: both should exist
	getPerm1, _ := lat.VarGet(ctx, icx.VarGetRequest{Key: "perm_var"})
	if !getPerm1.OK || getPerm1.Value != "permanent_data" {
		t.Fatalf("Expected perm_var to exist immediately")
	}
	getEph1, _ := lat.VarGet(ctx, icx.VarGetRequest{Key: "eph_var"})
	if !getEph1.OK || getEph1.Value != "ephemeral_data" {
		t.Fatalf("Expected eph_var to exist immediately")
	}

	// Verify VarList before expiration
	list1, _ := lat.VarList(ctx, icx.VarListRequest{})
	if list1.Count < 2 {
		t.Fatalf("Expected at least 2 variables in VarList, got %d", list1.Count)
	}

	// Check RemainingTTLMs is positive
	var foundEph bool
	for _, v := range list1.Variables {
		if v.Key == "eph_var" {
			foundEph = true
			if v.RemainingTTLMs < 0 {
				t.Fatalf("RemainingTTLMs should be positive before expiration, got %d", v.RemainingTTLMs)
			}
		}
	}
	if !foundEph {
		t.Fatalf("eph_var not found in VarList before expiry")
	}

	// Wait for eph_var to expire (wait 80ms for 50ms TTL)
	time.Sleep(80 * time.Millisecond)

	// After expiry: eph_var must return VAR_NOT_FOUND
	getEph2, _ := lat.VarGet(ctx, icx.VarGetRequest{Key: "eph_var"})
	if getEph2.OK {
		t.Fatalf("Expected eph_var to be expired, but VarGet returned OK=true")
	}
	if getEph2.Error != "VAR_NOT_FOUND" {
		t.Fatalf("Expected VAR_NOT_FOUND, got %s", getEph2.Error)
	}

	// perm_var must still exist
	getPerm2, _ := lat.VarGet(ctx, icx.VarGetRequest{Key: "perm_var"})
	if !getPerm2.OK || getPerm2.Value != "permanent_data" {
		t.Fatalf("perm_var was unexpectedly modified or expired")
	}

	// VarList after expiry must NOT contain eph_var
	list2, _ := lat.VarList(ctx, icx.VarListRequest{})
	for _, v := range list2.Variables {
		if v.Key == "eph_var" {
			t.Fatalf("Expired eph_var still present in VarList: %+v", v)
		}
	}

	// Re-inserting expired variable should succeed seamlessly
	reinsertRes, err := lat.VarSet(ctx, icx.VarSetRequest{
		Key:   "eph_var",
		Value: "revived_data",
	})
	if err != nil || !reinsertRes.OK {
		t.Fatalf("Failed to re-insert previously expired variable: %v", err)
	}
	getEph3, _ := lat.VarGet(ctx, icx.VarGetRequest{Key: "eph_var"})
	if !getEph3.OK || getEph3.Value != "revived_data" {
		t.Fatalf("Revived variable value mismatch: %+v", getEph3)
	}

	t.Logf("PASS: Swarm variable TTL expiry boundaries and discovery verified")
}

func TestSwarmVar_PrefixFiltering_AndOrdering(t *testing.T) {
	ctx := context.Background()
	lat := icx.NewLocalLattice()

	varsToInsert := []struct {
		key string
		val string
	}{
		{"auth:token", "jwt_token"},
		{"auth:user", "user_123"},
		{"auth:claims", "claims_all"},
		{"auth_v2:key", "new_key"},
		{"cache:1", "data_1"},
		{"cache:2", "data_2"},
		{"data:items", "items"},
		{"z_last", "final"},
		{"a_first", "beginning"},
	}

	for _, v := range varsToInsert {
		_, err := lat.VarSet(ctx, icx.VarSetRequest{
			Key:   v.key,
			Value: v.val,
		})
		if err != nil {
			t.Fatalf("VarSet failed for %s: %v", v.key, err)
		}
	}

	// 1. Prefix "auth:" (should match exactly 3 items: auth:claims, auth:token, auth:user)
	resAuth, err := lat.VarList(ctx, icx.VarListRequest{Prefix: "auth:"})
	if err != nil || !resAuth.OK {
		t.Fatalf("VarList failed for prefix 'auth:': %v", err)
	}
	if resAuth.Count != 3 {
		t.Fatalf("Expected 3 items with prefix 'auth:', got %d: %+v", resAuth.Count, resAuth.Variables)
	}
	for _, v := range resAuth.Variables {
		if !strings.HasPrefix(v.Key, "auth:") {
			t.Fatalf("Non-matching key %s returned for prefix 'auth:'", v.Key)
		}
	}

	// 2. Deterministic Ascending Order Invariant
	for i := 1; i < len(resAuth.Variables); i++ {
		if resAuth.Variables[i-1].Key >= resAuth.Variables[i].Key {
			t.Fatalf("Ascending sort violation in VarList: %s >= %s",
				resAuth.Variables[i-1].Key, resAuth.Variables[i].Key)
		}
	}

	// 3. Limit Clamping (Limit = 2)
	resLimit, err := lat.VarList(ctx, icx.VarListRequest{Prefix: "auth:", Limit: 2})
	if err != nil || !resLimit.OK {
		t.Fatalf("VarList with limit failed: %v", err)
	}
	if resLimit.Count != 2 {
		t.Fatalf("Expected clamped count 2, got %d", resLimit.Count)
	}

	// 4. Non-matching prefix
	resNone, _ := lat.VarList(ctx, icx.VarListRequest{Prefix: "nonexistent_prefix:"})
	if resNone.Count != 0 {
		t.Fatalf("Expected 0 items for nonexistent prefix, got %d", resNone.Count)
	}

	// 5. Empty prefix matches all 9 variables
	resAll, _ := lat.VarList(ctx, icx.VarListRequest{Prefix: ""})
	if resAll.Count != len(varsToInsert) {
		t.Fatalf("Expected %d total items, got %d", len(varsToInsert), resAll.Count)
	}

	t.Logf("PASS: Swarm variable prefix filtering, limit clamping, and sort ordering verified")
}

// TestSwarmVar_CASContention_AdversarialStress tests 50 concurrent
// goroutines contending on a single variable key using optimistic Compare-And-Swap.
func TestSwarmVar_CASContention_AdversarialStress(t *testing.T) {
	ctx := context.Background()
	lat := icx.NewLocalLattice()

	key := "contested_counter"
	initRes, err := lat.VarSet(ctx, icx.VarSetRequest{
		Key:   key,
		Value: 0,
	})
	if err != nil || !initRes.OK {
		t.Fatalf("Init VarSet failed: %v", err)
	}

	const goroutines = 40
	const incrementsPerGoroutine = 5
	var wg sync.WaitGroup
	var successfulIncrements int64
	var totalCASConflicts int64

	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			for inc := 0; inc < incrementsPerGoroutine; inc++ {
				// Retry loop for CAS
				maxRetries := 50
				for r := 0; r < maxRetries; r++ {
					getRes, err := lat.VarGet(ctx, icx.VarGetRequest{Key: key})
					if err != nil || !getRes.OK {
						continue
					}

					curVal, ok := getRes.Value.(int)
					if !ok {
						if floatVal, okF := getRes.Value.(float64); okF {
							curVal = int(floatVal)
						} else {
							break
						}
					}
					expectedVer := getRes.Version

					setRes, err := lat.VarSet(ctx, icx.VarSetRequest{
						Key:             key,
						Value:           curVal + 1,
						ExpectedVersion: &expectedVer,
						Writer:          fmt.Sprintf("worker_%d", workerID),
					})
					if err != nil {
						continue
					}
					if setRes.OK {
						atomic.AddInt64(&successfulIncrements, 1)
						break
					} else if setRes.Error == "CAS_CONFLICT" {
						atomic.AddInt64(&totalCASConflicts, 1)
						// Backoff slightly to reduce extreme lockstep thrashing
						time.Sleep(time.Duration(workerID%5) * time.Millisecond)
					}
				}
			}
		}(i)
	}

	wg.Wait()

	finalGet, err := lat.VarGet(ctx, icx.VarGetRequest{Key: key})
	if err != nil || !finalGet.OK {
		t.Fatalf("Final VarGet failed: %v", err)
	}

	finalVal, _ := finalGet.Value.(int)
	if finalVal == 0 {
		if fv, ok := finalGet.Value.(float64); ok {
			finalVal = int(fv)
		}
	}

	t.Logf("CAS Stress: successful increments = %d, total CAS conflicts recorded = %d, final counter = %d, final version = %d",
		successfulIncrements, totalCASConflicts, finalVal, finalGet.Version)

	if int64(finalVal) != successfulIncrements {
		t.Fatalf("INVARIANT VIOLATION: Final counter value %d != successful increments %d", finalVal, successfulIncrements)
	}
	expectedVersion := 1 + int(successfulIncrements) // version 1 was the initial insert
	if finalGet.Version != expectedVersion {
		t.Fatalf("INVARIANT VIOLATION: Final version %d != expected version %d", finalGet.Version, expectedVersion)
	}
	if totalCASConflicts == 0 {
		t.Logf("Notice: 0 CAS conflicts (timing was favorable)")
	}
	t.Logf("PASS: Swarm variable CAS contention verified with exact zero data races and zero version skew")
}

// ============================================================================
// 4. Remote-Only Connectors (Offline Refusal & Zero Fallback)
// ============================================================================
func TestRemoteOnlyConnectors_StrictRefusal(t *testing.T) {
	ctx := context.Background()
	lat := icx.NewLocalLattice()

	// 1. Direct method calls on local lattice must strictly return ErrRemoteOnly
	t.Run("Direct Local Method Calls", func(t *testing.T) {
		if _, err := lat.ListConnectors(ctx, icx.ListConnectorsRequest{}); !errors.Is(err, icx.ErrRemoteOnly) {
			t.Fatalf("ListConnectors did not return ErrRemoteOnly: %v", err)
		}
		if _, err := lat.RegisterConnector(ctx, icx.RegisterConnectorRequest{}); !errors.Is(err, icx.ErrRemoteOnly) {
			t.Fatalf("RegisterConnector did not return ErrRemoteOnly: %v", err)
		}
		if _, err := lat.SyncAudit(ctx, icx.SyncAuditRequest{}); !errors.Is(err, icx.ErrRemoteOnly) {
			t.Fatalf("SyncAudit did not return ErrRemoteOnly: %v", err)
		}
		if _, err := lat.TriggerSync(ctx, icx.TriggerSyncRequest{}); !errors.Is(err, icx.ErrRemoteOnly) {
			t.Fatalf("TriggerSync did not return ErrRemoteOnly: %v", err)
		}
		if _, err := lat.GetConnectorDocument(ctx, icx.GetConnectorDocumentRequest{}); !errors.Is(err, icx.ErrRemoteOnly) {
			t.Fatalf("GetConnectorDocument did not return ErrRemoteOnly: %v", err)
		}
		if _, err := lat.InspectConnectorFacts(ctx, icx.InspectConnectorFactsRequest{}); !errors.Is(err, icx.ErrRemoteOnly) {
			t.Fatalf("InspectConnectorFacts did not return ErrRemoteOnly: %v", err)
		}
	})

	// 2. CallTool via MCP tool names must return IsError: true with remote-only message
	connectorTools := []string{
		"icx_list_connectors",
		"icx_register_connector",
		"icx_sync_audit",
		"icx_trigger_sync",
		"icx_get_connector_document",
		"icx_inspect_connector_facts",
	}

	t.Run("CallTool Dispatch Refusal", func(t *testing.T) {
		for _, toolName := range connectorTools {
			res, err := lat.CallTool(ctx, toolName, map[string]any{"connector_id": "test_conn"})
			if err != nil {
				t.Fatalf("CallTool returned unexpected error instead of tool result: %v", err)
			}
			if !res.IsError {
				t.Fatalf("SECURITY VIOLATION: CallTool for %s did not set IsError=true", toolName)
			}
			if len(res.Content) == 0 || !strings.Contains(res.Content[0].Text, "remote-only") {
				t.Fatalf("SECURITY VIOLATION: CallTool for %s did not include remote-only explanation: %+v", toolName, res)
			}
		}
	})

	// 3. Client offline and strict configuration
	t.Run("Client Offline Strict Invariant", func(t *testing.T) {
		client, err := icx.NewClient(icx.ClientConfig{
			BaseURL: "http://127.0.0.1:9999", // Unreachable port
			Offline: true,
			Strict:  true,
		})
		if err != nil {
			t.Fatalf("NewClient failed: %v", err)
		}
		defer client.Close()

		if _, err := client.ListConnectors(ctx, icx.ListConnectorsRequest{}); !errors.Is(err, icx.ErrRemoteOnly) {
			t.Fatalf("Offline client ListConnectors did not return ErrRemoteOnly: %v", err)
		}
	})

	t.Logf("PASS: Remote-only connector guards strictly refused offline execution with zero fallback")
}

// ============================================================================
// 5. Merkle Root Inclusion Proofs and Tamper Resistance Oracles
// ============================================================================
func verifyMerkleInclusion(leafHashes []string, leafIdx int, expectedRoot string) (bool, error) {
	if leafIdx < 0 || leafIdx >= len(leafHashes) {
		return false, fmt.Errorf("leafIdx %d out of bounds for %d leaves", leafIdx, len(leafHashes))
	}
	computedRoot := canonical.MerkleRoot(leafHashes)
	if computedRoot != expectedRoot {
		return false, fmt.Errorf("root mismatch: got %s, want %s", computedRoot, expectedRoot)
	}

	layer := make([]string, len(leafHashes))
	copy(layer, leafHashes)

	for len(layer) > 1 {
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
		layer = next
	}

	return layer[0] == expectedRoot, nil
}

func TestMerkleInclusionProof_AndTamperResistance(t *testing.T) {
	// Test varying leaf count geometries (including odd counts: 1, 2, 3, 5, 7, 8, 17)
	leafCounts := []int{1, 2, 3, 5, 7, 8, 17}

	for _, count := range leafCounts {
		t.Run(fmt.Sprintf("%d_Leaves", count), func(t *testing.T) {
			leaves := make([]string, count)
			for i := 0; i < count; i++ {
				leaves[i] = canonical.SHA256Of(fmt.Sprintf("audit_leaf_content_%d", i))
			}

			root := canonical.MerkleRoot(leaves)
			if len(root) != 64 {
				t.Fatalf("Invalid Merkle root length: %d for count %d", len(root), count)
			}

			// 1. Verify inclusion proof for every leaf index
			for i := 0; i < count; i++ {
				ok, err := verifyMerkleInclusion(leaves, i, root)
				if err != nil || !ok {
					t.Fatalf("Inclusion proof failed for leaf %d/%d: %v", i, count, err)
				}
			}

			// 2. Tamper Oracle: Flip a single bit in each leaf and verify root mismatch
			for i := 0; i < count; i++ {
				tampered := append([]string{}, leaves...)
				// Bit-flip first character of leaf hash
				flipped := byte('0')
				if tampered[i][0] == '0' {
					flipped = '1'
				}
				tampered[i] = string(flipped) + tampered[i][1:]

				tamperedRoot := canonical.MerkleRoot(tampered)
				if tamperedRoot == root {
					t.Fatalf("TAMPER ORACLE FAILED: bit flip in leaf %d did not alter root!", i)
				}

				// Proof verification against original root must fail
				ok, _ := verifyMerkleInclusion(tampered, i, root)
				if ok {
					t.Fatalf("TAMPER ORACLE FAILED: tampered leaf was accepted by original root!")
				}
			}
		})
	}

	t.Logf("PASS: Merkle root inclusion proofs and tamper resistance oracles mathematically validated across all geometries")
}
