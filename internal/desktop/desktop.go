// Package desktop shows a wrapped session on the computer itself: a new
// tab in the running herdr, or (on macOS) a terminal window, attached to
// the session with `pilothouse attach`. Pilothouse tasks run under a
// detached holder, so without this they appear nowhere on the desktop.
//
// Nothing a device sends reaches a shell: the command typed into a herdr
// pane is built from the pilothouse path and a session id that must match
// sessionIDRE; the folder and label go to herdr as separate arguments; the
// macOS .command file quotes the folder.
package desktop

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/xasdta/pilothouse/internal/api"
)

// Modes for config `desktop.open`.
const (
	ModeAuto     = "auto"     // herdr when it runs, else the terminal app (macOS)
	ModeHerdr    = "herdr"    // herdr only
	ModeTerminal = "terminal" // the terminal app only (macOS)
	ModeOff      = "off"
)

// ErrUnavailable: nothing to open the session in (herdr not running, no
// terminal app on this OS, or turned off). The message says which.
var ErrUnavailable = errors.New("unavailable")

const (
	callTimeout  = 5 * time.Second
	probeTTL     = 10 * time.Second
	reopenWindow = 4 * time.Second // a second open of the same session in a terminal window is dropped
	maxLabel     = 32
	// maxRemembered bounds the herdr tabs remembered for reuse.
	maxRemembered = 256
	// commandFileAge is how long a terminal window's script is kept.
	commandFileAge = 24 * time.Hour
)

var (
	sessionIDRE = regexp.MustCompile(`^s_[a-z0-9]{4,40}$`)
	envNameRE   = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`)
)

// Spec is the session to open.
type Spec struct {
	SessionID string
	Title     string // tab label
	Cwd       string // the session's folder
	Repo      string // its repository (the task's main checkout for a task)
	// Focus brings it to the front (a person asked); off for tasks opened
	// automatically, so they don't take over the screen.
	Focus bool
}

// Opener opens sessions on the desktop. The zero value is not usable; use New.
type Opener struct {
	Mode         string
	TerminalApp  string // macOS app for .command files; default "Terminal"
	HerdrSession string // herdr --session name; empty = herdr's default session
	Pilothouse   string // the pilothouse binary typed into the tab
	Dir          string // where .command files are written (private)
	GOOS         string
	// Env is KEY=VALUE pairs the attach command needs (the daemon's
	// PILOTHOUSE_HOME and _RUN_DIR when they aren't the defaults).
	Env []string

	// LookPath and Run are exec.LookPath and running a command; tests
	// replace them.
	LookPath func(string) (string, error)
	Run      func(ctx context.Context, name string, args ...string) (stdout []byte, err error)
	Now      func() time.Time

	mu      sync.Mutex           // one open at a time
	tabs    map[string]herdrTab  // session id → the herdr tab opened for it
	windows map[string]time.Time // session id → last terminal window

	probeMu sync.Mutex // Info doesn't wait for an open in progress
	probe   probeResult
}

// herdrTab is a tab Pilothouse opened. herdr numbers tabs and panes per
// server run, so after a herdr restart the same ids can name someone
// else's tab: the terminal id (random per terminal) tells them apart.
type herdrTab struct{ tab, pane, terminal, workspace string }

type probeResult struct {
	at   time.Time
	info api.DesktopInfo
	err  error
}

// New returns an Opener with the real exec functions.
func New(mode, terminalApp, herdrSession, pilothouse, dir string) *Opener {
	if mode == "" {
		mode = ModeAuto
	}
	if terminalApp == "" {
		terminalApp = "Terminal"
	}
	return &Opener{
		Mode: mode, TerminalApp: terminalApp, HerdrSession: herdrSession, Pilothouse: pilothouse, Dir: dir,
		GOOS: runtime.GOOS, LookPath: exec.LookPath, Run: runCmd, Now: time.Now,
	}
}

// runCmd runs a command without a shell. The herdr pane variables are
// dropped: a daemon started from inside herdr would otherwise target the
// pane it was started in.
func runCmd(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	env := os.Environ()[:0:0]
	for _, kv := range os.Environ() {
		switch strings.SplitN(kv, "=", 2)[0] {
		case "HERDR_ENV", "HERDR_PANE_ID", "HERDR_TAB_ID", "HERDR_WORKSPACE_ID":
			continue
		}
		env = append(env, kv)
	}
	cmd.Env = env
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	if err != nil {
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = strings.TrimSpace(out.String())
		}
		return out.Bytes(), &RunError{Err: err, Output: msg}
	}
	return out.Bytes(), nil
}

// RunError is a command that failed, with what it printed.
type RunError struct {
	Err    error
	Output string
}

func (e *RunError) Error() string {
	if e.Output == "" {
		return e.Err.Error()
	}
	return e.Err.Error() + ": " + e.Output
}

// Info says where a session would open now. Cached for a few seconds:
// the web app asks on every session page.
func (o *Opener) Info(ctx context.Context) api.DesktopInfo {
	info, _ := o.Check(ctx)
	return info
}

// Check is Info with the reason when nothing is available.
func (o *Opener) Check(ctx context.Context) (api.DesktopInfo, error) {
	return o.probeInfo(ctx, false)
}

func (o *Opener) probeInfo(ctx context.Context, fresh bool) (api.DesktopInfo, error) {
	o.probeMu.Lock()
	defer o.probeMu.Unlock()
	now := o.Now()
	if !fresh && !o.probe.at.IsZero() && now.Sub(o.probe.at) < probeTTL {
		return o.probe.info, o.probe.err
	}
	info, err := o.detect(ctx)
	o.probe = probeResult{at: now, info: info, err: err}
	return info, err
}

func (o *Opener) detect(ctx context.Context) (api.DesktopInfo, error) {
	switch o.Mode {
	case ModeOff:
		return api.DesktopInfo{}, fmt.Errorf("%w: opening sessions on this computer is turned off (desktop.open: off)", ErrUnavailable)
	case ModeTerminal:
		return o.terminalInfo()
	}
	_, herdrErr := o.herdrWorkspaces(ctx)
	if herdrErr == nil {
		return api.DesktopInfo{Target: api.DesktopHerdr, Label: "herdr"}, nil
	}
	if o.Mode == ModeHerdr {
		return api.DesktopInfo{}, herdrErr
	}
	if info, err := o.terminalInfo(); err == nil {
		return info, nil
	}
	return api.DesktopInfo{}, fmt.Errorf("%w: herdr isn't running and there's no terminal app to open on this system", ErrUnavailable)
}

func (o *Opener) terminalInfo() (api.DesktopInfo, error) {
	if o.GOOS != "darwin" {
		return api.DesktopInfo{}, fmt.Errorf("%w: opening a terminal window needs macOS; on %s, run herdr and Pilothouse opens a tab there", ErrUnavailable, o.GOOS)
	}
	return api.DesktopInfo{Target: api.DesktopTerminal, Label: o.TerminalApp}, nil
}

// Open shows the session on the desktop.
func (o *Opener) Open(ctx context.Context, s Spec) (api.DesktopOpened, error) {
	if !sessionIDRE.MatchString(s.SessionID) {
		return api.DesktopOpened{}, fmt.Errorf("invalid session id %q", s.SessionID)
	}
	if o.Pilothouse == "" || !filepath.IsAbs(o.Pilothouse) {
		return api.DesktopOpened{}, fmt.Errorf("%w: can't find the pilothouse program to attach with", ErrUnavailable)
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	o.initMaps()
	info, err := o.probeInfo(ctx, true) // look again: herdr may have started or quit
	if err != nil {
		return api.DesktopOpened{}, err
	}
	if info.Target == api.DesktopHerdr {
		return o.openHerdr(ctx, s)
	}
	return o.openTerminal(ctx, s)
}

// ---- herdr ----

type herdrWorkspace struct {
	ID       string `json:"workspace_id"`
	Label    string `json:"label"`
	Focused  bool   `json:"focused"`
	Worktree *struct {
		CheckoutPath string `json:"checkout_path"`
		RepoRoot     string `json:"repo_root"`
	} `json:"worktree"`
}

type herdrError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (o *Opener) herdrBin() (string, error) {
	if p, err := o.LookPath("herdr"); err == nil {
		return p, nil
	}
	home, _ := os.UserHomeDir()
	for _, p := range []string{"/opt/homebrew/bin/herdr", "/usr/local/bin/herdr", filepath.Join(home, ".local/bin/herdr"), filepath.Join(home, ".cargo/bin/herdr")} {
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() && fi.Mode()&0o111 != 0 {
			return p, nil
		}
	}
	return "", fmt.Errorf("%w: herdr isn't installed", ErrUnavailable)
}

// herdr runs one herdr command and returns its "result" object.
func (o *Opener) herdr(ctx context.Context, args ...string) (json.RawMessage, error) {
	bin, err := o.herdrBin()
	if err != nil {
		return nil, err
	}
	what := "herdr " + strings.Join(args[:min(2, len(args))], " ")
	if o.HerdrSession != "" {
		args = append([]string{"--session", o.HerdrSession}, args...)
	}
	cctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	out, runErr := o.Run(cctx, bin, args...)
	var resp struct {
		Result json.RawMessage `json:"result"`
		Error  *herdrError     `json:"error"`
	}
	text := out
	var re *RunError
	if runErr != nil && errors.As(runErr, &re) && len(bytes.TrimSpace(text)) == 0 {
		text = []byte(re.Output) // herdr prints errors as JSON on stderr
	}
	if jerr := json.Unmarshal(bytes.TrimSpace(text), &resp); jerr == nil && resp.Error != nil {
		if resp.Error.Code == "server_not_running" {
			return nil, fmt.Errorf("%w: herdr isn't running", ErrUnavailable)
		}
		return nil, fmt.Errorf("%s: %s", what, resp.Error.Message)
	}
	if runErr != nil {
		if cctx.Err() == context.DeadlineExceeded {
			return nil, fmt.Errorf("%s: herdr didn't answer within %s", what, callTimeout)
		}
		return nil, fmt.Errorf("%s: %w", what, runErr)
	}
	if len(bytes.TrimSpace(out)) == 0 {
		return nil, nil // `pane run` succeeds silently
	}
	if len(resp.Result) == 0 {
		return nil, fmt.Errorf("%s: unexpected output %q", what, truncate(string(out), 200))
	}
	return resp.Result, nil
}

func (o *Opener) herdrWorkspaces(ctx context.Context) ([]herdrWorkspace, error) {
	res, err := o.herdr(ctx, "workspace", "list")
	if err != nil {
		return nil, err
	}
	var v struct {
		Workspaces []herdrWorkspace `json:"workspaces"`
	}
	if err := json.Unmarshal(res, &v); err != nil {
		return nil, fmt.Errorf("herdr workspace list: %w", err)
	}
	return v.Workspaces, nil
}

// pickWorkspace chooses the workspace for a session: the one checked out
// in its folder, then the one for its repository, then one named after
// the repository. "" lets herdr use the focused workspace.
func pickWorkspace(ws []herdrWorkspace, s Spec) (id, label string) {
	same := func(a, b string) bool { return a != "" && b != "" && filepath.Clean(a) == filepath.Clean(b) }
	for _, w := range ws {
		if w.Worktree != nil && same(w.Worktree.CheckoutPath, s.Cwd) {
			return w.ID, w.Label
		}
	}
	for _, w := range ws {
		if w.Worktree != nil && (same(w.Worktree.CheckoutPath, s.Repo) || same(w.Worktree.RepoRoot, s.Repo)) {
			return w.ID, w.Label
		}
	}
	if s.Repo != "" {
		base := filepath.Base(s.Repo)
		for _, w := range ws {
			if strings.EqualFold(strings.TrimSpace(w.Label), base) {
				return w.ID, w.Label
			}
		}
	}
	for _, w := range ws {
		if w.Focused {
			return "", w.Label
		}
	}
	return "", ""
}

func (o *Opener) openHerdr(ctx context.Context, s Spec) (api.DesktopOpened, error) {
	// Finish what was started even if the device goes away: a tab created
	// without its attach command would be left empty. Each herdr call
	// has its own timeout.
	ctx = context.WithoutCancel(ctx)
	out := api.DesktopOpened{Target: api.DesktopHerdr, Label: "herdr"}
	ws, err := o.herdrWorkspaces(ctx)
	if err != nil {
		return out, err
	}
	// Already open: bring that tab forward instead of adding another.
	if prev, ok := o.tabs[s.SessionID]; ok {
		if o.herdrStillOurs(ctx, prev) {
			if s.Focus {
				// Detached there earlier: the shell is back at its prompt.
				if o.herdrShellIdle(ctx, prev.pane) {
					if _, err := o.herdr(ctx, "pane", "run", prev.pane, o.attachCommand(s.SessionID)); err != nil {
						return out, err
					}
				}
				if err := o.focusHerdr(ctx, prev); err != nil {
					return out, err
				}
			}
			out.Reused = true
			out.Where = workspaceLabel(ws, prev.workspace)
			return out, nil
		}
		delete(o.tabs, s.SessionID) // closed, or herdr restarted
	}

	wsID, wsLabel := pickWorkspace(ws, s)
	args := []string{"tab", "create"}
	if wsID != "" {
		args = append(args, "--workspace", wsID)
	}
	if s.Cwd != "" && filepath.IsAbs(s.Cwd) {
		if fi, err := os.Stat(s.Cwd); err == nil && fi.IsDir() {
			args = append(args, "--cwd", s.Cwd)
		}
	}
	for _, kv := range o.Env {
		args = append(args, "--env", kv)
	}
	args = append(args, "--label", label(s))
	if s.Focus {
		args = append(args, "--focus")
	} else {
		args = append(args, "--no-focus")
	}
	res, err := o.herdr(ctx, args...)
	if err != nil {
		return out, err
	}
	var created struct {
		Tab struct {
			ID        string `json:"tab_id"`
			Workspace string `json:"workspace_id"`
		} `json:"tab"`
		Pane struct {
			ID       string `json:"pane_id"`
			Terminal string `json:"terminal_id"`
		} `json:"root_pane"`
	}
	if err := json.Unmarshal(res, &created); err != nil || created.Tab.ID == "" || created.Pane.ID == "" {
		return out, fmt.Errorf("herdr tab create: unexpected result %s", truncate(string(res), 200))
	}
	t := herdrTab{tab: created.Tab.ID, pane: created.Pane.ID, terminal: created.Pane.Terminal, workspace: created.Tab.Workspace}
	if _, err := o.herdr(ctx, "pane", "run", created.Pane.ID, o.attachCommand(s.SessionID)); err != nil {
		// Don't leave an empty tab behind.
		_, _ = o.herdr(context.WithoutCancel(ctx), "tab", "close", t.tab)
		return out, err
	}
	o.tabs[s.SessionID] = t
	if s.Focus {
		if err := o.focusHerdr(ctx, t); err != nil {
			return out, err
		}
	}
	out.Where = workspaceLabel(ws, t.workspace)
	if out.Where == "" {
		out.Where = wsLabel
	}
	return out, nil
}

// herdrStillOurs reports whether t is still the tab Pilothouse opened:
// its pane exists, is in that tab and runs the same terminal.
func (o *Opener) herdrStillOurs(ctx context.Context, t herdrTab) bool {
	if t.terminal == "" {
		return false
	}
	res, err := o.herdr(ctx, "pane", "get", t.pane)
	if err != nil {
		return false
	}
	var v struct {
		Pane struct {
			Tab      string `json:"tab_id"`
			Terminal string `json:"terminal_id"`
		} `json:"pane"`
	}
	return json.Unmarshal(res, &v) == nil && v.Pane.Tab == t.tab && v.Pane.Terminal == t.terminal
}

// herdrShellIdle reports whether the pane's shell is at its prompt (no
// foreground job). Unknown counts as busy: typing into a running program
// would be worse than not re-attaching.
func (o *Opener) herdrShellIdle(ctx context.Context, pane string) bool {
	if pane == "" {
		return false
	}
	res, err := o.herdr(ctx, "pane", "process-info", "--pane", pane)
	if err != nil {
		return false
	}
	var v struct {
		Info struct {
			Group int `json:"foreground_process_group_id"`
			Shell int `json:"shell_pid"`
		} `json:"process_info"`
	}
	return json.Unmarshal(res, &v) == nil && v.Info.Shell > 0 && v.Info.Group == v.Info.Shell
}

func (o *Opener) focusHerdr(ctx context.Context, t herdrTab) error {
	if t.workspace != "" {
		if _, err := o.herdr(ctx, "workspace", "focus", t.workspace); err != nil {
			return err
		}
	}
	_, err := o.herdr(ctx, "tab", "focus", t.tab)
	return err
}

func workspaceLabel(ws []herdrWorkspace, id string) string {
	for _, w := range ws {
		if w.ID == id {
			return w.Label
		}
	}
	return ""
}

// attachCommand is the line typed into the new tab's shell.
func (o *Opener) attachCommand(sessionID string) string {
	return shellQuote(o.Pilothouse) + " attach --replay " + sessionID
}

// ---- terminal window (macOS) ----

func (o *Opener) openTerminal(ctx context.Context, s Spec) (api.DesktopOpened, error) {
	out := api.DesktopOpened{Target: api.DesktopTerminal, Label: o.TerminalApp}
	now := o.Now()
	if last, ok := o.windows[s.SessionID]; ok && now.Sub(last) < reopenWindow {
		out.Reused = true
		return out, nil
	}
	if err := os.MkdirAll(o.Dir, 0o700); err != nil {
		return out, err
	}
	o.pruneCommandFiles(now)
	f := filepath.Join(o.Dir, s.SessionID+".command")
	if err := os.WriteFile(f, []byte(o.commandFile(s)), 0o700); err != nil {
		return out, err
	}
	if err := os.Chmod(f, 0o700); err != nil { // WriteFile keeps an existing file's mode
		return out, err
	}
	args := []string{}
	if !s.Focus {
		args = append(args, "-g") // don't bring the app to the front
	}
	args = append(args, "-a", o.TerminalApp, f)
	cctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	if _, err := o.Run(cctx, "/usr/bin/open", args...); err != nil {
		return out, fmt.Errorf("couldn't open %s: %w", o.TerminalApp, err)
	}
	o.windows[s.SessionID] = now
	return out, nil
}

// pruneCommandFiles removes scripts of earlier sessions: the terminal
// app reads one only as its window opens.
func (o *Opener) pruneCommandFiles(now time.Time) {
	old, _ := filepath.Glob(filepath.Join(o.Dir, "s_*.command"))
	for _, f := range old {
		if fi, err := os.Stat(f); err == nil && now.Sub(fi.ModTime()) > commandFileAge {
			_ = os.Remove(f)
		}
	}
}

// commandFile is a script the terminal app runs in a new window: attach,
// then leave a shell in the session's folder.
func (o *Opener) commandFile(s Spec) string {
	var b strings.Builder
	b.WriteString("#!/bin/sh\n# Opened by Pilothouse for session " + s.SessionID + ".\n")
	if s.Cwd != "" && filepath.IsAbs(s.Cwd) && !strings.ContainsAny(s.Cwd, "\x00\n\r") {
		b.WriteString("cd " + shellQuote(s.Cwd) + " 2>/dev/null\n")
	}
	for _, kv := range o.Env {
		if k, v, ok := strings.Cut(kv, "="); ok && envNameRE.MatchString(k) && !strings.ContainsAny(v, "\x00\n\r") {
			b.WriteString("export " + k + "=" + shellQuote(v) + "\n")
		}
	}
	b.WriteString(o.attachCommand(s.SessionID) + "\n")
	b.WriteString("exec \"${SHELL:-/bin/zsh}\" -l\n")
	return b.String()
}

// ---- helpers ----

// shellQuote quotes s for POSIX shells and fish; plain paths stay bare.
func shellQuote(s string) string {
	if s != "" && strings.IndexFunc(s, func(r rune) bool {
		return !(r < unicode.MaxASCII && (unicode.IsLetter(r) || unicode.IsDigit(r) || strings.ContainsRune("/._-+:@", r)))
	}) < 0 {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// label is the herdr tab name: the session's title, printable, short.
func label(s Spec) string {
	t := strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, strings.TrimSpace(s.Title))
	if t == "" {
		t = s.SessionID
	}
	return truncate(t, maxLabel)
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

// initMaps makes the maps and keeps them small: windows only matter for
// a few seconds, and a remembered tab is checked before it is reused, so
// forgetting the oldest ones only costs a new tab.
func (o *Opener) initMaps() {
	if o.tabs == nil {
		o.tabs = map[string]herdrTab{}
	}
	if o.windows == nil {
		o.windows = map[string]time.Time{}
	}
	now := o.Now()
	for id, at := range o.windows {
		if now.Sub(at) >= reopenWindow {
			delete(o.windows, id)
		}
	}
	for id := range o.tabs {
		if len(o.tabs) <= maxRemembered {
			break
		}
		delete(o.tabs, id)
	}
}
