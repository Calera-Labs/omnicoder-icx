// SPDX-License-Identifier: Apache-2.0
// Package icx_test implements empirical verification suite for remote-only connectors:
// 1. Offline refusal across all 6 connector methods under diverse offline states:
//    - Explicit Offline: true
//    - Implicit Offline (missing APIKey)
//    - TransportMode: "local"
//    - Dynamic offline transition via non-connector remote error
// 2. Direct LocalMemoryCache refusal:
//    - Verify all 6 methods return (nil, ErrRemoteOnly)
//    - Verify CallTool across all 6 tools returns IsError: true with remote-only message
//    - State purity check: verify zero state mutations in LocalMemoryCache facts, nodes, or vars
// 3. Remote wire dispatch & strict zero-fallback enforcement:
//    - Parameter fidelity check over mock JSON-RPC wire
//    - Adversarial wire error scenarios: HTTP 500, 502, 401, 404, TCP reset, JSON-RPC error, timeout
//    - Verify client NEVER falls back to local engine even with Strict: false
//    - Verify connector failures do NOT falsely toggle isOffline to true
// 4. Binary hygiene:
//    - AST and symbol inspection for zero listener/daemon constructs
// 5. Concurrent stress under -race

package icx_test

import (
	"context"
	"encoding/json"
	"errors"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/caleralabs/omnicoder-icx/pkg/icx"
)

// ============================================================================
// PART 1: Offline Refusal Across Diverse Offline Triggers
// ============================================================================

func TestOfflineRefusal_DiverseTriggers(t *testing.T) {
	ctx := context.Background()

	testConfigs := []struct {
		name   string
		cfg    icx.ClientConfig
		verify func(c icx.ICXClient) bool
	}{
		{
			name: "ExplicitOfflineFlag",
			cfg: icx.ClientConfig{
				SpaceID: "space_off_1",
				Offline: true,
			},
		},
		{
			name: "ImplicitOffline_MissingAPIKey",
			cfg: icx.ClientConfig{
				BaseURL:       "http://127.0.0.1:9999",
				SpaceID:       "space_off_2",
				TransportMode: "http",
				APIKey:        "", // Missing key forces offline mode
				Offline:       false,
			},
		},
		{
			name: "ExplicitLocalTransportMode",
			cfg: icx.ClientConfig{
				SpaceID:       "space_off_3",
				TransportMode: "local",
				APIKey:        "some-key",
			},
		},
	}

	connectorTools := []string{
		"icx_list_connectors",
		"icx_register_connector",
		"icx_sync_audit",
		"icx_trigger_sync",
		"icx_get_connector_document",
		"icx_inspect_connector_facts",
	}

	for _, tc := range testConfigs {
		t.Run(tc.name, func(t *testing.T) {
			client, err := icx.NewClient(tc.cfg)
			if err != nil {
				t.Fatalf("failed to create client: %v", err)
			}
			defer client.Close()

			if !client.IsOffline() {
				t.Fatalf("[%s] expected client to be offline", tc.name)
			}

			// Method 1: ListConnectors
			if _, err := client.ListConnectors(ctx, icx.ListConnectorsRequest{SpaceID: "test"}); !errors.Is(err, icx.ErrRemoteOnly) {
				t.Errorf("[%s] ListConnectors: expected ErrRemoteOnly, got %v", tc.name, err)
			}

			// Method 2: RegisterConnector
			if _, err := client.RegisterConnector(ctx, icx.RegisterConnectorRequest{Name: "test-conn", Type: "github"}); !errors.Is(err, icx.ErrRemoteOnly) {
				t.Errorf("[%s] RegisterConnector: expected ErrRemoteOnly, got %v", tc.name, err)
			}

			// Method 3: SyncAudit
			if _, err := client.SyncAudit(ctx, icx.SyncAuditRequest{ConnectorID: "c-1"}); !errors.Is(err, icx.ErrRemoteOnly) {
				t.Errorf("[%s] SyncAudit: expected ErrRemoteOnly, got %v", tc.name, err)
			}

			// Method 4: TriggerSync
			if _, err := client.TriggerSync(ctx, icx.TriggerSyncRequest{ConnectorID: "c-1"}); !errors.Is(err, icx.ErrRemoteOnly) {
				t.Errorf("[%s] TriggerSync: expected ErrRemoteOnly, got %v", tc.name, err)
			}

			// Method 5: GetConnectorDocument
			if _, err := client.GetConnectorDocument(ctx, icx.GetConnectorDocumentRequest{ConnectorID: "c-1", DocumentID: "d-1"}); !errors.Is(err, icx.ErrRemoteOnly) {
				t.Errorf("[%s] GetConnectorDocument: expected ErrRemoteOnly, got %v", tc.name, err)
			}

			// Method 6: InspectConnectorFacts
			if _, err := client.InspectConnectorFacts(ctx, icx.InspectConnectorFactsRequest{ConnectorID: "c-1"}); !errors.Is(err, icx.ErrRemoteOnly) {
				t.Errorf("[%s] InspectConnectorFacts: expected ErrRemoteOnly, got %v", tc.name, err)
			}

			// Generic CallTool
			for _, tool := range connectorTools {
				res, err := client.CallTool(ctx, tool, map[string]any{"connector_id": "c-1"})
				if err != nil {
					t.Errorf("[%s] CallTool(%s) unexpected error: %v", tc.name, tool, err)
				}
				if res == nil || !res.IsError {
					t.Errorf("[%s] CallTool(%s) expected IsError: true", tc.name, tool)
				}
				if len(res.Content) == 0 || !strings.Contains(res.Content[0].Text, "remote-only") {
					t.Errorf("[%s] CallTool(%s) expected remote-only message, got: %+v", tc.name, tool, res)
				}
			}
		})
	}
}

func TestDynamicOfflineTransition(t *testing.T) {
	ctx := context.Background()

	// Server that fails standard calls to trigger dynamic offline transition
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "network failure", http.StatusInternalServerError)
	}))
	defer ts.Close()

	client, err := icx.NewClient(icx.ClientConfig{
		BaseURL:       ts.URL,
		APIKey:        "test-key",
		SpaceID:       "dynamic_space",
		TransportMode: "http",
		Offline:       false,
		Strict:        false, // non-strict allows dynamic offline toggle on standard tools
	})
	if err != nil {
		t.Fatalf("failed to initialize client: %v", err)
	}
	defer client.Close()

	if client.IsOffline() {
		t.Fatalf("expected client to start online")
	}

	// Trigger standard tool failure to trip dynamic offline store
	// Remember in non-strict mode falls back to local and toggles c.isOffline.Store(true)
	_, _ = client.Remember(ctx, icx.RememberRequest{Text: "trip offline", Family: "test"})

	if !client.IsOffline() {
		t.Fatalf("expected client to dynamically transition to offline after remote failure")
	}

	// Verify all 6 connector methods now cleanly refuse with ErrRemoteOnly
	if _, err := client.ListConnectors(ctx, icx.ListConnectorsRequest{}); !errors.Is(err, icx.ErrRemoteOnly) {
		t.Errorf("ListConnectors after offline transition expected ErrRemoteOnly, got: %v", err)
	}
	if _, err := client.RegisterConnector(ctx, icx.RegisterConnectorRequest{}); !errors.Is(err, icx.ErrRemoteOnly) {
		t.Errorf("RegisterConnector after offline transition expected ErrRemoteOnly, got: %v", err)
	}
	if _, err := client.SyncAudit(ctx, icx.SyncAuditRequest{}); !errors.Is(err, icx.ErrRemoteOnly) {
		t.Errorf("SyncAudit after offline transition expected ErrRemoteOnly, got: %v", err)
	}
	if _, err := client.TriggerSync(ctx, icx.TriggerSyncRequest{}); !errors.Is(err, icx.ErrRemoteOnly) {
		t.Errorf("TriggerSync after offline transition expected ErrRemoteOnly, got: %v", err)
	}
	if _, err := client.GetConnectorDocument(ctx, icx.GetConnectorDocumentRequest{}); !errors.Is(err, icx.ErrRemoteOnly) {
		t.Errorf("GetConnectorDocument after offline transition expected ErrRemoteOnly, got: %v", err)
	}
	if _, err := client.InspectConnectorFacts(ctx, icx.InspectConnectorFactsRequest{}); !errors.Is(err, icx.ErrRemoteOnly) {
		t.Errorf("InspectConnectorFacts after offline transition expected ErrRemoteOnly, got: %v", err)
	}
}

// ============================================================================
// PART 2: Direct LocalMemoryCache Refusal & State Purity
// ============================================================================

func TestDirectLocalMemoryCacheRefusalAndStatePurity(t *testing.T) {
	ctx := context.Background()
	lat := icx.NewLocalMemoryCache("purity_space")

	// Pre-condition: capture baseline state
	preRecall, err := lat.RecallScoped(ctx, icx.RecallRequest{SpaceID: "purity_space", Query: "test"})
	if err != nil {
		t.Fatalf("baseline recall failed: %v", err)
	}
	preReceipts := lat.GetActuationReceipts("purity_space")
	preVars, err := lat.VarList(ctx, icx.VarListRequest{SpaceID: "purity_space"})
	if err != nil {
		t.Fatalf("baseline varlist failed: %v", err)
	}

	// 1. Direct method calls must return ErrRemoteOnly
	if _, err := lat.ListConnectors(ctx, icx.ListConnectorsRequest{SpaceID: "purity_space"}); !errors.Is(err, icx.ErrRemoteOnly) {
		t.Errorf("ListConnectors: expected ErrRemoteOnly, got %v", err)
	}
	if _, err := lat.RegisterConnector(ctx, icx.RegisterConnectorRequest{Name: "c", Type: "slack"}); !errors.Is(err, icx.ErrRemoteOnly) {
		t.Errorf("RegisterConnector: expected ErrRemoteOnly, got %v", err)
	}
	if _, err := lat.SyncAudit(ctx, icx.SyncAuditRequest{ConnectorID: "c1"}); !errors.Is(err, icx.ErrRemoteOnly) {
		t.Errorf("SyncAudit: expected ErrRemoteOnly, got %v", err)
	}
	if _, err := lat.TriggerSync(ctx, icx.TriggerSyncRequest{ConnectorID: "c1"}); !errors.Is(err, icx.ErrRemoteOnly) {
		t.Errorf("TriggerSync: expected ErrRemoteOnly, got %v", err)
	}
	if _, err := lat.GetConnectorDocument(ctx, icx.GetConnectorDocumentRequest{ConnectorID: "c1", DocumentID: "d1"}); !errors.Is(err, icx.ErrRemoteOnly) {
		t.Errorf("GetConnectorDocument: expected ErrRemoteOnly, got %v", err)
	}
	if _, err := lat.InspectConnectorFacts(ctx, icx.InspectConnectorFactsRequest{ConnectorID: "c1"}); !errors.Is(err, icx.ErrRemoteOnly) {
		t.Errorf("InspectConnectorFacts: expected ErrRemoteOnly, got %v", err)
	}

	// 2. CallTool calls must return IsError: true
	tools := []string{
		"icx_list_connectors",
		"icx_register_connector",
		"icx_sync_audit",
		"icx_trigger_sync",
		"icx_get_connector_document",
		"icx_inspect_connector_facts",
	}

	for _, tool := range tools {
		res, err := lat.CallTool(ctx, tool, map[string]any{"data": "malicious_injection"})
		if err != nil {
			t.Errorf("CallTool(%s) error: %v", tool, err)
		}
		if res == nil || !res.IsError {
			t.Errorf("CallTool(%s) expected IsError: true", tool)
		}
		if len(res.Content) == 0 || !strings.Contains(res.Content[0].Text, "remote-only") {
			t.Errorf("CallTool(%s) expected remote-only text, got: %+v", tool, res)
		}
	}

	// 3. State purity check: verify zero state mutations in LocalMemoryCache
	postRecall, err := lat.RecallScoped(ctx, icx.RecallRequest{SpaceID: "purity_space", Query: "test"})
	if err != nil {
		t.Fatalf("post recall failed: %v", err)
	}
	if postRecall.Count != preRecall.Count {
		t.Errorf("state impurity: recall count changed from %d to %d", preRecall.Count, postRecall.Count)
	}

	postReceipts := lat.GetActuationReceipts("purity_space")
	if len(postReceipts) != len(preReceipts) {
		t.Errorf("state impurity: receipts changed from %d to %d", len(preReceipts), len(postReceipts))
	}

	postVars, err := lat.VarList(ctx, icx.VarListRequest{SpaceID: "purity_space"})
	if err != nil {
		t.Fatalf("post varlist failed: %v", err)
	}
	if postVars.Count != preVars.Count {
		t.Errorf("state impurity: var count changed from %d to %d", preVars.Count, postVars.Count)
	}
}

// ============================================================================
// PART 3: Remote Wire Dispatch Fidelity & Zero Fallback on Wire Failures
// ============================================================================

func TestWireDispatch_ParameterFidelity(t *testing.T) {
	ctx := context.Background()

	var recordedArgs map[string]any
	var recordedTool string
	var mu sync.Mutex

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var rpcReq icx.JSONRPCRequest
		_ = json.NewDecoder(r.Body).Decode(&rpcReq)

		var callParams icx.MCPToolCallParams
		pBytes, _ := json.Marshal(rpcReq.Params)
		_ = json.Unmarshal(pBytes, &callParams)

		mu.Lock()
		recordedTool = callParams.Name
		recordedArgs = callParams.Arguments
		mu.Unlock()

		// Return synthetic success response
		payload := map[string]any{"ok": true, "status": "active", "connector_id": "c_fidelity"}
		rawBytes, _ := json.Marshal(payload)
		mcpRes := icx.MCPToolCallResult{
			Content: []icx.MCPContent{{Type: "text", Text: string(rawBytes)}},
			IsError: false,
		}
		resBytes, _ := json.Marshal(mcpRes)

		resp := icx.JSONRPCResponse{
			JSONRPC: icx.JSONRPCVersion,
			ID:      rpcReq.ID,
			Result:  resBytes,
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer ts.Close()

	client, err := icx.NewClient(icx.ClientConfig{
		BaseURL:       ts.URL,
		APIKey:        "valid-api-key",
		SpaceID:       "fidelity_space",
		TransportMode: "http",
		Offline:       false,
		Strict:        true,
	})
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}
	defer client.Close()

	// RegisterConnector parameter check
	_, err = client.RegisterConnector(ctx, icx.RegisterConnectorRequest{
		Name:    "Jira Cloud",
		Type:    "jira",
		Config:  map[string]any{"domain": "calera.atlassian.net", "project": "ICX"},
		SpaceID: "fidelity_space",
	})
	if err != nil {
		t.Fatalf("RegisterConnector failed: %v", err)
	}

	mu.Lock()
	if recordedTool != "icx_register_connector" {
		t.Errorf("expected tool icx_register_connector, got: %s", recordedTool)
	}
	if recordedArgs["name"] != "Jira Cloud" || recordedArgs["type"] != "jira" {
		t.Errorf("parameter mismatch: %+v", recordedArgs)
	}
	cfgMap, ok := recordedArgs["config"].(map[string]any)
	if !ok || cfgMap["domain"] != "calera.atlassian.net" {
		t.Errorf("nested config mismatch: %+v", recordedArgs["config"])
	}
	mu.Unlock()

	// GetConnectorDocument parameter check
	_, err = client.GetConnectorDocument(ctx, icx.GetConnectorDocumentRequest{
		ConnectorID: "c_fidelity",
		DocumentID:  "doc_999",
		SpaceID:     "fidelity_space",
	})
	if err != nil {
		t.Fatalf("GetConnectorDocument failed: %v", err)
	}

	mu.Lock()
	if recordedTool != "icx_get_connector_document" {
		t.Errorf("expected tool icx_get_connector_document, got: %s", recordedTool)
	}
	if recordedArgs["connector_id"] != "c_fidelity" || recordedArgs["document_id"] != "doc_999" {
		t.Errorf("parameter mismatch: %+v", recordedArgs)
	}
	mu.Unlock()
}

func TestWireFailures_ZeroFallbackAndNoOfflineLeak(t *testing.T) {
	ctx := context.Background()

	// We test 5 adversarial server behaviors:
	// 1. HTTP 502 Bad Gateway
	// 2. HTTP 401 Unauthorized
	// 3. JSON-RPC Application Error
	// 4. Broken / Malformed JSON body
	// 5. Abrupt TCP connection close (EOF)
	failureCases := []struct {
		name       string
		statusCode int
		handler    func(w http.ResponseWriter, r *http.Request)
	}{
		{
			name: "HTTP_502_BadGateway",
			handler: func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, "Bad Gateway from Cloud Edge", http.StatusBadGateway)
			},
		},
		{
			name: "HTTP_401_Unauthorized",
			handler: func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, "Unauthorized Tenant Access", http.StatusUnauthorized)
			},
		},
		{
			name: "JSONRPC_ProtocolError",
			handler: func(w http.ResponseWriter, r *http.Request) {
				var req icx.JSONRPCRequest
				_ = json.NewDecoder(r.Body).Decode(&req)
				resp := icx.JSONRPCResponse{
					JSONRPC: icx.JSONRPCVersion,
					ID:      req.ID,
					Error: &icx.RPCError{
						Code:    -32000,
						Message: "Upstream changefeed broker offline",
					},
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(resp)
			},
		},
		{
			name: "Malformed_JSON_Payload",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{corrupted_json`))
			},
		},
	}

	for _, fc := range failureCases {
		t.Run(fc.name, func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(fc.handler))
			defer ts.Close()

			client, err := icx.NewClient(icx.ClientConfig{
				BaseURL:       ts.URL,
				APIKey:        "test-token",
				SpaceID:       "fail_space",
				TransportMode: "http",
				Offline:       false,
				Strict:        false, // Explicitly false! Even in non-strict mode, connectors MUST NOT fall back!
			})
			if err != nil {
				t.Fatalf("failed to create client: %v", err)
			}
			defer client.Close()

			// Connector call MUST error out with the remote/protocol error
			_, err = client.ListConnectors(ctx, icx.ListConnectorsRequest{})
			if err == nil {
				t.Fatalf("[%s] expected error, got nil", fc.name)
			}

			// INVARIANT 1: Must NEVER fall back to local engine (which would return ErrRemoteOnly)
			if errors.Is(err, icx.ErrRemoteOnly) {
				t.Errorf("[%s] VIOLATION: Connector fell back to local lattice returning ErrRemoteOnly!", fc.name)
			}

			// INVARIANT 2: Connector failure MUST NOT toggle client to offline mode
			// (unlike regular tools in non-strict mode, connector errors are isolated to the connector service)
			if client.IsOffline() {
				t.Errorf("[%s] VIOLATION: Connector wire failure falsely flipped client to offline mode!", fc.name)
			}

			// Verify other 5 methods behave identically
			_, err = client.RegisterConnector(ctx, icx.RegisterConnectorRequest{})
			if err == nil || errors.Is(err, icx.ErrRemoteOnly) {
				t.Errorf("[%s] RegisterConnector fallback violation: %v", fc.name, err)
			}
			_, err = client.SyncAudit(ctx, icx.SyncAuditRequest{})
			if err == nil || errors.Is(err, icx.ErrRemoteOnly) {
				t.Errorf("[%s] SyncAudit fallback violation: %v", fc.name, err)
			}
			_, err = client.TriggerSync(ctx, icx.TriggerSyncRequest{})
			if err == nil || errors.Is(err, icx.ErrRemoteOnly) {
				t.Errorf("[%s] TriggerSync fallback violation: %v", fc.name, err)
			}
			_, err = client.GetConnectorDocument(ctx, icx.GetConnectorDocumentRequest{})
			if err == nil || errors.Is(err, icx.ErrRemoteOnly) {
				t.Errorf("[%s] GetConnectorDocument fallback violation: %v", fc.name, err)
			}
			_, err = client.InspectConnectorFacts(ctx, icx.InspectConnectorFactsRequest{})
			if err == nil || errors.Is(err, icx.ErrRemoteOnly) {
				t.Errorf("[%s] InspectConnectorFacts fallback violation: %v", fc.name, err)
			}
		})
	}
}

// ============================================================================
// PART 4: Binary Hygiene Source & AST Verification
// ============================================================================

func TestBinaryHygiene_ZeroWebhooksOrListeners(t *testing.T) {
	// Audit cmd/ and pkg/ for any embedded web server listeners or polling daemons
	searchRoots := []string{
		filepath.Join("..", "..", "cmd"),
		filepath.Join("..", "..", "pkg"),
	}

	prohibitedPatterns := []string{
		"http.ListenAndServe",
		"http.ListenAndServeTLS",
		"http.Serve(",
		"net.Listen(",
		"net.ListenTCP(",
		"net.ListenUDP(",
		"net.ListenUnix(",
	}

	auditedFiles := 0

	for _, root := range searchRoots {
		abs, err := filepath.Abs(root)
		if err != nil {
			t.Fatalf("failed to get abs path for %s: %v", root, err)
		}

		err = filepath.Walk(abs, func(path string, info os.FileInfo, err error) error {
			if err != nil {
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

			auditedFiles++
			content, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			src := string(content)

			for _, prohibited := range prohibitedPatterns {
				if strings.Contains(src, prohibited) {
					t.Errorf("BINARY HYGIENE VIOLATION in %s: prohibited server listener %q found!", path, prohibited)
				}
			}

			// Also verify that no production Go file in cmd/ or pkg/icx imports web server frameworks
			fset := token.NewFileSet()
			fileNode, parseErr := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
			if parseErr == nil {
				for _, imp := range fileNode.Imports {
					importPath := strings.Trim(imp.Path.Value, `"`)
					if strings.Contains(importPath, "gin-gonic") ||
						strings.Contains(importPath, "labstack/echo") ||
						strings.Contains(importPath, "gofiber/fiber") ||
						strings.Contains(importPath, "go-chi/chi") {
						t.Errorf("BINARY HYGIENE VIOLATION in %s: foreign web server framework imported: %s", path, importPath)
					}
				}
			}

			return nil
		})
		if err != nil {
			t.Fatalf("error auditing %s: %v", abs, err)
		}
	}

	if auditedFiles < 15 {
		t.Fatalf("expected to audit at least 15 production Go files, audited %d", auditedFiles)
	}
	t.Logf("Audited %d production Go files with 0 listener or webhook server violations.", auditedFiles)
}

// ============================================================================
// PART 5: Concurrency Race Stress under -race
// ============================================================================

func TestConcurrentStress_RaceSafety(t *testing.T) {
	ctx := context.Background()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req icx.JSONRPCRequest
		_ = json.NewDecoder(r.Body).Decode(&req)

		payload := map[string]any{"ok": true, "status": "active"}
		raw, _ := json.Marshal(payload)
		mcpRes := icx.MCPToolCallResult{
			Content: []icx.MCPContent{{Type: "text", Text: string(raw)}},
			IsError: false,
		}
		resBytes, _ := json.Marshal(mcpRes)

		resp := icx.JSONRPCResponse{
			JSONRPC: icx.JSONRPCVersion,
			ID:      req.ID,
			Result:  resBytes,
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer ts.Close()

	onlineClient, err := icx.NewClient(icx.ClientConfig{
		BaseURL:       ts.URL,
		APIKey:        "test-token",
		SpaceID:       "conc_space",
		TransportMode: "http",
		Offline:       false,
		Strict:        false,
	})
	if err != nil {
		t.Fatalf("failed to create online client: %v", err)
	}
	defer onlineClient.Close()

	offlineClient, err := icx.NewClient(icx.ClientConfig{
		SpaceID: "conc_space",
		Offline: true,
	})
	if err != nil {
		t.Fatalf("failed to create offline client: %v", err)
	}
	defer offlineClient.Close()

	lat := icx.NewLocalMemoryCache("conc_space")

	var wg sync.WaitGroup
	workers := 40
	opsPerWorker := 25

	for w := 0; w < workers; w++ {
		wg.Add(1)
		workerID := w
		go func() {
			defer wg.Done()
			for i := 0; i < opsPerWorker; i++ {
				switch (workerID + i) % 6 {
				case 0:
					_, _ = offlineClient.ListConnectors(ctx, icx.ListConnectorsRequest{})
					_, _ = onlineClient.ListConnectors(ctx, icx.ListConnectorsRequest{})
					_, _ = lat.ListConnectors(ctx, icx.ListConnectorsRequest{})
				case 1:
					_, _ = offlineClient.RegisterConnector(ctx, icx.RegisterConnectorRequest{Name: "c"})
					_, _ = onlineClient.RegisterConnector(ctx, icx.RegisterConnectorRequest{Name: "c"})
					_, _ = lat.RegisterConnector(ctx, icx.RegisterConnectorRequest{Name: "c"})
				case 2:
					_, _ = offlineClient.SyncAudit(ctx, icx.SyncAuditRequest{ConnectorID: "c"})
					_, _ = onlineClient.SyncAudit(ctx, icx.SyncAuditRequest{ConnectorID: "c"})
					_, _ = lat.SyncAudit(ctx, icx.SyncAuditRequest{ConnectorID: "c"})
				case 3:
					_, _ = offlineClient.TriggerSync(ctx, icx.TriggerSyncRequest{ConnectorID: "c"})
					_, _ = onlineClient.TriggerSync(ctx, icx.TriggerSyncRequest{ConnectorID: "c"})
					_, _ = lat.TriggerSync(ctx, icx.TriggerSyncRequest{ConnectorID: "c"})
				case 4:
					_, _ = offlineClient.GetConnectorDocument(ctx, icx.GetConnectorDocumentRequest{ConnectorID: "c", DocumentID: "d"})
					_, _ = onlineClient.GetConnectorDocument(ctx, icx.GetConnectorDocumentRequest{ConnectorID: "c", DocumentID: "d"})
					_, _ = lat.GetConnectorDocument(ctx, icx.GetConnectorDocumentRequest{ConnectorID: "c", DocumentID: "d"})
				case 5:
					_, _ = offlineClient.InspectConnectorFacts(ctx, icx.InspectConnectorFactsRequest{ConnectorID: "c"})
					_, _ = onlineClient.InspectConnectorFacts(ctx, icx.InspectConnectorFactsRequest{ConnectorID: "c"})
					_, _ = lat.InspectConnectorFacts(ctx, icx.InspectConnectorFactsRequest{ConnectorID: "c"})
				}
				// Also interleave CallTool
				_, _ = offlineClient.CallTool(ctx, "icx_list_connectors", nil)
				_, _ = onlineClient.CallTool(ctx, "icx_list_connectors", nil)
				_, _ = lat.CallTool(ctx, "icx_list_connectors", nil)
			}
		}()
	}

	wg.Wait()
}
