// SPDX-License-Identifier: Apache-2.0
// Package icx_test implements empirical adversarial stress tests for remote connectors:
// 1. High-concurrency stress test with 50+ concurrent goroutines under -race:
//    - Simultaneous connector calls on offline client, online client, and direct LocalMemoryCache
//    - Interleaved with heavy lattice mutations (VarSet with TTL, VarGet, VarList, Remember, Recall ribbon, DispatchActuation)
// 2. CallTool adversarial & pathological argument parsing:
//    - nil args, empty args, type confusion, massive payloads, null bytes, nested maps
//    - Zero panics, strict IsError: true with remote-only message on offline/LocalMemoryCache
// 3. Complete binary hygiene AST verification:
//    - Prohibits server listeners (http.ListenAndServe, net.Listen, etc.) across cmd/ and pkg/
//    - Prohibits polling daemons / long-running background tickers in connector code
// 4. Goroutine leak & lifecycle verification:
//    - Zero permanent daemon goroutine leaks from connector invocations
// 5. Strict error isolation & non-fallback verification:
//    - Connectors NEVER fall back to local engine even with Strict: false

package icx_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"math"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/caleralabs/omnicoder-icx/pkg/icx"
)

// ============================================================================
// PART 1: High-Concurrency Race Stress (50+ Workers under -race)
// ============================================================================

func TestConnectorStress_HighConcurrency_50Workers_Race(t *testing.T) {
	ctx := context.Background()

	// 1. Mock Remote ICX Server for online client
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var rpcReq icx.JSONRPCRequest
		if err := json.NewDecoder(r.Body).Decode(&rpcReq); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		var callParams icx.MCPToolCallParams
		pBytes, _ := json.Marshal(rpcReq.Params)
		_ = json.Unmarshal(pBytes, &callParams)

		var payload any
		switch callParams.Name {
		case "icx_list_connectors":
			payload = icx.ListConnectorsResponse{
				OK:    true,
				Count: 2,
				Connectors: []icx.ConnectorInfo{
					{ID: "c_gh", Name: "GitHub", Type: "github", Status: "active"},
					{ID: "c_sl", Name: "Slack", Type: "slack", Status: "active"},
				},
			}
		case "icx_register_connector":
			payload = icx.RegisterConnectorResponse{
				OK:          true,
				ConnectorID: "c_new_wire",
				Status:      "registered",
			}
		case "icx_sync_audit":
			payload = icx.SyncAuditResponse{
				OK:    true,
				Count: 1,
				Records: []icx.SyncAuditRecord{
					{SyncID: "s1", ConnectorID: "c_gh", EventsProcessed: 10, CISSeal: "seal:123"},
				},
			}
		case "icx_trigger_sync":
			payload = icx.TriggerSyncResponse{
				OK:     true,
				SyncID: "sync_active_42",
				Status: "running",
			}
		case "icx_get_connector_document":
			payload = icx.GetConnectorDocumentResponse{
				OK:         true,
				DocumentID: "doc_100",
				Title:      "Spec",
				Content:    "High concurrency specification",
			}
		case "icx_inspect_connector_facts":
			payload = icx.InspectConnectorFactsResponse{
				OK:          true,
				ConnectorID: "c_gh",
				Count:       1,
				TotalFacts:  1,
				Facts: []icx.FactCard{
					{FactID: "f1", Text: "Fact verified", SHA256: "sha256:f1"},
				},
			}
		default:
			payload = map[string]any{"ok": true}
		}

		rawJSON, _ := json.Marshal(payload)
		mcpRes := icx.MCPToolCallResult{
			Content: []icx.MCPContent{{Type: "text", Text: string(rawJSON)}},
			IsError: false,
		}
		mcpBytes, _ := json.Marshal(mcpRes)

		resp := icx.JSONRPCResponse{
			JSONRPC: icx.JSONRPCVersion,
			ID:      rpcReq.ID,
			Result:  mcpBytes,
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer mockServer.Close()

	// 2. Shared resources
	offlineClient, err := icx.NewClient(icx.ClientConfig{
		SpaceID: "stress_space",
		Offline: true,
	})
	if err != nil {
		t.Fatalf("failed to create offline client: %v", err)
	}
	defer offlineClient.Close()

	onlineClient, err := icx.NewClient(icx.ClientConfig{
		BaseURL:       mockServer.URL,
		APIKey:        "test-key",
		SpaceID:       "stress_space",
		TransportMode: "http",
		Offline:       false,
		Strict:        false,
	})
	if err != nil {
		t.Fatalf("failed to create online client: %v", err)
	}
	defer onlineClient.Close()

	lat := icx.NewLocalMemoryCache("stress_space")

	// Pre-populate facts and vars
	for i := 0; i < 20; i++ {
		_, _ = lat.VarSet(ctx, icx.VarSetRequest{
			Key:    fmt.Sprintf("key_%d", i),
			Value:  fmt.Sprintf("val_%d", i),
			Writer: "init",
			TTLMs:  5000,
		})
		_, _ = lat.Remember(ctx, icx.RememberRequest{
			Text:   fmt.Sprintf("Lattice fact %d", i),
			Family: "facts",
		})
	}

	const numWorkers = 50
	const opsPerWorker = 30
	var wg sync.WaitGroup
	wg.Add(numWorkers)

	var (
		offlineRefusals atomic.Int64
		onlineSuccesses atomic.Int64
		latticeRefusals atomic.Int64
		toolCallsDone   atomic.Int64
		memoryOpsDone   atomic.Int64
	)

	startBarrier := make(chan struct{})

	for w := 0; w < numWorkers; w++ {
		go func(workerID int) {
			defer wg.Done()
			<-startBarrier // Synchronized high contention burst

			connectorTools := []string{
				"icx_list_connectors",
				"icx_register_connector",
				"icx_sync_audit",
				"icx_trigger_sync",
				"icx_get_connector_document",
				"icx_inspect_connector_facts",
			}

			rng := rand.New(rand.NewSource(int64(workerID * 10007)))

			for i := 0; i < opsPerWorker; i++ {
				op := rng.Intn(6)
				switch op {
				case 0:
					// Offline client direct methods
					if _, err := offlineClient.ListConnectors(ctx, icx.ListConnectorsRequest{SpaceID: "stress_space"}); errors.Is(err, icx.ErrRemoteOnly) {
						offlineRefusals.Add(1)
					}
					if _, err := offlineClient.RegisterConnector(ctx, icx.RegisterConnectorRequest{Name: "c", Type: "gh"}); errors.Is(err, icx.ErrRemoteOnly) {
						offlineRefusals.Add(1)
					}
					if _, err := offlineClient.SyncAudit(ctx, icx.SyncAuditRequest{ConnectorID: "c1"}); errors.Is(err, icx.ErrRemoteOnly) {
						offlineRefusals.Add(1)
					}
					if _, err := offlineClient.TriggerSync(ctx, icx.TriggerSyncRequest{ConnectorID: "c1"}); errors.Is(err, icx.ErrRemoteOnly) {
						offlineRefusals.Add(1)
					}
					if _, err := offlineClient.GetConnectorDocument(ctx, icx.GetConnectorDocumentRequest{ConnectorID: "c1", DocumentID: "d1"}); errors.Is(err, icx.ErrRemoteOnly) {
						offlineRefusals.Add(1)
					}
					if _, err := offlineClient.InspectConnectorFacts(ctx, icx.InspectConnectorFactsRequest{ConnectorID: "c1"}); errors.Is(err, icx.ErrRemoteOnly) {
						offlineRefusals.Add(1)
					}

				case 1:
					// Direct LocalMemoryCache connector methods
					if _, err := lat.ListConnectors(ctx, icx.ListConnectorsRequest{}); errors.Is(err, icx.ErrRemoteOnly) {
						latticeRefusals.Add(1)
					}
					if _, err := lat.RegisterConnector(ctx, icx.RegisterConnectorRequest{}); errors.Is(err, icx.ErrRemoteOnly) {
						latticeRefusals.Add(1)
					}
					if _, err := lat.SyncAudit(ctx, icx.SyncAuditRequest{}); errors.Is(err, icx.ErrRemoteOnly) {
						latticeRefusals.Add(1)
					}
					if _, err := lat.TriggerSync(ctx, icx.TriggerSyncRequest{}); errors.Is(err, icx.ErrRemoteOnly) {
						latticeRefusals.Add(1)
					}
					if _, err := lat.GetConnectorDocument(ctx, icx.GetConnectorDocumentRequest{}); errors.Is(err, icx.ErrRemoteOnly) {
						latticeRefusals.Add(1)
					}
					if _, err := lat.InspectConnectorFacts(ctx, icx.InspectConnectorFactsRequest{}); errors.Is(err, icx.ErrRemoteOnly) {
						latticeRefusals.Add(1)
					}

				case 2:
					// Online client direct methods
					if res, err := onlineClient.ListConnectors(ctx, icx.ListConnectorsRequest{SpaceID: "stress_space"}); err == nil && res.OK {
						onlineSuccesses.Add(1)
					}
					if res, err := onlineClient.RegisterConnector(ctx, icx.RegisterConnectorRequest{Name: "c_new"}); err == nil && res.OK {
						onlineSuccesses.Add(1)
					}
					if res, err := onlineClient.SyncAudit(ctx, icx.SyncAuditRequest{ConnectorID: "c_gh"}); err == nil && res.OK {
						onlineSuccesses.Add(1)
					}
					if res, err := onlineClient.TriggerSync(ctx, icx.TriggerSyncRequest{ConnectorID: "c_gh"}); err == nil && res.OK {
						onlineSuccesses.Add(1)
					}
					if res, err := onlineClient.GetConnectorDocument(ctx, icx.GetConnectorDocumentRequest{ConnectorID: "c_gh", DocumentID: "doc_100"}); err == nil && res.OK {
						onlineSuccesses.Add(1)
					}
					if res, err := onlineClient.InspectConnectorFacts(ctx, icx.InspectConnectorFactsRequest{ConnectorID: "c_gh"}); err == nil && res.OK {
						onlineSuccesses.Add(1)
					}

				case 3:
					// CallTool with connector tools across offline, online, and lattice
					toolName := connectorTools[rng.Intn(len(connectorTools))]

					// Offline CallTool
					if res, err := offlineClient.CallTool(ctx, toolName, map[string]any{"space_id": "stress_space"}); err == nil && res != nil && res.IsError {
						toolCallsDone.Add(1)
					}

					// Lattice CallTool
					if res, err := lat.CallTool(ctx, toolName, nil); err == nil && res != nil && res.IsError {
						toolCallsDone.Add(1)
					}

					// Online CallTool
					if res, err := onlineClient.CallTool(ctx, toolName, map[string]any{"space_id": "stress_space"}); err == nil && res != nil && !res.IsError {
						toolCallsDone.Add(1)
					}

				case 4:
					// Concurrent Swarm Variable mutations
					k := fmt.Sprintf("key_%d", rng.Intn(20))
					_, _ = lat.VarSet(ctx, icx.VarSetRequest{
						Key:    k,
						Value:  fmt.Sprintf("mut_%d_%d", workerID, i),
						Writer: fmt.Sprintf("worker_%d", workerID),
						TTLMs:  rng.Intn(2000),
					})
					_, _ = lat.VarGet(ctx, icx.VarGetRequest{Key: k})
					_, _ = lat.VarList(ctx, icx.VarListRequest{Prefix: "key_"})
					memoryOpsDone.Add(3)

				case 5:
					// Concurrent Memory / Ribbon / Actuation Receipts
					_, _ = lat.Remember(ctx, icx.RememberRequest{
						Text:   fmt.Sprintf("concurrent fact %d from worker %d", i, workerID),
						Family: "stress",
					})
					_, _ = lat.RecallScoped(ctx, icx.RecallRequest{
						Query:  "concurrent",
						Format: "ribbon",
						TopK:   5,
					})
					_, _ = lat.DispatchActuation(ctx, icx.DispatchActuationRequest{
						Surface: "engine",
						Action:  "stress_test",
						Payload: map[string]any{
							"file_modified": "test.go",
							"exit_code":     0,
							"sha256_seal":   fmt.Sprintf("seal_%d_%d", workerID, i),
						},
					})
					_ = lat.GetActuationReceipts("stress_space")
					memoryOpsDone.Add(4)
				}
			}
		}(w)
	}

	// Release all workers simultaneously
	close(startBarrier)
	wg.Wait()

	t.Logf("High-concurrency stress test complete: offlineRefusals=%d, latticeRefusals=%d, onlineSuccesses=%d, toolCallsDone=%d, memoryOpsDone=%d",
		offlineRefusals.Load(), latticeRefusals.Load(), onlineSuccesses.Load(), toolCallsDone.Load(), memoryOpsDone.Load())

	if offlineRefusals.Load() == 0 {
		t.Errorf("expected offlineRefusals > 0, got 0")
	}
	if latticeRefusals.Load() == 0 {
		t.Errorf("expected latticeRefusals > 0, got 0")
	}
	if onlineSuccesses.Load() == 0 {
		t.Errorf("expected onlineSuccesses > 0, got 0")
	}
	if toolCallsDone.Load() == 0 {
		t.Errorf("expected toolCallsDone > 0, got 0")
	}
	if memoryOpsDone.Load() == 0 {
		t.Errorf("expected memoryOpsDone > 0, got 0")
	}
}

// ============================================================================
// PART 2: CallTool Adversarial & Pathological Argument Parsing
// ============================================================================

func TestCallTool_PathologicalArgs_NoPanic(t *testing.T) {
	ctx := context.Background()

	// Setup offline client and LocalMemoryCache
	offlineClient, err := icx.NewClient(icx.ClientConfig{
		SpaceID: "patho_space",
		Offline: true,
	})
	if err != nil {
		t.Fatalf("failed to create offline client: %v", err)
	}
	defer offlineClient.Close()

	// Mock server for online client
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var rpcReq icx.JSONRPCRequest
		_ = json.NewDecoder(r.Body).Decode(&rpcReq)
		mcpRes := icx.MCPToolCallResult{
			Content: []icx.MCPContent{{Type: "text", Text: `{"ok":true,"message":"processed"}`}},
			IsError: false,
		}
		mcpBytes, _ := json.Marshal(mcpRes)
		resp := icx.JSONRPCResponse{
			JSONRPC: icx.JSONRPCVersion,
			ID:      rpcReq.ID,
			Result:  mcpBytes,
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer mockServer.Close()

	onlineClient, err := icx.NewClient(icx.ClientConfig{
		BaseURL:       mockServer.URL,
		APIKey:        "test-key",
		SpaceID:       "patho_space",
		TransportMode: "http",
		Offline:       false,
	})
	if err != nil {
		t.Fatalf("failed to create online client: %v", err)
	}
	defer onlineClient.Close()

	lat := icx.NewLocalMemoryCache("patho_space")

	connectorTools := []string{
		"icx_list_connectors",
		"icx_register_connector",
		"icx_sync_audit",
		"icx_trigger_sync",
		"icx_get_connector_document",
		"icx_inspect_connector_facts",
	}

	// Pathological argument test suites
	giantString := strings.Repeat("A", 100000)
	adversarialArgs := []struct {
		desc string
		args map[string]any
	}{
		{desc: "nil args map", args: nil},
		{desc: "empty args map", args: map[string]any{}},
		{
			desc: "alien prototype and injected keys",
			args: map[string]any{
				"__proto__":       "polluted",
				"constructor":     map[string]any{"prototype": "danger"},
				"unknown_payload": 99999,
				"raw_exec":        "rm -rf /",
			},
		},
		{
			desc: "type confusion on connector identifiers",
			args: map[string]any{
				"space_id":     12345,
				"connector_id": true,
				"document_id":  []int{1, 2, 3},
				"name":         map[string]any{"foo": "bar"},
				"type":         3.14159,
			},
		},
		{
			desc: "massive payload values",
			args: map[string]any{
				"space_id":     giantString,
				"connector_id": giantString,
				"config":       map[string]any{"huge": giantString},
			},
		},
		{
			desc: "null bytes and control characters",
			args: map[string]any{
				"space_id":     "space\x00\xff\xfe\r\n\t",
				"connector_id": "c\x00_id\u0000",
				"document_id":  "\x00\x00\x00",
				"prefix":       "\r\n\x1b[31mRED\x1b[0m",
			},
		},
		{
			desc: "extreme numeric boundaries",
			args: map[string]any{
				"limit":        math.MaxInt64,
				"ttl_ms":       -999999,
				"top_k":        math.MinInt32,
				"force":        "not_a_bool",
				"connector_id": -1,
			},
		},
		{
			desc: "deeply nested dictionary",
			args: map[string]any{
				"config": map[string]any{
					"l1": map[string]any{
						"l2": map[string]any{
							"l3": map[string]any{
								"l4": map[string]any{
									"l5": []any{1, "two", 3.0, nil, map[string]any{"deep": true}},
								},
							},
						},
					},
				},
			},
		},
	}

	for _, tool := range connectorTools {
		for _, tc := range adversarialArgs {
			t.Run(fmt.Sprintf("%s_%s_Lattice", tool, tc.desc), func(t *testing.T) {
				defer func() {
					if r := recover(); r != nil {
						t.Fatalf("PANIC on LocalMemoryCache.CallTool(%q) with %s: %v", tool, tc.desc, r)
					}
				}()

				res, err := lat.CallTool(ctx, tool, tc.args)
				if err != nil {
					t.Fatalf("unexpected error from CallTool: %v", err)
				}
				if res == nil {
					t.Fatalf("expected non-nil MCPToolCallResult")
				}
				if !res.IsError {
					t.Errorf("expected res.IsError: true, got %+v", res)
				}
				if len(res.Content) == 0 || !strings.Contains(res.Content[0].Text, "remote-only") {
					t.Errorf("expected remote-only warning message, got %+v", res.Content)
				}
			})

			t.Run(fmt.Sprintf("%s_%s_OfflineClient", tool, tc.desc), func(t *testing.T) {
				defer func() {
					if r := recover(); r != nil {
						t.Fatalf("PANIC on Client.CallTool(%q) with %s: %v", tool, tc.desc, r)
					}
				}()

				res, err := offlineClient.CallTool(ctx, tool, tc.args)
				if err != nil {
					t.Fatalf("unexpected error from CallTool: %v", err)
				}
				if res == nil {
					t.Fatalf("expected non-nil MCPToolCallResult")
				}
				if !res.IsError {
					t.Errorf("expected res.IsError: true, got %+v", res)
				}
				if len(res.Content) == 0 || !strings.Contains(res.Content[0].Text, "remote-only") {
					t.Errorf("expected remote-only warning message, got %+v", res.Content)
				}
			})

			t.Run(fmt.Sprintf("%s_%s_OnlineClient", tool, tc.desc), func(t *testing.T) {
				defer func() {
					if r := recover(); r != nil {
						t.Fatalf("PANIC on OnlineClient.CallTool(%q) with %s: %v", tool, tc.desc, r)
					}
				}()

				res, err := onlineClient.CallTool(ctx, tool, tc.args)
				if err != nil {
					// Network or serialization errors are acceptable for pathological payloads, but never panics
					return
				}
				if res == nil {
					t.Fatalf("expected non-nil MCPToolCallResult")
				}
			})
		}
	}
}

// ============================================================================
// PART 3: Binary Hygiene Verification across Workspace
// ============================================================================

func TestBinaryHygiene_CodebaseAudit(t *testing.T) {
	// Root dirs in repository
	scanDirs := []string{
		filepath.Join("..", "..", "cmd"),
		filepath.Join("..", "..", "pkg"),
	}

	forbiddenServerCalls := []string{
		"http.ListenAndServe",
		"http.ListenAndServeTLS",
		"net.Listen",
		"net.ListenTCP",
		"net.ListenUDP",
		"net.ListenIP",
		"net.ListenUnix",
	}

	fset := token.NewFileSet()
	var scannedFiles int

	for _, dir := range scanDirs {
		absDir, err := filepath.Abs(dir)
		if err != nil {
			t.Fatalf("failed to get abs path for %s: %v", dir, err)
		}
		if _, err := os.Stat(absDir); os.IsNotExist(err) {
			continue
		}

		err = filepath.Walk(absDir, func(path string, info os.FileInfo, walkErr error) error {
			if walkErr != nil {
				return nil
			}
			if info.IsDir() {
				if info.Name() == ".worktrees" || info.Name() == ".omnicoder" {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") {
				return nil
			}
			// Skip test files
			if strings.HasSuffix(path, "_test.go") {
				return nil
			}

			scannedFiles++
			node, parseErr := parser.ParseFile(fset, path, nil, parser.ParseComments)
			if parseErr != nil {
				t.Fatalf("failed to parse file %s: %v", path, parseErr)
			}

			// AST inspect call expressions
			ast.Inspect(node, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}

				var callName string
				switch fun := call.Fun.(type) {
				case *ast.SelectorExpr:
					if id, ok := fun.X.(*ast.Ident); ok {
						callName = id.Name + "." + fun.Sel.Name
					}
				}

				for _, forbidden := range forbiddenServerCalls {
					if callName == forbidden {
						t.Errorf("Binary Hygiene Violation: Found forbidden listener call %q in %s:%d",
							forbidden, path, fset.Position(call.Pos()).Line)
					}
				}
				return true
			})

			// Content search for connector polling daemons
			contentBytes, readErr := os.ReadFile(path)
			if readErr != nil {
				return nil
			}
			contentStr := string(contentBytes)

			// Prohibit connector polling daemons
			if strings.Contains(path, "connector") || strings.Contains(path, "client.go") {
				if strings.Contains(contentStr, "time.NewTicker") || strings.Contains(contentStr, "time.Tick(") {
					t.Errorf("Binary Hygiene Violation: Found background ticker in connector file %s", path)
				}
			}

			return nil
		})

		if err != nil {
			t.Errorf("error during AST walk of %s: %v", absDir, err)
		}
	}

	if scannedFiles == 0 {
		t.Fatalf("expected scannedFiles > 0, got 0")
	}
	t.Logf("Binary hygiene verified: successfully audited %d production Go files with 0 violations.", scannedFiles)
}

// ============================================================================
// PART 4: Goroutine Leak & Lifecycle Verification
// ============================================================================

func TestGoroutineLeak_ConnectorCalls(t *testing.T) {
	ctx := context.Background()

	// Stabilize runtime goroutines
	runtime.GC()
	time.Sleep(50 * time.Millisecond)
	initialGoroutines := runtime.NumGoroutine()

	client, err := icx.NewClient(icx.ClientConfig{
		SpaceID: "leak_test",
		Offline: true,
	})
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}

	lat := icx.NewLocalMemoryCache("leak_test")

	// Execute 300 connector operations
	for i := 0; i < 50; i++ {
		_, _ = client.ListConnectors(ctx, icx.ListConnectorsRequest{})
		_, _ = client.RegisterConnector(ctx, icx.RegisterConnectorRequest{})
		_, _ = client.SyncAudit(ctx, icx.SyncAuditRequest{})
		_, _ = client.TriggerSync(ctx, icx.TriggerSyncRequest{})
		_, _ = client.GetConnectorDocument(ctx, icx.GetConnectorDocumentRequest{})
		_, _ = client.InspectConnectorFacts(ctx, icx.InspectConnectorFactsRequest{})

		_, _ = lat.ListConnectors(ctx, icx.ListConnectorsRequest{})
		_, _ = lat.RegisterConnector(ctx, icx.RegisterConnectorRequest{})
		_, _ = lat.SyncAudit(ctx, icx.SyncAuditRequest{})
		_, _ = lat.TriggerSync(ctx, icx.TriggerSyncRequest{})
		_, _ = lat.GetConnectorDocument(ctx, icx.GetConnectorDocumentRequest{})
		_, _ = lat.InspectConnectorFacts(ctx, icx.InspectConnectorFactsRequest{})

		_, _ = client.CallTool(ctx, "icx_list_connectors", nil)
		_, _ = lat.CallTool(ctx, "icx_list_connectors", nil)
	}

	_ = client.Close()

	runtime.GC()
	time.Sleep(50 * time.Millisecond)
	finalGoroutines := runtime.NumGoroutine()

	// Goroutines should not grow significantly
	if delta := finalGoroutines - initialGoroutines; delta > 4 {
		t.Errorf("Goroutine leak detected: initial=%d, final=%d, delta=%d", initialGoroutines, finalGoroutines, delta)
	}
}

// ============================================================================
// PART 5: Strict Error Isolation (No Local Fallback on Remote Failure)
// ============================================================================

func TestStrictErrorIsolation_NoFallback(t *testing.T) {
	ctx := context.Background()

	// Mock server that returns HTTP 503 Service Unavailable
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "upstream service unavailable", http.StatusServiceUnavailable)
	}))
	defer mockServer.Close()

	// Client configured with Strict: false
	// For standard tools (e.g. remember), it falls back to localEngine.
	// But for connector tools, it MUST return the remote error directly and NEVER fallback!
	client, err := icx.NewClient(icx.ClientConfig{
		BaseURL:       mockServer.URL,
		APIKey:        "test-key",
		SpaceID:       "isolation_space",
		TransportMode: "http",
		Offline:       false,
		Strict:        false, // Explicitly non-strict!
	})
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}
	defer client.Close()

	// 1. Connector operations MUST fail with HTTP error and NEVER fall back to local ErrRemoteOnly
	_, err = client.ListConnectors(ctx, icx.ListConnectorsRequest{})
	if err == nil {
		t.Fatalf("expected error from ListConnectors, got nil")
	}
	if errors.Is(err, icx.ErrRemoteOnly) {
		t.Errorf("VIOLATION: ListConnectors fell back to local engine! Got ErrRemoteOnly instead of remote HTTP error: %v", err)
	}
	if !strings.Contains(err.Error(), "503") {
		t.Errorf("expected 503 error, got: %v", err)
	}

	_, err = client.RegisterConnector(ctx, icx.RegisterConnectorRequest{})
	if err == nil || errors.Is(err, icx.ErrRemoteOnly) || !strings.Contains(err.Error(), "503") {
		t.Errorf("RegisterConnector fallback violation: %v", err)
	}

	_, err = client.SyncAudit(ctx, icx.SyncAuditRequest{})
	if err == nil || errors.Is(err, icx.ErrRemoteOnly) || !strings.Contains(err.Error(), "503") {
		t.Errorf("SyncAudit fallback violation: %v", err)
	}

	_, err = client.TriggerSync(ctx, icx.TriggerSyncRequest{})
	if err == nil || errors.Is(err, icx.ErrRemoteOnly) || !strings.Contains(err.Error(), "503") {
		t.Errorf("TriggerSync fallback violation: %v", err)
	}

	_, err = client.GetConnectorDocument(ctx, icx.GetConnectorDocumentRequest{})
	if err == nil || errors.Is(err, icx.ErrRemoteOnly) || !strings.Contains(err.Error(), "503") {
		t.Errorf("GetConnectorDocument fallback violation: %v", err)
	}

	_, err = client.InspectConnectorFacts(ctx, icx.InspectConnectorFactsRequest{})
	if err == nil || errors.Is(err, icx.ErrRemoteOnly) || !strings.Contains(err.Error(), "503") {
		t.Errorf("InspectConnectorFacts fallback violation: %v", err)
	}

	// CallTool for connector tool
	_, err = client.CallTool(ctx, "icx_list_connectors", map[string]any{})
	if err == nil || errors.Is(err, icx.ErrRemoteOnly) || !strings.Contains(err.Error(), "503") {
		t.Errorf("CallTool(icx_list_connectors) fallback violation: %v", err)
	}

	// 2. In contrast, standard tools (like icx_remember) SHOULD fall back to local engine when Strict: false
	remResp, err := client.Remember(ctx, icx.RememberRequest{Text: "fallback fact", Family: "test"})
	if err != nil {
		t.Fatalf("expected Remember to fallback to local engine, got error: %v", err)
	}
	if remResp == nil || remResp.FactID == "" {
		t.Errorf("expected successful local Remember response, got %+v", remResp)
	}
}
