package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/xasdta/pilothouse/internal/api"
	"github.com/xasdta/pilothouse/internal/core"
	"github.com/xasdta/pilothouse/internal/version"
)

// Linked machines: the link API between a hub and the machines linked to
// it, and the local routes behind `pilothouse link`.
//
//   - Linking: `pilothouse link add` on the hub makes a one-time code;
//     `pilothouse link join <hub URL> <code>` on the other machine sends
//     it to the hub with a fresh token the hub will present, and gets a
//     token of its own back. The hub first checks it can reach the
//     machine. Each side keeps the token it sends and a hash of the one it
//     receives.
//   - Hub → linked machine (/api/link/m/…, Bearer token): the hub reads
//     the machine's snapshot and events, sends it the hub's device list,
//     and forwards device calls. A forwarded call names the device; the
//     linked machine runs the same handler a paired device would reach,
//     so a signed action is checked here, against the device's key —
//     and only a key a person confirmed on this machine
//     (`pilothouse link trust`) may act. A new or replaced key may read,
//     not act. The hub's token alone can't answer anything.
//   - Linked machine → hub (/api/link/hub/…, Bearer token): notifications
//     (which the hub sends on without one-tap answers) and leaving.

const linkHeaderDevice = "X-Pilothouse-Device"

// appLinkRE is what a linked machine's notification may open on the hub.
var appLinkRE = regexp.MustCompile(`^/(?:[srt]/[A-Za-z0-9_-]{1,64})?$`)

// linkIDRE is a session or request id a linked machine may send.
var linkIDRE = regexp.MustCompile(`^[sr]_[A-Za-z0-9]{1,40}$`)

// maxLinkBody bounds what machines send each other: a notification can
// carry a whole plan or a large edit.
const maxLinkBody = 4 << 20

// decodeLinkBody reads a JSON body from another machine. Unlike device
// calls, unknown fields are ignored: the two machines may run different
// versions.
func decodeLinkBody(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxLinkBody)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", "invalid JSON body: "+err.Error())
		return false
	}
	return true
}

// ctxLinked marks a call that arrived over a link (its device is set).
const ctxLinked ctxKey = 100

func linkedCall(r *http.Request) bool {
	v, _ := r.Context().Value(ctxLinked).(bool)
	return v
}

func bearer(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if len(h) > 7 && strings.EqualFold(h[:7], "bearer ") {
		return strings.TrimSpace(h[7:])
	}
	return ""
}

// checkLinkURL accepts a machine's web URL: https with a host and nothing
// else, or plain http on loopback (two daemons on one computer, tests).
func checkLinkURL(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("give the machine's web address, like https://studio.tailnet.ts.net")
	}
	switch u.Scheme {
	case "https":
	case "http":
		if h := u.Hostname(); h != "127.0.0.1" && h != "localhost" && h != "::1" {
			return "", errors.New("the address must be https (run `pilothouse remote tailscale` on that machine)")
		}
	default:
		return "", errors.New("give the machine's web address, like https://studio.tailnet.ts.net")
	}
	return u.Scheme + "://" + u.Host, nil
}

// ---- linked side: the API the hub calls ----

// linkedRoutes are the device routes a hub may forward, served by the
// same handlers as for a paired device.
func (s *Server) linkedRoutes() *http.ServeMux {
	a := s.auth
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/snapshot", a(s.handleSnapshot))
	mux.HandleFunc("GET /api/events", a(s.handleEvents))
	mux.HandleFunc("GET /api/sessions/{id}", a(s.handleSession))
	mux.HandleFunc("GET /api/sessions/{id}/events", a(s.handleSessionEvents))
	mux.HandleFunc("POST /api/sessions/{id}/seen", a(csrf(s.handleSeen)))
	mux.HandleFunc("GET /api/requests/{id}", a(s.handleRequest))
	mux.HandleFunc("POST /api/requests/{id}/decide", a(csrf(s.handleDecide)))
	mux.HandleFunc("POST /api/requests/{id}/respond", a(csrf(s.handleRespond)))
	mux.HandleFunc("POST /api/sessions/{id}/reply", a(csrf(s.handleReply)))
	mux.HandleFunc("GET /api/sessions/{id}/activity", a(s.handleActivity))
	mux.HandleFunc("POST /api/sessions/{id}/terminal/token", a(csrf(s.handleTerminalToken)))
	mux.HandleFunc("GET /api/sessions/{id}/terminal", a(s.handleTerminalStream))
	mux.HandleFunc("POST /api/sessions/{id}/terminal/input", a(csrf(s.handleTerminalInput)))
	mux.HandleFunc("POST /api/sessions/{id}/desktop", a(csrf(s.handleOpenOnDesktop)))
	mux.HandleFunc("PUT /api/presence", a(csrf(s.handleSetPresence)))
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		writeErr(w, http.StatusNotFound, "not_found", "not available over a link")
	})
	return mux
}

// handleLinked serves /api/link/m/… for the hub.
func (s *Server) handleLinked(w http.ResponseWriter, r *http.Request) {
	tok := bearer(r)
	if tok == "" {
		writeErr(w, http.StatusUnauthorized, "unauthorized", "missing link token")
		return
	}
	hub, err := s.hubByToken(r.Context(), tok)
	if errors.Is(err, errNoLink) {
		writeErr(w, http.StatusUnauthorized, "unauthorized", s.machineName()+" isn't linked to this hub any more")
		return
	}
	if err != nil {
		s.writeCoreErr(w, err)
		return
	}
	sub := strings.TrimPrefix(r.URL.Path, "/api/link/m")
	switch {
	case sub == "/info" && r.Method == http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]string{"machine": s.machineName(), "version": version.Version})
		return
	case sub == "/devices" && r.Method == http.MethodPost:
		var b struct {
			Devices []linkDeviceIn `json:"devices"`
			Version int64          `json:"version"`
		}
		if !decodeLinkBody(w, r, &b) {
			return
		}
		if err := s.syncLinkDevices(r.Context(), b.Devices, b.Version); errors.Is(err, errStaleDevices) {
			// Tell the hub what this machine has, so it can send a newer
			// list (its clock may have gone back).
			writeJSON(w, http.StatusConflict, map[string]any{"code": "stale_devices", "error": "an older device list than this machine has", "version": s.devicesVersion(r.Context())})
			return
		} else if err != nil {
			s.writeCoreErr(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	case sub == "/leave" && r.Method == http.MethodPost:
		if err := s.deleteHub(r.Context()); err != nil {
			s.writeCoreErr(w, err)
			return
		}
		s.cutStreams() // what the hub had open here ends with the link
		s.log.Info("the hub removed this machine", "hub", hub.Machine)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	d := &deviceRow{Device: api.Device{ID: "link:" + hub.ID, Name: hub.Machine}, via: hub.Machine, untrusted: true}
	if id := r.Header.Get(linkHeaderDevice); id != "" {
		ld, err := s.linkDevice(r.Context(), id, hub.Machine)
		if errors.Is(err, errNoDevice) {
			writeErr(w, http.StatusForbidden, "unknown_device", fmt.Sprintf("%s doesn't know this device yet. Try again in a moment.", s.machineName()))
			return
		}
		if err != nil {
			s.writeCoreErr(w, err)
			return
		}
		d = ld
	}
	ctx := context.WithValue(context.WithValue(r.Context(), ctxDevice, d), ctxLinked, true)
	r2 := r.Clone(ctx)
	r2.URL.Path = "/api" + sub
	r2.URL.RawPath = ""
	r2.Header.Set("X-Pilothouse", "1")
	r2.Header.Set("Origin", originOf(r).base)
	s.linked.ServeHTTP(w, r2)
}

// untrustedMsg is what a device sees when it acts on a linked machine
// that hasn't confirmed its key.
func (s *Server) untrustedMsg() string {
	m := s.machineName()
	return fmt.Sprintf("%s hasn't confirmed this device yet. On %s, run `pilothouse link trust`.", m, m)
}

// ---- linked side: notifications go to the hub ----

// linkChannel sends this machine's notifications for a person who is
// away to its hub, which delivers them to the hub's devices.
type linkChannel struct{ s *Server }

// LinkChannel is the notify channel that forwards to this machine's hub
// (it does nothing while there is none).
func (s *Server) LinkChannel() interface {
	Name() string
	Remote() bool
	Send(ctx context.Context, n core.Notification) error
	Withdraw(ctx context.Context, r api.Request) error
} {
	return linkChannel{s}
}

func (linkChannel) Name() string { return "link" }
func (linkChannel) Remote() bool { return true }

func (c linkChannel) Send(ctx context.Context, n core.Notification) error {
	if n.Kind == core.NotifyTest || n.Linked {
		return nil
	}
	return c.s.toHub(ctx, "/notify", n)
}

func (c linkChannel) Withdraw(ctx context.Context, r api.Request) error {
	return c.s.toHub(ctx, "/withdraw", r)
}

func (s *Server) toHub(ctx context.Context, path string, v any) error {
	h, err := s.hub(ctx)
	if err != nil || h == nil || h.HubToken == "" {
		return err
	}
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.URL+"/api/link/hub"+path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+h.HubToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.links.client.Do(req)
	if err != nil {
		return fmt.Errorf("hub %s: %w", h.Machine, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("hub %s: %w", h.Machine, peerStatusErr(resp))
	}
	return nil
}

// ---- hub side: calls from linked machines ----

type linkJoinBody struct {
	Code      string `json:"code"`
	Machine   string `json:"machine"`
	URL       string `json:"url"`
	PeerToken string `json:"peer_token"`
	Version   string `json:"version,omitempty"`
}

type linkJoinAnswer struct {
	ID       string         `json:"id"`
	Machine  string         `json:"machine"`
	HubToken string         `json:"hub_token"`
	Version  string         `json:"version"`
	Devices  []linkDeviceIn `json:"devices"`
	// DevicesVersion orders this list against later ones (devVersion).
	DevicesVersion int64 `json:"devices_version"`
}

// handleLinkJoin links the calling machine to this hub (one-time code).
func (s *Server) handleLinkJoin(w http.ResponseWriter, r *http.Request) {
	if ok, wait := s.pairLimit.allow(clientKey(r)); !ok {
		w.Header().Set("Retry-After", itoa(int(wait.Seconds())+1))
		writeErr(w, http.StatusTooManyRequests, "rate_limited", "too many attempts; wait a minute and try again")
		return
	}
	var b linkJoinBody
	if !decodeLinkBody(w, r, &b) {
		return
	}
	if h, err := s.hub(r.Context()); err != nil || h != nil {
		writeErr(w, http.StatusConflict, "not_a_hub", fmt.Sprintf("%s is linked to another hub, so it can't be one. Link both machines to the same hub.", s.machineName()))
		return
	}
	u, err := checkLinkURL(b.URL)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	machine := cleanName(b.Machine)
	if machine == "" || len(b.PeerToken) < 32 || normalizeCode(b.Code) == "" {
		writeErr(w, http.StatusBadRequest, "bad_request", "machine, peer_token and code are required")
		return
	}
	// The code first: nothing is fetched for someone without one.
	if ok, err := s.codeValid(r.Context(), b.Code, "link"); err != nil {
		s.writeCoreErr(w, err)
		return
	} else if !ok {
		s.pairLimit.fail(clientKey(r))
		writeErr(w, http.StatusUnauthorized, "invalid_code", "this link code is wrong, used or expired — run `pilothouse link add` on the hub again")
		return
	}
	// Names tell machines apart in the app: each must be unique.
	if strings.EqualFold(machine, s.machineName()) {
		writeErr(w, http.StatusConflict, "name_taken", fmt.Sprintf("this machine has the same name as the hub (%s). Give it another with `machine:` in ~/.pilothouse/config.yaml, restart Pilothouse, and join again.", machine))
		return
	}
	if links, err := s.listLinks(r.Context()); err == nil {
		for _, l := range links {
			if strings.EqualFold(l.Machine, machine) && l.URL != u {
				writeErr(w, http.StatusConflict, "name_taken", fmt.Sprintf("another machine called %s is already linked. Give this one another name with `machine:` in ~/.pilothouse/config.yaml and join again, or unlink the other with `pilothouse link remove %s` on the hub.", machine, machine))
				return
			}
		}
	}
	// Reach the machine before spending the code, so a wrong address can
	// be fixed and retried.
	probe := &peerConn{row: linkRow{Machine: machine, URL: u, PeerToken: b.PeerToken}}
	var info struct {
		Machine string `json:"machine"`
	}
	if err := s.links.call(r.Context(), probe, http.MethodGet, "/info", nil, &info, 6*time.Second); err != nil {
		s.pairLimit.fail(clientKey(r))
		writeErr(w, http.StatusBadGateway, "unreachable", fmt.Sprintf("%s can't reach %s (%v). Check that remote access is on there (`pilothouse remote tailscale`).", s.machineName(), u, err))
		return
	}
	if _, ok, err := s.consumeCode(r.Context(), b.Code, "link"); err != nil {
		s.writeCoreErr(w, err)
		return
	} else if !ok {
		s.pairLimit.fail(clientKey(r))
		writeErr(w, http.StatusUnauthorized, "invalid_code", "this link code is wrong, used or expired — run `pilothouse link add` on the hub again")
		return
	}
	links, err := s.listLinks(r.Context())
	if err != nil {
		s.writeCoreErr(w, err)
		return
	}
	for _, l := range links {
		if l.URL == u { // linking again replaces the old link
			_ = s.deleteLink(r.Context(), l.ID)
			s.links.remove(l.ID)
		}
	}
	row := linkRow{ID: newLinkID(), Machine: machine, URL: u, PeerToken: b.PeerToken, CreatedAt: s.now()}
	hubToken := randomToken(32)
	if err := s.insertLink(r.Context(), row, hubToken); err != nil {
		s.writeCoreErr(w, err)
		return
	}
	ver := s.links.devVersion()
	ds, err := s.links.hubDevices(r.Context())
	if err != nil {
		s.writeCoreErr(w, err)
		return
	}
	s.log.Info("linked a machine", "machine", machine, "url", u)
	s.links.add(row)
	writeJSON(w, http.StatusOK, linkJoinAnswer{ID: row.ID, Machine: s.machineName(), HubToken: hubToken, Version: version.Version, Devices: ds, DevicesVersion: ver})
}

// handleLinkHub serves the other /api/link/hub/… calls (Bearer token).
func (s *Server) handleLinkHub(w http.ResponseWriter, r *http.Request) {
	l, err := s.linkByToken(r.Context(), bearer(r))
	if errors.Is(err, errNoLink) {
		writeErr(w, http.StatusUnauthorized, "unauthorized", "this machine isn't linked to "+s.machineName()+" any more")
		return
	}
	if err != nil {
		s.writeCoreErr(w, err)
		return
	}
	switch r.PathValue("op") {
	case "notify":
		var n core.Notification
		if !decodeLinkBody(w, r, &n) {
			return
		}
		n.Linked = true
		if !appLinkRE.MatchString(n.URL) { // a session, request or task page, nothing else
			n.URL = ""
		}
		if (n.Request != nil && !linkIDRE.MatchString(n.Request.ID)) || (n.Session != nil && !linkIDRE.MatchString(n.Session.ID)) {
			writeErr(w, http.StatusBadRequest, "bad_request", "invalid session or request id")
			return
		}
		if n.Session != nil {
			n.Session.Machine = l.Machine
		}
		if n.Title != "" && !strings.HasPrefix(n.Title, l.Machine) {
			n.Title = l.Machine + " · " + n.Title
		}
		if nt := s.notifier(); nt != nil {
			_ = nt.Notify(r.Context(), n)
		}
	case "withdraw":
		var req api.Request
		if !decodeLinkBody(w, r, &req) {
			return
		}
		if wd, ok := s.notifier().(interface {
			Withdraw(context.Context, api.Request) error
		}); ok {
			_ = wd.Withdraw(r.Context(), req)
		}
	case "leave":
		if err := s.deleteLink(r.Context(), l.ID); err != nil && !errors.Is(err, errNoLink) {
			s.writeCoreErr(w, err)
			return
		}
		s.links.remove(l.ID)
		s.log.Info("a linked machine left", "machine", l.Machine)
	default:
		writeErr(w, http.StatusNotFound, "not_found", "no such link call")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// SetNotifier gives the server the daemon's notifier, which delivers what
// linked machines forward.
func (s *Server) SetNotifier(n core.Notifier) {
	s.notifyMu.Lock()
	defer s.notifyMu.Unlock()
	s.notify = n
}

func (s *Server) notifier() core.Notifier {
	s.notifyMu.Lock()
	defer s.notifyMu.Unlock()
	return s.notify
}

// ---- local routes (pilothouse link) ----

func (s *Server) handleLocalLinkCode(w http.ResponseWriter, r *http.Request) {
	if h, err := s.hub(r.Context()); err != nil || h != nil {
		writeErr(w, http.StatusConflict, "not_a_hub", fmt.Sprintf("this machine is linked to %s. Run `pilothouse link add` there instead.", hubName(h)))
		return
	}
	code, exp, err := s.createCode(r.Context(), "link", pairTTL)
	if err != nil {
		s.writeCoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, api.LinkCode{Code: code, URL: s.PublicURL(), ExpiresAt: exp.UTC()})
}

func hubName(h *hubRow) string {
	if h == nil || h.Machine == "" {
		return "another hub"
	}
	return h.Machine
}

func (s *Server) handleLocalLinkJoin(w http.ResponseWriter, r *http.Request) {
	var b api.LinkJoin
	if !decodeBody(w, r, &b) {
		return
	}
	ctx := r.Context()
	if h, err := s.hub(ctx); err != nil || h != nil {
		writeErr(w, http.StatusConflict, "already_linked", fmt.Sprintf("this machine is already linked to %s. Run `pilothouse link remove %s` first.", hubName(h), hubName(h)))
		return
	}
	if ls, err := s.listLinks(ctx); err == nil && len(ls) > 0 {
		writeErr(w, http.StatusConflict, "is_hub", "other machines are linked to this one, so it can't join a hub. Link every machine to the same hub.")
		return
	}
	hubURL, err := checkLinkURL(b.URL)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	self := b.SelfURL
	if self == "" {
		self = s.PublicURL()
	}
	if self == "" {
		writeErr(w, http.StatusConflict, "no_remote", "turn on remote access on this machine first (`pilothouse remote tailscale`), so the hub can reach it")
		return
	}
	if self, err = checkLinkURL(self); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	// Accept the hub's calls before asking it to link: it checks it can
	// reach this machine first.
	peerToken := randomToken(32)
	if err := s.setHub(ctx, hubRow{ID: "pending", URL: hubURL, CreatedAt: s.now()}, peerToken); err != nil {
		s.writeCoreErr(w, err)
		return
	}
	ans, err := s.joinHub(ctx, hubURL, linkJoinBody{Code: b.Code, Machine: s.machineName(), URL: self, PeerToken: peerToken, Version: version.Version})
	if err != nil {
		_ = s.deleteHub(context.WithoutCancel(ctx))
		var pe *peerError
		if errors.As(err, &pe) && pe.Message != "" {
			writeErr(w, http.StatusBadGateway, orDefault(pe.Code, "link_failed"), pe.Message)
			return
		}
		writeErr(w, http.StatusBadGateway, "unreachable", fmt.Sprintf("can't reach %s: %v", hubURL, err))
		return
	}
	if _, err := s.d.DB.ExecContext(ctx, `UPDATE web_link_hub SET id = ?, machine = ?, hub_token = ?`, ans.ID, cleanName(ans.Machine), ans.HubToken); err != nil {
		s.writeCoreErr(w, err)
		return
	}
	// The hub may already have sent a newer list while it followed this
	// machine: then this one is stale, which is fine.
	if err := s.syncLinkDevices(ctx, ans.Devices, max(ans.DevicesVersion, 1)); err != nil && !errors.Is(err, errStaleDevices) {
		s.writeCoreErr(w, err)
		return
	}
	ds, err := s.listLinkDevices(ctx)
	if err != nil {
		s.writeCoreErr(w, err)
		return
	}
	s.log.Info("linked to a hub", "hub", ans.Machine, "url", hubURL)
	writeJSON(w, http.StatusOK, api.LinkJoined{
		Hub:     api.LinkPeer{ID: ans.ID, Machine: ans.Machine, URL: hubURL, Online: true, Version: ans.Version, Since: s.now().UTC()},
		Devices: ds,
	})
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

func (s *Server) joinHub(ctx context.Context, hubURL string, b linkJoinBody) (linkJoinAnswer, error) {
	var ans linkJoinAnswer
	body, _ := json.Marshal(b)
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, hubURL+"/api/link/hub/join", bytes.NewReader(body))
	if err != nil {
		return ans, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.links.client.Do(req)
	if err != nil {
		return ans, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ans, peerStatusErr(resp)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&ans); err != nil {
		return ans, err
	}
	if ans.ID == "" || len(ans.HubToken) < 32 {
		return ans, errors.New("the hub's answer is incomplete")
	}
	return ans, nil
}

func (s *Server) handleLocalLinkStatus(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	st := api.LinkStatus{Machine: s.machineName(), URL: s.PublicURL()}
	h, err := s.hub(ctx)
	if err != nil {
		s.writeCoreErr(w, err)
		return
	}
	if h != nil {
		st.Hub = &api.LinkPeer{ID: h.ID, Machine: h.Machine, URL: h.URL, Since: h.CreatedAt}
		// Is the hub reachable from here?
		pctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		req, _ := http.NewRequestWithContext(pctx, http.MethodGet, h.URL+"/manifest.webmanifest", nil)
		if resp, err := s.links.client.Do(req); err == nil {
			resp.Body.Close()
			st.Hub.Online = true
		} else {
			st.Hub.Error = "can't reach " + h.URL
		}
		cancel()
		if st.Devices, err = s.listLinkDevices(ctx); err != nil {
			s.writeCoreErr(w, err)
			return
		}
	}
	links, err := s.listLinks(ctx)
	if err != nil {
		s.writeCoreErr(w, err)
		return
	}
	state := map[string]api.MachineInfo{}
	s.links.mu.Lock()
	for id, p := range s.links.peers {
		state[id] = p.info()
	}
	s.links.mu.Unlock()
	for _, l := range links {
		p := api.LinkPeer{ID: l.ID, Machine: l.Machine, URL: l.URL, Since: l.CreatedAt}
		if m, ok := state[l.ID]; ok {
			p.Online, p.Version, p.LastSeen, p.Error = m.Online, m.Version, m.LastSeen, m.Error
		}
		st.Linked = append(st.Linked, p)
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) handleLocalLinkTrust(w http.ResponseWriter, r *http.Request) {
	var b api.LinkTrust
	if !decodeBody(w, r, &b) {
		return
	}
	if len(b.Devices) == 0 {
		writeErr(w, http.StatusBadRequest, "bad_request", "name the devices to trust, with the fingerprints shown")
		return
	}
	n, err := s.trustLinkDevices(r.Context(), b.Devices)
	if err != nil {
		s.writeCoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"trusted": n})
}

// handleLocalLinkRemove unlinks a machine (on the hub) or leaves the hub
// (on a linked machine). {id} is a link id or a machine name.
func (s *Server) handleLocalLinkRemove(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := r.PathValue("id")
	if h, err := s.hub(ctx); err == nil && h != nil && (h.ID == id || strings.EqualFold(h.Machine, id)) {
		_ = s.toHub(ctx, "/leave", struct{}{})
		if err := s.deleteHub(ctx); err != nil {
			s.writeCoreErr(w, err)
			return
		}
		s.cutStreams()
		writeJSON(w, http.StatusOK, api.LinkPeer{ID: h.ID, Machine: h.Machine, URL: h.URL})
		return
	}
	links, err := s.listLinks(ctx)
	if err != nil {
		s.writeCoreErr(w, err)
		return
	}
	for _, l := range links {
		if l.ID != id && !strings.EqualFold(l.Machine, id) {
			continue
		}
		s.links.mu.Lock()
		p := s.links.peers[l.ID]
		s.links.mu.Unlock()
		if p == nil {
			p = &peerConn{row: l}
		}
		if err := s.links.call(ctx, p, http.MethodPost, "/leave", struct{}{}, nil, 3*time.Second); err != nil { // best effort
			s.log.Warn("links: tell the machine it was unlinked", "machine", l.Machine, "err", err)
		}
		if err := s.deleteLink(ctx, l.ID); err != nil {
			s.writeCoreErr(w, err)
			return
		}
		s.links.remove(l.ID)
		writeJSON(w, http.StatusOK, api.LinkPeer{ID: l.ID, Machine: l.Machine, URL: l.URL})
		return
	}
	writeErr(w, http.StatusNotFound, "not_found", "no linked machine called "+id+" (see `pilothouse link ls`)")
}
