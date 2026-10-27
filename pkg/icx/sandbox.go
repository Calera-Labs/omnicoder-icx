// SPDX-License-Identifier: Apache-2.0
// Package icx provides sandboxed execution environments for ICX operations:
// 1. WasmKernel: In-process WebAssembly stack machine (PUSH, ADD, SUB, MUL, DIV, SQRT, MAX, MIN, ABS, HALT).
// 2. ArithmeticEvaluator: Exact arbitrary-precision fixed-point decimal arithmetic (shopspring/decimal).
// 3. ScriptSandbox: Pure Go sandboxed expression and statement execution.

package icx

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/shopspring/decimal"
)

// ============================================================================
// PART 1: WasmKernel (In-Process Stack Machine)
// ============================================================================

const (
	WasmMaxSteps = 10000
	WasmMaxStack = 1024
)

// WasmKernel executes in-process stack machine programs.
type WasmKernel struct {
	MaxSteps int
	MaxStack int
}

// NewWasmKernel instantiates a new WasmKernel.
func NewWasmKernel() *WasmKernel {
	return &WasmKernel{
		MaxSteps: WasmMaxSteps,
		MaxStack: WasmMaxStack,
	}
}

// Exec executes a slice of opcode tokens.
func (k *WasmKernel) Exec(program []string) WasmExecResponse {
	start := time.Now()
	stack := make([]float64, 0, 16)
	steps := 0

	for _, rawToken := range program {
		steps++
		if steps > k.MaxSteps {
			return WasmExecResponse{
				OK:        false,
				Stack:     stack,
				Steps:     steps,
				LatencyUs: float64(time.Since(start).Microseconds()),
				Error:     "WASM execution exceeded step limit",
			}
		}

		token := strings.TrimSpace(rawToken)
		if token == "" {
			continue
		}
		upper := strings.ToUpper(token)

		if upper == "HALT" {
			break
		}

		// PUSH:<n> or PUSH <n>
		if strings.HasPrefix(upper, "PUSH:") || strings.HasPrefix(upper, "PUSH ") {
			var valStr string
			if strings.HasPrefix(upper, "PUSH:") {
				valStr = strings.TrimSpace(token[5:])
			} else {
				valStr = strings.TrimSpace(token[5:])
			}
			val, err := strconv.ParseFloat(valStr, 64)
			if err != nil {
				return WasmExecResponse{
					OK:        false,
					Stack:     stack,
					Steps:     steps,
					LatencyUs: float64(time.Since(start).Microseconds()),
					Error:     fmt.Sprintf("Invalid PUSH value: %s", valStr),
				}
			}
			if len(stack) >= k.MaxStack {
				return WasmExecResponse{
					OK:        false,
					Stack:     stack,
					Steps:     steps,
					LatencyUs: float64(time.Since(start).Microseconds()),
					Error:     "WASM stack overflow",
				}
			}
			stack = append(stack, val)
			continue
		}

		switch upper {
		case "ADD":
			if len(stack) < 2 {
				return wasmUnderflow("ADD", stack, steps, start)
			}
			b := stack[len(stack)-1]
			a := stack[len(stack)-2]
			stack = stack[:len(stack)-2]
			stack = append(stack, a+b)

		case "SUB":
			if len(stack) < 2 {
				return wasmUnderflow("SUB", stack, steps, start)
			}
			b := stack[len(stack)-1]
			a := stack[len(stack)-2]
			stack = stack[:len(stack)-2]
			stack = append(stack, a-b)

		case "MUL":
			if len(stack) < 2 {
				return wasmUnderflow("MUL", stack, steps, start)
			}
			b := stack[len(stack)-1]
			a := stack[len(stack)-2]
			stack = stack[:len(stack)-2]
			stack = append(stack, a*b)

		case "DIV":
			if len(stack) < 2 {
				return wasmUnderflow("DIV", stack, steps, start)
			}
			b := stack[len(stack)-1]
			a := stack[len(stack)-2]
			stack = stack[:len(stack)-2]
			if b == 0 {
				return WasmExecResponse{
					OK:        false,
					Stack:     stack,
					Steps:     steps,
					LatencyUs: float64(time.Since(start).Microseconds()),
					Error:     "WASM DIV by zero",
				}
			}
			stack = append(stack, a/b)

		case "SQRT":
			if len(stack) < 1 {
				return wasmUnderflow("SQRT", stack, steps, start)
			}
			a := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if a < 0 {
				return WasmExecResponse{
					OK:        false,
					Stack:     stack,
					Steps:     steps,
					LatencyUs: float64(time.Since(start).Microseconds()),
					Error:     "WASM SQRT of negative number",
				}
			}
			stack = append(stack, math.Sqrt(a))

		case "ABS":
			if len(stack) < 1 {
				return wasmUnderflow("ABS", stack, steps, start)
			}
			a := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			stack = append(stack, math.Abs(a))

		case "MAX":
			if len(stack) < 2 {
				return wasmUnderflow("MAX", stack, steps, start)
			}
			b := stack[len(stack)-1]
			a := stack[len(stack)-2]
			stack = stack[:len(stack)-2]
			stack = append(stack, math.Max(a, b))

		case "MIN":
			if len(stack) < 2 {
				return wasmUnderflow("MIN", stack, steps, start)
			}
			b := stack[len(stack)-1]
			a := stack[len(stack)-2]
			stack = stack[:len(stack)-2]
			stack = append(stack, math.Min(a, b))

		default:
			return WasmExecResponse{
				OK:        false,
				Stack:     stack,
				Steps:     steps,
				LatencyUs: float64(time.Since(start).Microseconds()),
				Error:     fmt.Sprintf("Unknown WASM opcode: %s", token),
			}
		}
	}

	result := 0.0
	if len(stack) > 0 {
		result = stack[len(stack)-1]
	}

	return WasmExecResponse{
		OK:        true,
		Result:    result,
		Stack:     stack,
		Steps:     steps,
		LatencyUs: float64(time.Since(start).Microseconds()),
	}
}

func wasmUnderflow(op string, stack []float64, steps int, start time.Time) WasmExecResponse {
	return WasmExecResponse{
		OK:        false,
		Stack:     stack,
		Steps:     steps,
		LatencyUs: float64(time.Since(start).Microseconds()),
		Error:     fmt.Sprintf("stack underflow on %s", op),
	}
}

// ============================================================================
// PART 2: ArithmeticEvaluator (Exact Decimal Math - shopspring/decimal)
// ============================================================================

// Fault represents an arithmetic evaluation fault.
type Fault struct {
	Msg string
}

func (f Fault) Error() string { return "[SANDBOX_FAULT] " + f.Msg }

// EvalArithmetic evaluates an arithmetic expression string with shopspring/decimal.
// Rejects IEEE floats in env.
func EvalArithmetic(expr string, env map[string]any) (decimal.Decimal, error) {
	// Verify no IEEE floats are present in env
	for k, v := range env {
		switch v.(type) {
		case float32, float64:
			return decimal.Zero, Fault{Msg: fmt.Sprintf("IEEE float forbidden in environment for '%s'", k)}
		}
	}

	p := &arithParser{toks: arithLex(expr), env: env}
	v, err := p.parseExpr()
	if err != nil {
		return decimal.Zero, err
	}
	if p.peek().kind != "eof" {
		return decimal.Zero, Fault{Msg: fmt.Sprintf("trailing tokens in formula: '%s'", p.peek().val)}
	}
	return v, nil
}

type arithToken struct {
	kind string
	val  string
}

func arithLex(s string) []arithToken {
	out := make([]arithToken, 0, len(s)/3+4)
	i := 0
	for i < len(s) {
		r := rune(s[i])
		if unicode.IsSpace(r) {
			i++
			continue
		}
		if s[i] == '\'' || s[i] == '"' {
			q := s[i]
			j := i + 1
			for j < len(s) && s[j] != q {
				j++
			}
			out = append(out, arithToken{kind: "str", val: s[i+1 : j]})
			i = j + 1
			continue
		}
		if unicode.IsLetter(r) || r == '_' {
			j := i + 1
			for j < len(s) {
				rr := rune(s[j])
				if unicode.IsLetter(rr) || unicode.IsDigit(rr) || rr == '_' {
					j++
					continue
				}
				break
			}
			out = append(out, arithToken{kind: "id", val: s[i:j]})
			i = j
			continue
		}
		if unicode.IsDigit(r) || (r == '.' && i+1 < len(s) && unicode.IsDigit(rune(s[i+1]))) {
			j := i
			for j < len(s) && (unicode.IsDigit(rune(s[j])) || s[j] == '.') {
				j++
			}
			out = append(out, arithToken{kind: "num", val: s[i:j]})
			i = j
			continue
		}
		if strings.HasPrefix(s[i:], "**") {
			out = append(out, arithToken{kind: "op", val: "**"})
			i += 2
			continue
		}
		switch s[i] {
		case '+', '-', '*', '/', '%', '^', '(', ')', '[', ']', ',':
			out = append(out, arithToken{kind: "op", val: string(s[i])})
			i++
		default:
			out = append(out, arithToken{kind: "op", val: string(s[i])})
			i++
		}
	}
	out = append(out, arithToken{kind: "eof"})
	return out
}

type arithParser struct {
	toks []arithToken
	i    int
	env  map[string]any
}

func (p *arithParser) peek() arithToken {
	if p.i >= len(p.toks) {
		return arithToken{kind: "eof"}
	}
	return p.toks[p.i]
}

func (p *arithParser) eat(val string) bool {
	if p.peek().val == val {
		p.i++
		return true
	}
	return false
}

func (p *arithParser) parseExpr() (decimal.Decimal, error) {
	left, err := p.parseTerm()
	if err != nil {
		return decimal.Zero, err
	}
	for {
		if p.eat("+") {
			r, err := p.parseTerm()
			if err != nil {
				return decimal.Zero, err
			}
			left = left.Add(r)
			continue
		}
		if p.eat("-") {
			r, err := p.parseTerm()
			if err != nil {
				return decimal.Zero, err
			}
			left = left.Sub(r)
			continue
		}
		break
	}
	return left, nil
}

func (p *arithParser) parseTerm() (decimal.Decimal, error) {
	left, err := p.parseUnary()
	if err != nil {
		return decimal.Zero, err
	}
	for {
		if p.eat("*") {
			r, err := p.parseUnary()
			if err != nil {
				return decimal.Zero, err
			}
			left = left.Mul(r)
			continue
		}
		if p.eat("/") {
			r, err := p.parseUnary()
			if err != nil {
				return decimal.Zero, err
			}
			if r.IsZero() {
				return decimal.Zero, Fault{Msg: "division by zero"}
			}
			left = left.Div(r)
			continue
		}
		if p.eat("%") {
			r, err := p.parseUnary()
			if err != nil {
				return decimal.Zero, err
			}
			if r.IsZero() {
				return decimal.Zero, Fault{Msg: "modulo by zero"}
			}
			left = left.Mod(r)
			continue
		}
		break
	}
	return left, nil
}

func (p *arithParser) parseUnary() (decimal.Decimal, error) {
	if p.eat("-") {
		v, err := p.parseUnary()
		if err != nil {
			return decimal.Zero, err
		}
		return v.Neg(), nil
	}
	p.eat("+")
	return p.parsePower()
}

func (p *arithParser) parsePower() (decimal.Decimal, error) {
	left, err := p.parsePrimary()
	if err != nil {
		return decimal.Zero, err
	}
	if p.peek().val == "**" || p.peek().val == "^" {
		p.i++
		right, err := p.parseUnary()
		if err != nil {
			return decimal.Zero, err
		}
		return decPower(left, right)
	}
	return left, nil
}

func (p *arithParser) parsePrimary() (decimal.Decimal, error) {
	t := p.peek()
	if t.kind == "num" {
		p.i++
		d, err := decimal.NewFromString(t.val)
		if err != nil {
			return decimal.Zero, Fault{Msg: "invalid number: " + t.val}
		}
		return d, nil
	}
	if t.kind == "str" {
		p.i++
		d, err := decimal.NewFromString(t.val)
		if err != nil {
			return decimal.Zero, Fault{Msg: "invalid decimal string: " + t.val}
		}
		return d, nil
	}
	if t.kind == "id" {
		p.i++
		name := t.val
		if name == "Decimal" && p.eat("(") {
			inner := p.peek()
			if inner.kind != "str" && inner.kind != "num" {
				return decimal.Zero, Fault{Msg: "Decimal() requires string or numeric argument"}
			}
			p.i++
			if !p.eat(")") {
				return decimal.Zero, Fault{Msg: "Decimal() missing closing parenthesis"}
			}
			return decimal.NewFromString(inner.val)
		}
		if name == "abs" && p.eat("(") {
			v, err := p.parseExpr()
			if err != nil {
				return decimal.Zero, err
			}
			if !p.eat(")") {
				return decimal.Zero, Fault{Msg: "abs() missing )"}
			}
			return v.Abs(), nil
		}
		if name == "sum" && p.eat("(") {
			vals, err := p.parseArgList()
			if err != nil {
				return decimal.Zero, err
			}
			if !p.eat(")") {
				return decimal.Zero, Fault{Msg: "sum() missing )"}
			}
			acc := decimal.Zero
			for _, val := range vals {
				acc = acc.Add(val)
			}
			return acc, nil
		}
		if name == "round" && p.eat("(") {
			vals, err := p.parseArgList()
			if err != nil {
				return decimal.Zero, err
			}
			if !p.eat(")") || len(vals) == 0 {
				return decimal.Zero, Fault{Msg: "round(val, places) requires arguments"}
			}
			places := int32(0)
			if len(vals) > 1 {
				places = int32(vals[1].IntPart())
			}
			return vals[0].Round(places), nil
		}
		if name == "sqrt" && p.eat("(") {
			v, err := p.parseExpr()
			if err != nil {
				return decimal.Zero, err
			}
			if !p.eat(")") {
				return decimal.Zero, Fault{Msg: "sqrt() missing )"}
			}
			return decSquareRoot(v)
		}
		if name == "pow" && p.eat("(") {
			vals, err := p.parseArgList()
			if err != nil {
				return decimal.Zero, err
			}
			if !p.eat(")") || len(vals) < 2 {
				return decimal.Zero, Fault{Msg: "pow(base, exp) requires 2 arguments"}
			}
			return decPower(vals[0], vals[1])
		}
		if name == "cagr" && p.eat("(") {
			vals, err := p.parseArgList()
			if err != nil {
				return decimal.Zero, err
			}
			if !p.eat(")") || len(vals) < 3 {
				return decimal.Zero, Fault{Msg: "cagr(start, end, years) requires 3 arguments"}
			}
			startVal := vals[0]
			endVal := vals[1]
			years := vals[2]
			if startVal.IsZero() || years.IsZero() {
				return decimal.Zero, Fault{Msg: "cagr division by zero: start and years must be non-zero"}
			}
			growth := endVal.Div(startVal)
			invYears := decimal.NewFromInt(1).Div(years)
			pwr, err := decPower(growth, invYears)
			if err != nil {
				return decimal.Zero, err
			}
			return pwr.Sub(decimal.NewFromInt(1)), nil
		}
		if name == "floor" && p.eat("(") {
			v, err := p.parseExpr()
			if err != nil {
				return decimal.Zero, err
			}
			if !p.eat(")") {
				return decimal.Zero, Fault{Msg: "floor() missing )"}
			}
			return v.Floor(), nil
		}
		if name == "ceil" && p.eat("(") {
			v, err := p.parseExpr()
			if err != nil {
				return decimal.Zero, err
			}
			if !p.eat(")") {
				return decimal.Zero, Fault{Msg: "ceil() missing )"}
			}
			return v.Ceil(), nil
		}
		if name == "min" && p.eat("(") {
			vals, err := p.parseArgList()
			if err != nil {
				return decimal.Zero, err
			}
			if !p.eat(")") || len(vals) == 0 {
				return decimal.Zero, Fault{Msg: "min() requires arguments"}
			}
			m := vals[0]
			for _, val := range vals[1:] {
				if val.LessThan(m) {
					m = val
				}
			}
			return m, nil
		}
		if name == "max" && p.eat("(") {
			vals, err := p.parseArgList()
			if err != nil {
				return decimal.Zero, err
			}
			if !p.eat(")") || len(vals) == 0 {
				return decimal.Zero, Fault{Msg: "max() requires arguments"}
			}
			m := vals[0]
			for _, val := range vals[1:] {
				if val.GreaterThan(m) {
					m = val
				}
			}
			return m, nil
		}

		// Lookup from environment
		if p.env != nil {
			if v, ok := p.env[name]; ok {
				return toDecimal(v)
			}
		}
		return decimal.Zero, Fault{Msg: fmt.Sprintf("unresolved identifier: '%s'", name)}
	}
	if p.eat("(") {
		v, err := p.parseExpr()
		if err != nil {
			return decimal.Zero, err
		}
		if !p.eat(")") {
			return decimal.Zero, Fault{Msg: "missing closing parenthesis"}
		}
		return v, nil
	}
	return decimal.Zero, Fault{Msg: fmt.Sprintf("unexpected token: '%s'", t.val)}
}

func (p *arithParser) parseArgList() ([]decimal.Decimal, error) {
	var list []decimal.Decimal
	if p.peek().val == ")" {
		return list, nil
	}
	for {
		v, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		list = append(list, v)
		if !p.eat(",") {
			break
		}
	}
	return list, nil
}

func toDecimal(v any) (decimal.Decimal, error) {
	switch x := v.(type) {
	case decimal.Decimal:
		return x, nil
	case *decimal.Decimal:
		if x == nil {
			return decimal.Zero, Fault{Msg: "nil *decimal.Decimal"}
		}
		return *x, nil
	case int:
		return decimal.NewFromInt(int64(x)), nil
	case int64:
		return decimal.NewFromInt(x), nil
	case int32:
		return decimal.NewFromInt(int64(x)), nil
	case string:
		return decimal.NewFromString(x)
	default:
		return decimal.Zero, Fault{Msg: fmt.Sprintf("cannot convert %T to Decimal", v)}
	}
}

func decSquareRoot(d decimal.Decimal) (decimal.Decimal, error) {
	if d.IsNegative() {
		return decimal.Zero, Fault{Msg: "sqrt of negative number"}
	}
	if d.IsZero() {
		return decimal.Zero, nil
	}
	two := decimal.NewFromInt(2)
	x := d.Div(two)
	if x.IsZero() {
		x = d
	}
	for i := 0; i < 25; i++ {
		next := x.Add(d.Div(x)).Div(two)
		if next.Equal(x) {
			break
		}
		x = next
	}
	return x, nil
}

func decPower(base, exp decimal.Decimal) (decimal.Decimal, error) {
	if base.IsZero() && exp.IsNegative() {
		return decimal.Zero, Fault{Msg: "zero to negative power"}
	}
	if base.IsNegative() && !exp.IsInteger() {
		return decimal.Zero, Fault{Msg: "power with negative base and non-integer exponent"}
	}
	return base.Pow(exp), nil
}

// ============================================================================
// PART 3: ScriptSandbox (Pure Go Sandboxed Execution)
// ============================================================================

var bannedScriptTokens = []string{
	"import", "exec", "eval", "open", "os", "sys", "subprocess", "reflect", "unsafe", "__",
}

// ExecScript evaluates a multi-statement script safely in memory without subprocesses.
func ExecScript(source string, localsIn map[string]any) ExecResponse {
	start := time.Now()
	localsOut := make(map[string]any)
	for k, v := range localsIn {
		localsOut[k] = v
	}

	// Security audit: check for banned tokens
	lower := strings.ToLower(source)
	for _, banned := range bannedScriptTokens {
		// match whole word or prefix
		idx := strings.Index(lower, banned)
		if idx != -1 {
			return ExecResponse{
				OK:        false,
				LocalsOut: localsOut,
				LatencyUs: float64(time.Since(start).Microseconds()),
				Error:     fmt.Sprintf("Sandbox security violation: forbidden token '%s'", banned),
			}
		}
	}

	lines := strings.Split(source, "\n")
	var lastResult any = nil

	for lineIdx, rawLine := range lines {
		line := strings.TrimSpace(rawLine)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "//") {
			continue
		}

		// Handle semicolon-separated statements on single line
		statements := strings.Split(line, ";")
		for _, stmt := range statements {
			stmt = strings.TrimSpace(stmt)
			if stmt == "" {
				continue
			}

			// Assignment: var = expr
			if eqIdx := strings.Index(stmt, "="); eqIdx != -1 && !strings.ContainsAny(stmt[:eqIdx], "+-*/!<>=") {
				varName := strings.TrimSpace(stmt[:eqIdx])
				expr := strings.TrimSpace(stmt[eqIdx+1:])
				val, err := evalScriptExpr(expr, localsOut)
				if err != nil {
					return ExecResponse{
						OK:        false,
						LocalsOut: localsOut,
						LatencyUs: float64(time.Since(start).Microseconds()),
						Error:     fmt.Sprintf("Line %d statement error: %v", lineIdx+1, err),
					}
				}
				localsOut[varName] = val
				lastResult = val
				continue
			}

			// Bare expression
			val, err := evalScriptExpr(stmt, localsOut)
			if err != nil {
				return ExecResponse{
					OK:        false,
					LocalsOut: localsOut,
					LatencyUs: float64(time.Since(start).Microseconds()),
					Error:     fmt.Sprintf("Line %d expression error: %v", lineIdx+1, err),
				}
			}
			lastResult = val
		}
	}

	// Determine final result: priority result > value > ratio > lastResult
	finalResult := lastResult
	if r, ok := localsOut["result"]; ok {
		finalResult = r
	} else if v, ok := localsOut["value"]; ok {
		finalResult = v
	} else if rt, ok := localsOut["ratio"]; ok {
		finalResult = rt
	}

	return ExecResponse{
		OK:        true,
		Result:    finalResult,
		LocalsOut: localsOut,
		LatencyUs: float64(time.Since(start).Microseconds()),
	}
}

func evalScriptExpr(expr string, env map[string]any) (any, error) {
	expr = strings.TrimSpace(expr)

	// Ternary expression: cond ? true_val : false_val
	if qIdx := strings.Index(expr, "?"); qIdx != -1 {
		colonIdx := strings.LastIndex(expr, ":")
		if colonIdx > qIdx {
			condStr := strings.TrimSpace(expr[:qIdx])
			trueExpr := strings.TrimSpace(expr[qIdx+1 : colonIdx])
			falseExpr := strings.TrimSpace(expr[colonIdx+1:])

			condVal, err := evalScriptExpr(condStr, env)
			if err != nil {
				return nil, err
			}
			isTrue := false
			switch cv := condVal.(type) {
			case bool:
				isTrue = cv
			case int:
				isTrue = cv != 0
			case int64:
				isTrue = cv != 0
			case decimal.Decimal:
				isTrue = !cv.IsZero()
			case string:
				isTrue = cv != "" && cv != "false" && cv != "0"
			}
			if isTrue {
				return evalScriptExpr(trueExpr, env)
			}
			return evalScriptExpr(falseExpr, env)
		}
	}

	// Comparison operators
	for _, op := range []string{"==", "!=", "<=", ">=", "<", ">"} {
		if opIdx := strings.Index(expr, op); opIdx != -1 {
			leftStr := strings.TrimSpace(expr[:opIdx])
			rightStr := strings.TrimSpace(expr[opIdx+len(op):])
			leftVal, err1 := evalScriptExpr(leftStr, env)
			rightVal, err2 := evalScriptExpr(rightStr, env)
			if err1 == nil && err2 == nil {
				return compareValues(leftVal, rightVal, op)
			}
		}
	}

	// Try arithmetic evaluation with decimal.Decimal
	dec, err := EvalArithmetic(expr, env)
	if err == nil {
		// If it's an exact integer, return int64
		if dec.IsInteger() {
			return dec.IntPart(), nil
		}
		return dec.String(), nil
	}

	// String literal
	if (strings.HasPrefix(expr, `"`) && strings.HasSuffix(expr, `"`)) ||
		(strings.HasPrefix(expr, `'`) && strings.HasSuffix(expr, `'`)) {
		return expr[1 : len(expr)-1], nil
	}

	// Boolean literal
	if expr == "true" {
		return true, nil
	}
	if expr == "false" {
		return false, nil
	}

	// Identifier lookup
	if val, ok := env[expr]; ok {
		return val, nil
	}

	return nil, err
}

func compareValues(left, right any, op string) (bool, error) {
	dLeft, err1 := toDecimal(left)
	dRight, err2 := toDecimal(right)
	if err1 == nil && err2 == nil {
		switch op {
		case "==":
			return dLeft.Equal(dRight), nil
		case "!=":
			return !dLeft.Equal(dRight), nil
		case "<":
			return dLeft.LessThan(dRight), nil
		case "<=":
			return dLeft.LessThanOrEqual(dRight), nil
		case ">":
			return dLeft.GreaterThan(dRight), nil
		case ">=":
			return dLeft.GreaterThanOrEqual(dRight), nil
		}
	}

	sLeft := fmt.Sprintf("%v", left)
	sRight := fmt.Sprintf("%v", right)
	switch op {
	case "==":
		return sLeft == sRight, nil
	case "!=":
		return sLeft != sRight, nil
	case "<":
		return sLeft < sRight, nil
	case "<=":
		return sLeft <= sRight, nil
	case ">":
		return sLeft > sRight, nil
	case ">=":
		return sLeft >= sRight, nil
	default:
		return false, fmt.Errorf("unknown operator %s", op)
	}
}
