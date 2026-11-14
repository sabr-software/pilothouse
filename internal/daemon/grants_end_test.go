package daemon

import (
	"testing"
	"time"

	"github.com/sabr-software/pilothouse/internal/adapters"
	"github.com/sabr-software/pilothouse/internal/api"
)

// "Allow for this session" ends with the session. A conversation resumed
// later under the same agent session id starts without the old grants.
func TestSessionGrantEndsWithTheSession(t *testing.T) {
	h := newHarness(t)
	h.presence.setAway(true)
	h.start(t)
	waitFor(t, "away", func() bool { return h.d.Presence().State == api.PresenceAway })

	in := func(ev adapters.HookEvent) adapters.HookEvent {
		ev.AgentSessionID = "conv-1"
		return ev
	}
	result := make(chan fakeOutcome, 1)
	go func() { result <- h.hook(bg, t, in(permission("git push origin main", "tu-1"))) }()
	req := h.openRequest(t)
	if _, err := h.c.Decide(bg, req.ID, api.DecideBody{Behavior: api.BehaviorAllow, Scope: api.ScopeSession}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-result:
	case <-time.After(5 * time.Second):
		t.Fatal("hook still blocked after the decision")
	}
	if out := h.hook(bg, t, in(shell("git push origin dev"))); out.Behavior != api.BehaviorAllow {
		t.Fatalf("during the session: %+v, want allow by the grant", out)
	}

	h.hook(bg, t, in(adapters.HookEvent{Kind: adapters.HookSessionEnd}))
	if s, _ := h.c.Session(bg, req.SessionID); s.State != api.StateExited {
		t.Fatalf("session after SessionEnd: %s", s.State)
	}
	h.hook(bg, t, in(adapters.HookEvent{Kind: adapters.HookSessionStart}))
	if s, _ := h.c.Session(bg, req.SessionID); s.State == api.StateExited {
		t.Fatal("resumed conversation did not revive its session")
	}
	if out := h.hook(bg, t, in(shell("git push origin dev"))); out.Behavior != "" {
		t.Fatalf("after the session ended and was resumed: %+v, want no opinion", out)
	}
}
