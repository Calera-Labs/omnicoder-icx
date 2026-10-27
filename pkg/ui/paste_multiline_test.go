// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
)

func TestReadPrompt_BracketedPaste_SingleLine(t *testing.T) {
	input := "\x1b[200~single line pasted from clipboard\x1b[201~\n"
	reader := bufio.NewReader(strings.NewReader(input))
	var out bytes.Buffer

	prompt, err := ReadPrompt(reader, &out, "omnicoder> ")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	expected := "single line pasted from clipboard"
	if prompt != expected {
		t.Errorf("expected %q, got %q", expected, prompt)
	}
}

func TestReadPrompt_BracketedPaste_WindowsArtifacts(t *testing.T) {
	testCases := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "Windows terminal 0~ and 1~ prefix/suffix",
			input:    "0~build projects/demo/plan.md1~\n",
			expected: "build projects/demo/plan.md",
		},
		{
			name:     "Literal ^[[200~ and ^[[201~ escape code text",
			input:    "^[[200~build projects/demo/plan.md^[[201~\n",
			expected: "build projects/demo/plan.md",
		},
		{
			name:     "CSI [200~ and [201~ without ESC byte",
			input:    "[200~build projects/demo/plan.md[201~\n",
			expected: "build projects/demo/plan.md",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			reader := bufio.NewReader(strings.NewReader(tc.input))
			var out bytes.Buffer
			prompt, err := ReadPrompt(reader, &out, "omnicoder> ")
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if prompt != tc.expected {
				t.Errorf("expected %q, got %q", tc.expected, prompt)
			}
		})
	}
}

func TestReadPrompt_BracketedPaste_MultiLineLarge(t *testing.T) {
	var sb strings.Builder
	sb.WriteString("\x1b[200~")
	for i := 1; i <= 150; i++ {
		sb.WriteString(fmt.Sprintf("Line %d: log diagnostic event [code=%d]\n", i, i*10))
	}
	sb.WriteString("\x1b[201~\n")

	reader := bufio.NewReaderSize(strings.NewReader(sb.String()), 1024*1024)
	var out bytes.Buffer

	prompt, err := ReadPrompt(reader, &out, "omnicoder> ")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	lines := strings.Split(prompt, "\n")
	if len(lines) != 150 {
		t.Fatalf("expected 150 lines in pasted prompt, got %d lines", len(lines))
	}
	if !strings.HasPrefix(lines[0], "Line 1:") {
		t.Errorf("expected first line to start with 'Line 1:', got %q", lines[0])
	}
	if !strings.HasPrefix(lines[149], "Line 150:") {
		t.Errorf("expected last line to start with 'Line 150:', got %q", lines[149])
	}
	if strings.Contains(prompt, "\x1b[200~") || strings.Contains(prompt, "\x1b[201~") {
		t.Error("bracketed paste escape sequences were not cleanly stripped")
	}
}

func TestReadPrompt_MultiLineMode_Explicit(t *testing.T) {
	input := "First line of task\nSecond line of task\nThird line of task\n\n"
	reader := bufio.NewReader(strings.NewReader(input))
	var out bytes.Buffer

	prompt, err := ReadPromptMultiLine(reader, &out, "omnicoder [multiline]> ", true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	expected := "First line of task\nSecond line of task\nThird line of task"
	if prompt != expected {
		t.Errorf("expected %q, got %q", expected, prompt)
	}
}

func TestReadPrompt_MultiLineMode_EOFSubmission(t *testing.T) {
	input := "First line of task\nSecond line of task\nEOF\n"
	reader := bufio.NewReader(strings.NewReader(input))
	var out bytes.Buffer

	prompt, err := ReadPromptMultiLine(reader, &out, "omnicoder [multiline]> ", true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	expected := "First line of task\nSecond line of task"
	if prompt != expected {
		t.Errorf("expected %q, got %q", expected, prompt)
	}
}

func TestReadPrompt_InteractiveRapidPaste(t *testing.T) {
	SetInteractiveTerminal(true)
	defer SetInteractiveTerminal(false)

	pastedContent := "Line 1: build.bat not found\nLine 2: bash make build failed\nLine 3: please fix\n"
	reader := bufio.NewReaderSize(strings.NewReader(pastedContent), 1024*1024)
	var out bytes.Buffer

	prompt, err := ReadPrompt(reader, &out, "omnicoder> ")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	lines := strings.Split(prompt, "\n")
	if len(lines) != 3 {
		t.Fatalf("expected 3 lines in rapid paste, got %d (prompt=%q)", len(lines), prompt)
	}
	if lines[0] != "Line 1: build.bat not found" {
		t.Errorf("expected line 1 mismatch: got %q", lines[0])
	}
	if lines[2] != "Line 3: please fix" {
		t.Errorf("expected line 3 mismatch: got %q", lines[2])
	}
}

func TestCLI_MultiLineToggleCommand(t *testing.T) {
	session := NewSession(nil, nil, t.TempDir())
	if session.IsMultiLine() {
		t.Fatal("expected multi-line mode to be OFF initially")
	}

	input := strings.NewReader("/multiline\n/ml\n/exit\n")
	var output bytes.Buffer

	err := RunConversationalUI(context.Background(), session, input, &output)
	if err != nil {
		t.Fatalf("RunConversationalUI failed: %v", err)
	}

	outStr := output.String()
	if !strings.Contains(outStr, "[Multi-Line Mode: ON]") {
		t.Errorf("expected ON confirmation in output:\n%s", outStr)
	}
	if !strings.Contains(outStr, "[Multi-Line Mode: OFF]") {
		t.Errorf("expected OFF confirmation in output:\n%s", outStr)
	}
}

func TestCLI_PasteSlashCommand_Manual(t *testing.T) {
	session := NewSession(nil, nil, t.TempDir())

	// Call /paste manual followed by two lines and EOF, then /exit
	input := strings.NewReader("/paste manual\nError: line 1\nError: line 2\nEOF\n/exit\n")
	var output bytes.Buffer

	err := RunConversationalUI(context.Background(), session, input, &output)
	if err != nil {
		t.Fatalf("RunConversationalUI failed: %v", err)
	}

	outStr := output.String()
	if !strings.Contains(outStr, "[Interactive Paste Mode Active]") {
		t.Errorf("expected paste mode prompt in output:\n%s", outStr)
	}
}

func TestClipboard_GetClipboardText_NoCrash(t *testing.T) {
	// Verify that reading the clipboard succeeds or returns a clean error without panic
	text, err := GetClipboardText()
	if err != nil {
		t.Logf("clipboard unavailable (expected in headless CI/CD): %v", err)
	} else {
		t.Logf("clipboard returned %d bytes", len(text))
	}
}
