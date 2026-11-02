// Package client talks to the local daemon over its Unix socket. The CLI,
// the TUI, the hook shim and `pilothouse run` holders all use it.
package client

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/sabr-software/pilothouse/internal/adapters"
	"github.com/sabr-software/pilothouse/internal/api"
	"github.com/sabr-software/pilothouse/internal/paths"
)

// ErrDaemonDown means nothing is listening on the socket.
var ErrDaemonDown = errors.New("pilothouse daemon is not running (start it with `pilothouse start`)")

// APIError is a non-2xx response from the daemon.
type APIError struct {
	Status int
	Code   string
	Msg    string
}

func (e *APIError) Error() string { return e.Msg }

// IsCode reports whether err is an APIError with the given code.
func IsCode(err error, code string) bool {
	var ae *APIError
	return errors.As(err, &ae) && ae.Code == code
}

type Client struct {
	socket string
	hc     *http.Client
	name   string // X-Pilothouse-Client header; see WithClientName
}

// ClientHeader identifies the caller ("cli", "tui"); the daemon records it
// as Decision.By for decisions made over the Unix socket.
const ClientHeader = "X-Pilothouse-Client"

// WithClientName returns a copy of c that sends name in the
// X-Pilothouse-Client header on every request. The copy shares c's
// connection pool.
func (c *Client) WithClientName(name string) *Client {
	cp := *c
	cp.name = name
	return &cp
}

// New returns a client for the default socket.
func New() *Client { return NewSocket(paths.Socket()) }

// NewSocket returns a client for a specific socket path.
func NewSocket(socket string) *Client {
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		},
		MaxIdleConns:    4,
		IdleConnTimeout: 30 * time.Second,
	}
	return &Client{socket: socket, hc: &http.Client{Transport: tr}}
}

func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://pilothouse"+path, rd)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.name != "" {
		req.Header.Set(ClientHeader, c.name)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		if isDialErr(err) {
			return ErrDaemonDown
		}
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		var e api.Error
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		if json.Unmarshal(b, &e) != nil || e.Error == "" {
			e.Error = strings.TrimSpace(string(b))
			if e.Error == "" {
				e.Error = resp.Status
			}
		}
		return &APIError{Status: resp.StatusCode, Code: e.Code, Msg: e.Error}
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func isDialErr(err error) bool {
	var oe *net.OpError
	if errors.As(err, &oe) && oe.Op == "dial" {
		return true
	}
	s := err.Error()
	return strings.Contains(s, "connect: no such file") || strings.Contains(s, "connection refused")
}

func (c *Client) Health(ctx context.Context) (api.Health, error) {
	var h api.Health
	return h, c.do(ctx, "GET", "/v1/health", nil, &h)
}

func (c *Client) Snapshot(ctx context.Context) (api.Snapshot, error) {
	var s api.Snapshot
	return s, c.do(ctx, "GET", "/v1/snapshot", nil, &s)
}

// Hook forwards a hook envelope. It may block until a person answers.
func (c *Client) Hook(ctx context.Context, env adapters.HookEnvelope) (adapters.HookResponse, error) {
	var r adapters.HookResponse
	return r, c.do(ctx, "POST", "/v1/hook", env, &r)
}

func (c *Client) Sessions(ctx context.Context) ([]api.Session, error) {
	var s []api.Session
	return s, c.do(ctx, "GET", "/v1/sessions", nil, &s)
}

func (c *Client) Session(ctx context.Context, id string) (api.Session, error) {
	var s api.Session
	return s, c.do(ctx, "GET", "/v1/sessions/"+url.PathEscape(id), nil, &s)
}

func (c *Client) SessionEvents(ctx context.Context, id string, limit int) ([]api.Event, error) {
	var e []api.Event
	return e, c.do(ctx, "GET", "/v1/sessions/"+url.PathEscape(id)+"/events?limit="+strconv.Itoa(limit), nil, &e)
}

func (c *Client) MarkSeen(ctx context.Context, id string) error {
	return c.do(ctx, "POST", "/v1/sessions/"+url.PathEscape(id)+"/seen", nil, nil)
}

func (c *Client) Explain(ctx context.Context, id string) (api.Explain, error) {
	var e api.Explain
	return e, c.do(ctx, "GET", "/v1/sessions/"+url.PathEscape(id)+"/explain", nil, &e)
}

func (c *Client) RegisterWrapped(ctx context.Context, b api.WrappedSessionBody) (api.Session, error) {
	var s api.Session
	return s, c.do(ctx, "POST", "/v1/sessions/wrapped", b, &s)
}

func (c *Client) ReportOSC(ctx context.Context, sessionID string, b api.OSCBody) error {
	return c.do(ctx, "POST", "/v1/sessions/"+url.PathEscape(sessionID)+"/osc", b, nil)
}

func (c *Client) ReportExit(ctx context.Context, sessionID string, code int) error {
	return c.do(ctx, "POST", "/v1/sessions/"+url.PathEscape(sessionID)+"/exit", api.ExitBody{ExitCode: code}, nil)
}

// Requests lists requests; status "" means all recent.
func (c *Client) Requests(ctx context.Context, status api.RequestStatus) ([]api.Request, error) {
	var r []api.Request
	p := "/v1/requests"
	if status != "" {
		p += "?status=" + url.QueryEscape(string(status))
	}
	return r, c.do(ctx, "GET", p, nil, &r)
}

func (c *Client) Request(ctx context.Context, id string) (api.Request, error) {
	var r api.Request
	return r, c.do(ctx, "GET", "/v1/requests/"+url.PathEscape(id), nil, &r)
}

// Decide answers a request. Errors with code "already_decided" if someone
// answered first, "agent_caller" if called from inside an agent session.
func (c *Client) Decide(ctx context.Context, id string, b api.DecideBody) (api.Request, error) {
	var r api.Request
	return r, c.do(ctx, "POST", "/v1/requests/"+url.PathEscape(id)+"/decide", b, &r)
}

func (c *Client) Presence(ctx context.Context) (api.Presence, error) {
	var p api.Presence
	return p, c.do(ctx, "GET", "/v1/presence", nil, &p)
}

func (c *Client) SetPresence(ctx context.Context, m api.PresenceMode) (api.Presence, error) {
	var p api.Presence
	return p, c.do(ctx, "PUT", "/v1/presence", api.PresenceBody{Mode: m}, &p)
}

func (c *Client) Tasks(ctx context.Context) ([]api.Task, error) {
	var t []api.Task
	return t, c.do(ctx, "GET", "/v1/tasks", nil, &t)
}

func (c *Client) Task(ctx context.Context, id string) (api.Task, error) {
	var t api.Task
	return t, c.do(ctx, "GET", "/v1/tasks/"+url.PathEscape(id), nil, &t)
}

func (c *Client) PutTask(ctx context.Context, t api.Task) (api.Task, error) {
	var out api.Task
	return out, c.do(ctx, "POST", "/v1/tasks", t, &out)
}

func (c *Client) PolicyStatus(ctx context.Context) (api.PolicyStatus, error) {
	var s api.PolicyStatus
	return s, c.do(ctx, "GET", "/v1/policy/status", nil, &s)
}

func (c *Client) PolicyReload(ctx context.Context) (api.PolicyStatus, error) {
	var s api.PolicyStatus
	return s, c.do(ctx, "POST", "/v1/policy/reload", nil, &s)
}

func (c *Client) NotifyTest(ctx context.Context) error {
	return c.do(ctx, "POST", "/v1/notify/test", nil, nil)
}

// Shutdown asks the daemon to stop gracefully (held requests are handed back
// to the agents' own dialogs first). Errors with "agent_caller" from inside
// an agent session.
func (c *Client) Shutdown(ctx context.Context) error {
	return c.do(ctx, "POST", "/v1/shutdown", nil, nil)
}

func (c *Client) Pair(ctx context.Context) (api.PairStart, error) {
	var p api.PairStart
	return p, c.do(ctx, "POST", "/v1/pair", nil, &p)
}

func (c *Client) LoginLink(ctx context.Context) (api.LoginLink, error) {
	var l api.LoginLink
	return l, c.do(ctx, "POST", "/v1/login-link", nil, &l)
}

func (c *Client) Devices(ctx context.Context) ([]api.Device, error) {
	var d []api.Device
	return d, c.do(ctx, "GET", "/v1/devices", nil, &d)
}

func (c *Client) RevokeDevice(ctx context.Context, id string) error {
	return c.do(ctx, "DELETE", "/v1/devices/"+url.PathEscape(id), nil, nil)
}

// Events returns logged events after seq (non-streaming, up to 1000).
func (c *Client) Events(ctx context.Context, after int64) ([]api.Event, error) {
	var e []api.Event
	return e, c.do(ctx, "GET", "/v1/events?after="+strconv.FormatInt(after, 10), nil, &e)
}

// Stream follows the event log from after. It calls fn for each event in
// order. It returns ErrResync if the daemon says the client must reload
// a snapshot, nil when ctx is cancelled, or another error on disconnect.
func (c *Client) Stream(ctx context.Context, after int64, fn func(api.Event)) error {
	req, err := http.NewRequestWithContext(ctx, "GET", "http://pilothouse/v1/events?follow=1&after="+strconv.FormatInt(after, 10), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/event-stream")
	if c.name != "" {
		req.Header.Set(ClientHeader, c.name)
	}
	// No client timeout: the stream is long-lived; ctx ends it.
	resp, err := c.hc.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil
		}
		if isDialErr(err) {
			return ErrDaemonDown
		}
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("event stream: %s", resp.Status)
	}
	return ReadSSE(resp.Body, func(event string, data []byte) error {
		switch event {
		case "control":
			var sc api.StreamControl
			if json.Unmarshal(data, &sc) == nil && sc.ResyncRequired {
				return ErrResync
			}
		case "event", "":
			var ev api.Event
			if err := json.Unmarshal(data, &ev); err != nil {
				return fmt.Errorf("bad event: %w", err)
			}
			fn(ev)
		}
		return nil
	}, ctx)
}

// ErrResync means the client must reload a Snapshot before streaming again.
var ErrResync = errors.New("resync required")

// ReadSSE parses a text/event-stream body. Exported for the web tests.
func ReadSSE(r io.Reader, fn func(event string, data []byte) error, ctx context.Context) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 8<<20)
	var event string
	var data bytes.Buffer
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			if data.Len() > 0 {
				if err := fn(event, bytes.TrimSuffix(data.Bytes(), []byte("\n"))); err != nil {
					return err
				}
			}
			event = ""
			data.Reset()
		case strings.HasPrefix(line, ":"):
			// comment / heartbeat
		case strings.HasPrefix(line, "event:"):
			event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			data.WriteString(strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
			data.WriteByte('\n')
		}
	}
	if ctx.Err() != nil {
		return nil
	}
	if err := sc.Err(); err != nil {
		return err
	}
	return io.ErrUnexpectedEOF
}

// ---- linked machines (pilothouse link) ----

func (c *Client) LinkCode(ctx context.Context) (api.LinkCode, error) {
	var v api.LinkCode
	return v, c.do(ctx, "POST", "/v1/link/code", nil, &v)
}

func (c *Client) LinkJoin(ctx context.Context, j api.LinkJoin) (api.LinkJoined, error) {
	var v api.LinkJoined
	return v, c.do(ctx, "POST", "/v1/link/join", j, &v)
}

func (c *Client) LinkStatus(ctx context.Context) (api.LinkStatus, error) {
	var v api.LinkStatus
	return v, c.do(ctx, "GET", "/v1/link", nil, &v)
}

// LinkTrust confirms the hub's devices on this machine; it returns how
// many it trusted.
func (c *Client) LinkTrust(ctx context.Context, t api.LinkTrust) (int, error) {
	var v struct {
		Trusted int `json:"trusted"`
	}
	return v.Trusted, c.do(ctx, "POST", "/v1/link/trust", t, &v)
}

// LinkRemove unlinks a machine (on the hub) or leaves the hub; id is a
// link id or machine name.
func (c *Client) LinkRemove(ctx context.Context, id string) (api.LinkPeer, error) {
	var v api.LinkPeer
	return v, c.do(ctx, "DELETE", "/v1/link/"+url.PathEscape(id), nil, &v)
}
