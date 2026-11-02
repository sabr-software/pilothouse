package policy

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/sabr-software/pilothouse/internal/adapters"
	"github.com/sabr-software/pilothouse/internal/api"
)

func hookEvent(t *testing.T, seq int64, at time.Time, sid string, he adapters.HookEvent) api.Event {
	t.Helper()
	b, err := json.Marshal(he)
	if err != nil {
		t.Fatal(err)
	}
	return api.Event{Seq: seq, At: at, Type: api.EventHook, SessionID: sid, Data: b}
}

func decisionEvent(t *testing.T, seq int64, at time.Time, sid string, de DecisionEvent) api.Event {
	t.Helper()
	b, err := json.Marshal(de)
	if err != nil {
		t.Fatal(err)
	}
	return api.Event{Seq: seq, At: at, Type: api.EventPolicyDecision, SessionID: sid, Data: b}
}

func TestReplay(t *testing.T) {
	te := newTestEnv(t)
	writeFile(t, te.userFile(), `version: 1
rules:
  - {name: no-push, match: {command: "git push*"}, action: deny}
  - {name: codex-ask-curl, agents: [codex], match: {command: "curl*"}, action: ask}
`)
	e, _ := te.load(t)
	t0 := time.Now().Add(-time.Hour)
	pre := func(cmd, id string) adapters.HookEvent {
		return adapters.HookEvent{Kind: adapters.HookPreTool, Tool: "Bash", ToolKind: api.ToolShell, Command: cmd, ToolUseID: id, Cwd: te.repo}
	}
	events := []api.Event{
		// Too old: before --since.
		hookEvent(t, 1, t0.Add(-48*time.Hour), "s1", pre("git push", "old")),
		// Logged deny by a rule that still exists: unchanged.
		hookEvent(t, 2, t0, "s1", pre("git push origin x", "a")),
		decisionEvent(t, 3, t0, "s1", DecisionEvent{ToolUseID: "a", Result: Result{Action: api.BehaviorDeny, Rule: "no-push"}}),
		// Nothing logged, but now denied: changed.
		hookEvent(t, 4, t0, "s1", pre("git push --tags", "b")),
		// Not a pre_tool hook: ignored.
		hookEvent(t, 5, t0, "s1", adapters.HookEvent{Kind: adapters.HookPostTool, Tool: "Bash", Command: "git push"}),
		// Logged allow from an old rule; now nothing matches: changed.
		hookEvent(t, 6, t0, "s2", pre("ls", "c")),
		// A decision from another session must not pair with s2's hook.
		decisionEvent(t, 7, t0, "s9", DecisionEvent{Result: Result{Action: api.BehaviorDeny, Rule: "x"}}),
		decisionEvent(t, 8, t0, "s2", DecisionEvent{ToolUseID: "c", Result: Result{Action: api.BehaviorAllow, Rule: "old-allow-ls"}}),
		// Codex session: the agents filter needs the session's agent.
		hookEvent(t, 9, t0, "s3", pre("curl x", "")),
		decisionEvent(t, 10, t0, "s3", DecisionEvent{Result: Result{Action: api.BehaviorAsk, Rule: "codex-ask-curl"}}),
		// Garbage data is skipped, not fatal.
		{Seq: 11, At: t0, Type: api.EventHook, SessionID: "s1", Data: json.RawMessage(`{bad`)},
		// Mismatched tool_use_id: the decision belongs to another call.
		hookEvent(t, 12, t0, "s4", pre("git push", "d")),
		decisionEvent(t, 13, t0, "s4", DecisionEvent{ToolUseID: "zzz", Result: Result{Action: api.BehaviorDeny, Rule: "no-push"}}),
	}
	sessions := map[string]api.Session{
		"s3": {ID: "s3", Agent: api.AgentCodex, RepoRoot: te.repo},
	}
	rep := Replay(e, events, t0.Add(-time.Minute), func(id string) (api.Session, bool) {
		s, ok := sessions[id]
		return s, ok
	})
	if rep.Calls != 5 {
		t.Fatalf("calls = %d (%+v)", rep.Calls, rep.Items)
	}
	want := []struct {
		seq       int64
		then, now api.Behavior
		changed   bool
	}{
		{2, api.BehaviorDeny, api.BehaviorDeny, false},
		{4, "", api.BehaviorDeny, true},
		{6, api.BehaviorAllow, "", true},
		{9, api.BehaviorAsk, api.BehaviorAsk, false},
		{12, "", api.BehaviorDeny, true},
	}
	for i, w := range want {
		it := rep.Items[i]
		if it.Seq != w.seq || it.Then != w.then || it.Now != w.now || it.Changed != w.changed {
			t.Errorf("item %d = %+v, want %+v", i, it, w)
		}
	}
	if rep.Changed != 3 {
		t.Errorf("changed = %d", rep.Changed)
	}
	if rep.Items[3].Agent != api.AgentCodex {
		t.Errorf("session agent not applied: %+v", rep.Items[3])
	}
}

func TestDecisionEventJSON(t *testing.T) {
	// The embedded Result's fields are top-level on the wire, so the
	// daemon may also log a bare Result and replay still reads it.
	b, _ := json.Marshal(DecisionEvent{ToolUseID: "x", Result: Result{Action: api.BehaviorDeny, Rule: "r"}, Grant: "g_1"})
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if m["action"] != "deny" || m["rule"] != "r" || m["tool_use_id"] != "x" || m["grant"] != "g_1" {
		t.Fatalf("json = %s", b)
	}
	var de DecisionEvent
	if err := json.Unmarshal([]byte(`{"action":"ask","rule":"r2"}`), &de); err != nil || de.Action != api.BehaviorAsk || de.Rule != "r2" {
		t.Fatalf("bare result: %+v %v", de, err)
	}
}
