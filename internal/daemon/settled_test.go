package daemon

import (
	"testing"
	"time"

	"github.com/sabr-software/pilothouse/internal/adapters"
	"github.com/sabr-software/pilothouse/internal/api"
)

func permissionNotice() adapters.HookEvent {
	return adapters.HookEvent{Kind: adapters.HookNotification, NotificationType: "permission_prompt", Message: "Claude needs your permission"}
}

func onlyRequestSession(t *testing.T, h *harness, id string) api.Session {
	t.Helper()
	ss, err := h.c.Sessions(bg)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range ss {
		if s.ID == id {
			return s
		}
	}
	t.Fatalf("no session %s", id)
	return api.Session{}
}

// After an answer nothing waits on the person: the agent's own "needs your
// permission" notification, before or just after the answer, mustn't leave
// the session under Needs you until the allowed command finishes.
func TestAnsweredRequestLeavesNeedsYou(t *testing.T) {
	for _, late := range []bool{false, true} {
		h := newHarness(t)
		h.presence.setAway(true)
		h.start(t)
		waitFor(t, "away", func() bool { return h.d.Presence().State == api.PresenceAway })

		held := make(chan fakeOutcome, 1)
		go func() { held <- h.hook(bg, t, permission("git status", "")) }()
		req := h.openRequest(t)
		if !late {
			h.hook(bg, t, permissionNotice()) // while the request is open
		}
		if _, err := h.c.Decide(bg, req.ID, api.DecideBody{Behavior: api.BehaviorAllow}); err != nil {
			t.Fatal(err)
		}
		<-held
		if late {
			h.hook(bg, t, permissionNotice()) // just after the answer
		}
		if s := onlyRequestSession(t, h, req.SessionID); s.State == api.StateBlocked {
			t.Errorf("late=%v: answered, but the session is %s: %q", late, s.State, s.StateDetail)
		}
	}
}

// A real prompt with no request behind it still needs the person.
func TestPermissionNoticeWithoutRequestBlocks(t *testing.T) {
	h := newHarness(t)
	h.start(t)
	h.hook(bg, t, adapters.HookEvent{Kind: adapters.HookSessionStart, Cwd: t.TempDir(), AgentSessionID: "v-1"})
	h.hook(bg, t, permissionNotice())
	ss, _ := h.c.Sessions(bg)
	if len(ss) == 0 || ss[0].State != api.StateBlocked {
		t.Fatalf("sessions = %+v", ss)
	}
}

// A prompt that settles itself within the grace period notifies nobody; one
// that stays open does.
func TestHeldRequestNotifiesAfterGrace(t *testing.T) {
	h := newHarness(t)
	h.presence.setAway(true)
	h.opts = func(o *Options) { o.NotifyGrace = 150 * time.Millisecond }
	h.start(t)
	waitFor(t, "away", func() bool { return h.d.Presence().State == api.PresenceAway })
	before := len(h.notes.all())

	held := make(chan fakeOutcome, 1)
	go func() { held <- h.hook(bg, t, permission("git status", "")) }()
	req := h.openRequest(t)
	if _, err := h.c.Decide(bg, req.ID, api.DecideBody{Behavior: api.BehaviorAllow}); err != nil {
		t.Fatal(err)
	}
	<-held
	time.Sleep(300 * time.Millisecond)
	if n := len(h.notes.all()); n != before {
		t.Fatalf("a settled request notified: %d new", n-before)
	}

	go func() { held <- h.hook(bg, t, permission("git log", "")) }()
	req = h.openRequest(t)
	waitFor(t, "the notification", func() bool { return len(h.notes.all()) == before+1 })
	if _, err := h.c.Decide(bg, req.ID, api.DecideBody{Behavior: api.BehaviorDeny, Message: "no"}); err != nil {
		t.Fatal(err)
	}
	<-held
}
