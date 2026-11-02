package hookkit

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/sabr-software/pilothouse/internal/adapters"
)

// MaxMessage caps HookEvent.Message: prompts can be whole pasted files,
// and the event log needs the gist, not the payload.
const MaxMessage = 4096

// Payload is the union of the hook stdin fields Pilothouse reads. Claude Code
// and Codex share these names (Codex adopted Claude's hook schema).
type Payload struct {
	SessionID            string          `json:"session_id"`
	TranscriptPath       *string         `json:"transcript_path"` // Codex sends null before the rollout exists
	Cwd                  string          `json:"cwd"`
	Event                string          `json:"hook_event_name"`
	PermissionMode       string          `json:"permission_mode"`
	ToolName             string          `json:"tool_name"`
	ToolInput            json.RawMessage `json:"tool_input"`
	ToolUseID            string          `json:"tool_use_id"`
	Prompt               string          `json:"prompt"`
	Message              string          `json:"message"`
	NotificationType     string          `json:"notification_type"`
	LastAssistantMessage *string         `json:"last_assistant_message"`
	Source               string          `json:"source"`  // SessionStart
	Reason               string          `json:"reason"`  // SessionEnd
	Trigger              string          `json:"trigger"` // PreCompact
	AgentType            string          `json:"agent_type"`
}

// ParsePayload decodes a hook's stdin. The vendor event comes from the
// payload, or from the shim's argv when the payload lacks it.
func ParsePayload(env adapters.HookEnvelope) (Payload, error) {
	var p Payload
	if len(bytes.TrimSpace(env.Payload)) == 0 {
		return p, errors.New("empty hook payload")
	}
	if err := json.Unmarshal(env.Payload, &p); err != nil {
		return p, fmt.Errorf("hook payload: %w", err)
	}
	if p.Event == "" {
		p.Event = env.Event
	}
	if p.Event == "" {
		return p, errors.New("hook payload has no hook_event_name")
	}
	if p.Cwd == "" {
		p.Cwd = env.Cwd
	}
	if string(bytes.TrimSpace(p.ToolInput)) == "null" {
		p.ToolInput = nil
	}
	return p, nil
}

// HookEvent builds the vendor-neutral fields every adapter fills the same way.
func (p Payload) HookEvent(kind adapters.HookKind) adapters.HookEvent {
	ev := adapters.HookEvent{
		Kind:           kind,
		VendorEvent:    p.Event,
		AgentSessionID: p.SessionID,
		Cwd:            p.Cwd,
		ToolUseID:      p.ToolUseID,
		Tool:           p.ToolName,
		Input:          p.ToolInput,
		PermissionMode: p.PermissionMode,
	}
	if p.TranscriptPath != nil {
		ev.TranscriptPath = *p.TranscriptPath
	}
	switch kind {
	case adapters.HookPromptSubmit:
		ev.Message = CapBytes(p.Prompt, MaxMessage)
		ev.Summary = "Prompt: " + OneLine(p.Prompt, 100)
	case adapters.HookNotification:
		ev.Message = CapBytes(p.Message, MaxMessage)
		ev.NotificationType = p.NotificationType
		ev.Summary = OneLine(p.Message, 120)
	case adapters.HookStop, adapters.HookSubagentStop:
		if p.LastAssistantMessage != nil {
			ev.Message = CapBytes(*p.LastAssistantMessage, MaxMessage)
		}
		ev.Summary = stopSummary(kind, p, ev.Message)
	case adapters.HookSessionStart:
		ev.Summary = withDetail("Session started", p.Source, "startup")
	case adapters.HookSessionEnd:
		ev.Summary = withDetail("Session ended", p.Reason, "other")
	case adapters.HookPreCompact:
		ev.Summary = withDetail("Compacting context", p.Trigger, "")
	case adapters.HookUnknown:
		ev.Summary = p.Event
	}
	return ev
}

func stopSummary(kind adapters.HookKind, p Payload, msg string) string {
	if kind == adapters.HookSubagentStop {
		return withDetail("Subagent finished", p.AgentType, "")
	}
	if strings.TrimSpace(msg) == "" {
		return "Finished its turn"
	}
	return "Finished: " + OneLine(msg, 100)
}

// withDetail appends "(detail)" unless it is empty or the boring default.
func withDetail(s, detail, boring string) string {
	if detail == "" || detail == boring {
		return s
	}
	return s + " (" + detail + ")"
}

// Field reads one string field of a tool input ("" when absent).
func Field(input json.RawMessage, name string) string {
	if len(input) == 0 {
		return ""
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(input, &m) != nil {
		return ""
	}
	var s string
	if json.Unmarshal(m[name], &s) != nil {
		return ""
	}
	return s
}

// MCPName splits "mcp__server__tool"; ok is false for other tools. Server
// names may contain "_" (plugin servers: "plugin_x_db"), tool names too,
// so the split is on the first "__" after the prefix.
func MCPName(tool string) (server, name string, ok bool) {
	rest, found := strings.CutPrefix(tool, "mcp__")
	if !found {
		return "", "", false
	}
	server, name, found = strings.Cut(rest, "__")
	if !found || server == "" {
		return "", "", false
	}
	return server, name, true
}
