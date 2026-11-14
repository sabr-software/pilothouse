package web

import (
	"testing"

	"github.com/sabr-software/pilothouse/internal/api"
)

// An action link is a bearer credential: whoever holds the URL can use it.
// It may refuse a request, never approve one. A link stored as "allow" (one
// sent before 0.4.15) must be dead.
func TestActionTokenNeverAllows(t *testing.T) {
	e := newEnv(t)
	e.cfg.Web.PublicURL = "https://mac.tail1.ts.net"
	e.srv.pubURL = normalizePublicURL(e.cfg.Web.PublicURL)
	req := e.openRequest("r_legacy00001")
	host := reqOpt{host: "mac.tail1.ts.net", origin: "-", noCSRF: true}

	tok := randomToken(32)
	if _, err := e.db.Exec(`INSERT INTO web_action_tokens (token_hash, request_id, behavior, expires_at) VALUES (?, ?, 'allow', ?)`,
		hashSecret(tok), req.ID, ms(req.ExpiresAt)); err != nil {
		t.Fatal(err)
	}
	if w := e.do("POST", "/api/actions/"+tok, nil, host); w.Code != 410 {
		t.Errorf("an allow link answered %d %s, want 410", w.Code, w.Body)
	}
	for _, d := range e.fc.Decisions() {
		if d.Behavior != api.BehaviorDeny {
			t.Errorf("a link approved a request without a device signature: %+v", d)
		}
	}
}
