// SPDX-License-Identifier: Apache-2.0
package engine

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

func testTelemetryServicePlan() *PlanSpec {
	scope := "projects/telemetry-service"
	return &PlanSpec{
		ProjectScope: scope,
		ModuleName:   "telemetryservice",
		Phases: []PlanPhase{
			{
				PhaseNumber: 1,
				Name:        "Project Scaffolding & Core Substrate",
				TargetFiles: []string{
					scope + "/go.mod",
					scope + "/pkg/canonical/canonical.go",
					scope + "/pkg/canonical/merkle.go",
					scope + "/pkg/icx/types.go",
					scope + "/pkg/icx/lattice.go",
				},
				Dependencies:    []int{},
				VerificationCmd: "go test -v ./pkg/canonical/... ./pkg/icx/...",
			},
			{
				PhaseNumber: 2,
				Name:        "Ingestion Collectors",
				TargetFiles: []string{
					scope + "/pkg/ingest/collector.go",
					scope + "/pkg/ingest/mock.go",
				},
				Dependencies:    []int{1},
				VerificationCmd: "go test -v ./pkg/ingest/...",
			},
			{
				PhaseNumber: 3,
				Name:        "Indexing & Aggregation Engine",
				TargetFiles: []string{
					scope + "/pkg/index/cursor.go",
					scope + "/pkg/index/btree.go",
				},
				Dependencies:    []int{1},
				VerificationCmd: "go test -v ./pkg/index/...",
			},
			{
				PhaseNumber: 4,
				Name:        "HTTP Server & Broadcaster",
				TargetFiles: []string{
					scope + "/pkg/server/server.go",
					scope + "/pkg/server/api.go",
				},
				Dependencies:    []int{1},
				VerificationCmd: "go test -v ./pkg/server/...",
			},
			{
				PhaseNumber: 5,
				Name:        "Web Assets & Dashboard",
				TargetFiles: []string{
					scope + "/web/embed.go",
					scope + "/web/static/index.html",
				},
				Dependencies: []int{4},
			},
			{
				PhaseNumber: 6,
				Name:        "Client Telemetry Scripts",
				TargetFiles: []string{
					scope + "/web/static/js/dashboard.js",
					scope + "/web/static/js/app.js",
				},
				Dependencies: []int{4},
			},
			{
				PhaseNumber: 7,
				Name:        "Integration Tests & Verification",
				TargetFiles: []string{
					scope + "/tests/integration_test.go",
					scope + "/main.go",
				},
				Dependencies:    []int{2, 3, 4, 5, 6},
				VerificationCmd: "go test -v ./...",
			},
		},
	}
}

// TestCompileSwarmDAG_MultiWave verifies that a 7-phase PlanSpec compiles into 4 topological waves:
// Wave 0: Phase 1 (Scaffolding & Core Types)
// Wave 1: Phases 2, 3, 4 (Ingest, Index, Server in parallel)
// Wave 2: Phases 5, 6 (Web Assets, Client Scripts in parallel)
// Wave 3: Phase 7 (Integration Verification)
func TestCompileSwarmDAG_MultiWave(t *testing.T) {
	plan := testTelemetryServicePlan()
	if plan == nil {
		t.Fatal("testTelemetryServicePlan returned nil")
	}

	dag, err := CompileSwarmDAG(plan)
	if err != nil {
		t.Fatalf("CompileSwarmDAG failed: %v", err)
	}
	if dag == nil {
		t.Fatal("expected non-nil SwarmDAG")
	}

	// 1. Verify exactly 4 execution waves
	if len(dag.Waves) != 4 {
		t.Fatalf("expected exactly 4 waves, got %d: %v", len(dag.Waves), dag.Waves)
	}

	expectedWave0 := []int{1}
	expectedWave1 := []int{2, 3, 4}
	expectedWave2 := []int{5, 6}
	expectedWave3 := []int{7}

	if !reflect.DeepEqual(dag.Waves[0], expectedWave0) {
		t.Errorf("Wave 0 mismatch: expected %v, got %v", expectedWave0, dag.Waves[0])
	}
	if !reflect.DeepEqual(dag.Waves[1], expectedWave1) {
		t.Errorf("Wave 1 mismatch: expected %v, got %v", expectedWave1, dag.Waves[1])
	}
	if !reflect.DeepEqual(dag.Waves[2], expectedWave2) {
		t.Errorf("Wave 2 mismatch: expected %v, got %v", expectedWave2, dag.Waves[2])
	}
	if !reflect.DeepEqual(dag.Waves[3], expectedWave3) {
		t.Errorf("Wave 3 mismatch: expected %v, got %v", expectedWave3, dag.Waves[3])
	}

	// 2. Verify wave indices assigned to SwarmPhases
	if dag.Phases[1].Wave != 0 {
		t.Errorf("Phase 1 wave mismatch: expected 0, got %d", dag.Phases[1].Wave)
	}
	for _, p := range []int{2, 3, 4} {
		if dag.Phases[p].Wave != 1 {
			t.Errorf("Phase %d wave mismatch: expected 1, got %d", p, dag.Phases[p].Wave)
		}
	}
	for _, p := range []int{5, 6} {
		if dag.Phases[p].Wave != 2 {
			t.Errorf("Phase %d wave mismatch: expected 2, got %d", p, dag.Phases[p].Wave)
		}
	}
	if dag.Phases[7].Wave != 3 {
		t.Errorf("Phase 7 wave mismatch: expected 3, got %d", dag.Phases[7].Wave)
	}

	// 3. Verify worker assignments (1..N within each wave)
	if dag.Phases[1].AssignedWorker != 1 {
		t.Errorf("Phase 1 worker mismatch: expected 1, got %d", dag.Phases[1].AssignedWorker)
	}
	if dag.Phases[2].AssignedWorker != 1 || dag.Phases[3].AssignedWorker != 2 || dag.Phases[4].AssignedWorker != 3 {
		t.Errorf("Wave 1 workers mismatch: got [%d, %d, %d]",
			dag.Phases[2].AssignedWorker, dag.Phases[3].AssignedWorker, dag.Phases[4].AssignedWorker)
	}
	if dag.Phases[5].AssignedWorker != 1 || dag.Phases[6].AssignedWorker != 2 {
		t.Errorf("Wave 2 workers mismatch: got [%d, %d]",
			dag.Phases[5].AssignedWorker, dag.Phases[6].AssignedWorker)
	}
	if dag.Phases[7].AssignedWorker != 1 {
		t.Errorf("Phase 7 worker mismatch: expected 1, got %d", dag.Phases[7].AssignedWorker)
	}

	// 4. Verify Phase 1 ExportsContract contains core packages
	p1Contracts := dag.Phases[1].ExportsContract
	hasCanonical := false
	hasICX := false
	for _, c := range p1Contracts {
		if strings.Contains(c, "canonical") {
			hasCanonical = true
		}
		if strings.Contains(c, "icx") {
			hasICX = true
		}
	}
	if !hasCanonical || !hasICX {
		t.Errorf("Phase 1 ExportsContract missing canonical or icx: %v", p1Contracts)
	}

	// 5. Verify ScopePrefix
	if dag.Phases[3].ScopePrefix != "pkg/index/" {
		t.Errorf("Phase 3 ScopePrefix mismatch: expected 'pkg/index/', got '%s'", dag.Phases[3].ScopePrefix)
	}

	// 6. Verify CriticalPath
	if len(dag.CriticalPath) != 4 {
		t.Errorf("expected critical path length 4, got %d: %v", len(dag.CriticalPath), dag.CriticalPath)
	}
	if dag.CriticalPath[0] != 1 || dag.CriticalPath[3] != 7 {
		t.Errorf("expected critical path to start with 1 and end with 7, got %v", dag.CriticalPath)
	}
}

// TestCompileSwarmDAG_CycleDetection injects a circular dependency (Phase 2 depends on 3; Phase 3 depends on 2)
// and asserts ErrCyclicPlanGraph is returned with detailed diagnostics.
func TestCompileSwarmDAG_CycleDetection(t *testing.T) {
	plan := &PlanSpec{
		ProjectScope: "projects/cyclic-test",
		ModuleName:   "cyclictest",
		Phases: []PlanPhase{
			{
				PhaseNumber:     1,
				Name:            "Bootstrap",
				TargetFiles:     []string{"pkg/boot/boot.go"},
				Dependencies:    []int{},
				VerificationCmd: "go test ./pkg/boot/...",
			},
			{
				PhaseNumber:     2,
				Name:            "Module A",
				TargetFiles:     []string{"pkg/moda/a.go"},
				Dependencies:    []int{3}, // 2 depends on 3
				VerificationCmd: "go test ./pkg/moda/...",
			},
			{
				PhaseNumber:     3,
				Name:            "Module B",
				TargetFiles:     []string{"pkg/modb/b.go"},
				Dependencies:    []int{2}, // 3 depends on 2
				VerificationCmd: "go test ./pkg/modb/...",
			},
		},
	}

	dag, err := CompileSwarmDAG(plan)
	if err == nil {
		t.Fatal("expected ErrCyclicPlanGraph error, got nil")
	}
	if dag != nil {
		t.Fatalf("expected nil SwarmDAG on cycle, got %v", dag)
	}

	// Verify error identity matches ErrCyclicPlanGraph
	if !errors.Is(err, ErrCyclicPlanGraph) {
		t.Errorf("expected errors.Is(err, ErrCyclicPlanGraph) to be true, got err = %v", err)
	}

	// Verify diagnostic mentions cycle and the participating phases
	errMsg := err.Error()
	if !strings.Contains(errMsg, "cycle") && !strings.Contains(errMsg, "Cyclic") {
		t.Errorf("expected diagnostic to mention 'cycle', got: %s", errMsg)
	}
	if !strings.Contains(errMsg, "2") || !strings.Contains(errMsg, "3") {
		t.Errorf("expected diagnostic to mention participating phases 2 and 3, got: %s", errMsg)
	}

	// Verify CyclicPlanGraphError type extraction
	var cycleErr *CyclicPlanGraphError
	if !errors.As(err, &cycleErr) {
		t.Errorf("expected error to be of type *CyclicPlanGraphError, got %T", err)
	} else {
		if len(cycleErr.Cycle) < 2 {
			t.Errorf("expected cycle path to contain at least 2 nodes, got %v", cycleErr.Cycle)
		}
	}
}

// TestCompileSwarmDAG_IndependentPhases verifies that a plan with 5 independent phases
// compiles into a single parallel wave (Waves[0] = [1, 2, 3, 4, 5]).
func TestCompileSwarmDAG_IndependentPhases(t *testing.T) {
	plan := &PlanSpec{
		ProjectScope: "projects/parallel-independent",
		ModuleName:   "independent",
		Phases: []PlanPhase{
			{
				PhaseNumber: 1,
				Name:        "Service One",
				TargetFiles: []string{"services/svc1/main.go"},
			},
			{
				PhaseNumber: 2,
				Name:        "Service Two",
				TargetFiles: []string{"services/svc2/main.go"},
			},
			{
				PhaseNumber: 3,
				Name:        "Service Three",
				TargetFiles: []string{"services/svc3/main.go"},
			},
			{
				PhaseNumber: 4,
				Name:        "Service Four",
				TargetFiles: []string{"services/svc4/main.go"},
			},
			{
				PhaseNumber: 5,
				Name:        "Service Five",
				TargetFiles: []string{"services/svc5/main.go"},
			},
		},
	}

	dag, err := CompileSwarmDAG(plan)
	if err != nil {
		t.Fatalf("CompileSwarmDAG failed on independent phases: %v", err)
	}
	if dag == nil {
		t.Fatal("expected non-nil SwarmDAG")
	}

	// Exactly 1 wave
	if len(dag.Waves) != 1 {
		t.Fatalf("expected exactly 1 wave for independent phases, got %d: %v", len(dag.Waves), dag.Waves)
	}

	expectedWave0 := []int{1, 2, 3, 4, 5}
	if !reflect.DeepEqual(dag.Waves[0], expectedWave0) {
		t.Fatalf("expected Waves[0] = %v, got %v", expectedWave0, dag.Waves[0])
	}

	// All phases must have Wave == 0 and assigned worker 1..5
	for i := 1; i <= 5; i++ {
		sp, ok := dag.Phases[i]
		if !ok {
			t.Fatalf("phase %d missing from dag.Phases", i)
		}
		if sp.Wave != 0 {
			t.Errorf("phase %d: expected wave 0, got %d", i, sp.Wave)
		}
		if sp.AssignedWorker != i {
			t.Errorf("phase %d: expected assigned worker %d, got %d", i, i, sp.AssignedWorker)
		}
	}

	if len(dag.CriticalPath) != 1 {
		t.Errorf("expected critical path of length 1, got %v", dag.CriticalPath)
	}
}

// TestPlanSpec_BackwardsCompatibility verifies that ParsePlanMarkdown outputs continue to work
// seamlessly with ExecuteTask in pkg/engine/orchestrator.go.
func TestPlanSpec_BackwardsCompatibility(t *testing.T) {
	planMarkdown := `# Backwards Compatibility Test Plan

### Phase 1: Core Substrate
- Target: ` + "`" + `pkg/core/core.go` + "`" + `
` + "```go" + `
go test -v ./pkg/core/...
` + "```" + `
Implement core substrate and models.

### Phase 2: High-Level Client
- Target: ` + "`" + `pkg/client/client.go` + "`" + `
` + "```go" + `
go test -v ./pkg/client/...
` + "```" + `
Implement client interfaces and HTTP transport.
`

	spec := ParsePlanMarkdown(planMarkdown)
	if spec == nil {
		t.Fatal("ParsePlanMarkdown returned nil")
	}

	if len(spec.Phases) != 2 {
		t.Fatalf("expected 2 phases, got %d", len(spec.Phases))
	}

	p1 := spec.Phases[0]
	if p1.PhaseNumber != 1 || p1.Name != "Core Substrate" {
		t.Errorf("Phase 1 parsed incorrectly: %+v", p1)
	}
	if len(p1.TargetFiles) == 0 || p1.TargetFiles[0] != "pkg/core/core.go" {
		t.Errorf("Phase 1 TargetFiles mismatch: %v", p1.TargetFiles)
	}

	tmpDir := t.TempDir()

	// Verify plan state inspection
	if spec.IsComplete(tmpDir) {
		t.Error("expected IsComplete to return false in empty directory")
	}

	active := spec.DetermineActivePhase(tmpDir)
	if active == nil || active.PhaseNumber != 1 {
		t.Errorf("expected active phase 1, got %+v", active)
	}

	present, missing := spec.IsPhaseFilesPresent(tmpDir, &p1)
	if present || len(missing) == 0 {
		t.Errorf("expected phase files to be missing in empty dir: present=%v, missing=%v", present, missing)
	}

	// Verify seamless interop with single-agent Turn Orchestrator
	orch, err := NewOrchestrator(EngineConfig{
		WorkspaceDir: tmpDir,
		Offline:      true,
		MaxTurns:     1,
	})
	if err != nil {
		t.Fatalf("NewOrchestrator failed: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	report, _ := orch.ExecuteTask(ctx, RunConfig{
		Task:         "Execute Phase 1",
		WorkspaceDir: tmpDir,
		Offline:      true,
		MaxTurns:     1,
	})

	if report == nil {
		t.Fatal("expected non-nil RunReport from ExecuteTask")
	}
}

// TestParsePlanMarkdown_FuzzyMilestonesAndAliasing verifies that non-numeric milestones
// (e.g. "Milestone A", "Milestone B") are auto-aliased and dependencies are resolved seamlessly.
func TestParsePlanMarkdown_FuzzyMilestonesAndAliasing(t *testing.T) {
	planMd := `# Architectural Roadmap

### Milestone A: Core Engine Substrate
* Target Files:
  - internal/core/engine.go
* Verification: go test -v ./internal/core/...

### Milestone B: Index Engine Pipeline
* Target Files:
  - internal/index/scanner.go
* Dependencies: Milestone A
* Verification:
` + "```bash\n" + `go test -v ./internal/index/...
` + "```\n" + `

### Milestone C: Full System Verification
* Target Files:
  - tests/e2e_test.go
* Dependencies:
  - Milestone A
  - Milestone B
* Verification: ` + "`go test -v ./tests/...`" + `
`

	spec := ParsePlanMarkdown(planMd)
	if spec == nil {
		t.Fatal("expected non-nil PlanSpec from fuzzy milestone plan")
	}

	if len(spec.Phases) != 3 {
		t.Fatalf("expected 3 phases, got %d", len(spec.Phases))
	}

	// Phase 1 (Milestone A)
	p1 := spec.Phases[0]
	if p1.PhaseNumber != 1 {
		t.Errorf("expected phase 1 for Milestone A, got %d", p1.PhaseNumber)
	}
	if p1.OriginalID != "Milestone A" {
		t.Errorf("expected OriginalID 'Milestone A', got %q", p1.OriginalID)
	}
	if len(p1.Dependencies) != 0 {
		t.Errorf("expected 0 dependencies for Milestone A, got %v", p1.Dependencies)
	}
	if p1.VerificationCmd != "go test -v ./internal/core/..." {
		t.Errorf("expected plain verification cmd, got %q", p1.VerificationCmd)
	}

	// Phase 2 (Milestone B)
	p2 := spec.Phases[1]
	if p2.PhaseNumber != 2 {
		t.Errorf("expected phase 2 for Milestone B, got %d", p2.PhaseNumber)
	}
	if len(p2.Dependencies) != 1 || p2.Dependencies[0] != 1 {
		t.Errorf("expected Milestone B to depend on Phase 1, got %v", p2.Dependencies)
	}
	if p2.VerificationCmd != "go test -v ./internal/index/..." {
		t.Errorf("expected code-block verification cmd, got %q", p2.VerificationCmd)
	}

	// Phase 3 (Milestone C)
	p3 := spec.Phases[2]
	if p3.PhaseNumber != 3 {
		t.Errorf("expected phase 3 for Milestone C, got %d", p3.PhaseNumber)
	}
	expectedDeps := []int{1, 2}
	if !reflect.DeepEqual(p3.Dependencies, expectedDeps) {
		t.Errorf("expected Milestone C to depend on %v, got %v", expectedDeps, p3.Dependencies)
	}
	if p3.VerificationCmd != "go test -v ./tests/..." {
		t.Errorf("expected backtick verification cmd, got %q", p3.VerificationCmd)
	}

	// Verify SwarmDAG compilation
	dag, err := CompileSwarmDAG(spec)
	if err != nil {
		t.Fatalf("CompileSwarmDAG failed on fuzzy milestone plan: %v", err)
	}
	if len(dag.Waves) != 3 {
		t.Fatalf("expected 3 waves, got %d: %v", len(dag.Waves), dag.Waves)
	}
}

// TestParsePlanMarkdown_FuzzyHeadingsVariants verifies support for Steps, Tasks, and numbered headings.
func TestParsePlanMarkdown_FuzzyHeadingsVariants(t *testing.T) {
	planMd := `# Modern Project Plan

### Step 1: Initial Scaffolding
* Target Files: pkg/scaffold/scaffold.go
* Verification: go test ./pkg/scaffold/...

## Task 2 - Concurrency Worker
* Target Files: pkg/worker/worker.go
* Dependencies: Step 1
* Verification: go test ./pkg/worker/...

## 3. Storage Layer
* Target Files: pkg/storage/store.go
* Dependencies: [1, 2]
* Verification: go test ./pkg/storage/...
`

	spec := ParsePlanMarkdown(planMd)
	if spec == nil {
		t.Fatal("expected non-nil PlanSpec")
	}
	if len(spec.Phases) != 3 {
		t.Fatalf("expected 3 phases, got %d", len(spec.Phases))
	}

	if spec.Phases[0].PhaseNumber != 1 || spec.Phases[0].Name != "Initial Scaffolding" {
		t.Errorf("Phase 1 mismatch: %+v", spec.Phases[0])
	}
	if spec.Phases[1].PhaseNumber != 2 || len(spec.Phases[1].Dependencies) != 1 || spec.Phases[1].Dependencies[0] != 1 {
		t.Errorf("Phase 2 dependencies mismatch: %+v", spec.Phases[1])
	}
	if spec.Phases[2].PhaseNumber != 3 || len(spec.Phases[2].Dependencies) != 2 {
		t.Errorf("Phase 3 dependencies mismatch: %+v", spec.Phases[2])
	}
}

// TestParsePlanMarkdown_TargetSymbolsExtraction verifies fine-grained symbol scoping.
func TestParsePlanMarkdown_TargetSymbolsExtraction(t *testing.T) {
	planMd := `# Concurrency Sharding Plan

### Phase 14: Engine Worker Addition
* Target Files:
  - internal/core/engine.go (WorkerPool, NewWorkerPool)
* Verification: go test ./internal/core/...

### Phase 15: Metrics Hook
* Target Files:
  - internal/core/engine.go
* Target Symbols:
  - internal/core/engine.go#MetricsHook
  - internal/core/engine.go#RegisterHook
* Verification: go test ./internal/core/...
`

	spec := ParsePlanMarkdown(planMd)
	if spec == nil {
		t.Fatal("expected non-nil PlanSpec")
	}
	if len(spec.Phases) != 2 {
		t.Fatalf("expected 2 phases, got %d", len(spec.Phases))
	}

	p14 := spec.Phases[0]
	syms14 := p14.TargetSymbols["internal/core/engine.go"]
	if len(syms14) != 2 || syms14[0] != "WorkerPool" || syms14[1] != "NewWorkerPool" {
		t.Errorf("Phase 14 symbols mismatch: %v", syms14)
	}

	p15 := spec.Phases[1]
	syms15 := p15.TargetSymbols["internal/core/engine.go"]
	if len(syms15) != 2 || syms15[0] != "MetricsHook" || syms15[1] != "RegisterHook" {
		t.Errorf("Phase 15 symbols mismatch: %v", syms15)
	}
}

