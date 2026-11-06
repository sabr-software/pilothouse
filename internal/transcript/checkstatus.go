package transcript

import (
	"regexp"
	"strings"
)

// A transcript records one exit status per shell command, and that status
// only speaks for a check when nothing after the check decides it. Agents
// usually trim what they run ("go test ./... 2>&1 | tail -5"), and then the
// status is tail's: a failed run reads as clean. For those commands the
// check's own output decides, and when it can't, the result is "unclear"
// rather than a pass nobody saw.

// Check statuses. api.Check documents them.
const (
	checkRunning = "running"
	checkPassed  = "passed"
	checkFailed  = "failed"
	checkUnclear = "unclear"
)

var (
	// setErrexit: `set -e` stops at the first failure, so a later command
	// can't hide one.
	setErrexit = regexp.MustCompile(`(?:^|[\s;&(])set\s+-[a-zA-Z]*e`)
	headStage  = regexp.MustCompile(`^\s*head\b`)

	// backgrounded is an agent's reply for a command it left running: the
	// result is the launch, not the check.
	backgrounded = regexp.MustCompile(`(?i)running in (?:the )?background|will be notified when it completes`)

	// testFailure is what test runners print when a run fails.
	testFailure = regexp.MustCompile(`(?m)^\s*(?:--- FAIL\b|FAIL\b|FAILED\b|ERROR\s+\S+::|panic: |✖ [^\d\s])|test result: FAILED|ERR_ASSERTION|[ℹ#] fail [1-9]|\b[1-9]\d*\s+(?:failed|failing|failures?)\b|\b[1-9]\d* errors? in\b|, [1-9]\d* errors?\b`)
	// testSummary is a whole run's success, so it stands even when the
	// output was cut.
	testSummary = regexp.MustCompile(`(?m)test result: ok|\b[1-9]\d*\s+(?:passed|passing)\b|\b0 failures\b|[ℹ#] fail 0\b|^OK\b|(?i:\ball\b.*\btests?\b.*\bpassed\b)`)
	// testPackage is one package's success: a run can still fail after it.
	testPackage = regexp.MustCompile(`(?m)^ok\s+\S|^PASS\b`)
	// failureFilter is a filter that keeps only a run's failures, so what it
	// leaves empty passed: grep -v '^ok…', grep 'FAIL…'.
	failureFilter = regexp.MustCompile(`\bgrep\s+(?:-[a-zA-Z]+\s+)*['"][^'"]*(?:FAIL|fail|✖|×)|\bgrep\s+-[a-zA-Z]*v[a-zA-Z]*\s+['"]\^?ok`)

	// toolFailure is what linters and builds print when they fail:
	// compiler-style file:line:col diagnostics, or a tool's own summary.
	toolFailure = regexp.MustCompile(`(?mi)^[\w./@~+-]+\.\w+:\d+:\d+: |^[\w./@~+-]+\.\w+:\d+: error:|^[\w./@~+-]+\.\w+\(\d+,\d+\): |^\s*\d+:\d+\s+error\b|\berror TS\d+|^error(?:\[\w+\])?:|\b[1-9]\d* errors?\b|\bbuild failed\b|\bfailed to compile\b|\bBUILD FAILURE\b|could not compile|Code style issues|\bSC\d{4}\b`)
	// runFailure is a package manager reporting that a script failed: it
	// doesn't say which, so it never outweighs a check's own result.
	runFailure = regexp.MustCompile(`\bELIFECYCLE\b|npm ERR!|ERR_PNPM_|error Command failed`)

	// exitFailure and exitZero are a status the agent printed itself:
	// "; echo exit=$?".
	exitFailure = regexp.MustCompile(`(?mi)^\s*(?:exit(?:[ _-]?(?:code|status))?|rc)\s*[=:]?\s*[1-9]\d*\s*$`)
	exitZero    = regexp.MustCompile(`(?mi)^\s*(?:exit(?:[ _-]?(?:code|status))?|rc)\s*[=:]?\s*0\s*$`)

	// unformatted is what `gofmt -l` prints first: a file per line, exit 0.
	unformatted = regexp.MustCompile(`\A\S+\.go\s*$`)
	// harnessNote is what an agent's shell tool adds to a command's output.
	harnessNote = regexp.MustCompile(`(?m)^(?:\(Bash completed with no output\)|Shell cwd was reset to .*|\[This command modified .*)$`)
)

// exitView is what a command's exit status can say about a check in it.
type exitView struct {
	hidden bool // something after the check decides the status
	piped  bool // the check's output goes through a pipe
	cut    bool // that pipe's head may have dropped the output's end
}

// viewExit reads the command after a check, which ends at end in head.
func viewExit(head string, end int) exitView {
	var v exitView
	pipefail := strings.Contains(head, "pipefail")
	errexit := setErrexit.MatchString(head)
	rest := head[end:]
	own := true // still in the check's own pipeline
	for i := 0; i < len(rest); i++ {
		var next byte
		if i+1 < len(rest) {
			next = rest[i+1]
		}
		switch rest[i] {
		case '&':
			switch {
			case next == '&':
				own = false
				i++
			case next == '>' || (i > 0 && rest[i-1] == '>'): // 2>&1, &>file
			default: // backgrounded
				v.hidden = true
			}
		case '|':
			if next == '|' {
				v.hidden = true
				return v
			}
			if next == '&' {
				i++
			}
			if own {
				v.piped = true
				v.hidden = v.hidden || !pipefail
				v.cut = v.cut || headStage.MatchString(rest[i+1:])
			}
		case ';', '\n':
			if !errexit && strings.TrimSpace(rest[i+1:]) != "" {
				v.hidden = true
			}
			own = false
		}
	}
	return v
}

// checkStatus is the result of a check in this step. head is the command as
// commandHead returns it, and end is where the check's runner ends in it.
func (s step) checkStatus(kind, head string, end int) string {
	out := s.result
	switch {
	case s.pending:
		return checkRunning
	case s.isError:
		// "go vet ./... && go test ./...": a test failure means vet passed.
		if kind != "tests" && testFailure.MatchString(out) && !toolFailure.MatchString(out) {
			return checkPassed
		}
		return checkFailed
	case backgrounded.MatchString(out):
		return checkUnclear
	case kind == "lint" && strings.Contains(head, "gofmt -l") && unformatted.MatchString(out):
		return checkFailed // gofmt -l exits 0 and lists what it would change
	}
	v := viewExit(head, end)
	if !v.hidden {
		return checkPassed
	}
	if kind == "tests" {
		return s.testsFromOutput(v)
	}
	// Linters and builds say nothing when they pass, so what's left of the
	// output counts unless a build's end was cut off.
	switch {
	case toolFailure.MatchString(out), runFailure.MatchString(out):
		return checkFailed
	case exitFailure.MatchString(out) && !testFailure.MatchString(out):
		return checkFailed
	case kind == "build" && v.cut && strings.TrimSpace(harnessNote.ReplaceAllString(out, "")) != "":
		return checkUnclear
	}
	return checkPassed
}

// testsFromOutput judges a test run whose exit status is hidden.
func (s step) testsFromOutput(v exitView) string {
	out := s.result
	switch {
	case testFailure.MatchString(out), exitFailure.MatchString(out):
		return checkFailed
	case testSummary.MatchString(out):
		return checkPassed
	case runFailure.MatchString(out):
		return checkFailed
	case !v.cut && testPackage.MatchString(out):
		return checkPassed
	case failureFilter.MatchString(s.command):
		return checkPassed
	case !v.piped && exitZero.MatchString(out):
		return checkPassed
	}
	return checkUnclear
}
