package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/sabr-software/pilothouse/internal/api"
	"github.com/sabr-software/pilothouse/internal/core"
)

// ---- Desktop ----

// Desktop shows a local notification: on macOS terminal-notifier when it
// is installed, so a click opens the request or session in the web app,
// else osascript (macOS credits that to Script Editor: a click only opens
// Script Editor); notify-send on Linux. Text is passed as process
// arguments, never spliced into a script or shell, so a hostile command
// string can't inject anything.
type Desktop struct {
	GOOS     string
	LookPath func(string) (string, error)
	Run      func(ctx context.Context, name string, args ...string) error
	// Base is the web app's URL for click-through links ("" or nil: none).
	Base func() string
	// Local is its address on this computer: a tab there is reused too.
	Local func() string
	// Browser names the app that opens the links (notify.desktop_browser);
	// else AutoBrowser gives the bundle id of the browser Pilothouse is
	// used in on this computer; else the default browser opens them. So a
	// click lands in the browser that is paired.
	Browser     string
	AutoBrowser func() string
}

// Opener is the app a click opens its link in: App by name, else Bundle by
// bundle id, else the default browser. In a browser AppleScript can reach,
// a tab already at one of Origins (the link's first) shows the page instead
// of a new one.
type Opener struct {
	App, Bundle string
	Origins     []string
}

// NewDesktop returns a Desktop for this OS.
func NewDesktop() *Desktop {
	return &Desktop{
		GOOS:     runtime.GOOS,
		LookPath: exec.LookPath,
		Run: func(ctx context.Context, name string, args ...string) error {
			out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
			if err != nil && len(out) > 0 {
				return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
			}
			return err
		},
	}
}

func (*Desktop) Name() string { return "desktop" }
func (*Desktop) Remote() bool { return false }

// osascriptProgram reads title and body from argv: the script text is
// constant, so nothing user-controlled is ever parsed as AppleScript.
var osascriptProgram = []string{
	"-e", "on run argv",
	"-e", "display notification (item 2 of argv) with title (item 1 of argv)",
	"-e", "end run",
}

// DesktopCommand returns the command that shows title/body on goos, or
// "" when the platform has no supported notifier.
func DesktopCommand(goos, title, body string, lookPath func(string) (string, error)) (string, []string) {
	title, body = clean(title, 120), clean(body, 300)
	switch goos {
	case "darwin":
		args := append(append([]string{}, osascriptProgram...), "--", title, body)
		return "osascript", args
	case "linux", "freebsd", "openbsd", "netbsd":
		if lookPath == nil {
			return "", nil
		}
		p, err := lookPath("notify-send")
		if err != nil {
			return "", nil
		}
		// notify-send bodies may be parsed as markup by the server.
		return p, []string{"--app-name=Pilothouse", "--", title, escapeMarkup(body)}
	}
	return "", nil
}

// TerminalNotifierArgs returns terminal-notifier arguments that show
// title/body and open link ("": nothing) on a click, with o. group names
// the notification: a newer one in the same group replaces it.
func TerminalNotifierArgs(title, body, group, link string, o Opener) []string {
	args := []string{"-title", tnValue(clean(title, 120)), "-message", tnValue(clean(body, 300)), "-group", tnValue(group)}
	switch {
	case link == "":
	case o.App != "":
		args = append(args, "-execute", tnValue(tabCmd(link, o)+openCmd("-a", o.App, link)))
	case o.Bundle != "":
		args = append(args, "-execute", tnValue(tabCmd(link, o)+openCmd("-b", o.Bundle, link)))
	default:
		args = append(args, "-open", tnValue(link))
	}
	return args
}

// openCmd is the shell command (terminal-notifier runs -execute with
// /bin/sh) that opens link in app, or in the default browser when that
// app isn't installed. Every word from outside is single-quoted.
func openCmd(flag, app, link string) string {
	return "/usr/bin/open " + flag + " " + shQuote(app) + " " + shQuote(link) + " || /usr/bin/open " + shQuote(link)
}

// tabBrowsers are the browsers whose tabs AppleScript can reach (Firefox's
// can't be); pick is the line that brings tab t, number i, of window w
// forward.
var tabBrowsers = []struct{ id, app, pick string }{
	{"com.apple.Safari", "Safari", "set current tab of w to t"},
	{"com.google.Chrome", "Google Chrome", chromiumPick},
	{"com.microsoft.edgemac", "Microsoft Edge", chromiumPick},
	{"com.brave.Browser", "Brave Browser", chromiumPick},
	{"com.vivaldi.Vivaldi", "Vivaldi", chromiumPick},
	{"com.operasoftware.Opera", "Opera", chromiumPick},
}

const chromiumPick = "set active tab index of w to i"

// tabWait bounds the search in seconds: macOS asks once whether
// terminal-notifier may control the browser, and an unanswered prompt must
// not leave a click doing nothing. It is long enough to read the prompt.
const tabWait = 30

// tabProgram finds a tab at one of the origins in argv (after the page's
// path), shows the page there and brings it forward; it fails if there is
// none. Its two blanks are filled from tabBrowsers: nothing from outside is
// ever parsed as AppleScript.
var tabProgram = []string{
	"on run argv",
	"set thePath to item 1 of argv",
	"set origins to rest of argv",
	`tell application id "%s"`,
	"repeat with w in windows",
	"set i to 0",
	"repeat with t in tabs of w",
	"set i to i + 1",
	`set u to ""`,
	"try",
	"set u to URL of t as text",
	"end try",
	"repeat with o in origins",
	"set origin to o as text",
	`if u starts with origin & "/" then`,
	"if u is not origin & thePath then set URL of t to origin & thePath",
	"%s",
	"set index of w to 1",
	"activate",
	"return",
	"end if",
	"end repeat",
	"end repeat",
	"end repeat",
	"end tell",
	`error "no Pilothouse tab"`,
	"end run",
}

// tabCmd is the shell that tries the tab first and leaves what follows it
// to run only if that fails, or "" for a browser AppleScript can't reach.
func tabCmd(link string, o Opener) string {
	if len(o.Origins) == 0 {
		return ""
	}
	for _, b := range tabBrowsers {
		if b.id != o.Bundle && b.app != o.App {
			continue
		}
		var c strings.Builder
		c.WriteString("/usr/bin/osascript")
		for _, l := range tabProgram {
			switch {
			case strings.HasPrefix(l, "tell "):
				l = fmt.Sprintf(l, b.id)
			case l == "%s":
				l = b.pick
			}
			c.WriteString(" -e " + shQuote(l))
		}
		c.WriteString(" -- " + shQuote(strings.TrimPrefix(link, o.Origins[0])))
		for _, origin := range o.Origins {
			c.WriteString(" " + shQuote(origin))
		}
		return c.String() + " >/dev/null 2>&1 & p=$!; (sleep " + strconv.Itoa(tabWait) + "; kill $p) >/dev/null 2>&1 & k=$!; wait $p; r=$?; kill $k 2>/dev/null; [ $r -eq 0 ] || "
	}
	return ""
}

// Withdraw takes a closed request's banner off the screen and out of
// Notification Center: a click on it could only lead to a request that is
// already gone. Only terminal-notifier can; the group names the request.
func (d *Desktop) Withdraw(ctx context.Context, r api.Request) error {
	if d.GOOS != "darwin" || d.LookPath == nil {
		return nil
	}
	p, err := d.LookPath("terminal-notifier")
	if err != nil {
		return nil
	}
	return d.Run(ctx, p, "-remove", tnValue("pilothouse-"+r.ID))
}

// shQuote quotes s as one sh word.
func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// tnValue quotes s for terminal-notifier, which reads options through
// NSUserDefaults: a bare value is parsed as a property list, so "(a, b)"
// becomes an array, a leading "-" loses the value and quotes vanish. A
// quoted plist string comes through exactly; terminal-notifier then strips
// one leading backslash (its own escape), so that one is doubled.
func tnValue(s string) string {
	if strings.HasPrefix(s, `\`) {
		s = `\` + s
	}
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

// desktopGroup is the notification's terminal-notifier group: one per
// request, else per session, so a session's newer news replaces its older.
func desktopGroup(n core.Notification) string {
	switch {
	case n.Request != nil && n.Request.ID != "":
		return "pilothouse-" + n.Request.ID
	case n.Session != nil && n.Session.ID != "":
		return "pilothouse-" + n.Session.ID
	}
	return "pilothouse-" + string(n.Kind)
}

// Link is the web app page for n under base: the request, else the
// session, else the inbox; the sender's deep link (a task: /t/<id>) wins.
func Link(base string, n core.Notification) string {
	link := base + "/"
	switch {
	case n.Request != nil:
		link = base + "/r/" + url.PathEscape(n.Request.ID)
	case n.Session != nil:
		link = base + "/s/" + url.PathEscape(n.Session.ID)
	}
	if len(n.URL) > 1 && n.URL[0] == '/' && n.URL[1] != '/' {
		link = base + n.URL
	}
	return link
}

func (d *Desktop) Send(ctx context.Context, n core.Notification) error {
	if d.GOOS == "darwin" && d.LookPath != nil {
		if p, err := d.LookPath("terminal-notifier"); err == nil {
			link := ""
			o := Opener{App: d.Browser}
			if d.Base != nil {
				if b := d.Base(); b != "" {
					link = Link(b, n)
					o.Origins = []string{b}
					if d.Local != nil {
						if l := d.Local(); l != "" && l != b {
							o.Origins = append(o.Origins, l)
						}
					}
				}
			}
			// terminal-notifier 3 exits non-zero until macOS lets it
			// notify: osascript still shows this one.
			if o.App == "" && d.AutoBrowser != nil {
				o.Bundle = d.AutoBrowser()
			}
			if d.Run(ctx, p, TerminalNotifierArgs(n.Title, n.Body, desktopGroup(n), link, o)...) == nil {
				return nil
			}
		}
	}
	name, args := DesktopCommand(d.GOOS, n.Title, n.Body, d.LookPath)
	if name == "" {
		return nil // no desktop notifier here: not an error
	}
	return d.Run(ctx, name, args...)
}

// clean drops control characters (newlines included) and trims to n runes.
func clean(s string, n int) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
	return Short(s, n)
}

func escapeMarkup(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}

// ---- ntfy ----

// Ntfy publishes to an ntfy topic (https://docs.ntfy.sh/publish/).
// When the web app has a public URL, request notifications carry http
// action buttons (Allow / Deny) backed by single-use action tokens, and a
// click opens the request in the web app.
//
// Request notifications carry a sequence ID (X-Sequence-ID, ntfy server
// 2.16+): the "waiting in the terminal" notice that follows a timeout
// replaces the request in place, and Withdraw clears it from the phone
// (PUT /<topic>/<sequence>/clear) once someone answered elsewhere.
// Servers without that feature ignore the header and answer the clear
// with 404, which is not an error.
type Ntfy struct {
	Server string
	Topic  string
	Token  string // access token (tk_…) or "user:pass"
	Pusher Pusher // may be nil
	Client *http.Client
	// Backoff is the first retry delay after a 429 or 5xx (doubling, at
	// most ntfyRetries retries, within the delivery timeout). Zero means 1 s.
	Backoff time.Duration

	mu   sync.Mutex
	sent map[string]time.Time // request id → published (for Withdraw)
}

// ntfySeq is the ntfy sequence ID of a request's notifications.
func ntfySeq(requestID string) string {
	return "req-" + seqRE.ReplaceAllString(Short(requestID, 60), "")
}

var seqRE = regexp.MustCompile(`[^A-Za-z0-9_-]`)

// ntfyRetries bounds retries of a rate-limited (429) or failing (5xx) publish.
const ntfyRetries = 3

func (*Ntfy) Name() string { return "ntfy" }
func (*Ntfy) Remote() bool { return true }

// Request builds the publish request (exported for tests and `pilothouse doctor`).
func (c *Ntfy) Request(ctx context.Context, n core.Notification) (*http.Request, error) {
	body := Line(n)
	if n.Request != nil {
		if d := Details(n.Request); len(d) > 0 {
			body += "\n" + strings.Join(d, "\n")
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.topicURL(), strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	h := req.Header
	h.Set("Content-Type", "text/plain; charset=utf-8")
	h.Set("Title", headerText(n.Title))
	if n.Request != nil && n.Request.ID != "" {
		h.Set("X-Sequence-ID", ntfySeq(n.Request.ID))
	}
	switch n.Kind {
	case core.NotifyRequest:
		h.Set("Priority", "high")
		h.Set("Tags", "lock")
	case core.NotifyBlocked:
		h.Set("Priority", "high")
		h.Set("Tags", "hourglass_flowing_sand")
	case core.NotifyWaiting:
		h.Set("Priority", "default")
		h.Set("Tags", "white_check_mark")
	default:
		h.Set("Priority", "default")
		h.Set("Tags", "bell")
	}
	var actions []string
	if c.Pusher != nil {
		if pub := c.Pusher.PublicURL(); pub != "" {
			link := Link(pub, n)
			h.Set("Click", link)
			// One-tap Allow/Deny only for permissions: a question needs an
			// answer and a plan should be read, so those open the app.
			if n.Kind == core.NotifyRequest && n.Request != nil && n.Request.Status == api.RequestOpen && !n.Linked &&
				(n.Request.Checkpoint == api.CheckpointPermission || n.Request.Checkpoint == "") {
				allow := c.Pusher.ActionURL(n.Request.ID, api.BehaviorAllow)
				deny := c.Pusher.ActionURL(n.Request.ID, api.BehaviorDeny)
				if allow != "" && deny != "" {
					actions = append(actions,
						"http, Allow, "+allow+", method=POST, clear=true",
						"http, Deny, "+deny+", method=POST, clear=true")
				}
			}
			actions = append(actions, "view, Open, "+link)
		}
	}
	if len(actions) > 0 {
		h.Set("Actions", strings.Join(actions, "; "))
	}
	c.auth(req)
	return req, nil
}

func (c *Ntfy) topicURL() string {
	server := strings.TrimRight(c.Server, "/")
	if server == "" {
		server = "https://ntfy.sh"
	}
	return server + "/" + url.PathEscape(c.Topic)
}

func (c *Ntfy) auth(req *http.Request) {
	switch {
	case c.Token == "":
	case strings.Contains(c.Token, ":"):
		u, p, _ := strings.Cut(c.Token, ":")
		req.SetBasicAuth(u, p)
	default:
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
}

func (c *Ntfy) Send(ctx context.Context, n core.Notification) error {
	err := c.publish(ctx, n)
	if err == nil && n.Request != nil && (n.Kind == core.NotifyRequest || n.Kind == core.NotifyBlocked) {
		c.mu.Lock()
		if c.sent == nil {
			c.sent = map[string]time.Time{}
		}
		if len(c.sent) >= maxRemembered {
			for k, at := range c.sent {
				if time.Since(at) > 24*time.Hour || len(c.sent) >= maxRemembered {
					delete(c.sent, k)
				}
			}
		}
		c.sent[n.Request.ID] = time.Now()
		c.mu.Unlock()
	}
	return err
}

// Withdraw clears a request's notification once someone answered it (or
// it moved back to the terminal). A timed-out request is left alone: the
// "waiting in the terminal" notice already replaced it.
func (c *Ntfy) Withdraw(ctx context.Context, r api.Request) error {
	c.mu.Lock()
	_, ok := c.sent[r.ID]
	delete(c.sent, r.ID)
	c.mu.Unlock()
	if !ok || r.Status == api.RequestOpen || r.Status == api.RequestExpired {
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, c.topicURL()+"/"+ntfySeq(r.ID)+"/clear", nil)
	if err != nil {
		return err
	}
	c.auth(req)
	status, _, err := doStatus(c.Client, req)
	if status == http.StatusNotFound || status == http.StatusMethodNotAllowed {
		return nil // a server older than 2.16: nothing to clear
	}
	return err
}

func (c *Ntfy) publish(ctx context.Context, n core.Notification) error {
	if c.Topic == "" {
		return errors.New("ntfy: no topic configured")
	}
	delay := c.Backoff
	if delay <= 0 {
		delay = time.Second
	}
	for attempt := 0; ; attempt++ {
		req, err := c.Request(ctx, n)
		if err != nil {
			return err
		}
		status, retryAfter, err := doStatus(c.Client, req)
		if err == nil || !retryable(status) || attempt >= ntfyRetries {
			return err
		}
		wait := delay
		if retryAfter > wait && retryAfter <= 30*time.Second {
			wait = retryAfter
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%w (gave up retrying: %v)", err, ctx.Err())
		case <-time.After(wait):
		}
		delay *= 2
	}
}

// retryable: ntfy rate limits (429) and server errors are worth a retry.
func retryable(status int) bool {
	return status == http.StatusTooManyRequests || status >= 500
}

// headerText makes a header-safe title: ASCII as is, anything else as an
// RFC 2047 encoded word (ntfy decodes these).
func headerText(s string) string {
	s = clean(s, 200)
	for _, r := range s {
		if r > unicode.MaxASCII {
			return mime.BEncoding.Encode("utf-8", s)
		}
	}
	return s
}

// ---- Webhooks ----

// Webhook posts Slack- and Discord-compatible JSON: Slack reads "text",
// Discord reads "content"; each ignores the other.
type Webhook struct {
	URL    string
	Client *http.Client
}

func (*Webhook) Name() string { return "webhook" }
func (*Webhook) Remote() bool { return true }

// Payload is the JSON body.
func (*Webhook) Payload(n core.Notification) []byte {
	line := Short(Line(n), 1900) // Discord's limit is 2000
	b, _ := json.Marshal(map[string]string{"text": line, "content": line})
	return b
}

func (w *Webhook) Send(ctx context.Context, n core.Notification) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.URL, bytes.NewReader(w.Payload(n)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	return do(w.Client, req)
}

func do(hc *http.Client, req *http.Request) error {
	_, _, err := doStatus(hc, req)
	return err
}

// doStatus sends req and returns the HTTP status (0 when no response) and
// any Retry-After delay the server asked for.
func doStatus(hc *http.Client, req *http.Request) (int, time.Duration, error) {
	if hc == nil {
		hc = &http.Client{Timeout: defaultTimeout}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return 0, 0, fmt.Errorf("%s: %w", req.URL.Host, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	if resp.StatusCode/100 != 2 {
		var after time.Duration
		if secs, err := strconv.Atoi(strings.TrimSpace(resp.Header.Get("Retry-After"))); err == nil && secs > 0 {
			after = time.Duration(secs) * time.Second
		}
		return resp.StatusCode, after, fmt.Errorf("%s: %s %s", req.URL.Host, resp.Status, strings.TrimSpace(string(body)))
	}
	return resp.StatusCode, 0, nil
}
