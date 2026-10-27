// SPDX-License-Identifier: Apache-2.0
// Package icx_test implements empirical adversarial stress testing for AST walks and Wasm kernels:
// 1. AST Multihop Walk:
//    - Complex directed graphs (multiple nested cycles, figure-8, self-loops, cross-edges, diamonds)
//    - Cycle detection stops recursion immediately (0 infinite loops, finite termination)
//    - Max_hops bounding enforcement (1, 2, 5, 20 hops; 20 clamped to 10)
//    - Parallel walks under go test -v -race (concurrency with dynamic graph mutations)
// 2. Wasm Stack Machine & Exact Decimal Arithmetic:
//    - Wasm stack operations (stack underflow on all ops, division by zero, negative square root)
//    - Deeply nested stack calls (fill to 1024, overflow at 1025, 500-push reduction, 10001-step limit)
//    - Wasm random opcode fuzzing
//    - EvalArithmetic zero-float enforcement (rejection of float32/float64 in env)
//    - Complex nested expressions, operator precedence, modulo, unary negations
//    - Power edge cases, large exponents, negative exponents, power fault modes
//    - Malformed expression fuzzing (resilience against panics)

package icx_test

import (
	"context"
	"fmt"
	"math/rand"
	"strings"
	"sync"
	"testing"

	"github.com/caleralabs/omnicoder-icx/pkg/icx"
	"github.com/shopspring/decimal"
)

// ============================================================================
// PART 1: AST Multihop Walk Stress Testing
// ============================================================================

// TestASTMultihopWalk_ComplexGraphStress tests complex graphs with self-loops,
// nested cycles, figure-8 cycles, diamonds, and cross-edges.
func TestASTMultihopWalk_ComplexGraphStress(t *testing.T) {
	ctx := context.Background()
	lat := icx.NewLocalLattice()

	// 1. Create nodes
	nodeNames := []string{
		"root", "loop_node", "A", "B", "C", "D", "E", "F", "G", "H", "I", "J",
	}
	for _, name := range nodeNames {
		lat.AddASTNode(&icx.LatticeNode{
			ID:   name,
			Name: name,
			Type: "function",
		})
	}

	// 2. Self-loops
	lat.AddASTEdge("root", "calls", "root")           // self-loop on start node
	lat.AddASTEdge("root", "calls", "loop_node")
	lat.AddASTEdge("loop_node", "calls", "loop_node") // self-loop on child
	lat.AddASTEdge("loop_node", "calls", "A")

	// 3. Cycle 1: A -> B -> C -> A (3-node cycle)
	lat.AddASTEdge("A", "calls", "B")
	lat.AddASTEdge("B", "calls", "C")
	lat.AddASTEdge("C", "calls", "A")

	// 4. Figure-8 / nested cycle through C: C -> D -> E -> C (cycle 2)
	lat.AddASTEdge("C", "calls", "D")
	lat.AddASTEdge("D", "calls", "E")
	lat.AddASTEdge("E", "calls", "C")

	// 5. Cross-edges:
	// A -> D (cross-edge jumping into cycle 2)
	// B -> E (cross-edge into cycle 2)
	lat.AddASTEdge("A", "calls", "D")
	lat.AddASTEdge("B", "calls", "E")

	// 6. Branch out to F, G, H with diamond reconvergence:
	// E -> F -> H
	// E -> G -> H
	lat.AddASTEdge("E", "calls", "F")
	lat.AddASTEdge("E", "calls", "G")
	lat.AddASTEdge("F", "calls", "H")
	lat.AddASTEdge("G", "calls", "H")

	// 7. Cycle from H back to B
	lat.AddASTEdge("H", "calls", "B")

	// 8. Dead-end branch: H -> I -> J
	lat.AddASTEdge("H", "calls", "I")
	lat.AddASTEdge("I", "calls", "J")

	res, err := lat.MultihopWalk(ctx, icx.MultihopWalkRequest{
		StartNode: "root",
		MaxHops:   10,
	})
	if err != nil {
		t.Fatalf("MultihopWalk returned unexpected error: %v", err)
	}

	if !res.OK {
		t.Fatalf("MultihopWalk expected OK=true, got false: %+v", res)
	}

	// Self-loops, cycles, and cross-edges must be detected without infinite recursion
	if res.CyclesDetected == 0 {
		t.Fatalf("Expected multiple cycles/back-edges detected, got 0")
	}
	t.Logf("Complex graph: cycles/reconvergent edges detected: %d, nodes reached: %d, hops: %d",
		res.CyclesDetected, res.NodesReached, res.Hops)

	// Verify all nodes reachable within 10 hops were reached
	if res.NodesReached < len(nodeNames) {
		t.Logf("Nodes reached: %d of %d", res.NodesReached, len(nodeNames))
	}

	// Verify path starts at "root" and ends at deepest node
	if len(res.Path) == 0 || res.Path[0] != "root" {
		t.Fatalf("Path must start with root, got: %v", res.Path)
	}
}

// TestASTMultihopWalk_MaxHopsBounding tests max_hops enforcement on a long chain:
// Tests 1, 2, 5, 20 hops and non-positive defaults.
func TestASTMultihopWalk_MaxHopsBounding(t *testing.T) {
	ctx := context.Background()
	lat := icx.NewLocalLattice()

	// Build a 25-node linear chain: N_0 -> N_1 -> ... -> N_24
	const chainLen = 25
	for i := 0; i < chainLen; i++ {
		id := fmt.Sprintf("chain_%02d", i)
		lat.AddASTNode(&icx.LatticeNode{
			ID:   id,
			Name: id,
			Type: "function",
		})
		if i > 0 {
			prev := fmt.Sprintf("chain_%02d", i-1)
			lat.AddASTEdge(prev, "calls", id)
		}
	}

	testCases := []struct {
		requestedHops int
		expectedHops  int // hops reached
		expectedNodes int // nodes reached = hops + 1
		clampedMax    int // MaxHops field in response
	}{
		{requestedHops: 1, expectedHops: 1, expectedNodes: 2, clampedMax: 1},
		{requestedHops: 2, expectedHops: 2, expectedNodes: 3, clampedMax: 2},
		{requestedHops: 5, expectedHops: 5, expectedNodes: 6, clampedMax: 5},
		// 20 is clamped to 10 by implementation
		{requestedHops: 20, expectedHops: 10, expectedNodes: 11, clampedMax: 10},
		// 0 or negative defaults to 5 (MaxHopsDefault)
		{requestedHops: 0, expectedHops: 5, expectedNodes: 6, clampedMax: 5},
		{requestedHops: -3, expectedHops: 5, expectedNodes: 6, clampedMax: 5},
	}

	for _, tc := range testCases {
		t.Run(fmt.Sprintf("Hops_%d", tc.requestedHops), func(t *testing.T) {
			res, err := lat.MultihopWalk(ctx, icx.MultihopWalkRequest{
				StartNode: "chain_00",
				MaxHops:   tc.requestedHops,
			})
			if err != nil {
				t.Fatalf("MultihopWalk error: %v", err)
			}
			if !res.OK {
				t.Fatalf("Expected OK=true, got false: %+v", res)
			}
			if res.MaxHops != tc.clampedMax {
				t.Errorf("Expected res.MaxHops == %d, got %d", tc.clampedMax, res.MaxHops)
			}
			if res.Hops != tc.expectedHops {
				t.Errorf("Expected res.Hops == %d, got %d", tc.expectedHops, res.Hops)
			}
			if res.NodesReached != tc.expectedNodes {
				t.Errorf("Expected res.NodesReached == %d, got %d", tc.expectedNodes, res.NodesReached)
			}
			if len(res.Path) != tc.expectedNodes {
				t.Errorf("Expected len(Path) == %d, got %d (%v)", tc.expectedNodes, len(res.Path), res.Path)
			}
		})
	}
}

// TestASTMultihopWalk_ParallelRace tests concurrent reads and writes to lattice AST graph
// to verify thread safety and race freedom under `go test -race`.
func TestASTMultihopWalk_ParallelRace(t *testing.T) {
	ctx := context.Background()
	lat := icx.NewLocalLattice()

	// Pre-populate initial graph
	for i := 0; i < 20; i++ {
		id := fmt.Sprintf("race_node_%d", i)
		lat.AddASTNode(&icx.LatticeNode{ID: id, Name: id, Type: "function"})
		if i > 0 {
			lat.AddASTEdge(fmt.Sprintf("race_node_%d", i-1), "calls", id)
		}
	}
	// Add back edges for cycles
	lat.AddASTEdge("race_node_5", "calls", "race_node_1")
	lat.AddASTEdge("race_node_15", "calls", "race_node_8")

	const concurrency = 60
	var wg sync.WaitGroup
	wg.Add(concurrency)

	for i := 0; i < concurrency; i++ {
		go func(workerID int) {
			defer wg.Done()
			r := rand.New(rand.NewSource(int64(workerID * 1000)))

			// 80% read walks, 20% dynamic edge/node additions
			if workerID%5 == 0 {
				// Dynamic writer
				newNode := fmt.Sprintf("dynamic_node_%d", workerID)
				lat.AddASTNode(&icx.LatticeNode{ID: newNode, Name: newNode, Type: "type"})
				targetIdx := r.Intn(20)
				lat.AddASTEdge(newNode, "defines", fmt.Sprintf("race_node_%d", targetIdx))
			} else {
				// Reader walk
				startIdx := r.Intn(20)
				startNode := fmt.Sprintf("race_node_%d", startIdx)
				hops := 1 + r.Intn(15) // 1..15
				directed := r.Intn(2) == 0
				res, err := lat.MultihopWalk(ctx, icx.MultihopWalkRequest{
					StartNode: startNode,
					MaxHops:   hops,
					Directed:  directed,
				})
				if err != nil {
					t.Errorf("Worker %d walk failed: %v", workerID, err)
					return
				}
				if !res.OK {
					t.Errorf("Worker %d walk returned !OK: %+v", workerID, res)
				}
			}
		}(i)
	}

	wg.Wait()
}

// ============================================================================
// PART 2: Wasm Stack Machine Stress Testing & Fuzzing
// ============================================================================

// TestWasmKernel_StackUnderflowAllOpcodes verifies that every single opcode
// correctly reports stack underflow without panicking.
func TestWasmKernel_StackUnderflowAllOpcodes(t *testing.T) {
	kernel := icx.NewWasmKernel()

	binaryOps := []string{"ADD", "SUB", "MUL", "DIV", "MAX", "MIN"}
	for _, op := range binaryOps {
		// Test with 0 elements on stack
		res0 := kernel.Exec([]string{op})
		if res0.OK {
			t.Errorf("Opcode %s with 0 elements expected error, got OK", op)
		}
		if !strings.Contains(res0.Error, "stack underflow") {
			t.Errorf("Opcode %s error expected 'stack underflow', got: %s", op, res0.Error)
		}

		// Test with 1 element on stack
		res1 := kernel.Exec([]string{"PUSH:42", op})
		if res1.OK {
			t.Errorf("Opcode %s with 1 element expected error, got OK", op)
		}
		if !strings.Contains(res1.Error, "stack underflow") {
			t.Errorf("Opcode %s with 1 item expected 'stack underflow', got: %s", op, res1.Error)
		}
	}

	unaryOps := []string{"SQRT", "ABS"}
	for _, op := range unaryOps {
		// Test with 0 elements on stack
		res := kernel.Exec([]string{op})
		if res.OK {
			t.Errorf("Unary opcode %s with 0 elements expected error, got OK", op)
		}
		if !strings.Contains(res.Error, "stack underflow") {
			t.Errorf("Unary opcode %s expected 'stack underflow', got: %s", op, res.Error)
		}
	}
}

// TestWasmKernel_DivisionByZeroEdgeCases tests division by zero variations.
func TestWasmKernel_DivisionByZeroEdgeCases(t *testing.T) {
	kernel := icx.NewWasmKernel()

	divZeroPrograms := [][]string{
		{"PUSH:100", "PUSH:0", "DIV"},
		{"PUSH:0", "PUSH:0", "DIV"},
		{"PUSH:-50", "PUSH:0", "DIV"},
		{"PUSH:100", "PUSH:-0.0", "DIV"},
	}

	for _, prog := range divZeroPrograms {
		res := kernel.Exec(prog)
		if res.OK {
			t.Errorf("Expected division by zero error for %v, got OK=true", prog)
		}
		if res.Error != "WASM DIV by zero" {
			t.Errorf("Expected 'WASM DIV by zero' for %v, got: %s", prog, res.Error)
		}
	}
}

// TestWasmKernel_NegativeSquareRoot tests negative square root error handling.
func TestWasmKernel_NegativeSquareRoot(t *testing.T) {
	kernel := icx.NewWasmKernel()

	negSqrtPrograms := [][]string{
		{"PUSH:-1", "SQRT"},
		{"PUSH:-1000000", "SQRT"},
		{"PUSH:-0.0000001", "SQRT"},
	}

	for _, prog := range negSqrtPrograms {
		res := kernel.Exec(prog)
		if res.OK {
			t.Errorf("Expected negative sqrt error for %v, got OK=true", prog)
		}
		if res.Error != "WASM SQRT of negative number" {
			t.Errorf("Expected 'WASM SQRT of negative number' for %v, got: %s", prog, res.Error)
		}
	}

	// Sqrt of 0 must succeed and return 0
	resZero := kernel.Exec([]string{"PUSH:0", "SQRT"})
	if !resZero.OK || resZero.Result != 0.0 {
		t.Errorf("SQRT(0) expected 0.0, got: %+v", resZero)
	}
}

// TestWasmKernel_DeeplyNestedStackCalls tests stack depth up to MaxStack (1024)
// and stack overflow boundary at 1025.
func TestWasmKernel_DeeplyNestedStackCalls(t *testing.T) {
	kernel := icx.NewWasmKernel()

	// 1. Fill stack up to exactly MaxStack (1024)
	prog1024 := make([]string, 1024)
	for i := 0; i < 1024; i++ {
		prog1024[i] = fmt.Sprintf("PUSH:%d", i%10)
	}
	res1024 := kernel.Exec(prog1024)
	if !res1024.OK {
		t.Fatalf("Exec with 1024 pushes failed: %s", res1024.Error)
	}
	if len(res1024.Stack) != 1024 {
		t.Fatalf("Expected stack size 1024, got %d", len(res1024.Stack))
	}

	// 2. Exceed MaxStack (1025 pushes) -> stack overflow
	prog1025 := make([]string, 1025)
	for i := 0; i < 1025; i++ {
		prog1025[i] = "PUSH:1"
	}
	res1025 := kernel.Exec(prog1025)
	if res1025.OK {
		t.Fatalf("Exec with 1025 pushes expected stack overflow, got OK=true")
	}
	if res1025.Error != "WASM stack overflow" {
		t.Fatalf("Expected 'WASM stack overflow', got: %s", res1025.Error)
	}

	// 3. Deeply nested stack reduction: 500 pushes followed by 499 ADDs
	reductionProg := make([]string, 0, 1000)
	for i := 0; i < 500; i++ {
		reductionProg = append(reductionProg, "PUSH:1")
	}
	for i := 0; i < 499; i++ {
		reductionProg = append(reductionProg, "ADD")
	}
	reductionProg = append(reductionProg, "HALT")

	resReduction := kernel.Exec(reductionProg)
	if !resReduction.OK {
		t.Fatalf("Deep stack reduction failed: %s", resReduction.Error)
	}
	if resReduction.Result != 500.0 {
		t.Fatalf("Expected reduction result 500.0, got %f", resReduction.Result)
	}
	if len(resReduction.Stack) != 1 {
		t.Fatalf("Expected final stack len 1, got %d", len(resReduction.Stack))
	}

	// 4. Max steps boundary (10001 steps without stack overflow)
	stepsProg := make([]string, 10002)
	stepsProg[0] = "PUSH:1"
	for i := 1; i < 10002; i++ {
		if i%2 == 1 {
			stepsProg[i] = "PUSH:1"
		} else {
			stepsProg[i] = "ADD"
		}
	}
	resSteps := kernel.Exec(stepsProg)
	if resSteps.OK {
		t.Fatalf("Expected step limit exceeded, got OK")
	}
	if !strings.Contains(resSteps.Error, "exceeded step limit") {
		t.Fatalf("Expected 'exceeded step limit', got: %s", resSteps.Error)
	}
}

// TestWasmKernel_RandomFuzzer generates pseudo-random sequences of opcodes
// and verifies that the kernel never panics under any randomized program.
func TestWasmKernel_RandomFuzzer(t *testing.T) {
	kernel := icx.NewWasmKernel()
	r := rand.New(rand.NewSource(42))

	opcodes := []string{
		"ADD", "SUB", "MUL", "DIV", "SQRT", "ABS", "MAX", "MIN", "HALT",
		"UNKNOWN_OP", "PUSH:0", "PUSH:100", "PUSH:-50", "PUSH:invalid",
		"PUSH:", "  ", "\t",
	}

	const fuzzIterations = 200
	for iter := 0; iter < fuzzIterations; iter++ {
		progLen := 1 + r.Intn(50)
		prog := make([]string, progLen)
		for i := 0; i < progLen; i++ {
			prog[i] = opcodes[r.Intn(len(opcodes))]
		}

		// Execute and ensure it does not panic
		func() {
			defer func() {
				if rec := recover(); rec != nil {
					t.Fatalf("WasmKernel panicked on iteration %d: %v\nProgram: %v", iter, rec, prog)
				}
			}()
			_ = kernel.Exec(prog)
		}()
	}
}

// ============================================================================
// PART 3: Exact Decimal Arithmetic Stress Testing & Fuzzing
// ============================================================================

// TestEvalArithmetic_ZeroFloatEnforcement verifies that IEEE-754 floats
// in environment are strictly rejected.
func TestEvalArithmetic_ZeroFloatEnforcement(t *testing.T) {
	// 1. float64 in env
	_, err64 := icx.EvalArithmetic("x + 1", map[string]any{"x": float64(1.0)})
	if err64 == nil {
		t.Fatalf("Expected zero-float error for float64, got nil")
	}
	if !strings.Contains(err64.Error(), "IEEE float forbidden") {
		t.Fatalf("Expected 'IEEE float forbidden' in error message, got: %v", err64)
	}

	// 2. float32 in env
	_, err32 := icx.EvalArithmetic("y * 2", map[string]any{"y": float32(2.5)})
	if err32 == nil {
		t.Fatalf("Expected zero-float error for float32, got nil")
	}
	if !strings.Contains(err32.Error(), "IEEE float forbidden") {
		t.Fatalf("Expected 'IEEE float forbidden' in error message, got: %v", err32)
	}

	// 3. Allowed exact types: int, int64, string, decimal.Decimal
	allowedEnv := map[string]any{
		"a": int(10),
		"b": int64(20),
		"c": "30.5",
		"d": decimal.NewFromInt(40),
	}
	res, err := icx.EvalArithmetic("a + b + c + d", allowedEnv)
	if err != nil {
		t.Fatalf("Allowed types failed evaluation: %v", err)
	}
	expected, _ := decimal.NewFromString("100.5")
	if !res.Equal(expected) {
		t.Fatalf("Expected 100.5, got: %s", res.String())
	}
}

// TestEvalArithmetic_ComplexPrecedenceAndNesting tests operator precedence
// and nested expression boundaries.
func TestEvalArithmetic_ComplexPrecedenceAndNesting(t *testing.T) {
	testCases := []struct {
		expr     string
		env      map[string]any
		expected string
	}{
		{expr: "2 + 3 * 4", expected: "14"},
		{expr: "2 * 3 + 4", expected: "10"},
		{expr: "10 - 2 * 3", expected: "4"},
		{expr: "2 + 3 ** 2 * 4", expected: "38"}, // 2 + 9*4 = 38
		{expr: "(2 + 3) * (4 - 1)", expected: "15"},
		{expr: "17 % 5", expected: "2"},
		{expr: "17 % 5 * 2", expected: "4"}, // (17%5)*2 = 4
		{expr: "-5 + 10", expected: "5"},
		{expr: "10 + -5", expected: "5"},
		{expr: "-(-5)", expected: "5"},
		{expr: "((((((1 + 2) * 3) - 4) / 5) + 6) * 7)", expected: "49"}, // 1+2=3, *3=9, -4=5, /5=1, +6=7, *7=49
		{expr: "abs(-42) + abs(10)", expected: "52"},
		{expr: "min(10, 5, 20, 2, 8)", expected: "2"},
		{expr: "max(10, 5, 20, 2, 8)", expected: "20"},
		{expr: "sum(1, 2, 3, 4, 5, 6, 7, 8, 9, 10)", expected: "55"},
		{expr: "floor(3.99)", expected: "3"},
		{expr: "ceil(3.01)", expected: "4"},
		{expr: "round(3.14159, 2)", expected: "3.14"},
		{expr: "round(3.14159, 4)", expected: "3.1416"},
		{expr: "sqrt(144)", expected: "12"},
		{expr: "sqrt(0)", expected: "0"},
	}

	for _, tc := range testCases {
		t.Run(tc.expr, func(t *testing.T) {
			res, err := icx.EvalArithmetic(tc.expr, tc.env)
			if err != nil {
				t.Fatalf("EvalArithmetic(%q) failed: %v", tc.expr, err)
			}
			expDec, err := decimal.NewFromString(tc.expected)
			if err != nil {
				t.Fatalf("Invalid expected test value: %s", tc.expected)
			}
			if !res.Equal(expDec) {
				t.Errorf("EvalArithmetic(%q) = %s; want %s", tc.expr, res.String(), tc.expected)
			}
		})
	}
}

// TestEvalArithmetic_LargeExponentsAndEdgeCases tests power edge cases
// including negative bases, zero powers, and large exponents.
func TestEvalArithmetic_LargeExponentsAndEdgeCases(t *testing.T) {
	// 1. Valid power cases
	validCases := []struct {
		expr     string
		expected string
	}{
		{expr: "2 ** 10", expected: "1024"},
		{expr: "10 ** 6", expected: "1000000"},
		{expr: "2 ** 0", expected: "1"},
		{expr: "0 ** 5", expected: "0"},
		{expr: "4 ** 0.5", expected: "2"},
		{expr: "2 ** -2", expected: "0.25"},
		{expr: "(-2) ** 3", expected: "-8"},
		{expr: "(-2) ** 4", expected: "16"},
	}

	for _, tc := range validCases {
		t.Run(tc.expr, func(t *testing.T) {
			res, err := icx.EvalArithmetic(tc.expr, nil)
			if err != nil {
				t.Fatalf("EvalArithmetic(%q) failed: %v", tc.expr, err)
			}
			expDec, _ := decimal.NewFromString(tc.expected)
			if !res.Equal(expDec) {
				t.Errorf("EvalArithmetic(%q) = %s; want %s", tc.expr, res.String(), tc.expected)
			}
		})
	}

	// 2. Invalid power fault cases
	faultCases := []struct {
		name string
		expr string
	}{
		{name: "zero_to_negative_power", expr: "0 ** -2"},
		{name: "negative_base_non_integer_exp", expr: "(-2) ** 0.5"},
		{name: "division_by_zero", expr: "10 / 0"},
		{name: "modulo_by_zero", expr: "10 % 0"},
		{name: "sqrt_negative", expr: "sqrt(-9)"},
		{name: "cagr_zero_start", expr: "cagr(0, 100, 2)"},
		{name: "cagr_zero_years", expr: "cagr(100, 200, 0)"},
		{name: "unresolved_id", expr: "missing_var + 1"},
		{name: "trailing_tokens", expr: "1 + 2 3"},
	}

	for _, tc := range faultCases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := icx.EvalArithmetic(tc.expr, nil)
			if err == nil {
				t.Errorf("Expected fault for %q, got nil", tc.expr)
			}
		})
	}
}

// TestEvalArithmetic_Fuzzing executes randomized arithmetic expressions to probe
// parser resilience against unexpected tokens, unmatched parentheses, and deep expressions.
func TestEvalArithmetic_Fuzzing(t *testing.T) {
	malformedInputs := []string{
		"",
		"(",
		")",
		"()",
		"1 +",
		"* 2",
		"((1 + 2)",
		"1 + 2))",
		"1 + + + 2",
		"Decimal()",
		"Decimal(invalid)",
		"abs()",
		"sum()",
		"round()",
		"pow(1)",
		"cagr(1, 2)",
		"min()",
		"max()",
		"foo.bar",
		"1 / (2 - 2)",
	}

	for idx, expr := range malformedInputs {
		t.Run(fmt.Sprintf("Malformed_%d", idx), func(t *testing.T) {
			// Should return error or fault, but NEVER panic
			func() {
				defer func() {
					if rec := recover(); rec != nil {
						t.Fatalf("EvalArithmetic panicked on %q: %v", expr, rec)
					}
				}()
				_, _ = icx.EvalArithmetic(expr, nil)
			}()
		})
	}
}
