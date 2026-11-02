package web

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/xasdta/pilothouse/internal/api"
	"github.com/xasdta/pilothouse/internal/core"
	"github.com/xasdta/pilothouse/internal/web/webtest"
)

// deskCore adds core.Desktop to the fake core.
type deskCore struct {
	*webtest.FakeCore
	mu     sync.Mutex
	opened []string // id|by
	err    error
}

func (c *deskCore) DesktopInfo(context.Context) api.DesktopInfo {
	return api.DesktopInfo{Target: api.DesktopHerdr, Label: "herdr", Machine: "devbox"}
}

func (c *deskCore) OpenOnDesktop(_ context.Context, id, by string) (api.DesktopOpened, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return api.DesktopOpened{}, c.err
	}
	c.opened = append(c.opened, id+"|"+by)
	return api.DesktopOpened{Target: api.DesktopHerdr, Label: "herdr", Where: "PageMind", Machine: "devbox"}, nil
}

func (c *deskCore) got() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.opened...)
}

func TestDesktopInfo(t *testing.T) {
	// A core that can't open anything answers an empty target.
	e := newEnv(t)
	k, pub := genKey(t)
	ck, _ := e.pair(pub)
	if w := e.do("GET", "/api/desktop", nil, reqOpt{}); w.Code != 401 {
		t.Fatalf("no cookie: %d", w.Code)
	}
	w := e.do("GET", "/api/desktop", nil, reqOpt{cookie: ck})
	var info api.DesktopInfo
	mustJSON(t, w, &info)
	if w.Code != 200 || info.Target != "" {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	termSessions(e.fc, e.clock.Now())
	if w := e.do("POST", "/api/sessions/s_term000001/desktop", e.signAction(k, ActionDesktop, "s_term000001", struct{}{}), reqOpt{cookie: ck}); w.Code != 501 || errCode(t, w) != "unsupported" {
		t.Fatalf("unsupported core: %d %s", w.Code, w.Body)
	}

	var dc *deskCore
	e = newEnv(t, func(d *Deps) { dc = &deskCore{FakeCore: d.Core.(*webtest.FakeCore)}; d.Core = dc })
	_, pub = genKey(t)
	ck, _ = e.pair(pub)
	w = e.do("GET", "/api/desktop", nil, reqOpt{cookie: ck})
	mustJSON(t, w, &info)
	if info != (api.DesktopInfo{Target: "herdr", Label: "herdr", Machine: "devbox"}) {
		t.Fatalf("info %+v", info)
	}
}

func TestOpenOnDesktop(t *testing.T) {
	var dc *deskCore
	e := newEnv(t, func(d *Deps) { dc = &deskCore{FakeCore: d.Core.(*webtest.FakeCore)}; d.Core = dc })
	termSessions(e.fc, e.clock.Now())
	k, pub := genKey(t)
	ck, devID := e.pair(pub)
	open := func(id string, body any, o ...reqOpt) (int, string) {
		opt := reqOpt{cookie: ck}
		if len(o) > 0 {
			opt = o[0]
		}
		w := e.do("POST", "/api/sessions/"+id+"/desktop", body, opt)
		return w.Code, errCode(t, w) + " " + w.Body.String()
	}

	body := e.signAction(k, ActionDesktop, "s_term000001", struct{}{})
	w := e.do("POST", "/api/sessions/s_term000001/desktop", body, reqOpt{cookie: ck})
	var res api.DesktopOpened
	mustJSON(t, w, &res)
	if w.Code != 200 || res.Where != "PageMind" || res.Label != "herdr" {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if got := dc.got(); len(got) != 1 || got[0] != "s_term000001|device:"+devID {
		t.Fatalf("core got %q", got)
	}

	// Each of these is refused before the core is asked.
	other, _ := genKey(t)
	for name, tc := range map[string]struct {
		id   string
		body any
		opt  *reqOpt
		code int
	}{
		"replayed":        {"s_term000001", body, nil, 403},
		"no cookie":       {"s_term000001", e.signAction(k, ActionDesktop, "s_term000001", struct{}{}), &reqOpt{}, 401},
		"no CSRF header":  {"s_term000001", e.signAction(k, ActionDesktop, "s_term000001", struct{}{}), &reqOpt{cookie: ck, noCSRF: true}, 403},
		"unsigned":        {"s_term000001", map[string]string{}, nil, 400},
		"other key":       {"s_term000001", e.signAction(other, ActionDesktop, "s_term000001", struct{}{}), nil, 403},
		"other action":    {"s_term000001", e.signAction(k, ActionTerminal, "s_term000001", struct{}{}), nil, 403},
		"other session":   {"s_other00001", e.signAction(k, ActionDesktop, "s_term000001", struct{}{}), nil, 403},
		"payload fields":  {"s_term000001", e.signAction(k, ActionDesktop, "s_term000001", map[string]string{"cmd": "rm -rf ~"}), nil, 400},
		"prefix id":       {"s_term", e.signAction(k, ActionDesktop, "s_term", struct{}{}), nil, 404},
		"unknown session": {"s_nope000001", e.signAction(k, ActionDesktop, "s_nope000001", struct{}{}), nil, 404},
	} {
		var code int
		var msg string
		if tc.opt != nil {
			code, msg = open(tc.id, tc.body, *tc.opt)
		} else {
			code, msg = open(tc.id, tc.body)
		}
		if code != tc.code {
			t.Errorf("%s: %d %s, want %d", name, code, msg, tc.code)
		}
	}
	if got := dc.got(); len(got) != 1 {
		t.Fatalf("core was asked again: %q", got)
	}

	// The core's refusals are shown as they are.
	for _, tc := range []struct {
		err  error
		code int
		want string
	}{
		{fmt.Errorf("%w: herdr isn't running", core.ErrUnsupported), 501, "herdr isn't running"},
		{core.Invalid("this session has ended"), 400, "this session has ended"},
	} {
		dc.mu.Lock()
		dc.err = tc.err
		dc.mu.Unlock()
		w := e.do("POST", "/api/sessions/s_term000001/desktop", e.signAction(k, ActionDesktop, "s_term000001", struct{}{}), reqOpt{cookie: ck})
		var ae api.Error
		mustJSON(t, w, &ae)
		if w.Code != tc.code || ae.Error != tc.want {
			t.Fatalf("%v: %d %s", tc.err, w.Code, w.Body)
		}
	}
}
