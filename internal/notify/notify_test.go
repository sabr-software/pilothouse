package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sabr-software/pilothouse/internal/api"
	"github.com/sabr-software/pilothouse/internal/config"
	"github.com/sabr-software/pilothouse/internal/core"
)

func isolate(t *testing.T) {
	t.Setenv("PILOTHOUSE_HOME", t.TempDir())
	t.Setenv("PILOTHOUSE_USER_HOME", t.TempDir())
	run, err := os.MkdirTemp("/tmp", "ag")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(run) })
	t.Setenv("PILOTHOUSE_RUN_DIR", run)
}

type fakeChannel struct {
	name   string
	remote bool
	delay  time.Duration
	err    error
	mu     sync.Mutex
	got    []core.Notification
	block  chan struct{}
}

func (f *fakeChannel) Name() string { return f.name }
func (f *fakeChannel) Remote() bool { return f.remote }
func (f *fakeChannel) Send(ctx context.Context, n core.Notification) error {
	if f.block != nil {
		select {
		case <-f.block:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	f.mu.Lock()
	f.got = append(f.got, n)
	f.mu.Unlock()
	return f.err
}

func (f *fakeChannel) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.got)
}

func sampleRequest() *api.Request {
	return &api.Request{ID: "r_abc", Agent: api.AgentClaude, Tool: "Bash", ToolKind: api.ToolShell,
		Command: "git push origin main", Cwd: "/src/pilothouse", Status: api.RequestOpen, ExpiresAt: time.Now().Add(5 * time.Minute)}
}

func sampleSession() *api.Session {
	return &api.Session{ID: "s_abc", Agent: api.AgentClaude, Title: "pilothouse", Branch: "main"}
}

func TestFanOutRouting(t *testing.T) {
	isolate(t)
	desk := &fakeChannel{name: "desktop"}
	push := &fakeChannel{name: "webpush", remote: true}
	hook := &fakeChannel{name: "webhook", remote: true, err: errors.New("boom")}
	n := NewWith(nil, time.Second, desk, push, hook)

	ctx := context.Background()
	_ = n.Notify(ctx, core.Notification{Kind: core.NotifyBlocked, Request: sampleRequest(), Remote: false})
	_ = n.Notify(ctx, core.Notification{Kind: core.NotifyRequest, Request: sampleRequest(), Session: sampleSession(), Remote: true})
	_ = n.Notify(ctx, core.Notification{Kind: core.NotifyTest, Remote: false})
	n.Close()

	if desk.count() != 3 || push.count() != 2 || hook.count() != 2 {
		t.Fatalf("desktop=%d push=%d webhook=%d", desk.count(), push.count(), hook.count())
	}
	got := push.got[0]
	if got.Title != "Claude Code · pilothouse (main)" || got.Body != "Wants to run `git push origin main`" {
		t.Fatalf("rendered %q / %q", got.Title, got.Body)
	}
	if err := n.Notify(ctx, core.Notification{Kind: core.NotifyTest}); !errors.Is(err, ErrClosed) {
		t.Fatalf("after close: %v", err)
	}
}

func TestNotifyNeverBlocks(t *testing.T) {
	isolate(t)
	slow := &fakeChannel{name: "slow", remote: true, block: make(chan struct{})}
	fast := &fakeChannel{name: "fast", remote: true}
	n := NewWith(nil, 50*time.Millisecond, slow, fast)
	start := time.Now()
	for i := 0; i < urgentQueueSize*2; i++ {
		_ = n.Notify(context.Background(), core.Notification{Kind: core.NotifyRequest, Request: sampleRequest(), Remote: true})
	}
	if el := time.Since(start); el > 200*time.Millisecond {
		t.Fatalf("Notify blocked for %v", el)
	}
	// The fast channel isn't held up by the slow one.
	deadline := time.Now().Add(2 * time.Second)
	for fast.count() < queueSize && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if fast.count() < queueSize {
		t.Fatalf("fast channel delivered %d", fast.count())
	}
	close(slow.block)
	n.Close()
	if slow.count() > urgentQueueSize+1 {
		t.Fatalf("slow channel got %d; queue must be bounded", slow.count())
	}
}

func TestPerChannelTimeout(t *testing.T) {
	isolate(t)
	stuck := &fakeChannel{name: "stuck", block: make(chan struct{})}
	n := NewWith(nil, 30*time.Millisecond, stuck)
	_ = n.Notify(context.Background(), core.Notification{Kind: core.NotifyTest})
	_ = n.Notify(context.Background(), core.Notification{Kind: core.NotifyTest})
	done := make(chan struct{})
	go func() { n.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("timeouts not applied")
	}
}

func TestText(t *testing.T) {
	r := sampleRequest()
	s := sampleSession()
	if got := Line(core.Notification{Kind: core.NotifyRequest, Request: r, Session: s}); got != "Claude Code wants to run `git push origin main` · pilothouse (main)" {
		t.Fatal(got)
	}
	long := *r
	long.Command = strings.Repeat("echo hi && ", 30)
	if got := Action(&long); len([]rune(got)) > maxCommand+20 || !strings.Contains(got, "…") {
		t.Fatalf("long command not shortened: %q", got)
	}
	multi := *r
	multi.Command, multi.ToolKind = "", api.ToolEdit
	multi.Paths = []string{"/src/pilothouse/internal/web/web.go", "/src/pilothouse/README.md"}
	if got := Action(&multi); got != "wants to edit `internal/web/web.go` and 1 more" {
		t.Fatal(got)
	}
	codex := *r
	codex.Agent = api.AgentCodex
	if got := Line(core.Notification{Kind: core.NotifyBlocked, Request: &codex}); got != "Codex wants to run `git push origin main` · pilothouse — answer in the terminal" {
		t.Fatal(got)
	}
	w := Render(core.Notification{Kind: core.NotifyWaiting, Session: &api.Session{Agent: api.AgentCodex, Title: "site", LastMessage: "All done.\nTests pass."}})
	if w.Title != "Codex · site" || w.Body != "Finished: All done. Tests pass." {
		t.Fatalf("%q / %q", w.Title, w.Body)
	}
	if got := Render(core.Notification{Kind: core.NotifyRequest, Title: "Custom", Request: r}); got.Title != "Custom" {
		t.Fatal("caller title overwritten")
	}
}

func TestDesktopCommandEscaping(t *testing.T) {
	hostile := `"; do shell script "rm -rf ~" --` + "\n" + `end run`
	name, args := DesktopCommand("darwin", "Claude Code · repo", hostile, nil)
	if name != "osascript" {
		t.Fatal(name)
	}
	// The script is constant; the text only ever travels as argv.
	script := strings.Join(args[:len(osascriptProgram)], " ")
	if strings.Contains(script, "rm -rf") || strings.Contains(script, "Claude") {
		t.Fatalf("user text reached the script: %q", script)
	}
	tail := args[len(osascriptProgram):]
	if len(tail) != 3 || tail[0] != "--" || tail[1] != "Claude Code · repo" || !strings.Contains(tail[2], `do shell script "rm -rf ~"`) || strings.Contains(tail[2], "\n") {
		t.Fatalf("argv = %q", tail)
	}

	look := func(string) (string, error) { return "/usr/bin/notify-send", nil }
	name, args = DesktopCommand("linux", "-t 1", "<b>x</b> & y", look)
	if name != "/usr/bin/notify-send" || args[1] != "--" || args[2] != "-t 1" || args[3] != "&lt;b&gt;x&lt;/b&gt; &amp; y" {
		t.Fatalf("linux = %s %q", name, args)
	}
	missing := func(string) (string, error) { return "", errors.New("not found") }
	if name, _ := DesktopCommand("linux", "a", "b", missing); name != "" {
		t.Fatal("notify-send missing should be a no-op")
	}
	if name, _ := DesktopCommand("windows", "a", "b", look); name != "" {
		t.Fatal("unsupported OS should be a no-op")
	}

	var ran []string
	d := &Desktop{GOOS: "darwin", Run: func(_ context.Context, n string, a ...string) error {
		ran = append([]string{n}, a...)
		return nil
	}}
	if err := d.Send(context.Background(), Render(core.Notification{Kind: core.NotifyBlocked, Request: sampleRequest()})); err != nil || ran[0] != "osascript" {
		t.Fatalf("send: %v %q", err, ran)
	}
}

// TestDesktopClickOpensPage: with terminal-notifier installed, a click
// opens the request, session or task in the web app, and one that can't
// notify (terminal-notifier 3 before macOS allows it) falls back to
// osascript.
func TestDesktopClickOpensPage(t *testing.T) {
	var ran [][]string
	fail := false
	d := &Desktop{GOOS: "darwin",
		LookPath: func(name string) (string, error) { return "/opt/homebrew/bin/" + name, nil },
		Base:     func() string { return "https://mac.example.ts.net" },
		Run: func(_ context.Context, n string, a ...string) error {
			ran = append(ran, append([]string{n}, a...))
			if fail && strings.HasSuffix(n, "terminal-notifier") {
				return errors.New("exit status 3")
			}
			return nil
		}}
	opt := func(args []string, name string) string {
		for i := 1; i+1 < len(args); i++ {
			if args[i] == name {
				return args[i+1]
			}
		}
		return ""
	}
	for _, c := range []struct {
		n           core.Notification
		open, group string
	}{
		{core.Notification{Kind: core.NotifyRequest, Title: "t", Body: "b", Request: sampleRequest(), Session: sampleSession()}, "/r/r_abc", "pilothouse-r_abc"},
		{core.Notification{Kind: core.NotifyWaiting, Title: "t", Body: "b", Session: sampleSession()}, "/s/s_abc", "pilothouse-s_abc"},
		{core.Notification{Kind: core.NotifyWaiting, Title: "t", Body: "b", Session: sampleSession(), URL: "/t/t_1"}, "/t/t_1", "pilothouse-s_abc"},
		{core.Notification{Kind: core.NotifyTest, Title: "t", Body: "b"}, "/", "pilothouse-test"},
	} {
		ran = nil
		if err := d.Send(context.Background(), c.n); err != nil || len(ran) != 1 || ran[0][0] != "/opt/homebrew/bin/terminal-notifier" {
			t.Fatalf("send: %v %q", err, ran)
		}
		if got := opt(ran[0], "-open"); got != `"https://mac.example.ts.net`+c.open+`"` {
			t.Fatalf("-open = %s, want %s", got, c.open)
		}
		if got := opt(ran[0], "-group"); got != `"`+c.group+`"` {
			t.Fatalf("-group = %s, want %s", got, c.group)
		}
	}

	d.Base = func() string { return "" } // no web app: no link
	ran = nil
	if err := d.Send(context.Background(), core.Notification{Title: "t", Body: "b"}); err != nil || opt(ran[0], "-open") != "" {
		t.Fatalf("no base: %v %q", err, ran)
	}

	fail = true
	ran = nil
	if err := d.Send(context.Background(), core.Notification{Title: "t", Body: "b"}); err != nil || len(ran) != 2 || ran[1][0] != "osascript" {
		t.Fatalf("fallback: %v %q", err, ran)
	}
}

// terminal-notifier reads options through NSUserDefaults, which parses a
// bare value as a property list: every value goes as a quoted plist
// string, and a leading backslash (terminal-notifier's own escape) is
// doubled.
func TestTerminalNotifierArgsQuote(t *testing.T) {
	for in, want := range map[string]string{
		"Claude Code · ERP · claude (master)": `"Claude Code · ERP · claude (master)"`,
		"(a, b)":                              `"(a, b)"`,
		"{a=b;}":                              `"{a=b;}"`,
		"- [x] done":                          `"- [x] done"`,
		`said "hi" \ ok`:                      `"said \"hi\" \\ ok"`,
		`\[ZOMG]`:                             `"\\\\[ZOMG]"`,
		"line\nbreak\tand tab":                `"line break and tab"`,
	} {
		if got := TerminalNotifierArgs(in, in, "g", "")[1]; got != want {
			t.Errorf("%q → %s, want %s", in, got, want)
		}
	}
	args := TerminalNotifierArgs("t", "b", "pilothouse-s_1", "http://127.0.0.1:7517/s/s_1")
	if strings.Join(args, " ") != `-title "t" -message "b" -group "pilothouse-s_1" -open "http://127.0.0.1:7517/s/s_1"` {
		t.Fatalf("args = %q", args)
	}
}

// Channels gives the desktop channel the web app's URL for its links.
func TestChannelsDesktopBase(t *testing.T) {
	cfg := config.Default()
	cfg.Notify.Desktop = true
	for _, ch := range Channels(cfg, &fakePusher{public: "https://mac.example.ts.net"}, nil) {
		if d, ok := ch.(*Desktop); ok {
			if d.Base == nil || d.Base() != "https://mac.example.ts.net" {
				t.Fatal("desktop has no web app URL")
			}
			return
		}
	}
	t.Fatal("no desktop channel")
}

type fakePusher struct {
	public string
	pushed []core.Notification
}

func (p *fakePusher) Push(_ context.Context, n core.Notification) error {
	p.pushed = append(p.pushed, n)
	return nil
}
func (p *fakePusher) ActionURL(id string, b api.Behavior) string {
	if p.public == "" {
		return ""
	}
	return p.public + "/api/actions/tok-" + id + "-" + string(b)
}
func (p *fakePusher) BaseURL() string   { return p.public }
func (p *fakePusher) PublicURL() string { return p.public }

type captured struct {
	path   string
	header http.Header
	body   string
}

func capture(t *testing.T, status int) (*httptest.Server, func() []captured) {
	var mu sync.Mutex
	var got []captured
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		got = append(got, captured{r.URL.Path, r.Header.Clone(), string(b)})
		mu.Unlock()
		w.WriteHeader(status)
	}))
	t.Cleanup(ts.Close)
	return ts, func() []captured {
		mu.Lock()
		defer mu.Unlock()
		return append([]captured(nil), got...)
	}
}

func TestNtfy(t *testing.T) {
	isolate(t)
	ts, got := capture(t, 200)
	p := &fakePusher{public: "https://mac.tail1.ts.net"}
	c := &Ntfy{Server: ts.URL + "/", Topic: "pilothouse-secret", Token: "tk_abc", Pusher: p, Client: ts.Client()}
	n := Render(core.Notification{Kind: core.NotifyRequest, Request: sampleRequest(), Session: &api.Session{Title: "répo"}, Remote: true})
	if err := c.Send(context.Background(), n); err != nil {
		t.Fatal(err)
	}
	r := got()[0]
	if r.path != "/pilothouse-secret" || r.body != "Claude Code wants to run `git push origin main` · répo" {
		t.Fatalf("publish %s %q", r.path, r.body)
	}
	h := r.header
	if title, err := new(mime.WordDecoder).DecodeHeader(h.Get("Title")); err != nil || title != "Claude Code · répo" {
		t.Errorf("Title = %q (%v)", h.Get("Title"), err)
	}
	if h.Get("Priority") != "high" || h.Get("Tags") != "lock" || h.Get("Authorization") != "Bearer tk_abc" || h.Get("Click") != "https://mac.tail1.ts.net/r/r_abc" {
		t.Errorf("headers = %v", h)
	}
	want := "http, Allow, https://mac.tail1.ts.net/api/actions/tok-r_abc-allow, method=POST, clear=true; " +
		"http, Deny, https://mac.tail1.ts.net/api/actions/tok-r_abc-deny, method=POST, clear=true; " +
		"view, Open, https://mac.tail1.ts.net/r/r_abc"
	if h.Get("Actions") != want {
		t.Errorf("Actions = %q", h.Get("Actions"))
	}

	// No public URL: no buttons, no click link.
	c2 := &Ntfy{Server: ts.URL, Topic: "t", Token: "user:pass", Pusher: &fakePusher{}, Client: ts.Client()}
	if err := c2.Send(context.Background(), n); err != nil {
		t.Fatal(err)
	}
	h = got()[1].header
	if h.Get("Actions") != "" || h.Get("Click") != "" || !strings.HasPrefix(h.Get("Authorization"), "Basic ") {
		t.Errorf("local-only headers = %v", h)
	}

	bad, _ := capture(t, 403)
	if err := (&Ntfy{Server: bad.URL, Topic: "t", Client: bad.Client()}).Send(context.Background(), n); err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("error = %v", err)
	}
}

func TestWebhook(t *testing.T) {
	isolate(t)
	ts, got := capture(t, 204)
	w := &Webhook{URL: ts.URL + "/hook", Client: ts.Client()}
	if err := w.Send(context.Background(), Render(core.Notification{Kind: core.NotifyRequest, Request: sampleRequest(), Session: sampleSession()})); err != nil {
		t.Fatal(err)
	}
	var body map[string]string
	if err := json.Unmarshal([]byte(got()[0].body), &body); err != nil {
		t.Fatal(err)
	}
	want := "Claude Code wants to run `git push origin main` · pilothouse (main)"
	if body["text"] != want || body["content"] != want || got()[0].header.Get("Content-Type") != "application/json" {
		t.Fatalf("body = %v", body)
	}
}

func TestChannelsFromConfig(t *testing.T) {
	isolate(t)
	cfg := config.Default()
	cfg.Notify.Ntfy.Topic = "t"
	cfg.Notify.Webhooks = []string{"https://hooks.example/1", ""}
	names := func(cs []Channel) string {
		var s []string
		for _, c := range cs {
			s = append(s, c.Name())
		}
		return strings.Join(s, ",")
	}
	if got := names(Channels(cfg, &fakePusher{}, nil)); got != "desktop,webpush,ntfy,webhook" {
		t.Fatal(got)
	}
	var nilPusher *fakePusher
	cfg.Notify.Desktop = false
	if got := names(Channels(cfg, nilPusher, nil)); got != "ntfy,webhook" {
		t.Fatalf("typed nil pusher: %s", got)
	}
	// New wires the same channels. Desktop is off here: tests never show
	// real notifications.
	quiet := config.Default()
	quiet.Notify.Desktop = false
	n := New(quiet, nil, nil).(*Notifier)
	if len(n.workers) != 0 {
		t.Fatalf("workers = %d", len(n.workers))
	}
	if err := n.Notify(context.Background(), core.Notification{Kind: core.NotifyTest}); err != nil {
		t.Fatal(err)
	}
	n.Close()
}

func waitingFor(id, msg string) core.Notification {
	return core.Notification{Kind: core.NotifyWaiting, Session: &api.Session{ID: id}, Body: msg, Remote: true}
}

func drain(w *worker) []core.Notification {
	w.close()
	var out []core.Notification
	for {
		n, ok := w.pop()
		if !ok {
			return out
		}
		out = append(out, n)
	}
}

// TestRequestsNeverDroppedBehindWaiting is the QA M7 regression: a queue
// full of "finished its turn" notifications must not drop a permission
// request; requests are delivered first.
func TestRequestsNeverDroppedBehindWaiting(t *testing.T) {
	w := &worker{wake: make(chan struct{}, 1)}
	for i := 0; i < queueSize*2; i++ {
		w.push(waitingFor(fmt.Sprintf("s_%d", i), "done"))
	}
	for i := 0; i < 5; i++ {
		if d := w.push(core.Notification{Kind: core.NotifyRequest, Request: &api.Request{ID: fmt.Sprintf("r_%d", i)}}); d != nil && d.Kind == core.NotifyRequest {
			t.Fatalf("request dropped: %+v", d)
		}
	}
	got := drain(w)
	for i := 0; i < 5; i++ {
		if got[i].Kind != core.NotifyRequest || got[i].Request.ID != fmt.Sprintf("r_%d", i) {
			t.Fatalf("delivery %d = %s, want request r_%d first, in order", i, got[i].Kind, i)
		}
	}
	if len(got) > queueSize {
		t.Fatalf("delivered %d, lanes must stay bounded", len(got))
	}
}

func TestWaitingCoalescedPerSession(t *testing.T) {
	w := &worker{wake: make(chan struct{}, 1)}
	for i := 0; i < 10; i++ {
		w.push(waitingFor("s_a", fmt.Sprintf("turn %d", i)))
	}
	w.push(waitingFor("s_b", "other"))
	got := drain(w)
	if len(got) != 2 || got[0].Session.ID != "s_a" || got[0].Body != "turn 9" || got[1].Session.ID != "s_b" {
		t.Fatalf("got %+v", got)
	}
}

func TestNtfyRetries(t *testing.T) {
	isolate(t)
	var hits atomic.Int32
	var failFirst atomic.Int32
	failFirst.Store(2)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) <= failFirst.Load() {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(200)
	}))
	t.Cleanup(ts.Close)
	n := Render(core.Notification{Kind: core.NotifyRequest, Request: sampleRequest(), Remote: true})
	c := &Ntfy{Server: ts.URL, Topic: "t", Client: ts.Client(), Backoff: 5 * time.Millisecond}
	if err := c.Send(context.Background(), n); err != nil || hits.Load() != 3 {
		t.Fatalf("429 twice then ok: err=%v hits=%d", err, hits.Load())
	}

	// Always 503: bounded attempts, then the error.
	hits.Store(0)
	failFirst.Store(1 << 20)
	if err := c.Send(context.Background(), n); err == nil || hits.Load() != ntfyRetries+1 {
		t.Fatalf("503 forever: err=%v hits=%d", err, hits.Load())
	}

	// 4xx other than 429: no retry.
	bad, got := capture(t, 403)
	if err := (&Ntfy{Server: bad.URL, Topic: "t", Client: bad.Client(), Backoff: time.Millisecond}).Send(context.Background(), n); err == nil || len(got()) != 1 {
		t.Fatalf("403: err=%v attempts=%d", err, len(got()))
	}
}
