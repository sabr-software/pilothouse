package daemon

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sabr-software/pilothouse/internal/adapters"
	"github.com/sabr-software/pilothouse/internal/api"
	"github.com/sabr-software/pilothouse/internal/client"
	"github.com/sabr-software/pilothouse/internal/core"
	"github.com/sabr-software/pilothouse/internal/policy"
)

// The web package's local routes (pair, login-link, devices) are mounted on
// the Unix socket and guarded by the daemon's agent-caller guard.
func TestWebLocalRoutesGuarded(t *testing.T) {
	h := newHarness(t)
	h.start(t)
	ps, err := h.c.Pair(bg)
	if err != nil || ps.Code == "" || ps.URL == "" {
		t.Fatalf("pair from a person's terminal = %+v, %v", ps, err)
	}
	if _, err := h.c.Devices(bg); err != nil {
		t.Fatalf("devices: %v", err)
	}

	h.rejectCallers(errors.New("called from inside an agent"))
	for name, err := range map[string]error{
		"pair":       func() error { _, err := h.c.Pair(bg); return err }(),
		"login-link": func() error { _, err := h.c.LoginLink(bg); return err }(),
		"devices":    func() error { _, err := h.c.Devices(bg); return err }(),
		"revoke":     h.c.RevokeDevice(bg, "d_x"),
	} {
		if !client.IsCode(err, "agent_caller") {
			t.Errorf("%s under the guard: %v, want 403 agent_caller", name, err)
		}
	}
}

func TestNotifierUsesStandardWordingAndIsClosed(t *testing.T) {
	h := newHarness(t)
	h.presence.setAway(true)
	h.start(t)
	waitFor(t, "away", func() bool { return h.d.Presence().State == api.PresenceAway })
	h.hook(bg, t, adapters.HookEvent{Kind: adapters.HookStop, Message: "Done."})
	if err := h.c.NotifyTest(bg); err != nil {
		t.Fatal(err)
	}
	ns := h.notes.all()
	if len(ns) != 2 || ns[0].Kind != core.NotifyWaiting || ns[1].Kind != core.NotifyTest {
		t.Fatalf("notifications = %+v", ns)
	}
	for _, n := range ns {
		if n.Title != "" || n.Body != "" || n.URL != "" {
			t.Errorf("%s notification sets its own wording: %+v", n.Kind, n)
		}
	}
	h.stop(t)
	if !h.notes.closed.Load() {
		t.Fatal("notifier not closed on shutdown")
	}
}

// policy.decision events carry policy.DecisionEvent, so `pilothouse policy
// replay` pairs each logged decision with its pre_tool call.
func TestPolicyReplayPairsDecisions(t *testing.T) {
	h := newHarness(t)
	h.policy.set("rm -rf build", api.BehaviorDeny, "no-rm")
	h.start(t)
	for i, cmd := range []string{"rm -rf build", "ls", "rm -rf build"} {
		ev := shell(cmd)
		ev.ToolUseID = "tu-" + string(rune('a'+i))
		h.hook(bg, t, ev)
	}
	evs, err := h.c.Events(bg, 0)
	if err != nil {
		t.Fatal(err)
	}
	snap, _ := h.c.Snapshot(bg)
	sessions := map[string]api.Session{}
	for _, s := range snap.Sessions {
		sessions[s.ID] = s
	}
	// Today's rules: none (an empty user file), so the deny would change.
	eng, _ := policy.Load(policy.Options{UserFile: filepath.Join(t.TempDir(), "policy.yaml")})
	rep := policy.Replay(eng, evs, time.Time{}, func(id string) (api.Session, bool) {
		s, ok := sessions[id]
		return s, ok
	})
	if rep.Calls != 3 || len(rep.Items) != 3 {
		t.Fatalf("replay = %+v", rep)
	}
	want := []struct {
		then    api.Behavior
		rule    string
		changed bool
	}{{api.BehaviorDeny, "no-rm", true}, {"", "", false}, {api.BehaviorDeny, "no-rm", true}}
	for i, w := range want {
		it := rep.Items[i]
		if it.Then != w.then || it.ThenRule != w.rule || it.Changed != w.changed {
			t.Errorf("item %d (%s) = then %q/%q changed %v; want %q/%q %v",
				i, it.Summary, it.Then, it.ThenRule, it.Changed, w.then, w.rule, w.changed)
		}
	}
	if rep.Changed != 2 {
		t.Errorf("changed = %d, want 2", rep.Changed)
	}
}

// Decision.By comes from the X-Pilothouse-Client header: cli or tui,
// anything else (or nothing) is recorded as cli.
func TestDecisionByFromClientHeader(t *testing.T) {
	if client.ClientHeader != "X-Pilothouse-Client" {
		t.Fatalf("client header = %q", client.ClientHeader)
	}
	h := newHarness(t)
	h.start(t)
	for i, tc := range []struct{ name, want string }{{"", "cli"}, {"tui", "tui"}, {"cli", "cli"}, {"device:d_x", "cli"}} {
		h.hook(bg, t, permission("make step"+string(rune('0'+i)), ""))
		rs, _ := h.c.Requests(bg, api.RequestSteppedAside)
		if len(rs) != 1 {
			t.Fatalf("stepped aside = %+v", rs)
		}
		c := h.c
		if tc.name != "" {
			c = c.WithClientName(tc.name)
		}
		r, err := c.Decide(bg, rs[0].ID, api.DecideBody{Behavior: api.BehaviorDeny})
		if err != nil {
			t.Fatal(err)
		}
		if r.Decision.By != tc.want {
			t.Errorf("header %q → By %q, want %q", tc.name, r.Decision.By, tc.want)
		}
	}
}

// A session grant answers the permission checkpoint directly — unless the
// policy said ask/deny, and policy allow is never applied there (Codex's
// pre_tool can't express ask/allow, so its permission checkpoint must still
// reach a person).
func TestGrantsAtPermissionCheckpoint(t *testing.T) {
	h := newHarness(t)
	h.presence.setAway(true)
	h.policy.set("git push origin main", api.BehaviorAsk, "ask-main")
	h.policy.set("npm publish", api.BehaviorAllow, "allow-publish")
	h.start(t)
	waitFor(t, "away", func() bool { return h.d.Presence().State == api.PresenceAway })

	// Earn a grant for `git push` by answering "allow for this session".
	held := make(chan fakeOutcome, 1)
	go func() { held <- h.hook(bg, t, permission("git push origin dev", "")) }()
	req := h.openRequest(t)
	if _, err := h.c.Decide(bg, req.ID, api.DecideBody{Behavior: api.BehaviorAllow, Scope: api.ScopeSession}); err != nil {
		t.Fatal(err)
	}
	<-held

	// Covered by the grant: allowed at once, no routing, no notification.
	before := len(h.notes.all())
	start := time.Now()
	out := h.hook(bg, t, permission("git push origin feature", "tu-g"))
	if out.Behavior != api.BehaviorAllow || time.Since(start) > 2*time.Second {
		t.Fatalf("granted permission = %+v after %s", out, time.Since(start))
	}
	decided, _ := h.c.Requests(bg, api.RequestDecided)
	if len(decided) != 2 || !strings.HasPrefix(decided[0].Decision.By, "grant:g_") || decided[0].Route != "" {
		t.Fatalf("granted request = %+v", decided[0])
	}
	if len(h.notes.all()) != before {
		t.Fatal("a granted request notified someone")
	}

	// Policy ask (Codex pre_tool turned it into no-opinion) and policy allow
	// both still go to a person.
	for _, cmd := range []string{"git push origin main", "npm publish"} {
		res := make(chan fakeOutcome, 1)
		go func() { res <- h.hook(bg, t, permission(cmd, "")) }()
		r := h.openRequest(t)
		if r.Command != cmd || r.Route != api.RouteDevices {
			t.Fatalf("%s: request = %+v", cmd, r)
		}
		if _, err := h.c.Decide(bg, r.ID, api.DecideBody{Behavior: api.BehaviorDeny, Message: "not now"}); err != nil {
			t.Fatal(err)
		}
		if out := <-res; out.Behavior != api.BehaviorDeny {
			t.Fatalf("%s: hook = %+v", cmd, out)
		}
	}
}

// `pilothouse task new` registers the task before its session exists; the
// session links it when it shows up, through the holder or a hook.
func TestTaskRegisteredBeforeSession(t *testing.T) {
	h := newHarness(t)
	h.start(t)

	viaHolder, err := h.c.PutTask(bg, api.Task{Title: "a", Prompt: "do a", Agent: fakeKind, Status: api.TaskRunning})
	if err != nil || viaHolder.SessionID != "" {
		t.Fatalf("put task = %+v, %v", viaHolder, err)
	}
	s, err := h.c.RegisterWrapped(bg, api.WrappedSessionBody{SessionID: "s_taskholder1", Agent: fakeKind,
		Cwd: t.TempDir(), PID: 9001, TaskID: viaHolder.ID})
	if err != nil {
		t.Fatal(err)
	}
	if tk, _ := h.c.Task(bg, viaHolder.ID); tk.SessionID != s.ID || s.TaskID != viaHolder.ID {
		t.Fatalf("holder: task %+v, session task %q", tk, s.TaskID)
	}

	viaHook, err := h.c.PutTask(bg, api.Task{Title: "b", Prompt: "do b", Agent: fakeKind})
	if err != nil {
		t.Fatal(err)
	}
	ev := adapters.HookEvent{Kind: adapters.HookSessionStart, AgentSessionID: "vendor-task-b"}
	if _, err := h.hookEnv(bg, ev, map[string]string{"PILOTHOUSE_TASK_ID": viaHook.ID}); err != nil {
		t.Fatal(err)
	}
	tk, _ := h.c.Task(bg, viaHook.ID)
	if tk.SessionID == "" {
		t.Fatalf("hook did not link the task: %+v", tk)
	}
	if hs, _ := h.c.Session(bg, tk.SessionID); hs.TaskID != viaHook.ID {
		t.Fatalf("session task = %q", hs.TaskID)
	}
	evs, _ := h.c.Events(bg, 0)
	var updates int
	for _, e := range evs {
		if e.Type == api.EventTaskUpdated {
			updates++
		}
	}
	if updates != 4 { // two puts, two links
		t.Fatalf("task.updated events = %d, want 4", updates)
	}
}
