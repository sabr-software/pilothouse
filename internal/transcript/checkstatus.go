package transcript

import (
	"regexp"
	"sort"
	"strings"
)

// A transcript records one exit status per shell command, and that status
// only speaks for a check when nothing after the check decides it. Agents
// usually trim what they run ("go test ./... 2>&1 | tail -5"), and then the
// status is tail's: a failed run reads as clean. For those commands the
// check's own output decides, and when it can't, the result is "unclear"
// rather than a pass nobody saw.
//
// A command often holds several checks ("go vet ./... && staticcheck ./...
// | tail"). Each is judged on what the one status and the one output can
// say about it, and a kind's verdict is the worst of its runs.

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
	// wholeStage is a pipe stage that passes a run's last lines on as they
	// are; any other may drop them (grep, awk, wc).
	wholeStage = regexp.MustCompile(`^\s*(?:tail|tee|cat)\b`)

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
	toolFailure = regexp.MustCompile(`(?mi)^[\w./@~+-]+\.\w+:\d+:\d+: |^[\w./@~+-]+\.\w+:\d+: error:|^[\w./@~+-]+\.\w+\(\d+,\d+\): |^\s*\d+:\d+\s+error\b|\berror TS\d+|^error(?:\[\w+\])?:|^vet: |\b[1-9]\d* errors?\b|\bbuild failed\b|\bfailed to compile\b|\bBUILD FAILURE\b|could not compile|Code style issues|\bSC\d{4}\b`)
	// A run failure is whatever ran a check reporting that it failed, or
	// that it never started. It doesn't say which script, so it never
	// outweighs a check's own result, and it counts against the checks its
	// tool ran: a package manager's script error (scriptFailure), make's
	// (makeFailure), the go command's (goMessage), the shell's "command
	// not found" (notFound, which names the command).
	scriptFailure = regexp.MustCompile(`(?m)\bELIFECYCLE\b|npm ERR!|^npm error\b|ERR_PNPM_|error Command failed`)
	scriptRunner  = regexp.MustCompile(`^(?:npm|pnpm|yarn|bun)\b`)
	makeFailure   = regexp.MustCompile(`(?m)^g?make(?:\[\d+\])?: \*\*\* `)
	makeRunner    = regexp.MustCompile(`^g?make\b`)
	notFound      = regexp.MustCompile(`(?m)^\S*sh: command not found: (\S+)\s*$|^\S*sh: (?:line \d+: |\d+: )?(\S+): (?:command )?not found\s*$`)
	// goMessage is a line from the go command itself. It reports progress
	// the way it reports errors ("go: downloading …", "go: cannot find main
	// module"), and goProgress is the progress.
	goMessage  = regexp.MustCompile(`(?m)^go: (.*)$`)
	goProgress = regexp.MustCompile(`^(?:downloading|finding|extracting|verifying|found|added|upgraded|downgraded|removed|creating|converting|to add|warning|writing|no module dependencies|switching|using|trying)\b|; switching to go\S+$|^module \S+ is deprecated`)
	goRunner   = regexp.MustCompile(`^go\s`)

	// exitFailure and exitZero are a status the agent printed itself:
	// "; echo exit=$?".
	exitFailure = regexp.MustCompile(`(?mi)^\s*(?:exit(?:[ _-]?(?:code|status))?|rc)\s*[=:]?\s*[1-9]\d*\s*$`)
	exitZero    = regexp.MustCompile(`(?mi)^\s*(?:exit(?:[ _-]?(?:code|status))?|rc)\s*[=:]?\s*0\s*$`)

	// unformatted is what `gofmt -l` prints: a file per line, exit 0.
	// listsFiles is another command in the line that prints paths that way.
	unformatted = regexp.MustCompile(`(?m)^\S+\.go\s*$`)
	listsFiles  = regexp.MustCompile(`(?:^|[;&\n])\s*(?:ls|find|fd|git\s+(?:diff|show|ls-files))\b[^|;&\n]*(?:$|[;&\n])`)
	// countStage counts gofmt's list instead of showing it: "| wc -l".
	countStage = regexp.MustCompile(`^[^|;&\n]*\|\s*wc\b`)
	someCount  = regexp.MustCompile(`(?m)^\s*[1-9]\d*\s*$`)
	// harnessNote is what an agent's shell tool adds to a command's output.
	harnessNote = regexp.MustCompile(`(?m)^(?:\(Bash completed with no output\)|Shell cwd was reset to .*|\[This command modified .*)$`)

	// commandPrefix is what may stand between the start of a simple command
	// and a check's runner: variable assignments, shell keywords and the
	// wrappers people run tools through. Anything else there makes the
	// runner's name an argument: "pip install pytest", "which staticcheck".
	commandPrefix = regexp.MustCompile(`^\s*(?:(?:` +
		`[A-Za-z_]\w*=\S*` +
		`|[!{]|time(?:\s+-p)?|if|then|elif|else|while|until|do` +
		`|command|exec|nohup|unbuffer` +
		`|(?:sudo|env|nice|caffeinate|xargs|stdbuf)(?:\s+-\S+(?:\s+\d+)?)*` +
		`|timeout(?:\s+(?:-[ks]\s+\S+|-\S+))*\s+\S+` +
		`|(?:npx|bunx|uvx)(?:\s+(?:(?:-p|--package|--from)(?:=\S+|\s+\S+)|-\S+))*` +
		`|(?:npm|pnpm|yarn|bun)(?:\s+(?:(?:--filter|--dir|--prefix|--cwd|--workspace|-[FCw])(?:=\S+|\s+\S+)|-\S+))*(?:\s+workspace\s+\S+)?(?:\s+(?:exec|run|x|dlx))?(?:\s+--)?` +
		`|(?:uv|poetry|pipenv|pdm|hatch|rye|pipx)\s+run(?:\s+-\S+)*` +
		`|bundle\s+exec` +
		`|python[\d.]*\s+-m` +
		`|(?:docker|podman)(?:\s+compose|-compose)?\s+(?:run|exec)\s.*` +
		`|(?:mise|direnv|devbox|doppler|dotenv|dotenvx|op|aws-vault)\s(?:.*\s)?--` +
		`|find\s.*\s-exec(?:dir)?` +
		`)\s+)*$`)
)

// toolFailed reads a linter's or a build's failure off a command's output.
func toolFailed(out string) bool { return toolFailure.MatchString(out) }

// runFailures says, for each check in a command, whether the output has a
// run failure that is its own. A failure belongs to the checks its tool
// ran ("make: ***" to "make test"), and when the command has no such
// check, to any of them: something they ran printed it.
func runFailures(out string, rs []runner) []bool {
	failed := make([]bool, len(rs))
	blame := func(own func(name string) bool, orElse func(name string) bool) {
		owned := false
		for i, r := range rs {
			if own(r.name) {
				failed[i], owned = true, true
			}
		}
		for i, r := range rs {
			if !owned && orElse(r.name) {
				failed[i] = true
			}
		}
	}
	anyone := func(string) bool { return true }
	if scriptFailure.MatchString(out) {
		blame(scriptRunner.MatchString, anyone)
	}
	if makeFailure.MatchString(out) {
		blame(makeRunner.MatchString, anyone)
	}
	for _, m := range goMessage.FindAllStringSubmatch(out, -1) {
		if !goProgress.MatchString(m[1]) {
			blame(goRunner.MatchString, anyone)
			break
		}
	}
	for _, m := range notFound.FindAllStringSubmatch(out, -1) {
		missing := m[1] + m[2]
		// A command nobody asked for by name was a script's: npm's, make's.
		blame(func(name string) bool { return strings.Fields(name)[0] == missing },
			func(name string) bool { return scriptRunner.MatchString(name) || makeRunner.MatchString(name) })
	}
	return failed
}

// runner is one check in a command: its kind, its name as people say it,
// and where that name starts and ends in the command's head.
type runner struct {
	kind, name string
	start, end int
}

// judged is a check with its result in a step.
type judged struct {
	runner
	status string
}

// checkKinds is the order checks are reported in.
var checkKinds = []string{"tests", "lint", "build"}

// runnersIn finds every check in a command's head, in the order they run.
func runnersIn(head string) []runner {
	var rs []runner
	for _, kind := range checkKinds {
		re := checkCommands[kind]
		for pos := 0; pos < len(head); {
			m := re.FindStringSubmatchIndex(head[pos:])
			if m == nil {
				break
			}
			start, end := pos+m[2], pos+m[3]
			// The simple command the name is in starts after the last
			// separator before it.
			from := strings.LastIndexAny(head[:start], ";&|()\n`") + 1
			if commandPrefix.MatchString(head[from:start]) {
				rs = append(rs, runner{kind, strings.Join(strings.Fields(head[start:end]), " "), start, end})
			}
			pos = end
		}
	}
	sort.SliceStable(rs, func(i, j int) bool { return rs[i].start < rs[j].start })
	return rs
}

// exitView is what a command's exit status can say about a check in it.
type exitView struct {
	hidden bool // something after the check decides the status
	piped  bool // the check's output goes through a pipe
	cut    bool // that pipe's head may have dropped the output's end
	sifted bool // that pipe may have dropped any of the output
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
				v.sifted = v.sifted || !wholeStage.MatchString(rest[i+1:])
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

// checks judges every check in the step's command, in the order they run.
func (s step) checks() []judged {
	head := commandHead(s.command)
	rs := runnersIn(head)
	if len(rs) == 0 {
		return nil
	}
	js := make([]judged, len(rs))
	for i, r := range rs {
		js[i].runner = r
	}
	all := func(status string) []judged {
		for i := range js {
			js[i].status = status
		}
		return js
	}
	out := s.result
	switch {
	case s.pending:
		return all(checkRunning)
	case s.lost:
		return all(checkUnclear)
	case !s.isError && backgrounded.MatchString(out):
		return all(checkUnclear)
	}
	// A command that was cut short ("…") may go on to pipe or chain its
	// checks: its exit status speaks for none of them.
	clipped := len(head) == len(s.command) && strings.HasSuffix(strings.TrimSpace(s.command), "…")
	views := make([]exitView, len(rs))
	hiddenTests := 0
	for i, r := range rs {
		views[i] = viewExit(head, r.end)
		if clipped {
			views[i].hidden, views[i].cut = true, true
		}
		if r.kind == "tests" && views[i].hidden {
			hiddenTests++
		}
	}
	ran := runFailures(out, rs)
	var open []int // the checks a failed exit status speaks for
	for i, r := range rs {
		v := views[i]
		if r.name == "gofmt -l" {
			if js[i].status = s.gofmtFromOutput(head, r); js[i].status != "" {
				continue
			}
		}
		switch {
		case v.hidden && r.kind == "tests":
			// A filter after this run and before the next one is this run's.
			to := len(head)
			for _, next := range rs[i+1:] {
				if next.kind == "tests" {
					to = next.start
					break
				}
			}
			js[i].status = s.testsFromOutput(v, s.command[r.end:to], hiddenTests == 1, ran[i])
		case v.hidden:
			js[i].status = s.toolFromOutput(r.kind, v, ran[i])
		case s.isError:
			open = append(open, i)
		default:
			js[i].status = checkPassed
		}
	}
	if len(open) > 0 {
		// Alone in its command, a check's failure is its own whatever it
		// printed.
		chained := strings.ContainsAny(strings.NewReplacer(">&", "", "&>", "").Replace(strings.TrimSpace(head)), ";&|\n")
		s.blame(js, open, hiddenTests > 0, chained)
	}
	return js
}

// gofmtFromOutput judges a `gofmt -l`, which exits 0 and lists what it
// would change, or returns "" when the output has no such list.
func (s step) gofmtFromOutput(head string, r runner) string {
	out := s.result
	switch {
	case unformatted.MatchString(out):
		if listsFiles.MatchString(head) {
			return checkUnclear // the paths may be that command's
		}
		return checkFailed
	case countStage.MatchString(head[r.end:]) && someCount.MatchString(out):
		if someCount.FindString(out) == out {
			return checkFailed
		}
		return checkUnclear // a count, and other output around it
	}
	return ""
}

// blame settles the checks a failed exit status speaks for. They run
// fail-fast (&&, set -e, or last in the command), so one command failed:
// what ran before it passed, and what comes after it never ran.
func (s step) blame(js []judged, open []int, hiddenTests, chained bool) {
	out := s.result
	testFail, toolFail := testFailure.MatchString(out), toolFailed(out)
	firstTest, toolAfter := -1, false
	for n, i := range open {
		switch {
		case js[i].kind != "tests":
			toolAfter = toolAfter || firstTest >= 0
		case firstTest < 0:
			firstTest = n
		}
	}
	set := func(is []int, status string) {
		for _, i := range is {
			js[i].status = status
		}
	}
	// When nothing says which one failed, the failure is each one's.
	set(open, checkFailed)
	switch {
	case testFail && !toolFail && firstTest >= 0:
		// "go vet ./... && go test ./...": a test failure means vet passed.
		// A check after the tests may never have run.
		set(open[:firstTest], checkPassed)
		for _, i := range open[firstTest:] {
			if js[i].kind != "tests" {
				js[i].status = checkUnclear
			}
		}
	case testFail && !toolFail && !hiddenTests && chained:
		// A test failure and no test run here to pin it on: something else
		// in the command printed it, before these checks or after them.
		set(open, checkUnclear)
	case !testFail && toolFail && toolAfter && !hiddenTests && (testSummary.MatchString(out) || testPackage.MatchString(out)):
		// "go build ./... && go test ./... && go vet ./...": the tests
		// passed, and a linter or build after them printed the failure.
		set(open[:firstTest+1], checkPassed)
	}
}

// toolFromOutput judges a linter or build whose exit status is hidden.
// They say nothing when they pass, so what's left of the output counts
// unless a build's end was cut off.
func (s step) toolFromOutput(kind string, v exitView, ranFailed bool) string {
	out := s.result
	switch {
	case toolFailed(out), ranFailed:
		return checkFailed
	case exitFailure.MatchString(out) && !testFailure.MatchString(out):
		return checkFailed
	case s.isError && !testFailure.MatchString(out):
		return checkFailed // the command failed, and not on a test
	case kind == "build" && v.cut && strings.TrimSpace(harnessNote.ReplaceAllString(out, "")) != "":
		return checkUnclear
	}
	return checkPassed
}

// testsFromOutput judges a test run whose exit status is hidden. filter is
// the command after the run, where a filter on its output would be. alone
// is false when another hidden test run shares the output: a result in it
// then counts only for a run whose own last lines must be there. ranFailed
// is whatever ran the tests saying it failed.
func (s step) testsFromOutput(v exitView, filter string, alone, ranFailed bool) string {
	out := s.result
	whole := !v.cut && (alone || !v.sifted)
	switch {
	case testFailure.MatchString(out), exitFailure.MatchString(out):
		return checkFailed
	case testSummary.MatchString(out) && (alone || whole):
		return checkPassed
	case s.isError:
		return checkFailed // the command failed, and no summary says these tests didn't
	case ranFailed:
		return checkFailed
	case whole && testPackage.MatchString(out):
		return checkPassed
	case failureFilter.MatchString(filter):
		return checkPassed
	case !v.piped && exitZero.MatchString(out):
		return checkPassed
	}
	return checkUnclear
}

// rank orders results by what a verdict must not hide.
func rank(status string) int {
	switch status {
	case checkFailed:
		return 4
	case checkRunning:
		return 3
	case checkUnclear:
		return 2
	case checkPassed:
		return 1
	}
	return 0
}

// worst is the check that decides a kind's verdict in a step: the worst
// result among its runs, and the first of them when they agree. kind ""
// is every kind. ok is false when the step has no such check.
func worst(js []judged, kind string) (j judged, ok bool) {
	for _, c := range js {
		if (kind == "" || c.kind == kind) && rank(c.status) > rank(j.status) {
			j, ok = c, true
		}
	}
	return j, ok
}
