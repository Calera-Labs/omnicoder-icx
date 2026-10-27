// SPDX-License-Identifier: Apache-2.0
package icx_test

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/caleralabs/omnicoder-icx/pkg/icx"
)

// TestVarSet_TTL_Expiry verifies that variables with TTL expire after the specified duration,
// are omitted from VarList, return VAR_NOT_FOUND in VarGet, and can be recreated with version 1.
func TestVarSet_TTL_Expiry(t *testing.T) {
	ctx := context.Background()
	lat := icx.NewLocalLattice()

	key := "temp_token"
	val := "SECRET123"

	// Set with TTL = 40ms
	setRes, err := lat.VarSet(ctx, icx.VarSetRequest{
		Key:    key,
		Value:  val,
		Writer: "tester",
		TTLMs:  40,
	})
	if err != nil || !setRes.OK {
		t.Fatalf("VarSet failed: %+v, err: %v", setRes, err)
	}

	// Immediately verify via VarGet
	getRes, err := lat.VarGet(ctx, icx.VarGetRequest{Key: key})
	if err != nil || !getRes.OK {
		t.Fatalf("Immediate VarGet failed: %+v, err: %v", getRes, err)
	}
	if getRes.Value != val {
		t.Fatalf("Expected value %q, got %v", val, getRes.Value)
	}

	// Verify via VarList
	listRes, err := lat.VarList(ctx, icx.VarListRequest{Prefix: "temp"})
	if err != nil || !listRes.OK {
		t.Fatalf("VarList failed: %+v, err: %v", listRes, err)
	}
	if listRes.Count != 1 || len(listRes.Variables) != 1 {
		t.Fatalf("Expected 1 variable in VarList, got count=%d, len=%d", listRes.Count, len(listRes.Variables))
	}
	vInfo := listRes.Variables[0]
	if vInfo.Key != key {
		t.Fatalf("Expected key %q, got %q", key, vInfo.Key)
	}
	if vInfo.RemainingTTLMs <= 0 || vInfo.RemainingTTLMs > 40 {
		t.Fatalf("Expected RemainingTTLMs in (0, 40], got %d", vInfo.RemainingTTLMs)
	}

	// Sleep past expiration
	time.Sleep(60 * time.Millisecond)

	// Verify VarGet returns VAR_NOT_FOUND
	getAfter, err := lat.VarGet(ctx, icx.VarGetRequest{Key: key})
	if err != nil {
		t.Fatalf("VarGet unexpected error: %v", err)
	}
	if getAfter.OK || getAfter.Error != "VAR_NOT_FOUND" {
		t.Fatalf("Expected VAR_NOT_FOUND after TTL, got: %+v", getAfter)
	}

	// Verify VarList omits expired variable
	listAfter, err := lat.VarList(ctx, icx.VarListRequest{Prefix: "temp"})
	if err != nil || !listAfter.OK {
		t.Fatalf("VarList after expiry failed: %+v, err: %v", listAfter, err)
	}
	if listAfter.Count != 0 || len(listAfter.Variables) != 0 {
		t.Fatalf("Expected 0 variables after expiry, got %d", listAfter.Count)
	}

	// Re-create expired key with ExpectedVersion = 0 (treating expired key as 0)
	initVer := 0
	recreateRes, err := lat.VarSet(ctx, icx.VarSetRequest{
		Key:             key,
		Value:           "RECREATED",
		ExpectedVersion: &initVer,
		Writer:          "tester2",
	})
	if err != nil || !recreateRes.OK {
		t.Fatalf("Expected successful recreation of expired key: %+v, err: %v", recreateRes, err)
	}
	if recreateRes.Version != 1 {
		t.Fatalf("Expected version 1 after recreation, got %d", recreateRes.Version)
	}
}

// TestVarList_PrefixFilteringAndSorting verifies prefix filtering, deterministic
// ascending key sort order, and limit clamping.
func TestVarList_PrefixFilteringAndSorting(t *testing.T) {
	ctx := context.Background()
	lat := icx.NewLocalLattice()

	keys := []string{"alpha_2", "alpha_1", "beta_2", "beta_1", "gamma_1"}
	for _, k := range keys {
		res, err := lat.VarSet(ctx, icx.VarSetRequest{
			Key:    k,
			Value:  "val_" + k,
			Writer: "init",
		})
		if err != nil || !res.OK {
			t.Fatalf("Failed to set key %s: %+v, err: %v", k, res, err)
		}
	}

	// Test prefix "alpha_"
	listAlpha, err := lat.VarList(ctx, icx.VarListRequest{Prefix: "alpha_"})
	if err != nil || !listAlpha.OK {
		t.Fatalf("VarList prefix alpha_ failed: %+v, err: %v", listAlpha, err)
	}
	if listAlpha.Count != 2 || len(listAlpha.Variables) != 2 {
		t.Fatalf("Expected 2 alpha_ vars, got %d", listAlpha.Count)
	}
	if listAlpha.Variables[0].Key != "alpha_1" || listAlpha.Variables[1].Key != "alpha_2" {
		t.Fatalf("Expected ascending order [alpha_1, alpha_2], got [%s, %s]",
			listAlpha.Variables[0].Key, listAlpha.Variables[1].Key)
	}

	// Test prefix "beta_"
	listBeta, err := lat.VarList(ctx, icx.VarListRequest{Prefix: "beta_"})
	if err != nil || !listBeta.OK {
		t.Fatalf("VarList prefix beta_ failed: %+v, err: %v", listBeta, err)
	}
	if listBeta.Count != 2 || listBeta.Variables[0].Key != "beta_1" || listBeta.Variables[1].Key != "beta_2" {
		t.Fatalf("Expected [beta_1, beta_2], got %+v", listBeta.Variables)
	}

	// Test prefix "" (all variables)
	listAll, err := lat.VarList(ctx, icx.VarListRequest{})
	if err != nil || !listAll.OK {
		t.Fatalf("VarList all failed: %+v, err: %v", listAll, err)
	}
	if listAll.Count != 5 {
		t.Fatalf("Expected 5 total vars, got %d", listAll.Count)
	}
	expectedOrder := []string{"alpha_1", "alpha_2", "beta_1", "beta_2", "gamma_1"}
	for i, expKey := range expectedOrder {
		if listAll.Variables[i].Key != expKey {
			t.Errorf("At index %d: expected %s, got %s", i, expKey, listAll.Variables[i].Key)
		}
	}

	// Test nonexistent prefix
	listNone, err := lat.VarList(ctx, icx.VarListRequest{Prefix: "nonexistent"})
	if err != nil || !listNone.OK {
		t.Fatalf("VarList nonexistent failed: %+v, err: %v", listNone, err)
	}
	if listNone.Count != 0 || len(listNone.Variables) != 0 {
		t.Fatalf("Expected 0 vars for nonexistent prefix, got %d", listNone.Count)
	}

	// Test Limit clamping
	listLimit, err := lat.VarList(ctx, icx.VarListRequest{Limit: 2})
	if err != nil || !listLimit.OK {
		t.Fatalf("VarList limit failed: %+v, err: %v", listLimit, err)
	}
	if listLimit.Count != 2 || len(listLimit.Variables) != 2 {
		t.Fatalf("Expected clamped count=2, got %d", listLimit.Count)
	}
	if listLimit.Variables[0].Key != "alpha_1" || listLimit.Variables[1].Key != "alpha_2" {
		t.Fatalf("Expected first 2 sorted items, got %+v", listLimit.Variables)
	}

	// Verify Vars map contains keys
	if listLimit.Vars["alpha_1"] != "val_alpha_1" || listLimit.Vars["alpha_2"] != "val_alpha_2" {
		t.Fatalf("Expected Vars map to match values, got %+v", listLimit.Vars)
	}
}

// TestVarSet_PersistentNoTTL verifies that variables with TTL = 0 remain persistent and report RemainingTTLMs = -1.
func TestVarSet_PersistentNoTTL(t *testing.T) {
	ctx := context.Background()
	lat := icx.NewLocalLattice()

	key := "permanent_config"
	setRes, err := lat.VarSet(ctx, icx.VarSetRequest{
		Key:    key,
		Value:  "STATIC_VALUE",
		Writer: "admin",
		TTLMs:  0, // No TTL
	})
	if err != nil || !setRes.OK {
		t.Fatalf("VarSet failed: %+v, err: %v", setRes, err)
	}

	listRes, err := lat.VarList(ctx, icx.VarListRequest{Prefix: "permanent"})
	if err != nil || !listRes.OK || listRes.Count != 1 {
		t.Fatalf("VarList failed: %+v, err: %v", listRes, err)
	}
	if listRes.Variables[0].RemainingTTLMs != -1 {
		t.Fatalf("Expected RemainingTTLMs == -1 for persistent variable, got %d", listRes.Variables[0].RemainingTTLMs)
	}

	// Sleep 50ms and verify still active
	time.Sleep(50 * time.Millisecond)
	getRes, err := lat.VarGet(ctx, icx.VarGetRequest{Key: key})
	if err != nil || !getRes.OK {
		t.Fatalf("Persistent variable expired unexpectedly: %+v, err: %v", getRes, err)
	}
}

// TestVarSet_CASConflictOnExpiredKey verifies that attempting a CAS update with an old expected version
// against an expired key triggers CAS_CONFLICT.
func TestVarSet_CASConflictOnExpiredKey(t *testing.T) {
	ctx := context.Background()
	lat := icx.NewLocalLattice()

	key := "volatile_key"
	_, err := lat.VarSet(ctx, icx.VarSetRequest{
		Key:    key,
		Value:  "FIRST",
		TTLMs:  30,
		Writer: "w1",
	})
	if err != nil {
		t.Fatalf("VarSet failed: %v", err)
	}

	time.Sleep(45 * time.Millisecond)

	// Attempt CAS expecting version 1 — since it expired, current version is 0
	ver1 := 1
	resConflict, err := lat.VarSet(ctx, icx.VarSetRequest{
		Key:             key,
		Value:           "SECOND",
		ExpectedVersion: &ver1,
		Writer:          "w2",
	})
	if err != nil {
		t.Fatalf("VarSet unexpected error: %v", err)
	}
	if resConflict.OK || resConflict.Error != "CAS_CONFLICT" {
		t.Fatalf("Expected CAS_CONFLICT on expired key expecting v1, got: %+v", resConflict)
	}
	if resConflict.ActualVersion != 0 {
		t.Fatalf("Expected ActualVersion 0 for expired key, got %d", resConflict.ActualVersion)
	}
}

// TestVarCallTool_TTLAndVarList verifies icx_var_set with ttl_ms and icx_var_list through CallTool.
func TestVarCallTool_TTLAndVarList(t *testing.T) {
	ctx := context.Background()
	lat := icx.NewLocalLattice()

	// Call icx_var_set via CallTool with ttl_ms as float64 (typical from JSON unmarshal)
	callRes, err := lat.CallTool(ctx, "icx_var_set", map[string]any{
		"key":    "tool_var",
		"value":  "tool_val",
		"writer": "agent",
		"ttl_ms": float64(40),
	})
	if err != nil || callRes.IsError {
		t.Fatalf("CallTool icx_var_set failed: %+v, err: %v", callRes, err)
	}

	// Call icx_var_list via CallTool
	listCallRes, err := lat.CallTool(ctx, "icx_var_list", map[string]any{
		"prefix": "tool_",
	})
	if err != nil || listCallRes.IsError {
		t.Fatalf("CallTool icx_var_list failed: %+v, err: %v", listCallRes, err)
	}
	var listResp icx.VarListResponse
	if err := json.Unmarshal([]byte(listCallRes.Content[0].Text), &listResp); err != nil {
		t.Fatalf("Failed to parse icx_var_list response: %v", err)
	}
	if listResp.Count != 1 || listResp.Variables[0].Key != "tool_var" {
		t.Fatalf("Unexpected icx_var_list response: %+v", listResp)
	}

	// Wait for TTL expiration
	time.Sleep(55 * time.Millisecond)

	// Verify icx_var_get returns not found
	getCallRes, err := lat.CallTool(ctx, "icx_var_get", map[string]any{
		"key": "tool_var",
	})
	if err != nil || getCallRes.IsError {
		t.Fatalf("CallTool icx_var_get unexpected error: %+v, err: %v", getCallRes, err)
	}
	var getResp icx.VarGetResponse
	if err := json.Unmarshal([]byte(getCallRes.Content[0].Text), &getResp); err != nil {
		t.Fatalf("Failed to parse icx_var_get response: %v", err)
	}
	if getResp.OK || getResp.Error != "VAR_NOT_FOUND" {
		t.Fatalf("Expected VAR_NOT_FOUND, got %+v", getResp)
	}

	// Verify icx_var_list is empty
	listCallAfter, err := lat.CallTool(ctx, "icx_var_list", map[string]any{
		"prefix": "tool_",
	})
	if err != nil || listCallAfter.IsError {
		t.Fatalf("CallTool icx_var_list after expiry failed: %+v", listCallAfter)
	}
	var listRespAfter icx.VarListResponse
	_ = json.Unmarshal([]byte(listCallAfter.Content[0].Text), &listRespAfter)
	if listRespAfter.Count != 0 {
		t.Fatalf("Expected count=0 after TTL expiry, got %d", listRespAfter.Count)
	}
}

// TestClient_VarList_RemoteWireAndFallback verifies Client.VarList over wire JSON-RPC and offline fallback.
func TestClient_VarList_RemoteWireAndFallback(t *testing.T) {
	ctx := context.Background()

	// 1. Test offline fallback
	offlineClient, err := icx.NewClient(icx.ClientConfig{
		Offline: true,
	})
	if err != nil {
		t.Fatalf("Failed to create offline client: %v", err)
	}
	defer offlineClient.Close()

	_, err = offlineClient.VarSet(ctx, icx.VarSetRequest{
		Key:   "offline_var",
		Value: 999,
	})
	if err != nil {
		t.Fatalf("VarSet offline failed: %v", err)
	}

	offList, err := offlineClient.VarList(ctx, icx.VarListRequest{Prefix: "offline"})
	if err != nil || !offList.OK {
		t.Fatalf("VarList offline failed: %+v, err: %v", offList, err)
	}
	if offList.Count != 1 || offList.Variables[0].Key != "offline_var" {
		t.Fatalf("Unexpected offline VarList response: %+v", offList)
	}

	// 2. Test remote mock JSON-RPC wire dispatch
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var reqBody map[string]any
		_ = json.NewDecoder(r.Body).Decode(&reqBody)

		method, _ := reqBody["method"].(string)
		if method != "tools/call" {
			http.Error(w, "invalid method", http.StatusBadRequest)
			return
		}
		params, _ := reqBody["params"].(map[string]any)
		toolName, _ := params["name"].(string)
		if toolName != "icx_var_list" {
			http.Error(w, "unexpected tool", http.StatusBadRequest)
			return
		}

		resp := map[string]any{
			"jsonrpc": "2.0",
			"id":      reqBody["id"],
			"result": map[string]any{
				"content": []map[string]any{
					{
						"type": "text",
						"text": `{"ok":true,"count":1,"variables":[{"key":"remote_key","version":1,"remaining_ttl_ms":5000}]}`,
					},
				},
				"isError": false,
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer mockServer.Close()

	remoteClient, err := icx.NewClient(icx.ClientConfig{
		BaseURL:       mockServer.URL,
		APIKey:        "mock-token",
		TransportMode: "http",
		Strict:        true,
	})
	if err != nil {
		t.Fatalf("Failed to create remote client: %v", err)
	}
	defer remoteClient.Close()

	remoteList, err := remoteClient.VarList(ctx, icx.VarListRequest{Prefix: "remote"})
	if err != nil {
		t.Fatalf("Remote VarList wire dispatch failed: %v", err)
	}
	if !remoteList.OK || remoteList.Count != 1 || len(remoteList.Variables) != 1 {
		t.Fatalf("Unexpected remote VarList response: %+v", remoteList)
	}
	if remoteList.Variables[0].Key != "remote_key" {
		t.Fatalf("Expected remote_key, got %s", remoteList.Variables[0].Key)
	}
}

// TestVarTTL_ConcurrentContention_Race exercises 30 concurrent goroutines rapidly setting,
// getting, listing, and expiring variables to ensure 0 race conditions.
func TestVarTTL_ConcurrentContention_Race(t *testing.T) {
	ctx := context.Background()
	lat := icx.NewLocalLattice()

	const numWorkers = 30
	var wg sync.WaitGroup
	wg.Add(numWorkers)

	var opsCompleted atomic.Int64

	for i := 0; i < numWorkers; i++ {
		go func(workerID int) {
			defer wg.Done()
			for iter := 0; iter < 10; iter++ {
				key := fmt.Sprintf("race_var_%d", workerID%5)
				ttl := 20 + rand.Intn(30) // 20-50ms TTL

				// Set
				_, _ = lat.VarSet(ctx, icx.VarSetRequest{
					Key:    key,
					Value:  fmt.Sprintf("worker_%d_iter_%d", workerID, iter),
					Writer: fmt.Sprintf("worker_%d", workerID),
					TTLMs:  ttl,
				})

				// Get
				_, _ = lat.VarGet(ctx, icx.VarGetRequest{Key: key})

				// List
				_, _ = lat.VarList(ctx, icx.VarListRequest{
					Prefix: "race_var_",
					Limit:  5,
				})

				opsCompleted.Add(1)
				time.Sleep(time.Duration(1+rand.Intn(3)) * time.Millisecond)
			}
		}(i)
	}

	wg.Wait()

	if opsCompleted.Load() != numWorkers*10 {
		t.Fatalf("Expected %d operations completed, got %d", numWorkers*10, opsCompleted.Load())
	}
}
