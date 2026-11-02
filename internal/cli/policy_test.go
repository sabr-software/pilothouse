package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sabr-software/pilothouse/internal/adapters"
	"github.com/sabr-software/pilothouse/internal/api"
	"github.com/sabr-software/pilothouse/internal/paths"
	"github.com/sabr-software/pilothouse/internal/policy"
)

// polTestEnv isolates every location (BUILD-SPEC §2.1).
func polTestEnv(t *testing.T) (home string) {
	t.Helper()
	home = t.TempDir()
	if r, err := filepath.EvalSymlinks(home); err == nil {
		home = r
	}
	run, err := os.MkdirTemp("/tmp", "ag")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(run) })
	t.Setenv("PILOTHOUSE_HOME", filepath.Join(home, ".pilothouse"))
	t.Setenv("PILOTHOUSE_USER_HOME", home)
	t.Setenv("PILOTHOUSE_RUN_DIR", run)
	t.Setenv("NO_COLOR", "1")
	return home
}

func polRun(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := newPolicyCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), err
}

func TestPolicyInit(t *testing.T) {
	polTestEnv(t)
	out, err := polRun(t, "init")
	if err != nil || !strings.Contains(out, "Wrote ~/.pilothouse/policy.yaml (7 rules)") {
		t.Fatalf("init: %v\n%s", err, out)
	}
	fi, err := os.Stat(paths.Policy())
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("policy file mode: %v %v", fi, err)
	}
	if _, err := polRun(t, "init"); err == nil || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("second init must refuse: %v", err)
	}
	if err := os.WriteFile(paths.Policy(), []byte("version: 1\n# mine\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err = polRun(t, "init", "--force")
	if err != nil || !strings.Contains(out, "Backed up") {
		t.Fatalf("init --force: %v\n%s", err, out)
	}
	backups, _ := filepath.Glob(paths.Policy() + ".bak-*")
	if len(backups) != 1 {
		t.Fatalf("backups = %v", backups)
	}
	if b, _ := os.ReadFile(backups[0]); string(b) != "version: 1\n# mine\n" {
		t.Fatalf("backup content %q", b)
	}
}

func TestPolicyCheck(t *testing.T) {
	home := polTestEnv(t)
	t.Chdir(home) // not a git repo
	out, err := polRun(t, "check")
	if err != nil || !strings.Contains(out, "No policy files yet") {
		t.Fatalf("empty: %v %s", err, out)
	}
	if _, err := polRun(t, "init"); err != nil {
		t.Fatal(err)
	}
	if out, err := polRun(t, "check"); err != nil || !strings.Contains(out, "✓ ~/.pilothouse/policy.yaml  7 rules") {
		t.Fatalf("valid: %v %s", err, out)
	}
	bad := filepath.Join(home, "bad.yaml")
	os.WriteFile(bad, []byte("version: 1\nrules:\n  - name: x\n    match: {tool: A}\n    acton: deny\n"), 0o600)
	out, err = polRun(t, "check", bad)
	var ee ExitError
	if !errors.As(err, &ee) || ee.Code != 1 {
		t.Fatalf("want exit 1, got %v", err)
	}
	if !strings.Contains(out, "✗ ~/bad.yaml") || !strings.Contains(out, "~/bad.yaml:5: unknown key \"acton\"") {
		t.Fatalf("output:\n%s", out)
	}
	if _, err := polRun(t, "check", filepath.Join(home, "missing.yaml")); err == nil {
		t.Fatal("missing file must fail")
	}
}

func TestPolicyTestCommand(t *testing.T) {
	home := polTestEnv(t)
	t.Chdir(home)
	if _, err := polRun(t, "init"); err != nil {
		t.Fatal(err)
	}
	out, err := polRun(t, "test", "--command", "git push --force origin x", "--repo", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Decision: DENY by no-force-push (~/.pilothouse/policy.yaml)", "Message:  Force-push is blocked", "Matched:", "deny  no-force-push"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}

	out, err = polRun(t, "test", "--kind", "read", "--path", ".env", "--cwd", home, "--repo", "", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var js struct {
		Input  policy.Input  `json:"input"`
		Result policy.Result `json:"result"`
	}
	if err := json.Unmarshal([]byte(out), &js); err != nil {
		t.Fatalf("json: %v\n%s", err, out)
	}
	if js.Result.Action != api.BehaviorDeny || js.Result.Rule != "no-secret-files" || len(js.Result.Matched) != 2 {
		t.Fatalf("result %+v", js.Result)
	}
	var raw bytes.Buffer
	json.Compact(&raw, js.Input.Input)
	if js.Input.Tool != "Read" || raw.String() != `{"file_path":".env"}` {
		t.Fatalf("input %+v", js.Input)
	}

	out, _ = polRun(t, "test", "--command", "ls", "--repo", "")
	if !strings.Contains(out, "Decision: none") {
		t.Errorf("no match output:\n%s", out)
	}
	out, _ = polRun(t, "test", "--agent", "codex", "--tool", "shell", "--command", "pilothouse approve r_1", "--repo", "")
	if !strings.Contains(out, "ASK by pilothouse-self-protection (built-in)") {
		t.Errorf("self-protection output:\n%s", out)
	}
	out, _ = polRun(t, "test", "--path", "x.go", "--repo", "")
	if !strings.Contains(out, "assuming --kind edit") {
		t.Errorf("kind note missing:\n%s", out)
	}
	for _, args := range [][]string{
		{"test"},
		{"test", "--command", "x", "--agent", "gpt"},
		{"test", "--command", "x", "--kind", "bash"},
		{"test", "--tool", "X", "--input", "{bad"},
	} {
		if _, err := polRun(t, args...); err == nil {
			t.Errorf("%v: want error", args)
		}
	}
}

func TestPolicyToolKind(t *testing.T) {
	for tool, want := range map[string]api.ToolKind{
		"Bash": api.ToolShell, "shell": api.ToolShell, "Write": api.ToolEdit, "apply_patch": api.ToolEdit,
		"Grep": api.ToolRead, "WebFetch": api.ToolWeb, "mcp__github__create_pr": api.ToolMCP, "Task": api.ToolOther,
	} {
		if got := polToolKind(tool); got != want {
			t.Errorf("%s: %s want %s", tool, got, want)
		}
	}
}

func TestPolicyTrustAndStatus(t *testing.T) {
	home := polTestEnv(t)
	repo := filepath.Join(home, "proj")
	os.MkdirAll(filepath.Join(repo, ".pilothouse"), 0o700)
	os.WriteFile(filepath.Join(repo, policy.RepoPolicyFile), []byte("version: 1\nrules:\n  - {name: m, match: {command: \"make*\"}, action: allow}\n"), 0o600)

	out, err := polRun(t, "trust", repo)
	if err != nil || !strings.Contains(out, "Trusted ~/proj") || !strings.Contains(out, "1 rule") {
		t.Fatalf("trust: %v\n%s", err, out)
	}
	if out, _ := polRun(t, "trust", repo); !strings.Contains(out, "already trusted") {
		t.Fatalf("trust again:\n%s", out)
	}
	t.Chdir(home)
	out, err = polRun(t, "status")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Daemon not running", "builtin", "not created", "Trusted repos:", "~/proj since"} {
		if !strings.Contains(out, want) {
			t.Errorf("status missing %q:\n%s", want, out)
		}
	}
	out, err = polRun(t, "status", "--json")
	if err != nil || !strings.Contains(out, `"source": "local"`) {
		t.Fatalf("status json: %v\n%s", err, out)
	}
	if out, err := polRun(t, "untrust", repo); err != nil || !strings.Contains(out, "Untrusted ~/proj") {
		t.Fatalf("untrust: %v\n%s", err, out)
	}
	if out, _ := polRun(t, "untrust", repo); !strings.Contains(out, "was not trusted") {
		t.Fatalf("untrust again:\n%s", out)
	}
	if _, err := polRun(t, "trust", filepath.Join(home, "nope")); err == nil {
		t.Fatal("missing dir must fail")
	}
	if _, err := polRun(t, "trust"); err == nil || !strings.Contains(err.Error(), "not inside a git repository") {
		t.Fatalf("no repo: %v", err)
	}
}

func TestPolicyReplayDaemonDown(t *testing.T) {
	polTestEnv(t)
	_, err := polRun(t, "replay")
	if err == nil || !strings.Contains(err.Error(), "pilothouse start") {
		t.Fatalf("err = %v", err)
	}
}

func TestPolicyReplay(t *testing.T) {
	polTestEnv(t)
	if err := os.MkdirAll(filepath.Dir(paths.Policy()), 0o700); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(paths.Policy(), []byte("version: 1\nrules:\n  - {name: no-push, match: {command: \"git push*\"}, action: deny}\n"), 0o600)

	now := time.Now()
	hook := func(seq int64, cmd string) api.Event {
		b, _ := json.Marshal(adapters.HookEvent{Kind: adapters.HookPreTool, Tool: "Bash", ToolKind: api.ToolShell, Command: cmd, Summary: "Run: " + cmd})
		return api.Event{Seq: seq, At: now.Add(-time.Hour), Type: api.EventHook, SessionID: "s_1", Data: b}
	}
	events := []api.Event{
		hook(1, "git push origin x"),
		hook(2, "ls"),
		{Seq: 3, At: now.Add(-time.Hour), Type: api.EventSessionUpdated, SessionID: "s_1"},
	}
	// Pad past one page so paging is exercised.
	for i := int64(4); i < 1500; i++ {
		events = append(events, api.Event{Seq: i, At: now, Type: api.EventPresenceChanged})
	}
	events = append(events, hook(1500, "git push --tags"))

	sock := paths.Socket()
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/events", func(w http.ResponseWriter, r *http.Request) {
		after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
		page := []api.Event{}
		for _, ev := range events {
			if ev.Seq > after && len(page) < 1000 {
				page = append(page, ev)
			}
		}
		json.NewEncoder(w).Encode(page)
	})
	mux.HandleFunc("GET /v1/sessions", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]api.Session{{ID: "s_1", Agent: api.AgentClaude}})
	})
	srv := &http.Server{Handler: mux}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })

	out, err := polRun(t, "replay", "--since", "2d")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !strings.Contains(out, "Replayed 3 tool calls") || !strings.Contains(out, "2 verdicts would change") ||
		!strings.Contains(out, "Run: git push origin x") || strings.Contains(out, "Run: ls") {
		t.Fatalf("output:\n%s", out)
	}
	out, err = polRun(t, "replay", "--json", "--since", "30m")
	if err != nil {
		t.Fatal(err)
	}
	var rep policy.ReplayReport
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if rep.Calls != 0 {
		t.Fatalf("--since 30m should exclude hour-old calls: %+v", rep)
	}
}

func TestPolParseSince(t *testing.T) {
	for in, want := range map[string]time.Duration{"7d": 7 * 24 * time.Hour, "1.5d": 36 * time.Hour, "90m": 90 * time.Minute} {
		if got, err := polParseSince(in); err != nil || got != want {
			t.Errorf("%s: %v %v", in, got, err)
		}
	}
	for _, bad := range []string{"", "x", "-1d", "0s"} {
		if _, err := polParseSince(bad); err == nil {
			t.Errorf("%q: want error", bad)
		}
	}
}
