package daemon

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sabr-software/pilothouse/internal/adapters"
	"github.com/sabr-software/pilothouse/internal/api"
	"github.com/sabr-software/pilothouse/internal/client"
	"github.com/sabr-software/pilothouse/internal/core"
	"github.com/sabr-software/pilothouse/internal/desktop"
	"github.com/sabr-software/pilothouse/internal/paths"
	"github.com/sabr-software/pilothouse/internal/task"
	"github.com/sabr-software/pilothouse/internal/wrap"
)

// deskRecorder is a running herdr as far as the Opener can tell.
type deskRecorder struct {
	mu    sync.Mutex
	calls []string
}

func (r *deskRecorder) run(_ context.Context, _ string, args ...string) ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, strings.Join(args, " "))
	switch strings.Join(args[:2], " ") {
	case "workspace list":
		return []byte(`{"result":{"workspaces":[{"workspace_id":"w1","label":"app","focused":true}]}}`), nil
	case "tab create":
		return []byte(`{"result":{"tab":{"tab_id":"w1:t2","workspace_id":"w1"},"root_pane":{"pane_id":"w1:p5"}}}`), nil
	case "pane run":
		return nil, nil // silent, like herdr
	}
	return []byte(`{"result":{}}`), nil
}

func (r *deskRecorder) got() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.calls...)
}

func deskHarness(t *testing.T, h *harness) *deskRecorder {
	t.Helper()
	rec := &deskRecorder{}
	o := desktop.New(desktop.ModeAuto, "", "", "/usr/local/bin/pilothouse", t.TempDir())
	o.LookPath = func(string) (string, error) { return "/bin/herdr", nil }
	o.Run = rec.run
	prev := h.opts
	h.opts = func(opts *Options) {
		if prev != nil {
			prev(opts)
		}
		opts.Desktop = o
	}
	return rec
}

func TestOpenOnDesktop(t *testing.T) {
	h := newHarness(t)
	rec := deskHarness(t, h)
	h.start(t)
	ctx := context.Background()

	if info := h.d.DesktopInfo(ctx); info.Target != api.DesktopHerdr || info.Machine == "" {
		t.Fatalf("info %+v", info)
	}

	hp := startWrapped(t, h, "sleep 30")
	res, err := h.d.OpenOnDesktop(ctx, hp.sid, "device:d_test")
	if err != nil || res.Target != api.DesktopHerdr || res.Where != "app" || res.Machine == "" {
		t.Fatalf("%+v %v", res, err)
	}
	if !contains(rec.got(), "pane run w1:p5 /usr/local/bin/pilothouse attach --replay "+hp.sid) {
		t.Fatalf("calls %q", rec.got())
	}

	// An observed session has no terminal to attach to.
	h.hook(ctx, t, adapters.HookEvent{Kind: adapters.HookSessionStart, Cwd: t.TempDir(), AgentSessionID: "v-obs"})
	ss, _ := h.c.Sessions(ctx)
	var observed string
	for _, s := range ss {
		if s.Mode == api.ModeObserved {
			observed = s.ID
		}
	}
	if _, err := h.d.OpenOnDesktop(ctx, observed, "device:d_test"); !errors.Is(err, core.ErrUnsupported) {
		t.Fatalf("observed: %v", err)
	}
	if _, err := h.d.OpenOnDesktop(ctx, "s_nosuchsess", "device:d_test"); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("unknown: %v", err)
	}

	// Once the agent is gone there's nothing to open.
	hp.stop()
	<-hp.done
	waitFor(t, "session ended", func() bool {
		_, err := h.d.OpenOnDesktop(ctx, hp.sid, "device:d_test")
		return errors.Is(err, core.ErrInvalid)
	})
}

func TestOpenOnDesktopUnavailable(t *testing.T) {
	h := newHarness(t)
	o := desktop.New(desktop.ModeHerdr, "", "", "/usr/local/bin/pilothouse", t.TempDir())
	o.LookPath = func(string) (string, error) { return "/bin/herdr", nil }
	o.Run = func(context.Context, string, ...string) ([]byte, error) { // never the real herdr
		return nil, &desktop.RunError{Err: errors.New("exit status 1"), Output: `{"error":{"code":"server_not_running","message":"no herdr server is running"}}`}
	}
	h.opts = func(opts *Options) { opts.Desktop = o }
	h.start(t)
	hp := startWrapped(t, h, "sleep 30")
	if info := h.d.DesktopInfo(context.Background()); info.Target != "" {
		t.Fatalf("info %+v", info)
	}
	_, err := h.d.OpenOnDesktop(context.Background(), hp.sid, "device:d_test")
	if !errors.Is(err, core.ErrUnsupported) || !strings.Contains(err.Error(), "herdr isn't running") {
		t.Fatalf("err %v", err)
	}
}

// holderStarter starts tasks under real in-process holders.
type holderStarter struct {
	sids  chan string
	mu    sync.Mutex
	stops []func()
}

func (s *holderStarter) start(_ context.Context, spec task.StartSpec) (string, error) {
	sid := wrap.NewSessionID()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = wrap.RunHolder(ctx, wrap.HolderConfig{SessionID: sid, TaskID: spec.TaskID, Name: spec.Name, Dir: spec.Dir,
			Argv: []string{"/bin/sh", "-c", "sleep 30"}, Linger: 200 * time.Millisecond, Daemon: client.NewSocket(paths.Socket())})
	}()
	s.mu.Lock()
	s.stops = append(s.stops, func() { cancel(); <-done })
	s.mu.Unlock()
	s.sids <- sid
	return sid, nil
}

func TestTasksOpenOnDesktop(t *testing.T) {
	for _, on := range []bool{true, false} {
		t.Run(map[bool]string{true: "on", false: "off"}[on], func(t *testing.T) {
			h := newHarness(t)
			taskEnv(t)
			hs := &holderStarter{sids: make(chan string, 1)}
			h.opts = func(o *Options) { o.TaskStarter = hs.start }
			rec := deskHarness(t, h)
			h.cfg.Desktop.OpenTasks = on
			repo := newGitRepo(t, "app")
			h.start(t)
			t.Cleanup(func() {
				hs.mu.Lock()
				defer hs.mu.Unlock()
				for _, f := range hs.stops {
					f()
				}
			})
			h.sessionIn(t, repo, "v-app")
			tk, err := h.d.StartTask(bg, api.TaskStart{Repo: repo, Prompt: "Fix the bug"}, "device:d_test")
			if err != nil {
				t.Fatal(err)
			}
			sid := <-hs.sids
			want := "tab create --workspace w1 --cwd " + tk.Worktree + " --label fix-the-bug --no-focus"
			if !on {
				time.Sleep(1500 * time.Millisecond)
				if len(rec.got()) != 0 {
					t.Fatalf("opened with open_tasks off: %q", rec.got())
				}
				return
			}
			waitFor(t, "task opened in herdr", func() bool {
				return contains(rec.got(), "pane run w1:p5 /usr/local/bin/pilothouse attach --replay "+sid)
			})
			calls := rec.got()
			if !contains(calls, want) {
				t.Fatalf("calls %q\nwant %q", calls, want)
			}
			for _, c := range calls {
				if strings.Contains(c, "focus w") {
					t.Fatalf("a task opened by itself took focus: %q", c)
				}
			}
		})
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

// Sessions running in herdr tabs take herdr's names, matched by the
// agent's own session id; task sessions and sessions herdr doesn't run
// keep theirs, and a rename isn't activity.
func TestSyncHerdrNames(t *testing.T) {
	h := newHarness(t)
	o := desktop.New(desktop.ModeAuto, "", "", "/usr/local/bin/pilothouse", t.TempDir())
	o.LookPath = func(string) (string, error) { return "/bin/herdr", nil }
	var mu sync.Mutex
	tab := "UI HarborNest"
	o.Run = func(_ context.Context, _ string, args ...string) ([]byte, error) {
		mu.Lock()
		defer mu.Unlock()
		switch strings.Join(args[:2], " ") {
		case "agent list":
			return []byte(`{"result":{"agents":[{"agent_session":{"value":"cc-harbor"},"tab_id":"w6:t2","workspace_id":"w6"}]}}`), nil
		case "tab list":
			return []byte(`{"result":{"tabs":[{"tab_id":"w6:t2","label":"` + tab + `"}]}}`), nil
		case "workspace list":
			return []byte(`{"result":{"workspaces":[{"workspace_id":"w6","label":"HarborNest"}]}}`), nil
		}
		return []byte(`{"result":{}}`), nil
	}
	h.opts = func(opts *Options) { opts.Desktop = o }
	h.start(t)
	ctx := context.Background()

	dir := t.TempDir()
	h.hook(ctx, t, adapters.HookEvent{Kind: adapters.HookSessionStart, Cwd: dir, AgentSessionID: "cc-harbor"})
	h.hook(ctx, t, adapters.HookEvent{Kind: adapters.HookSessionStart, Cwd: t.TempDir(), AgentSessionID: "cc-elsewhere"})
	find := func(agentID string) api.Session {
		ss, _ := h.c.Sessions(ctx)
		for _, s := range ss {
			if s.AgentSessionID == agentID {
				return s
			}
		}
		t.Fatalf("no session %s", agentID)
		return api.Session{}
	}
	before := find("cc-harbor")
	other := find("cc-elsewhere").Title

	h.d.syncHerdrNames(ctx)
	got := find("cc-harbor")
	if got.Title != "HarborNest · UI HarborNest" {
		t.Fatalf("title %q", got.Title)
	}
	if !got.UpdatedAt.Equal(before.UpdatedAt) {
		t.Errorf("a rename moved UpdatedAt: %v → %v", before.UpdatedAt, got.UpdatedAt)
	}
	if find("cc-elsewhere").Title != other {
		t.Errorf("a session herdr doesn't run was renamed")
	}

	// Renaming the tab in herdr follows; nothing changed writes nothing.
	mu.Lock()
	tab = "Release"
	mu.Unlock()
	h.d.syncHerdrNames(ctx)
	if got := find("cc-harbor").Title; got != "HarborNest · Release" {
		t.Fatalf("after rename: %q", got)
	}
	seq := func() int64 { s, _ := h.c.Snapshot(ctx); return s.Seq }
	n := seq()
	h.d.syncHerdrNames(ctx)
	if seq() != n {
		t.Errorf("an unchanged name emitted an event")
	}
}
