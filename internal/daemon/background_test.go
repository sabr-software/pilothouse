package daemon

import (
	"testing"

	"github.com/sabr-software/pilothouse/internal/adapters"
	"github.com/sabr-software/pilothouse/internal/api"
)

func TestBackgroundRun(t *testing.T) {
	p := func(pid int, comm, args string) adapters.Proc { return adapters.Proc{PID: pid, Comm: comm, Args: args} }
	cases := []struct {
		name  string
		agent api.AgentKind
		env   adapters.HookEnvelope
		pid   int
		bg    bool
		by    string
	}{
		{"claude -p from a launchd job's script (the relay evening refresh)", api.AgentClaude, adapters.HookEnvelope{
			Ancestors: []adapters.Proc{p(39259, "claude", "claude -p Refresh the Relay bridge feeds. You are running headlessly"),
				p(39246, "zsh", "zsh scripts/relay-bridge.sh"), p(38573, "zsh", "/bin/zsh /Users/x/Projects/relay/scripts/relay-evening.sh")},
			Env: map[string]string{"XPC_SERVICE_NAME": "com.relay.evening"}}, 39259, true, "relay-bridge.sh, from launchd job com.relay.evening"},
		{"interactive claude in herdr", api.AgentClaude, adapters.HookEnvelope{
			Ancestors: []adapters.Proc{p(500, "claude", "claude"), p(499, "zsh", "-zsh"), p(400, "herdr", "herdr")},
			TTY:       "/dev/ttys004", Env: map[string]string{"XPC_SERVICE_NAME": "0"}}, 500, false, ""},
		{"interactive claude in Terminal", api.AgentClaude, adapters.HookEnvelope{
			Ancestors: []adapters.Proc{p(500, "claude", "claude --model opus")},
			TTY:       "/dev/ttys001", Env: map[string]string{"XPC_SERVICE_NAME": "application.com.apple.Terminal.123"}}, 500, false, ""},
		{"a prompt that mentions -p is not print mode", api.AgentClaude, adapters.HookEnvelope{
			Ancestors: []adapters.Proc{p(500, "claude", "claude fix the flag parsing so that -p and --print work everywhere")},
			TTY:       "/dev/ttys001"}, 500, false, ""},
		{"print mode after other options", api.AgentClaude, adapters.HookEnvelope{
			Ancestors: []adapters.Proc{p(500, "claude", "claude --model opus --permission-mode acceptEdits -p summarize")}}, 500, true, ""},
		{"claude -p typed in a terminal", api.AgentClaude, adapters.HookEnvelope{
			Ancestors: []adapters.Proc{p(500, "claude", "claude -p summarize"), p(499, "zsh", "-zsh"), p(1, "login", "login")},
			TTY:       "/dev/ttys001"}, 500, true, ""},
		{"codex exec from cron via sh -c", api.AgentCodex, adapters.HookEnvelope{
			Ancestors: []adapters.Proc{p(700, "codex", "codex exec fix the build"), p(699, "sh", "sh -c cd ~/app && codex exec fix the build"), p(698, "cron", "cron")}},
			700, true, "cron"},
		{"opencode run from a node script under systemd", api.AgentOpenCode, adapters.HookEnvelope{
			Ancestors: []adapters.Proc{p(800, "opencode", "opencode run triage"), p(799, "node", "node /srv/bots/triage.js")},
			Env:       map[string]string{"INVOCATION_ID": "abc"}}, 800, true, "triage.js, from a systemd service"},
		{"a launchd job without print mode still has no terminal", api.AgentClaude, adapters.HookEnvelope{
			Ancestors: []adapters.Proc{p(900, "claude", "claude")}, Env: map[string]string{"XPC_SERVICE_NAME": "com.me.nightly"}},
			900, true, "launchd job com.me.nightly"},
	}
	for _, c := range cases {
		bg, by := backgroundRun(c.agent, c.pid, c.env)
		if bg != c.bg || by != c.by {
			t.Errorf("%s: got (%v, %q), want (%v, %q)", c.name, bg, by, c.bg, c.by)
		}
	}
}
