package notify

import (
	"testing"

	"github.com/sabr-software/pilothouse/internal/api"
	"github.com/sabr-software/pilothouse/internal/core"
)

func TestWhereBackground(t *testing.T) {
	s := &api.Session{Title: "relay", Branch: "main", Background: true}
	if got := Where(nil, s); got != "relay (main, background)" {
		t.Errorf("Where = %q", got)
	}
	s.Branch = ""
	if got := Where(nil, s); got != "relay (background)" {
		t.Errorf("Where = %q", got)
	}
	if got := Render(core.Notification{Kind: core.NotifyRequest, Session: s, Request: &api.Request{Agent: api.AgentClaude, Tool: "Edit", Summary: "Edit: data/bridge/meta.json"}}).Title; got != "Claude Code · relay (background)" {
		t.Errorf("title = %q", got)
	}
}
