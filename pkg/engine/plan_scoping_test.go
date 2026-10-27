package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPlanSpec_FileExists_ScopedAndBasenameResolution(t *testing.T) {
	ws := t.TempDir()
	scope := "projects/widget-service"
	projectDir := filepath.Join(ws, filepath.FromSlash(scope))

	// Create project directory structure
	canonicalDir := filepath.Join(projectDir, "pkg", "canonical")
	if err := os.MkdirAll(canonicalDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Write files
	if err := os.WriteFile(filepath.Join(projectDir, "go.mod"), []byte("module widgetservice\n\ngo 1.22\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(canonicalDir, "canonical.go"), []byte("package canonical\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(canonicalDir, "merkle.go"), []byte("package canonical\n"), 0644); err != nil {
		t.Fatal(err)
	}

	plan := &PlanSpec{
		ProjectScope: scope,
		ModuleName:   "widgetservice",
		Phases: []PlanPhase{
			{
				PhaseNumber: 1,
				Name:        "Phase 1 Core",
				TargetFiles: []string{
					"go.mod",                               // relative to project
					"pkg/canonical/canonical.go",           // relative to project
					"merkle.go",                            // short filename
				},
			},
			{
				PhaseNumber: 2,
				Name:        "Phase 2 Service",
				TargetFiles: []string{
					scope + "/pkg/service/service.go",     // full scoped path
				},
			},
		},
	}

	// Phase 1 files should all exist via FileExists
	for _, tf := range plan.Phases[0].TargetFiles {
		if !plan.FileExists(ws, tf) {
			t.Errorf("expected plan.FileExists(ws, %q) == true", tf)
		}
	}

	complete, missing := plan.IsPhaseFilesPresent(ws, &plan.Phases[0])
	if !complete || len(missing) > 0 {
		t.Fatalf("expected Phase 1 complete, got complete=%v, missing=%v", complete, missing)
	}

	// Active phase should be Phase 2
	active := plan.DetermineActivePhase(ws)
	if active == nil || active.PhaseNumber != 2 {
		t.Fatalf("expected Phase 2 active, got %+v", active)
	}

	if plan.IsComplete(ws) {
		t.Fatalf("expected plan.IsComplete() == false since Phase 2 is missing")
	}

	// Now create Phase 2 file
	svcDir := filepath.Join(projectDir, "pkg", "service")
	_ = os.MkdirAll(svcDir, 0755)
	_ = os.WriteFile(filepath.Join(svcDir, "service.go"), []byte("package service\n"), 0644)

	// Now all files exist
	activeAfter := plan.DetermineActivePhase(ws)
	if activeAfter != nil {
		t.Fatalf("expected nil active phase (all complete), got Phase %d", activeAfter.PhaseNumber)
	}

	if !plan.IsComplete(ws) {
		t.Fatalf("expected plan.IsComplete() == true")
	}
}

func TestPlanSpec_FileExists_NoHostRepoPollution(t *testing.T) {
	ws := t.TempDir()

	// Simulate host repository packages existing in workspace root
	hostIcxDir := filepath.Join(ws, "pkg", "icx")
	if err := os.MkdirAll(hostIcxDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hostIcxDir, "types.go"), []byte("package icx\n// host repo types"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hostIcxDir, "lattice.go"), []byte("package icx\n// host repo lattice"), 0644); err != nil {
		t.Fatal(err)
	}

	// Create scoped project directory with only go.mod
	scope := "projects/telemetry-service"
	projDir := filepath.Join(ws, filepath.FromSlash(scope))
	if err := os.MkdirAll(projDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projDir, "go.mod"), []byte("module telemetryservice\n"), 0644); err != nil {
		t.Fatal(err)
	}

	plan := &PlanSpec{
		ProjectScope: scope,
		ModuleName:   "telemetryservice",
		Phases: []PlanPhase{
			{
				PhaseNumber: 1,
				Name:        "Phase 1 Core",
				TargetFiles: []string{
					scope + "/go.mod",
					"pkg/canonical/canonical.go",
					"merkle.go",
					"pkg/icx/types.go",
					"pkg/icx/lattice.go",
				},
			},
		},
	}

	// go.mod exists in project scope
	if !plan.FileExists(ws, scope+"/go.mod") {
		t.Errorf("expected plan.FileExists(ws, %q) == true", scope+"/go.mod")
	}

	// pkg/icx/types.go and pkg/icx/lattice.go exist in host repo ws/pkg/icx, but NOT in project scope!
	// FileExists MUST return false!
	if plan.FileExists(ws, "pkg/icx/types.go") {
		t.Errorf("FileExists falsely matched host repo file for pkg/icx/types.go!")
	}
	if plan.FileExists(ws, "pkg/icx/lattice.go") {
		t.Errorf("FileExists falsely matched host repo file for pkg/icx/lattice.go!")
	}

	// Phase 1 must NOT be complete
	complete, missing := plan.IsPhaseFilesPresent(ws, &plan.Phases[0])
	if complete {
		t.Errorf("expected Phase 1 to be incomplete")
	}
	if len(missing) != 4 {
		t.Errorf("expected 4 missing files, got %d: %v", len(missing), missing)
	}

	// Now write canonical.go containing Merkle implementation inside project scope
	canonicalDir := filepath.Join(projDir, "pkg", "canonical")
	_ = os.MkdirAll(canonicalDir, 0755)
	_ = os.WriteFile(filepath.Join(canonicalDir, "canonical.go"), []byte("package canonical\n// ComputeMerkleRoot calculates root"), 0644)

	// Both canonical.go and merkle.go should now be recognized as satisfied in project scope
	if !plan.FileExists(ws, "pkg/canonical/canonical.go") {
		t.Errorf("expected plan.FileExists(ws, canonical.go) == true")
	}
	if !plan.FileExists(ws, "merkle.go") {
		t.Errorf("expected plan.FileExists(ws, merkle.go) == true when canonical.go contains Merkle")
	}
}

func TestParseMultiPhasePlanMarkdown(t *testing.T) {
	content := `# Telemetry Service Implementation Plan

## Executive Summary
Pure Go telemetry microservice located in ` + "`projects/telemetry-service`" + `.

## Phase 1: Core Substrate
- Step 1.1: Create ` + "`projects/telemetry-service/go.mod`" + ` and ` + "`projects/telemetry-service/pkg/canonical/canonical.go`" + `
Verification: ` + "`go test ./pkg/canonical/...`" + `

## Phase 2: Collectors
- Step 2.1: Implement ` + "`projects/telemetry-service/pkg/ingest/http.go`" + `, ` + "`projects/telemetry-service/pkg/ingest/grpc.go`" + `, ` + "`projects/telemetry-service/pkg/ingest/udp.go`" + `, ` + "`projects/telemetry-service/pkg/ingest/batch.go`" + `, ` + "`projects/telemetry-service/pkg/ingest/retry.go`" + `, and ` + "`projects/telemetry-service/pkg/ingest/mock.go`" + ` (connects to api.service.io and stream.example.com, binary telemetry.exe excluded).
Verification: ` + "`go test ./pkg/ingest/...`" + `
`
	spec := ParsePlanMarkdown(content)
	if spec == nil {
		t.Fatal("spec is nil")
	}
	if len(spec.Phases) != 2 {
		t.Fatalf("expected 2 phases, got %d", len(spec.Phases))
	}
	// Verify Phase 2 has clean target files without web domains
	ph2 := spec.Phases[1]
	for _, f := range ph2.TargetFiles {
		if strings.HasSuffix(f, ".com") || strings.HasSuffix(f, ".io") {
			t.Errorf("Phase 2 contains invalid web domain as target file: %q", f)
		}
	}
	if len(ph2.TargetFiles) != 6 {
		t.Errorf("expected 6 files in Phase 2, got %d: %v", len(ph2.TargetFiles), ph2.TargetFiles)
	}
	// Verify no phase contains .exe binaries or URLs
	for _, ph := range spec.Phases {
		for _, f := range ph.TargetFiles {
			if strings.HasSuffix(f, ".exe") || strings.Contains(f, "://") {
				t.Errorf("Phase %d contains invalid target file: %q", ph.PhaseNumber, f)
			}
		}
	}

	// Verify Architecture preamble is captured
	if !strings.Contains(spec.Architecture, "Executive Summary") {
		t.Errorf("expected Architecture to contain Executive Summary, got %d bytes", len(spec.Architecture))
	}

	// Verify Phase 1 SpecSection contains Step 1.1
	if !strings.Contains(spec.Phases[0].SpecSection, "Step 1.1") {
		t.Errorf("expected Phase 1 SpecSection to contain Step 1.1, got: %s", spec.Phases[0].SpecSection)
	}

	// Verify Phase 1 prompt generation
	p1Prompt := spec.GetPhasePrompt(1, t.TempDir())
	if !strings.Contains(p1Prompt, "Step 1.1") || !strings.Contains(p1Prompt, "ZERO STUBS") {
		t.Errorf("expected Phase 1 prompt to contain Step 1.1 and ZERO STUBS, got length: %d", len(p1Prompt))
	}
}

func TestPlanSpec_RichSpecExtraction(t *testing.T) {
	planMd := `# Master System Plan

## Architecture Overview
System uses Pure Go 1.22 with embedded HTTP dashboard.

### 5.2 pkg/canonical: Canonical Hashing
` + "```go\n// pkg/canonical/canonical.go\npackage canonical\nfunc ComputeMerkleRoot(leaves []string) string { return \"\" }\n```" + `

### Phase 1: Core Substrate
- Step 1.1: Create projects/service/go.mod
- Step 1.2: Implement projects/service/pkg/canonical/canonical.go
Verification: ` + "`go test ./pkg/canonical/...`" + `

### Phase 2: Web Client
- Step 2.1: Implement projects/service/web/index.html
`

	spec := ParsePlanMarkdown(planMd)
	if spec == nil {
		t.Fatal("expected non-nil spec")
	}
	if len(spec.Phases) != 2 {
		t.Fatalf("expected 2 phases, got %d", len(spec.Phases))
	}
	if !strings.Contains(spec.Architecture, "Architecture Overview") {
		t.Errorf("expected Architecture to contain 'Architecture Overview', got: %s", spec.Architecture)
	}

	ph1 := spec.Phases[0]
	if !strings.Contains(ph1.SpecSection, "Step 1.1") {
		t.Errorf("expected SpecSection to contain Step 1.1, got: %s", ph1.SpecSection)
	}

	// Verify FileSpecs has canonical.go code
	canonicalKey := "projects/service/pkg/canonical/canonical.go"
	fileSpec, ok := ph1.FileSpecs[canonicalKey]
	if !ok || !strings.Contains(fileSpec, "ComputeMerkleRoot") {
		t.Errorf("expected FileSpecs for %q to contain ComputeMerkleRoot, got: %q", canonicalKey, fileSpec)
	}

	// Test GetPhasePrompt
	prompt := spec.GetPhasePrompt(1, t.TempDir())
	if !strings.Contains(prompt, "=== PLAN EXECUTION: PHASE 1 / 2 ===") {
		t.Errorf("expected prompt to contain phase execution header, got: %s", prompt)
	}
	if !strings.Contains(prompt, "ComputeMerkleRoot") {
		t.Errorf("expected prompt to contain extracted file spec, got: %s", prompt)
	}
	if !strings.Contains(prompt, "ZERO STUBS") {
		t.Errorf("expected prompt to contain zero stubs invariant")
	}
}

func TestDetectPlanSpec_IgnoreCompilerDiagnostics(t *testing.T) {
	// A compiler diagnostic containing "projects/my-service" must NOT trigger plan detection
	errorOutput := "python: can't open file '/workspace/projects/my-service/projects/graph-viewer/test_graph.py': [Errno 2] No such file or directory"
	spec := DetectPlanSpec(errorOutput, ".")
	if spec != nil {
		t.Fatalf("expected nil PlanSpec for compiler error output, but got %v", spec)
	}

	buildFailedOutput := "[Build Failed] python -m unittest projects/graph-viewer/test_graph.py failed:\nModuleNotFoundError: No module named 'projects/my-service'"
	spec2 := DetectPlanSpec(buildFailedOutput, ".")
	if spec2 != nil {
		t.Fatalf("expected nil PlanSpec for build failed output, but got %v", spec2)
	}
}

