package ui

import "testing"

func TestZeroTestsExecuted(t *testing.T) {
	cases := map[string]bool{
		"Ran 0 tests in 0.000s\n\nOK": true,
		"NO TESTS RAN":                true,
		"collected 0 items":           true,
		"?   \tpkg\t[no test files]":  false,
		"Ran 5 tests in 0.1s\n\nOK":   false,
		"==== 3 passed in 0.2s ====":  false,
	}
	for out, want := range cases {
		if got := zeroTestsExecuted(out); got != want {
			t.Errorf("zeroTestsExecuted(%q)=%v want %v", out, got, want)
		}
	}
	if !isTestRunnerCmd("python -m unittest discover -s tests") || isTestRunnerCmd("go build ./...") {
		t.Error("isTestRunnerCmd misclassified")
	}
}
