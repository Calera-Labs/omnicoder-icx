// SPDX-License-Identifier: Apache-2.0

package devops_test

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/caleralabs/omnicoder-icx/pkg/devops"
)

func TestSynthesizeRepoMap_PureGoAST(t *testing.T) {
	tmpDir := t.TempDir()

	goSource := `package service

import "context"

const DefaultPort = 8080
var GlobalReady = true

type Config struct {
	Port int
	Host string
}

type Runner interface {
	Run(ctx context.Context) error
}

type StringAlias string

func NewConfig(host string, port int) *Config {
	return &Config{Host: host, Port: port}
}

func (c *Config) Addr() string {
	return c.Host
}
`
	if err := os.WriteFile(filepath.Join(tmpDir, "service.go"), []byte(goSource), 0644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}

	res, err := devops.SynthesizeRepoMap(devops.RepoMapRequest{
		WorkspaceDir: tmpDir,
		TokenBudget:  1024,
	})
	if err != nil {
		t.Fatalf("SynthesizeRepoMap failed: %v", err)
	}

	if res.FilesScanned != 1 {
		t.Errorf("expected 1 file scanned, got %d", res.FilesScanned)
	}
	if res.SymbolsFound < 6 {
		t.Errorf("expected at least 6 symbols, got %d", res.SymbolsFound)
	}

	// Verify symbol kinds
	kindsFound := make(map[string]bool)
	for _, s := range res.TopSymbols {
		kindsFound[s.Kind] = true
	}
	for _, expectedKind := range []string{"struct", "interface", "func", "method", "const", "var"} {
		if !kindsFound[expectedKind] {
			t.Errorf("missing expected symbol kind: %s", expectedKind)
		}
	}

	// Verify tree outline formatting
	if !strings.Contains(res.RepoMap, "service.go:") {
		t.Errorf("repo map outline missing file header: %s", res.RepoMap)
	}
	if !strings.Contains(res.RepoMap, "│ type Config struct") {
		t.Errorf("repo map outline missing Config struct: %s", res.RepoMap)
	}
	if !strings.Contains(res.RepoMap, "│ type Runner interface") {
		t.Errorf("repo map outline missing Runner interface: %s", res.RepoMap)
	}
}

func TestSynthesizeRepoMap_PolyglotExtraction(t *testing.T) {
	tmpDir := t.TempDir()

	tsSource := `
export interface UserSession {
    token: string;
}
export class AuthManager {
    login() {}
}
export type TokenMap = Record<string, string>;
export function verifyToken(t: string): boolean { return true; }
export const hashPassword = (p: string) => { return p; };
`
	pySource := `
class DataEngine(BaseEngine):
    def connect(self):
        pass

async def fetch_records(query: str) -> list:
    return []
`
	rsSource := `
pub struct MemoryPool;
pub enum Priority { Low, High }
pub trait Allocator {
    fn alloc(&self);
}
pub async fn init_pool(size: usize) -> Result<(), Error> {
    Ok(())
}
`
	if err := os.WriteFile(filepath.Join(tmpDir, "auth.ts"), []byte(tsSource), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmpDir, "engine.py"), []byte(pySource), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmpDir, "pool.rs"), []byte(rsSource), 0644); err != nil {
		t.Fatal(err)
	}

	res, err := devops.SynthesizeRepoMap(devops.RepoMapRequest{
		WorkspaceDir: tmpDir,
		TokenBudget:  2048,
	})
	if err != nil {
		t.Fatalf("SynthesizeRepoMap polyglot failed: %v", err)
	}

	if res.FilesScanned != 3 {
		t.Errorf("expected 3 files scanned, got %d", res.FilesScanned)
	}

	symbols := make(map[string]string)
	for _, s := range res.TopSymbols {
		symbols[s.Name] = s.Kind
	}

	// Verify TS
	if symbols["UserSession"] != "interface" {
		t.Errorf("expected UserSession interface, got %v", symbols["UserSession"])
	}
	if symbols["AuthManager"] != "class" {
		t.Errorf("expected AuthManager class, got %v", symbols["AuthManager"])
	}

	// Verify Python
	if symbols["DataEngine"] != "class" {
		t.Errorf("expected DataEngine class, got %v", symbols["DataEngine"])
	}
	if symbols["fetch_records"] != "func" {
		t.Errorf("expected fetch_records func, got %v", symbols["fetch_records"])
	}

	// Verify Rust
	if symbols["MemoryPool"] != "struct" {
		t.Errorf("expected MemoryPool struct, got %v", symbols["MemoryPool"])
	}
	if symbols["Priority"] != "enum" {
		t.Errorf("expected Priority enum, got %v", symbols["Priority"])
	}
	if symbols["Allocator"] != "trait" {
		t.Errorf("expected Allocator trait, got %v", symbols["Allocator"])
	}
}

func TestSynthesizeRepoMap_PageRankConvergence(t *testing.T) {
	tmpDir := t.TempDir()

	// Circular bipartite reference: fileA uses BFunc, fileB uses CFunc, fileC uses AFunc
	fileA := `package main
type AFunc struct{}
func UseB() {
    CallBFunc()
}
`
	fileB := `package main
func CallBFunc() {}
func UseC() {
    CallCFunc()
}
`
	fileC := `package main
func CallCFunc() {}
func UseA() {
    _ = AFunc{}
}
`
	_ = os.WriteFile(filepath.Join(tmpDir, "a.go"), []byte(fileA), 0644)
	_ = os.WriteFile(filepath.Join(tmpDir, "b.go"), []byte(fileB), 0644)
	_ = os.WriteFile(filepath.Join(tmpDir, "c.go"), []byte(fileC), 0644)

	res, err := devops.SynthesizeRepoMap(devops.RepoMapRequest{
		WorkspaceDir: tmpDir,
		TokenBudget:  1024,
	})
	if err != nil {
		t.Fatalf("failed: %v", err)
	}

	if len(res.TopSymbols) == 0 {
		t.Fatalf("expected symbols found in circular graph")
	}

	for _, s := range res.TopSymbols {
		if math.IsNaN(s.Score) || math.IsInf(s.Score, 0) || s.Score <= 0 {
			t.Errorf("invalid PageRank score for symbol %s: %f", s.Name, s.Score)
		}
	}
}

func TestSynthesizeRepoMap_PersonalizationBiasing(t *testing.T) {
	tmpDir := t.TempDir()

	file1 := `package main
type Alpha struct{}
`
	file2 := `package main
type Beta struct{}
`
	file3 := `package auth
type AuthHandler struct{}
`
	_ = os.WriteFile(filepath.Join(tmpDir, "alpha.go"), []byte(file1), 0644)
	_ = os.WriteFile(filepath.Join(tmpDir, "beta.go"), []byte(file2), 0644)
	_ = os.WriteFile(filepath.Join(tmpDir, "auth.go"), []byte(file3), 0644)

	// 1. Focus file biasing
	resFocus, err := devops.SynthesizeRepoMap(devops.RepoMapRequest{
		WorkspaceDir: tmpDir,
		FocusFiles:   []string{"beta.go"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(resFocus.TopSymbols) == 0 || resFocus.TopSymbols[0].Name != "Beta" {
		t.Errorf("expected Beta to rank top with focus bias, got %v", resFocus.TopSymbols[0].Name)
	}

	// 2. Query biasing
	resQuery, err := devops.SynthesizeRepoMap(devops.RepoMapRequest{
		WorkspaceDir: tmpDir,
		Query:        "auth",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(resQuery.TopSymbols) == 0 || resQuery.TopSymbols[0].Name != "AuthHandler" {
		t.Errorf("expected AuthHandler to rank top with query bias, got %v", resQuery.TopSymbols[0].Name)
	}
}

func TestSynthesizeRepoMap_TokenBudgetClamping(t *testing.T) {
	tmpDir := t.TempDir()

	// Generate 30 files with symbols
	for i := 0; i < 30; i++ {
		content := fmt.Sprintf(`package mod%d
type Struct%d struct {
    Field%d string
}
func Function%d(param%d int) string {
    return "ok"
}
`, i, i, i, i, i)
		_ = os.WriteFile(filepath.Join(tmpDir, fmt.Sprintf("file_%02d.go", i)), []byte(content), 0644)
	}

	// Budget 500
	res500, err := devops.SynthesizeRepoMap(devops.RepoMapRequest{
		WorkspaceDir: tmpDir,
		TokenBudget:  500,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res500.TokenCount > 500 {
		t.Errorf("budget 500 exceeded: got %d tokens", res500.TokenCount)
	}

	// Budget 1024
	res1024, err := devops.SynthesizeRepoMap(devops.RepoMapRequest{
		WorkspaceDir: tmpDir,
		TokenBudget:  1024,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res1024.TokenCount > 1024 {
		t.Errorf("budget 1024 exceeded: got %d tokens", res1024.TokenCount)
	}

	// Budget tiny (10)
	res10, err := devops.SynthesizeRepoMap(devops.RepoMapRequest{
		WorkspaceDir: tmpDir,
		TokenBudget:  10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res10.RepoMap == "" {
		t.Errorf("expected non-empty clamped repo map for small budget")
	}
}

func TestSynthesizeRepoMap_EmptyAndIgnoredDirectories(t *testing.T) {
	tmpDir := t.TempDir()

	_ = os.MkdirAll(filepath.Join(tmpDir, ".git"), 0755)
	_ = os.WriteFile(filepath.Join(tmpDir, ".git", "hidden.go"), []byte("package git\ntype GitHidden struct{}"), 0644)

	_ = os.MkdirAll(filepath.Join(tmpDir, "node_modules"), 0755)
	_ = os.WriteFile(filepath.Join(tmpDir, "node_modules", "package.ts"), []byte("export class NodeModule {}"), 0644)

	res, err := devops.SynthesizeRepoMap(devops.RepoMapRequest{
		WorkspaceDir: tmpDir,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.FilesScanned != 0 {
		t.Errorf("ignored dirs should not be scanned, got %d files", res.FilesScanned)
	}
	if !strings.Contains(res.RepoMap, "No source files discovered") {
		t.Errorf("expected empty message, got %s", res.RepoMap)
	}
}

func TestExtractFileSymbolsAndRefs_GoSyntaxErrorFallback(t *testing.T) {
	tmpDir := t.TempDir()

	// WIP Go file with syntax error but recognizable declarations
	wipGo := `package incomplete

type Partial struct {
// missing closing brace or broken syntax
func BrokenMethod(
`
	_ = os.WriteFile(filepath.Join(tmpDir, "wip.go"), []byte(wipGo), 0644)

	res, err := devops.SynthesizeRepoMap(devops.RepoMapRequest{
		WorkspaceDir: tmpDir,
	})
	if err != nil {
		t.Fatalf("parser should handle syntax errors gracefully, got: %v", err)
	}
	if res.FilesScanned != 1 {
		t.Errorf("expected 1 file scanned, got %d", res.FilesScanned)
	}
}

func TestRepoMap_ConcurrencyRace(t *testing.T) {
	tmpDir := t.TempDir()
	_ = os.WriteFile(filepath.Join(tmpDir, "core.go"), []byte("package core\ntype Engine struct{}\nfunc Run() {}"), 0644)

	const goroutines = 10
	var wg sync.WaitGroup
	wg.Add(goroutines)

	for i := 0; i < goroutines; i++ {
		go func(idx int) {
			defer wg.Done()
			_, err := devops.SynthesizeRepoMap(devops.RepoMapRequest{
				WorkspaceDir: tmpDir,
				TokenBudget:  512,
			})
			if err != nil {
				t.Errorf("goroutine %d failed: %v", idx, err)
			}
		}(i)
	}

	wg.Wait()
}

func TestSynthesizeRepoMap_QueryFirstPacking(t *testing.T) {
	tmpDir := t.TempDir()

	// Create many root package files that would normally dominate PageRank
	for i := 1; i <= 15; i++ {
		dir := filepath.Join(tmpDir, "pkg", fmt.Sprintf("mod%d", i))
		_ = os.MkdirAll(dir, 0755)
		content := fmt.Sprintf("package mod%d\ntype Service%d struct{}\nfunc Action%d() {}\n", i, i, i)
		_ = os.WriteFile(filepath.Join(dir, "service.go"), []byte(content), 0644)
	}

	// Create subproject file
	projDir := filepath.Join(tmpDir, "projects", "taskservice")
	_ = os.MkdirAll(projDir, 0755)
	projContent := "package main\ntype TaskRecord struct{}\nfunc HandleTasks() {}\n"
	_ = os.WriteFile(filepath.Join(projDir, "server.go"), []byte(projContent), 0644)

	// Test 1: Query by path segment "taskservice" with tight token budget (300 tokens)
	res, err := devops.SynthesizeRepoMap(devops.RepoMapRequest{
		WorkspaceDir: tmpDir,
		TokenBudget:  300,
		Query:        "taskservice",
	})
	if err != nil {
		t.Fatalf("SynthesizeRepoMap failed: %v", err)
	}
	if !strings.HasPrefix(strings.ReplaceAll(res.RepoMap, "\\", "/"), "projects/taskservice/server.go:") {
		t.Errorf("expected projects/taskservice/server.go to be packed first in outline, got:\n%s", res.RepoMap)
	}

	// Test 2: Query by symbol "TaskRecord" with tight token budget
	resSym, err := devops.SynthesizeRepoMap(devops.RepoMapRequest{
		WorkspaceDir: tmpDir,
		TokenBudget:  300,
		Query:        "TaskRecord",
	})
	if err != nil {
		t.Fatalf("SynthesizeRepoMap failed: %v", err)
	}
	if !strings.HasPrefix(strings.ReplaceAll(resSym.RepoMap, "\\", "/"), "projects/taskservice/server.go:") {
		t.Errorf("expected projects/taskservice/server.go to be packed first when querying symbol 'TaskRecord', got:\n%s", resSym.RepoMap)
	}
}

