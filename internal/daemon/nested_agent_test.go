package daemon

import (
	"context"
	"os"
	"testing"

	"github.com/sabr-software/pilothouse/internal/adapters"
	"github.com/sabr-software/pilothouse/internal/api"
)

// An agent started inside another agent's wrapped session inherits
// PILOTHOUSE_SESSION_ID. Its hooks describe a different conversation, so
// they must not be folded into the outer agent's session.
func TestNestedAgentOfAnotherKindGetsItsOwnSession(t *testing.T) {
	h := newHarness(t)
	h.start(t)
	ctx := context.Background()
	const sid = "s_outer"
	outer, err := h.c.RegisterWrapped(ctx, api.WrappedSessionBody{SessionID: sid, Agent: api.AgentClaude, Cwd: os.TempDir(), PID: os.Getpid()})
	if err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"PILOTHOUSE_SESSION_ID": sid}
	nested := adapters.HookEvent{Kind: adapters.HookSessionStart, AgentSessionID: "inner-1", TranscriptPath: "/tmp/inner.jsonl"}
	if _, err := h.hookEnv(ctx, nested, env); err != nil {
		t.Fatal(err)
	}
	if _, err := h.hookEnv(ctx, adapters.HookEvent{Kind: adapters.HookSessionEnd, AgentSessionID: "inner-1"}, env); err != nil {
		t.Fatal(err)
	}

	got, err := h.c.Session(ctx, sid)
	if err != nil {
		t.Fatal(err)
	}
	if got.AgentSessionID != outer.AgentSessionID || got.TranscriptPath != outer.TranscriptPath {
		t.Errorf("the nested agent replaced the outer session's conversation: id %q, transcript %q", got.AgentSessionID, got.TranscriptPath)
	}
	if got.ExitedAt != nil {
		t.Errorf("the nested agent's exit ended the outer session")
	}
	all, err := h.c.Sessions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var inner *api.Session
	for i := range all {
		if all[i].AgentSessionID == "inner-1" {
			inner = &all[i]
		}
	}
	if inner == nil || inner.ID == sid || inner.Agent != fakeKind {
		t.Fatalf("the nested agent has no session of its own: %+v", all)
	}

	// The same agent keeps joining its own wrapped session, whatever
	// conversation id it reports (a cleared or resumed conversation).
	const own = "s_own"
	if _, err := h.c.RegisterWrapped(ctx, api.WrappedSessionBody{SessionID: own, Agent: fakeKind, Cwd: os.TempDir(), PID: os.Getpid()}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.hookEnv(ctx, adapters.HookEvent{Kind: adapters.HookSessionStart, AgentSessionID: "conv-2"},
		map[string]string{"PILOTHOUSE_SESSION_ID": own}); err != nil {
		t.Fatal(err)
	}
	if s, _ := h.c.Session(ctx, own); s.AgentSessionID != "conv-2" {
		t.Errorf("the wrapped agent's own hook didn't join its session: %+v", s)
	}
	// A generic wrapped session (`pilothouse run -- sh`) takes whichever
	// agent reports from inside it.
	const gen = "s_generic"
	if _, err := h.c.RegisterWrapped(ctx, api.WrappedSessionBody{SessionID: gen, Agent: api.AgentGeneric, Cwd: os.TempDir(), PID: os.Getpid()}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.hookEnv(ctx, adapters.HookEvent{Kind: adapters.HookSessionStart, AgentSessionID: "conv-3"},
		map[string]string{"PILOTHOUSE_SESSION_ID": gen}); err != nil {
		t.Fatal(err)
	}
	if s, _ := h.c.Session(ctx, gen); s.AgentSessionID != "conv-3" {
		t.Errorf("a hook inside a generic wrapped session didn't join it: %+v", s)
	}
}
