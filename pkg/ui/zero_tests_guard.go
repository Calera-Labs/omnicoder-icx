package ui

import (
	"regexp"
	"strings"
)

var zeroTestsPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\bRan 0 tests?\b`),      // python unittest
	regexp.MustCompile(`(?i)\bNO TESTS RAN\b`),      // unittest 3.12+
	regexp.MustCompile(`(?i)\bno tests ran\b`),      // pytest
	regexp.MustCompile(`(?i)\bcollected 0 items\b`), // pytest
	regexp.MustCompile(`(?i)\bNo tests found\b`),    // jest / vitest
	regexp.MustCompile(`(?i)\brunning 0 tests\b`),   // cargo test
}

// isTestRunnerCmd reports whether cmd is a test invocation, so a zero-test run
// can be distinguished from builds that legitimately print nothing about tests.
func isTestRunnerCmd(cmd string) bool {
	c := strings.ToLower(cmd)
	for _, m := range []string{"unittest", "pytest", "go test", "npm test", "npm run test", "jest", "vitest", "cargo test"} {
		if strings.Contains(c, m) {
			return true
		}
	}
	return false
}

// zeroTestsExecuted detects runner output showing that no test actually ran.
// Exit code 0 in that situation is not a verification pass.
func zeroTestsExecuted(output string) bool {
	for _, re := range zeroTestsPatterns {
		if re.MatchString(output) {
			return true
		}
	}
	return false
}
