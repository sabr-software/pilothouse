package web

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sabr-software/pilothouse/internal/api"
	"github.com/sabr-software/pilothouse/internal/core"
	"github.com/sabr-software/pilothouse/internal/web/webtest"
)

// liveEnv is an env served on a real loopback listener, so two servers
// (a hub and a linked machine) can call each other.
type liveEnv struct {
	*env
	ts  *httptest.Server
	url string
}

func newLiveEnv(t *testing.T, machine string, mut ...func(*Deps)) *liveEnv {
	t.Helper()
	e := newEnv(t, mut...)
	e.cfg.Machine = machine
	e.fc.Machine = machine
	ts := httptest.NewServer(e.h)
	t.Cleanup(ts.Close)
	e.cfg.Web.Addr = strings.TrimPrefix(ts.URL, "http://")
	return &liveEnv{env: e, ts: ts, url: ts.URL}
}

// local calls a local-admin route with a JSON body.
func (e *liveEnv) admin(method, path string, body any) *httptest.ResponseRecorder {
	e.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	r := httptest.NewRequest(method, path, rd)
	w := httptest.NewRecorder()
	e.env.local.ServeHTTP(w, r)
	return w
}

// call is a device call over the real listener.
func (e *liveEnv) call(method, path, cookie string, body any) (int, []byte) {
	e.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, e.url+path, rd)
	req.Header.Set("Cookie", cookieName+"="+cookie)
	req.Header.Set("X-Pilothouse", "1")
	req.Header.Set("Origin", e.url)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

func (e *liveEnv) snapshot(cookie string) api.Snapshot {
	e.t.Helper()
	code, b := e.call("GET", "/api/snapshot", cookie, nil)
	if code != 200 {
		e.t.Fatalf("snapshot: %d %s", code, b)
	}
	var s api.Snapshot
	if err := json.Unmarshal(b, &s); err != nil {
		e.t.Fatal(err)
	}
	return s
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

type captureNotifier struct {
	mu    sync.Mutex
	notes []core.Notification
}

func (c *captureNotifier) Notify(_ context.Context, n core.Notification) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.notes = append(c.notes, n)
	return nil
}

func (c *captureNotifier) all() []core.Notification {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]core.Notification(nil), c.notes...)
}

type linkFixture struct {
	hub, peer *liveEnv
	key       *ecdsa.PrivateKey
	cookie    string
	deviceID  string
	notes     *captureNotifier
	joined    api.LinkJoined
}

// linkMachines pairs a phone with a hub "studio" and links "laptop" to it.
// peerMut adjusts the laptop's dependencies (its core, say).
func linkMachines(t *testing.T, peerMut ...func(*Deps)) *linkFixture {
	t.Helper()
	hub := newLiveEnv(t, "studio")
	peer := newLiveEnv(t, "laptop", peerMut...)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	hub.srv.StartLinks(ctx)
	peer.srv.StartLinks(ctx)
	notes := &captureNotifier{}
	hub.srv.SetNotifier(notes)

	k, pub := genKey(t)
	cookie, id := hub.pair(pub)

	peer.fc.PutSession(api.Session{ID: "s_laptop1", Machine: "laptop", Agent: api.AgentClaude, State: api.StateWorking, Title: "api"})
	now := peer.clock.Now()
	peer.fc.PutRequest(api.Request{ID: "r_laptop1", SessionID: "s_laptop1", Agent: api.AgentClaude, Checkpoint: api.CheckpointPermission,
		Tool: "Bash", ToolKind: api.ToolShell, Command: "git push", Summary: "Run: git push", Status: api.RequestOpen,
		Route: api.RouteDevices, CreatedAt: now, ExpiresAt: now.Add(9 * time.Minute)})

	w := hub.admin("POST", "/v1/link/code", nil)
	if w.Code != 200 {
		t.Fatalf("link code: %d %s", w.Code, w.Body)
	}
	var lc api.LinkCode
	mustJSON(t, w, &lc)
	w = peer.admin("POST", "/v1/link/join", api.LinkJoin{URL: hub.url, Code: lc.Code, SelfURL: peer.url})
	if w.Code != 200 {
		t.Fatalf("link join: %d %s", w.Code, w.Body)
	}
	var j api.LinkJoined
	mustJSON(t, w, &j)
	return &linkFixture{hub: hub, peer: peer, key: k, cookie: cookie, deviceID: id, notes: notes, joined: j}
}

func hasSession(s api.Snapshot, id string) bool {
	for _, x := range s.Sessions {
		if x.ID == id {
			return true
		}
	}
	return false
}

func TestLinkMergesMachinesAndForwardsSignedAnswers(t *testing.T) {
	f := linkMachines(t)
	hub, peer := f.hub, f.peer

	if f.joined.Hub.Machine != "studio" || len(f.joined.Devices) != 1 || f.joined.Devices[0].Trusted || f.joined.Devices[0].ID != f.deviceID {
		t.Fatalf("joined: %+v", f.joined)
	}

	// The hub shows both machines.
	var snap api.Snapshot
	eventually(t, "the laptop's session on the hub", func() bool {
		snap = hub.snapshot(f.cookie)
		return hasSession(snap, "s_laptop1")
	})
	if len(snap.Machines) != 2 || !snap.Machines[0].Self || snap.Machines[1].Name != "laptop" || !snap.Machines[1].Online {
		t.Fatalf("machines: %+v", snap.Machines)
	}
	if snap.Seq < time.Now().Add(-time.Hour).UnixMicro() {
		t.Fatalf("merged seq %d is not in the merged range", snap.Seq)
	}

	// Reads are forwarded to the machine that has the session.
	if code, b := hub.call("GET", "/api/sessions/s_laptop1", f.cookie, nil); code != 200 || !strings.Contains(string(b), `"machine":"laptop"`) {
		t.Fatalf("forwarded read: %d %s", code, b)
	}

	// The laptop hasn't confirmed the phone's key: it can't answer yet.
	respond := func() (int, []byte) {
		return hub.call("POST", "/api/requests/r_laptop1/respond", f.cookie,
			hub.signAction(f.key, ActionDecide, "r_laptop1", respondBody{Behavior: api.BehaviorAllow}))
	}
	code, b := respond()
	if code != http.StatusForbidden || !strings.Contains(string(b), "untrusted_device") || !strings.Contains(string(b), "pilothouse link trust") {
		t.Fatalf("untrusted answer: %d %s", code, b)
	}
	if len(peer.fc.Decisions()) != 0 {
		t.Fatal("an untrusted device answered")
	}

	// The link token alone can't answer either (no device, or a forged signature).
	direct := func(device string, body any) int {
		bb, _ := json.Marshal(body)
		req, _ := http.NewRequest("POST", peer.url+"/api/link/m/requests/r_laptop1/respond", bytes.NewReader(bb))
		req.Header.Set("Authorization", "Bearer "+peerTokenOf(t, hub))
		if device != "" {
			req.Header.Set(linkHeaderDevice, device)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if c := direct("", hub.signAction(f.key, ActionDecide, "r_laptop1", respondBody{Behavior: api.BehaviorAllow})); c != http.StatusForbidden {
		t.Fatalf("token without device: %d", c)
	}

	// Confirm on the laptop, then the answer goes through, signed by the phone.
	w := trustAll(t, peer)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"trusted":1`) {
		t.Fatalf("trust: %d %s", w.Code, w.Body)
	}
	forged, _ := genKey(t)
	if c := direct(f.deviceID, hub.signAction(forged, ActionDecide, "r_laptop1", respondBody{Behavior: api.BehaviorAllow})); c != http.StatusForbidden {
		t.Fatalf("forged signature over the link: %d", c)
	}
	code, b = respond()
	if code != 200 {
		t.Fatalf("trusted answer: %d %s", code, b)
	}
	ds := peer.fc.Decisions()
	if len(ds) != 1 || ds[0].By != "device:"+f.deviceID || ds[0].Behavior != api.BehaviorAllow {
		t.Fatalf("decisions on the laptop: %+v", ds)
	}

	// Presence follows the phone everywhere.
	if code, b := hub.call("PUT", "/api/presence", f.cookie, api.PresenceBody{Mode: api.PresenceForceAway}); code != 200 {
		t.Fatalf("presence: %d %s", code, b)
	}
	eventually(t, "away on the laptop", func() bool { return peer.fc.Presence().Mode == api.PresenceForceAway })
}

// peerTokenOf reads the token the hub presents to its (only) linked machine.
func peerTokenOf(t *testing.T, hub *liveEnv) string {
	t.Helper()
	ls, err := hub.srv.listLinks(context.Background())
	if err != nil || len(ls) != 1 {
		t.Fatalf("links: %v %v", ls, err)
	}
	return ls[0].PeerToken
}

func TestLinkMergedEventStream(t *testing.T) {
	f := linkMachines(t)
	hub, peer := f.hub, f.peer
	var snap api.Snapshot
	eventually(t, "merged view", func() bool {
		snap = hub.snapshot(f.cookie)
		return hasSession(snap, "s_laptop1")
	})

	req, _ := http.NewRequest("GET", hub.url+"/api/events?after="+itoa64(snap.Seq), nil)
	req.Header.Set("Cookie", cookieName+"="+f.cookie)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	resp, err := http.DefaultClient.Do(req.WithContext(ctx))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	got := make(chan api.Event, 16)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		var ev string
		for sc.Scan() {
			line := sc.Text()
			switch {
			case strings.HasPrefix(line, "event: "):
				ev = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: ") && ev == "event":
				var e api.Event
				if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &e) == nil {
					got <- e
				}
			}
		}
	}()

	// One event from each machine, in one stream, with increasing numbers.
	peer.fc.UpdateSession("s_laptop1", func(s *api.Session) { s.State = api.StateWaiting })
	hub.fc.PutSession(api.Session{ID: "s_studio1", Machine: "studio", Agent: api.AgentCodex, State: api.StateWorking})
	seen := map[string]int64{}
	last := snap.Seq
	for len(seen) < 2 {
		select {
		case e := <-got:
			if e.Seq <= last {
				t.Fatalf("seq went from %d to %d", last, e.Seq)
			}
			last = e.Seq
			seen[e.SessionID] = e.Seq
		case <-ctx.Done():
			t.Fatalf("events seen: %v", seen)
		}
	}
	if seen["s_laptop1"] == 0 || seen["s_studio1"] == 0 {
		t.Fatalf("events seen: %v", seen)
	}
}

func TestLinkForwardsNotificationsWithoutOneTapAnswers(t *testing.T) {
	f := linkMachines(t)
	r, _ := f.peer.fc.Request(context.Background(), "r_laptop1")
	n := core.Notification{Kind: core.NotifyRequest, Title: "Run: git push", Request: &r, Remote: true}
	if err := f.peer.srv.LinkChannel().Send(context.Background(), n); err != nil {
		t.Fatal(err)
	}
	notes := f.notes.all()
	if len(notes) != 1 || !notes[0].Linked || notes[0].Title != "laptop · Run: git push" || notes[0].Request.ID != "r_laptop1" {
		t.Fatalf("forwarded: %+v", notes)
	}
	// A notification may open a session, request or task page, nothing else.
	for url, want := range map[string]string{"/r/r_laptop1": "/r/r_laptop1", "/api/sessions/s_laptop1": "", "//evil.example/x": "", "/s/../api": ""} {
		n.URL = url
		if err := f.peer.srv.LinkChannel().Send(context.Background(), n); err != nil {
			t.Fatal(err)
		}
		all := f.notes.all()
		if got := all[len(all)-1].URL; got != want {
			t.Fatalf("notification URL %q became %q, want %q", url, got, want)
		}
	}
}

func TestLinkRemoveAndCodes(t *testing.T) {
	f := linkMachines(t)
	hub, peer := f.hub, f.peer
	eventually(t, "merged view", func() bool { return hasSession(hub.snapshot(f.cookie), "s_laptop1") })

	// A link code can't pair a device, and a pairing code can't link.
	w := hub.admin("POST", "/v1/link/code", nil)
	var lc api.LinkCode
	mustJSON(t, w, &lc)
	_, pub := genKey(t)
	if w := hub.do("POST", "/api/pair", pairBody{Code: lc.Code, Name: "x", PublicKey: pub}, reqOpt{}); w.Code != http.StatusUnauthorized {
		t.Fatalf("pairing with a link code: %d %s", w.Code, w.Body)
	}

	// The laptop can't join a second hub, and the hub can't join anyone.
	if w := peer.admin("POST", "/v1/link/join", api.LinkJoin{URL: hub.url, Code: lc.Code, SelfURL: peer.url}); w.Code != http.StatusConflict {
		t.Fatalf("second join: %d %s", w.Code, w.Body)
	}
	if w := hub.admin("POST", "/v1/link/join", api.LinkJoin{URL: peer.url, Code: "XXXX-XXXX-XXXX", SelfURL: hub.url}); w.Code != http.StatusConflict {
		t.Fatalf("hub joining: %d %s", w.Code, w.Body)
	}

	// Status on both sides.
	var st api.LinkStatus
	mustJSON(t, hub.admin("GET", "/v1/link", nil), &st)
	if len(st.Linked) != 1 || st.Linked[0].Machine != "laptop" || !st.Linked[0].Online {
		t.Fatalf("hub status: %+v", st)
	}
	mustJSON(t, peer.admin("GET", "/v1/link", nil), &st)
	if st.Hub == nil || st.Hub.Machine != "studio" || len(st.Devices) != 1 {
		t.Fatalf("laptop status: %+v", st)
	}

	// Unlinking on the hub tells the laptop and drops its sessions.
	if w := hub.admin("DELETE", "/v1/link/laptop", nil); w.Code != 200 {
		t.Fatalf("remove: %d %s", w.Code, w.Body)
	}
	if hasSession(hub.snapshot(f.cookie), "s_laptop1") {
		t.Fatal("the laptop's session is still shown")
	}
	var after api.LinkStatus
	mustJSON(t, peer.admin("GET", "/v1/link", nil), &after)
	if after.Hub != nil {
		t.Fatalf("the laptop still has a hub: %+v", after.Hub)
	}
	// Its old token no longer works.
	req, _ := http.NewRequest("GET", peer.url+"/api/link/m/snapshot", nil)
	req.Header.Set("Authorization", "Bearer "+strings.Repeat("x", 43))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("bad token: %d", resp.StatusCode)
	}
}

func TestLinkNewDeviceKeyNeedsTrust(t *testing.T) {
	f := linkMachines(t)
	trustAll(t, f.peer)
	// The hub sends a changed key for the same device (paired again,
	// restored): the laptop stops trusting it until confirmed again.
	ctx := context.Background()
	ds, _ := f.peer.srv.listLinkDevices(ctx)
	oldKey := ""
	if err := f.peer.d().QueryRowContext(ctx, `SELECT public_key FROM web_link_devices WHERE id = ?`, f.deviceID).Scan(&oldKey); err != nil || !ds[0].Trusted {
		t.Fatalf("setup: %v %+v", err, ds)
	}
	ver := f.peer.clock.Now().Add(time.Hour).UnixMicro() // above anything the hub sent
	_, pub := genKey(t)
	if err := f.peer.srv.syncLinkDevices(ctx, []linkDeviceIn{{ID: f.deviceID, Name: "Test phone", PublicKey: pub}}, ver); err != nil {
		t.Fatal(err)
	}
	ds, _ = f.peer.srv.listLinkDevices(ctx)
	if len(ds) != 1 || ds[0].Trusted {
		t.Fatalf("new key trusted without asking: %+v", ds)
	}
	// An older list arriving late can't bring the old, trusted key back.
	if err := f.peer.srv.syncLinkDevices(ctx, []linkDeviceIn{{ID: f.deviceID, Name: "Test phone", PublicKey: oldKey}}, ver-1); !errors.Is(err, errStaleDevices) {
		t.Fatalf("stale list: %v", err)
	}
	ds, _ = f.peer.srv.listLinkDevices(ctx)
	if ds[0].Trusted || ds[0].Fingerprint != keyFingerprint(pub) {
		t.Fatalf("a stale list was applied: %+v", ds)
	}
	// Trust binds to the fingerprint shown: a different one trusts nothing.
	if n, _ := f.peer.srv.trustLinkDevices(ctx, []api.LinkTrustDevice{{ID: f.deviceID, Fingerprint: keyFingerprint(oldKey)}}); n != 0 {
		t.Fatal("trusted a key the person wasn't shown")
	}
	if w := f.peer.admin("POST", "/v1/link/trust", api.LinkTrust{}); w.Code != 400 {
		t.Fatalf("trust without naming devices: %d %s", w.Code, w.Body)
	}
	// A device the hub no longer lists is revoked here.
	if err := f.peer.srv.syncLinkDevices(ctx, nil, ver+1); err != nil {
		t.Fatal(err)
	}
	ds, _ = f.peer.srv.listLinkDevices(ctx)
	if ds[0].RevokedAt == nil {
		t.Fatalf("not revoked: %+v", ds)
	}
	if n, _ := f.peer.srv.trustLinkDevices(ctx, []api.LinkTrustDevice{{ID: f.deviceID, Fingerprint: ds[0].Fingerprint}}); n != 0 {
		t.Fatal("trusted a revoked device")
	}
}

// trustAll trusts every device the machine lists, with the fingerprints shown.
func trustAll(t *testing.T, e *liveEnv) *httptest.ResponseRecorder {
	t.Helper()
	ds, err := e.srv.listLinkDevices(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var want []api.LinkTrustDevice
	for _, d := range ds {
		want = append(want, api.LinkTrustDevice{ID: d.ID, Fingerprint: d.Fingerprint})
	}
	return e.admin("POST", "/v1/link/trust", api.LinkTrust{Devices: want})
}

func (e *liveEnv) d() *sql.DB { return e.db }

func TestCheckLinkURL(t *testing.T) {
	for in, want := range map[string]string{
		"https://studio.tail1.ts.net":      "https://studio.tail1.ts.net",
		"https://studio.tail1.ts.net/":     "https://studio.tail1.ts.net",
		"http://127.0.0.1:7517":            "http://127.0.0.1:7517",
		"http://studio.tail1.ts.net":       "",
		"https://studio.tail1.ts.net/x":    "",
		"https://u:p@studio.tail1.ts.net":  "",
		"ftp://studio":                     "",
		"studio.tail1.ts.net":              "",
		"https://studio.tail1.ts.net?a=1":  "",
		"https://studio.tail1.ts.net#frag": "",
	} {
		got, err := checkLinkURL(in)
		if (want == "") != (err != nil) || got != want {
			t.Errorf("%q: %q %v, want %q", in, got, err, want)
		}
	}
}

func TestFedStreamResync(t *testing.T) {
	e := newEnv(t)
	h := e.srv.links
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.ctx = ctx
	if err := h.merge(ctx); err != nil {
		t.Fatal(err)
	}
	h.mu.Lock()
	head := h.next - 1
	h.mu.Unlock()
	if s, _ := h.subscribe(ctx, 5); !s.Resync {
		t.Fatal("an old core sequence number must reload")
	}
	s, _ := h.subscribe(ctx, head)
	if s.Resync {
		t.Fatal("the head must stream")
	}
	h.push("", api.Event{Type: api.EventSessionUpdated, SessionID: "s_1"})
	if ev := <-s.C; ev.Seq != head+1 {
		t.Fatalf("seq %d, want %d", ev.Seq, head+1)
	}
	h.invalidate()
	if _, ok := <-s.C; ok {
		t.Fatal("invalidate must end streams")
	}
	if s2, _ := h.subscribe(ctx, head+1); !s2.Resync {
		t.Fatal("after invalidate a client must reload")
	}
	h.mu.Lock()
	head = h.next - 1
	h.mu.Unlock()
	if s3, _ := h.subscribe(ctx, head); s3.Resync {
		t.Fatal("a fresh snapshot's seq must stream")
	}
}

func itoa64(n int64) string { return strconv.FormatInt(n, 10) }

// "Open in herdr" on a linked machine's session opens it on that machine,
// which checks the phone's signature itself.
func TestLinkForwardsOpenOnDesktop(t *testing.T) {
	var dc *deskCore
	f := linkMachines(t, func(d *Deps) { dc = &deskCore{FakeCore: d.Core.(*webtest.FakeCore)}; d.Core = dc })
	hub := f.hub
	eventually(t, "the laptop's session on the hub", func() bool { return hasSession(hub.snapshot(f.cookie), "s_laptop1") })
	open := func(k *ecdsa.PrivateKey) (int, []byte) {
		return hub.call("POST", "/api/sessions/s_laptop1/desktop", f.cookie, hub.signAction(k, ActionDesktop, "s_laptop1", struct{}{}))
	}

	// Not yet trusted on the laptop: refused there.
	if code, b := open(f.key); code != http.StatusForbidden || !strings.Contains(string(b), "untrusted_device") {
		t.Fatalf("untrusted: %d %s", code, b)
	}
	if w := trustAll(t, f.peer); w.Code != 200 {
		t.Fatalf("trust: %d %s", w.Code, w.Body)
	}
	forged, _ := genKey(t)
	if code, b := open(forged); code != http.StatusForbidden {
		t.Fatalf("forged: %d %s", code, b)
	}
	code, b := open(f.key)
	if code != 200 || !strings.Contains(string(b), `"label":"herdr"`) {
		t.Fatalf("open: %d %s", code, b)
	}
	if got := dc.got(); len(got) != 1 || got[0] != "s_laptop1|device:"+f.deviceID {
		t.Fatalf("the laptop opened %q", got)
	}
}

// A linked machine can't pose as the hub's own sessions.
func TestLinkCantClaimTheHubsIDs(t *testing.T) {
	f := linkMachines(t)
	hub := f.hub
	hub.fc.PutSession(api.Session{ID: "s_shared01", Machine: "studio", Agent: api.AgentClaude, State: api.StateWorking, Title: "real"})
	f.peer.fc.PutSession(api.Session{ID: "s_shared01", Machine: "studio", Agent: api.AgentClaude, State: api.StateWorking, Title: "fake"})
	eventually(t, "the laptop's session on the hub", func() bool { return hasSession(hub.snapshot(f.cookie), "s_laptop1") })
	n := 0
	for _, x := range hub.snapshot(f.cookie).Sessions {
		if x.ID == "s_shared01" {
			n++
			if x.Title != "real" {
				t.Fatalf("the linked machine's copy is shown: %+v", x)
			}
		}
	}
	if n != 1 {
		t.Fatalf("s_shared01 shown %d times", n)
	}
	if p := hub.srv.links.ownerOf(context.Background(), "s_shared01"); p != nil {
		t.Fatalf("the hub's own session would be forwarded to %s", p.row.Machine)
	}
	// Its sessions carry the name the hub knows it by.
	f.peer.fc.PutSession(api.Session{ID: "s_laptop2", Machine: "studio", Agent: api.AgentClaude, State: api.StateWorking})
	eventually(t, "s_laptop2 on the hub", func() bool {
		for _, x := range hub.snapshot(f.cookie).Sessions {
			if x.ID == "s_laptop2" {
				return x.Machine == "laptop"
			}
		}
		return false
	})
}

// Whatever a linked machine answers, only JSON and event streams reach the
// browser, sandboxed: nothing can run on the hub's origin.
func TestLinkForwardOnlyPassesJSON(t *testing.T) {
	hub := newLiveEnv(t, "studio")
	evil := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/link/m/sessions/s_evil1":
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte(`<script src="/api/sessions/s_evil1/activity"></script>`))
		case "/api/link/m/sessions/s_evil1/activity":
			w.Header().Set("Content-Type", "text/javascript")
			_, _ = w.Write([]byte(`alert(1)`))
		default:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ok":true}`))
		}
	}))
	t.Cleanup(evil.Close)
	p := &peerConn{row: linkRow{ID: "l_evil", Machine: "evil", URL: evil.URL, PeerToken: "x"}}
	do := func(path string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", path, nil)
		r = r.WithContext(context.WithValue(r.Context(), ctxDevice, &deviceRow{Device: api.Device{ID: "d_test1"}}))
		w := httptest.NewRecorder()
		hub.srv.links.forward(w, r, p)
		return w
	}
	for _, path := range []string{"/api/sessions/s_evil1", "/api/sessions/s_evil1/activity"} {
		if w := do(path); w.Code != http.StatusBadGateway || strings.Contains(w.Body.String(), "script") || strings.Contains(w.Body.String(), "alert") {
			t.Fatalf("%s: %d %q %s", path, w.Code, w.Header().Get("Content-Type"), w.Body)
		}
	}
	w := do("/api/sessions/s_ok")
	if w.Code != 200 || w.Header().Get("Content-Type") != "application/json; charset=utf-8" ||
		w.Header().Get("X-Content-Type-Options") != "nosniff" || !strings.Contains(w.Header().Get("Content-Security-Policy"), "sandbox") {
		t.Fatalf("json: %d %v", w.Code, w.Header())
	}
}

// Joining checks the code before contacting anything, and machine names
// must differ.
func TestLinkJoinChecksCodeFirstAndNames(t *testing.T) {
	hub := newLiveEnv(t, "studio")
	var hits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"machine":"x"}`))
	}))
	t.Cleanup(target.Close)
	join := func(machine, code string) (int, []byte) {
		return hub.call("POST", "/api/link/hub/join", "", linkJoinBody{Code: code, Machine: machine, URL: target.URL, PeerToken: strings.Repeat("a", 40)})
	}
	if code, b := join("laptop", "AAAA-BBBB-CCCC"); code != http.StatusUnauthorized || hits.Load() != 0 {
		t.Fatalf("bad code: %d %s, %d fetches", code, b, hits.Load())
	}
	w := hub.admin("POST", "/v1/link/code", nil)
	var lc api.LinkCode
	mustJSON(t, w, &lc)
	if code, b := join("Studio", lc.Code); code != http.StatusConflict || !strings.Contains(string(b), "same name as the hub") || hits.Load() != 0 {
		t.Fatalf("hub's name: %d %s", code, b)
	}
}

// Device-list versions: a far-future one is refused (it would freeze the
// list); a stale one is answered 409 with the current version, and its
// explicit revocations still apply.
func TestLinkDeviceListVersions(t *testing.T) {
	f := linkMachines(t)
	ctx := context.Background()
	peer := f.peer
	if err := peer.srv.syncLinkDevices(ctx, nil, peer.clock.Now().Add(48*time.Hour).UnixMicro()); err == nil {
		t.Fatal("a version far in the future was accepted")
	}
	cur := peer.srv.devicesVersion(ctx)
	if cur == 0 {
		t.Fatal("no version recorded at join")
	}
	ds, _ := peer.srv.listLinkDevices(ctx)
	var pub string
	_ = peer.d().QueryRowContext(ctx, `SELECT public_key FROM web_link_devices WHERE id = ?`, f.deviceID).Scan(&pub)
	revoked := time.Now().UTC()
	err := peer.srv.syncLinkDevices(ctx, []linkDeviceIn{{ID: f.deviceID, Name: ds[0].Name, PublicKey: pub, RevokedAt: &revoked}}, cur-1)
	if !errors.Is(err, errStaleDevices) {
		t.Fatalf("stale list: %v", err)
	}
	ds, _ = peer.srv.listLinkDevices(ctx)
	if ds[0].RevokedAt == nil {
		t.Fatal("a revocation in a stale list was dropped")
	}
	// Over the link the machine says what it has, and the hub's parser reads it.
	hubToken := peerTokenOf(t, f.hub)
	bb, _ := json.Marshal(map[string]any{"devices": []linkDeviceIn{}, "version": cur - 5})
	req, _ := http.NewRequest("POST", peer.url+"/api/link/m/devices", bytes.NewReader(bb))
	req.Header.Set("Authorization", "Bearer "+hubToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	pe := peerStatusErr(resp).(*peerError)
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict || pe.Code != "stale_devices" || pe.Version != cur {
		t.Fatalf("stale answer: %d %+v (have %d)", resp.StatusCode, pe, cur)
	}
}

// Live session events from a linked machine carry the hub's name for it.
func TestLinkLiveEventsUseTheHubsName(t *testing.T) {
	f := linkMachines(t)
	h := f.hub.srv.links
	eventually(t, "the laptop followed", func() bool { return hasSession(f.hub.snapshot(f.cookie), "s_laptop1") })
	h.mu.Lock()
	var src string
	for id := range h.peers {
		src = id
	}
	h.mu.Unlock()
	b, _ := json.Marshal(api.Session{ID: "s_laptop9", Machine: "studio"})
	h.push(src, api.Event{Type: api.EventSessionUpdated, SessionID: "s_laptop9", Data: b})
	h.mu.Lock()
	last := h.ring[len(h.ring)-1]
	h.mu.Unlock()
	var x api.Session
	if err := json.Unmarshal(last.Data, &x); err != nil || x.ID != "s_laptop9" || x.Machine != "laptop" {
		t.Fatalf("live event: %s", last.Data)
	}
}

func TestPeerStatusErrReadsBothShapes(t *testing.T) {
	for body, want := range map[string]peerError{
		`{"error":"this link code is wrong","code":"invalid_code"}`: {Code: "invalid_code", Message: "this link code is wrong"},
		`{"error":{"code":"old","message":"older shape"}}`:          {Code: "old", Message: "older shape"},
		`{"error":"stale","code":"stale_devices","version":42}`:     {Code: "stale_devices", Message: "stale", Version: 42},
		`not json`: {},
	} {
		resp := &http.Response{StatusCode: 409, Body: io.NopCloser(strings.NewReader(body))}
		got := *peerStatusErr(resp).(*peerError)
		want.Status = 409
		if got != want {
			t.Fatalf("%s → %+v, want %+v", body, got, want)
		}
	}
}

// The hub's refusal reaches the person joining, word for word.
func TestLinkJoinErrorReachesTheJoiningMachine(t *testing.T) {
	hub := newLiveEnv(t, "studio")
	twin := newLiveEnv(t, "studio")
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	hub.srv.StartLinks(ctx)
	twin.srv.StartLinks(ctx)
	w := hub.admin("POST", "/v1/link/code", nil)
	var lc api.LinkCode
	mustJSON(t, w, &lc)
	w = twin.admin("POST", "/v1/link/join", api.LinkJoin{URL: hub.url, Code: lc.Code, SelfURL: twin.url})
	if w.Code == 200 || !strings.Contains(w.Body.String(), "same name as the hub") {
		t.Fatalf("join with the hub's name: %d %s", w.Code, w.Body)
	}
}
