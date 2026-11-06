package transcript

import (
	"strings"
	"testing"
	"time"

	"github.com/sabr-software/pilothouse/internal/api"
)

// statusOf judges one command as buildReceipt would.
func statusOf(kind, command, result string, isError bool) string {
	head := commandHead(command)
	_, end := runnerAt(checkCommands[kind], head)
	return step{command: command, result: result, isError: isError}.checkStatus(kind, head, end)
}

// The outputs below are real, from agents' transcripts.
func TestCheckStatus(t *testing.T) {
	cases := []struct {
		name, kind, command, result string
		isError                     bool
		want                        string
	}{
		// The exit status speaks for the check.
		{name: "plain pass", kind: "tests", command: "go test ./...", want: "passed"},
		{name: "plain failure", kind: "tests", command: "go test ./...", isError: true, want: "failed"},
		{name: "chained", kind: "tests", command: "go test ./... && git push", want: "passed"},
		{name: "chain ending in a pipe", kind: "tests", command: "go test ./... && git status | head -3", want: "passed"},
		{name: "redirect", kind: "tests", command: "go test ./... > /tmp/out 2>&1", want: "passed"},
		{name: "pipefail", kind: "tests", command: "set -o pipefail; go test ./... | tail -3", want: "passed"},
		{name: "set -e", kind: "tests", command: "set -e; go test ./...; git status", want: "passed"},
		{name: "trailing semicolon", kind: "tests", command: "go test ./...;", want: "passed"},
		{name: "quoted pipe", kind: "tests", command: `go test -run "A|B" ./...`, want: "passed"},

		// Hidden behind a pipe: failures the exit status doesn't show.
		{name: "pytest", kind: "tests", command: "python3 -m pytest -q 2>&1 | tail -4", result: "FAILED tests/test_live.py::test_poll\n3 failed, 115 passed in 1.12s", want: "failed"},
		{name: "go, last line", kind: "tests", command: "go test ./... 2>&1 | tail -30", result: "ok  \tx/web\t1.995s\nok  \tx/wrap\t(cached)\nFAIL", want: "failed"},
		{name: "go, one test", kind: "tests", command: "go test -run TestX ./internal/web/ 2>&1 | tail -15", result: "--- FAIL: TestX (0.01s)\nFAIL\nFAIL\tx/web\t0.344s", want: "failed"},
		{name: "vitest", kind: "tests", command: "pnpm vitest run 2>&1 | tail -3", result: "      Tests  1 failed | 5 passed (6)", want: "failed"},
		{name: "playwright lines", kind: "tests", command: "pnpm test 2>&1 | tail -3", result: "  3 failed\n  13 did not run\n  37 passed (14.1s)", want: "failed"},
		{name: "node test runner", kind: "tests", command: "npm test 2>&1 | head -10", result: "ℹ tests 43\nℹ pass 39\nℹ fail 4", want: "failed"},
		{name: "or true", kind: "tests", command: "go test ./... || true", result: "FAIL\tx/web\t0.3s\nFAIL", want: "failed"},
		{name: "failed, filter hides ok", kind: "tests", command: `go test ./... 2>&1 | grep -E "^(ok|FAIL)\s"`, result: "FAIL\tx/a\t0.3s\nok  \tx/b\t0.2s", want: "failed"},
		{name: "exit printed", kind: "tests", command: `pnpm test > log 2>&1; echo "exit=$?"`, result: "exit=1", want: "failed"},

		// Hidden, but the output settles it.
		{name: "vitest pass", kind: "tests", command: "pnpm vitest run 2>&1 | tail -3", result: " Test Files  3 passed (3)\n      Tests  61 passed (61)", want: "passed"},
		{name: "go pass", kind: "tests", command: "go test ./... 2>&1 | tail -3", result: "ok  \tx/web\t1.995s", want: "passed"},
		{name: "summary survives head", kind: "tests", command: "npx vitest run 2>&1 | tail -3 | head -1", result: "      Tests  8 passed (8)", want: "passed"},
		{name: "node pass", kind: "tests", command: "npm test 2>&1 | tail -4", result: "ℹ tests 43\nℹ pass 43\nℹ fail 0", want: "passed"},
		{name: "failures filtered in, none", kind: "tests", command: `go test ./... 2>&1 | grep -E "^(FAIL|---)"`, want: "passed"},
		{name: "ok filtered out, nothing left", kind: "tests", command: `go test -race ./... 2>&1 | grep -v "^ok\|no test files" | tail -30; echo "suite done"`, result: "suite done", want: "passed"},
		{name: "exit zero printed", kind: "tests", command: `pnpm test > log 2>&1; echo "exit=$?"`, result: "exit=0", want: "passed"},
		{name: "another script failed", kind: "tests", command: "pnpm lint; pnpm test 2>&1 | tail -3", result: "ELIFECYCLE Command failed with exit code 1.\n      Tests  37 passed (37)", want: "passed"},

		// Hidden, and the output doesn't say.
		{name: "package lines cut short", kind: "tests", command: "go test ./... 2>&1 | head -3", result: "ok  \tx/a\t1s\nok  \tx/b\t1s", want: "unclear"},
		{name: "filtered to nothing", kind: "tests", command: "go test ./... 2>&1 | grep coverage", want: "unclear"},
		{name: "tail's exit status", kind: "tests", command: `go test ./... | tail -3; echo "exit=$?"`, result: "exit=0", want: "unclear"},
		{name: "left running", kind: "tests", command: "pnpm test", result: "Command running in background with ID: b1. You will be notified when it completes.", want: "unclear"},
		{name: "build left running", kind: "build", command: "xcodebuild -scheme App", result: "Command running in background with ID: b1.", want: "unclear"},

		// Linters and builds.
		{name: "vet silent", kind: "lint", command: "go vet ./... 2>&1 | tail -5", want: "passed"},
		{name: "vet finding", kind: "lint", command: "go vet ./... 2>&1 | tail -5", result: "internal/web/web.go:12:3: unreachable code", want: "failed"},
		{name: "eslint warnings only", kind: "lint", command: "npx eslint . 2>&1 | tail -5", result: "✖ 58 problems (0 errors, 58 warnings)\n  0 errors and 15 warnings potentially fixable", want: "passed"},
		{name: "eslint errors", kind: "lint", command: "npx eslint . 2>&1 | tail -5", result: "✖ 22 problems (20 errors, 2 warnings)", want: "failed"},
		{name: "gofmt lists a file", kind: "lint", command: "gofmt -l .", result: "internal/web/web.go", want: "failed"},
		{name: "gofmt silent", kind: "lint", command: "gofmt -l . && go vet ./...", want: "passed"},
		{name: "gofmt then tests", kind: "lint", command: "gofmt -l . && go vet ./... && go test ./...", result: "ok  \tx/web\t1.9s", want: "passed"},
		{name: "prettier", kind: "lint", command: "npx prettier --check . 2>&1 | tail -2", result: "[warn] Code style issues found in 7 files.", want: "failed"},
		{name: "typecheck filtered to nothing", kind: "lint", command: `pnpm typecheck 2>&1 | grep -E "error" | head -30`, result: "(Bash completed with no output)", want: "passed"},
		{name: "tsc", kind: "lint", command: "npx tsc --noEmit 2>&1 | tail -3", result: "src/a.tsx(132,22): error TS2745: This JSX tag's 'children' prop", want: "failed"},
		{name: "a test failure isn't vet's", kind: "lint", command: "go vet ./... && go test ./...", result: "--- FAIL: TestX (0.01s)\nFAIL", isError: true, want: "passed"},
		{name: "exit printed for tests", kind: "lint", command: `go vet ./... && go test ./... > log; echo "exit=$?"; grep FAIL log`, result: "exit=1\n--- FAIL: TestX (0.08s)\nFAIL", want: "passed"},
		{name: "clock time", kind: "build", command: "pnpm build 2>&1 | tail -3", result: "20:20:07 [build] 34 page(s) built in 605ms\n20:20:07 [build] Complete!", want: "passed"},
		{name: "playwright line isn't a diagnostic", kind: "build", command: "pnpm build 2>&1 | tail -3; pnpm e2e", result: "e2e/console.spec.ts:184:1 › the page sends a nonce-based policy", want: "passed"},
		{name: "next", kind: "build", command: "npx next build 2>&1 | tail -40", result: "> Build failed because of webpack errors", want: "failed"},
		{name: "build cut short", kind: "build", command: "pnpm build 2>&1 | head -5", result: "> site@1.0 build\n> astro build", want: "unclear"},
		{name: "go build silent", kind: "build", command: "go build ./... 2>&1 | head -20", want: "passed"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := statusOf(c.kind, c.command, c.result, c.isError); got != c.want {
				t.Errorf("%s: %q → %s, want %s", c.kind, c.command, got, c.want)
			}
		})
	}
}

// A failure the agent trimmed away with tail still fails the receipt, so
// its passed count and earlier-failure tally stay honest.
func TestReceiptSeesThroughAPipe(t *testing.T) {
	f := newFeed(api.AgentClaude, "/repo", nil, time.UTC, 100)
	f.add(&item{ActivityItem: api.ActivityItem{ID: "p", Kind: api.ActivityPrompt, At: t0, Turn: 1}})
	run := func(id string, m int, result string) {
		f.add(&item{ActivityItem: api.ActivityItem{ID: id, Kind: api.ActivityTool, ToolKind: api.ToolShell, At: t0.Add(time.Duration(m) * time.Minute), Result: result},
			command: "pnpm vitest run 2>&1 | tail -3", fold: true})
	}
	run("a", 1, "Tests  2 failed | 59 passed (61)")
	run("b", 2, "Tests  1 failed | 60 passed (61)")
	rc := f.receipt()
	if rc.Tests == nil || rc.Tests.Status != "failed" || rc.Tests.Count != 0 || rc.Tests.FailedBefore != 1 {
		t.Errorf("failing: %+v", rc.Tests)
	}
	run("c", 3, "Tests  61 passed (61)")
	rc = f.receipt()
	if rc.Tests.Status != "passed" || rc.Tests.Count != 61 || rc.Tests.FailedBefore != 2 {
		t.Errorf("fixed: %+v", rc.Tests)
	}
}

// A long command can push a run's result out of the tail the feed keeps
// for display; the receipt still reads it, and only for the latest turn.
func TestReceiptReadsLongCheckOutput(t *testing.T) {
	f := newFeed(api.AgentClaude, "/repo", nil, time.UTC, 100)
	f.prompt(t0)
	f.add(&item{ActivityItem: api.ActivityItem{ID: "p", Kind: api.ActivityPrompt, At: t0, Turn: 1}})
	f.add(&item{ActivityItem: api.ActivityItem{ID: "t", Kind: api.ActivityTool, ToolKind: api.ToolShell, At: t0.Add(time.Minute), Pending: true},
		command: "go test ./... 2>&1 | tail -3; git log -50"})
	long := "--- FAIL: TestX (0.01s)\nFAIL\n" + strings.Repeat("abc123 a commit subject\n", 100)
	if f.result("t", long, false, nil) == nil {
		t.Fatal("no item")
	}
	if it := f.byID["t"]; strings.Contains(it.Result, "FAIL") {
		t.Fatal("the display tail should have lost it")
	}
	if rc := f.receipt(); rc.Tests == nil || rc.Tests.Status != "failed" {
		t.Errorf("tests = %+v", rc.Tests)
	}
	f.prompt(t0.Add(time.Hour))
	if len(f.checkOutput) != 0 {
		t.Errorf("kept %d outputs past the turn", len(f.checkOutput))
	}
}

// A step in the feed says what the receipt says, so the report's timeline
// can't call a run passed under a verdict that says it failed.
func TestStepCarriesCheckStatus(t *testing.T) {
	cases := []struct{ command, result, want string }{
		{"pnpm vitest run 2>&1 | tail -3", "Tests  1 failed | 60 passed (61)", "failed"},
		{"pnpm vitest run 2>&1 | tail -3", "Tests  61 passed (61)", "passed"},
		{"go test ./... | grep coverage", "", "unclear"},
		{"go vet ./... && go test ./...", "ok  \tx/web\t1.9s", "passed"},
		{"git status", "On branch main", ""},
	}
	for _, c := range cases {
		f := newFeed(api.AgentClaude, "/repo", nil, time.UTC, 100)
		f.add(&item{ActivityItem: api.ActivityItem{ID: "t", Kind: api.ActivityTool, ToolKind: api.ToolShell, At: t0, Pending: true}, command: c.command})
		if it := f.result("t", c.result, false, nil); it == nil || it.CheckStatus != c.want {
			t.Errorf("%q → %+v, want %q", c.command, it, c.want)
		}
	}
}

// Catch-up says what the receipt says.
func TestCatchUpSeesThroughAPipe(t *testing.T) {
	line := func(result string) string {
		f := newFeed(api.AgentClaude, "/repo", nil, time.UTC, 100)
		f.add(&item{ActivityItem: api.ActivityItem{ID: "t", Kind: api.ActivityTool, ToolKind: api.ToolShell, At: t0.Add(time.Minute), Result: result},
			command: "pnpm test 2>&1 | head -3", fold: true})
		for _, l := range f.catchUp(t0, t0.Add(time.Hour), CatchUpExtra{}).Lines {
			if strings.HasPrefix(l, "Tests:") {
				return l
			}
		}
		return ""
	}
	if got := line("Tests  1 failed | 60 passed (61)"); !strings.Contains(got, "failed") {
		t.Errorf("failed run: %q", got)
	}
	if got := line("> app@1.0 test"); !strings.Contains(got, "doesn't show") {
		t.Errorf("unclear run: %q", got)
	}
	if got := line("Tests  61 passed (61)"); !strings.Contains(got, "pnpm test passed") {
		t.Errorf("passed run: %q", got)
	}
}
