package ui

import (
	"strings"
	"testing"
)

// TestExtractContent_NestedCodeBlocks verifies extraction of nested code blocks:
// 4-backtick outer fences wrapping 3-backtick inner code blocks.
func TestExtractContent_NestedCodeBlocks(t *testing.T) {
	intent := &ActionIntent{Type: ActionWrite}

	// 1. Outer 4-backtick fence wrapping Go code
	input1 := "````go\n```go\nfunc main() {\n\tprintln(\"hello world\")\n}\n```\n````"
	expected1 := "```go\nfunc main() {\n\tprintln(\"hello world\")\n}\n```\n"
	res1 := intent.ExtractContent(input1)
	if res1 != expected1 {
		t.Fatalf("Nested 4-backtick outer fence failed.\nGot:\n%s\nExpected:\n%s", res1, expected1)
	}

	// 2. Outer 4-backtick fence wrapping multiple inner 3-backtick code blocks
	input2 := "````markdown\nHere is some Go code:\n```go\nfunc add(a, b int) int { return a + b }\n```\nAnd here is a bash command:\n```bash\ngo test ./...\n```\nEnd of deliverable.\n````"
	expected2 := "Here is some Go code:\n```go\nfunc add(a, b int) int { return a + b }\n```\nAnd here is a bash command:\n```bash\ngo test ./...\n```\nEnd of deliverable.\n"
	res2 := intent.ExtractContent(input2)
	if res2 != expected2 {
		t.Fatalf("Nested 4-backtick with multiple code blocks failed.\nGot:\n%s\nExpected:\n%s", res2, expected2)
	}

	// 3. 4-backtick outer fence with leading preamble and action tag
	input3 := "I have generated the file for you:\n[ACTION:WRITE path=\"example.md\"]\n````markdown\n# Architecture\n```yaml\nversion: '3'\n```\n````"
	expected3 := "# Architecture\n```yaml\nversion: '3'\n```\n"
	res3 := intent.ExtractContent(input3)
	if res3 != expected3 {
		t.Fatalf("Preambled 4-backtick outer fence failed.\nGot:\n%s\nExpected:\n%s", res3, expected3)
	}
}

// TestExtractContent_5and6BacktickFences verifies unwrapping with 5-backtick
// and 6-backtick outer fences.
func TestExtractContent_5and6BacktickFences(t *testing.T) {
	intent := &ActionIntent{Type: ActionWrite}

	// 1. 5-backtick outer fence wrapping 4-backtick and 3-backtick fences
	input5 := "`````markdown\n````go\n```go\nfunc Level3() {}\n```\n````\n`````"
	expected5 := "````go\n```go\nfunc Level3() {}\n```\n````\n"
	res5 := intent.ExtractContent(input5)
	if res5 != expected5 {
		t.Fatalf("5-backtick outer fence failed.\nGot:\n%s\nExpected:\n%s", res5, expected5)
	}

	// 2. 6-backtick outer fence wrapping 5-backtick fences
	input6 := "``````text\n`````markdown\nDeep nesting\n`````\n``````"
	expected6 := "`````markdown\nDeep nesting\n`````\n"
	res6 := intent.ExtractContent(input6)
	if res6 != expected6 {
		t.Fatalf("6-backtick outer fence failed.\nGot:\n%s\nExpected:\n%s", res6, expected6)
	}
}

// TestExtractContent_MarkdownDocs verifies that standalone markdown documents
// with titles, lists, and internal code snippets are preserved in their entirety.
func TestExtractContent_MarkdownDocs(t *testing.T) {
	intent := &ActionIntent{Type: ActionWrite}

	// Standalone markdown document starting with title, with lists and multiple snippets
	inputDoc := `# System Specification

This document describes the OmniCoder architecture.

## Features
- Personalized PageRank RepoMap
- 4-Tier Fuzzy Edit Blocks
- Deterministic 8-Stage Engine

## Implementation Example
` + "```go\npackage main\n\nfunc main() {}\n```\n\n" +
		"## Build Command\n" +
		"```bash\ngo build -o omnicoder.exe ./cmd/omnicoder\n```\n\n" +
		"All tests passing."

	res := intent.ExtractContent(inputDoc)
	// Because this document starts with a title (#) and is not wrapped in outer fences,
	// ExtractContent should preserve the complete document with all headings, lists, and code snippets.
	if !strings.Contains(res, "# System Specification") {
		t.Fatalf("Markdown document title missing.\nGot:\n%s", res)
	}
	if !strings.Contains(res, "- Personalized PageRank RepoMap") {
		t.Fatalf("Markdown document list item missing.\nGot:\n%s", res)
	}
	if !strings.Contains(res, "```go\npackage main\n\nfunc main() {}\n```") {
		t.Fatalf("Markdown internal go snippet corrupted.\nGot:\n%s", res)
	}
	if !strings.Contains(res, "```bash\ngo build -o omnicoder.exe ./cmd/omnicoder\n```") {
		t.Fatalf("Markdown internal bash snippet corrupted.\nGot:\n%s", res)
	}
	if !strings.Contains(res, "All tests passing.") {
		t.Fatalf("Markdown concluding text missing.\nGot:\n%s", res)
	}
}

// TestExtractContent_EdgeCases verifies behavior with raw text, empty responses,
// malformed action tags, and unicode content.
func TestExtractContent_EdgeCases(t *testing.T) {
	intent := &ActionIntent{Type: ActionWrite}

	// 1. Raw text without fences
	rawText := "This is pure plain text content without any markdown fences."
	resRaw := intent.ExtractContent(rawText)
	if strings.TrimSpace(resRaw) != rawText {
		t.Fatalf("Raw text without fences altered.\nGot: %q\nExpected: %q", resRaw, rawText)
	}

	// 2. Empty string and whitespace-only
	if resEmpty := intent.ExtractContent(""); resEmpty != "" {
		t.Fatalf("Empty string should return empty, got %q", resEmpty)
	}
	if resWhitespace := intent.ExtractContent("   \t\r\n   "); resWhitespace != "" {
		t.Fatalf("Whitespace string should return empty, got %q", resWhitespace)
	}

	// 3. Malformed action tags: unclosed, empty type, missing value, nested brackets
	malformed := []string{
		"[ACTION:WRITE path=\"unclosed.txt\"",
		"[ACTION: ]",
		"[ACTION:WRITE path=]",
		"[ACTION:WRITE path=\"[[nested]]\"]",
		"[ACTION:UNKNOWN_ACTION]",
		"Text before [ACTION:WRITE path=\"missing_end\" and text after",
	}
	for _, m := range malformed {
		// Should not panic or hang
		out := intent.ExtractContent(m)
		if out == "" && strings.TrimSpace(m) != "" {
			t.Logf("Malformed tag %q returned empty", m)
		}
	}

	// 4. Unicode robustness: CJK, emojis, arabic, accents, quotes
	unicodeDoc := "# \u67b6\u6784\u8bbe\u8ba1\u6587\u6863 (Architecture)\n\n" +
		"\U0001F680 OmniCoder \u65e5\u5fd7\n" +
		"- \u7279\u6027 1: \u56de\u987e\n" +
		"- \u0645\u0631\u062d\u0628\u0627 (Arabic RTL text)\n" +
		"- Caf\u00e9 & na\u00efve accents\n" +
		"```python\nprint(\"\u2728 \u5b89\u5168 \u2728\")\n```\n"

	resUnicode := intent.ExtractContent(unicodeDoc)
	if !strings.Contains(resUnicode, "\u67b6\u6784\u8bbe\u8ba1\u6587\u6863") {
		t.Fatalf("Unicode CJK lost.\nGot:\n%s", resUnicode)
	}
	if !strings.Contains(resUnicode, "\U0001F680 OmniCoder") {
		t.Fatalf("Unicode emoji lost.\nGot:\n%s", resUnicode)
	}
	if !strings.Contains(resUnicode, "\u0645\u0631\u062d\u0628\u0627") {
		t.Fatalf("Unicode arabic text lost.\nGot:\n%s", resUnicode)
	}
	if !strings.Contains(resUnicode, "\u2728 \u5b89\u5168 \u2728") {
		t.Fatalf("Unicode in code block lost.\nGot:\n%s", resUnicode)
	}
}

// TestExtractContent_ActionTags_InsideVsOutsideCode verifies that action tags
// outside code fences are stripped, while action tags embedded inside code fences
// or inline code are preserved.
func TestExtractContent_ActionTags_InsideVsOutsideCode(t *testing.T) {
	intent := &ActionIntent{Type: ActionWrite}

	// Scenario A: Action tag OUTSIDE code block
	inputA := "[ACTION:WRITE path=\"main.go\"]\n```go\npackage main\n\nfunc main() {}\n```"
	resA := intent.ExtractContent(inputA)
	if strings.Contains(resA, "[ACTION:WRITE") {
		t.Fatalf("Outside action tag was NOT stripped from output.\nGot:\n%s", resA)
	}
	if !strings.Contains(resA, "package main") {
		t.Fatalf("Code content missing in Scenario A.\nGot:\n%s", resA)
	}

	// Scenario B: Action tag INSIDE code block
	inputB := "```go\npackage main\n\n// Demonstrating OmniCoder tags:\n// [ACTION:BUILD task=\"compile\"]\nconst sample = \"[ACTION:WRITE path=\\\"out.txt\\\"]\"\n```"
	resB := intent.ExtractContent(inputB)
	if !strings.Contains(resB, "[ACTION:BUILD task=\"compile\"]") {
		t.Fatalf("Action tag inside code comment was erroneously stripped.\nGot:\n%s", resB)
	}
	if !strings.Contains(resB, "[ACTION:WRITE path=\\\"out.txt\\\"]") {
		t.Fatalf("Action tag inside code string literal was erroneously stripped.\nGot:\n%s", resB)
	}

	// Scenario C: Both outside and inside action tags in same response
	inputC := "[ACTION:WRITE path=\"service.go\"]\n" +
		"```go\n" +
		"package service\n\n" +
		"// Note: tag below is documentation\n" +
		"// [ACTION:HEAL cmd=\"go test\"]\n" +
		"func Heal() {}\n" +
		"```\n" +
		"[ACTION:MAP query=\"symbols\"]"

	resC := intent.ExtractContent(inputC)
	if strings.Contains(resC, "[ACTION:WRITE path=\"service.go\"]") {
		t.Fatalf("Outer WRITE action tag was NOT stripped.\nGot:\n%s", resC)
	}
	if strings.Contains(resC, "[ACTION:MAP query=\"symbols\"]") {
		t.Fatalf("Outer MAP action tag was NOT stripped.\nGot:\n%s", resC)
	}
	if !strings.Contains(resC, "[ACTION:HEAL cmd=\"go test\"]") {
		t.Fatalf("Internal HEAL action tag in code was erroneously stripped.\nGot:\n%s", resC)
	}

	// Scenario D: Inline backtick containing action tag
	inputD := "Here is inline code: `[ACTION:BUILD task=\"inline\"]` in text."
	resD := StripActionTags(inputD)
	if !strings.Contains(resD, "`[ACTION:BUILD task=\"inline\"]`") {
		t.Fatalf("Action tag inside inline code was erroneously stripped by StripActionTags.\nGot:\n%s", resD)
	}
}

// TestExtractContent_DuplicatePathOverride verifies that when an assistant response
// contains a draft write followed by a corrected write for the same file path, each intent
// extracts its designated code block without colliding or falling back to the earlier draft.
func TestExtractContent_DuplicatePathOverride(t *testing.T) {
	response := "Here is the draft:\n" +
		"[ACTION:WRITE path=\"projects/my-service/pkg/server/server.go\"]\n" +
		"```go\n" +
		"package server\n\n" +
		"import (\n\t\"embedgo\"\n\t\"os5\"\n)\n" +
		"```\n\n" +
		"*(Corrected and complete server.go without invalid pseudo-imports):*\n" +
		"[ACTION:WRITE path=\"projects/my-service/pkg/server/server.go\"]\n" +
		"```go\n" +
		"package server\n\n" +
		"import (\n\t\"encoding/json\"\n\t\"net/http\"\n)\n\n" +
		"type Server struct {}\n" +
		"```\n"

	intents := ParseAllActionIntents(response)
	if len(intents) != 2 {
		t.Fatalf("expected 2 intents, got %d", len(intents))
	}

	draft := intents[0].ExtractContent(response)
	if !strings.Contains(draft, "embedgo") {
		t.Fatalf("intent[0] should extract draft with embedgo, got:\n%s", draft)
	}

	corrected := intents[1].ExtractContent(response)
	if strings.Contains(corrected, "embedgo") {
		t.Fatalf("intent[1] erroneously extracted draft with embedgo! Got:\n%s", corrected)
	}
	if !strings.Contains(corrected, "type Server struct") {
		t.Fatalf("intent[1] should extract corrected code with 'type Server struct', got:\n%s", corrected)
	}
}

// TestExtractContent_ClosingTagAndNarrationSanitization verifies that:
// 1. Closing tags [/ACTION:WRITE] clamp extraction and drop all trailing conversational chatter.
// 2. Terminal bash blocks do not hijack HTML/JSON file content in single-intent mode.
// 3. Conversational transitions (Now let's run..., Next, let's create...) are pruned from raw payloads.
func TestExtractContent_ClosingTagAndNarrationSanitization(t *testing.T) {
	// Case 1: Trailing narration after [/ACTION:WRITE] on package.json
	responseWithNarration := `[ACTION:WRITE path="projects/testapp/package.json"]
{
  "name": "testapp",
  "version": "1.0.0"
}
[/ACTION:WRITE]

Now let's run the test suite and build verification.

[ACTION:BUILD task="npm test"]
`
	intents := ParseAllActionIntents(responseWithNarration)
	if len(intents) != 2 {
		t.Fatalf("expected 2 intents, got %d", len(intents))
	}

	pkgContent := intents[0].ExtractContent(responseWithNarration)
	if strings.Contains(pkgContent, "Now let's run") {
		t.Fatalf("pkgContent contaminated with trailing narration!\nGot:\n%s", pkgContent)
	}
	if strings.Contains(pkgContent, "[/ACTION") {
		t.Fatalf("pkgContent contains raw closing tag!\nGot:\n%s", pkgContent)
	}
	if !strings.Contains(pkgContent, `"name": "testapp"`) {
		t.Fatalf("pkgContent missing expected JSON body!\nGot:\n%s", pkgContent)
	}

	// Case 2: Single-intent index.html followed by terminal instructions code block
	htmlWithBashBlock := `To fix this so that Vite serves it properly, let's update index.html:

[ACTION:WRITE path="projects/testapp/index.html"]
<!doctype html>
<html>
  <body><div id="root"></div></body>
</html>
[/ACTION:WRITE]

### How to Run Your App:
` + "```bash\ncd projects/testapp\nnpm run dev\n```"

	singleIntent := ParseActionIntent(htmlWithBashBlock)
	if singleIntent == nil {
		t.Fatal("expected parsed single intent")
	}

	htmlContent := singleIntent.ExtractContent(htmlWithBashBlock)
	if strings.Contains(htmlContent, "cd projects/testapp") || strings.Contains(htmlContent, "npm run dev") {
		t.Fatalf("single-intent HTML extraction was hijacked by unrelated bash code block!\nGot:\n%s", htmlContent)
	}
	if !strings.Contains(htmlContent, "<!doctype html>") || !strings.Contains(htmlContent, "</html>") {
		t.Fatalf("single-intent HTML missing valid HTML body!\nGot:\n%s", htmlContent)
	}

	// Case 3: Raw TSX file with trailing markdown bullet roadmap
	tsxWithRoadmap := `[ACTION:WRITE path="projects/testapp/src/main.tsx"]
import React from 'react';
import ReactDOM from 'react-dom/client';

ReactDOM.createRoot(document.getElementById('root')!).render(<div />);
[/ACTION:WRITE]

Now, let's create the full feature-rich application in App.tsx. This includes:
1. Live Chronometer: Real-time display
2. Interactive Converter: Switch modes
`
	tsxIntent := ParseActionIntent(tsxWithRoadmap)
	if tsxIntent == nil {
		t.Fatal("expected parsed tsx intent")
	}
	tsxContent := tsxIntent.ExtractContent(tsxWithRoadmap)
	if strings.Contains(tsxContent, "Now, let's create") || strings.Contains(tsxContent, "Live Chronometer") {
		t.Fatalf("tsxContent contaminated with roadmap commentary!\nGot:\n%s", tsxContent)
	}
	if !strings.Contains(tsxContent, "createRoot") {
		t.Fatalf("tsxContent missing valid TSX code!\nGot:\n%s", tsxContent)
	}
}
