package transcript

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sabr-software/pilothouse/internal/api"
)

// ran is one finished shell command in a turn.
type ran struct {
	command, result string
	isError         bool
}

// receiptFor is the receipt of a turn that ran these commands in order.
func receiptFor(cmds ...ran) *api.Receipt {
	f := newFeed(api.AgentClaude, "/repo", nil, time.UTC, 100)
	f.add(&item{ActivityItem: api.ActivityItem{ID: "p", Kind: api.ActivityPrompt, At: t0, Turn: 1}})
	for i, c := range cmds {
		f.add(&item{ActivityItem: api.ActivityItem{ID: "c" + itoa(i), Kind: api.ActivityTool, ToolKind: api.ToolShell,
			At: t0.Add(time.Duration(i+1) * time.Minute), Result: c.result, IsError: c.isError}, command: c.command, fold: true})
	}
	return f.receipt()
}

func checkIs(c *api.Check) string {
	if c == nil {
		return ""
	}
	return c.Status
}

// verdicts is a receipt's three checks as "tests/lint/build" statuses.
func verdicts(rc *api.Receipt) string {
	return checkIs(rc.Tests) + "/" + checkIs(rc.Lint) + "/" + checkIs(rc.Build)
}

// C1: every runner in a command is judged, and the kind's verdict is the
// worst one known. Before, only the first runner of each kind counted.
func TestEveryRunnerIsJudged(t *testing.T) {
	const goFail = "--- FAIL: TestX (0.01s)\nFAIL\nFAIL\tx/api\t0.3s"
	cases := []struct {
		name, command, result string
		isError               bool
		want                  string // tests/lint/build
		step                  string // the step's status in the feed
	}{
		{name: "second linter fails behind a pipe", command: "go vet ./... && staticcheck ./... 2>&1 | tail -20",
			result: "x.go:12:2: func unused is unused (U1000)", want: "/failed/", step: "failed"},
		{name: "second test run fails behind a pipe", command: "cd web && npm test && cd ../api && go test ./... 2>&1 | tail -5",
			result: goFail, want: "failed//", step: "failed"},
		{name: "both linters pass", command: "go vet ./... && staticcheck ./... 2>&1 | tail -20", want: "/passed/", step: "passed"},
		{name: "both test runs pass", command: "cd web && npm test && cd ../api && go test ./... 2>&1 | tail -5",
			result: "ok  \tx/api\t0.3s", want: "passed//", step: "passed"},
		{name: "second run's result is cut off", command: "npm test && go test ./... 2>&1 | head -2",
			result: "ok  \tx/a\t1s\nok  \tx/b\t1s", want: "unclear//", step: "unclear"},
		{name: "a cut run can't borrow another's summary", command: "pnpm vitest run 2>&1 | tail -2; go test ./... 2>&1 | head -1",
			result: "      Tests  8 passed (8)\nok  \tx/a\t1s", want: "unclear//", step: "unclear"},
		{name: "a later filter isn't an earlier run's", command: `go test ./a 2>&1 | grep coverage; go test ./b 2>&1 | grep -E "^(FAIL|---)"`,
			want: "unclear//", step: "unclear"},
		{name: "lint fails behind passing tests", command: "go test ./... 2>&1 | tail -3; staticcheck ./... 2>&1 | tail -3",
			result: "ok  \tx/a\t1s\nx.go:12:2: func unused is unused (U1000)", want: "passed/failed/", step: "failed"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := verdicts(receiptFor(ran{c.command, c.result, c.isError})); got != c.want {
				t.Errorf("receipt: %q → %s, want %s", c.command, got, c.want)
			}
			if got := stepCheck(c.command, c.result, c.isError); got != c.step {
				t.Errorf("step: %q → %s, want %s", c.command, got, c.step)
			}
		})
	}
	// The check is named for the run that decided it.
	rc := receiptFor(ran{"go vet ./... && staticcheck ./... 2>&1 | tail -20", "x.go:12:2: func unused is unused (U1000)", false})
	if rc.Lint == nil || rc.Lint.Name != "staticcheck" {
		t.Errorf("lint = %+v, want staticcheck", rc.Lint)
	}
}

// C2: gofmt -l exits 0 and lists what it would change, however many files.
func TestGofmtListsSeveralFiles(t *testing.T) {
	for _, c := range []struct{ command, result, want string }{
		{"gofmt -l .", "a.go", "failed"},
		{"gofmt -l .", "a.go\nb.go\nc.go\n", "failed"},
		{"gofmt -l . && go vet ./...", "internal/web/a.go\ninternal/web/b.go", "failed"},
		{"go build ./... && gofmt -l .", "internal/web/a.go\ninternal/web/b.go", "failed"},
		{"gofmt -l .", "", "passed"},
		{"gofmt -l . && go test ./...", "ok  \tx/web\t1.9s", "passed"},
		{"gofmt -l $(git ls-files '*.go')", "a.go\nb.go", "failed"},
		// A count of the list is the list.
		{"gofmt -l . | wc -l", "       3", "failed"},
		{"gofmt -l . | wc -l", "0", "passed"},
		// Paths another command in the line may have printed decide nothing.
		{"gofmt -l .; git diff --name-only", "a.go\nb.go", "unclear"},
	} {
		if got := checkIs(receiptFor(ran{c.command, c.result, false}).Lint); got != c.want {
			t.Errorf("%q with %q → %s, want %s", c.command, c.result, got, c.want)
		}
	}
}

// C3: a runner's name is a check only where a command starts, not as
// another command's argument.
func TestRunnerNeedsCommandPosition(t *testing.T) {
	for _, cmd := range []string{
		"pip install pytest",
		"pip install -U pytest mypy ruff",
		"which staticcheck",
		"git checkout -b nox",
		"brew install golangci-lint shellcheck",
		"npm install --save-dev jest eslint",
		"pnpm add -D vitest",
		"man eslint",
		"command -v staticcheck",
		"git log --oneline -- tox",
		"rm -rf node_modules/.cache/jest ",
		"ls ~/go/bin | grep staticcheck",
		"echo run go test ./... next",
	} {
		rc := receiptFor(ran{command: cmd})
		if got := verdicts(rc); got != "//" {
			t.Errorf("%q counted as a check: %s", cmd, got)
		}
		if got := stepCheck(cmd, "", false); got != "" {
			t.Errorf("%q is a %s step", cmd, got)
		}
	}
	// Real runs still count, behind the usual wrappers.
	for cmd, want := range map[string]string{
		"go test ./...":                                  "passed//",
		"cd web && pnpm vitest run":                      "passed//",
		"(cd api && go test ./...)":                      "passed//",
		"CGO_ENABLED=0 go build ./...":                   "//passed",
		"npx eslint .":                                   "/passed/",
		"npx --no-install tsc --noEmit":                  "/passed/",
		"time go test -race ./...":                       "passed//",
		"uv run pytest -q":                               "passed//",
		"poetry run mypy .":                              "/passed/",
		"python3 -m pytest -q":                           "passed//",
		"python -m mypy src":                             "/passed/",
		"bundle exec rspec":                              "passed//",
		"env GOFLAGS=-count=1 go test ./...":             "passed//",
		"timeout 300 go test ./...":                      "passed//",
		"timeout -k 5 300 go test ./...":                 "passed//",
		"timeout --foreground 300 go test ./...":         "passed//",
		"pnpm --filter web exec vitest run":              "passed//",
		"for d in a b; do (cd $d && go vet ./...); done": "/passed/",
		"if ! go vet ./...; then echo no; fi":            "/passed/",
		"git ls-files '*.go' | xargs gofmt -l":           "/passed/",
		"docker compose run --rm api pytest -q":          "passed//",
		"gofmt -l . && go vet ./... && go test ./...":    "passed/passed/",
		"pnpm build && pnpm test":                        "passed//passed",
		"yarn workspace web jest --ci":                   "passed//",
		"npx -p typescript tsc --noEmit":                 "/passed/",
		"uvx ruff check .":                               "/passed/",
		"mise exec -- go test ./...":                     "passed//",
	} {
		if got := verdicts(receiptFor(ran{command: cmd})); got != want {
			t.Errorf("%q → %s, want %s", cmd, got, want)
		}
	}
}

// C4: OpenCode's checks are judged on the command it ran, not on the
// one-line, 160-character summary the feed shows.
func TestOpenCodeJudgesTheWholeCommand(t *testing.T) {
	db, path := openTestOpenCode(t)
	sid := "ses_checks"
	mustExec(t, db, `INSERT INTO session
		(id, cost, tokens_input, tokens_output, tokens_reasoning, tokens_cache_read, tokens_cache_write, model)
		VALUES (?, 0, 0, 0, 0, 0, 0, '{}')`, sid)
	mustExec(t, db, `INSERT INTO message (id, session_id, time_created, time_updated, data) VALUES (?, ?, ?, ?, ?)`,
		"m1", sid, 1000, 1000, `{"role":"user"}`)
	mustExec(t, db, `INSERT INTO message (id, session_id, time_created, time_updated, data) VALUES (?, ?, ?, ?, ?)`,
		"m2", sid, 1100, 1500, `{"role":"assistant"}`)
	part(t, db, "p1", "m1", sid, 1000, 1000, `{"type":"text","text":"fix it"}`)
	// The summary ends inside the test command: its pipe is past the cut.
	long := "cd /Users/someone/Projects/a-rather-long-path/to/the/repository/services/api && GOFLAGS=-count=1 go test ./internal/web/... ./internal/daemon/... ./internal/x/... 2>&1 | tail -5"
	if len(long) <= 160 {
		t.Fatal("command fits the summary")
	}
	tool := func(id string, at int64, command, output string) {
		part(t, db, id, "m2", sid, at, at, fmt.Sprintf(
			`{"type":"tool","tool":"bash","state":{"status":"completed","input":{"command":%q},"output":%q,"metadata":{"exit":0}}}`, command, output))
	}
	tool("p2", 1200, long, "--- FAIL: TestX (0.01s)\nFAIL\nFAIL\tx/web\t0.3s")
	// Two lines: the second command's exit status is not the linter's.
	tool("p3", 1300, "staticcheck ./...\necho done", "x.go:12:2: func unused is unused (U1000)\ndone")
	res, err := ReadOpenCode(path, sid, "/repo", "", 10, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if res.Receipt == nil || checkIs(res.Receipt.Tests) != "failed" || checkIs(res.Receipt.Lint) != "failed" {
		t.Errorf("receipt: tests %+v lint %+v", res.Receipt.Tests, res.Receipt.Lint)
	}
	if res.Receipt.Tests != nil && res.Receipt.Tests.Command != long {
		t.Errorf("command = %q", res.Receipt.Tests.Command)
	}
	for _, it := range res.Items {
		if it.ToolKind == api.ToolShell && it.CheckStatus != "failed" {
			t.Errorf("step %s (%s) = %q, want failed", it.ID, it.Summary, it.CheckStatus)
		}
	}
}

// C5: failures a pipe hides, as the tools around a check print them.
func TestHiddenFailuresAreRecognised(t *testing.T) {
	for _, c := range []struct{ kind, command, result, want string }{
		{"build", "make build 2>&1 | tail -1", "make: *** [Makefile:12: build] Error 2", "failed"},
		{"build", "make build 2>&1 | tail -1", "make[1]: *** [web/Makefile:4: all] Error 1", "failed"},
		{"tests", "make test 2>&1 | tail -1", "make: *** [Makefile:9: test] Error 1", "failed"},
		{"build", "go build ./... 2>&1 | tail -3", "go: cannot find main module, but found .git/config in /repo", "failed"},
		{"build", "go build ./... 2>&1 | tail -3", "go: go.mod file not found in current directory or any parent directory; see 'go help modules'", "failed"},
		{"lint", "go vet ./... 2>&1 | tail -5", "# x/web\nvet: ./web.go:12:3: undefined: handler", "failed"},
		{"lint", "npm run lint 2>&1 | tail -3", "npm error Lifecycle script `lint` failed with error:\nnpm error code 1", "failed"},
		{"lint", "npm run lint 2>&1 | tail -3", `npm error Missing script: "lint"`, "failed"},
		{"build", "npm run build 2>&1 | tail -3", "npm error code ELIFECYCLE", "failed"},
		// A check that never ran didn't pass.
		{"lint", "staticcheck ./... 2>&1 | tail -5", "zsh: command not found: staticcheck", "failed"},
		{"lint", "golangci-lint run 2>&1 | tail -5", "bash: golangci-lint: command not found", "failed"},
		{"lint", "npm run lint 2>&1 | tail -5", "sh: 1: eslint: not found", "failed"},
		// The Go tool's progress lines are not failures.
		{"build", "go build ./... 2>&1 | tail -3", "go: downloading golang.org/x/sys v0.30.0\ngo: finding module for package x/y", "passed"},
		{"lint", "go vet ./... 2>&1 | tail -3", "go: downloading golang.org/x/tools v0.30.0", "passed"},
		{"build", "go build ./... 2>&1 | tail -3", "go: x/y@v1.2.3 requires go >= 1.24; switching to go1.24.2", "passed"},
		{"tests", "go test ./... 2>&1 | tail -3", "go: downloading github.com/x/y v1.2.3\nok  \tx/web\t1.9s", "passed"},
		// A failure belongs to the checks its tool ran, not to its neighbours.
		{"tests", "go test ./... 2>&1 | tail -3; foo", "ok  \tx/web\t1.9s\nzsh: command not found: foo", "passed"},
		{"tests", "make test 2>&1 | tail -3", "ok  \tx/web\t1.9s\nmake: *** [Makefile:9: test] Error 1", "failed"},
		{"tests", "go test ./... 2>&1 | tail -3; make build 2>&1 | tail -1", "ok  \tx/web\t1.9s\nmake: *** [Makefile:12: build] Error 2", "passed"},
		{"build", "go test ./... 2>&1 | tail -3; make build 2>&1 | tail -1", "ok  \tx/web\t1.9s\nmake: *** [Makefile:12: build] Error 2", "failed"},
		{"lint", "go vet ./... 2>&1 | tail -3; npm run build 2>&1 | tail -3", "npm error code ELIFECYCLE", "passed"},
		{"build", "go vet ./... 2>&1 | tail -3; npm run build 2>&1 | tail -3", "npm error code ELIFECYCLE", "failed"},
		{"lint", "npx eslint . 2>&1 | tail -3", "npm error could not determine executable to run", "failed"},
	} {
		rc := receiptFor(ran{c.command, c.result, false})
		got := map[string]string{"tests": checkIs(rc.Tests), "lint": checkIs(rc.Lint), "build": checkIs(rc.Build)}[c.kind]
		if got != c.want {
			t.Errorf("%s: %q with %q → %s, want %s", c.kind, c.command, c.result, got, c.want)
		}
	}
}

// C6: a check whose result never arrived (an over-long line, an
// interrupted call) did not pass: nobody saw how it ended.
func TestLostResultIsUnclear(t *testing.T) {
	f := newFeed(api.AgentClaude, "/repo", nil, time.UTC, 100)
	f.prompt(t0)
	f.add(&item{ActivityItem: api.ActivityItem{ID: "p", Kind: api.ActivityPrompt, At: t0, Turn: 1}})
	f.add(&item{ActivityItem: api.ActivityItem{ID: "t", Kind: api.ActivityTool, ToolKind: api.ToolShell, At: t0.Add(time.Minute), Pending: true},
		command: "go test ./... && go vet ./..."})
	f.add(&item{ActivityItem: api.ActivityItem{ID: "g", Kind: api.ActivityTool, ToolKind: api.ToolShell, At: t0.Add(time.Minute), Pending: true},
		command: "git status"})
	if rc := f.receipt(); verdicts(rc) != "running/running/" {
		t.Fatalf("waiting: %s", verdicts(rc))
	}
	f.settle("") // the agent moved on without the result
	rc := f.receipt()
	if got := verdicts(rc); got != "unclear/unclear/" {
		t.Errorf("receipt: %s, want unclear/unclear/", got)
	}
	if it := f.byID["t"]; it.Pending || it.CheckStatus != "unclear" {
		t.Errorf("step: pending %v, status %q", it.Pending, it.CheckStatus)
	}
	if it := f.byID["g"]; it.CheckStatus != "" {
		t.Errorf("git status is a %q check", it.CheckStatus)
	}
	var line string
	for _, l := range f.catchUp(t0, t0.Add(time.Hour), CatchUpExtra{}).Lines {
		if strings.HasPrefix(l, "Tests:") {
			line = l
		}
	}
	if !strings.Contains(line, "doesn't show") {
		t.Errorf("catch-up: %q", line)
	}
	// A result that did arrive still counts.
	f.add(&item{ActivityItem: api.ActivityItem{ID: "u", Kind: api.ActivityTool, ToolKind: api.ToolShell, At: t0.Add(2 * time.Minute), Pending: true},
		command: "go test ./..."})
	f.result("u", "ok  \tx/web\t1.9s", false, nil)
	f.settle("")
	if rc := f.receipt(); checkIs(rc.Tests) != "passed" {
		t.Errorf("finished run: %+v", rc.Tests)
	}
}

// C7: an edit a subagent made after the tests ran makes them stale, like
// the agent's own.
func TestSubagentEditsMakeChecksStale(t *testing.T) {
	f := newFeed(api.AgentClaude, "/repo", nil, time.UTC, 100)
	f.add(&item{ActivityItem: api.ActivityItem{ID: "p", Kind: api.ActivityPrompt, At: t0, Turn: 1}})
	f.add(&item{ActivityItem: api.ActivityItem{ID: "t", Kind: api.ActivityTool, ToolKind: api.ToolShell, At: t0.Add(time.Minute), Result: "ok  \tx\t0.2s"},
		command: "go test ./...", fold: true})
	if rc := f.receipt(); rc.Tests == nil || !rc.Tests.Fresh {
		t.Fatalf("before the edit: %+v", rc.Tests)
	}
	edit := func(id string, m int, isError bool) {
		f.add(&item{ActivityItem: api.ActivityItem{ID: id, Kind: api.ActivityTool, ToolKind: api.ToolEdit, At: t0.Add(time.Duration(m) * time.Minute),
			Sidechain: true, Turn: 1, IsError: isError}, edits: []fileEdit{{"/repo/x.go", api.FileEdit, 2, 1}}, fold: true, chain: "agent-1"})
	}
	edit("failed", 2, true) // an edit that didn't happen changes nothing
	if rc := f.receipt(); !rc.Tests.Fresh {
		t.Errorf("a failed subagent edit made the tests stale")
	}
	edit("e", 3, false)
	rc := f.receipt()
	if rc.Tests.Fresh {
		t.Errorf("tests are fresh after a subagent's edit: %+v", rc.Tests)
	}
	if rc.LastEdit == nil || !rc.LastEdit.Equal(t0.Add(3*time.Minute)) {
		t.Errorf("last edit = %v", rc.LastEdit)
	}

	// The same from feed items alone (OpenCode, the demo).
	items := []api.ActivityItem{
		{Kind: api.ActivityPrompt, At: t0, Turn: 1},
		{Kind: api.ActivityTool, ToolKind: api.ToolShell, At: t0.Add(time.Minute), Summary: "Run: npm test", Result: "Tests: 12 passed, 12 total"},
		{Kind: api.ActivityTool, ToolKind: api.ToolEdit, At: t0.Add(2 * time.Minute), Summary: "Edit src/a.ts", Sidechain: true},
	}
	if rc := ReceiptOf(items, nil); rc.Tests == nil || rc.Tests.Fresh {
		t.Errorf("items: tests = %+v", rc.Tests)
	}
}

// C8: a turn of commands that write megabyte files through heredocs is
// read in the time its short command lines take, not its payloads.
func TestReceiptSkipsLargePayloads(t *testing.T) {
	f := newFeed(api.AgentClaude, "/repo", nil, time.UTC, 100)
	f.add(&item{ActivityItem: api.ActivityItem{ID: "p", Kind: api.ActivityPrompt, At: t0, Turn: 1}})
	payload := strings.Repeat("a line of generated fixture data, like any other\n", 1<<20/49)
	for i := 0; i < 60; i++ {
		f.add(&item{ActivityItem: api.ActivityItem{ID: "w" + itoa(i), Kind: api.ActivityTool, ToolKind: api.ToolShell, At: t0.Add(time.Duration(i+1) * time.Second)},
			command: "cat > testdata/fixture" + itoa(i) + ".txt <<'EOF'\n" + payload + "EOF", fold: true})
	}
	f.add(&item{ActivityItem: api.ActivityItem{ID: "c", Kind: api.ActivityTool, ToolKind: api.ToolShell, At: t0.Add(2 * time.Minute)},
		command: "git commit -q -F - <<'EOF'\nfixtures: regenerate\n\n" + payload + "EOF", fold: true})
	limit := 500 * time.Millisecond
	if raceEnabled {
		limit *= 5
	}
	var rc *api.Receipt
	took := time.Hour
	for try := 0; try < 3 && took > limit; try++ { // a busy machine gets another go
		start := time.Now()
		rc = f.receipt()
		took = time.Since(start)
	}
	if took > limit {
		t.Errorf("receipt took %v for 61 MB of heredocs, want under %v", took, limit)
	}
	if rc.Commit != "fixtures: regenerate" {
		t.Errorf("commit = %q", rc.Commit)
	}
}

// C9: a turn longer than the feed keeps its prompt, its counts and its
// early checks in the receipt.
func TestReceiptOutlivesTheFeedWindow(t *testing.T) {
	f := newFeed(api.AgentClaude, "/repo", nil, time.UTC, 20)
	n := 0
	add := func(it *item) {
		n++
		it.ID, it.At = "i"+itoa(n), t0.Add(time.Duration(n)*time.Second)
		f.add(it)
	}
	shell := func(command, result string, isError bool) {
		add(&item{ActivityItem: api.ActivityItem{Kind: api.ActivityTool, ToolKind: api.ToolShell, Result: result, IsError: isError}, command: command, fold: true})
	}
	// An earlier turn, long gone by the end.
	f.prompt(t0)
	add(&item{ActivityItem: api.ActivityItem{Kind: api.ActivityPrompt, Turn: 1}})
	shell("pnpm build", "", false)
	add(&item{ActivityItem: api.ActivityItem{Kind: api.ActivityTool, ToolKind: api.ToolEdit}, edits: []fileEdit{{"/repo/old.go", api.FileEdit, 9, 9}}, fold: true})

	f.prompt(t0.Add(time.Minute))
	add(&item{ActivityItem: api.ActivityItem{Kind: api.ActivityPrompt, Turn: 2}})
	since := t0.Add(time.Duration(n) * time.Second)
	shell("go test ./...", "--- FAIL: TestX (0.01s)\nFAIL", true)
	shell("go vet ./...", "", false)
	add(&item{ActivityItem: api.ActivityItem{Kind: api.ActivityTool, ToolKind: api.ToolEdit}, edits: []fileEdit{{"/repo/x.go", api.FileEdit, 5, 2}}, fold: true})
	lastEdit := t0.Add(time.Duration(n) * time.Second)
	shell("go test ./...", "ok  \tx/web\t1.9s", false)
	shell(`git commit -am "web: fix x"`, "", false)
	want := f.receipt()
	if want.Turn != 2 || verdicts(want) != "passed/passed/" || want.Edits != 1 {
		t.Fatalf("before the window moves: %+v", want)
	}
	for i := 0; i < 50; i++ { // a long tail of reads pushes all of it out
		add(&item{ActivityItem: api.ActivityItem{Kind: api.ActivityTool, ToolKind: api.ToolRead, Tool: "Read"}, fold: true})
	}
	if len(f.items) != 20 {
		t.Fatalf("feed kept %d items", len(f.items))
	}
	rc := f.receipt()
	if rc.Turn != 2 || !rc.Since.Equal(since) {
		t.Errorf("turn %d since %v, want 2 since %v", rc.Turn, rc.Since, since)
	}
	if rc.Tests == nil || rc.Tests.Status != "passed" || rc.Tests.FailedBefore != 1 || !rc.Tests.Fresh {
		t.Errorf("tests = %+v", rc.Tests)
	}
	if rc.Lint == nil || rc.Lint.Status != "passed" || rc.Lint.Fresh {
		t.Errorf("lint = %+v, want passed before the last edit", rc.Lint)
	}
	if rc.Build != nil {
		t.Errorf("build = %+v: that was the turn before", rc.Build)
	}
	if rc.Edits != 1 || rc.Files != 1 || rc.Added != 5 || rc.Removed != 2 || rc.Reads != 50 || rc.Commit != "web: fix x" {
		t.Errorf("counts = %+v", rc)
	}
	if rc.LastEdit == nil || !rc.LastEdit.Equal(lastEdit) {
		t.Errorf("last edit = %v, want %v", rc.LastEdit, lastEdit)
	}
	// The next turn starts clean.
	f.prompt(t0.Add(time.Hour))
	add(&item{ActivityItem: api.ActivityItem{Kind: api.ActivityPrompt, Turn: 3}})
	if rc := f.receipt(); rc.Turn != 3 || verdicts(rc) != "//" || rc.Edits != 0 || rc.Reads != 0 {
		t.Errorf("next turn: %+v", rc)
	}
}

// C9: when a chain of checks fails, the ones that demonstrably ran and
// passed before the failure are not blamed for it, and the ones that never
// ran are not called passed.
func TestFailedChainBlamesTheRightCheck(t *testing.T) {
	const vetFail = "# x/web\n# [x/web]\n./web.go:12:3: unreachable code"
	for _, c := range []struct {
		name, command, result, want string
	}{
		{"only vet fails", "go build ./... && go test ./... && go vet ./...", "ok  \tx/web\t1.9s\nok  \tx/api\t0.4s\n" + vetFail, "passed/failed/passed"},
		{"tests fail, vet never runs", "go build ./... && go test ./... && go vet ./...", "ok  \tx/api\t0.4s\n--- FAIL: TestX (0.01s)\nFAIL\nFAIL\tx/web\t0.3s", "failed/unclear/passed"},
		{"tests fail after vet", "go vet ./... && go test ./...", "--- FAIL: TestX (0.01s)\nFAIL", "failed/passed/"},
		{"tests fail before vet", "go test ./... && go vet ./...", "--- FAIL: TestX (0.01s)\nFAIL", "failed/unclear/"},
		{"nothing says which", "go build ./... && go test ./... && go vet ./...", "signal: killed", "failed/failed/failed"},
		{"build fails first", "go build ./... && go test ./... && go vet ./...", "# x/web\n./web.go:12:3: undefined: handler", "failed/failed/failed"},
		{"a lone failure is its own", "pnpm test --filter webhooks", "expected 3 attempts, got 5", "failed//"},
		{"partial passes aren't a pass", "go test ./...", "ok  \tx/api\t0.4s\nCommand timed out after 2m 0s", "failed//"},
		{"a later command's exit status", "go test ./... 2>&1 | tail -3; go vet ./...", "--- FAIL: TestX (0.01s)\nFAIL", "failed/failed/"},
		{"alone, whatever it printed", "staticcheck ./...", "panic: runtime error: index out of range", "/failed/"},
		{"another command's test failure", "go vet ./... && ./scripts/e2e.sh", "2 failed, 14 passed", "/unclear/"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := verdicts(receiptFor(ran{c.command, c.result, true})); got != c.want {
				t.Errorf("%q → %s, want %s", c.command, got, c.want)
			}
		})
	}
}
