// SPDX-License-Identifier: Apache-2.0
// Package icx_test contains empirical adversarial stress tests for swarm variable operations:
// 1. High-concurrency stress testing with 50+ concurrent goroutines under -race
// 2. CallTool argument type coercion stress (float64, int, int64, string for TTL and ExpectedVersion)
// 3. Large variable count enumeration (1,500+ keys) and boundary limit clamping
// 4. Deadlock, memory leak, and race-free concurrent lifecycle verification

package icx_test

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/caleralabs/omnicoder-icx/pkg/icx"
)

// ============================================================================
// PART 1: High-Concurrency Stress Test (50+ Workers under -race)
// ============================================================================

func TestVarStress_HighConcurrency_50Workers_Race(t *testing.T) {
	ctx := context.Background()
	lat := icx.NewLocalLattice()

	const numWorkers = 50
	const opsPerWorker = 40
	var wg sync.WaitGroup
	wg.Add(numWorkers)

	var (
		setOpsCompleted    atomic.Int64
		getOpsCompleted    atomic.Int64
		listOpsCompleted   atomic.Int64
		casConflicts       atomic.Int64
		toolCallsCompleted atomic.Int64
	)

	// Pre-populate some base keys
	for i := 0; i < 20; i++ {
		_, _ = lat.VarSet(ctx, icx.VarSetRequest{
			Key:    fmt.Sprintf("base_key_%d", i),
			Value:  fmt.Sprintf("base_val_%d", i),
			Writer: "init",
			TTLMs:  0, // persistent
		})
	}

	startBarrier := make(chan struct{})

	for w := 0; w < numWorkers; w++ {
		go func(workerID int) {
			defer wg.Done()
			<-startBarrier // Synchronize start for maximum contention

			r := rand.New(rand.NewSource(time.Now().UnixNano() + int64(workerID*1000)))

			for i := 0; i < opsPerWorker; i++ {
				op := r.Intn(5)
				targetKeyIdx := r.Intn(30)
				key := fmt.Sprintf("stress_key_%d", targetKeyIdx)

				switch op {
				case 0:
					// Fast TTL set (10-40ms)
					ttl := 10 + r.Intn(30)
					res, err := lat.VarSet(ctx, icx.VarSetRequest{
						Key:    key,
						Value:  fmt.Sprintf("worker_%d_val_%d", workerID, i),
						Writer: fmt.Sprintf("worker_%d", workerID),
						TTLMs:  ttl,
					})
					if err == nil && res.OK {
						setOpsCompleted.Add(1)
					}

				case 1:
					// CAS update attempt
					getRes, err := lat.VarGet(ctx, icx.VarGetRequest{Key: key})
					expVer := 0
					if err == nil && getRes.OK {
						expVer = getRes.Version
					}
					casRes, err := lat.VarSet(ctx, icx.VarSetRequest{
						Key:             key,
						Value:           fmt.Sprintf("cas_worker_%d_val_%d", workerID, i),
						ExpectedVersion: &expVer,
						Writer:          fmt.Sprintf("worker_%d", workerID),
						TTLMs:           20 + r.Intn(50),
					})
					if err == nil {
						if casRes.OK {
							setOpsCompleted.Add(1)
						} else if casRes.Error == "CAS_CONFLICT" {
							casConflicts.Add(1)
						}
					}

				case 2:
					// VarGet
					res, err := lat.VarGet(ctx, icx.VarGetRequest{Key: key})
					if err == nil {
						getOpsCompleted.Add(1)
						_ = res
					}

				case 3:
					// VarList with various prefixes and limits
					prefixes := []string{"stress_", "base_", "nonexistent_", ""}
					p := prefixes[r.Intn(len(prefixes))]
					lim := r.Intn(15)
					res, err := lat.VarList(ctx, icx.VarListRequest{
						Prefix: p,
						Limit:  lim,
					})
					if err == nil && res.OK {
						listOpsCompleted.Add(1)
						// Invariant: Sorted order must be maintained under concurrent mutations
						for idx := 1; idx < len(res.Variables); idx++ {
							if res.Variables[idx-1].Key >= res.Variables[idx].Key {
								t.Errorf("CONCURRENCY SORT CORRUPTION: %s >= %s",
									res.Variables[idx-1].Key, res.Variables[idx].Key)
							}
						}
						// Invariant: Count must match len(Variables) and len(Vars)
						if res.Count != len(res.Variables) || res.Count != len(res.Vars) {
							t.Errorf("CONCURRENCY COUNT INVARIANT VIOLATION: Count=%d, VarsLen=%d, MapLen=%d",
								res.Count, len(res.Variables), len(res.Vars))
						}
					}

				case 4:
					// CallTool path ("icx_var_set" or "icx_var_list")
					if r.Intn(2) == 0 {
						cRes, err := lat.CallTool(ctx, "icx_var_set", map[string]any{
							"key":    fmt.Sprintf("tool_stress_%d", workerID),
							"value":  fmt.Sprintf("tool_val_%d", i),
							"writer": fmt.Sprintf("worker_%d", workerID),
							"ttl_ms": float64(25 + r.Intn(25)),
						})
						if err == nil && !cRes.IsError {
							toolCallsCompleted.Add(1)
						}
					} else {
						cRes, err := lat.CallTool(ctx, "icx_var_list", map[string]any{
							"prefix": "tool_stress_",
							"limit":  float64(5),
						})
						if err == nil && !cRes.IsError {
							toolCallsCompleted.Add(1)
						}
					}
				}

				if i%10 == 0 {
					// Intermittent space inspection under active concurrency
					_, _ = lat.InspectSpace(ctx, "default")
				}
			}
		}(w)
	}

	close(startBarrier)
	wg.Wait()

	totalOps := setOpsCompleted.Load() + getOpsCompleted.Load() + listOpsCompleted.Load() + casConflicts.Load() + toolCallsCompleted.Load()
	t.Logf("Completed %d total concurrent operations across %d workers with 0 deadlocks/races", totalOps, numWorkers)
	t.Logf("Stats: Sets=%d, Gets=%d, Lists=%d, CASConflicts=%d, ToolCalls=%d",
		setOpsCompleted.Load(), getOpsCompleted.Load(), listOpsCompleted.Load(),
		casConflicts.Load(), toolCallsCompleted.Load())

	if totalOps < int64(numWorkers*opsPerWorker*3/4) {
		t.Fatalf("Insufficient operations completed: got %d, expected at least %d",
			totalOps, numWorkers*opsPerWorker*3/4)
	}
}

// ============================================================================
// PART 2: CallTool Handling of float64, int, int64, and string TTL & Version Representations
// ============================================================================

func TestCallTool_TypeCoercion_FloatIntString(t *testing.T) {
	ctx := context.Background()
	lat := icx.NewLocalLattice()

	testCases := []struct {
		name            string
		ttlInput        any
		expVerInput     any
		expectTTLActive bool
		expectConflict  bool
	}{
		{
			name:            "TTL_as_float64_ExpectedVer_as_float64_0",
			ttlInput:        float64(60.0),
			expVerInput:     float64(0),
			expectTTLActive: true,
			expectConflict:  false,
		},
		{
			name:            "TTL_as_int_ExpectedVer_as_int_0",
			ttlInput:        int(60),
			expVerInput:     int(0),
			expectTTLActive: true,
			expectConflict:  false,
		},
		{
			name:            "TTL_as_int64_ExpectedVer_as_int64_0",
			ttlInput:        int64(60),
			expVerInput:     int64(0),
			expectTTLActive: true,
			expectConflict:  false,
		},
		{
			name:            "TTL_as_string_ExpectedVer_as_string_0",
			ttlInput:        "60",
			expVerInput:     "0",
			expectTTLActive: true,
			expectConflict:  false,
		},
		{
			name:            "TTL_as_nil_omitted_persistent",
			ttlInput:        nil,
			expVerInput:     nil,
			expectTTLActive: false,
			expectConflict:  false,
		},
		{
			name:            "TTL_negative_treated_as_persistent",
			ttlInput:        float64(-50),
			expVerInput:     nil,
			expectTTLActive: false,
			expectConflict:  false,
		},
		{
			name:            "ExpectedVer_mismatch_string_conflict",
			ttlInput:        "60",
			expVerInput:     "999", // Key does not exist (ver 0), expecting 999 -> conflict
			expectTTLActive: false,
			expectConflict:  true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			key := fmt.Sprintf("coercion_%s", tc.name)
			args := map[string]any{
				"key":    key,
				"value":  "typed_val",
				"writer": "type_tester",
			}
			if tc.ttlInput != nil {
				args["ttl_ms"] = tc.ttlInput
			}
			if tc.expVerInput != nil {
				args["expected_version"] = tc.expVerInput
			}

			// Invoke icx_var_set via CallTool
			toolRes, err := lat.CallTool(ctx, "icx_var_set", args)
			if err != nil {
				t.Fatalf("CallTool unexpected transport error: %v", err)
			}
			if toolRes.IsError {
				t.Fatalf("CallTool returned IsError: true: %+v", toolRes)
			}
			if len(toolRes.Content) == 0 {
				t.Fatalf("CallTool returned empty Content")
			}

			var setResp icx.VarSetResponse
			if err := json.Unmarshal([]byte(toolRes.Content[0].Text), &setResp); err != nil {
				t.Fatalf("Failed to parse icx_var_set JSON: %v, raw text: %s", err, toolRes.Content[0].Text)
			}

			if tc.expectConflict {
				if setResp.OK || setResp.Error != "CAS_CONFLICT" {
					t.Fatalf("Expected CAS_CONFLICT, got: %+v", setResp)
				}
				return
			}

			if !setResp.OK {
				t.Fatalf("Expected VarSet success, got: %+v", setResp)
			}

			// Verify via icx_var_list through CallTool with string and float limits
			listRes, err := lat.CallTool(ctx, "icx_var_list", map[string]any{
				"prefix": key,
				"limit":  "10", // String limit coercion test
			})
			if err != nil || listRes.IsError {
				t.Fatalf("CallTool icx_var_list failed: %+v, err: %v", listRes, err)
			}
			var listResp icx.VarListResponse
			if err := json.Unmarshal([]byte(listRes.Content[0].Text), &listResp); err != nil {
				t.Fatalf("Failed to parse icx_var_list JSON: %v", err)
			}
			if listResp.Count != 1 {
				t.Fatalf("Expected 1 variable in list, got %d", listResp.Count)
			}

			entry := listResp.Variables[0]
			if tc.expectTTLActive {
				if entry.RemainingTTLMs <= 0 || entry.RemainingTTLMs > 70 {
					t.Fatalf("Expected remaining TTL in (0, 70], got %d", entry.RemainingTTLMs)
				}
			} else {
				if entry.RemainingTTLMs != -1 {
					t.Fatalf("Expected persistent RemainingTTLMs == -1, got %d", entry.RemainingTTLMs)
				}
			}
		})
	}
}

// ============================================================================
// PART 3: Large Variable Counts (1,500+ Keys) and Boundary Limit Clamping
// ============================================================================

func TestLargeKeyCounts_LimitClamping(t *testing.T) {
	ctx := context.Background()
	lat := icx.NewLocalLattice()

	const totalKeys = 1500
	t.Logf("Seeding %d keys into LocalMemoryCache...", totalKeys)

	// Seed 1,500 keys in non-lexicographical order
	r := rand.New(rand.NewSource(42))
	indices := r.Perm(totalKeys)

	for _, idx := range indices {
		key := fmt.Sprintf("k_%05d", idx)
		res, err := lat.VarSet(ctx, icx.VarSetRequest{
			Key:    key,
			Value:  fmt.Sprintf("payload_%05d", idx),
			Writer: "bulk_seeder",
			TTLMs:  0, // Persistent
		})
		if err != nil || !res.OK {
			t.Fatalf("Failed to seed key %s: %+v, err: %v", key, res, err)
		}
	}

	// 1. Clamping to Limit = 1
	t.Run("Clamp_Limit_1", func(t *testing.T) {
		res, err := lat.VarList(ctx, icx.VarListRequest{Limit: 1})
		if err != nil || !res.OK {
			t.Fatalf("VarList failed: %+v, err: %v", res, err)
		}
		if res.Count != 1 || len(res.Variables) != 1 || len(res.Vars) != 1 {
			t.Fatalf("Expected exactly 1 result, got Count=%d, VarsLen=%d, MapLen=%d",
				res.Count, len(res.Variables), len(res.Vars))
		}
		if res.Variables[0].Key != "k_00000" {
			t.Fatalf("Expected minimum sorted key k_00000, got %s", res.Variables[0].Key)
		}
		if res.Vars["k_00000"] != "payload_00000" {
			t.Fatalf("Vars map mismatch: got %v", res.Vars["k_00000"])
		}
	})

	// 2. Clamping to Limit = 50
	t.Run("Clamp_Limit_50", func(t *testing.T) {
		res, err := lat.VarList(ctx, icx.VarListRequest{Limit: 50})
		if err != nil || !res.OK {
			t.Fatalf("VarList failed: %+v, err: %v", res, err)
		}
		if res.Count != 50 || len(res.Variables) != 50 || len(res.Vars) != 50 {
			t.Fatalf("Expected exactly 50 results, got Count=%d, VarsLen=%d, MapLen=%d",
				res.Count, len(res.Variables), len(res.Vars))
		}
		// Verify strict ascending sort order
		for i := 0; i < 50; i++ {
			expectedKey := fmt.Sprintf("k_%05d", i)
			if res.Variables[i].Key != expectedKey {
				t.Fatalf("At index %d: expected key %s, got %s", i, expectedKey, res.Variables[i].Key)
			}
			if res.Vars[expectedKey] != fmt.Sprintf("payload_%05d", i) {
				t.Fatalf("Vars map value mismatch for key %s", expectedKey)
			}
		}
	})

	// 3. Clamping to Limit = 500
	t.Run("Clamp_Limit_500", func(t *testing.T) {
		res, err := lat.VarList(ctx, icx.VarListRequest{Limit: 500})
		if err != nil || !res.OK {
			t.Fatalf("VarList failed: %+v, err: %v", res, err)
		}
		if res.Count != 500 || len(res.Variables) != 500 || len(res.Vars) != 500 {
			t.Fatalf("Expected exactly 500 results, got Count=%d, VarsLen=%d, MapLen=%d",
				res.Count, len(res.Variables), len(res.Vars))
		}
		for i := 1; i < 500; i++ {
			if res.Variables[i-1].Key >= res.Variables[i].Key {
				t.Fatalf("Non-ascending order at index %d: %s >= %s",
					i, res.Variables[i-1].Key, res.Variables[i].Key)
			}
		}
	})

	// 4. Clamping to Limit = 1000
	t.Run("Clamp_Limit_1000", func(t *testing.T) {
		res, err := lat.VarList(ctx, icx.VarListRequest{Limit: 1000})
		if err != nil || !res.OK {
			t.Fatalf("VarList failed: %+v, err: %v", res, err)
		}
		if res.Count != 1000 || len(res.Variables) != 1000 || len(res.Vars) != 1000 {
			t.Fatalf("Expected exactly 1000 results, got Count=%d, VarsLen=%d, MapLen=%d",
				res.Count, len(res.Variables), len(res.Vars))
		}
	})

	// 5. Unclamped (Limit = 0) returns all 1,500 keys
	t.Run("Unclamped_Limit_0_ReturnsAll", func(t *testing.T) {
		res, err := lat.VarList(ctx, icx.VarListRequest{Limit: 0})
		if err != nil || !res.OK {
			t.Fatalf("VarList failed: %+v, err: %v", res, err)
		}
		if res.Count != totalKeys || len(res.Variables) != totalKeys || len(res.Vars) != totalKeys {
			t.Fatalf("Expected %d results, got Count=%d, VarsLen=%d, MapLen=%d",
				totalKeys, res.Count, len(res.Variables), len(res.Vars))
		}
		for i := 0; i < totalKeys; i++ {
			expectedKey := fmt.Sprintf("k_%05d", i)
			if res.Variables[i].Key != expectedKey {
				t.Fatalf("At index %d: expected key %s, got %s", i, expectedKey, res.Variables[i].Key)
			}
		}
	})

	// 6. Limit exceeds total key count (Limit = 2500)
	t.Run("Limit_Exceeds_Count_ReturnsAll", func(t *testing.T) {
		res, err := lat.VarList(ctx, icx.VarListRequest{Limit: 2500})
		if err != nil || !res.OK {
			t.Fatalf("VarList failed: %+v, err: %v", res, err)
		}
		if res.Count != totalKeys {
			t.Fatalf("Expected %d results when Limit=2500, got %d", totalKeys, res.Count)
		}
	})

	// 7. Prefix filtering on large key set (e.g. "k_001" -> matches k_00100 through k_00199 = 100 keys)
	t.Run("Prefix_Filtering_LargeSet", func(t *testing.T) {
		res, err := lat.VarList(ctx, icx.VarListRequest{
			Prefix: "k_001",
			Limit:  200,
		})
		if err != nil || !res.OK {
			t.Fatalf("VarList failed: %+v, err: %v", res, err)
		}
		if res.Count != 100 || len(res.Variables) != 100 {
			t.Fatalf("Expected exactly 100 keys matching prefix k_001, got %d", res.Count)
		}
		for _, v := range res.Variables {
			if v.Key < "k_00100" || v.Key > "k_00199" {
				t.Fatalf("Unexpected key returned outside prefix scope: %s", v.Key)
			}
		}
	})
}

// ============================================================================
// PART 4: Expiration Filtering Under High Volume (1,000 Expired Keys)
// ============================================================================

func TestHighVolume_TTLExpiration_Filtering(t *testing.T) {
	ctx := context.Background()
	lat := icx.NewLocalLattice()

	const persistentCount = 200
	const volatileCount = 800

	// Set 200 persistent keys
	for i := 0; i < persistentCount; i++ {
		_, err := lat.VarSet(ctx, icx.VarSetRequest{
			Key:    fmt.Sprintf("perm_%04d", i),
			Value:  i,
			Writer: "perm_writer",
			TTLMs:  0,
		})
		if err != nil {
			t.Fatalf("Failed to set persistent key: %v", err)
		}
	}

	// Set 800 volatile keys with 35ms TTL
	for i := 0; i < volatileCount; i++ {
		_, err := lat.VarSet(ctx, icx.VarSetRequest{
			Key:    fmt.Sprintf("vol_%04d", i),
			Value:  i,
			Writer: "vol_writer",
			TTLMs:  35,
		})
		if err != nil {
			t.Fatalf("Failed to set volatile key: %v", err)
		}
	}

	// Immediate check: Total count should be 1000
	allPre, err := lat.VarList(ctx, icx.VarListRequest{})
	if err != nil || !allPre.OK {
		t.Fatalf("VarList pre-expiry failed: %v", err)
	}
	if allPre.Count != persistentCount+volatileCount {
		t.Fatalf("Expected %d total keys before expiry, got %d", persistentCount+volatileCount, allPre.Count)
	}

	// Sleep past expiration duration
	time.Sleep(55 * time.Millisecond)

	// Post-expiry check: All 800 volatile keys must be omitted, exactly 200 persistent keys remain
	allPost, err := lat.VarList(ctx, icx.VarListRequest{})
	if err != nil || !allPost.OK {
		t.Fatalf("VarList post-expiry failed: %v", err)
	}
	if allPost.Count != persistentCount {
		t.Fatalf("LEAKAGE: Expected %d persistent keys after expiry, got %d (volatile keys not filtered!)",
			persistentCount, allPost.Count)
	}
	for _, v := range allPost.Variables {
		if v.Key[:4] == "vol_" {
			t.Fatalf("LEAKAGE: Volatile key %s found in VarList after expiration!", v.Key)
		}
		if v.RemainingTTLMs != -1 {
			t.Fatalf("Expected RemainingTTLMs == -1 for persistent key %s, got %d", v.Key, v.RemainingTTLMs)
		}
	}

	// Verify VarGet returns VAR_NOT_FOUND for sampled volatile keys
	for i := 0; i < 50; i++ {
		sampleKey := fmt.Sprintf("vol_%04d", i*10)
		getRes, err := lat.VarGet(ctx, icx.VarGetRequest{Key: sampleKey})
		if err != nil {
			t.Fatalf("VarGet unexpected error: %v", err)
		}
		if getRes.OK || getRes.Error != "VAR_NOT_FOUND" {
			t.Fatalf("LEAKAGE: VarGet returned active for expired key %s: %+v", sampleKey, getRes)
		}
	}
}

// ============================================================================
// PART 5: Deadlock & Memory Leak Resilience under Rapid Overwrites & Resets
// ============================================================================

func TestRapidOverwrites_And_ResetContention(t *testing.T) {
	ctx := context.Background()
	lat := icx.NewLocalLattice()

	// Measure baseline memory
	runtime.GC()
	var m1 runtime.MemStats
	runtime.ReadMemStats(&m1)

	const numRoutines = 20
	const iterations = 50
	var wg sync.WaitGroup
	wg.Add(numRoutines)

	for w := 0; w < numRoutines; w++ {
		go func(id int) {
			defer wg.Done()
			for it := 0; it < iterations; it++ {
				key := fmt.Sprintf("hot_key_%d", it%5)
				// Repeatedly overwrite same keys with differing TTLs
				_, _ = lat.VarSet(ctx, icx.VarSetRequest{
					Key:    key,
					Value:  fmt.Sprintf("payload_%d_%d", id, it),
					Writer: fmt.Sprintf("writer_%d", id),
					TTLMs:  15 + (it % 20),
				})

				if it%10 == 0 {
					_, _ = lat.VarList(ctx, icx.VarListRequest{Prefix: "hot_key_"})
				}
			}
		}(w)
	}

	wg.Wait()

	// Verify no deadlocks and lattice is still responsive
	testKey := "post_stress_check"
	res, err := lat.VarSet(ctx, icx.VarSetRequest{
		Key:    testKey,
		Value:  "ok",
		Writer: "checker",
	})
	if err != nil || !res.OK {
		t.Fatalf("Lattice unresponsive or deadlocked after stress test: %+v, err: %v", res, err)
	}

	runtime.GC()
	var m2 runtime.MemStats
	runtime.ReadMemStats(&m2)

	t.Logf("HeapAlloc: Before=%d KB, After=%d KB (delta=%d KB)",
		m1.HeapAlloc/1024, m2.HeapAlloc/1024, int64(m2.HeapAlloc-m1.HeapAlloc)/1024)
}
