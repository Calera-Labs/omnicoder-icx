// SPDX-License-Identifier: Apache-2.0
package ui

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/caleralabs/omnicoder-icx/pkg/engine"
	"github.com/caleralabs/omnicoder-icx/pkg/llm"
)

func TestBuildPlan_NoHostBleedAndNoFalseStall(t *testing.T) {
	ws := t.TempDir()

	// 1. Simulate host repo files in ws/pkg/icx
	hostIcxDir := filepath.Join(ws, "pkg", "icx")
	_ = os.MkdirAll(hostIcxDir, 0755)
	_ = os.WriteFile(filepath.Join(hostIcxDir, "types.go"), []byte("package icx\n// host types\n"), 0644)
	_ = os.WriteFile(filepath.Join(hostIcxDir, "lattice.go"), []byte("package icx\n// host lattice\n"), 0644)

	// 2. Create a plan file in ws/docs/plan.md targeting projects/my-service
	planDir := filepath.Join(ws, "docs")
	_ = os.MkdirAll(planDir, 0755)
	planContent := `
# My Service Build Plan

### Phase 1: Core Substrate
- **Step 1.1:** Create module root at ` + "`projects/my-service/go.mod`" + `
- **Step 1.2:** Implement ` + "`pkg/canonical/canonical.go`" + ` and ` + "`merkle.go`" + `
- **Step 1.3:** Implement ` + "`pkg/icx/types.go`" + `
- **Step 1.4:** Implement ` + "`pkg/icx/lattice.go`" + `
- Verification Gate: ` + "`go test ./...`" + `

### Phase 2: Services
- **Step 2.1:** Implement ` + "`pkg/service/service.go`" + `
`
	planFile := filepath.Join(planDir, "plan.md")
	if err := os.WriteFile(planFile, []byte(planContent), 0644); err != nil {
		t.Fatal(err)
	}

	turn := 0
	// 3. Mock LLM server providing sequential phase files
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		turn++
		var replyText string
		switch turn {
		case 1:
			replyText = `I will start by creating projects/my-service/go.mod.
[ACTION:WRITE path="projects/my-service/go.mod"]
` + "```go\nmodule myservice\n\ngo 1.22\n```"
		case 2:
			replyText = `Now I will implement pkg/canonical/canonical.go.
[ACTION:WRITE path="projects/my-service/pkg/canonical/canonical.go"]
` + "```go\npackage canonical\n\nfunc ComputeMerkleRoot(leaves [][]byte) []byte {\n\treturn []byte(\"merkle_root\")\n}\n```"
		case 3:
			replyText = `Now I will implement pkg/icx/types.go.
[ACTION:WRITE path="projects/my-service/pkg/icx/types.go"]
` + "```go\npackage icx\n\ntype Item struct {\n\tID string\n}\n```"
		case 4:
			replyText = `Now I will implement pkg/icx/lattice.go.
[ACTION:WRITE path="projects/my-service/pkg/icx/lattice.go"]
` + "```go\npackage icx\n\nfunc ComputeHash(x, y, z float64) int64 {\n\treturn 42\n}\n```"
		case 5:
			replyText = `Now I will implement Phase 2: pkg/service/service.go.
[ACTION:WRITE path="projects/my-service/pkg/service/service.go"]
` + "```go\npackage service\n\ntype Service struct{}\n```"
		default:
			replyText = "All done.\nDONE\n"
		}

		resp := llm.ChatCompletionResponse{
			ID:      fmt.Sprintf("chatcmpl-%d", turn),
			Object:  "chat.completion",
			Created: time.Now().Unix(),
			Choices: []llm.Choice{
				{
					Index: 0,
					Message: llm.ChatMessage{
						Role:    "assistant",
						Content: replyText,
					},
					FinishReason: "stop",
				},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer mockServer.Close()

	client, err := llm.NewClient(llm.ClientConfig{
		BaseURL: mockServer.URL,
		APIKey:  "test-key",
		Model:   "gemini-3.5-flash-lite",
	})
	if err != nil {
		t.Fatalf("failed to create llm client: %v", err)
	}

	session := NewSession(client, nil, ws)
	defer session.Close()

	var out bytes.Buffer
	prompt := fmt.Sprintf("build %q", planFile)

	err = executeConversationalTurn(context.Background(), session, prompt, &out)
	if err != nil {
		t.Fatalf("unexpected error from executeConversationalTurn: %v", err)
	}

	outStr := out.String()
	if strings.Contains(outStr, "[Stall]") {
		t.Fatalf("executeConversationalTurn triggered [Stall] unexpectedly! Output:\n%s", outStr)
	}

	// Verify all project files exist under projects/my-service
	expectedFiles := []string{
		"projects/my-service/go.mod",
		"projects/my-service/pkg/canonical/canonical.go",
		"projects/my-service/pkg/icx/types.go",
		"projects/my-service/pkg/icx/lattice.go",
		"projects/my-service/pkg/service/service.go",
	}

	for _, ef := range expectedFiles {
		fullPath := filepath.Join(ws, filepath.FromSlash(ef))
		if _, statErr := os.Stat(fullPath); statErr != nil {
			t.Errorf("expected file %s to exist on disk: %v", ef, statErr)
		}
	}
}

func TestBuildPlan_IgnoresDomainNamesAndBinaryArtifacts(t *testing.T) {
	ws := t.TempDir()

	planDir := filepath.Join(ws, "docs")
	_ = os.MkdirAll(planDir, 0755)
	planContent := `
# Telemetry Service Test Plan

### Phase 1: Ingestion Connectors
- **Step 1.1:** Implement ` + "`projects/telemetry/go.mod`" + `
- **Step 1.2:** Implement ` + "`pkg/ingest/http.go`" + ` connecting to ` + "`api.service.io`" + ` / ` + "`metrics.example.com`" + ` with backoff
- **Step 1.3:** Implement ` + "`pkg/ingest/ws.go`" + ` connecting to ` + "`stream.example.io`" + ` WebSocket
- **Step 1.4:** Compile standalone binary: ` + "`go build -o telemetry.exe main.go`" + `
- Verification Gate: ` + "`go test ./...`" + `
`
	planFile := filepath.Join(planDir, "plan.md")
	if err := os.WriteFile(planFile, []byte(planContent), 0644); err != nil {
		t.Fatal(err)
	}

	turn := 0
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		turn++
		var replyText string
		switch turn {
		case 1:
			replyText = `Writing all phase 1 files.
[ACTION:WRITE path="projects/telemetry/go.mod"]
` + "```go\nmodule telemetry\n\ngo 1.22\n```\n" + `
[ACTION:WRITE path="projects/telemetry/pkg/ingest/http.go"]
` + "```go\npackage ingest\n\ntype HTTPCollector struct{}\n```\n" + `
[ACTION:WRITE path="projects/telemetry/pkg/ingest/ws.go"]
` + "```go\npackage ingest\n\ntype WSCollector struct{}\n```\n" + `
[ACTION:WRITE path="projects/telemetry/main.go"]
` + "```go\npackage main\n\nfunc main() {}\n```\n"
		default:
			replyText = "All done.\nDONE\n"
		}

		resp := llm.ChatCompletionResponse{
			ID:      fmt.Sprintf("chatcmpl-%d", turn),
			Object:  "chat.completion",
			Created: time.Now().Unix(),
			Choices: []llm.Choice{
				{
					Index: 0,
					Message: llm.ChatMessage{
						Role:    "assistant",
						Content: replyText,
					},
					FinishReason: "stop",
				},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer mockServer.Close()

	client, err := llm.NewClient(llm.ClientConfig{
		BaseURL: mockServer.URL,
		APIKey:  "test-key",
		Model:   "gemini-3.5-flash-lite",
	})
	if err != nil {
		t.Fatalf("failed to create llm client: %v", err)
	}

	session := NewSession(client, nil, ws)
	defer session.Close()
	var out bytes.Buffer
	prompt := fmt.Sprintf("build %q", planFile)

	err = executeConversationalTurn(context.Background(), session, prompt, &out)
	if err != nil {
		t.Fatalf("unexpected error from executeConversationalTurn: %v", err)
	}

	outStr := out.String()
	if strings.Contains(outStr, "[Stall]") {
		t.Fatalf("executeConversationalTurn stalled on web domains! Output:\n%s", outStr)
	}
	if strings.Contains(outStr, "api.service.io") || strings.Contains(outStr, "metrics.example.com") || strings.Contains(outStr, "stream.example.io") {
		t.Fatalf("executeConversationalTurn treated domain names as missing files! Output:\n%s", outStr)
	}
}

// TestBuildPlan_CrossProjectIsolation_NoForeignPlanInterference verifies that an existing active plan
// from a prior conversation (e.g. projects/service-a) NEVER hijacks a turn when the user works on a
// different project (e.g. projects/service-b), does NOT trigger Phase Incomplete Guards,
// and does NOT nest or scaffold foreign project files.
func TestBuildPlan_CrossProjectIsolation_NoForeignPlanInterference(t *testing.T) {
	ws := t.TempDir()

	// 1. Create a foreign plan for service-a in docs/service_a.md
	planDir := filepath.Join(ws, "docs")
	_ = os.MkdirAll(planDir, 0755)
	foreignPlanContent := `
# Service A Implementation Plan

### Phase 1: Substrate Scaffolding
- **Step 1.1:** Create module root at ` + "`projects/service-a/go.mod`" + `
- **Step 1.2:** Implement ` + "`pkg/canonical/canonical.go`" + `
- Verification Gate: ` + "`go test ./...`" + `
`
	foreignPlanFile := filepath.Join(planDir, "service_a.md")
	_ = os.WriteFile(foreignPlanFile, []byte(foreignPlanContent), 0644)

	// 2. Mock LLM server answering request for service-b
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		replyText := `Here is the service-b module.
[ACTION:WRITE path="projects/service-b/worker.py"]
` + "```python\n# Worker engine\ndef get_tasks():\n    return []\n```\n" + `
[ACTION:BUILD task="python -c \"print('verified')\""]
DONE
`
		resp := llm.ChatCompletionResponse{
			Choices: []llm.Choice{
				{
					Index: 0,
					Message: llm.ChatMessage{
						Role:    "assistant",
						Content: replyText,
					},
					FinishReason: "stop",
				},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer mockServer.Close()

	client, err := llm.NewClient(llm.ClientConfig{
		BaseURL: mockServer.URL,
		APIKey:  "test-key",
		Model:   "gemini-3.5-flash-lite",
	})
	if err != nil {
		t.Fatalf("failed to create llm client: %v", err)
	}

	session := NewSession(client, nil, ws)
	defer session.Close()

	// 3. Pre-seed the session with the foreign service-a plan as if from a prior conversation
	foreignSpec := engine.ParsePlanMarkdown(foreignPlanContent)
	if foreignSpec == nil {
		t.Fatal("failed to parse foreign plan")
	}
	session.SetActivePlan(foreignSpec, foreignPlanFile)

	var out bytes.Buffer
	prompt := "make me a worker module in projects/service-b"

	err = executeConversationalTurn(context.Background(), session, prompt, &out)
	if err != nil {
		t.Fatalf("unexpected error from executeConversationalTurn: %v", err)
	}

	outStr := out.String()

	// 4. Assertions:
	// A) Phase Incomplete Guard MUST NOT trigger for service-a
	if strings.Contains(outStr, "[Phase Incomplete Guard]") {
		t.Fatalf("Phase Incomplete Guard erroneously triggered for foreign plan! Output:\n%s", outStr)
	}

	// B) Autonomous Scaffolding MUST NOT create go.mod for service-a
	if strings.Contains(outStr, "[Autonomous Scaffolding]") {
		t.Fatalf("Autonomous Scaffolding erroneously triggered for foreign plan! Output:\n%s", outStr)
	}

	// C) No foreign files created under projects/service-b/projects/service-a
	nestedCorruptDir := filepath.Join(ws, "projects", "service-b", "projects")
	if _, err := os.Stat(nestedCorruptDir); err == nil {
		t.Fatalf("Corrupt nested projects directory was created: %s", nestedCorruptDir)
	}

	// D) The target file projects/service-b/worker.py was successfully written
	targetFile := filepath.Join(ws, "projects", "service-b", "worker.py")
	if _, err := os.Stat(targetFile); err != nil {
		t.Fatalf("Expected target file %s was not written! Err: %v", targetFile, err)
	}
}


