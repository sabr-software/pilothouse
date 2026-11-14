package transcript

import (
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/sabr-software/pilothouse/internal/api"
)

// A receipt is what a session's latest turn did, read off its transcript:
// the checks it ran (tests, lint, build) with their last result and
// whether they ran after the last edit, the files it changed, the commit
// it made, and the calls that were refused or failed. Nothing is
// summarised: every field is a count, a command or a parsed number.

// step is one tool call as the receipt needs it.
type step struct {
	at             time.Time
	kind           api.ToolKind
	tool           string
	command        string
	result         string
	isError        bool
	pending        bool
	lost           bool // the call ended and its result never reached us
	denied         bool
	side           bool // a subagent's call: only its edits count, for freshness
	edits          []fileEdit
	added, removed int // when the source has no per-edit counts
	paths          []string
}

var (
	// lintCommand recognises linters and type checks.
	lintCommand = regexp.MustCompile(`(?:^|[\s;&|(])((?:npm|pnpm|yarn|bun)(?:\s+run)?\s+(?:lint|typecheck|check-types|type-check)(?::[\w-]+)?|eslint|golangci-lint|staticcheck|go\s+vet|gofmt\s+-l|ruff(?:\s+check)?|flake8|pylint|mypy|biome\s+(?:lint|check)|tsc\s+(?:[\w.=-]+\s+)*--noEmit|cargo\s+clippy|prettier\s+--check|rubocop|swiftlint|shellcheck)(?:\s|$|;|&|\|)`)
	// buildCommand recognises builds.
	buildCommand = regexp.MustCompile(`(?:^|[\s;&|(])((?:npm|pnpm|yarn|bun)(?:\s+run)?\s+build(?::[\w-]+)?|go\s+build|cargo\s+build|vite\s+build|next\s+build|astro\s+build|make\s+(?:build|all|release)|gradlew?\s+build|mvn\s+(?:[\w:.=-]+\s+)*package|dotnet\s+build|swift\s+build|xcodebuild|docker\s+build)(?:\s|$|;|&|\|)`)
	// searchCommand / readCommand classify plain shell reads.
	searchCommand = regexp.MustCompile(`^\s*(?:cd\s+\S+\s*(?:&&|;)\s*)?(?:grep|rg|ag|ack|find|fd|git\s+grep)\b`)
	readCommand   = regexp.MustCompile(`^\s*(?:cd\s+\S+\s*(?:&&|;)\s*)?(?:cat|head|tail|sed\s+-n|less|bat|wc|ls|tree|nl)\b`)
	commitCommand = regexp.MustCompile(`(?:^|[\s;&|(])git\s+(?:-C\s+\S+\s+)?commit(?:\s|$)`)
	commitMessage = regexp.MustCompile(`(?:^|\s)-[a-zA-Z]*m\s*(?:"((?:[^"\\]|\\.)*)"|'([^']*)'|(\S+))`)
	// passCount finds "48 passed", "48 passing", "Tests: 48 passed", "test result: ok. 48 passed".
	passCount = regexp.MustCompile(`(?i)\b(\d+)\s+(?:passed|passing|tests? passed)\b`)
)

// checkCommands recognise each kind of check in a command.
var checkCommands = map[string]*regexp.Regexp{"tests": testCommand, "lint": lintCommand, "build": buildCommand}

// stepCheck is a finished command's result as a check, for the activity
// feed, or "": the worst result among the checks in it.
func stepCheck(command, output string, isError bool) string {
	j, _ := worst(step{command: command, result: output, isError: isError}.checks(), "")
	return j.status
}

// commandHead is what a shell command runs, for recognising checks: the
// text before any heredoc, with quoted strings blanked, so a script that
// merely mentions "go test" (python3 - <<'EOF' …) isn't a test run. Every
// byte keeps its place: an offset in the head is the same one in command.
func commandHead(command string) string {
	if i := strings.Index(command, "<<"); i >= 0 {
		command = command[:i]
	}
	if !strings.ContainsAny(command, `'"`) {
		return command
	}
	b := []byte(command)
	var quote byte
	for i, c := range b {
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			}
			b[i] = ' '
		case c == '\'' || c == '"':
			quote = c
			b[i] = ' '
		}
	}
	return string(b)
}

// commitSubject returns the first line of a git commit's message, "" when
// the command commits without -m, and ok=false when it isn't a commit.
// The commit is looked for in what the command runs, not in the megabytes
// a heredoc may carry, and the message is read no further than its line.
func commitSubject(command string) (subject string, ok bool) {
	loc := commitCommand.FindStringIndex(commandHead(command))
	if loc == nil {
		return "", false
	}
	rest := command[loc[1]:]
	if i := strings.Index(rest, "<<"); i >= 0 { // -m "$(cat <<'EOF' … EOF)"
		body := rest[i:]
		for {
			nl := strings.IndexByte(body, '\n')
			if nl < 0 {
				return "", true
			}
			body = body[nl+1:]
			line := body
			if end := strings.IndexByte(body, '\n'); end >= 0 {
				line = body[:end]
			}
			if line = strings.TrimSpace(line); line != "" {
				return line, true
			}
		}
	}
	m := commitMessage.FindStringSubmatch(rest)
	if m == nil {
		return "", true
	}
	msg := m[1] + m[2] + m[3]
	msg = strings.ReplaceAll(msg, `\"`, `"`)
	if i := strings.IndexAny(msg, "\n"); i >= 0 {
		msg = msg[:i]
	}
	return strings.TrimSpace(msg), true
}

// testsPassed reads how many tests passed from a runner's output, or 0.
func testsPassed(result string) int {
	ms := passCount.FindAllStringSubmatch(result, -1)
	if len(ms) == 0 {
		return 0
	}
	n, _ := strconv.Atoi(ms[len(ms)-1][1])
	return n
}

func isSearch(s step) bool {
	t := strings.ToLower(s.tool)
	if s.kind == api.ToolRead && (strings.Contains(t, "grep") || strings.Contains(t, "glob") || strings.Contains(t, "search") || strings.Contains(t, "find")) {
		return true
	}
	return s.kind == api.ToolShell && searchCommand.MatchString(s.command)
}

func isRead(s step) bool {
	if s.kind == api.ToolRead {
		return !isSearch(s)
	}
	return s.kind == api.ToolShell && readCommand.MatchString(s.command)
}

// maxTurnFiles bounds the paths a turn's tally remembers.
const maxTurnFiles = 5000

// tally is one kind of check over a turn.
type tally struct {
	last   *api.Check // never changed once set: copies of a tally share it
	failed int        // failed runs before the last one
}

// turnTally folds a turn's tool calls, oldest first, into a receipt. The
// feed keeps one for the calls of the latest turn it no longer holds, so a
// long turn's receipt still has its early checks and its counts.
type turnTally struct {
	rc       api.Receipt // the counts; the rest is filled in by receipt
	checks   map[string]tally
	files    map[string]bool
	base     map[string]bool // files of the tally this one continues
	lastEdit time.Time
}

func newTurnTally() *turnTally {
	return &turnTally{checks: map[string]tally{}, files: map[string]bool{}}
}

// extend is a tally that continues a and leaves it as it is. A nil a is a
// turn's start.
func (a *turnTally) extend() *turnTally {
	n := newTurnTally()
	if a == nil {
		return n
	}
	n.rc, n.lastEdit, n.base = a.rc, a.lastEdit, a.files
	for kind, t := range a.checks {
		n.checks[kind] = t
	}
	return n
}

func (a *turnTally) file(path string) {
	if !a.base[path] && len(a.files) < maxTurnFiles {
		a.files[path] = true
	}
}

// add counts one tool call.
func (a *turnTally) add(s step) {
	rc := &a.rc
	if s.side {
		// A subagent's calls aren't the turn's own, but an edit it made
		// changed the code the turn's checks ran on.
		if s.kind == api.ToolEdit && !s.isError && !s.denied && s.at.After(a.lastEdit) {
			a.lastEdit = s.at
		}
		return
	}
	switch {
	case s.denied:
		rc.Blocked++
		return
	case s.isError && !s.pending && s.kind != api.ToolShell:
		rc.Failed++
		return
	}
	switch {
	case isSearch(s):
		rc.Searches++
	case isRead(s):
		rc.Reads++
	}
	if s.kind == api.ToolEdit && !s.isError {
		rc.Edits++
		if s.at.After(a.lastEdit) {
			a.lastEdit = s.at
		}
		for _, e := range s.edits {
			a.file(e.path)
			rc.Added += e.added
			rc.Removed += e.removed
		}
		for _, p := range s.paths {
			a.file(p)
		}
		rc.Added += s.added
		rc.Removed += s.removed
	}
	if s.kind != api.ToolShell || s.command == "" {
		return
	}
	if subj, ok := commitSubject(s.command); ok && !s.isError && !s.pending {
		rc.Commit = subj
		if rc.Commit == "" {
			rc.Commit = "(no message)"
		}
	}
	js := s.checks()
	for _, kind := range checkKinds {
		// A command's verdict for a kind is its worst run's, under that
		// run's name.
		j, ok := worst(js, kind)
		if !ok {
			continue
		}
		t := a.checks[kind]
		if t.last != nil && t.last.Status == checkFailed {
			t.failed++
		}
		t.last = &api.Check{Name: j.name, Command: s.command, At: s.at, Status: j.status}
		if kind == "tests" && j.status == checkPassed {
			t.last.Count = testsPassed(s.result)
		}
		a.checks[kind] = t
	}
	// A search that finds nothing exits 1: not a failure worth counting.
	if s.isError && !s.pending && !isSearch(s) && len(js) == 0 {
		rc.Failed++
	}
}

// receipt is the turn so far.
func (a *turnTally) receipt(turn int, since time.Time) *api.Receipt {
	rc := a.rc
	rc.Turn, rc.Since, rc.Files = turn, since, len(a.base)+len(a.files)
	if !a.lastEdit.IsZero() {
		le := a.lastEdit
		rc.LastEdit = &le
	}
	for kind, t := range a.checks {
		c := *t.last
		c.FailedBefore = t.failed
		c.Fresh = a.lastEdit.IsZero() || c.At.After(a.lastEdit)
		switch kind {
		case "tests":
			rc.Tests = &c
		case "lint":
			rc.Lint = &c
		case "build":
			rc.Build = &c
		}
	}
	return &rc
}

// output is a tool call's output, as much as the feed still has.
func (f *feed) output(it *item) string {
	if out, ok := f.checkOutput[it.ID]; ok {
		return out
	}
	return it.Result
}

// step is a feed item as a receipt counts it. ok is false for what it
// doesn't count: prompts and replies, questions and plans, and a
// subagent's calls other than its edits.
func (f *feed) step(it *item) (s step, ok bool) {
	if it.Kind != api.ActivityTool || it.asks > 0 || it.plan || (it.Sidechain && it.ToolKind != api.ToolEdit) {
		return s, false
	}
	return step{at: it.At, kind: it.ToolKind, tool: it.Tool, command: it.command, result: f.output(it),
		isError: it.IsError, pending: it.Pending, lost: it.lost, denied: it.denied, side: it.Sidechain, edits: it.edits}, true
}

// receipt is the receipt of the feed's latest turn.
func (f *feed) receipt() *api.Receipt {
	turn, since := 0, time.Time{}
	if p := f.lastPrompt; p != nil {
		turn, since = p.Turn, p.At
	}
	a := f.early.extend()
	for _, it := range f.items {
		if it.At.Before(since) {
			continue
		}
		if s, ok := f.step(it); ok {
			a.add(s)
		}
	}
	return a.receipt(turn, since)
}

// checkInput is a command and its output in full, for a source whose items
// only carry a one-line summary and the output's tail.
type checkInput struct{ command, output string }

// ReceiptOf builds the latest turn's receipt from feed items, for sources
// without a transcript feed of their own (OpenCode, the demo). Shell
// commands come from "Run: …" summaries; files from "Edit …" summaries,
// with line counts from files when given (keyed by path).
func ReceiptOf(items []api.ActivityItem, files []api.FileChange) *api.Receipt {
	return receiptOf(items, files, nil)
}

// receiptOf is ReceiptOf with the commands a summary cuts short, by item ID.
func receiptOf(items []api.ActivityItem, files []api.FileChange, full map[string]checkInput) *api.Receipt {
	turn, since := 0, time.Time{}
	for i := len(items) - 1; i >= 0; i-- {
		if it := items[i]; it.Kind == api.ActivityPrompt && !it.Sidechain {
			turn, since = it.Turn, it.At
			break
		}
	}
	counts := map[string]api.FileChange{}
	for _, fc := range files {
		counts[fc.Path] = fc
	}
	seen := map[string]bool{}
	a := newTurnTally()
	for _, it := range items {
		if it.At.Before(since) || it.Kind != api.ActivityTool || (it.Sidechain && it.ToolKind != api.ToolEdit) {
			continue
		}
		s := step{at: it.At, kind: it.ToolKind, tool: it.Tool, result: it.Result, isError: it.IsError, pending: it.Pending, side: it.Sidechain}
		if s.kind == api.ToolShell {
			s.command = strings.TrimSpace(strings.TrimPrefix(it.Summary, "Run:"))
			if in, ok := full[it.ID]; ok {
				s.command, s.result = in.command, in.output
			}
		}
		if s.isError && deniedText.MatchString(it.Result) {
			s.denied = true
		}
		if s.kind == api.ToolEdit {
			p := it.Summary
			for _, pre := range []string{"Edit:", "Write:", "Edit", "Write"} {
				p = strings.TrimPrefix(p, pre)
			}
			p = strings.TrimSpace(p)
			if p != "" {
				s.paths = []string{p}
				if fc, ok := counts[p]; ok && !seen[p] {
					s.added, s.removed = fc.Added, fc.Removed
					seen[p] = true
				}
			}
		}
		a.add(s)
	}
	return a.receipt(turn, since)
}

// deniedText recognises a refusal in a tool result.
var deniedText = regexp.MustCompile(`(?i)\b(?:denied|blocked by|refused|not allowed by)\b`)
