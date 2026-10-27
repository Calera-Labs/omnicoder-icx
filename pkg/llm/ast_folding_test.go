// SPDX-License-Identifier: Apache-2.0
package llm_test

import (
	"strings"
	"testing"

	"github.com/caleralabs/omnicoder-icx/pkg/llm"
)

func TestASTFolding_PythonBlueprintRegistration(t *testing.T) {
	rawSource := `class Blueprint(Scaffold):
    def __init__(self, name, import_name, static_folder=None):
        self.name = name
        self.import_name = import_name
        self.static_folder = static_folder

    def make_setup_state(self, app, options, first_registration=False):
        return BlueprintSetupState(self, app, options, first_registration)

    def register(self, app: "Flask", options: dict) -> None:
        if self.name in app.blueprints:
>           assert app.blueprints[self.name] is self, "Conflict"
            # BUG: Multiple registrations with same name overwrite without warning
        app.blueprints[self.name] = self
`

	folded := llm.FoldUnreferencedAST(rawSource, -1, []string{"register"}, ".py")

	// 1. Verify unreferenced functions before register were folded
	if !strings.Contains(folded, "lines folded]") {
		t.Fatalf("expected folded output to contain '[N lines folded]', got:\n%s", folded)
	}

	// 2. Verify register method is preserved in full
	if !strings.Contains(folded, "def register(self, app: \"Flask\", options: dict) -> None:") {
		t.Errorf("expected register signature to be preserved, got:\n%s", folded)
	}
	if !strings.Contains(folded, ">           assert app.blueprints[self.name] is self") {
		t.Errorf("expected focus line with '>' to be preserved, got:\n%s", folded)
	}
	if !strings.Contains(folded, "class Blueprint(Scaffold):") {
		t.Errorf("expected enclosing class declaration to be preserved, got:\n%s", folded)
	}

	// 3. Verify token count reduction
	origTokens := llm.EstimateTokens(rawSource)
	foldedTokens := llm.EstimateTokens(folded)
	if foldedTokens >= origTokens {
		t.Errorf("expected folded tokens (%d) < original tokens (%d)", foldedTokens, origTokens)
	}
}

func TestASTFolding_GoFunctionBodyFolding(t *testing.T) {
	rawSource := `package service

func UnrelatedHelperOne(a, b int) int {
	res := a * 2
	res += b
	return res
}

func UnrelatedHelperTwo(msg string) error {
	if len(msg) == 0 {
		return errors.New("empty")
	}
	return nil
}

func TargetHandler(w http.ResponseWriter, r *http.Request) {
>	assert r.Method == "POST"
	w.WriteHeader(200)
}
`

	folded := llm.FoldUnreferencedAST(rawSource, -1, []string{"TargetHandler"}, ".go")

	if !strings.Contains(folded, "lines folded]") {
		t.Fatalf("expected Go unreferenced helper bodies to be folded, got:\n%s", folded)
	}

	if !strings.Contains(folded, "func TargetHandler(w http.ResponseWriter, r *http.Request)") {
		t.Errorf("expected TargetHandler signature preserved, got:\n%s", folded)
	}
	if !strings.Contains(folded, ">	assert r.Method == \"POST\"") {
		t.Errorf("expected focus line with '>' preserved, got:\n%s", folded)
	}
}

func TestASTFolding_TypeScriptBodyFolding(t *testing.T) {
	rawSource := `export class Router {
    unrelatedMethod(path: string): boolean {
        const normalized = path.trim().toLowerCase();
        return normalized.startsWith("/api");
    }

    handleRoute(req: Request): Response {
>       assert(req.url !== null);
        return new Response("OK");
    }
}
`

	folded := llm.FoldUnreferencedAST(rawSource, -1, []string{"handleRoute"}, ".ts")

	if !strings.Contains(folded, "lines folded]") {
		t.Fatalf("expected TypeScript unreferenced method body to be folded, got:\n%s", folded)
	}
	if !strings.Contains(folded, "handleRoute(req: Request): Response") {
		t.Errorf("expected handleRoute preserved, got:\n%s", folded)
	}
}

func TestASTFolding_GateEditCard_BudgetDiscipline(t *testing.T) {
	// Simulate a massive 150-line Python file containing many methods
	var sb strings.Builder
	sb.WriteString("class LargeController:\n")
	for i := 1; i <= 10; i++ {
		sb.WriteString(strings.Repeat(" ", 4) + "def helper_method_" + string(rune('0'+i)) + "(self, x):\n")
		sb.WriteString(strings.Repeat(" ", 8) + "val = x * 2\n")
		sb.WriteString(strings.Repeat(" ", 8) + "data = [val, val + 1, val + 2]\n")
		sb.WriteString(strings.Repeat(" ", 8) + "return sum(data)\n\n")
	}
	sb.WriteString("    def target_method(self, request: dict) -> dict:\n")
	sb.WriteString("        if not request:\n")
	sb.WriteString(">           assert 'auth' in request, 'Missing auth token'\n")
	sb.WriteString("        return {'status': 'ok'}\n")

	massiveSnippet := sb.String()

	card := llm.GateEditCard{
		FilePath:       "src/controllers/large.py",
		Instruction:    "Modify target_method to accept Bearer tokens without assertion error.",
		ContextSnippet: massiveSnippet,
	}

	sysPrompt, userPrompt, err := card.FormatPrompt()
	if err != nil {
		t.Fatalf("FormatPrompt failed with error: %v", err)
	}

	// 1. Strict Token Budget Invariant: Total tokens < MaxCardTokenBudget (300)
	totalTokens := llm.EstimateTokens(sysPrompt + " " + userPrompt)
	if totalTokens >= llm.MaxCardTokenBudget {
		t.Errorf("violation: total card tokens (%d) >= MaxCardTokenBudget (%d)", totalTokens, llm.MaxCardTokenBudget)
	}

	// 2. Validate with ValidateTokenBudget
	if err := llm.ValidateTokenBudget(sysPrompt, userPrompt, llm.MaxCardTokenBudget); err != nil {
		t.Errorf("ValidateTokenBudget failed: %v", err)
	}

	// 3. Verify user prompt contains AST folding indicators
	if !strings.Contains(userPrompt, "lines folded]") && !strings.Contains(userPrompt, "target_method") {
		t.Errorf("expected user prompt to contain folded context or target_method, got:\n%s", userPrompt)
	}
}

func TestASTFolding_GateEditCard_InstructionKeywordFocus(t *testing.T) {
	rawSource := `class Blueprint(Scaffold):
    def initialize(self, name):
        self.name = name
        self.ready = True
        return self

    def register(self, app, options):
        if self.name in app.blueprints:
            assert app.blueprints[self.name] is self
        app.blueprints[self.name] = self
`

	// No '>' marker in snippet, but instruction explicitly names "register"
	card := llm.GateEditCard{
		FilePath:       "flask/blueprints.py",
		Instruction:    "Modify register() to allow nested prefixes without identity conflict.",
		ContextSnippet: rawSource,
	}

	sysPrompt, userPrompt, err := card.FormatPrompt()
	if err != nil {
		t.Fatalf("FormatPrompt failed: %v", err)
	}

	totalTokens := llm.EstimateTokens(sysPrompt + " " + userPrompt)
	if totalTokens >= llm.MaxCardTokenBudget {
		t.Errorf("card tokens (%d) >= %d", totalTokens, llm.MaxCardTokenBudget)
	}

	if !strings.Contains(userPrompt, "register") {
		t.Errorf("expected user prompt to preserve register method, got:\n%s", userPrompt)
	}
}
