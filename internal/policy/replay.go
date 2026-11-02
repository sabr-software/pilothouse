package policy

import (
	"encoding/json"
	"time"

	"github.com/sabr-software/pilothouse/internal/adapters"
	"github.com/sabr-software/pilothouse/internal/api"
)

// DecisionEvent is the Data of an api.EventPolicyDecision event. The
// daemon emits one after a pre_tool hook whenever a rule or a session
// grant decided the call. The embedded Result is the policy verdict
// alone; Grant and Outcome record what the daemon finally answered.
type DecisionEvent struct {
	ToolUseID string `json:"tool_use_id,omitempty"`
	Tool      string `json:"tool,omitempty"`
	Summary   string `json:"summary,omitempty"`
	Result
	Grant   string       `json:"grant,omitempty"`   // grant id, when a session grant allowed the call
	Outcome api.Behavior `json:"outcome,omitempty"` // behavior sent to the agent
}

// ReplayItem compares one logged pre_tool call with the current rules.
type ReplayItem struct {
	Seq       int64         `json:"seq"`
	At        time.Time     `json:"at"`
	SessionID string        `json:"session_id"`
	Agent     api.AgentKind `json:"agent,omitempty"`
	Tool      string        `json:"tool"`
	Summary   string        `json:"summary"`
	// Then is the policy verdict logged at the time ("" = no rule matched).
	Then     api.Behavior `json:"then"`
	ThenRule string       `json:"then_rule,omitempty"`
	// Now is the verdict of the current rules.
	Now     api.Behavior `json:"now"`
	NowRule string       `json:"now_rule,omitempty"`
	Changed bool         `json:"changed"`
	Result  Result       `json:"result"`
}

// ReplayReport summarises a replay.
type ReplayReport struct {
	Since   time.Time    `json:"since"`
	Calls   int          `json:"calls"`   // pre_tool calls re-evaluated
	Changed int          `json:"changed"` // calls whose verdict would differ
	Items   []ReplayItem `json:"items"`   // every call, oldest first
}

// Replay re-evaluates logged pre_tool hook events with e. events must be
// in Seq order and include the policy.decision events that followed
// them; sessions supplies agent, repo, branch and mode, which hook events
// don't carry (it may return false for unknown sessions). Each hook is
// paired with the first policy.decision of the same session that follows
// it — by tool_use_id when both have one — before that session's next
// pre_tool hook. Time-based rules see the time of the original call.
func Replay(e *Engine, events []api.Event, since time.Time, sessions func(id string) (api.Session, bool)) ReplayReport {
	rep := ReplayReport{Since: since}
	type pending struct {
		idx       int
		toolUseID string
	}
	open := map[string]pending{} // session → hook awaiting its decision
	for _, ev := range events {
		switch ev.Type {
		case api.EventHook:
			var he adapters.HookEvent
			if json.Unmarshal(ev.Data, &he) != nil || he.Kind != adapters.HookPreTool {
				continue
			}
			delete(open, ev.SessionID)
			if ev.At.Before(since) {
				continue
			}
			in := Input{
				Tool: he.Tool, ToolKind: he.ToolKind, Command: he.Command, Paths: he.Paths,
				Input: he.Input, Cwd: he.Cwd, Checkpoint: api.CheckpointPreTool, Now: ev.At,
			}
			if s, ok := sessions(ev.SessionID); ok {
				in.Agent, in.Repo, in.Branch, in.Mode = s.Agent, s.RepoRoot, s.Branch, s.Mode
			}
			res := e.Evaluate(in)
			summary := he.Summary
			if summary == "" {
				summary = he.Command
			}
			rep.Items = append(rep.Items, ReplayItem{
				Seq: ev.Seq, At: ev.At, SessionID: ev.SessionID, Agent: in.Agent,
				Tool: he.Tool, Summary: summary,
				Now: res.Action, NowRule: res.Rule, Result: res,
			})
			open[ev.SessionID] = pending{idx: len(rep.Items) - 1, toolUseID: he.ToolUseID}
		case api.EventPolicyDecision:
			p, ok := open[ev.SessionID]
			if !ok {
				continue
			}
			var de DecisionEvent
			if json.Unmarshal(ev.Data, &de) != nil {
				continue
			}
			if p.toolUseID != "" && de.ToolUseID != "" && p.toolUseID != de.ToolUseID {
				continue
			}
			it := &rep.Items[p.idx]
			it.Then, it.ThenRule = de.Action, de.Rule
			delete(open, ev.SessionID)
		}
	}
	for i := range rep.Items {
		it := &rep.Items[i]
		it.Changed = it.Then != it.Now
		if it.Changed {
			rep.Changed++
		}
	}
	rep.Calls = len(rep.Items)
	return rep
}
