package desktop

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sabr-software/pilothouse/internal/api"
)

// fakeHerdr answers herdr commands like herdr 0.9 does.
type fakeHerdr struct {
	mu        sync.Mutex
	running   bool
	calls     [][]string
	tabs      map[string]bool
	panes     map[string][2]string // pane → tab, terminal id
	made      int
	restarts  int  // herdr restarted this many times: new terminal ids
	idle      bool // the pane's shell is at its prompt
	failRun   bool
	workspace string // JSON array of workspaces
}

const herdrBin = "/fake/bin/herdr"

func newFake() *fakeHerdr {
	return &fakeHerdr{running: true, tabs: map[string]bool{}, panes: map[string][2]string{}, workspace: `[
		{"workspace_id":"w1","label":"Halyard","focused":false,"worktree":{"checkout_path":"/src/relay","repo_root":"/src/relay"}},
		{"workspace_id":"w8","label":"PageMind","focused":false},
		{"workspace_id":"wF","label":"ERP","focused":true}]`}
}

func (f *fakeHerdr) run(_ context.Context, name string, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, append([]string{name}, args...))
	if name != herdrBin {
		return nil, nil // /usr/bin/open
	}
	if len(args) > 1 && args[0] == "--session" {
		args = args[2:]
	}
	fail := func(code, msg string) ([]byte, error) {
		return nil, &RunError{Err: errors.New("exit status 1"), Output: `{"id":"x","error":{"code":"` + code + `","message":"` + msg + `"}}`}
	}
	if !f.running {
		return fail("server_not_running", "no herdr server is running")
	}
	switch strings.Join(args[:2], " ") {
	case "workspace list":
		return []byte(`{"id":"cli:workspace:list","result":{"type":"workspace_list","workspaces":` + f.workspace + `}}`), nil
	case "tab create":
		ws := "wF"
		for i, a := range args {
			if a == "--workspace" {
				ws = args[i+1]
			}
		}
		f.made++
		id := ws + ":t" + strconv.Itoa(f.made+1)
		pane := ws + ":p" + strconv.Itoa(f.made+8)
		term := "term_" + strconv.Itoa(f.restarts) + "x" + strconv.Itoa(f.made)
		f.tabs[id] = true
		f.panes[pane] = [2]string{id, term}
		return []byte(`{"id":"cli:tab:create","result":{"root_pane":{"pane_id":"` + pane + `","tab_id":"` + id + `","terminal_id":"` + term + `"},"tab":{"tab_id":"` + id + `","workspace_id":"` + ws + `","label":"x"},"type":"tab_created"}}`), nil
	case "pane get":
		p, ok := f.panes[args[2]]
		if !ok {
			return fail("pane_not_found", "pane "+args[2]+" not found")
		}
		return []byte(`{"id":"cli:pane:get","result":{"pane":{"pane_id":"` + args[2] + `","tab_id":"` + p[0] + `","terminal_id":"` + p[1] + `"}}}`), nil
	case "tab get", "tab focus":
		if !f.tabs[args[2]] {
			return fail("tab_not_found", "tab "+args[2]+" not found")
		}
		return []byte(`{"id":"cli:tab:get","result":{"tab":{"tab_id":"` + args[2] + `"}}}`), nil
	case "tab close":
		f.closeTab(args[2])
		return []byte(`{"id":"cli:tab:close","result":{}}`), nil
	case "pane run":
		if f.failRun {
			return fail("pane_not_found", "pane gone")
		}
		return nil, nil // herdr 0.9 prints nothing for pane run
	case "pane process-info":
		group := 200
		if f.idle {
			group = 100
		}
		return []byte(`{"id":"cli:pane:process_info","result":{"process_info":{"foreground_process_group_id":` + strconv.Itoa(group) + `,"shell_pid":100}}}`), nil
	case "agent list":
		return []byte(`{"id":"cli:agent:list","result":{"type":"agent_list","agents":[
			{"agent":"claude","agent_session":{"agent":"claude","kind":"id","value":"cc-harbor"},"tab_id":"w1:t3","workspace_id":"w1","terminal_title_stripped":"Go-live readiness"},
			{"agent":"claude","agent_session":{"agent":"claude","kind":"id","value":"cc-orbit"},"tab_id":"w8:t1","workspace_id":"w8"},
			{"agent":"claude","agent_session":{"agent":"claude","kind":"id","value":"cc-same"},"tab_id":"wF:t2","workspace_id":"wF"},
			{"agent":"opencode","agent_session":{"agent":"opencode","kind":"id","value":"ses_erp"},"tab_id":"wF:t3","workspace_id":"wF"},
			{"agent":"claude","tab_id":"wF:t4","workspace_id":"wF"}]}}`), nil
	case "tab list":
		return []byte(`{"id":"cli:tab:list","result":{"type":"tab_list","tabs":[
			{"tab_id":"w1:t3","label":"Harbor","workspace_id":"w1"},
			{"tab_id":"w8:t1","label":"1","workspace_id":"w8"},
			{"tab_id":"wF:t2","label":"erp","workspace_id":"wF"},
			{"tab_id":"wF:t3","label":"  claude\u0007   session ","workspace_id":"wF"},
			{"tab_id":"wF:t4","label":"no id","workspace_id":"wF"}]}}`), nil
	case "workspace focus":
		return []byte(`{"id":"cli:workspace:focus","result":{}}`), nil
	}
	return fail("unknown", "unexpected "+strings.Join(args, " "))
}

// herdrCalls returns the herdr calls without the binary, from index i.
func (f *fakeHerdr) herdrCalls(i int) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.calls[i:] {
		if c[0] == herdrBin {
			out = append(out, strings.Join(c[1:], " "))
		}
	}
	return out
}

func (f *fakeHerdr) closeTab(id string) {
	delete(f.tabs, id)
	for p, v := range f.panes {
		if v[0] == id {
			delete(f.panes, p)
		}
	}
}

// restart is herdr quitting and starting again: tab and pane numbers
// begin again, terminals are new.
func (f *fakeHerdr) restart() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tabs, f.panes, f.made = map[string]bool{}, map[string][2]string{}, 0
	f.restarts++
}

func (f *fakeHerdr) n() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.calls) }

func newOpener(t *testing.T, f *fakeHerdr, mode, goos string) *Opener {
	t.Helper()
	o := New(mode, "", "", "/opt/homebrew/bin/pilothouse", t.TempDir())
	o.GOOS = goos
	o.Run = f.run
	o.LookPath = func(name string) (string, error) {
		if name == "herdr" {
			return herdrBin, nil
		}
		return "", exec.ErrNotFound
	}
	return o
}

var task = Spec{SessionID: "s_jrqpq7oveb", Title: "give-me-a-summary-of-what", Cwd: "", Repo: "/Users/x/PageMind", Focus: true}

func TestOpenInHerdrCreatesTabAndAttaches(t *testing.T) {
	f := newFake()
	o := newOpener(t, f, ModeAuto, "darwin")
	cwd := t.TempDir()
	s := task
	s.Cwd = cwd
	res, err := o.Open(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	want := api.DesktopOpened{Target: api.DesktopHerdr, Label: "herdr", Where: "PageMind"}
	if res != want {
		t.Fatalf("result %+v, want %+v", res, want)
	}
	got := f.herdrCalls(0)
	wantCalls := []string{
		"workspace list", // Open's probe
		"workspace list", // openHerdr
		"tab create --workspace w8 --cwd " + cwd + " --label give-me-a-summary-of-what --focus",
		"pane run w8:p9 /opt/homebrew/bin/pilothouse attach --replay s_jrqpq7oveb",
		"workspace focus w8",
		"tab focus w8:t2",
	}
	if !reflect.DeepEqual(got, wantCalls) {
		t.Fatalf("calls:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(wantCalls, "\n"))
	}
}

func TestOpenInHerdrReusesTheTab(t *testing.T) {
	f := newFake()
	o := newOpener(t, f, ModeAuto, "darwin")
	if _, err := o.Open(context.Background(), task); err != nil {
		t.Fatal(err)
	}

	// Still attached there: only focus.
	i := f.n()
	res, err := o.Open(context.Background(), task)
	if err != nil || !res.Reused || res.Where != "PageMind" {
		t.Fatalf("%+v %v", res, err)
	}
	if got := f.herdrCalls(i); !reflect.DeepEqual(got, []string{"workspace list", "workspace list", "pane get w8:p9", "pane process-info --pane w8:p9", "workspace focus w8", "tab focus w8:t2"}) {
		t.Fatalf("reuse calls %q", got)
	}

	// Detached in that tab: attach again.
	f.idle = true
	i = f.n()
	if _, err := o.Open(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	if got := f.herdrCalls(i); !contains(got, "pane run w8:p9 /opt/homebrew/bin/pilothouse attach --replay s_jrqpq7oveb") {
		t.Fatalf("no re-attach: %q", got)
	}

	// Opened automatically (no focus): nothing typed, nothing focused.
	i = f.n()
	auto := task
	auto.Focus = false
	if res, err := o.Open(context.Background(), auto); err != nil || !res.Reused {
		t.Fatalf("%+v %v", res, err)
	}
	for _, c := range f.herdrCalls(i) {
		if strings.HasPrefix(c, "pane run") || strings.Contains(c, "focus") {
			t.Fatalf("auto reuse ran %q", c)
		}
	}

	// The person closed the tab: a new one.
	f.closeTab("w8:t2")
	i = f.n()
	res, err = o.Open(context.Background(), task)
	if err != nil || res.Reused {
		t.Fatalf("%+v %v", res, err)
	}
	if got := f.herdrCalls(i); !contains(got, "pane get w8:p9") || !hasPrefix(got, "tab create") {
		t.Fatalf("calls %q", got)
	}

	// herdr restarted and the same numbers now name someone else's tab:
	// nothing is typed there; a new tab is made.
	f.restart()
	f.idle = true
	f.mu.Lock()
	f.tabs["w8:t3"] = true
	f.panes["w8:p10"] = [2]string{"w8:t3", "term_theirs"}
	f.made = 2
	f.mu.Unlock()
	i = f.n()
	res, err = o.Open(context.Background(), task)
	if err != nil || res.Reused {
		t.Fatalf("after restart: %+v %v", res, err)
	}
	got := f.herdrCalls(i)
	for _, c := range got {
		if strings.Contains(c, "w8:p10") && !strings.HasPrefix(c, "pane get") {
			t.Fatalf("touched someone else's pane: %q", got)
		}
	}
	if !contains(got, "pane run w8:p11 /opt/homebrew/bin/pilothouse attach --replay s_jrqpq7oveb") {
		t.Fatalf("no new tab after restart: %q", got)
	}
}

func TestOpenInHerdrWithoutFocus(t *testing.T) {
	f := newFake()
	o := newOpener(t, f, ModeHerdr, "linux")
	o.Env = []string{"PILOTHOUSE_HOME=/tmp/ph home"}
	s := task
	s.Focus = false
	s.Repo = "/elsewhere/unknown"
	if _, err := o.Open(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	got := f.herdrCalls(0)
	// No matching workspace: herdr picks (no --workspace). No focus calls.
	if !contains(got, "tab create --env PILOTHOUSE_HOME=/tmp/ph home --label give-me-a-summary-of-what --no-focus") {
		t.Fatalf("calls %q", got)
	}
	for _, c := range got {
		if strings.Contains(c, "focus w") {
			t.Fatalf("focused: %q", c)
		}
	}
}

func TestOpenInHerdrClosesTheTabWhenTypingFails(t *testing.T) {
	f := newFake()
	f.failRun = true
	o := newOpener(t, f, ModeAuto, "darwin")
	_, err := o.Open(context.Background(), task)
	if err == nil || !strings.Contains(err.Error(), "herdr pane run: pane gone") {
		t.Fatalf("err %v", err)
	}
	if !contains(f.herdrCalls(0), "tab close w8:t2") || len(f.tabs) != 0 {
		t.Fatalf("tab left behind: %q", f.herdrCalls(0))
	}
}

func TestFallsBackToTerminalOnMac(t *testing.T) {
	f := newFake()
	f.running = false
	o := newOpener(t, f, ModeAuto, "darwin")
	cwd := filepath.Join(t.TempDir(), "it's $(touch pwned) here")
	if err := os.Mkdir(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	s := task
	s.Cwd = cwd
	if info := o.Info(context.Background()); info.Target != api.DesktopTerminal || info.Label != "Terminal" {
		t.Fatalf("info %+v", info)
	}
	stale := filepath.Join(o.Dir, "s_oldoldold1.command")
	if err := os.MkdirAll(o.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stale, nil, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(stale, time.Now().Add(-48*time.Hour), time.Now().Add(-48*time.Hour)); err != nil {
		t.Fatal(err)
	}
	// A fixed clock: running the script below can take seconds under load,
	// and "right away" must not depend on that.
	start := time.Now()
	o.Now = func() time.Time { return start }
	res, err := o.Open(context.Background(), s)
	if _, err := os.Stat(stale); err == nil {
		t.Fatal("an old script was kept")
	}
	if err != nil || res.Target != api.DesktopTerminal || res.Label != "Terminal" || res.Reused {
		t.Fatalf("%+v %v", res, err)
	}
	file := filepath.Join(o.Dir, "s_jrqpq7oveb.command")
	last := f.calls[len(f.calls)-1]
	if want := []string{"/usr/bin/open", "-a", "Terminal", file}; !reflect.DeepEqual(last, want) {
		t.Fatalf("open call %q, want %q", last, want)
	}
	fi, err := os.Stat(file)
	if err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("command file %v %v", fi, err)
	}

	// Run the script with stand-ins: it must cd into the odd folder
	// literally and attach to the session.
	bin := t.TempDir()
	log := filepath.Join(bin, "log")
	if err := os.WriteFile(filepath.Join(bin, "pilothouse"), []byte("#!/bin/sh\necho \"$PWD|$*\" > \""+log+"\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	o.Pilothouse = filepath.Join(bin, "pilothouse")
	o.Env = []string{"PILOTHOUSE_RUN_DIR=/tmp/it's run", "bad name=x", "PILOTHOUSE_HOME=a\nb"}
	if err := os.WriteFile(filepath.Join(bin, "pilothouse"), []byte("#!/bin/sh\necho \"$PWD|$*|$PILOTHOUSE_RUN_DIR|${PILOTHOUSE_HOME:-unset}\" > \""+log+"\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(bin, "run.command")
	if err := os.WriteFile(script, []byte(o.commandFile(s)), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("/bin/sh", script)
	cmd.Env = append(os.Environ(), "SHELL=/usr/bin/true")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("script: %v %s", err, out)
	}
	b, _ := os.ReadFile(log)
	real, _ := filepath.EvalSymlinks(cwd)
	tail := "|attach --replay s_jrqpq7oveb|/tmp/it's run|unset"
	if got := strings.TrimSpace(string(b)); got != real+tail && got != cwd+tail {
		t.Fatalf("script ran %q", got)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(cwd), "pwned")); err == nil {
		t.Fatal("folder name was run as a command")
	}

	// A second open right away doesn't open a second window.
	n := f.n()
	if res, err := o.Open(context.Background(), s); err != nil || !res.Reused || f.n() != n+1 { // +1: the herdr probe
		t.Fatalf("%+v %v calls %d→%d", res, err, n, f.n())
	}
	// Later it does, in the background when not asked by a person.
	o.Now = func() time.Time { return time.Now().Add(time.Minute) }
	s.Focus = false
	if _, err := o.Open(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	if last := f.calls[len(f.calls)-1]; !reflect.DeepEqual(last[:4], []string{"/usr/bin/open", "-g", "-a", "Terminal"}) {
		t.Fatalf("background open %q", last)
	}
}

func TestUnavailable(t *testing.T) {
	for _, tc := range []struct{ mode, goos, msg string }{
		{ModeAuto, "linux", "herdr isn't running and there's no terminal app"},
		{ModeHerdr, "darwin", "herdr isn't running"},
		{ModeOff, "darwin", "turned off"},
		{ModeTerminal, "linux", "needs macOS"},
	} {
		f := newFake()
		f.running = false
		o := newOpener(t, f, tc.mode, tc.goos)
		if info := o.Info(context.Background()); info.Target != "" {
			t.Fatalf("%v: info %+v", tc, info)
		}
		_, err := o.Open(context.Background(), task)
		if !errors.Is(err, ErrUnavailable) || !strings.Contains(err.Error(), tc.msg) {
			t.Fatalf("%v: err %v", tc, err)
		}
	}
}

func TestRejectsBadInput(t *testing.T) {
	o := newOpener(t, newFake(), ModeAuto, "darwin")
	for _, id := range []string{"", "s_x; rm -rf ~", "../s_abc", "S_ABCDEF", "s_abc\n"} {
		s := task
		s.SessionID = id
		if _, err := o.Open(context.Background(), s); err == nil {
			t.Fatalf("accepted id %q", id)
		}
	}
	o.Pilothouse = "pilothouse"
	if _, err := o.Open(context.Background(), task); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("relative pilothouse path: %v", err)
	}
}

func TestInfoIsCached(t *testing.T) {
	f := newFake()
	o := newOpener(t, f, ModeAuto, "darwin")
	now := time.Now()
	o.Now = func() time.Time { return now }
	for range 3 {
		if info := o.Info(context.Background()); info.Target != api.DesktopHerdr {
			t.Fatalf("info %+v", info)
		}
	}
	if f.n() != 1 {
		t.Fatalf("probed %d times", f.n())
	}
	f.running = false
	now = now.Add(probeTTL + time.Second)
	if info := o.Info(context.Background()); info.Target != api.DesktopTerminal {
		t.Fatalf("after herdr quit: %+v", info)
	}
}

func TestHerdrSessionFlag(t *testing.T) {
	f := newFake()
	o := newOpener(t, f, ModeHerdr, "darwin")
	o.HerdrSession = "work"
	o.Info(context.Background())
	if got := f.calls[0]; !reflect.DeepEqual(got, []string{herdrBin, "--session", "work", "workspace", "list"}) {
		t.Fatalf("call %q", got)
	}
}

func TestPickWorkspace(t *testing.T) {
	ws := []herdrWorkspace{
		{ID: "w1", Label: "relay"},
		{ID: "w2", Label: "Other", Worktree: &struct {
			CheckoutPath string `json:"checkout_path"`
			RepoRoot     string `json:"repo_root"`
		}{"/src/app", "/src/app"}},
		{ID: "w3", Label: "task", Worktree: &struct {
			CheckoutPath string `json:"checkout_path"`
			RepoRoot     string `json:"repo_root"`
		}{"/wt/app/fix", "/wt/app/fix"}},
		{ID: "w4", Label: "Focused", Focused: true},
	}
	for _, tc := range []struct {
		s        Spec
		id, name string
	}{
		{Spec{Cwd: "/wt/app/fix", Repo: "/src/app"}, "w3", "task"}, // its own checkout first
		{Spec{Cwd: "/wt/app/other", Repo: "/src/app"}, "w2", "Other"},
		{Spec{Cwd: "/x", Repo: "/home/me/Relay"}, "w1", "relay"}, // by name, any case
		{Spec{Cwd: "/x", Repo: "/nowhere"}, "", "Focused"},
		{Spec{}, "", "Focused"},
	} {
		id, name := pickWorkspace(ws, tc.s)
		if id != tc.id || name != tc.name {
			t.Fatalf("%+v → %q %q, want %q %q", tc.s, id, name, tc.id, tc.name)
		}
	}
}

func TestHelpers(t *testing.T) {
	for in, want := range map[string]string{
		"/opt/homebrew/bin/pilothouse": "/opt/homebrew/bin/pilothouse",
		"/Users/a b/bin/pilothouse":    "'/Users/a b/bin/pilothouse'",
		"/x/it's":                      `'/x/it'\''s'`,
		"":                             "''",
	} {
		if got := shellQuote(in); got != want {
			t.Fatalf("shellQuote(%q) = %q, want %q", in, got, want)
		}
	}
	if got := label(Spec{SessionID: "s_abcd", Title: "  fix\x1b[31m the\nbug  "}); got != "fix[31m thebug" {
		t.Fatalf("label %q", got)
	}
	if got := label(Spec{SessionID: "s_abcd"}); got != "s_abcd" {
		t.Fatalf("empty label %q", got)
	}
	long := label(Spec{Title: strings.Repeat("é", 50)})
	if n := len([]rune(long)); n != maxLabel || !strings.HasSuffix(long, "…") {
		t.Fatalf("long label %q (%d)", long, n)
	}
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

func hasPrefix(xs []string, p string) bool {
	for _, v := range xs {
		if strings.HasPrefix(v, p) {
			return true
		}
	}
	return false
}

func TestHerdrNames(t *testing.T) {
	f := newFake()
	o := newOpener(t, f, ModeAuto, "darwin")
	got, err := o.HerdrNames(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"cc-harbor": "Halyard · Harbor",     // workspace · tab
		"cc-orbit":  "PageMind",             // herdr's own tab number adds nothing
		"cc-same":   "ERP",                  // nor a tab named like its workspace
		"ses_erp":   "ERP · claude session", // control characters and extra spaces dropped
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("names = %q, want %q", got, want)
	}

	f.mu.Lock()
	f.running = false
	f.mu.Unlock()
	if _, err := o.HerdrNames(context.Background()); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("herdr not running: %v", err)
	}
}

func TestHerdrName(t *testing.T) {
	long := strings.Repeat("x", 100)
	for _, c := range [][3]string{
		{"HarborNest", "UI HarborNest", "HarborNest · UI HarborNest"},
		{"HarborNest", "12", "HarborNest"},
		{"", "Tab", "Tab"},
		{"", "", ""},
		{"Ws", long, "Ws · " + strings.Repeat("x", maxName-len([]rune("Ws · "))-1) + "…"},
	} {
		if got := herdrName(c[0], c[1]); got != c[2] {
			t.Errorf("herdrName(%q, %q) = %q, want %q", c[0], c[1], got, c[2])
		}
	}
}
