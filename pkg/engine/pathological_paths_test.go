// SPDX-License-Identifier: Apache-2.0
package engine

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/caleralabs/omnicoder-icx/pkg/icx"
	"github.com/caleralabs/omnicoder-icx/pkg/llm"
)

func TestEngine_PathologicalPaths_FormatASTRibbon(t *testing.T) {
	testPaths := []struct {
		name string
		path string
	}{
		{"Empty string", ""},
		{"Whitespace only", "   \t\n  "},
		{"file_ prefix only", "file_"},
		{"file_ with whitespace", "file_   "},
		{"Dot only", "."},
		{"Slash only", "/"},
		{"Backslash only", "\\"},
		{"Double dot only", ".."},
		{"Multiple dots", "......."},
		{"Multiple slashes", "////"},
		{"Trailing slash", "foo/bar/"},
		{"Trailing dot slash", "foo/bar/."},
		{"Trailing dot dot slash", "foo/bar/.."},
		{"Relative path traversal", "../../../../../../../../etc/passwd"},
		{"Windows drive path", "C:\\Workspace\\Project\\deep\\nested\\file.go"},
		{"1000+ char repetitive path", strings.Repeat("sub_dir_name/", 100) + "main.go"},
		{"1000+ char single token without slash", strings.Repeat("LongSymbolWithoutAnySlash", 50)},
		{"5000+ char massive path", strings.Repeat("a/", 2500) + "end.go"},
		{"Unicode Cyrillic path", "директория/поддиректория/файл_компонента.go"},
		{"Unicode CJK path", "项目/核心模块/分布式存储/事务管理器.go"},
		{"Unicode Arabic RTL path", "مسار/مجلد/ملف_المشروع.go"},
		{"Unicode Emoji rich path", "🚀/⚡/🔥/💻/microservice_📦.go"},
		{"Unicode multi-byte boundary cut", strings.Repeat("a", 35) + "日本語.go"},
		{"Unicode 4-byte emoji boundary cut", strings.Repeat("b", 35) + "🚀🚀🚀.go"},
		{"Mixed dots and slashes", "./././..//./../..././foo.go"},
		{"Spaces inside path segments", "My Workspace/Sample Project/src/app.go"},
	}

	steps := []icx.WalkStep{
		{Depth: 0, Name: "root_file.go", Type: "file", Relation: "root"},
		{Depth: 1, Name: "HandlerMethod", Type: "function", Relation: "defines"},
		{Depth: 2, Name: "WorkerService", Type: "type", Relation: "calls"},
		{Depth: 3, Name: "DataStore", Type: "interface", Relation: "implements"},
	}

	for _, tc := range testPaths {
		t.Run(tc.name, func(t *testing.T) {
			candidates := []string{tc.path}
			ribbon := FormatASTRibbon(candidates, steps, 155)
			tokens := llm.EstimateTokens(ribbon)

			if tokens >= 160 {
				t.Fatalf("VIOLATION: FormatASTRibbon produced %d tokens >= 160 cap for %q:\n%s", tokens, tc.path, ribbon)
			}
			if !strings.HasPrefix(ribbon, "<nav:ribbon:") {
				t.Errorf("expected <nav:ribbon: prefix, got: %s", ribbon)
			}
			if !strings.HasSuffix(ribbon, "/>") {
				t.Errorf("ribbon does not close with '/>' for %q: %s", tc.path, ribbon)
			}

			// Empty steps test
			ribbonEmpty := FormatASTRibbon(candidates, nil, 155)
			tokensEmpty := llm.EstimateTokens(ribbonEmpty)
			if tokensEmpty >= 160 {
				t.Fatalf("VIOLATION: empty steps ribbon produced %d tokens >= 160 cap for %q:\n%s", tokensEmpty, tc.path, ribbonEmpty)
			}
		})
	}
}

func TestEngine_Stage2Pipeline_PathologicalFocusFiles(t *testing.T) {
	ctx := context.Background()
	lat := icx.NewLocalMemoryCache("pathological_pipeline_space")

	lat.AddASTNode(&icx.LatticeNode{
		ID:        "file_pathological.go",
		Name:      "pathological.go",
		Type:      "file",
		Dimension: "3D_VOLUMETRIC",
	})
	lat.AddASTNode(&icx.LatticeNode{
		ID:        "func_exec",
		Name:      "ExecuteMethod",
		Type:      "function",
		Dimension: "3D_VOLUMETRIC",
	})
	lat.AddASTEdge("file_pathological.go", "defines", "func_exec")

	orch, err := NewOrchestrator(EngineConfig{
		WorkspaceDir: t.TempDir(),
		ICXClient:    lat,
		DecisionGate: llm.NewMockDecisionGate(),
	})
	if err != nil {
		t.Fatalf("NewOrchestrator failed: %v", err)
	}

	pipeline := &TurnPipeline{orch: orch}

	pathologicalFocusList := [][]string{
		{""},
		{"."},
		{"/root/file.go"},
		{strings.Repeat("deep_dir/", 50) + "target.go"},
		{"🚀_emoji_file.go"},
		{"файл_кириллица.go"},
		{"项目_cjk.go"},
		{strings.Repeat("a", 500)},
	}

	for i, focus := range pathologicalFocusList {
		name := fmt.Sprintf("FocusCase_%d", i)
		t.Run(name, func(t *testing.T) {
			state := NewEngineState("pathological focus test", EngineConfig{MaxTurns: 3})
			state.LatticeSpaceID = "pathological_pipeline_space"
			turn := &TurnState{
				TurnNumber: 1,
				Task:       "test pathological focus files",
				FocusFiles: focus,
			}

			if err := pipeline.executeStage2ASTBlastRadiusWalk(ctx, state, turn, EngineConfig{MaxTurns: 3}); err != nil {
				t.Fatalf("executeStage2ASTBlastRadiusWalk failed: %v", err)
			}

			tokens := llm.EstimateTokens(turn.ASTRibbon)
			if tokens >= 160 {
				t.Fatalf("VIOLATION: stage 2 produced %d tokens >= 160 cap for focus %v:\n%s", tokens, focus, turn.ASTRibbon)
			}
			if !strings.HasPrefix(turn.ASTRibbon, "<nav:ribbon:") || !strings.HasSuffix(turn.ASTRibbon, "/>") {
				t.Fatalf("malformed ribbon structure: %s", turn.ASTRibbon)
			}
		})
	}
}
