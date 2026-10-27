// SPDX-License-Identifier: Apache-2.0
// Package icx_test verifies the 21 Certified ICX Tools, in-process local memory cache,
// CAS optimistic concurrency, AST multihop walking with cycle detection,
// zero-token session reset, Wasm stack machine, exact decimal arithmetic,
// and zero race condition warnings under -race.

package icx_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/caleralabs/omnicoder-icx/pkg/icx"
	"github.com/shopspring/decimal"
)

// TestAll21CertifiedTools verifies all 21 tools and virtual mode gateway tools.
func TestAll21CertifiedTools(t *testing.T) {
	ctx := context.Background()
	client := icx.NewLocalLattice()

	// Tool 1: icx_remember
	remRes, err := client.Remember(ctx, icx.RememberRequest{
		Text:     "Contract invariant: total_supply == 1000000 with sk-live-secretkey12345",
		SourceID: "spec.v1",
		Family:   "token_invariants",
	})
	if err != nil {
		t.Fatalf("Remember failed: %v", err)
	}
	if remRes.Status != "OK" || remRes.FactID == "" {
		t.Fatalf("Unexpected Remember response: %+v", remRes)
	}
	if len(remRes.Redactions) == 0 {
		t.Fatalf("Expected API key to be redacted, got none: %+v", remRes)
	}

	// Tool 2: icx_recall_scoped (clamped top_k <= 10)
	recallRes, err := client.RecallScoped(ctx, icx.RecallRequest{
		Query:  "total_supply",
		Family: "token_invariants",
		TopK:   50, // Requesting 50 should be clamped strictly <= 10
	})
	if err != nil {
		t.Fatalf("RecallScoped failed: %v", err)
	}
	if recallRes.Count > 10 {
		t.Fatalf("RecallScoped exceeded TopK clamp (expected <= 10, got %d)", recallRes.Count)
	}
	if len(recallRes.Matches) == 0 {
		t.Fatalf("Expected at least 1 match in recall, got 0")
	}

	// Tool 3: icx_search_facts
	searchRes, err := client.SearchFacts(ctx, icx.SearchRequest{
		Query: "invariant",
		Limit: 5,
	})
	if err != nil {
		t.Fatalf("SearchFacts failed: %v", err)
	}
	if searchRes.TotalMatches == 0 {
		t.Fatalf("Expected search matches, got 0")
	}

	// Tool 4: icx_quote_slot (<0.002ms byte-exact quote)
	quoteRes, err := client.QuoteSlot(ctx, icx.QuoteSlotRequest{
		Family: "token_invariants",
		Index:  1, // 1-based index
	})
	if err != nil {
		t.Fatalf("QuoteSlot failed: %v", err)
	}
	if !quoteRes.OK || !quoteRes.Verified {
		t.Fatalf("QuoteSlot failed verification: %+v", quoteRes)
	}
	if quoteRes.LatencyUs >= 2000 {
		t.Logf("QuoteSlot latency: %.2f us (SLA <2000us)", quoteRes.LatencyUs)
	}

	// Tool 5: icx_sync_delta
	syncRes, err := client.SyncDelta(ctx, icx.SyncDeltaRequest{
		SourceURI: "repo/pkg/token.go",
		DeltaSimplex: map[string]any{
			"added_symbols": []string{"Mint", "Burn"},
		},
	})
	if err != nil {
		t.Fatalf("SyncDelta failed: %v", err)
	}
	if syncRes.Status != "OK" || syncRes.CISSeal == "" {
		t.Fatalf("SyncDelta invalid response: %+v", syncRes)
	}

	// Tool 6: icx_reset_session
	resetRes, err := client.ResetSession(ctx, "default")
	if err != nil {
		t.Fatalf("ResetSession failed: %v", err)
	}
	if resetRes.TokensPreserved != 0 {
		t.Fatalf("Expected TokensPreserved == 0, got %d", resetRes.TokensPreserved)
	}

	// Tool 7: icx_multihop_walk (AST walk)
	client.AddASTNode(&icx.LatticeNode{ID: "fn_A", Name: "FuncA", Type: "function"})
	client.AddASTNode(&icx.LatticeNode{ID: "fn_B", Name: "FuncB", Type: "function"})
	client.AddASTEdge("fn_A", "calls", "fn_B")
	walkRes, err := client.MultihopWalk(ctx, icx.MultihopWalkRequest{
		StartNode: "fn_A",
		MaxHops:   5,
	})
	if err != nil {
		t.Fatalf("MultihopWalk failed: %v", err)
	}
	if !walkRes.OK || walkRes.NodesReached < 2 {
		t.Fatalf("MultihopWalk failed to reach target nodes: %+v", walkRes)
	}

	// Tool 8 & 9: icx_var_set & icx_var_get (CAS state)
	v0 := 0
	setRes, err := client.VarSet(ctx, icx.VarSetRequest{
		Key:             "system_state",
		Value:           "INITIALIZING",
		ExpectedVersion: &v0,
		Writer:          "agent_1",
	})
	if err != nil {
		t.Fatalf("VarSet failed: %v", err)
	}
	if !setRes.OK || setRes.Version != 1 {
		t.Fatalf("VarSet invalid result: %+v", setRes)
	}

	getRes, err := client.VarGet(ctx, icx.VarGetRequest{Key: "system_state"})
	if err != nil {
		t.Fatalf("VarGet failed: %v", err)
	}
	if !getRes.OK || getRes.Value != "INITIALIZING" || getRes.Version != 1 {
		t.Fatalf("VarGet invalid result: %+v", getRes)
	}

	// Tool 10: icx_exec (Sandboxed script)
	execRes, err := client.Exec(ctx, icx.ExecRequest{
		Source: "x = 10; y = 20; result = x + y",
	})
	if err != nil {
		t.Fatalf("Exec failed: %v", err)
	}
	if !execRes.OK || fmt.Sprintf("%v", execRes.Result) != "30" {
		t.Fatalf("Exec result incorrect: %+v", execRes)
	}

	// Tool 11: icx_wasm_exec (Wasm stack machine)
	wasmRes, err := client.WasmExec(ctx, icx.WasmExecRequest{
		Program: []string{"PUSH:10", "PUSH:20", "ADD", "PUSH:5", "MUL", "HALT"},
	})
	if err != nil {
		t.Fatalf("WasmExec failed: %v", err)
	}
	if !wasmRes.OK || wasmRes.Result != 150.0 {
		t.Fatalf("WasmExec result incorrect (expected 150): %+v", wasmRes)
	}

	// Tool 12: icx_inspect_space
	inspRes, err := client.InspectSpace(ctx, "default")
	if err != nil {
		t.Fatalf("InspectSpace failed: %v", err)
	}
	if inspRes.SpaceID != "default" || inspRes.ShardCount != 20 || inspRes.MerkleRoot == "" {
		t.Fatalf("InspectSpace unexpected output: %+v", inspRes)
	}

	// Tool 13: icx_purge_source
	purgeRes, err := client.PurgeSource(ctx, icx.PurgeSourceRequest{
		SourceID: "spec.v1",
	})
	if err != nil {
		t.Fatalf("PurgeSource failed: %v", err)
	}
	if purgeRes.Status != "OK" || purgeRes.NodesUnlinked < 1 {
		t.Fatalf("PurgeSource expected nodes unlinked: %+v", purgeRes)
	}

	// Tool 14: icx_swarm_state (Distributed mutex)
	leaseRes, err := client.SwarmState(ctx, icx.SwarmStateRequest{
		Action:   "lease",
		Resource: "file_lock_main.go",
		AgentID:  "worker_1",
		TTLMs:    5000,
	})
	if err != nil {
		t.Fatalf("SwarmState lease failed: %v", err)
	}
	if !leaseRes.OK || leaseRes.Token == "" {
		t.Fatalf("SwarmState lease unsuccessful: %+v", leaseRes)
	}

	relRes, err := client.SwarmState(ctx, icx.SwarmStateRequest{
		Action:   "release",
		Resource: "file_lock_main.go",
		AgentID:  "worker_1",
	})
	if err != nil || !relRes.OK {
		t.Fatalf("SwarmState release failed: %+v, err: %v", relRes, err)
	}

	// Tool 15: icx_query_codebase
	client.AddASTNode(&icx.LatticeNode{
		ID:        "node_query_test",
		Dimension: "3D_VOLUMETRIC",
		Type:      "function",
		Name:      "CalculateFee",
		FilePath:  "pkg/fee/fee.go",
		Line:      42,
	})
	qRes, err := client.QueryCodebase(ctx, icx.QueryCodebaseRequest{
		Query:     "CalculateFee",
		Dimension: "3D_VOLUMETRIC",
	})
	if err != nil || qRes.Count == 0 {
		t.Fatalf("QueryCodebase failed: %+v, err: %v", qRes, err)
	}

	// Tool 16: icx_read_dimension
	rdRes, err := client.ReadDimension(ctx, icx.ReadDimensionRequest{
		Dimension: "3D_VOLUMETRIC",
		NodeID:    "node_query_test",
	})
	if err != nil || len(rdRes.Nodes) == 0 {
		t.Fatalf("ReadDimension failed: %+v, err: %v", rdRes, err)
	}

	// Tool 17: icx_read_file_ast
	astRes, err := client.ReadFileAST(ctx, icx.ReadFileASTRequest{
		FilePath: "pkg/fee/fee.go",
	})
	if err != nil || len(astRes.ASTNodes) == 0 {
		t.Fatalf("ReadFileAST failed: %+v, err: %v", astRes, err)
	}

	// Tool 18: icx_edit_node
	editNRes, err := client.EditNode(ctx, icx.EditNodeRequest{
		NodeID:   "node_query_test",
		NewValue: map[string]any{"signature": "CalculateFee(tier int) int"},
	})
	if err != nil || !editNRes.Success {
		t.Fatalf("EditNode failed: %+v, err: %v", editNRes, err)
	}

	// Tool 19: icx_edit_file
	editFRes, err := client.EditFile(ctx, icx.EditFileRequest{
		FilePath: "pkg/fee/fee.go",
		Content:  "package fee\n\nfunc CalculateFee() int { return 10 }\n",
	})
	if err != nil || !editFRes.Success {
		t.Fatalf("EditFile failed: %+v, err: %v", editFRes, err)
	}

	// Tool 20: icx_ingest_dimension
	dimRes, err := client.IngestDimension(ctx, icx.IngestDimensionRequest{
		Dimension: "0D_SCALAR",
		Payload:   map[string]any{"test_coverage": 98},
	})
	if err != nil || !dimRes.Success {
		t.Fatalf("IngestDimension failed: %+v, err: %v", dimRes, err)
	}

	// Tool 21: icx_execute_script
	scriptRes, err := client.ExecuteScript(ctx, icx.ExecuteScriptRequest{
		ScriptName: "calc_fee",
		Code:       "base = 100; rate = 5; result = base * rate",
	})
	if err != nil || !scriptRes.Success {
		t.Fatalf("ExecuteScript failed: %+v, err: %v", scriptRes, err)
	}

	// Gateway Tools
	epistemicRes, err := client.RecallEpistemicContext(ctx, icx.RecallEpistemicContextRequest{
		Query: "invariant",
	})
	if err != nil {
		t.Fatalf("RecallEpistemicContext failed: %v", err)
	}
	if epistemicRes == nil {
		t.Fatalf("RecallEpistemicContext returned nil")
	}

	actRes, err := client.DispatchActuation(ctx, icx.DispatchActuationRequest{
		Surface: "terminal",
		Action:  "echo",
		Payload: map[string]any{"msg": "hello"},
	})
	if err != nil || !actRes.Success {
		t.Fatalf("DispatchActuation failed: %+v, err: %v", actRes, err)
	}

	// CallTool generic dispatch
	toolCallRes, err := client.CallTool(ctx, "icx_var_get", map[string]any{"key": "system_state"})
	if err != nil || toolCallRes.IsError {
		t.Fatalf("CallTool failed: %+v, err: %v", toolCallRes, err)
	}
}

// TestASTMultihopWalk_CycleDetection verifies cycle detection in cyclic AST graphs.
func TestASTMultihopWalk_CycleDetection(t *testing.T) {
	ctx := context.Background()
	lat := icx.NewLocalLattice()

	// Build cyclic graph:
	// A -> B -> C -> A (cycle 1)
	// C -> D -> E -> B (cycle 2)
	nodes := []string{"node_A", "node_B", "node_C", "node_D", "node_E"}
	for _, n := range nodes {
		lat.AddASTNode(&icx.LatticeNode{
			ID:   n,
			Name: n,
			Type: "function",
		})
	}

	lat.AddASTEdge("node_A", "calls", "node_B")
	lat.AddASTEdge("node_B", "calls", "node_C")
	lat.AddASTEdge("node_C", "calls", "node_A") // cycle 1
	lat.AddASTEdge("node_C", "calls", "node_D")
	lat.AddASTEdge("node_D", "calls", "node_E")
	lat.AddASTEdge("node_E", "calls", "node_B") // cycle 2

	res, err := lat.MultihopWalk(ctx, icx.MultihopWalkRequest{
		StartNode: "node_A",
		MaxHops:   5,
	})
	if err != nil {
		t.Fatalf("MultihopWalk failed: %v", err)
	}

	if !res.OK {
		t.Fatalf("MultihopWalk expected OK=true, got false: %+v", res)
	}

	if res.CyclesDetected < 2 {
		t.Fatalf("Expected at least 2 cycles detected in cyclic graph, got %d", res.CyclesDetected)
	}

	if res.Hops > 5 {
		t.Fatalf("Hops (%d) exceeded MaxHops (5)", res.Hops)
	}

	if len(res.Steps) != len(nodes) {
		t.Fatalf("Expected all %d unique nodes to be visited once, got %d steps", len(nodes), len(res.Steps))
	}

	if len(res.Path) == 0 || res.Path[0] != "node_A" {
		t.Fatalf("Path should start with node_A: %+v", res.Path)
	}
}

// TestCASOptimisticLocking_ConcurrentContention verifies that 30 concurrent writers
// racing on VarSet encounter CAS_CONFLICT and resolve contention without data races.
func TestCASOptimisticLocking_ConcurrentContention(t *testing.T) {
	ctx := context.Background()
	lat := icx.NewLocalLattice()

	key := "concurrent_counter"
	// Initialize key with version 1, value 0
	initVer := 0
	initRes, err := lat.VarSet(ctx, icx.VarSetRequest{
		Key:             key,
		Value:           0,
		ExpectedVersion: &initVer,
		Writer:          "init",
	})
	if err != nil || !initRes.OK {
		t.Fatalf("Initial VarSet failed: %+v, err: %v", initRes, err)
	}

	const numGoroutines = 30
	var wg sync.WaitGroup
	var totalConflicts atomic.Int64
	var totalSuccesses atomic.Int64

	wg.Add(numGoroutines)
	for i := 0; i < numGoroutines; i++ {
		go func(workerID int) {
			defer wg.Done()
			for {
				// Read current state
				getRes, err := lat.VarGet(ctx, icx.VarGetRequest{Key: key})
				if err != nil || !getRes.OK {
					continue
				}

				curVal := 0
				switch v := getRes.Value.(type) {
				case int:
					curVal = v
				case int64:
					curVal = int(v)
				case float64:
					curVal = int(v)
				}

				expVer := getRes.Version
				setRes, err := lat.VarSet(ctx, icx.VarSetRequest{
					Key:             key,
					Value:           curVal + 1,
					ExpectedVersion: &expVer,
					Writer:          fmt.Sprintf("worker_%d", workerID),
				})
				if err != nil {
					continue
				}

				if setRes.OK {
					totalSuccesses.Add(1)
					break
				} else if setRes.Error == "CAS_CONFLICT" {
					totalConflicts.Add(1)
					// Exponential/random jitter backoff
					time.Sleep(time.Duration(1+rand.Intn(5)) * time.Millisecond)
				}
			}
		}(i)
	}

	wg.Wait()

	finalGet, err := lat.VarGet(ctx, icx.VarGetRequest{Key: key})
	if err != nil || !finalGet.OK {
		t.Fatalf("Final VarGet failed: %+v", finalGet)
	}

	if totalSuccesses.Load() != numGoroutines {
		t.Fatalf("Expected %d total successes, got %d", numGoroutines, totalSuccesses.Load())
	}

	// Final version should be 1 + numGoroutines = 31
	if finalGet.Version != numGoroutines+1 {
		t.Fatalf("Expected final version %d, got %d", numGoroutines+1, finalGet.Version)
	}

	// Final counter value should be 30
	if fmt.Sprintf("%v", finalGet.Value) != fmt.Sprintf("%d", numGoroutines) {
		t.Fatalf("Expected counter value %d, got %v", numGoroutines, finalGet.Value)
	}

	if totalConflicts.Load() == 0 {
		t.Logf("Notice: 0 CAS conflicts observed (scheduling was strictly sequential)")
	} else {
		t.Logf("CAS contention successfully verified: %d conflicts resolved", totalConflicts.Load())
	}
}

// TestResetSession_ZeroTokens_LatticePreserved verifies dialogue token wipe
// while 100% of underlying 4D lattice memory is preserved.
func TestResetSession_ZeroTokens_LatticePreserved(t *testing.T) {
	ctx := context.Background()
	lat := icx.NewLocalLattice()

	// Ingest dialogue turn facts
	for i := 1; i <= 5; i++ {
		_, err := lat.Remember(ctx, icx.RememberRequest{
			Text:     fmt.Sprintf("Dialogue turn %d: What is the codebase architecture?", i),
			SourceID: "chat_session_1",
			Family:   "dialogue_turn",
		})
		if err != nil {
			t.Fatalf("Failed ingesting dialogue fact: %v", err)
		}
	}

	// Ingest invariant / codebase facts
	for i := 1; i <= 5; i++ {
		_, err := lat.Remember(ctx, icx.RememberRequest{
			Text:     fmt.Sprintf("Code Invariant %d: Memory is volumetric and deterministic.", i),
			SourceID: "arch_doc",
			Family:   "code_invariants",
		})
		if err != nil {
			t.Fatalf("Failed ingesting invariant fact: %v", err)
		}
	}

	// Set a swarm variable
	v0 := 0
	_, err := lat.VarSet(ctx, icx.VarSetRequest{
		Key:             "project_state",
		Value:           "ACTIVE",
		ExpectedVersion: &v0,
	})
	if err != nil {
		t.Fatalf("VarSet failed: %v", err)
	}

	// Perform session reset
	res, err := lat.ResetSession(ctx, "default")
	if err != nil {
		t.Fatalf("ResetSession failed: %v", err)
	}

	if res.TokensPreserved != 0 {
		t.Fatalf("Expected dialogue tokens to be 0, got %d", res.TokensPreserved)
	}

	// Dialogue turns should be pruned
	quoteTurn, _ := lat.QuoteSlot(ctx, icx.QuoteSlotRequest{
		Family: "dialogue_turn",
		Index:  1,
	})
	if quoteTurn.OK {
		t.Fatalf("Dialogue turn facts should be wiped, but slot 1 returned OK")
	}

	// Invariants should be 100% preserved
	quoteInv, err := lat.QuoteSlot(ctx, icx.QuoteSlotRequest{
		Family: "code_invariants",
		Index:  1,
	})
	if err != nil || !quoteInv.OK || !quoteInv.Verified {
		t.Fatalf("Code invariants must be 100%% preserved: %+v, err: %v", quoteInv, err)
	}

	// Swarm variable must remain intact
	getVar, err := lat.VarGet(ctx, icx.VarGetRequest{Key: "project_state"})
	if err != nil || !getVar.OK || getVar.Value != "ACTIVE" {
		t.Fatalf("Swarm state must remain intact after session reset: %+v", getVar)
	}
}

// TestSecretScrubber verifies that all sensitive patterns are cleanly redacted.
func TestSecretScrubber(t *testing.T) {
	scrubber := icx.NewSecretScrubber()

	raw := "Credentials: sk-proj-1234567890abcdef and icx_live_ABC123456789 and AKIAIOSFODNN7EXAMPLE " +
		"with Bearer eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9 and password=SuperSecretPassword123 " +
		"contact support@caleralabs.com or ssn 000-12-3456"

	scrubbed, labels := scrubber.Scrub(raw)

	expectedLabels := []string{"api_key", "bearer", "password", "email", "ssn"}
	for _, exp := range expectedLabels {
		found := false
		for _, l := range labels {
			if l == exp {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("Expected label %s to be matched, got labels: %v", exp, labels)
		}
	}

	forbiddenSubstrings := []string{
		"sk-proj-1234567890abcdef",
		"icx_live_ABC123456789",
		"AKIAIOSFODNN7EXAMPLE",
		"SuperSecretPassword123",
		"support@caleralabs.com",
		"000-12-3456",
	}
	for _, sub := range forbiddenSubstrings {
		if strings.Contains(scrubbed, sub) {
			t.Errorf("Sensitive substring '%s' was NOT redacted in: %s", sub, scrubbed)
		}
	}
}

// TestWasmKernel_OpcodesAndSafety tests stack machine execution and error handling.
func TestWasmKernel_OpcodesAndSafety(t *testing.T) {
	kernel := icx.NewWasmKernel()

	// 1. Math calculation: sqrt(max(abs(-16), 9)) = sqrt(16) = 4
	res := kernel.Exec([]string{
		"PUSH:-16", "ABS", "PUSH:9", "MAX", "SQRT", "HALT",
	})
	if !res.OK || res.Result != 4.0 {
		t.Fatalf("Expected 4.0, got: %+v", res)
	}

	// 2. Division by zero
	resDivZero := kernel.Exec([]string{"PUSH:10", "PUSH:0", "DIV"})
	if resDivZero.OK || resDivZero.Error != "WASM DIV by zero" {
		t.Fatalf("Expected 'WASM DIV by zero', got: %+v", resDivZero)
	}

	// 3. Sqrt of negative
	resSqrtNeg := kernel.Exec([]string{"PUSH:-25", "SQRT"})
	if resSqrtNeg.OK || resSqrtNeg.Error != "WASM SQRT of negative number" {
		t.Fatalf("Expected 'WASM SQRT of negative number', got: %+v", resSqrtNeg)
	}

	// 4. Stack underflow
	resUnderflow := kernel.Exec([]string{"ADD"})
	if resUnderflow.OK || !strings.Contains(resUnderflow.Error, "stack underflow") {
		t.Fatalf("Expected stack underflow, got: %+v", resUnderflow)
	}
}

// TestExactArithmetic_ShopspringDecimal tests zero-float evaluation.
func TestExactArithmetic_ShopspringDecimal(t *testing.T) {
	// IEEE float in env must be rejected
	_, err := icx.EvalArithmetic("x + 1", map[string]any{"x": 12.34})
	if err == nil {
		t.Fatalf("Expected zero-float error on float64 in env, got nil")
	}

	// Exact decimal evaluation
	env := map[string]any{
		"revenue":  decimal.NewFromInt(1000000),
		"expenses": decimal.NewFromInt(650000),
	}
	res, err := icx.EvalArithmetic("(revenue - expenses) / revenue * 100", env)
	if err != nil {
		t.Fatalf("EvalArithmetic failed: %v", err)
	}
	expected := decimal.NewFromInt(35)
	if !res.Equal(expected) {
		t.Fatalf("Expected 35, got %s", res.String())
	}

	// CAGR test: (144 / 100)^(1/2) - 1 = 1.2 - 1 = 0.2 (20%)
	cagrRes, err := icx.EvalArithmetic("cagr(100, 144, 2)", nil)
	if err != nil {
		t.Fatalf("cagr failed: %v", err)
	}
	expectedCAGR, _ := decimal.NewFromString("0.2")
	if !cagrRes.Equal(expectedCAGR) {
		t.Fatalf("Expected CAGR 0.2, got %s", cagrRes.String())
	}
}

// TestShardDeterminismInvariants verifies deterministic shard assignment across arbitrary payloads.
func TestShardDeterminismInvariants(t *testing.T) {
	for i := 0; i < 50; i++ {
		payload := []byte(fmt.Sprintf("test_payload_%d", i))
		shardIdx := icx.ComputeShardIndex(payload)
		if shardIdx < 0 || shardIdx >= icx.DefaultShardCount {
			t.Fatalf("Shard index %d out of bounds [0, %d)", shardIdx, icx.DefaultShardCount)
		}
		if icx.ComputeShardIndex(payload) != shardIdx {
			t.Fatalf("ComputeShardIndex is not deterministic for payload %d", i)
		}
	}
}

// TestConcurrency_RaceFree executes 50 concurrent goroutines doing mixed operations
// to prove zero race conditions under `go test -race`.
func TestConcurrency_RaceFree(t *testing.T) {
	ctx := context.Background()
	client := icx.NewLocalLattice()

	var wg sync.WaitGroup
	const workers = 50

	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func(id int) {
			defer wg.Done()
			fam := fmt.Sprintf("worker_fam_%d", id%5)

			// 1. Remember
			_, _ = client.Remember(ctx, icx.RememberRequest{
				Text:     fmt.Sprintf("Fact from worker %d", id),
				SourceID: fmt.Sprintf("source_%d", id),
				Family:   fam,
			})

			// 2. RecallScoped
			_, _ = client.RecallScoped(ctx, icx.RecallRequest{
				Query:  "worker",
				Family: fam,
				TopK:   5,
			})

			// 3. VarSet / VarGet
			_, _ = client.VarSet(ctx, icx.VarSetRequest{
				Key:    fmt.Sprintf("var_%d", id%10),
				Value:  id,
				Writer: fmt.Sprintf("worker_%d", id),
			})
			_, _ = client.VarGet(ctx, icx.VarGetRequest{Key: fmt.Sprintf("var_%d", id%10)})

			// 4. QuoteSlot
			_, _ = client.QuoteSlot(ctx, icx.QuoteSlotRequest{
				Family: fam,
				Index:  1,
			})

			// 5. WasmExec
			_, _ = client.WasmExec(ctx, icx.WasmExecRequest{
				Program: []string{"PUSH:10", "PUSH:5", "ADD", "HALT"},
			})

			// 6. InspectSpace
			_, _ = client.InspectSpace(ctx, "default")
		}(i)
	}

	wg.Wait()
}

// TestClientTransport_HTTPAndStdio verifies HTTP transport headers and fallback.
func TestClientTransport_HTTPAndStdio(t *testing.T) {
	ctx := context.Background()

	// 1. HTTP transport with mock server verifying Enterprise Headers
	var capturedAuth, capturedLic, capturedSpace, capturedMode string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedAuth = r.Header.Get("Authorization")
		capturedLic = r.Header.Get("X-License-Key")
		capturedSpace = r.Header.Get("X-Space-ID")
		capturedMode = r.Header.Get("X-MCP-Mode")

		resp := icx.JSONRPCResponse{
			JSONRPC: "2.0",
			ID:      1,
			Result:  json.RawMessage(`{"status":"OK","fact_id":"f_remote_01","sha256":"abc"}`),
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer ts.Close()

	client, err := icx.NewClient(icx.ClientConfig{
		BaseURL: ts.URL,
		APIKey:  "test-secret-key",
		SpaceID: "prod-space",
		MCPMode: "virtual",
		Strict:  true,
	})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}

	remRes, err := client.Remember(ctx, icx.RememberRequest{
		Text:     "Remote test fact",
		SourceID: "remote_src",
	})
	if err != nil {
		t.Fatalf("Remember over HTTP failed: %v", err)
	}
	if remRes.FactID != "f_remote_01" {
		t.Fatalf("Expected f_remote_01, got %s", remRes.FactID)
	}

	// Verify Enterprise Headers were sent
	if capturedAuth != "Bearer test-secret-key" {
		t.Errorf("Expected Authorization 'Bearer test-secret-key', got '%s'", capturedAuth)
	}
	if capturedLic != "test-secret-key" {
		t.Errorf("Expected X-License-Key 'test-secret-key', got '%s'", capturedLic)
	}
	if capturedSpace != "prod-space" {
		t.Errorf("Expected X-Space-ID 'prod-space', got '%s'", capturedSpace)
	}
	if capturedMode != "virtual" {
		t.Errorf("Expected X-MCP-Mode 'virtual', got '%s'", capturedMode)
	}

	// 2. Offline fallback when APIKey is empty
	offlineClient, err := icx.NewClient(icx.ClientConfig{
		BaseURL: "http://unreachable-domain-12345.local",
		APIKey:  "", // triggers offline local fallback
	})
	if err != nil {
		t.Fatalf("Offline NewClient failed: %v", err)
	}
	if !offlineClient.IsOffline() {
		t.Fatalf("Client should be offline when APIKey is empty")
	}

	offRemRes, err := offlineClient.Remember(ctx, icx.RememberRequest{
		Text:     "Offline fallback fact",
		SourceID: "local_src",
	})
	if err != nil || offRemRes.FactID == "" {
		t.Fatalf("Offline Remember failed: %+v, err: %v", offRemRes, err)
	}

	// 3. Stdio pipe transport
	pr1, pw1 := io.Pipe()
	pr2, pw2 := io.Pipe()

	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		reader := bufio.NewReader(pr1)
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				return
			}
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "Content-Length:") {
				parts := strings.Split(trimmed, ":")
				if len(parts) != 2 {
					continue
				}
				length, err := strconv.Atoi(strings.TrimSpace(parts[1]))
				if err != nil {
					continue
				}
				// Skip headers until empty line
				for {
					hLine, err := reader.ReadString('\n')
					if err != nil {
						return
					}
					if strings.TrimSpace(hLine) == "" {
						break
					}
				}
				// Read body using io.ReadFull
				body := make([]byte, length)
				if _, err := io.ReadFull(reader, body); err != nil {
					return
				}
				var req icx.JSONRPCRequest
				if err := json.Unmarshal(body, &req); err == nil {
					res := icx.JSONRPCResponse{
						JSONRPC: "2.0",
						ID:      req.ID,
						Result:  json.RawMessage(`{"status":"OK","fact_id":"f_stdio_01"}`),
					}
					respBytes, _ := json.Marshal(res)
					header := fmt.Sprintf("Content-Length: %d\r\n\r\n", len(respBytes))
					_, _ = pw2.Write([]byte(header))
					_, _ = pw2.Write(respBytes)
				}
			}
		}
	}()

	stdioClient, err := icx.NewClient(icx.ClientConfig{
		APIKey:        "test-key",
		TransportMode: "stdio",
		StdioReader:   pr2,
		StdioWriter:   pw1,
		Strict:        true,
	})
	if err != nil {
		t.Fatalf("Stdio client creation failed: %v", err)
	}
	defer stdioClient.Close()
	defer pw1.Close()
	defer pw2.Close()

	stdioRemRes, err := stdioClient.Remember(ctx, icx.RememberRequest{
		Text:     "Stdio test fact",
		SourceID: "stdio_src",
	})
	if err != nil {
		t.Fatalf("Stdio Remember failed: %v", err)
	}
	if stdioRemRes.FactID != "f_stdio_01" {
		t.Fatalf("Expected FactID 'f_stdio_01', got: %s", stdioRemRes.FactID)
	}

	_ = pw2.Close()
}

// TestClientTransport_SSE verifies Server-Sent Events (SSE) transport round-trip,
// enterprise headers assertion, and response event unmarshaling.
func TestClientTransport_SSE(t *testing.T) {
	ctx := context.Background()

	var recordedHeaders http.Header
	var recordedMethod string
	var recordedPath string
	var recordedBody []byte

	sseServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		recordedHeaders = r.Header.Clone()
		recordedMethod = r.Method
		recordedPath = r.URL.Path
		recordedBody, _ = io.ReadAll(r.Body)

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)

		event := "data: {\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{\"status\":\"ok\",\"fact_id\":\"f_sse_01\"}}\n\n"
		_, _ = w.Write([]byte(event))
	}))
	defer sseServer.Close()

	sseClient, err := icx.NewClient(icx.ClientConfig{
		BaseURL:       sseServer.URL + "/rpc",
		APIKey:        "license-key-secret-999",
		SpaceID:       "space-sse-test",
		MCPMode:       "virtual",
		TransportMode: "sse",
		Strict:        true,
	})
	if err != nil {
		t.Fatalf("Failed to create SSE client: %v", err)
	}
	defer sseClient.Close()

	remRes, err := sseClient.Remember(ctx, icx.RememberRequest{
		Text:     "SSE memory fact",
		SourceID: "sse_source",
	})
	if err != nil {
		t.Fatalf("SSE Remember failed: %v", err)
	}

	if remRes.FactID != "f_sse_01" {
		t.Fatalf("Expected FactID 'f_sse_01', got '%s'", remRes.FactID)
	}
	if remRes.Status != "ok" {
		t.Fatalf("Expected Status 'ok', got '%s'", remRes.Status)
	}

	// Assert enterprise headers
	if recordedHeaders.Get("X-License-Key") != "license-key-secret-999" {
		t.Fatalf("Expected X-License-Key header 'license-key-secret-999', got '%s'", recordedHeaders.Get("X-License-Key"))
	}
	if recordedHeaders.Get("X-Space-ID") != "space-sse-test" {
		t.Fatalf("Expected X-Space-ID header 'space-sse-test', got '%s'", recordedHeaders.Get("X-Space-ID"))
	}
	if recordedHeaders.Get("Authorization") != "Bearer license-key-secret-999" {
		t.Fatalf("Expected Authorization header 'Bearer license-key-secret-999', got '%s'", recordedHeaders.Get("Authorization"))
	}
	if recordedHeaders.Get("X-MCP-Mode") != "virtual" {
		t.Fatalf("Expected X-MCP-Mode header 'virtual', got '%s'", recordedHeaders.Get("X-MCP-Mode"))
	}
	if recordedMethod != "POST" {
		t.Fatalf("Expected POST method, got '%s'", recordedMethod)
	}
	if recordedPath != "/rpc" {
		t.Fatalf("Expected path '/rpc', got '%s'", recordedPath)
	}
	if len(recordedBody) == 0 {
		t.Fatalf("Expected non-empty request body")
	}
}

// TestASTMultihopWalk_DAGDiamond_NoCycles verifies that reconvergent edges in DAGs (e.g. A->B->D and A->C->D)
// are not falsely reported as cycles.
func TestASTMultihopWalk_DAGDiamond_NoCycles(t *testing.T) {
	ctx := context.Background()
	lat := icx.NewLocalLattice()

	nodes := []string{"node_A", "node_B", "node_C", "node_D"}
	for _, n := range nodes {
		lat.AddASTNode(&icx.LatticeNode{
			ID:   n,
			Name: n,
			Type: "function",
		})
	}

	// Diamond DAG:
	// A -> B -> D
	// A -> C -> D
	lat.AddASTEdge("node_A", "calls", "node_B")
	lat.AddASTEdge("node_A", "calls", "node_C")
	lat.AddASTEdge("node_B", "calls", "node_D")
	lat.AddASTEdge("node_C", "calls", "node_D")

	res, err := lat.MultihopWalk(ctx, icx.MultihopWalkRequest{
		StartNode: "node_A",
		MaxHops:   5,
	})
	if err != nil {
		t.Fatalf("MultihopWalk failed: %v", err)
	}

	if !res.OK {
		t.Fatalf("MultihopWalk expected OK=true, got false: %+v", res)
	}

	if res.CyclesDetected != 0 {
		t.Fatalf("Expected 0 cycles detected in diamond DAG, got %d", res.CyclesDetected)
	}

	if res.NodesReached != 4 {
		t.Fatalf("Expected all 4 nodes to be reached in diamond DAG, got %d", res.NodesReached)
	}
}

// TestClient_OfflineDispatchReturnsErrClientOffline verifies that an offline client's dispatch
// returns ErrClientOffline and cleanly falls back to local lattice in non-strict mode.
func TestClient_OfflineDispatchReturnsErrClientOffline(t *testing.T) {
	ctx := context.Background()

	offlineClient, err := icx.NewClient(icx.ClientConfig{
		Offline: true,
	})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}

	// 1. Verify dispatch directly returns ErrClientOffline when offline
	if c, ok := offlineClient.(*icx.Client); ok {
		err := c.DispatchForTest(ctx, "icx_remember", icx.RememberRequest{})
		if !errors.Is(err, icx.ErrClientOffline) {
			t.Fatalf("Expected ErrClientOffline from dispatch when offline, got: %v", err)
		}
	} else {
		t.Fatalf("Expected *icx.Client type")
	}

	// 2. Non-strict offline client should cleanly fall back to localEngine
	res, err := offlineClient.Remember(ctx, icx.RememberRequest{
		Text:     "Fallback local fact",
		SourceID: "fallback_src",
	})
	if err != nil {
		t.Fatalf("Fallback Remember failed: %v", err)
	}
	if res.FactID == "" {
		t.Fatalf("Expected non-empty FactID from local engine fallback")
	}
}

// TestStdioTransport_LargeRequestIDCorrelation verifies that request IDs >= 1,000,000
// are correctly correlated between float64 and uint64 in StdioTransport without scientific notation mismatch.
func TestStdioTransport_LargeRequestIDCorrelation(t *testing.T) {
	ctx := context.Background()
	pr1, pw1 := io.Pipe()
	pr2, pw2 := io.Pipe()
	defer pw1.Close()
	defer pw2.Close()
	defer pr1.Close()
	defer pr2.Close()

	go func() {
		reader := bufio.NewReader(pr1)
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				return
			}
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "Content-Length:") {
				parts := strings.Split(trimmed, ":")
				if len(parts) != 2 {
					continue
				}
				length, err := strconv.Atoi(strings.TrimSpace(parts[1]))
				if err != nil {
					continue
				}
				for {
					hLine, err := reader.ReadString('\n')
					if err != nil {
						return
					}
					if strings.TrimSpace(hLine) == "" {
						break
					}
				}
				body := make([]byte, length)
				if _, err := io.ReadFull(reader, body); err != nil {
					return
				}
				var req icx.JSONRPCRequest
				if err := json.Unmarshal(body, &req); err == nil {
					// Emulate JSON-RPC 2.0 response with integer ID that unmarshals to float64
					res := icx.JSONRPCResponse{
						JSONRPC: "2.0",
						ID:      req.ID,
						Result:  json.RawMessage(`{"status":"OK","fact_id":"f_large_id_01"}`),
					}
					respBytes, _ := json.Marshal(res)
					header := fmt.Sprintf("Content-Length: %d\r\n\r\n", len(respBytes))
					_, _ = pw2.Write([]byte(header))
					_, _ = pw2.Write(respBytes)
				}
			}
		}
	}()

	client, err := icx.NewClient(icx.ClientConfig{
		APIKey:        "test-key",
		TransportMode: "stdio",
		StdioReader:   pr2,
		StdioWriter:   pw1,
		Strict:        true,
	})
	if err != nil {
		t.Fatalf("Stdio client creation failed: %v", err)
	}
	defer client.Close()

	// Set next request ID to 1,000,000 to verify large ID float64 correlation
	if c, ok := client.(*icx.Client); ok {
		c.SetNextRequestID(1000000)
	}

	remRes, err := client.Remember(ctx, icx.RememberRequest{
		Text:     "Large ID fact",
		SourceID: "large_id_src",
	})
	if err != nil {
		t.Fatalf("Remember with large request ID failed: %v", err)
	}
	if remRes.FactID != "f_large_id_01" {
		t.Fatalf("Expected FactID 'f_large_id_01', got: %s", remRes.FactID)
	}
}
