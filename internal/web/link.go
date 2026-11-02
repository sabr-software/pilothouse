package web

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/xasdta/pilothouse/internal/api"
	"github.com/xasdta/pilothouse/internal/core"
)

// Linked machines, hub side (`pilothouse link`).
//
// A hub shows its own sessions and those of the machines linked to it in
// one view. It follows each linked machine's event stream over the
// machine's own web URL (on the tailnet), merges those events with its
// own into one stream with sequence numbers of its own, and forwards
// what a device does with a linked machine's session or request to that
// machine unchanged: the linked machine checks the device's signature
// itself, against a key a person confirmed there (linked.go). The hub
// can't answer for a device.
//
// While no machine is linked the hub is out of the way: snapshots and
// events are the core's own, sequence numbers included.

const (
	fedRingCap   = 4096 // merged events kept for clients that reconnect
	fedSubBuffer = 1024 // events a slow client may fall behind before it must reload
	peerTimeout  = 4 * time.Second
	peerSilence  = 50 * time.Second // a linked machine pings every 15 s
	maxForwarded = 1 << 20
)

// peerEventTypes are the events a linked machine contributes to the
// merged stream. Presence, devices, rules and tasks are per machine, and
// the app shows the hub's.
var peerEventTypes = map[api.EventType]bool{
	api.EventSessionStarted: true, api.EventSessionUpdated: true, api.EventSessionExited: true,
	api.EventRequestOpened: true, api.EventRequestClosed: true, api.EventSessionReply: true,
}

type peerConn struct {
	row    linkRow
	cancel context.CancelFunc
	// guarded by linkHub.mu
	online   bool
	version  string
	lastSeen time.Time
	errMsg   string
	snap     *api.Snapshot // last snapshot fetched
}

func (p *peerConn) info() api.MachineInfo {
	m := api.MachineInfo{Name: p.row.Machine, Online: p.online, Version: p.version, Error: p.errMsg}
	if !p.lastSeen.IsZero() {
		t := p.lastSeen.UTC()
		m.LastSeen = &t
	}
	return m
}

type fedSub struct {
	ch     chan api.Event
	closed bool
}

type linkHub struct {
	s      *Server
	client *http.Client

	mu         sync.Mutex
	ctx        context.Context // nil until StartLinks
	merged     bool
	next       int64 // next merged sequence number
	floor      int64 // a client must reload if its `after` is below floor-1
	lastDevVer int64 // last device-list version sent (devVersion)
	ring       []api.Event
	subs       map[*fedSub]struct{}
	owner      map[string]string // session or request id → link id, or localOwner
	peers      map[string]*peerConn
}

func newLinkHub(s *Server) *linkHub {
	return &linkHub{
		s: s,
		// Never follow redirects: a link URL must be the machine itself
		// (a redirect could reach an address checkLinkURL refuses).
		client: &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
		subs:   map[*fedSub]struct{}{},
		owner:  map[string]string{},
		peers:  map[string]*peerConn{},
	}
}

// StartLinks follows the machines linked to this one, and keeps this
// machine's own hub (if it has one) up to date, until ctx ends. The
// daemon calls it once the web listener is up.
func (s *Server) StartLinks(ctx context.Context) {
	h := s.links
	h.mu.Lock()
	h.ctx = ctx
	h.mu.Unlock()
	links, err := s.listLinks(ctx)
	if err != nil {
		s.log.Warn("links", "err", err)
		return
	}
	for _, l := range links {
		h.add(l)
	}
}

// add starts following a linked machine, merging views if it is the first.
func (h *linkHub) add(l linkRow) {
	h.mu.Lock()
	ctx := h.ctx
	if ctx == nil || ctx.Err() != nil {
		h.mu.Unlock()
		return
	}
	if old := h.peers[l.ID]; old != nil {
		old.cancel()
	}
	pctx, cancel := context.WithCancel(ctx)
	p := &peerConn{row: l, cancel: cancel}
	h.peers[l.ID] = p
	first := !h.merged
	h.mu.Unlock()
	if first {
		if err := h.merge(ctx); err != nil {
			h.s.log.Warn("links: merge views", "err", err)
		}
	}
	go h.follow(pctx, p)
}

// remove stops following a linked machine and drops its sessions.
func (h *linkHub) remove(id string) {
	h.mu.Lock()
	p := h.peers[id]
	if p == nil {
		h.mu.Unlock()
		return
	}
	p.cancel()
	delete(h.peers, id)
	for k, v := range h.owner {
		if v == id {
			delete(h.owner, k)
		}
	}
	h.mu.Unlock()
	h.invalidate() // its sessions disappear with the next snapshot
}

// merge switches to the merged stream. Live event streams are cut, so
// clients reconnect, find their sequence number unknown and reload.
func (h *linkHub) merge(ctx context.Context) error {
	snap, err := h.s.d.Core.Snapshot(ctx)
	if err != nil {
		return err
	}
	sub, err := h.s.d.Core.Subscribe(ctx, snap.Seq)
	if err != nil {
		return err
	}
	h.mu.Lock()
	if h.merged {
		h.mu.Unlock()
		sub.Close()
		return nil
	}
	h.merged = true
	// Wall-clock microseconds: above any core sequence number, and above
	// any merged number handed out before a restart.
	base := time.Now().UnixMicro()
	h.next, h.floor = base, base
	h.mu.Unlock()
	h.s.cutStreams()
	go h.pumpLocal(ctx, sub)
	return nil
}

// pumpLocal feeds this machine's own events into the merged stream. If
// the core drops the subscription, clients reload.
func (h *linkHub) pumpLocal(ctx context.Context, sub *core.Subscription) {
	for {
		for ev := range sub.C {
			h.push("", ev)
		}
		sub.Close()
		if ctx.Err() != nil {
			return
		}
		h.invalidate()
		snap, err := h.s.d.Core.Snapshot(ctx)
		if err == nil {
			sub, err = h.s.d.Core.Subscribe(ctx, snap.Seq)
		}
		if err != nil {
			h.s.log.Warn("links: follow this machine", "err", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Second):
			}
			continue
		}
		if sub.Resync {
			continue
		}
		go h.pushDevices() // a device change may have fallen between the two subscriptions
	}
}

// push adds an event from src ("" = this machine, else a link id) to the
// merged stream.
func (h *linkHub) push(src string, ev api.Event) {
	if ev.Type == api.EventHook || (src != "" && !peerEventTypes[ev.Type]) {
		return
	}
	h.mu.Lock()
	ctx := h.ctx
	h.mu.Unlock()
	if src != "" && ctx != nil && (h.localID(ctx, ev.SessionID) || h.localID(ctx, ev.RequestID)) {
		h.s.log.Warn("links: a linked machine sent an event for this machine's session", "link", src, "session", ev.SessionID)
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.merged || (src != "" && h.peers[src] == nil) {
		return // not merging, or the machine was removed meanwhile
	}
	if src != "" && (!h.claimable(ev.SessionID, src) || !h.claimable(ev.RequestID, src)) {
		return // another machine's id
	}
	if src != "" && strings.HasPrefix(string(ev.Type), "session.") && len(ev.Data) > 0 {
		// Its sessions carry the name this hub knows it by, not one it chose.
		var x api.Session
		if json.Unmarshal(ev.Data, &x) == nil && x.ID != "" {
			x.Machine = h.peers[src].row.Machine
			if b, err := json.Marshal(x); err == nil {
				ev.Data = b
			}
		}
	}
	if ev.SessionID != "" {
		h.setOwner(ev.SessionID, src)
	}
	if ev.RequestID != "" {
		h.setOwner(ev.RequestID, src)
	}
	ev.Seq = h.next
	h.next++
	h.ring = append(h.ring, ev)
	if len(h.ring) > fedRingCap {
		h.ring = append(h.ring[:0:0], h.ring[len(h.ring)-fedRingCap:]...)
	}
	if len(h.ring) > 0 {
		h.floor = h.ring[0].Seq
	}
	for sub := range h.subs {
		select {
		case sub.ch <- ev:
		default: // fell behind: it must reload
			h.dropLocked(sub)
		}
	}
	if src == "" && ev.Type == api.EventDeviceChanged {
		go h.pushDevices()
	}
}

// localOwner marks an id as this machine's in owner.
const localOwner = "\x00local"

func (h *linkHub) setOwner(id, src string) {
	if src == "" {
		src = localOwner
	}
	if len(h.owner) > 100_000 { // ids only accumulate; start over well before it matters
		clear(h.owner)
	}
	h.owner[id] = src
}

// claimable reports whether linked machine src may show id as its own:
// an id this machine or another linked machine already has is refused,
// so a linked machine can't pose as them (and receive what the person
// sends there). Call with h.mu held.
func (h *linkHub) claimable(id, src string) bool {
	o, ok := h.owner[id]
	return id == "" || !ok || o == src
}

// localID reports whether this machine itself has the session or request.
func (h *linkHub) localID(ctx context.Context, id string) bool {
	switch {
	case strings.HasPrefix(id, "s_"):
		_, err := h.s.d.Core.Session(ctx, id)
		return err == nil
	case strings.HasPrefix(id, "r_"):
		_, err := h.s.d.Core.Request(ctx, id)
		return err == nil
	}
	return false
}

func (h *linkHub) dropLocked(sub *fedSub) {
	if !sub.closed {
		sub.closed = true
		close(sub.ch)
	}
	delete(h.subs, sub)
}

// invalidate makes every client reload its snapshot: events may have
// been missed (a linked machine reconnected, or this machine's stream
// was dropped).
func (h *linkHub) invalidate() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.merged {
		return
	}
	h.next++ // a client that saw everything still lands below floor-1
	h.floor = h.next
	h.ring = nil
	for sub := range h.subs {
		h.dropLocked(sub)
	}
}

// subscribe is core.Core.Subscribe over the merged stream.
func (h *linkHub) subscribe(ctx context.Context, after int64) (*core.Subscription, error) {
	h.mu.Lock()
	if !h.merged {
		h.mu.Unlock()
		return h.s.d.Core.Subscribe(ctx, after)
	}
	defer h.mu.Unlock()
	if after < h.floor-1 || after > h.next-1 {
		return &core.Subscription{Resync: true, Close: func() {}}, nil
	}
	sub := &fedSub{ch: make(chan api.Event, fedSubBuffer)}
	for _, ev := range h.ring {
		if ev.Seq <= after {
			continue
		}
		select {
		case sub.ch <- ev:
		default:
			return &core.Subscription{Resync: true, Close: func() {}}, nil
		}
	}
	h.subs[sub] = struct{}{}
	return &core.Subscription{C: sub.ch, Close: func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.dropLocked(sub)
	}}, nil
}

// snapshot is core.Core.Snapshot over every machine. Its Seq is the
// merged stream's position when it started: each machine's part is
// fetched afterwards, so it is at least that new, and events after Seq
// replay on top of it (they carry whole objects, so replaying is safe).
func (h *linkHub) snapshot(ctx context.Context) (api.Snapshot, error) {
	h.mu.Lock()
	if !h.merged {
		h.mu.Unlock()
		return h.s.d.Core.Snapshot(ctx)
	}
	seq := h.next - 1
	peers := make([]*peerConn, 0, len(h.peers))
	for _, p := range h.peers {
		peers = append(peers, p)
	}
	h.mu.Unlock()

	snap, err := h.s.d.Core.Snapshot(ctx)
	if err != nil {
		return snap, err
	}
	snap.Seq = seq
	parts := make([]*api.Snapshot, len(peers))
	var wg sync.WaitGroup
	for i, p := range peers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var ps api.Snapshot
			if err := h.call(ctx, p, http.MethodGet, "/snapshot", nil, &ps, peerTimeout); err == nil {
				parts[i] = &ps
			}
		}()
	}
	wg.Wait()

	h.mu.Lock()
	defer h.mu.Unlock()
	for _, x := range snap.Sessions {
		h.setOwner(x.ID, "")
	}
	for _, r := range snap.Requests {
		h.setOwner(r.ID, "")
	}
	snap.Machines = []api.MachineInfo{{Name: snap.Machine, Self: true, Online: true, Version: snap.Version}}
	for i, p := range peers {
		if h.peers[p.row.ID] != p {
			continue // removed meanwhile
		}
		if ps := parts[i]; ps != nil {
			p.snap, p.online, p.version, p.lastSeen, p.errMsg = ps, true, ps.Version, h.s.now(), ""
		}
		snap.Machines = append(snap.Machines, p.info())
		if p.snap == nil {
			continue // never reached: nothing to show yet
		}
		for _, x := range p.snap.Sessions {
			if !h.claimable(x.ID, p.row.ID) {
				continue
			}
			h.setOwner(x.ID, p.row.ID)
			x.Machine = p.row.Machine // the name this hub knows it by, not what it claims
			snap.Sessions = append(snap.Sessions, x)
		}
		for _, r := range p.snap.Requests {
			if !h.claimable(r.ID, p.row.ID) || !h.claimable(r.SessionID, p.row.ID) {
				continue
			}
			h.setOwner(r.ID, p.row.ID)
			snap.Requests = append(snap.Requests, r)
		}
	}
	return snap, nil
}

// follow keeps a linked machine's part of the merged view up to date:
// load its snapshot, send it this hub's devices, then stream its events.
// It reconnects with backoff until ctx ends.
func (h *linkHub) follow(ctx context.Context, p *peerConn) {
	backoff := time.Second
	for ctx.Err() == nil {
		began := time.Now()
		err := h.followOnce(ctx, p)
		if ctx.Err() != nil {
			return
		}
		if time.Since(began) > time.Minute {
			backoff = time.Second // it was up for a while: retry soon
		}
		h.mu.Lock()
		was := p.online
		p.online = false
		p.errMsg = peerErrText(p.row.Machine, err)
		h.mu.Unlock()
		if was {
			h.s.log.Info("linked machine offline", "machine", p.row.Machine, "err", err)
			h.machineChanged(p)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 30*time.Second)
	}
}

func (h *linkHub) followOnce(ctx context.Context, p *peerConn) error {
	var snap api.Snapshot
	if err := h.call(ctx, p, http.MethodGet, "/snapshot", nil, &snap, peerTimeout); err != nil {
		return err
	}
	if err := h.sendDevices(ctx, p); err != nil {
		return err
	}
	h.mu.Lock()
	if h.peers[p.row.ID] != p { // removed or linked again meanwhile
		h.mu.Unlock()
		return context.Canceled
	}
	p.snap, p.online, p.version, p.lastSeen, p.errMsg = &snap, true, snap.Version, h.s.now(), ""
	for _, x := range snap.Sessions {
		if h.claimable(x.ID, p.row.ID) {
			h.setOwner(x.ID, p.row.ID)
		}
	}
	for _, r := range snap.Requests {
		if h.claimable(r.ID, p.row.ID) {
			h.setOwner(r.ID, p.row.ID)
		}
	}
	h.mu.Unlock()
	h.s.log.Info("linked machine online", "machine", p.row.Machine, "version", snap.Version)
	// Events may have been missed while it was away: replay its current
	// sessions and requests (whole objects, so clients just update).
	h.replay(p, snap)
	h.machineChanged(p)
	return h.stream(ctx, p, snap.Seq)
}

// replay pushes a linked machine's snapshot into the merged stream as
// events, so clients catch up without reloading.
func (h *linkHub) replay(p *peerConn, snap api.Snapshot) {
	at := h.s.now().UTC()
	for _, x := range snap.Sessions {
		x.Machine = p.row.Machine
		b, _ := json.Marshal(x)
		h.push(p.row.ID, api.Event{At: at, Type: api.EventSessionUpdated, SessionID: x.ID, Data: b})
	}
	for _, r := range snap.Requests {
		t := api.EventRequestClosed
		if r.Status == api.RequestOpen {
			t = api.EventRequestOpened
		}
		b, _ := json.Marshal(r)
		h.push(p.row.ID, api.Event{At: at, Type: t, SessionID: r.SessionID, RequestID: r.ID, Data: b})
	}
}

// stream reads a linked machine's events after seq until it fails.
func (h *linkHub) stream(ctx context.Context, p *peerConn, after int64) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.row.URL+"/api/link/m/events?after="+strconv.FormatInt(after, 10), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+p.row.PeerToken)
	req.Header.Set("Accept", "text/event-stream")
	// The silence limit also covers connecting: a machine that accepts the
	// connection and then says nothing is offline.
	alive := time.AfterFunc(peerSilence, cancel)
	defer alive.Stop()
	resp, err := h.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return peerStatusErr(resp)
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	var event string
	var data []byte
	for sc.Scan() {
		alive.Reset(peerSilence)
		line := sc.Bytes()
		switch {
		case len(line) == 0:
			switch event {
			case "event":
				var ev api.Event
				if json.Unmarshal(data, &ev) == nil {
					h.mu.Lock()
					p.lastSeen = h.s.now()
					h.mu.Unlock()
					h.push(p.row.ID, ev)
				}
			case "control":
				return errors.New("the event stream asked for a reload")
			}
			event, data = "", data[:0]
		case bytes.HasPrefix(line, []byte("event:")):
			event = strings.TrimSpace(string(line[len("event:"):]))
		case bytes.HasPrefix(line, []byte("data:")):
			data = append(data, bytes.TrimSpace(line[len("data:"):])...)
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	return io.ErrUnexpectedEOF
}

// machineChanged tells clients a machine came or went.
func (h *linkHub) machineChanged(p *peerConn) {
	h.mu.Lock()
	info := p.info()
	if h.peers[p.row.ID] != p {
		info.Removed, info.Online = true, false
	}
	h.mu.Unlock()
	b, _ := json.Marshal(info)
	h.push("", api.Event{At: h.s.now().UTC(), Type: api.EventMachineChanged, Data: b})
}

// hubDevices is what linked machines learn about this hub's devices.
func (h *linkHub) hubDevices(ctx context.Context) ([]linkDeviceIn, error) {
	rows, err := h.s.d.DB.QueryContext(ctx, `SELECT id, name, public_key, revoked_at FROM web_devices`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []linkDeviceIn{}
	for rows.Next() {
		var d linkDeviceIn
		var revoked *int64
		if err := rows.Scan(&d.ID, &d.Name, &d.PublicKey, &revoked); err != nil {
			return nil, err
		}
		if revoked != nil {
			t := time.UnixMilli(*revoked).UTC()
			d.RevokedAt = &t
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func (h *linkHub) sendDevices(ctx context.Context, p *peerConn) error {
	// The version orders lists that arrive out of order: a machine keeps
	// only the newest, so a revoked device or an old key can't come back.
	ver := h.devVersion()
	ds, err := h.hubDevices(ctx)
	if err != nil {
		return err
	}
	err = h.call(ctx, p, http.MethodPost, "/devices", map[string]any{"devices": ds, "version": ver}, nil, peerTimeout)
	var pe *peerError
	if errors.As(err, &pe) && pe.Code == "stale_devices" && pe.Version > 0 {
		// It has a newer list than this hub's clock gives (the clock went
		// back): continue above its version.
		h.mu.Lock()
		h.lastDevVer = max(h.lastDevVer, pe.Version)
		h.mu.Unlock()
		ver = h.devVersion()
		err = h.call(ctx, p, http.MethodPost, "/devices", map[string]any{"devices": ds, "version": ver}, nil, peerTimeout)
	}
	return err
}

// devVersion is strictly increasing, also across restarts (wall clock).
func (h *linkHub) devVersion() int64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	v := h.s.now().UnixMicro()
	if v <= h.lastDevVer {
		v = h.lastDevVer + 1
	}
	h.lastDevVer = v
	return v
}

// pushDevices sends this hub's devices to every linked machine (after a
// device paired, was renamed, revoked or got a new key).
func (h *linkHub) pushDevices() {
	h.mu.Lock()
	ctx := h.ctx
	peers := make([]*peerConn, 0, len(h.peers))
	for _, p := range h.peers {
		peers = append(peers, p)
	}
	h.mu.Unlock()
	if ctx == nil {
		return
	}
	for _, p := range peers {
		go h.sendDevicesRetrying(ctx, p)
	}
}

// sendDevicesRetrying sends the device list until it arrives, the machine
// is unlinked or ctx ends. A machine that is offline also gets the list
// when the hub reconnects to it (followOnce).
func (h *linkHub) sendDevicesRetrying(ctx context.Context, p *peerConn) {
	wait := time.Second
	for {
		err := h.sendDevices(ctx, p)
		if err == nil {
			return
		}
		h.s.log.Info("links: send devices", "machine", p.row.Machine, "err", err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		h.mu.Lock()
		gone := h.peers[p.row.ID] != p
		h.mu.Unlock()
		if gone {
			return
		}
		wait = min(wait*2, 2*time.Minute)
	}
}

// setPresence passes a presence change from a device on to every linked
// machine, so "away" means away everywhere.
func (h *linkHub) setPresence(mode api.PresenceMode) {
	h.mu.Lock()
	ctx := h.ctx
	peers := make([]*peerConn, 0, len(h.peers))
	for _, p := range h.peers {
		peers = append(peers, p)
	}
	h.mu.Unlock()
	if ctx == nil {
		return
	}
	for _, p := range peers {
		go func() {
			if err := h.call(ctx, p, http.MethodPut, "/presence", api.PresenceBody{Mode: mode}, nil, peerTimeout); err != nil {
				h.s.log.Debug("links: presence", "machine", p.row.Machine, "err", err)
			}
		}()
	}
}

// call makes a JSON call to a linked machine's link API.
func (h *linkHub) call(ctx context.Context, p *peerConn, method, path string, in, out any, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, p.row.URL+"/api/link/m"+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+p.row.PeerToken)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := h.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return peerStatusErr(resp)
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		return nil
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 32<<20)).Decode(out)
}

// peerError is an error answer from a linked machine.
type peerError struct {
	Status  int
	Code    string `json:"code"`
	Message string `json:"message"`
	Version int64  `json:"-"` // stale_devices: the version the machine has
}

func (e *peerError) Error() string {
	if e.Message != "" {
		return e.Message
	}
	return fmt.Sprintf("HTTP %d", e.Status)
}

func peerStatusErr(resp *http.Response) error {
	e := &peerError{Status: resp.StatusCode}
	// Pilothouse answers {"error": "…", "code": "…"} (writeErr).
	var b struct {
		Error   json.RawMessage `json:"error"`
		Code    string          `json:"code"`
		Version int64           `json:"version"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&b) == nil {
		e.Code, e.Version = b.Code, b.Version
		var msg string
		var nested struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		}
		switch {
		case json.Unmarshal(b.Error, &msg) == nil:
			e.Message = msg
		case json.Unmarshal(b.Error, &nested) == nil:
			e.Code, e.Message = cmp.Or(e.Code, nested.Code), nested.Message
		}
	}
	return e
}

// peerErrText says, for a person, why a linked machine is offline.
func peerErrText(machine string, err error) string {
	var pe *peerError
	switch {
	case err == nil:
		return ""
	case errors.As(err, &pe) && pe.Status == http.StatusUnauthorized:
		return machine + " no longer accepts this hub: link it again"
	case errors.As(err, &pe):
		return machine + " answered: " + pe.Error()
	default:
		return "can't reach " + machine
	}
}

// ---- forwarding device calls ----

// ownerOf returns the linked machine a session or request id belongs to,
// or nil for this machine. An id not seen yet (a link opened from a
// notification before the event arrived) is looked up on every machine.
func (h *linkHub) ownerOf(ctx context.Context, id string) *peerConn {
	if id == "" {
		return nil
	}
	h.mu.Lock()
	merged := h.merged
	h.mu.Unlock()
	// This machine's own ids are never forwarded, whatever a linked
	// machine claims.
	if !merged || h.localID(ctx, id) {
		return nil
	}
	h.mu.Lock()
	if p := h.peers[h.owner[id]]; p != nil {
		h.mu.Unlock()
		return p
	}
	peers := make([]*peerConn, 0, len(h.peers))
	for _, p := range h.peers {
		peers = append(peers, p)
	}
	h.mu.Unlock()
	var path string
	switch {
	case strings.HasPrefix(id, "s_"):
		path = "/sessions/" + url.PathEscape(id)
	case strings.HasPrefix(id, "r_"):
		path = "/requests/" + url.PathEscape(id)
	default:
		return nil
	}
	found := make(chan *peerConn, len(peers))
	for _, p := range peers {
		go func() {
			if h.call(ctx, p, http.MethodGet, path, nil, nil, 2*time.Second) == nil {
				found <- p
			} else {
				found <- nil
			}
		}()
	}
	for range peers {
		if p := <-found; p != nil {
			h.mu.Lock()
			ok := h.claimable(id, p.row.ID)
			if ok {
				h.setOwner(id, p.row.ID)
			}
			h.mu.Unlock()
			if ok {
				return p
			}
		}
	}
	return nil
}

// fwd serves a device call about a session or request ({id}) on the
// machine that has it. Wrap it inside auth and csrf: the device is
// authenticated here, and the linked machine checks any signature again.
func (s *Server) fwd(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if linkedCall(r) {
			next(w, r)
			return
		}
		p := s.links.ownerOf(r.Context(), r.PathValue("id"))
		if p == nil {
			next(w, r)
			return
		}
		s.links.forward(w, r, p)
	}
}

// forward relays r to the linked machine p as the same device, and its
// answer back, streaming (the live terminal is an event stream).
func (h *linkHub) forward(w http.ResponseWriter, r *http.Request, p *peerConn) {
	d := deviceOf(r)
	// Revoking the device on the hub ends what it has open through it.
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	key := h.s.addStream(d.ID, cancel)
	defer h.s.removeStream(d.ID, key)
	target := p.row.URL + "/api/link/m" + strings.TrimPrefix(r.URL.Path, "/api")
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}
	var body io.Reader
	if r.Body != nil {
		body = http.MaxBytesReader(w, r.Body, maxForwarded)
	}
	req, err := http.NewRequestWithContext(ctx, r.Method, target, body)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	for _, k := range []string{"Content-Type", "Accept", "X-Terminal-Token", "Last-Event-ID"} {
		if v := r.Header.Get(k); v != "" {
			req.Header.Set(k, v)
		}
	}
	req.Header.Set("Authorization", "Bearer "+p.row.PeerToken)
	req.Header.Set("X-Pilothouse-Device", d.ID)
	resp, err := h.client.Do(req)
	if err != nil {
		if r.Context().Err() == nil {
			writeErr(w, http.StatusBadGateway, "machine_offline", fmt.Sprintf("%s isn't reachable right now — try again when it's online", p.row.Machine))
		}
		return
	}
	defer resp.Body.Close()
	// Only what the device API answers: JSON, an event stream, or nothing.
	// Anything else from a linked machine (HTML, script) is refused, so it
	// can never run on this hub's origin, where the device's key lives.
	ct := resp.Header.Get("Content-Type")
	mt, _, _ := mime.ParseMediaType(ct)
	if ct != "" && mt != "application/json" && mt != "text/event-stream" {
		h.s.log.Warn("links: refused a forwarded answer", "machine", p.row.Machine, "content_type", ct)
		writeErr(w, http.StatusBadGateway, "bad_gateway", p.row.Machine+" sent an answer the hub doesn't pass on")
		return
	}
	// No type at all is served as JSON: never left to sniffing.
	switch {
	case mt == "text/event-stream":
		w.Header().Set("Content-Type", mt)
	default:
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
	}
	for _, k := range []string{"Cache-Control", "Retry-After"} {
		if v := resp.Header.Get(k); v != "" {
			w.Header().Set(k, v)
		}
	}
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "sandbox; default-src 'none'")
	rc := http.NewResponseController(w)
	if mt == "text/event-stream" {
		_ = rc.SetWriteDeadline(time.Time{})
		w.Header().Set("X-Accel-Buffering", "no")
	}
	w.WriteHeader(resp.StatusCode)
	buf := make([]byte, 32<<10)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			_ = rc.Flush()
		}
		if err != nil {
			return
		}
	}
}

// cutStreams ends every live event stream; clients reconnect.
func (s *Server) cutStreams() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, m := range s.streams {
		for _, cancel := range m {
			cancel()
		}
	}
}
