// SPDX-License-Identifier: Apache-2.0
package devops

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateGoSource(t *testing.T) {
	valid := `package sample

import "fmt"

func Hello() string {
	return "world"
}
`
	if err := ValidateGoSource(valid); err != nil {
		t.Fatalf("expected valid Go code to pass, got: %v", err)
	}

	empty := ""
	if err := ValidateGoSource(empty); err == nil {
		t.Fatal("expected empty code to fail validation")
	}

	invalidSyntax := `package sample

func Broken( {
`
	if err := ValidateGoSource(invalidSyntax); err == nil {
		t.Fatal("expected invalid syntax to fail validation")
	} else if !strings.Contains(err.Error(), "syntax error") {
		t.Errorf("expected error message to mention syntax error, got: %v", err)
	}
}

func TestGetGoPackageName(t *testing.T) {
	code := `// Package doc
package mypkg

func Foo() {}
`
	pkg, err := GetGoPackageName(code)
	if err != nil {
		t.Fatalf("failed to get package name: %v", err)
	}
	if pkg != "mypkg" {
		t.Errorf("expected 'mypkg', got %q", pkg)
	}

	noPkg := `func Foo() {}`
	_, err = GetGoPackageName(noPkg)
	if err == nil {
		t.Fatal("expected error when package declaration is missing")
	}
}

func TestInspectGoSource(t *testing.T) {
	code := `package mathutils

import (
	"math"
	"strings"
)

type Calculator struct {
	Val float64
}

type Transformer interface {
	Transform()
}

func (c *Calculator) Add(x float64) float64 {
	return c.Val + x
}

func Helper() {}
`
	info, err := InspectGoSource(code)
	if err != nil {
		t.Fatalf("failed to inspect Go source: %v", err)
	}
	if info.PackageName != "mathutils" {
		t.Errorf("expected package 'mathutils', got %q", info.PackageName)
	}
	if len(info.Imports) != 2 || info.Imports[0] != "math" || info.Imports[1] != "strings" {
		t.Errorf("unexpected imports: %v", info.Imports)
	}
	if len(info.Types) != 2 || info.Types[0] != "Calculator" || info.Types[1] != "Transformer" {
		t.Errorf("unexpected types: %v", info.Types)
	}
	if len(info.Functions) != 2 {
		t.Errorf("expected 2 functions, got %d: %v", len(info.Functions), info.Functions)
	}
}

func TestCheckGoPackageConsistency(t *testing.T) {
	tmpDir := t.TempDir()

	fileA := filepath.Join(tmpDir, "file_a.go")
	if err := os.WriteFile(fileA, []byte("package alpha\n\nfunc A() {}\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// 1. Same package in same dir passes
	codeB := "package alpha\n\nfunc B() {}\n"
	if err := CheckGoPackageConsistency(tmpDir, "file_b.go", codeB); err != nil {
		t.Errorf("expected matching package to pass, got: %v", err)
	}

	// 2. Different package in same dir fails
	codeC := "package beta\n\nfunc C() {}\n"
	if err := CheckGoPackageConsistency(tmpDir, "file_c.go", codeC); err == nil {
		t.Error("expected mismatched package to fail consistency check")
	} else if !strings.Contains(err.Error(), "package declaration mismatch") {
		t.Errorf("unexpected error message: %v", err)
	}

	// 3. Test package alpha_test is allowed for file_a_test.go
	codeTest := "package alpha_test\n\nfunc TestA() {}\n"
	if err := CheckGoPackageConsistency(tmpDir, "file_a_test.go", codeTest); err != nil {
		t.Errorf("expected alpha_test to be allowed alongside alpha, got: %v", err)
	}

	// 4. Overwriting file_a.go with same package passes
	if err := CheckGoPackageConsistency(tmpDir, "file_a.go", "package alpha\n\nfunc A2() {}\n"); err != nil {
		t.Errorf("overwriting existing file with same package should pass, got: %v", err)
	}
}

func TestValidateGoPreflight(t *testing.T) {
	tmpDir := t.TempDir()

	existing := filepath.Join(tmpDir, "service.go")
	if err := os.WriteFile(existing, []byte("package service\n\ntype Service struct{}\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// Valid syntax + matching package -> PASS
	goodCode := "package service\n\nfunc New() *Service { return &Service{} }\n"
	if err := ValidateGoPreflight(tmpDir, "service_impl.go", goodCode); err != nil {
		t.Fatalf("expected valid preflight to pass, got: %v", err)
	}

	// Invalid syntax -> FAIL
	badSyntax := "package service\n\nfunc New( {\n"
	if err := ValidateGoPreflight(tmpDir, "broken.go", badSyntax); err == nil {
		t.Fatal("expected invalid syntax to fail preflight")
	}

	// Mismatched package -> FAIL
	mismatchedPkg := "package different\n\nfunc New() {}\n"
	if err := ValidateGoPreflight(tmpDir, "diff.go", mismatchedPkg); err == nil {
		t.Fatal("expected mismatched package to fail preflight")
	}
}

func TestNormalizeGoSource(t *testing.T) {
	// 1. Missing space in package declaration
	pkgNoSpace := "packagecanonical\n\nfunc Foo() {}\n"
	normPkg := NormalizeGoSource(pkgNoSpace)
	if !strings.HasPrefix(normPkg, "package canonical") {
		t.Errorf("expected package canonical, got: %s", normPkg)
	}

	// 2. Erroneous non-std crypto/fnv
	cryptoFnv := "package foo\n\nimport (\n\t\"crypto/fnv\"\n)\n"
	normFnv := NormalizeGoSource(cryptoFnv)
	if !strings.Contains(normFnv, `"hash/fnv"`) || strings.Contains(normFnv, `"crypto/fnv"`) {
		t.Errorf("expected hash/fnv replacement, got: %s", normFnv)
	}

	// 3. Top-level := declaration
	topAssign := "package bar\n\nval := 0.0\n\nfunc Bar() {}\n"
	normAssign := NormalizeGoSource(topAssign)
	if !strings.Contains(normAssign, "var val = 0.0") {
		t.Errorf("expected var val = 0.0, got: %s", normAssign)
	}

	// 4. Out-of-order import hoisting
	scatteredImports := `package baz

func Helper1() {}

import "reflect"

func Helper2() {}

import (
	"strings"
)

func Helper3() {}
`
	normImports := NormalizeGoSource(scatteredImports)
	if err := ValidateGoSource(normImports); err != nil {
		t.Fatalf("expected hoisted imports to pass ValidateGoSource, got: %v\nResult:\n%s", err, normImports)
	}
	if !strings.Contains(normImports, `"reflect"`) || !strings.Contains(normImports, `"strings"`) {
		t.Errorf("expected both reflect and strings in normalized imports: %s", normImports)
	}
}

func TestDetectUnclosedLiterals(t *testing.T) {
	// 1. Python unclosed triple quotes
	unclosedPy := `
HTML_TEMPLATE = """
<html>
<body>Hello</body>
`
	if unclosed, reason := DetectUnclosedLiterals(unclosedPy, ".py"); !unclosed {
		t.Errorf("expected unclosed triple quote to be detected, got false")
	} else if !strings.Contains(reason, "unterminated triple-quoted string") {
		t.Errorf("unexpected reason: %s", reason)
	}

	closedPy := `
HTML_TEMPLATE = """
<html>
<body>Hello</body>
</html>
"""
`
	if unclosed, reason := DetectUnclosedLiterals(closedPy, ".py"); unclosed {
		t.Errorf("expected closed triple quote to pass, got error: %s", reason)
	}

	// 2. Python unclosed bracket
	unclosedBracket := `
def foo():
    items = [1, 2, 3
`
	if unclosed, _ := DetectUnclosedLiterals(unclosedBracket, ".py"); !unclosed {
		t.Errorf("expected unclosed bracket to be detected")
	}

	// 3. JS unclosed template literal
	unclosedJS := "const html = `<div>Hello; const x = 5;"
	if unclosed, _ := DetectUnclosedLiterals(unclosedJS, ".js"); !unclosed {
		t.Errorf("expected unclosed JS template literal to be detected")
	}

	// 4. JS valid code with backticks inside comments and string literals must NOT false-positive
	validJSWithBacktickInCommentAndString := "// Note: use `foo` here\nconst msg = \"unclosed ` backtick in string\";\nconst tpl = `hello world`;\n"
	if unclosed, reason := DetectUnclosedLiterals(validJSWithBacktickInCommentAndString, ".ts"); unclosed {
		t.Errorf("expected backticks inside comments/strings to pass, got error: %s", reason)
	}
}

func TestCheckPlatformRuntimeSlip(t *testing.T) {
	pyCodeSlip := `
import subprocess
p = subprocess.Popen(["python3", "server.py"])
`
	if slip, reason := CheckPlatformRuntimeSlip("client.py", pyCodeSlip, "windows"); !slip {
		t.Errorf("expected python3 slip to be detected on windows")
	} else if !strings.Contains(reason, "hardcoded 'python3'") {
		t.Errorf("unexpected slip reason: %s", reason)
	}

	// On linux it should not flag as slip
	if slip, _ := CheckPlatformRuntimeSlip("client.py", pyCodeSlip, "linux"); slip {
		t.Errorf("expected python3 not to be flagged on linux")
	}

	pyCodeFixed := `
import sys, subprocess
p = subprocess.Popen([sys.executable, "server.py"])
`
	if slip, _ := CheckPlatformRuntimeSlip("client.py", pyCodeFixed, "windows"); slip {
		t.Errorf("expected sys.executable to pass on windows")
	}
}

func TestValidatePolyglotPreflight(t *testing.T) {
	// Valid python
	validPy := "def add(a: int, b: int) -> int:\n    return a + b\n"
	if err := ValidatePolyglotPreflight(".", "math_util.py", validPy); err != nil {
		t.Errorf("expected valid python to pass preflight, got: %v", err)
	}

	// Invalid python with truncated string
	invalidPy := "s = \"\"\"\nhello\n"
	if err := ValidatePolyglotPreflight(".", "broken.py", invalidPy); err == nil {
		t.Errorf("expected broken python with unclosed triple quote to fail preflight")
	}

	// Invalid JSON
	invalidJSON := `{"name": "test", "val": }`
	if err := ValidatePolyglotPreflight(".", "data.json", invalidJSON); err == nil {
		t.Errorf("expected invalid JSON to fail preflight")
	}

	// Valid JSON
	validJSON := `{"name": "test", "val": 123}`
	if err := ValidatePolyglotPreflight(".", "data.json", validJSON); err != nil {
		t.Errorf("expected valid JSON to pass preflight, got: %v", err)
	}
}

func TestValidatePythonSource_UnicodeEmoji(t *testing.T) {
	// Emoji and UTF-8 characters that failed under cp1252 stdin decoding on Windows
	src := "import streamlit as st\nst.title(\"🏛️ Dashboard\")\nst.sidebar.header(\"🛠️ Controls\")\nst.markdown(\"🚀 📊 📈 ℹ️ 📚 🔍\")\n"
	if err := ValidatePythonSource(src, "app.py"); err != nil {
		t.Fatalf("valid UTF-8 python with emoji was rejected: %v", err)
	}

	// Preflight check should also pass cleanly
	if err := ValidatePolyglotPreflight(".", "app.py", src); err != nil {
		t.Fatalf("preflight rejected valid UTF-8 python: %v", err)
	}
}

func TestValidatePythonSource_RealSyntaxErrorStillCaught(t *testing.T) {
	badSyntax := "def broken_func(:\n    pass\n"
	if err := ValidatePythonSource(badSyntax, "test.py"); err == nil {
		t.Fatal("expected syntax error to be caught, but got nil")
	} else if !strings.Contains(err.Error(), "syntax error") {
		t.Fatalf("expected error message to mention syntax error, got: %v", err)
	}
}

func TestDetectGoMutexSelfDeadlock(t *testing.T) {
	deadlockingCode := `package raft

import "sync"

type Raft struct {
	mu sync.Mutex
}

func (rf *Raft) resetElectionTimeout() {
	rf.mu.Lock()
	defer rf.mu.Unlock()
}

func (rf *Raft) RequestVote() {
	rf.mu.Lock()
	defer rf.mu.Unlock()
	rf.resetElectionTimeout()
}
`

	issues := DetectGoMutexSelfDeadlock(deadlockingCode)
	if len(issues) == 0 {
		t.Fatalf("expected self-deadlock to be detected in deadlockingCode, got 0 issues")
	}
	if !strings.Contains(issues[0], "RequestVote") || !strings.Contains(issues[0], "resetElectionTimeout") {
		t.Errorf("expected issue to mention RequestVote calling resetElectionTimeout, got: %s", issues[0])
	}

	// Code using unexported locked helper pattern should pass cleanly
	cleanCode := `package raft

import "sync"

type Raft struct {
	mu sync.Mutex
}

func (rf *Raft) resetElectionTimeoutLocked() {
	// assumes rf.mu is already held
}

func (rf *Raft) resetElectionTimeout() {
	rf.mu.Lock()
	defer rf.mu.Unlock()
	rf.resetElectionTimeoutLocked()
}

func (rf *Raft) RequestVote() {
	rf.mu.Lock()
	defer rf.mu.Unlock()
	rf.resetElectionTimeoutLocked()
}
`
	cleanIssues := DetectGoMutexSelfDeadlock(cleanCode)
	if len(cleanIssues) != 0 {
		t.Errorf("expected clean locked-helper code to have 0 issues, got: %v", cleanIssues)
	}

	// Code that unlocks before calling or spawns goroutine with `go r.workerLoop()` must NOT false-positive
	sequentialAndGoStmtCode := `package main

import "sync"

type Runner struct {
	mu sync.Mutex
}

func (r *Runner) workerLoop() {
	r.mu.Lock()
	defer r.mu.Unlock()
}

func (r *Runner) broadcastProgress() {
	r.mu.Lock()
	defer r.mu.Unlock()
}

func (r *Runner) Start() {
	r.mu.Lock()
	r.mu.Unlock()
	go r.workerLoop()
}

func (r *Runner) UpdateNode() {
	r.mu.Lock()
	r.mu.Unlock()
	r.broadcastProgress()
}
`
	if issues := DetectGoMutexSelfDeadlock(sequentialAndGoStmtCode); len(issues) != 0 {
		t.Errorf("expected sequential unlock and go statement not to trigger false-positive deadlock, got: %v", issues)
	}
}

func TestDetectGoRPCReplyDrop(t *testing.T) {
	buggyRPCCode := `package transport

type Transport struct{}

func (t *Transport) Send(target string, method string, args any, reply any) error {
	ch := make(chan any, 1)
	res := <-ch
	_ = res
	return nil
}
`
	warnings := DetectGoRPCReplyDrop(buggyRPCCode)
	if len(warnings) == 0 {
		t.Fatalf("expected RPC reply drop warning, got 0")
	}
	if !strings.Contains(warnings[0], "Send") || !strings.Contains(warnings[0], "reply") {
		t.Errorf("expected warning to mention Send and reply parameter, got: %s", warnings[0])
	}

	validRPCCode := `package transport

type Transport struct{}

func (t *Transport) Send(target string, method string, args any, reply any) error {
	ch := make(chan any, 1)
	res := <-ch
	if p, ok := reply.(*string); ok {
		*p = res.(string)
	}
	return nil
}
`
	validWarnings := DetectGoRPCReplyDrop(validRPCCode)
	if len(validWarnings) != 0 {
		t.Errorf("expected populated reply to have 0 warnings, got: %v", validWarnings)
	}
}

func TestReplaceASTNode(t *testing.T) {
	// 1. Go function replacement
	goSrc := `package main

import "fmt"

func OldHandler(msg string) string {
	return "old: " + msg
}

func Other() {}
`
	newGoFunc := `func OldHandler(msg string) string {
	return fmt.Sprintf("new: %s", msg)
}`
	updatedGo, ok, err := ReplaceASTNode(goSrc, "OldHandler", newGoFunc, ".go")
	if err != nil || !ok {
		t.Fatalf("ReplaceASTNode on Go failed: %v", err)
	}
	if !strings.Contains(updatedGo, "fmt.Sprintf") || !strings.Contains(updatedGo, "func Other()") {
		t.Errorf("expected updated Go code with replacement: %s", updatedGo)
	}

	// 2. TypeScript / React component replacement
	tsSrc := `import React from 'react';

export interface ButtonProps {
	label: string;
}

export const Button: React.FC<ButtonProps> = ({ label }) => {
	return <button>{label}</button>;
};

export function App() {
	return <div><Button label="test" /></div>;
}
`
	newButton := `export const Button: React.FC<ButtonProps> = ({ label }) => {
	return <button className="btn-primary">{label.toUpperCase()}</button>;
};`

	updatedTS, ok, err := ReplaceASTNode(tsSrc, "Button", newButton, ".tsx")
	if err != nil || !ok {
		t.Fatalf("ReplaceASTNode on TSX failed: %v", err)
	}
	if !strings.Contains(updatedTS, "btn-primary") || !strings.Contains(updatedTS, "export function App()") {
		t.Errorf("expected updated TSX with Button replacement: %s", updatedTS)
	}

	// 3. Python function replacement
	pySrc := `import os

def calculate_metric(a, b):
    # Old logic
    res = a * b
    return res

def runner():
    print(calculate_metric(2, 3))
`
	newPyFunc := `def calculate_metric(a, b):
    # New logic with check
    if a < 0:
        return 0
    return a * b + 1`

	updatedPy, ok, err := ReplaceASTNode(pySrc, "calculate_metric", newPyFunc, ".py")
	if err != nil || !ok {
		t.Fatalf("ReplaceASTNode on Python failed: %v", err)
	}
	if !strings.Contains(updatedPy, "New logic with check") || !strings.Contains(updatedPy, "def runner():") {
		t.Errorf("expected updated Python with calculate_metric replacement: %s", updatedPy)
	}
}

func TestAscent5Phase4_VerifyDifferentialPythonAST(t *testing.T) {
	tempDir := t.TempDir()
	pkgDir := filepath.Join(tempDir, "my_pkg")
	_ = os.MkdirAll(pkgDir, 0755)
	_ = os.WriteFile(filepath.Join(pkgDir, "__init__.py"), []byte(""), 0644)
	_ = os.WriteFile(filepath.Join(pkgDir, "helpers.py"), []byte("def valid_helper(x, y=1):\n    return x + y\n"), 0644)

	origCode := `from .helpers import valid_helper

def compute(a, b=2):
    return valid_helper(a, y=b)
`
	// 1. Invalid intra-workspace ImportFrom + invalid CallKwarg + UndefinedName
	badPatch := `from .helpers import valid_helper, nonexistent_helper

def compute(a, b=2):
    val = compute(a, bad_kwarg=10)
    return unimported_module.run(val)
`
	diags := VerifyDifferentialPythonAST(tempDir, "my_pkg/core.py", origCode, badPatch)
	if len(diags) < 3 {
		t.Fatalf("expected all 3 differential AST diagnostics (ImportFrom, CallKwarg, UndefinedName), got: %+v", diags)
	}
	kinds := map[string]bool{}
	for _, d := range diags {
		kinds[d.Kind] = true
	}
	if !kinds["ImportFrom"] || !kinds["CallKwarg"] || !kinds["UndefinedName"] {
		t.Fatalf("missing expected diagnostic kinds, got: %+v", diags)
	}

	// 2. Clean patch must produce 0 diagnostics
	cleanPatch := `import math
from .helpers import valid_helper

def compute(a, b=2):
    scaled = math.ceil(a)
    return valid_helper(scaled, y=b)
`
	cleanDiags := VerifyDifferentialPythonAST(tempDir, "my_pkg/core.py", origCode, cleanPatch)
	if len(cleanDiags) != 0 {
		t.Fatalf("expected 0 diagnostics on clean patch, got: %+v", cleanDiags)
	}
}




