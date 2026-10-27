// SPDX-License-Identifier: Apache-2.0
package ui

import (
	"strings"
	"testing"
)

// TestAdversarial_ClosingBracketInsideAttribute tests if action tags can contain
// square brackets inside quoted attribute values (e.g. slice index, regex, or type param).
func TestAdversarial_ClosingBracketInsideAttribute(t *testing.T) {
	tests := []struct {
		name        string
		input       string
		expectedTask string
		expectedCmd  string
	}{
		{
			name:         "slice index in task",
			input:        `[ACTION:BUILD task="Fix slice bounds check in items[0] logic"]`,
			expectedTask: "Fix slice bounds check in items[0] logic",
		},
		{
			name:         "regex character class in heal command",
			input:        `[ACTION:HEAL cmd="go test -run 'Test[A-Z]+' ./..."]`,
			expectedCmd:  "go test -run 'Test[A-Z]+' ./...",
		},
		{
			name:         "multiple bracket pairs in task",
			input:        `[ACTION:BUILD task="Compare a[i] with b[j] in sort"]`,
			expectedTask: "Compare a[i] with b[j] in sort",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			action := ParseActionIntent(tc.input)
			if action == nil {
				t.Fatalf("FAILED: ParseActionIntent returned nil for %q", tc.input)
			}
			if tc.expectedTask != "" && action.Task() != tc.expectedTask {
				t.Errorf("DATA CORRUPTION: expected task %q, got %q (raw tag: %q)", tc.expectedTask, action.Task(), action.RawTag)
			}
			if tc.expectedCmd != "" && action.Cmd() != tc.expectedCmd {
				t.Errorf("DATA CORRUPTION: expected cmd %q, got %q (raw tag: %q)", tc.expectedCmd, action.Cmd(), action.RawTag)
			}
		})
	}
}

// TestAdversarial_EscapedQuotesAndNestedQuotes tests handling of escaped quotes
// and alternating quotes within attributes.
func TestAdversarial_EscapedQuotesAndNestedQuotes(t *testing.T) {
	tests := []struct {
		name         string
		input        string
		expectedTask string
	}{
		{
			name:         "escaped double quotes inside double quotes",
			input:        `[ACTION:BUILD task="Log \"critical error\" before exit"]`,
			expectedTask: `Log \"critical error\" before exit`,
		},
		{
			name:         "double quotes inside single quotes",
			input:        `[ACTION:BUILD task='Use "double" quotes inside single']`,
			expectedTask: `Use "double" quotes inside single`,
		},
		{
			name:         "single quotes inside double quotes",
			input:        `[ACTION:BUILD task="Use 'single' quotes inside double"]`,
			expectedTask: `Use 'single' quotes inside double`,
		},
		{
			name:         "windows path with backslashes",
			input:        `[ACTION:BUILD task="Fix path C:\Users\app\main.go"]`,
			expectedTask: `Fix path C:\Users\app\main.go`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			action := ParseActionIntent(tc.input)
			if action == nil {
				t.Fatalf("FAILED: ParseActionIntent returned nil for %q", tc.input)
			}
			if action.Task() != tc.expectedTask {
				t.Errorf("DATA LOSS: expected task %q, got %q (attrs: %v)", tc.expectedTask, action.Task(), action.Attributes)
			}
		})
	}
}

// TestAdversarial_MarkdownCodeBlocksAndFences tests whether tags inside markdown
// code fences or backticks are incorrectly parsed as executable action intents.
func TestAdversarial_MarkdownCodeBlocksAndFences(t *testing.T) {
	t.Run("tag inside triple backtick code fence should not be extracted when real tag exists outside", func(t *testing.T) {
		input := "Here is an example format that you should not run:\n" +
			"```markdown\n" +
			"[ACTION:BUILD task=\"DO_NOT_RUN_EXAMPLE\"]\n" +
			"```\n\n" +
			"Here is the actual plan:\n" +
			"[ACTION:BUILD task=\"ACTUAL_EXECUTION_TASK\"]\n"

		action := ParseActionIntent(input)
		if action == nil {
			t.Fatal("expected action to be found")
		}
		if action.Task() == "DO_NOT_RUN_EXAMPLE" {
			t.Errorf("SECURITY/ACCIDENTAL EXECUTION: parser extracted tag from inside markdown code block: %q", action.Task())
		}
		if action.Task() != "ACTUAL_EXECUTION_TASK" {
			t.Errorf("expected actual execution task, got %q", action.Task())
		}
	})

	t.Run("tag exclusively inside code fence should not trigger execution", func(t *testing.T) {
		input := "You can run this command:\n```bash\n[ACTION:HEAL cmd=\"rm -rf /\"]\n```\nNo further actions needed."
		action := ParseActionIntent(input)
		if action != nil {
			t.Errorf("FALSE POSITIVE: extracted intent from code block: %+v", action)
		}
	})

	t.Run("tag inside inline backticks should not trigger execution", func(t *testing.T) {
		input := "Note: the format is `[ACTION:BUILD task=\"example\"]`."
		action := ParseActionIntent(input)
		if action != nil {
			t.Errorf("FALSE POSITIVE: extracted intent from inline backticks: %+v", action)
		}
	})
}

// TestAdversarial_UnkeyedTagsWithEqualSigns tests unkeyed tags that contain `=`
// (such as CLI flags in commands).
func TestAdversarial_UnkeyedTagsWithEqualSigns(t *testing.T) {
	tests := []struct {
		name        string
		input       string
		expectedCmd string
	}{
		{
			name:        "unkeyed heal command with flag containing equal sign",
			input:       `[ACTION:HEAL go test -v -run=TestSpecific ./...]`,
			expectedCmd: "go test -v -run=TestSpecific ./...",
		},
		{
			name:        "unkeyed heal command with multiple flags",
			input:       `[ACTION:HEAL go test -count=1 -timeout=30s ./pkg/ui]`,
			expectedCmd: "go test -count=1 -timeout=30s ./pkg/ui",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			action := ParseActionIntent(tc.input)
			if action == nil {
				t.Fatalf("FAILED: ParseActionIntent returned nil for %q", tc.input)
			}
			if action.Cmd() != tc.expectedCmd {
				t.Errorf("DATA CORRUPTION: expected cmd %q, got %q (attrs: %v)", tc.expectedCmd, action.Cmd(), action.Attributes)
			}
		})
	}
}

// TestAdversarial_UnquotedAttributesMultiWord tests unquoted attributes with multiple words.
func TestAdversarial_UnquotedAttributesMultiWord(t *testing.T) {
	input := `[ACTION:BUILD task=Refactor entire authentication pipeline]`
	action := ParseActionIntent(input)
	if action == nil {
		t.Fatal("expected action to be parsed")
	}
	expected := "Refactor entire authentication pipeline"
	if action.Task() != expected {
		t.Errorf("TRUNCATION: expected task %q, got %q (attrs: %v)", expected, action.Task(), action.Attributes)
	}
}

// TestAdversarial_EmptyAttributes tests behavior on empty or whitespace attributes.
func TestAdversarial_EmptyAttributes(t *testing.T) {
	tests := []struct {
		name  string
		input string
	}{
		{
			name:  "empty double quotes",
			input: `[ACTION:BUILD task=""]`,
		},
		{
			name:  "empty single quotes",
			input: `[ACTION:BUILD task='']`,
		},
		{
			name:  "tag without attributes or payload",
			input: `[ACTION:BUILD]`,
		},
		{
			name:  "tag with only spaces",
			input: `[ACTION:BUILD    ]`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			action := ParseActionIntent(tc.input)
			// Should either be nil or have empty Task/Cmd/Query without panic
			if action != nil {
				t.Logf("Tag %q parsed as: %+v, Task: %q", tc.input, action, action.Task())
			}
		})
	}
}

// TestAdversarial_MultiLineTags tests tags formatted across multiple lines.
func TestAdversarial_MultiLineTags(t *testing.T) {
	tests := []struct {
		name         string
		input        string
		expectedTask string
	}{
		{
			name: "tag with attributes on new lines",
			input: "[ACTION:BUILD\n" +
				"  task=\"Multi-line tag\"]\n",
			expectedTask: "Multi-line tag",
		},
		{
			name: "attribute value with embedded newline",
			input: "[ACTION:BUILD task=\"Line 1\nLine 2\"]\n",
			expectedTask: "Line 1\nLine 2",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			action := ParseActionIntent(tc.input)
			if action == nil {
				t.Fatalf("FAILED: ParseActionIntent returned nil for %q", tc.input)
			}
			if action.Task() != tc.expectedTask {
				t.Errorf("expected task %q, got %q", tc.expectedTask, action.Task())
			}
		})
	}
}

// TestAdversarial_UnicodeAndSpecialChars tests emojis, international characters, and symbols.
func TestAdversarial_UnicodeAndSpecialChars(t *testing.T) {
	tests := []struct {
		name          string
		input         string
		expectedTask  string
		expectedQuery string
	}{
		{
			name:         "emojis in task",
			input:        `[ACTION:BUILD task="Implement 🚀 rocket performance in 📦 package"]`,
			expectedTask: "Implement 🚀 rocket performance in 📦 package",
		},
		{
			name:          "Japanese characters in query",
			input:         `[ACTION:MAP query="pkg/インターフェース/テスト"]`,
			expectedQuery: "pkg/インターフェース/テスト",
		},
		{
			name:         "mathematical and punctuation symbols",
			input:        `[ACTION:BUILD task="Compute α + β <= γ && δ != ε"]`,
			expectedTask: "Compute α + β <= γ && δ != ε",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			action := ParseActionIntent(tc.input)
			if action == nil {
				t.Fatalf("FAILED: ParseActionIntent returned nil for %q", tc.input)
			}
			if tc.expectedTask != "" && action.Task() != tc.expectedTask {
				t.Errorf("expected task %q, got %q", tc.expectedTask, action.Task())
			}
			if tc.expectedQuery != "" && action.Query() != tc.expectedQuery {
				t.Errorf("expected query %q, got %q", tc.expectedQuery, action.Query())
			}
		})
	}
}

// TestAdversarial_NestedBrackets tests nested brackets or tags.
func TestAdversarial_NestedBrackets(t *testing.T) {
	input := `[[ACTION:BUILD task="double opening brackets"]]`
	action := ParseActionIntent(input)
	if action == nil {
		t.Fatal("expected action to be found even with outer brackets")
	}
	if action.Task() != "double opening brackets" {
		t.Errorf("expected task 'double opening brackets', got %q", action.Task())
	}
}

// TestAdversarial_StripActionTags_CorruptedRemainder tests if StripActionTags cleanly
// removes tags containing brackets without leaving trailing attribute fragments.
func TestAdversarial_StripActionTags_CorruptedRemainder(t *testing.T) {
	input := `Prefix [ACTION:BUILD task="check items[0] logic"] Suffix`
	stripped := StripActionTags(input)
	expected := "Prefix Suffix"
	if stripped != expected {
		t.Errorf("CORRUPTED STRIP: expected %q, got %q", expected, stripped)
	}
}

// TestAdversarial_MultipleTagsPerTurn tests parser behavior when multiple tags
// are present in a single response turn.
func TestAdversarial_MultipleTagsPerTurn(t *testing.T) {
	input := "First step:\n[ACTION:MAP query=\"pkg/ui\"]\nSecond step:\n[ACTION:BUILD task=\"Implement UI\"]\n"
	all := ParseAllActionIntents(input)
	if len(all) != 2 {
		t.Fatalf("expected 2 intents from ParseAllActionIntents, got %d", len(all))
	}
	first := ParseActionIntent(input)
	if first == nil || first.Type != ActionMap {
		t.Errorf("expected first intent to be ActionMap, got %+v", first)
	}
}

// TestAdversarial_FuzzingNoPanic tests that malformed and pathological inputs
// never panic or crash the parser.
func TestAdversarial_FuzzingNoPanic(t *testing.T) {

	pathologicalInputs := []string{
		"",
		"[",
		"]",
		"[]",
		"[ACTION]",
		"[ACTION:]",
		"[ACTION: ]",
		"[ACTION:BUILD",
		"ACTION:BUILD]",
		"[ACTION:BUILD task=]",
		"[ACTION:BUILD =\"task\"]",
		`[ACTION:BUILD task="unclosed quote]`,
		`[ACTION:BUILD task='unclosed quote]`,
		"[ACTION:BUILD \x00\x01\x02\x03]",
		strings.Repeat("[ACTION:BUILD ", 1000),
		strings.Repeat("]", 1000),
		"[ACTION:" + strings.Repeat("A", 10000) + " task=\"" + strings.Repeat("B", 10000) + "\"]",
		"[ACTION:BUILD task=\"" + strings.Repeat("[", 500) + "\"]",
	}

	for i, input := range pathologicalInputs {
		// Ensure no panic occurs
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("PANIC on input %d (%q): %v", i, input, r)
				}
			}()
			_ = ParseActionIntent(input)
			_ = ParseAllActionIntents(input)
			_ = StripActionTags(input)
		}()
	}
}
