// SPDX-License-Identifier: Apache-2.0

package devops_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/caleralabs/omnicoder-icx/pkg/devops"
)

func TestExtractCandidateTokens_CLIRunnerAndAssertions(t *testing.T) {
	failureMsg := `AssertionError: assert 'Subdomain' in 'Endpoint  Methods  Rule\n'`
	contextLines := []string{
		`    app = Flask(__name__, static_folder=None)`,
		`    app.add_url_rule("/a", subdomain="a", endpoint="a")`,
		`    cli = FlaskGroup(create_app=lambda: app)`,
		`>   result = runner.invoke(cli, ["routes"])`,
		`    assert result.exit_code == 0`,
		`>   assert "Subdomain" in result.output`,
		`E   AssertionError: assert 'Subdomain' in ...`,
	}
	cmdToRun := `pytest tests/test_cli.py::test_subdomain`

	tokens := devops.ExtractCandidateTokens(failureMsg, contextLines, cmdToRun)

	tokenMap := make(map[string]bool)
	for _, tok := range tokens {
		tokenMap[strings.ToLower(tok)] = true
	}

	// Verify key tokens were extracted
	if !tokenMap["routes"] {
		t.Errorf("expected 'routes' in candidate tokens, got: %v", tokens)
	}
	if !tokenMap["subdomain"] {
		t.Errorf("expected 'subdomain' in candidate tokens, got: %v", tokens)
	}

	// Verify stopwords were filtered out
	for _, bad := range []string{"assert", "runner", "invoke", "result", "output", "exit_code", "pytest"} {
		if tokenMap[bad] {
			t.Errorf("stopword %q should have been filtered out, got: %v", bad, tokens)
		}
	}
}

func TestSearchCodebase_ClickCommandAndConvention(t *testing.T) {
	tmpDir := t.TempDir()

	srcDir := filepath.Join(tmpDir, "src", "flask")
	if err := os.MkdirAll(srcDir, 0755); err != nil {
		t.Fatalf("failed to create src dir: %v", err)
	}

	cliCode := `package main
import click

@click.command("routes", short_help="Show the routes for the app.")
@click.option("--sort", "-s", default="endpoint")
@click.option("--all-methods", is_flag=True)
def routes_command(sort: str, all_methods: bool) -> None:
    """Show all registered routes with endpoints and methods.
    Very long docstring that spans multiple lines and explains
    everything in great detail to test docstring skipping.
    """
    rules = list(current_app.url_map.iter_rules())
    headers = ("Endpoint", "Methods", "Rule")
    click.echo(headers)
`
	if err := os.WriteFile(filepath.Join(srcDir, "cli.py"), []byte(cliCode), 0644); err != nil {
		t.Fatalf("failed to write cli.py: %v", err)
	}

	results, err := devops.SearchCodebase(devops.CodeSearchOptions{
		WorkspaceDir: tmpDir,
		QueryTokens:  []string{"routes", "subdomain"},
		HintFiles:    []string{"cli.py"},
		ExcludeTests: true,
	})
	if err != nil {
		t.Fatalf("SearchCodebase failed: %v", err)
	}

	if len(results) == 0 {
		t.Fatalf("expected search results for 'routes', got none")
	}

	top := results[0]
	if !strings.HasSuffix(top.File, "cli.py") {
		t.Errorf("expected top result to be in cli.py, got: %s", top.File)
	}
	if top.Score < 100 {
		t.Errorf("expected high score (>100) for decorator match, got %d", top.Score)
	}

	// Verify context lines include decorators and function definition
	joinedContext := strings.Join(top.ContextLines, "\n")
	if !strings.Contains(joinedContext, `click.command("routes"`) {
		t.Errorf("context lines missing command decorator: %s", joinedContext)
	}
	if !strings.Contains(joinedContext, "def routes_command") {
		t.Errorf("context lines missing function def: %s", joinedContext)
	}
	// Verify docstring was skipped to save token budget
	if strings.Contains(joinedContext, "Very long docstring that spans multiple lines") {
		t.Errorf("context lines should have skipped verbose docstring: %s", joinedContext)
	}
}

func TestSearchCodebase_PolyglotDefinitions(t *testing.T) {
	tmpDir := t.TempDir()

	srcDir := filepath.Join(tmpDir, "pkg", "router")
	if err := os.MkdirAll(srcDir, 0755); err != nil {
		t.Fatalf("failed to create pkg dir: %v", err)
	}

	goCode := `package router

type RouteManager struct {
    routes []string
}

func (rm *RouteManager) RegisterRoute(path string) {
    rm.routes = append(rm.routes, path)
}
`
	if err := os.WriteFile(filepath.Join(srcDir, "router.go"), []byte(goCode), 0644); err != nil {
		t.Fatalf("failed to write router.go: %v", err)
	}

	results, err := devops.SearchCodebase(devops.CodeSearchOptions{
		WorkspaceDir: tmpDir,
		QueryTokens:  []string{"RouteManager", "RegisterRoute"},
		ExcludeTests: true,
	})
	if err != nil {
		t.Fatalf("SearchCodebase failed: %v", err)
	}

	if len(results) < 2 {
		t.Fatalf("expected at least 2 results (struct and method), got %d", len(results))
	}

	kinds := make(map[string]bool)
	for _, r := range results {
		kinds[r.MatchKind] = true
	}
	if !kinds["class_def"] && !kinds["function_def"] {
		t.Errorf("expected class_def or function_def in results, got: %v", kinds)
	}
}

func TestExtractFunctionBlockWithHints_RoutesCommand(t *testing.T) {
	raw := `import click

@click.command("routes")
def routes_command(sort: str, all_methods: bool) -> None:
    """Show all registered routes with endpoints and methods."""
    rules = list(current_app.url_map.iter_rules())
    if not rules:
        click.echo("No routes were registered.")
        return
    headers = ("Endpoint", "Methods", "Rule")
    for rule in rules:
        click.echo(f"{rule.endpoint} {rule.methods} {rule.rule}")
`
	lines := strings.Split(raw, "\n")
	hints := []string{"routes", "Subdomain", "cli"}
	block := devops.ExtractFunctionBlockWithHints(lines, 4, hints)
	joined := strings.Join(block, "\n")
	if !strings.Contains(joined, "def routes_command") {
		t.Errorf("expected block to contain 'def routes_command', got:\n%s", joined)
	}
}

func TestExtractFunctionBlockWithHints_ImportPath(t *testing.T) {
	raw := `import importlib

def import_path(p, mode="prepend", root=None):
    """Import a file path as a Python module."""
    line_a = 1
    line_b = 2
    line_c = 3
    line_d = 4
    line_e = 5
    line_f = 6
    line_g = 7
    line_h = 8
    line_i = 9
    line_j = 10
    line_k = 11
    line_l = 12
    line_m = 13
    line_n = 14
    line_o = 15
    line_p = 16
    line_q = 17
    line_r = 18
    line_s = 19
    line_t = 20
    if mode == "importlib":
        module_name = module_name_from_path(p, root)
        return importlib.import_module(module_name)
    return None
`
	lines := strings.Split(raw, "\n")
	hints := []string{"importlib", "import_path", "module_name"}
	block := devops.ExtractFunctionBlockWithHints(lines, 3, hints)
	joined := strings.Join(block, "\n")
	if !strings.Contains(joined, `if mode == "importlib":`) {
		t.Errorf("expected block to contain 'if mode == \"importlib\":', got:\n%s", joined)
	}
}

func TestSearchCodebase_GoReceiverAndConstFallback(t *testing.T) {
	tmpDir := t.TempDir()

	code := `package observability

import "net/http"

const ReqTotal = "service_http_requests_total"

var inflight int64

type Collector struct{}

func (c *Collector) Reset() {
	inflight = 0
}

func (c *Collector) Middleware(next http.Handler) http.Handler {
	return next
}
`
	if err := os.WriteFile(filepath.Join(tmpDir, "metrics.go"), []byte(code), 0644); err != nil {
		t.Fatalf("failed to write metrics.go: %v", err)
	}

	// 1. Test searching for const ReqTotal
	resConst, err := devops.SearchCodebase(devops.CodeSearchOptions{
		WorkspaceDir: tmpDir,
		RawQuery:     "ReqTotal",
		QueryTokens:  []string{"ReqTotal"},
	})
	if err != nil {
		t.Fatalf("SearchCodebase failed: %v", err)
	}
	if len(resConst) == 0 {
		t.Fatalf("expected result for 'ReqTotal', got 0")
	}
	if resConst[0].MatchKind != "const_var_def" && resConst[0].MatchKind != "text_match" {
		t.Errorf("expected const_var_def or text_match, got: %s", resConst[0].MatchKind)
	}

	// 2. Test searching for receiver method signature
	resMethod, err := devops.SearchCodebase(devops.CodeSearchOptions{
		WorkspaceDir: tmpDir,
		RawQuery:     "func (c *Collector) Reset",
		QueryTokens:  []string{"func", "(c", "*Collector)", "Reset"},
	})
	if err != nil {
		t.Fatalf("SearchCodebase failed: %v", err)
	}
	if len(resMethod) == 0 {
		t.Fatalf("expected result for 'func (c *Collector) Reset', got 0")
	}
	if resMethod[0].Line != 11 {
		t.Errorf("expected line 11 for Reset, got line %d", resMethod[0].Line)
	}

	// 3. Test searching for var inflight
	resVar, err := devops.SearchCodebase(devops.CodeSearchOptions{
		WorkspaceDir: tmpDir,
		RawQuery:     "inflight",
		QueryTokens:  []string{"inflight"},
	})
	if err != nil {
		t.Fatalf("SearchCodebase failed: %v", err)
	}
	if len(resVar) == 0 {
		t.Fatalf("expected result for 'inflight', got 0")
	}
}

