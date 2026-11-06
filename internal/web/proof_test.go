package web

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// verdicts runs proof.js's verdict over receipts in node, and returns what
// it says for each.
func verdicts(t *testing.T, cases string) []map[string]any {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node isn't installed")
	}
	src, err := staticFS.ReadFile("static/js/proof.js")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	mod := filepath.Join(dir, "proof.mjs")
	if err := os.WriteFile(mod, src, 0o600); err != nil {
		t.Fatal(err)
	}
	script := fmt.Sprintf(`import { verdict } from %q;
console.log(JSON.stringify(%s.map(([rc, working]) => verdict(rc, working))));`, mod, cases)
	out, err := exec.Command(node, "--input-type=module", "-e", script).CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, out)
	}
	var got []map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	return got
}

// The verdict is the one sentence a person reads to decide whether "done"
// means done, so each state's wording is pinned here.
func TestVerdictSaysWhatTheChecksMean(t *testing.T) {
	const pass = `{status:'passed',fresh:true}`
	const tests61 = `{status:'passed',fresh:true,count:61}`
	const staleTests = `{status:'passed',fresh:false,count:61}`
	const failed = `{status:'failed',fresh:true}`
	const failedBeforeEdit = `{status:'failed',fresh:false}`
	const unclear = `{status:'unclear',fresh:true}`
	const running = `{status:'running',fresh:true}`
	cases := []struct {
		name, receipt   string
		working         bool
		tone, title     string
		detail, askText string // substrings; askText "" means no offer
	}{
		{name: "all passed", receipt: `{edits:3,files:2,tests:` + tests61 + `,lint:` + pass + `,build:` + pass + `}`,
			tone: "ok", title: "Checked after the last edit", detail: "61 tests, lint and build passed on the latest code."},
		{name: "some ran", receipt: `{edits:1,files:1,tests:` + tests61 + `}`,
			tone: "ok", title: "Checked after the last edit", detail: "61 tests passed on the latest code. Lint and build didn’t run."},
		{name: "one test", receipt: `{edits:1,files:1,tests:{status:'passed',fresh:true,count:1},lint:` + pass + `,build:` + pass + `}`,
			tone: "ok", title: "Checked after the last edit", detail: "1 test, lint and build passed"},
		{name: "no edits", receipt: `{edits:0,tests:` + tests61 + `}`,
			tone: "ok", title: "Checks passed", detail: "61 tests passed. No files changed this turn."},
		{name: "failed", receipt: `{edits:1,files:1,tests:` + failed + `,lint:` + pass + `}`,
			tone: "bad", title: "Tests failed", detail: "isn’t finished", askText: "Tests failed. Fix the cause, run tests again"},
		{name: "two failed", receipt: `{edits:1,files:1,tests:` + failed + `,lint:` + failed + `}`,
			tone: "bad", title: "Tests and lint failed", detail: "isn’t finished", askText: "run tests and lint again"},
		{name: "failed, then edited", receipt: `{edits:2,files:1,tests:` + failedBeforeEdit + `}`,
			tone: "bad", title: "Tests failed", detail: "any fix is unproven", askText: "Fix the cause"},
		{name: "failed while working", receipt: `{edits:1,files:1,tests:` + failed + `}`, working: true,
			tone: "bad", title: "Tests failed", detail: "still working"},
		{name: "a failure outranks the rest", receipt: `{edits:1,files:1,tests:` + staleTests + `,lint:` + unclear + `,build:` + failed + `}`,
			tone: "bad", title: "Build failed", detail: "isn’t finished", askText: "Build failed."},
		{name: "running", receipt: `{edits:1,files:1,tests:` + running + `,lint:` + pass + `}`, working: true,
			tone: "run", title: "Tests running", detail: "when it finishes"},
		{name: "stale", receipt: `{edits:2,files:1,tests:` + staleTests + `,lint:` + pass + `}`,
			tone: "warn", title: "Edited after the last check", detail: "Tests passed, but it changed code afterwards", askText: "You changed code after tests ran"},
		{name: "stale while working", receipt: `{edits:2,files:1,tests:` + staleTests + `}`, working: true,
			tone: "warn", title: "Edited since the last check", detail: "It may run them again."},
		{name: "unclear", receipt: `{edits:1,files:1,tests:` + unclear + `,lint:` + pass + `}`,
			tone: "warn", title: "Result not shown", detail: "Tests ran, but the output doesn’t show how it went", askText: "without trimming the output"},
		{name: "not checked", receipt: `{edits:4,files:3}`,
			tone: "none", title: "Not checked", detail: "It changed 3 files but ran no tests", askText: "Run the tests, lint and build"},
		{name: "not checked yet", receipt: `{edits:1,files:1}`, working: true,
			tone: "none", title: "Not checked yet", detail: "It has changed 1 file and run no tests"},
		{name: "read only", receipt: `{edits:0,searches:3,reads:1}`,
			tone: "none", title: "Nothing changed", detail: "It only looked: 3 searches, 1 read."},
	}
	var b strings.Builder
	b.WriteByte('[')
	for _, c := range cases {
		fmt.Fprintf(&b, "[%s,%t],", c.receipt, c.working)
	}
	b.WriteByte(']')
	got := verdicts(t, b.String())
	for i, c := range cases {
		v := got[i]
		if v["tone"] != c.tone || v["title"] != c.title {
			t.Errorf("%s: tone %v title %q", c.name, v["tone"], v["title"])
		}
		if d, _ := v["detail"].(string); !strings.Contains(d, c.detail) {
			t.Errorf("%s: detail %q, want %q", c.name, d, c.detail)
		}
		ask, _ := v["ask"].(map[string]any)
		switch {
		case c.askText == "" && ask != nil:
			t.Errorf("%s: offered %v", c.name, ask)
		case c.askText != "" && (ask == nil || !strings.Contains(ask["text"].(string), c.askText)):
			t.Errorf("%s: ask %v, want %q", c.name, ask, c.askText)
		}
	}
}

// Nothing to say about a turn that did nothing.
func TestVerdictOfAnEmptyTurn(t *testing.T) {
	if got := verdicts(t, `[[{edits:0,searches:0,reads:0},false]]`); got[0] != nil {
		t.Errorf("verdict = %v", got[0])
	}
}
