// SPDX-License-Identifier: Apache-2.0
package ui

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/caleralabs/omnicoder-icx/pkg/devops"
	"github.com/caleralabs/omnicoder-icx/pkg/engine"
	"github.com/caleralabs/omnicoder-icx/pkg/llm"
)

func TestActionIntent_PatchParsing(t *testing.T) {
	msg := `Here is the surgical patch:
[ACTION:PATCH path="pkg/ingest/handler.go"]
<<<<<<< SEARCH
func oldFunc() int {
    return 1
}
=======
func oldFunc() int {
    return 42
}
>>>>>>> REPLACE
`
	intents := ParseAllActionIntents(msg)
	if len(intents) != 1 {
		t.Fatalf("expected 1 intent, got %d", len(intents))
	}
	intent := intents[0]
	if intent.Type != ActionPatch {
		t.Errorf("expected ActionPatch, got %s", intent.Type)
	}
	if !intent.IsPatch() {
		t.Errorf("expected IsPatch() == true")
	}
	if !intent.IsFileModification() {
		t.Errorf("expected IsFileModification() == true")
	}
	if intent.Path() != "pkg/ingest/handler.go" {
		t.Errorf("expected path 'pkg/ingest/handler.go', got %q", intent.Path())
	}
}

func TestExecutePatch_AiderBlocks(t *testing.T) {
	tmpDir := t.TempDir()
	filePath := filepath.Join(tmpDir, "test.go")
	initialContent := `package main

import "fmt"

func Hello() {
    fmt.Println("hello world")
}

func Goodbye() {
    fmt.Println("goodbye world")
}
`
	if err := os.WriteFile(filePath, []byte(initialContent), 0644); err != nil {
		t.Fatal(err)
	}

	session := NewSession(nil, nil, tmpDir)

	patchContent := `<<<<<<< SEARCH
func Hello() {
    fmt.Println("hello world")
}
=======
func Hello() {
    fmt.Println("hello universe")
}
>>>>>>> REPLACE`

	var out bytes.Buffer
	err := executePatch(session, "test.go", patchContent, &out)
	if err != nil {
		t.Fatalf("executePatch failed: %v", err)
	}

	updated, err := os.ReadFile(filePath)
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(string(updated), "hello universe") {
		t.Errorf("expected 'hello universe' in patched file, got:\n%s", string(updated))
	}
	if !strings.Contains(string(updated), "func Goodbye()") {
		t.Errorf("expected Goodbye() to remain intact, got:\n%s", string(updated))
	}
}

func TestTryMergeGoFunction_SingleMethod(t *testing.T) {
	existingCode := `package mathutils

import "math"

type Calculator struct {
    multiplier float64
}

func (c *Calculator) Add(a, b float64) float64 {
    return a + b
}

func (c *Calculator) Multiply(a, b float64) float64 {
    return a * b * c.multiplier
}

func (c *Calculator) Sqrt(x float64) float64 {
    return math.Sqrt(x)
}
`

	incomingCode := `package mathutils

func (c *Calculator) Multiply(a, b float64) float64 {
    // Fixed zero multiplier edge case
    if c.multiplier == 0 {
        return 0
    }
    return a * b * c.multiplier
}
`

	merged, ok := tryMergeGoFunction(existingCode, incomingCode)
	if !ok {
		t.Fatalf("expected tryMergeGoFunction to succeed")
	}

	if !strings.Contains(merged, "Fixed zero multiplier edge case") {
		t.Errorf("expected merged code to contain new comment/logic")
	}
	if !strings.Contains(merged, "func (c *Calculator) Add") {
		t.Errorf("expected existing Add method to be preserved")
	}
	if !strings.Contains(merged, "func (c *Calculator) Sqrt") {
		t.Errorf("expected existing Sqrt method to be preserved")
	}
	if !strings.Contains(merged, "type Calculator struct") {
		t.Errorf("expected existing Calculator struct to be preserved")
	}
}

func TestTryMergeGoFunction_WithoutPackageHeader(t *testing.T) {
	existingCode := `package engine

func StepOne() int {
    return 1
}

func StepTwo() int {
    return 2
}
`

	// Incoming code is just the function without package declaration
	incomingCode := `func StepTwo() int {
    return 200
}
`

	merged, ok := tryMergeGoFunction(existingCode, incomingCode)
	if !ok {
		t.Fatalf("expected tryMergeGoFunction to succeed even without package header")
	}

	if !strings.Contains(merged, "return 200") {
		t.Errorf("expected merged code to contain updated return 200")
	}
	if !strings.Contains(merged, "func StepOne()") {
		t.Errorf("expected StepOne to be preserved")
	}
}

func TestCountAndDetectMissingGoFunctions(t *testing.T) {
	fullCode := `package queue

type Worker struct {}

func (w *Worker) Process() {}
func (w *Worker) Dispatch() {}
func LoadConfig() {}
func Validate() {}
`

	count := countGoFunctions(fullCode)
	if count != 4 {
		t.Errorf("expected 4 functions, got %d", count)
	}

	stubCode := `package queue

type Worker struct {}

func (w *Worker) Process() {}
`

	missing := detectMissingGoFunctions(fullCode, stubCode)
	if len(missing) != 3 {
		t.Fatalf("expected 3 missing functions, got %d: %v", len(missing), missing)
	}

	expectedMissing := map[string]bool{
		"(*Worker).Dispatch": true,
		"LoadConfig":         true,
		"Validate":           true,
	}
	for _, m := range missing {
		if !expectedMissing[m] {
			t.Errorf("unexpected missing function reported: %s", m)
		}
	}
}

func TestBuildSelfHealingPrompt(t *testing.T) {
	tmpDir := t.TempDir()
	srcFile := filepath.Join(tmpDir, "sample.go")
	srcContent := `package main

import "fmt"

func Calculate() int {
    var x = "bad"
    return x
}
`
	if err := os.WriteFile(srcFile, []byte(srcContent), 0644); err != nil {
		t.Fatal(err)
	}

	rawDiag := "sample.go:6:5: cannot use x (variable of type string) as int value in return statement"
	prompt := buildSelfHealingPrompt(tmpDir, "", 1, rawDiag)

	if !strings.Contains(prompt, "Verification test or build failed (healing attempt 1/3)") {
		t.Errorf("expected header in prompt, got:\n%s", prompt)
	}
	if !strings.Contains(prompt, "=== SOURCE CONTEXT FOR ERRORS ===") {
		t.Errorf("expected source context header in prompt, got:\n%s", prompt)
	}
	if !strings.Contains(prompt, "return x") {
		t.Errorf("expected offending line in snippet, got:\n%s", prompt)
	}
	if !strings.Contains(prompt, "PREFER SURGICAL PATCHES") {
		t.Errorf("expected surgical patch instructions in prompt, got:\n%s", prompt)
	}
}

func TestPlanSpec_PhaseDetectionAndVerification(t *testing.T) {
	scope := "projects/telemetry-service"
	plan := &engine.PlanSpec{
		ProjectScope: scope,
		ModuleName:   "telemetryservice",
		Phases: []engine.PlanPhase{
			{
				PhaseNumber: 1,
				Name:        "Core Substrate",
				TargetFiles: []string{
					scope + "/go.mod",
					scope + "/pkg/canonical/canonical.go",
					scope + "/pkg/icx/types.go",
				},
			},
			{
				PhaseNumber: 2,
				Name:        "Ingestion Collectors",
				TargetFiles: []string{
					scope + "/pkg/ingest/http.go",
					scope + "/pkg/ingest/grpc.go",
				},
			},
			{
				PhaseNumber: 5,
				Name:        "Web Client",
				TargetFiles: []string{
					scope + "/web/static/js/dashboard.js",
				},
			},
		},
	}

	// 1. Test FindPhaseForFile
	ph1 := plan.FindPhaseForFile("projects/telemetry-service/pkg/canonical/canonical.go")
	if ph1 == nil || ph1.PhaseNumber != 1 {
		t.Errorf("expected Phase 1, got %v", ph1)
	}

	ph2 := plan.FindPhaseForFile("pkg/ingest/http.go")
	if ph2 == nil || ph2.PhaseNumber != 2 {
		t.Errorf("expected Phase 2, got %v", ph2)
	}

	ph5 := plan.FindPhaseForFile("projects/telemetry-service/web/static/js/dashboard.js")
	if ph5 == nil || ph5.PhaseNumber != 5 {
		t.Errorf("expected Phase 5, got %v", ph5)
	}

	// 2. Test IsPhaseFilesPresent in empty directory
	tmpDir := t.TempDir()
	complete, missing := plan.IsPhaseFilesPresent(tmpDir, ph1)
	if complete {
		t.Errorf("expected incomplete for empty dir")
	}
	if len(missing) != len(ph1.TargetFiles) {
		t.Errorf("expected all %d files missing, got %d", len(ph1.TargetFiles), len(missing))
	}

	// 3. Create all Phase 1 files and verify IsPhaseFilesPresent
	for _, f := range ph1.TargetFiles {
		absPath := filepath.Join(tmpDir, filepath.FromSlash(f))
		_ = os.MkdirAll(filepath.Dir(absPath), 0755)
		_ = os.WriteFile(absPath, []byte("package test\n"), 0644)
	}
	complete, missing = plan.IsPhaseFilesPresent(tmpDir, ph1)
	if !complete || len(missing) != 0 {
		t.Errorf("expected complete == true, got complete=%v, missing=%v", complete, missing)
	}
}

func TestTryMergeGoFunction_WithNewHelperAndTypes(t *testing.T) {
	existingCode := `package worker

type Worker struct {
	id int
}

func (w *Worker) Work() int {
	return 1
}

func (w *Worker) Idle() bool {
	return false
}
`

	incomingCode := `package worker

type Helper struct {
	name string
}

func (w *Worker) Work() int {
	return 100
}

func NewHelper() *Helper {
	return &Helper{name: "sub"}
}
`

	merged, ok := tryMergeGoFunction(existingCode, incomingCode)
	if !ok {
		t.Fatalf("expected tryMergeGoFunction to succeed with new helper and type")
	}

	if !strings.Contains(merged, "return 100") {
		t.Errorf("expected Work() to be updated to return 100, got:\n%s", merged)
	}
	if !strings.Contains(merged, "func (w *Worker) Idle() bool") {
		t.Errorf("expected Idle() to be preserved, got:\n%s", merged)
	}
	if !strings.Contains(merged, "type Worker struct") {
		t.Errorf("expected Worker struct to be preserved, got:\n%s", merged)
	}
	if !strings.Contains(merged, "type Helper struct") {
		t.Errorf("expected Helper struct to be appended, got:\n%s", merged)
	}
	if !strings.Contains(merged, "func NewHelper()") {
		t.Errorf("expected NewHelper function to be appended, got:\n%s", merged)
	}
}

func TestPreheal_StandardGoCompilerDiagnostics(t *testing.T) {
	dir := t.TempDir()

	// 1. Test real compiler error format "<ident> declared and not used"
	src1 := "package main\n\nfunc Run() {\n\tvalRaw := 1.0\n\tprintln(\"done\")\n}\n"
	if err := os.WriteFile(filepath.Join(dir, "worker.go"), []byte(src1), 0644); err != nil {
		t.Fatal(err)
	}
	diag1 := "worker.go:4:2: valRaw declared and not used\n"
	why1, ok1 := prehealSandbox(dir, diag1)
	if !ok1 {
		t.Errorf("expected prehealSandbox to heal '<ident> declared and not used'")
	}
	if !strings.Contains(why1, "valRaw") {
		t.Errorf("expected explanation to mention valRaw, got: %s", why1)
	}
	body1 := string(mustRead(t, filepath.Join(dir, "worker.go")))
	if strings.Contains(body1, "valRaw :=") {
		t.Errorf("expected valRaw declaration to be removed or commented, got:\n%s", body1)
	}

	// 2. Test duplicate declaration within single file
	src2 := "package main\n\nfunc Calc() int { return 1 }\n\nfunc Calc() int { return 2 }\n"
	if err := os.WriteFile(filepath.Join(dir, "calc.go"), []byte(src2), 0644); err != nil {
		t.Fatal(err)
	}
	diag2 := "calc.go:5:6: Calc redeclared in this block\n\tcalc.go:3:6: other declaration of Calc\n"
	why2, ok2 := prehealSandbox(dir, diag2)
	if !ok2 {
		t.Errorf("expected prehealSandbox to heal duplicate declaration in single file")
	}
	if !strings.Contains(why2, "redeclared") {
		t.Errorf("expected explanation to mention redeclared, got: %s", why2)
	}
	body2 := string(mustRead(t, filepath.Join(dir, "calc.go")))
	countCalc := strings.Count(body2, "func Calc()")
	if countCalc != 1 {
		t.Errorf("expected exactly 1 func Calc(), got %d in:\n%s", countCalc, body2)
	}

	// 3. Test multiple adjacent unused variables (e.g. valA and valB)
	src3 := "package main\n\nimport \"math\"\n\nfunc Calc(u float64) {\n\tvalA := math.Sin(u)\n\tvalB := math.Cos(u)\n\tprintln(\"done\")\n}\n"
	if err := os.WriteFile(filepath.Join(dir, "trig.go"), []byte(src3), 0644); err != nil {
		t.Fatal(err)
	}
	diag3 := "trig.go:6:2: valA declared and not used\ntrig.go:7:2: valB declared and not used\n"
	why3, ok3 := prehealSandbox(dir, diag3)
	if !ok3 {
		t.Errorf("expected prehealSandbox to heal adjacent unused variables, got explanation: %s", why3)
	}
	body3 := string(mustRead(t, filepath.Join(dir, "trig.go")))
	if err := devops.ValidateGoSource(body3); err != nil {
		t.Fatalf("prehealed code has syntax errors: %v\nCode:\n%s", err, body3)
	}
	if strings.Contains(body3, "valA :=") || strings.Contains(body3, "valB :=") {
		t.Errorf("expected valA and valB to be healed with blank assignments, got:\n%s", body3)
	}
}

func TestSendMessageAll_FatPayloadPreservesLastAssistant(t *testing.T) {
	mockClient := llm.NewMockLLMClient()
	// Canned response with code block large enough to trigger Fat-Payload Guard (>2048 bytes)
	mockClient.CannedContent = "```go\n" + strings.Repeat("// A very substantive implementation block\n", 60) + "```\n"

	session := NewSession(mockClient, nil, t.TempDir())
	ctx := context.Background()

	// Turn 1
	_, _, err := session.SendMessageAll(ctx, "turn 1 request")
	if err != nil {
		t.Fatal(err)
	}

	// Turn 2
	_, _, err = session.SendMessageAll(ctx, "turn 2 request")
	if err != nil {
		t.Fatal(err)
	}

	// Turn 3: check what was passed to mockClient on the 3rd request
	_, _, err = session.SendMessageAll(ctx, "turn 3 request")
	if err != nil {
		t.Fatal(err)
	}

	reqs := mockClient.ReceivedRequests
	if len(reqs) < 3 {
		t.Fatalf("expected at least 3 requests received, got %d", len(reqs))
	}

	lastReq := reqs[len(reqs)-1]
	// Inspect messages in lastReq:
	// Turn 1 assistant message should be compacted ("[Code preserved:")
	// Turn 2 assistant message (immediately preceding Turn 3) should NOT be compacted!
	var assistantMsgs []string
	for _, msg := range lastReq.Messages {
		if msg.Role == "assistant" {
			assistantMsgs = append(assistantMsgs, msg.Content)
		}
	}

	if len(assistantMsgs) < 2 {
		t.Fatalf("expected 2 assistant messages in history, got %d", len(assistantMsgs))
	}

	firstAssistant := assistantMsgs[0]
	secondAssistant := assistantMsgs[1]

	if !strings.Contains(firstAssistant, "[Code preserved:") {
		t.Errorf("expected first assistant message to be compacted by Fat-Payload Guard, got:\n%s", firstAssistant[:min(len(firstAssistant), 200)])
	}
	if strings.Contains(secondAssistant, "[Code preserved:") {
		t.Errorf("expected latest assistant message to be PRESERVED in full, but it was compacted!")
	}
	if !strings.Contains(secondAssistant, "// A very substantive implementation block") {
		t.Errorf("expected latest assistant message to contain full code body")
	}
}

func TestSendMessageAll_FatPayloadCompactsHistoricalUserDiagnostics(t *testing.T) {
	mockClient := llm.NewMockLLMClient()
	mockClient.CannedContent = "I understand the diagnostic failure and will apply a surgical fix."

	session := NewSession(mockClient, nil, t.TempDir())
	ctx := context.Background()

	// Turn 1 user message: large diagnostic payload (>1024 bytes)
	turn1Diag := "Verification test execution failed with exit code 1:\n=== COMPILER OUTPUT ===\n" +
		strings.Repeat("pkg/service/handler.go:88: undefined: OldServiceMethod\n", 35)

	// Turn 1
	_, _, err := session.SendMessageAll(ctx, turn1Diag)
	if err != nil {
		t.Fatal(err)
	}

	// Turn 2
	turn2Prompt := "Please patch pkg/service/handler.go"
	_, _, err = session.SendMessageAll(ctx, turn2Prompt)
	if err != nil {
		t.Fatal(err)
	}

	// Turn 3: check what was passed to mockClient on the 3rd request
	turn3Prompt := "Check build status"
	_, _, err = session.SendMessageAll(ctx, turn3Prompt)
	if err != nil {
		t.Fatal(err)
	}

	reqs := mockClient.ReceivedRequests
	if len(reqs) < 3 {
		t.Fatalf("expected at least 3 requests received, got %d", len(reqs))
	}

	lastReq := reqs[len(reqs)-1]
	// In the last request:
	// Turn 1 user message should be compacted (contains "historical diagnostic output truncated")
	var userMsgs []string
	for _, msg := range lastReq.Messages {
		if msg.Role == "user" {
			userMsgs = append(userMsgs, msg.Content)
		}
	}

	if len(userMsgs) < 3 {
		t.Fatalf("expected 3 user messages in history, got %d", len(userMsgs))
	}

	turn1Msg := userMsgs[0]
	if !strings.Contains(turn1Msg, "historical diagnostic output truncated") {
		t.Errorf("expected Turn 1 user diagnostic to be compacted, got:\n%s", turn1Msg)
	}
	if !strings.Contains(turn1Msg, "SHA-256:") {
		t.Errorf("compacted Turn 1 user diagnostic missing SHA-256 seal: %s", turn1Msg)
	}
}

func TestCodePreservationGuard_SplicesPartialWriteInsteadOfRejecting(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("NO_COLOR", "1")

	// Target project file with multiple functions
	appDir := filepath.Join(tmpDir, "projects", "sampleapp")
	if err := os.MkdirAll(appDir, 0755); err != nil {
		t.Fatal(err)
	}
	goMod := "module sampleapp\n\ngo 1.22\n"
	if err := os.WriteFile(filepath.Join(appDir, "go.mod"), []byte(goMod), 0644); err != nil {
		t.Fatal(err)
	}

	origCode := `package sampleapp

// Alpha computes the primary alpha index for the subsystem.
func Alpha() int {
	return 1
}

// Beta computes the secondary beta coefficient.
func Beta() int {
	return 2
}

// Gamma computes the tertiary gamma ratio.
func Gamma() int {
	return 3
}

// Delta computes the quaternary delta factor.
func Delta() int {
	return 4
}

// Epsilon computes the fifth order tensor parameter.
func Epsilon() int {
	return 5
}
`
	filePath := filepath.Join(appDir, "sample.go")
	if err := os.WriteFile(filePath, []byte(origCode), 0644); err != nil {
		t.Fatal(err)
	}

	// LLM attempts a partial write updating Beta and adding Omega, but omitting Alpha, Gamma, Delta.
	// File shrinkage drops size by >40% and functions are missing.
	// Guard C should invoke tryMergeGoFunction fallback to splice Beta and Omega rather than rejecting.
	partialCode := `package sampleapp

func Beta() int {
	return 200
}

func Omega() int {
	return 500
}
`

	mockClient := llm.NewMockLLMClient()
	mockClient.CannedContent = `Here is the fix:
[ACTION:WRITE path="projects/sampleapp/sample.go"]
` + "```go\n" + partialCode + "\n```\n" + `[ACTION:BUILD task="go test ./..."]`

	session := NewSession(mockClient, nil, tmpDir)
	in := strings.NewReader("fix Beta\n/quit\n")
	var out bytes.Buffer

	ctx := context.Background()
	_ = RunConversationalUI(ctx, session, in, &out)

	outStr := out.String()
	if !strings.Contains(outStr, "[Go Function Splicer]") {
		t.Errorf("expected [Go Function Splicer] badge in output, got:\n%s", outStr)
	}
	if strings.Contains(outStr, "rejected incoming truncated write") {
		t.Errorf("expected write NOT to be rejected by Code Preservation Guard, but it was rejected:\n%s", outStr)
	}

	// Verify file content was non-destructively spliced
	updatedData, err := os.ReadFile(filePath)
	if err != nil {
		t.Fatal(err)
	}
	updatedStr := string(updatedData)

	if !strings.Contains(updatedStr, "return 200") {
		t.Errorf("expected Beta() to be updated with return 200, got:\n%s", updatedStr)
	}
	if !strings.Contains(updatedStr, "func Alpha()") {
		t.Errorf("expected Alpha() to be preserved, got:\n%s", updatedStr)
	}
	if !strings.Contains(updatedStr, "func Gamma()") {
		t.Errorf("expected Gamma() to be preserved, got:\n%s", updatedStr)
	}
	if !strings.Contains(updatedStr, "func Omega()") {
		t.Errorf("expected Omega() to be appended, got:\n%s", updatedStr)
	}
}

func TestTryMergeGoFunction_WithConstAndVar(t *testing.T) {
	existingCode := `package cfg

const MaxSpeed = 50
var GlobalCount = 10

func Run() int {
	return MaxSpeed
}
`
	// Incoming code is a partial write updating MaxSpeed/GlobalCount and adding MinSpeed/ExtraDebug,
	// but omitting func Run(). tryMergeGoFunction must splice values and preserve Run().
	incomingCode := `package cfg

const MaxSpeed = 100
const MinSpeed = 5
var GlobalCount = 20
var ExtraDebug = true
`
	merged, ok := tryMergeGoFunction(existingCode, incomingCode)
	if !ok {
		t.Fatalf("expected tryMergeGoFunction to succeed with const and var updates")
	}

	if !strings.Contains(merged, "MaxSpeed = 100") {
		t.Errorf("expected MaxSpeed = 100 in merged code, got:\n%s", merged)
	}
	if !strings.Contains(merged, "MinSpeed = 5") {
		t.Errorf("expected MinSpeed = 5 appended to merged code, got:\n%s", merged)
	}
	if !strings.Contains(merged, "GlobalCount = 20") {
		t.Errorf("expected GlobalCount = 20 in merged code, got:\n%s", merged)
	}
	if !strings.Contains(merged, "ExtraDebug = true") {
		t.Errorf("expected ExtraDebug = true appended to merged code, got:\n%s", merged)
	}
	if !strings.Contains(merged, "func Run()") {
		t.Errorf("expected existing Run() to be preserved, got:\n%s", merged)
	}
}

func TestTryMergeGoFunction_TypesOnlyFile(t *testing.T) {
	existingCode := `package models

type Player struct {
	ID   int
	Name string
}

type Scoreboard struct {
	Scores map[int]int
}
`
	// Incoming code updates Player to add Level, but omits Scoreboard.
	// Both existing and incoming have 0 functions.
	incomingCode := `package models

type Player struct {
	ID    int
	Name  string
	Level int
}
`
	merged, ok := tryMergeGoFunction(existingCode, incomingCode)
	if !ok {
		t.Fatalf("expected tryMergeGoFunction to succeed on types-only file partial update")
	}

	if !strings.Contains(merged, "Level int") {
		t.Errorf("expected Player to have Level int, got:\n%s", merged)
	}
	if !strings.Contains(merged, "type Scoreboard struct") {
		t.Errorf("expected Scoreboard to be preserved in merged code, got:\n%s", merged)
	}
}

func TestTryMergeGoFunction_ImportsWithoutPackageHeader(t *testing.T) {
	existingCode := `package mathutil

import (
	"fmt"
)

func Print(s string) {
	fmt.Println(s)
}
`
	incomingCode := `import "math"

func Sqrt(x float64) float64 {
	return math.Sqrt(x)
}
`
	merged, ok := tryMergeGoFunction(existingCode, incomingCode)
	if !ok {
		t.Fatalf("expected tryMergeGoFunction to succeed for incoming without package header")
	}

	if !strings.Contains(merged, `"math"`) {
		t.Errorf("expected math import to be spliced into merged imports, got:\n%s", merged)
	}
	if !strings.Contains(merged, `"fmt"`) {
		t.Errorf("expected fmt import to be preserved, got:\n%s", merged)
	}
	if !strings.Contains(merged, "func Sqrt") {
		t.Errorf("expected Sqrt to be appended, got:\n%s", merged)
	}
	if !strings.Contains(merged, "func Print") {
		t.Errorf("expected Print to be preserved, got:\n%s", merged)
	}
}

func TestGuardC_AllowsValidNonGoRewrites(t *testing.T) {
	tmpDir := t.TempDir()
	appDir := filepath.Join(tmpDir, "projects", "webapp")
	if err := os.MkdirAll(appDir, 0755); err != nil {
		t.Fatal(err)
	}

	// 1. Valid JSON rewrite with >70% shrinkage
	jsonPath := filepath.Join(appDir, "config.json")
	origJSON := strings.Repeat(`{"key": "value", "description": "a very long configuration blob"}`+"\n", 15)
	if err := os.WriteFile(jsonPath, []byte(origJSON), 0644); err != nil {
		t.Fatal(err)
	}

	cleanJSON := `{"mode": "production", "workers": 4}`
	mockClient := llm.NewMockLLMClient()
	mockClient.CannedContent = `Updating config:
[ACTION:WRITE path="projects/webapp/config.json"]
` + "```json\n" + cleanJSON + "\n```\n"

	session := NewSession(mockClient, nil, tmpDir)
	in := strings.NewReader("update config\n/quit\n")
	var out bytes.Buffer

	ctx := context.Background()
	_ = RunConversationalUI(ctx, session, in, &out)

	if strings.Contains(out.String(), "rejected incoming truncated write") {
		t.Errorf("expected valid JSON write NOT to be rejected by Code Preservation Guard, got:\n%s", out.String())
	}
	savedJSON, err := os.ReadFile(jsonPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(savedJSON), `"mode": "production"`) {
		t.Errorf("expected new JSON to be saved, got:\n%s", string(savedJSON))
	}

	// 2. Valid HTML rewrite with >75% shrinkage
	htmlPath := filepath.Join(appDir, "index.html")
	origHTML := "<!DOCTYPE html><html><body>" + strings.Repeat("<p>Bloated legacy paragraph</p>\n", 25) + "</body></html>"
	if err := os.WriteFile(htmlPath, []byte(origHTML), 0644); err != nil {
		t.Fatal(err)
	}

	cleanHTML := `<!DOCTYPE html><html><head><title>Clean App</title></head><body><h1>Welcome</h1></body></html>`
	mockClient.CannedContent = `Updating html:
[ACTION:WRITE path="projects/webapp/index.html"]
` + "```html\n" + cleanHTML + "\n```\n"

	in2 := strings.NewReader("update html\n/quit\n")
	var out2 bytes.Buffer
	_ = RunConversationalUI(ctx, session, in2, &out2)

	if strings.Contains(out2.String(), "rejected incoming truncated write") {
		t.Errorf("expected valid HTML write NOT to be rejected by Code Preservation Guard, got:\n%s", out2.String())
	}
	savedHTML, err := os.ReadFile(htmlPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(savedHTML), "<h1>Welcome</h1>") {
		t.Errorf("expected new HTML to be saved, got:\n%s", string(savedHTML))
	}
}

func TestBuildSelfHealingPrompt_ExtractsGuardRejectionAndIncludesSource(t *testing.T) {
	tmpDir := t.TempDir()
	appDir := filepath.Join(tmpDir, "projects", "myapp")
	if err := os.MkdirAll(appDir, 0755); err != nil {
		t.Fatal(err)
	}

	filePath := filepath.Join(appDir, "sample.go")
	content := `package myapp

func Alpha() int {
	return 1
}

func Beta() int {
	return 2
}
`
	if err := os.WriteFile(filePath, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	guardDiag := `File write for "projects/myapp/sample.go" was rejected by Code Preservation Guard: rejected incoming truncated write (100 -> 30 bytes, -70%); would delete functions: Beta. Use [ACTION:PATCH path="projects/myapp/sample.go"] with search/replace blocks to modify specific lines, or provide the complete file content.`
	prompt := buildSelfHealingPrompt(tmpDir, appDir, 1, guardDiag)

	if !strings.Contains(prompt, "=== FULL FILE CONTENT FOR projects/myapp/sample.go") {
		t.Errorf("expected prompt to contain FULL FILE CONTENT for rejected file, got:\n%s", prompt)
	}
	if !strings.Contains(prompt, "func Alpha()") || !strings.Contains(prompt, "func Beta()") {
		t.Errorf("expected prompt to contain file source code, got:\n%s", prompt)
	}
	if !strings.Contains(prompt, "=== GUARD REJECTION NOTICE ===") {
		t.Errorf("expected prompt to contain GUARD REJECTION NOTICE, got:\n%s", prompt)
	}
}

func TestIsSingleFunctionUpdate(t *testing.T) {
	multiCode := `package server

type Server struct {
	addr string
}

func NewServer(addr string) *Server {
	return &Server{addr: addr}
}

func (s *Server) Start() error {
	return nil
}

func (s *Server) Shutdown() error {
	return nil
}
`

	// Case 1: Snippet with no package header
	snippetNoPkg := `func (s *Server) Start() error {
	return nil
}`
	if !isSingleFunctionUpdate(multiCode, snippetNoPkg) {
		t.Errorf("expected snippet without package header to be single function update")
	}

	// Case 2: Snippet with package header and only 1 method
	singleMethod := `package server

func (s *Server) Start() error {
	return nil
}`
	if !isSingleFunctionUpdate(multiCode, singleMethod) {
		t.Errorf("expected single method to be recognized as single function update")
	}

	// Case 3: Full rewrite with struct, constructor, and 2 methods
	fullRewrite := `package server

type Server struct {
	addr string
}

func NewServer(addr string) *Server {
	return &Server{addr: addr}
}

func (s *Server) Start() error {
	return nil
}`
	if isSingleFunctionUpdate(multiCode, fullRewrite) {
		t.Errorf("expected full rewrite to NOT be classified as single function update")
	}
}

func TestAreMissingSymbolsDeclaredInPackage(t *testing.T) {
	tmpDir := t.TempDir()
	pkgDir := filepath.Join(tmpDir, "pkg", "server")
	if err := os.MkdirAll(pkgDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Sibling file api.go defines handleHealth and handleMetrics
	apiContent := `package server

import "net/http"

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
}

func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
}
`
	if err := os.WriteFile(filepath.Join(pkgDir, "api.go"), []byte(apiContent), 0644); err != nil {
		t.Fatal(err)
	}

	serverPath := filepath.Join(pkgDir, "server.go")

	// Missing methods are declared in api.go
	missingMethods := []string{"(*Server).handleHealth", "(*Server).handleMetrics"}
	if !areMissingSymbolsDeclaredInPackage(tmpDir, serverPath, missingMethods, nil) {
		t.Errorf("expected missing methods to be found in companion api.go file")
	}

	// Unknown method should return false
	if areMissingSymbolsDeclaredInPackage(tmpDir, serverPath, []string{"(*Server).nonExistent"}, nil) {
		t.Errorf("expected nonExistent method to return false")
	}
}

func TestCheckImplementationDepth_CompanionFilesAndSubstance(t *testing.T) {
	prodPlan := &engine.PlanSpec{
		RawPlanContent: strings.Repeat("# Master Implementation Plan\nPhase details...\n", 80),
	}

	// 1. api.go with 21 non-empty lines should NOT be rejected
	apiGo := `package server

import (
	"encoding/json"
	"net/http"
	"strings"
)

func (s *Server) HandleEntityREST(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(r.URL.Path, "/")
	if len(parts) < 4 {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	id := parts[3]
	val, ok := s.Store.Get(id)
	if !ok {
		http.Error(w, "entity not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(val)
}
`
	tooShallow, reason := checkImplementationDepth("projects/telemetry-service/pkg/server/api.go", apiGo, prodPlan)
	if tooShallow {
		t.Errorf("expected api.go to be exempt from depth guard, got rejected: %s", reason)
	}

	// 2. Toy stub with 6 lines should still be rejected
	toyStub := `package server

func Dummy() {}
`
	tooShallow, _ = checkImplementationDepth("projects/telemetry-service/pkg/server/logic.go", toyStub, prodPlan)
	if !tooShallow {
		t.Errorf("expected 3-line toy stub to be rejected")
	}
}

