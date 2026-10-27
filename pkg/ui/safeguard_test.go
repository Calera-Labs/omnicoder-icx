// SPDX-License-Identifier: Apache-2.0
package ui

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/caleralabs/omnicoder-icx/pkg/engine"
	"github.com/caleralabs/omnicoder-icx/pkg/llm"
)

// TestSafeguard_PreflightGoSyntaxRejection verifies that invalid Go syntax is rejected
// by the Go Pre-flight Syntax Guard before touching the filesystem.
func TestSafeguard_PreflightGoSyntaxRejection(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("NO_COLOR", "1")

	// Target file does not exist yet
	targetRel := "projects/myapp/broken.go"
	brokenCode := `package myapp

func BrokenFunction( {
	fmt.Println("missing closing paren and brace"
`

	mockClient := llm.NewMockLLMClient()
	mockClient.CannedContent = `Here is the code:
[ACTION:WRITE path="projects/myapp/broken.go"]
` + "```go\n" + brokenCode + "\n```\n"

	session := NewSession(mockClient, nil, tmpDir)

	in := strings.NewReader("create broken function\n/quit\n")
	var out bytes.Buffer

	ctx := context.Background()
	_ = RunConversationalUI(ctx, session, in, &out)

	outStr := out.String()
	if !strings.Contains(outStr, "[Go Pre-flight Syntax Guard]") {
		t.Errorf("expected [Go Pre-flight Syntax Guard] badge in output, got:\n%s", outStr)
	}

	// Verify file was NEVER written to disk
	fullPath := filepath.Join(tmpDir, filepath.FromSlash(targetRel))
	if _, err := os.Stat(fullPath); !os.IsNotExist(err) {
		t.Fatalf("CRITICAL SAFEGUARD FAILURE: broken Go file was written to disk at %s", fullPath)
	}
}

// TestSafeguard_PreflightPackageConsistencyRejection verifies that writing a file with
// a conflicting package declaration into an existing package directory is rejected.
func TestSafeguard_PreflightPackageConsistencyRejection(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("NO_COLOR", "1")

	// Create existing valid package file
	projDir := filepath.Join(tmpDir, "projects", "service")
	if err := os.MkdirAll(projDir, 0755); err != nil {
		t.Fatal(err)
	}
	existingFile := filepath.Join(projDir, "service.go")
	if err := os.WriteFile(existingFile, []byte("package service\n\nfunc Run() {}\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// Incoming code declares a conflicting package name
	conflictingCode := `package rogue

func Helper() {}
`

	mockClient := llm.NewMockLLMClient()
	mockClient.CannedContent = `Here is helper:
[ACTION:WRITE path="projects/service/helper.go"]
` + "```go\n" + conflictingCode + "\n```\n"

	session := NewSession(mockClient, nil, tmpDir)

	in := strings.NewReader("add helper to service\n/quit\n")
	var out bytes.Buffer

	ctx := context.Background()
	_ = RunConversationalUI(ctx, session, in, &out)

	outStr := out.String()
	if !strings.Contains(outStr, "[Go Pre-flight Syntax Guard]") {
		t.Errorf("expected [Go Pre-flight Syntax Guard] badge in output, got:\n%s", outStr)
	}

	// Verify helper.go was NOT created with conflicting package
	helperPath := filepath.Join(projDir, "helper.go")
	if _, err := os.Stat(helperPath); !os.IsNotExist(err) {
		t.Fatalf("CRITICAL SAFEGUARD FAILURE: conflicting package file was written to disk at %s", helperPath)
	}
}

// TestSafeguard_HostPackageShield_RejectsAppTask verifies that when executing an application task,
// attempts by the LLM to write or patch OmniCoder host platform packages (pkg/*, cmd/*) are shielded.
func TestSafeguard_HostPackageShield_RejectsAppTask(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("NO_COLOR", "1")

	// Pre-create host package file
	hostPkgDir := filepath.Join(tmpDir, "pkg", "engine")
	if err := os.MkdirAll(hostPkgDir, 0755); err != nil {
		t.Fatal(err)
	}
	hostFile := filepath.Join(hostPkgDir, "turn.go")
	originalContent := "package engine\n\n// Core host engine logic\nfunc TurnEngine() int { return 42 }\n"
	if err := os.WriteFile(hostFile, []byte(originalContent), 0644); err != nil {
		t.Fatal(err)
	}

	// LLM tries to overwrite host turn.go during an application task
	clobberCode := `package engine

func TurnEngine() int { return 0 } // Overwritten by app task
`

	mockClient := llm.NewMockLLMClient()
	mockClient.CannedContent = `I am building the inventory dashboard app.
[ACTION:WRITE path="pkg/engine/turn.go"]
` + "```go\n" + clobberCode + "\n```\n"

	session := NewSession(mockClient, nil, tmpDir)

	// Non-host prompt
	in := strings.NewReader("build inventory dashboard application\n/quit\n")
	var out bytes.Buffer

	ctx := context.Background()
	_ = RunConversationalUI(ctx, session, in, &out)

	outStr := out.String()
	if !strings.Contains(outStr, "[Host Package Shield]") {
		t.Errorf("expected [Host Package Shield] in output, got:\n%s", outStr)
	}

	// Verify host file was 100% UNTOUCHED
	afterData, err := os.ReadFile(hostFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(afterData) != originalContent {
		t.Fatalf("CRITICAL SAFEGUARD FAILURE: Host package file was modified during app task!\nExpected:\n%s\nGot:\n%s",
			originalContent, string(afterData))
	}
}

// TestSafeguard_HostPackageShield_AllowsHostTask verifies that when the prompt is an explicit
// host task (e.g. installing safeguards), host package modifications are permitted.
func TestSafeguard_HostPackageShield_AllowsHostTask(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("NO_COLOR", "1")

	hostPkgDir := filepath.Join(tmpDir, "pkg", "devops")
	if err := os.MkdirAll(hostPkgDir, 0755); err != nil {
		t.Fatal(err)
	}
	hostFile := filepath.Join(hostPkgDir, "guard.go")
	originalContent := "package devops\n\nfunc OldGuard() {}\n"
	if err := os.WriteFile(hostFile, []byte(originalContent), 0644); err != nil {
		t.Fatal(err)
	}

	updatedContent := `package devops

func OldGuard() {}
func NewSafeguard() bool { return true }
`

	mockClient := llm.NewMockLLMClient()
	mockClient.CannedContent = `Updating safeguards in host engine.
[ACTION:WRITE path="pkg/devops/guard.go"]
` + "```go\n" + updatedContent + "\n```\n"

	session := NewSession(mockClient, nil, tmpDir)

	// Host task prompt with safeguard keyword
	in := strings.NewReader("safely implement and install host engine safeguards\n/quit\n")
	var out bytes.Buffer

	ctx := context.Background()
	_ = RunConversationalUI(ctx, session, in, &out)

	outStr := out.String()
	if strings.Contains(outStr, "[Host Package Shield]") {
		t.Errorf("did NOT expect [Host Package Shield] to block explicit safeguard task, got:\n%s", outStr)
	}

	// Verify file was updated
	afterData, err := os.ReadFile(hostFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(afterData), "NewSafeguard") {
		t.Errorf("expected host file to be updated for host task, got:\n%s", string(afterData))
	}
}

// TestSafeguard_PreTurnSnapshot_AutomaticRollback verifies that when an autonomous turn
// fails verification and exhausts healing, all modified and created files are rolled back.
func TestSafeguard_PreTurnSnapshot_AutomaticRollback(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("NO_COLOR", "1")

	// Create an existing file with initial content
	projDir := filepath.Join(tmpDir, "projects", "calculator")
	if err := os.MkdirAll(projDir, 0755); err != nil {
		t.Fatal(err)
	}
	calcFile := filepath.Join(projDir, "calc.go")
	initialContent := "package calculator\n\nfunc Add(a, b int) int { return a + b }\n"
	if err := os.WriteFile(calcFile, []byte(initialContent), 0644); err != nil {
		t.Fatal(err)
	}

	// Mock client writes a change and runs a build command that fails repeatedly
	mockClient := llm.NewMockLLMClient()
	mockClient.CannedContent = `I made changes that fail tests:
[ACTION:WRITE path="projects/calculator/calc.go"]
` + "```go\npackage calculator\n\nfunc Add(a, b int) int { return 999 }\n```\n" +
		`[ACTION:WRITE path="projects/calculator/new_unwanted.go"]
` + "```go\npackage calculator\n\nfunc Unwanted() {}\n```\n" +
		`[ACTION:BUILD task="cmd /c exit 1"]`

	session := NewSession(mockClient, nil, tmpDir)

	in := strings.NewReader("modify calculator\n/quit\n")
	var out bytes.Buffer

	ctx := context.Background()
	_ = RunConversationalUI(ctx, session, in, &out)

	outStr := out.String()
	if !strings.Contains(outStr, "[Rollback Guard]") {
		t.Errorf("expected [Rollback Guard] badge in output following failed healing, got:\n%s", outStr)
	}

	// 1. Verify existing file was restored to initialContent
	calcData, err := os.ReadFile(calcFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(calcData) != initialContent {
		t.Errorf("CRITICAL ROLLBACK FAILURE: existing file was not reverted!\nExpected:\n%s\nGot:\n%s",
			initialContent, string(calcData))
	}

	// 2. Verify new file created during the failed turn was deleted
	newFile := filepath.Join(projDir, "new_unwanted.go")
	if _, err := os.Stat(newFile); !os.IsNotExist(err) {
		t.Errorf("CRITICAL ROLLBACK FAILURE: newly created file was not removed during rollback: %s", newFile)
	}
}

// TestSafeguard_PreservesNewScaffoldOnBuildFail verifies that when scaffolding a brand-new project
// (all files in projects/ are newly created), files are preserved on disk even if an initial build command fails.
func TestSafeguard_PreservesNewScaffoldOnBuildFail(t *testing.T) {
	tmpDir := t.TempDir()

	mockClient := llm.NewMockLLMClient()
	mockClient.CannedContent = `I am creating a brand new widget project:
[ACTION:WRITE path="projects/brand_new_app/main.go"]
` + "```go\npackage main\n\nfunc main() {}\n```\n" +
		`[ACTION:BUILD task="cmd /c exit 1"]`

	session := NewSession(mockClient, nil, tmpDir)

	in := strings.NewReader("create brand new widget app\n/quit\n")
	var out bytes.Buffer

	ctx := context.Background()
	_ = RunConversationalUI(ctx, session, in, &out)

	outStr := out.String()
	if !strings.Contains(outStr, "[Scaffold Preserved]") {
		t.Errorf("expected [Scaffold Preserved] badge in output following failed build on new project, got:\n%s", outStr)
	}

	// Verify new scaffold file was PRESERVED on disk
	newFile := filepath.Join(tmpDir, "projects", "brand_new_app", "main.go")
	if _, err := os.Stat(newFile); err != nil {
		t.Errorf("expected newly scaffolded file to be preserved on disk, but got err: %v", err)
	}
}

// TestSafeguard_SessionWriteFile_HostProtection verifies session.WriteFile enforces
// host package protection directly when session is not executing a host task.
func TestSafeguard_SessionWriteFile_HostProtection(t *testing.T) {
	tmpDir := t.TempDir()
	session := NewSession(nil, nil, tmpDir)

	session.SetHostTask(false)
	_, _, err := session.WriteFile("pkg/engine/hacked.go", []byte("package engine\n"))
	if err == nil {
		t.Fatal("expected session.WriteFile to reject write to pkg/engine when not a host task")
	}
	if !strings.Contains(err.Error(), "refusing to modify host engine package") {
		t.Errorf("unexpected error message: %v", err)
	}

	session.SetHostTask(true)
	written, _, err := session.WriteFile("pkg/engine/allowed.go", []byte("package engine\n"))
	if err != nil {
		t.Fatalf("expected session.WriteFile to allow write when isHostTask == true, got: %v", err)
	}
	if !strings.Contains(written, "pkg") {
		t.Errorf("unexpected written path: %s", written)
	}
}

// TestSafeguard_EngineKeywordRecognition verifies that safeguard and self-preservation
// phrases are recognized as host tasks.
func TestSafeguard_EngineKeywordRecognition(t *testing.T) {
	tasks := []struct {
		task     string
		expected bool
	}{
		{"safely implement and install host engine safeguards", true},
		{"implement host shields and safeguards", true},
		{"self-preservation and code preservation check", true},
		{"upgrade omnicoder platform engine", true},
		{"build inventory dashboard web app", false},
		{"create a service metrics monitor", false},
		{"write a personal portfolio website", false},
		{"build an app in projects/bank with safeguards against overdrafts", false},
		{"implement rate limit safeguard in apps/gateway", false},
	}

	for _, tt := range tasks {
		got := engine.IsHostPackageTask(tt.task)
		if got != tt.expected {
			t.Errorf("IsHostPackageTask(%q) = %v; expected %v", tt.task, got, tt.expected)
		}
	}
}

// TestSafeguard_ImplementationDepthGuard verifies that shallow toy stubs are rejected
// by the Implementation Depth Guard, while substantive code and valid config files pass.
func TestSafeguard_ImplementationDepthGuard(t *testing.T) {
	prodPlan := &engine.PlanSpec{
		RawPlanContent: strings.Repeat("# Production Master Plan Specification\nThis is a full scale plan.\n", 100),
	}

	// Case 1: Shallow Go package file (<25 non-empty lines) should be rejected
	shallowGo := `package ingest

type MetricEvent struct {
	Value float64
}

func Fetch() []MetricEvent {
	return nil
}
`
	tooShallow, reason := checkImplementationDepth("projects/monitor/pkg/ingest/metrics.go", shallowGo, prodPlan)
	if !tooShallow {
		t.Errorf("expected shallow Go file to be rejected, got tooShallow=false")
	}
	if !strings.Contains(reason, "minimum 25 lines required") {
		t.Errorf("unexpected rejection reason: %s", reason)
	}

	// Case 2: Substantive Go package file (>=25 non-empty lines) should pass
	var substantiveGo strings.Builder
	substantiveGo.WriteString("package ingest\n\nimport (\n\t\"fmt\"\n\t\"time\"\n)\n\n")
	substantiveGo.WriteString("type MetricSample struct {\n\tHostID string\n\tLatencyMs float64\n\tTimestamp time.Time\n\tThroughput float64\n}\n\n")
	substantiveGo.WriteString("type MetricsClient struct {\n\tEndpoint string\n\tTimeout time.Duration\n}\n\n")
	substantiveGo.WriteString("func NewMetricsClient(ep string) *MetricsClient {\n\treturn &MetricsClient{Endpoint: ep, Timeout: 5 * time.Second}\n}\n\n")
	substantiveGo.WriteString("func (c *MetricsClient) Connect() error {\n\tif c.Endpoint == \"\" {\n\t\treturn fmt.Errorf(\"empty endpoint\")\n\t}\n\treturn nil\n}\n\n")
	substantiveGo.WriteString("func (c *MetricsClient) Fetch() ([]MetricSample, error) {\n\tif err := c.Connect(); err != nil {\n\t\treturn nil, err\n\t}\n\treturn []MetricSample{}, nil\n}\n")

	tooShallow, reason = checkImplementationDepth("projects/monitor/pkg/ingest/metrics.go", substantiveGo.String(), prodPlan)
	if tooShallow {
		t.Errorf("expected substantive Go file to pass depth guard, got rejected: %s", reason)
	}

	// Case 3: Shallow JavaScript file in web/ (<30 non-empty lines) should be rejected
	shallowJS := `class DashboardView {
	constructor() {
		this.container = null;
	}
	init() {
		console.log("init");
	}
}
`
	tooShallow, reason = checkImplementationDepth("projects/monitor/web/static/js/dashboard.js", shallowJS, prodPlan)
	if !tooShallow {
		t.Errorf("expected shallow JS file to be rejected, got tooShallow=false")
	}
	if !strings.Contains(reason, "minimum 30 lines required") {
		t.Errorf("unexpected rejection reason: %s", reason)
	}

	// Case 4: go.mod is exempt even if only 5 lines
	goMod := "module monitor\n\ngo 1.22\n\nrequire github.com/gorilla/websocket v1.5.3\n"
	tooShallow, reason = checkImplementationDepth("projects/monitor/go.mod", goMod, prodPlan)
	if tooShallow {
		t.Errorf("expected go.mod to be exempt from depth guard, got rejected: %s", reason)
	}
}

