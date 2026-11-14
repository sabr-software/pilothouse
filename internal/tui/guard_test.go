package tui

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/sabr-software/pilothouse/internal/api"
)

// A question that arrives with no options must not put the cursor at -1:
// `1` then enter used to index Options[-1] and take the inbox down.
func TestQuestionWithoutOptionsDoesNotPanic(t *testing.T) {
	for _, tc := range []struct {
		name  string
		multi bool
		other bool
		keys  []string
	}{
		{"single, digit then enter", false, false, []string{"1", "enter"}},
		{"single, digit then space", false, false, []string{"1", " "}},
		{"multi, digit then enter", true, false, []string{"1", "enter"}},
		{"multi, digit, space, enter", true, false, []string{"1", " ", "enter"}},
		{"own words allowed", false, true, []string{"1", "enter"}},
		{"moving around first", true, false, []string{"1", "k", "j", "k", "enter"}},
		{"nine", false, false, []string{"9", "enter"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			snap := askSnapshot()
			for i := range snap.Requests {
				if snap.Requests[i].ID == "r_q7a8b9c0de" {
					snap.Requests[i].Questions = []api.Question{{Header: "Free", Text: "What should the branch be called?",
						MultiSelect: tc.multi, AllowOther: tc.other, Options: []api.QuestionOption{}}}
				}
			}
			be := &fakeBackend{snap: snap}
			m := newTestModel(t, be, 120, 35)
			m.Update(snapshotMsg{snap: snap})
			t.Cleanup(m.stopStream)
			selectKey(t, m, "r:r_q7a8b9c0de")
			for _, k := range tc.keys {
				press(m, k)
				if m.ask.cursor < 0 {
					t.Errorf("after %q the cursor is %d", k, m.ask.cursor)
				}
				_ = m.View()
			}
			if len(be.decides) != 0 {
				t.Fatalf("nothing can be answered without options or own words: %+v", be.decides)
			}
			if tc.other && m.mode != modeOther {
				t.Fatalf("enter on a question with only own words should start typing, mode %v", m.mode)
			}
		})
	}
}

// twoHeld is a snapshot whose first two rows are held requests: a
// harmless one (selected in the tests) and a dangerous neighbour.
func twoHeld() api.Snapshot {
	s := fixtureSnapshot()
	s.Requests = append(s.Requests,
		api.Request{ID: "r_safe000001", SessionID: "s_w1x3y5z7ab", Agent: api.AgentClaude, Checkpoint: api.CheckpointPermission,
			Tool: "Bash", ToolKind: api.ToolShell, Command: "ls -la", Summary: "Run: ls -la", Cwd: "/home/dev/src/docs",
			Status: api.RequestOpen, Route: api.RouteDevices, CreatedAt: t0.Add(-90 * time.Second), ExpiresAt: t0.Add(8 * time.Minute)},
		api.Request{ID: "r_danger0001", SessionID: "s_m8n6b4v2cx", Agent: api.AgentCodex, Checkpoint: api.CheckpointPermission,
			Tool: "shell", ToolKind: api.ToolShell, Command: "rm -rf ~/src", Summary: "Run: rm -rf ~/src", Cwd: "/home/dev/src/infra",
			Status: api.RequestOpen, Route: api.RouteDevices, CreatedAt: t0.Add(-80 * time.Second), ExpiresAt: t0.Add(8 * time.Minute)},
	)
	return s
}

// clockModel is a loaded model whose clock the test moves.
func clockModel(t *testing.T, snap api.Snapshot) (*Model, *fakeBackend, *time.Time) {
	t.Helper()
	t.Setenv("PILOTHOUSE_USER_HOME", "/home/dev")
	now := t0
	be := &fakeBackend{snap: snap}
	m := New(Options{Backend: be, Exe: "/usr/local/bin/pilothouse", Now: func() time.Time { return now }, Renderer: plainRenderer(), NoTick: true})
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 35})
	m.Update(snapshotMsg{snap: snap})
	t.Cleanup(m.stopStream)
	return m, be, &now
}

func closedElsewhere(t *testing.T, m *Model, snap api.Snapshot, id string, seq int64) {
	t.Helper()
	for _, r := range snap.Requests {
		if r.ID != id {
			continue
		}
		r.Status = api.RequestDecided
		r.Decision = &api.Decision{Behavior: api.BehaviorAllow, By: "device:d_phone"}
		b, _ := json.Marshal(r)
		m.Update(eventMsg{gen: m.gen, ev: api.Event{Seq: seq, Type: api.EventRequestClosed, Data: b}})
		return
	}
	t.Fatalf("no request %s", id)
}

// run executes what a key returned, the way the runtime would.
func run(m *Model, cmd tea.Cmd) {
	if cmd == nil {
		return
	}
	if msg := cmd(); msg != nil {
		m.Update(msg)
	}
}

// The selected request is answered on a phone while `y` is on its way:
// the key must not approve the request that slid under the cursor.
func TestKeyInFlightDoesNotAnswerTheNeighbour(t *testing.T) {
	for _, k := range []string{"y", "a", "n", "enter", "r"} {
		t.Run(k, func(t *testing.T) {
			snap := twoHeld()
			m, be, now := clockModel(t, snap)
			selectKey(t, m, "r:r_safe000001")
			_ = m.View()
			closedElsewhere(t, m, snap, "r_safe000001", 200)

			// Before the new selection was even drawn.
			run(m, press(m, k))
			if len(be.decides) != 0 || len(be.replies) != 0 || m.mode != modeList {
				t.Fatalf("%q before a redraw: mode %v decides %+v", k, m.mode, be.decides)
			}
			// Drawn, but the key was already on its way.
			_ = m.View()
			*now = now.Add(200 * time.Millisecond)
			run(m, press(m, k))
			if len(be.decides) != 0 || len(be.replies) != 0 || m.mode != modeList {
				t.Fatalf("%q right after the redraw: mode %v decides %+v", k, m.mode, be.decides)
			}
			if v := m.View(); !strings.Contains(v, "selection moved") {
				t.Fatalf("the ignored key must be explained:\n%s", v)
			}
			// Each ignored key restarts the wait, so a held key or a burst
			// can't run past it.
			*now = now.Add(selectionSettle - 100*time.Millisecond)
			run(m, press(m, k))
			if len(be.decides) != 0 || m.mode != modeList {
				t.Fatalf("%q inside the restarted wait: mode %v decides %+v", k, m.mode, be.decides)
			}
		})
	}

	// After the new selection has been on screen for a moment, a fresh
	// key acts on it.
	snap := twoHeld()
	m, be, now := clockModel(t, snap)
	selectKey(t, m, "r:r_safe000001")
	_ = m.View()
	closedElsewhere(t, m, snap, "r_safe000001", 200)
	if m.selKey != "r:r_danger0001" {
		t.Fatalf("selection = %s", m.selKey)
	}
	_ = m.View()
	*now = now.Add(selectionSettle + 50*time.Millisecond)
	run(m, press(m, "y"))
	if len(be.decides) != 1 || be.decides[0].id != "r_danger0001" {
		t.Fatalf("deliberate y = %+v", be.decides)
	}
}

// A burst ("yy" read as one message) and time passing without a redraw
// don't get through either.
func TestSelectionHoldNeedsARedraw(t *testing.T) {
	snap := twoHeld()
	m, be, now := clockModel(t, snap)
	selectKey(t, m, "r:r_safe000001")
	closedElsewhere(t, m, snap, "r_safe000001", 200)
	*now = now.Add(time.Minute) // never drawn: the person has not seen it
	run(m, press(m, "yy"))
	if len(be.decides) != 0 {
		t.Fatalf("undrawn selection answered: %+v", be.decides)
	}
	// Moving doesn't lift the hold: "jy" typed for the old list would
	// answer whatever is one row down now.
	_ = m.View()
	run(m, press(m, "j"))
	run(m, press(m, "y"))
	if len(be.decides) != 0 {
		t.Fatalf("answered after a move inside the wait: %+v", be.decides)
	}
}

// Answering the selected request yourself moves on without a hold, and a
// request that closes elsewhere in the list doesn't start one.
func TestSelectionHoldOnlyWhenTheSelectionLeft(t *testing.T) {
	snap := twoHeld()
	m, be, _ := clockModel(t, snap)
	selectKey(t, m, "r:r_safe000001")
	_ = m.View()
	execCmd(t, m, press(m, "y"))
	if len(be.decides) != 1 || be.decides[0].id != "r_safe000001" {
		t.Fatalf("own answer = %+v", be.decides)
	}
	_ = m.View()
	execCmd(t, m, press(m, "y"))
	if len(be.decides) != 2 || be.decides[1].id != "r_danger0001" {
		t.Fatalf("next answer = %+v", be.decides)
	}

	m, be, _ = clockModel(t, snap)
	selectKey(t, m, "r:r_danger0001")
	_ = m.View()
	closedElsewhere(t, m, snap, "r_safe000001", 200) // not the selected one
	execCmd(t, m, press(m, "y"))
	if len(be.decides) != 1 || be.decides[0].id != "r_danger0001" {
		t.Fatalf("selection kept, answer = %+v", be.decides)
	}
}
