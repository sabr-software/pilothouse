package daemon

import (
	"path"
	"strings"

	"github.com/sabr-software/pilothouse/internal/adapters"
	"github.com/sabr-software/pilothouse/internal/api"
)

// Background runs: an agent nobody is watching at a terminal, started by a
// script or a scheduler (`claude -p` from a launchd job, say). The inbox
// labels them so they aren't mistaken for the session in your terminal
// that shares their folder.

// printMode reports a command line that runs the agent once, without its
// interactive terminal UI. args is the agent process's command line.
func printMode(agent api.AgentKind, args string) bool {
	f := strings.Fields(args)
	if len(f) < 2 {
		return false
	}
	switch agent {
	case api.AgentClaude:
		// Only the options before the prompt: a prompt may say "-p" too.
		for i := 1; i < len(f); i++ {
			a := f[i]
			switch {
			case a == "-p" || a == "--print" || strings.HasPrefix(a, "--print="):
				return true
			case !strings.HasPrefix(a, "-"):
				return false // the prompt begins
			case claudeValueFlags[a]:
				i++ // its value
			}
		}
	case api.AgentCodex:
		return f[1] == "exec" || f[1] == "e"
	case api.AgentOpenCode:
		return f[1] == "run"
	}
	return false
}

// claudeValueFlags are Claude Code options followed by a value.
var claudeValueFlags = map[string]bool{"--model": true, "--fallback-model": true, "--permission-mode": true,
	"--output-format": true, "--input-format": true, "--allowedTools": true, "--allowed-tools": true,
	"--disallowedTools": true, "--disallowed-tools": true, "--append-system-prompt": true, "--system-prompt": true,
	"--add-dir": true, "--mcp-config": true, "--settings": true, "--session-id": true, "--max-turns": true,
	"--agents": true, "--agent": true, "-r": true, "--resume": true, "--setting-sources": true}

// launchdJob is the launchd job a process runs under, from
// XPC_SERVICE_NAME: "" for apps ("application.…") and Terminal ("0").
func launchdJob(v string) string {
	if v == "" || v == "0" || strings.HasPrefix(v, "application.") {
		return ""
	}
	return v
}

var shells = map[string]bool{"sh": true, "bash": true, "zsh": true, "dash": true, "fish": true, "ksh": true,
	"node": true, "python": true, "python3": true, "ruby": true, "perl": true, "deno": true, "bun": true}

// scriptOf names what a parent process runs: a shell or interpreter's
// script ("relay-bridge.sh"), else the program itself.
func scriptOf(p adapters.Proc) string {
	comm := path.Base(p.Comm)
	if !shells[strings.TrimPrefix(comm, "-")] {
		return comm
	}
	f := strings.Fields(p.Args)
	for i := 1; i < len(f); i++ {
		a := f[i]
		if a == "-c" { // an inline command: nothing to name
			return ""
		}
		if strings.HasPrefix(a, "-") {
			continue
		}
		return path.Base(a)
	}
	return ""
}

// backgroundRun decides whether the agent at agentPID runs in the
// background and, if so, what started it.
func backgroundRun(agent api.AgentKind, agentPID int, env adapters.HookEnvelope) (bool, string) {
	at := -1
	for i, p := range env.Ancestors {
		if p.PID == agentPID {
			at = i
			break
		}
	}
	job := launchdJob(env.Env["XPC_SERVICE_NAME"])
	systemd := env.Env["INVOCATION_ID"] != ""
	bg := at >= 0 && printMode(agent, env.Ancestors[at].Args)
	if !bg && env.TTY == "" && (job != "" || systemd) {
		bg = true // a scheduled job has no terminal to be interactive in
	}
	if !bg {
		return false, ""
	}
	var script string
	cron := false
	if at >= 0 {
		up := env.Ancestors[at+1:]
		for _, p := range up {
			if c := path.Base(p.Comm); c == "cron" || c == "crond" {
				cron = true
			}
		}
		// The nearest script that started it. A shell running no script is
		// someone's terminal, or an inline `sh -c`: nothing more to name.
		for _, p := range up {
			if script = scriptOf(p); script != "" || shells[strings.TrimPrefix(path.Base(p.Comm), "-")] {
				break
			}
		}
	}
	var from string
	switch {
	case job != "":
		from = "launchd job " + job
	case systemd:
		from = "a systemd service"
	case cron:
		from = "cron"
	}
	switch {
	case script != "" && from != "":
		return true, script + ", from " + from
	case script != "":
		return true, script
	}
	return true, from
}
