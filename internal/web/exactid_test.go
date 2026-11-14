package web

import (
	"testing"

	"github.com/sabr-software/pilothouse/internal/api"
	"github.com/sabr-software/pilothouse/internal/web/webtest"
)

// The core expands id prefixes for the command line. A device signs the id
// it sends, so a signed call must name the whole id: a signature over
// "r_exact" is not a decision on r_exact00001.
func TestSignedCallsNameWholeIDs(t *testing.T) {
	rc := &replyCore{}
	e := newEnv(t, func(d *Deps) { rc.FakeCore = d.Core.(*webtest.FakeCore); d.Core = rc })
	k, pub := genKey(t)
	ck, _ := e.pair(pub)
	o := reqOpt{cookie: ck}

	e.openRequest("r_exact00001")
	if w := e.do("POST", "/api/requests/r_exact/decide", e.signed(k, "r_exact", api.BehaviorAllow, api.ScopeOnce), o); w.Code != 404 {
		t.Errorf("decide by prefix: %d %s", w.Code, w.Body)
	}
	body := respondBody{Behavior: api.BehaviorAllow}
	if w := e.do("POST", "/api/requests/r_exact/respond", e.signAction(k, ActionDecide, "r_exact", body), o); w.Code != 404 {
		t.Errorf("respond by prefix: %d %s", w.Code, w.Body)
	}
	if ds := e.fc.Decisions(); len(ds) != 0 {
		t.Fatalf("a prefix decided a request: %+v", ds)
	}

	e.fc.PutSession(api.Session{ID: "s_exact00001", Agent: api.AgentClaude, State: api.StateWorking})
	if w := e.do("POST", "/api/sessions/s_exact/reply", e.signAction(k, ActionReply, "s_exact", api.Reply{Text: "hi"}), o); w.Code != 404 {
		t.Errorf("reply by prefix: %d %s", w.Code, w.Body)
	}
	if len(rc.replies) != 0 {
		t.Fatalf("a prefix reached a session: %+v", rc.replies)
	}

	// Whole ids still work.
	if w := e.do("POST", "/api/sessions/s_exact00001/reply", e.signAction(k, ActionReply, "s_exact00001", api.Reply{Text: "hi"}), o); w.Code != 200 {
		t.Errorf("reply: %d %s", w.Code, w.Body)
	}
	if w := e.do("POST", "/api/requests/r_exact00001/decide", e.signed(k, "r_exact00001", api.BehaviorAllow, api.ScopeOnce), o); w.Code != 200 {
		t.Errorf("decide: %d %s", w.Code, w.Body)
	}
}
